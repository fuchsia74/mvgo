package main

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// cloner 承载一次 clone 批次的共享状态。
type cloner struct {
	s           *session
	src         *vmRef
	targetDS    string
	linked      bool
	snapshot    *types.ManagedObjectReference // 链接克隆的父盘快照
	snapDevices []types.BaseVirtualDevice     // 快照时刻的设备列表(取父盘用)
}

// ensureSnapshot 确保源机有名为 snapName 的只读快照,返回快照 ref 及其设备列表。
// 找到同名则复用,否则创建。
func (c *cloner) ensureSnapshot(ctx context.Context, snapName string) (
	*types.ManagedObjectReference, []types.BaseVirtualDevice, error) {

	// 先在现有快照树里找
	if c.src.mo.Snapshot != nil {
		if ref := searchSnapshot(c.src.mo.Snapshot.RootSnapshotList, snapName); ref != nil {
			fmt.Printf("  复用已有快照 '%s'\n", snapName)
			devs, err := c.snapshotDevices(ctx, *ref)
			return ref, devs, err
		}
	}

	fmt.Printf("  为源虚机创建快照 '%s'(链接克隆父盘冻结点)\n", snapName)
	task, err := c.src.obj.CreateSnapshot(ctx, snapName,
		"Base snapshot for linked clones", false, false)
	if err != nil {
		return nil, nil, err
	}
	if err := task.Wait(ctx); err != nil {
		return nil, nil, err
	}
	// 重新读快照树
	var vmm mo.VirtualMachine
	if err := c.src.obj.Properties(ctx, c.src.obj.Reference(),
		[]string{"snapshot"}, &vmm); err != nil {
		return nil, nil, err
	}
	if vmm.Snapshot == nil {
		return nil, nil, fmt.Errorf("创建后仍读不到快照树")
	}
	ref := searchSnapshot(vmm.Snapshot.RootSnapshotList, snapName)
	if ref == nil {
		return nil, nil, fmt.Errorf("创建后找不到快照 '%s'", snapName)
	}
	devs, err := c.snapshotDevices(ctx, *ref)
	return ref, devs, err
}

// searchSnapshot 在快照树里递归找同名快照,返回其 ref。
func searchSnapshot(nodes []types.VirtualMachineSnapshotTree, name string) *types.ManagedObjectReference {
	for i := range nodes {
		if nodes[i].Name == name {
			ref := nodes[i].Snapshot
			return &ref
		}
		if found := searchSnapshot(nodes[i].ChildSnapshotList, name); found != nil {
			return found
		}
	}
	return nil
}

// snapshotDevices 取快照时刻的设备列表(父盘只读点)。
func (c *cloner) snapshotDevices(ctx context.Context,
	snapRef types.ManagedObjectReference) ([]types.BaseVirtualDevice, error) {
	var snap mo.VirtualMachineSnapshot
	pc := c.s.client.Client
	obj := object.NewCommon(pc, snapRef)
	if err := obj.Properties(ctx, snapRef, []string{"config"}, &snap); err != nil {
		return nil, err
	}
	if snap.Config.Hardware.Device == nil {
		return nil, fmt.Errorf("快照无设备信息")
	}
	return snap.Config.Hardware.Device, nil
}

// resolveHostPool 定位源机所在 host 及其资源池(CreateVM 需要)。
func (c *cloner) resolveHostPool(ctx context.Context) (
	*object.HostSystem, *object.ResourcePool, *object.Datacenter, error) {
	if c.src.mo.Runtime.Host == nil {
		return nil, nil, nil, fmt.Errorf("源机无 runtime.host")
	}
	host := object.NewHostSystem(c.s.client.Client, *c.src.mo.Runtime.Host)
	// host.parent 是 ComputeResource,取其 resourcePool
	var hs mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"parent"}, &hs); err != nil {
		return nil, nil, nil, err
	}
	if hs.Parent == nil {
		return nil, nil, nil, fmt.Errorf("host 无 parent(ComputeResource)")
	}
	var cr mo.ComputeResource
	crObj := object.NewCommon(c.s.client.Client, *hs.Parent)
	if err := crObj.Properties(ctx, *hs.Parent, []string{"resourcePool"}, &cr); err != nil {
		return nil, nil, nil, err
	}
	if cr.ResourcePool == nil {
		return nil, nil, nil, fmt.Errorf("ComputeResource 无 resourcePool")
	}
	pool := object.NewResourcePool(c.s.client.Client, *cr.ResourcePool)
	return host, pool, c.s.dc, nil
}

// findDatastore 在源机 host 的 datastore 里按名找目标 datastore ref。
func (c *cloner) findDatastore(ctx context.Context, host *object.HostSystem,
	name string) (*types.ManagedObjectReference, error) {
	var hs mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"datastore"}, &hs); err != nil {
		return nil, err
	}
	for _, dsRef := range hs.Datastore {
		var ds mo.Datastore
		obj := object.NewCommon(c.s.client.Client, dsRef)
		if err := obj.Properties(ctx, dsRef, []string{"name"}, &ds); err != nil {
			continue
		}
		if ds.Name == name {
			r := dsRef
			return &r, nil
		}
	}
	return nil, fmt.Errorf("找不到 datastore: %s", name)
}

// cloneOne 复制单个虚机:建目录 -> 构建磁盘 backing -> 建配置 -> 注册。
func (c *cloner) cloneOne(ctx context.Context, newName string) (*vmRef, error) {
	host, pool, dc, err := c.resolveHostPool(ctx)
	if err != nil {
		return nil, err
	}
	targetDSRef, err := c.findDatastore(ctx, host, c.targetDS)
	if err != nil {
		return nil, err
	}

	fm := object.NewFileManager(c.s.client.Client)
	newDir := fmt.Sprintf("[%s] %s", c.targetDS, newName)
	fmt.Printf("  创建目录 %s\n", newDir)
	if err := fm.MakeDirectory(ctx, newDir, dc, true); err != nil {
		if !isFileAlreadyExists(err) {
			return nil, err
		}
	}

	var diskBackings []types.BaseVirtualDeviceBackingInfo
	if c.linked {
		fmt.Println("  链接克隆:基于快照建 delta 差分盘(不复制数据)")
		diskBackings = c.buildLinkedBackings(newName)
	} else {
		diskBackings, err = c.copyDisksAndBuildBackings(ctx, dc, targetDSRef, newName)
		if err != nil {
			return nil, err
		}
	}

	config := c.buildConfigSpec(newName, newDir, diskBackings)

	fmt.Printf("  注册虚机 %s\n", newName)
	folder := object.NewFolder(c.s.client.Client, c.s.dc.Reference())
	// 用 datacenter 的 vmFolder
	vmFolderRef, err := c.vmFolder(ctx)
	if err != nil {
		return nil, err
	}
	folder = object.NewFolder(c.s.client.Client, vmFolderRef)
	task, err := folder.CreateVM(ctx, config, pool, host)
	if err != nil {
		return nil, err
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, err
	}
	newRef := info.Result.(types.ManagedObjectReference)
	return &vmRef{obj: object.NewVirtualMachine(c.s.client.Client, newRef)}, nil
}

// vmFolder 取 datacenter 的 vmFolder ref。
func (c *cloner) vmFolder(ctx context.Context) (types.ManagedObjectReference, error) {
	var dc mo.Datacenter
	if err := c.s.dc.Properties(ctx, c.s.dc.Reference(),
		[]string{"vmFolder"}, &dc); err != nil {
		return types.ManagedObjectReference{}, err
	}
	return dc.VmFolder, nil
}

// buildLinkedBackings 链接克隆:每块盘建 delta 差分盘,parent 指向快照只读父盘。
func (c *cloner) buildLinkedBackings(newName string) []types.BaseVirtualDeviceBackingInfo {
	var backings []types.BaseVirtualDeviceBackingInfo
	diskNum := 0
	for _, dev := range c.snapDevices {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		pb, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			continue
		}
		parent := &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
				FileName:  pb.FileName,
				Datastore: pb.Datastore,
			},
			DiskMode: pb.DiskMode,
		}
		child := &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
				FileName: fmt.Sprintf("[%s] %s/%s_%d-delta.vmdk",
					c.targetDS, newName, newName, diskNum),
			},
			DiskMode: "persistent",
			Parent:   parent, // 关键:形成 delta -> 父盘链
		}
		backings = append(backings, child)
		diskNum++
	}
	return backings
}

// copyDisksAndBuildBackings 完整复制:逐块复制 vmdk,再构建 backing 指向副本。
func (c *cloner) copyDisksAndBuildBackings(ctx context.Context, dc *object.Datacenter,
	targetDSRef *types.ManagedObjectReference, newName string) (
	[]types.BaseVirtualDeviceBackingInfo, error) {

	vdm := object.NewVirtualDiskManager(c.s.client.Client)
	var backings []types.BaseVirtualDeviceBackingInfo
	diskNum := 0
	for _, dev := range c.src.mo.Config.Hardware.Device {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		pb, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			continue
		}
		srcVmdk := pb.FileName
		dstVmdk := fmt.Sprintf("[%s] %s/%s_%d.vmdk", c.targetDS, newName, newName, diskNum)
		fmt.Printf("  复制磁盘 %s -> %s\n", srcVmdk, dstVmdk)
		task, err := vdm.CopyVirtualDisk(ctx, srcVmdk, dc, dstVmdk, dc, nil, true)
		if err != nil {
			return nil, err
		}
		if err := task.Wait(ctx); err != nil {
			return nil, err
		}
		thin := true
		backing := &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
				FileName:  dstVmdk,
				Datastore: targetDSRef,
			},
			DiskMode:        "persistent",
			ThinProvisioned: &thin,
		}
		backings = append(backings, backing)
		diskNum++
	}
	return backings, nil
}

// buildConfigSpec 构建新虚机 ConfigSpec:控制器原样复制;磁盘套用 backing;
// 网卡保留类型/网络,MAC 重生成。linked 时磁盘 spec 带 fileOperation=create。
func (c *cloner) buildConfigSpec(newName, newDir string,
	diskBackings []types.BaseVirtualDeviceBackingInfo) types.VirtualMachineConfigSpec {

	var changes []types.BaseVirtualDeviceConfigSpec
	diskIndex := 0
	for _, dev := range c.src.mo.Config.Hardware.Device {
		switch d := dev.(type) {
		case *types.VirtualIDEController, *types.VirtualPS2Controller,
			*types.VirtualPCIController, *types.VirtualSIOController,
			*types.VirtualLsiLogicController, *types.VirtualLsiLogicSASController,
			*types.ParaVirtualSCSIController, *types.VirtualBusLogicController,
			*types.VirtualAHCIController, *types.VirtualNVMEController:
			changes = append(changes, &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    dev,
			})
		case *types.VirtualDisk:
			if diskIndex >= len(diskBackings) {
				continue
			}
			d.Backing = diskBackings[diskIndex]
			spec := &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    d,
			}
			if c.linked {
				// 链接克隆:delta 盘文件尚不存在,让 ESXi 依 backing.parent 现场生成
				spec.FileOperation = types.VirtualDeviceConfigSpecFileOperationCreate
			}
			changes = append(changes, spec)
			diskIndex++
		default:
			// 网卡:复制类型和网络,MAC 让 ESXi 自动生成
			if nic, ok := dev.(types.BaseVirtualEthernetCard); ok {
				card := nic.GetVirtualEthernetCard()
				card.MacAddress = ""
				card.AddressType = "generated"
				card.Key = 0
				changes = append(changes, &types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationAdd,
					Device:    dev,
				})
			}
		}
	}

	return types.VirtualMachineConfigSpec{
		Name:         newName,
		MemoryMB:     int64(c.src.mo.Config.Hardware.MemoryMB),
		NumCPUs:      c.src.mo.Config.Hardware.NumCPU,
		GuestId:      c.src.mo.Config.GuestId,
		Files:        &types.VirtualMachineFileInfo{VmPathName: newDir},
		DeviceChange: changes,
	}
}

// isFileAlreadyExists 判断是否为"目录已存在"错误(MakeDirectory 幂等)。
func isFileAlreadyExists(err error) bool {
	if soap.IsSoapFault(err) {
		if _, ok := soap.ToSoapFault(err).VimFault().(types.FileAlreadyExists); ok {
			return true
		}
	}
	return false
}

package main

import (
	"context"

	"github.com/vmware/govmomi/vim25/types"
)

func runDelete(ctx context.Context, g *globalOpts, args []string) {
	var f filterOpts
	fs := subFlagSet("delete")
	addFilters(fs, &f)
	keepFiles := fs.Bool("keep-files", false,
		"仅从清单注销(UnregisterVM),保留磁盘文件;默认连同磁盘一并删除(Destroy)")
	force := fs.Bool("force", false,
		"删除前对开机中的虚机先硬断电;不加则跳过开机的虚机")
	yes := fs.Bool("yes", false, "跳过二次确认")
	workers := fs.Int("workers", 8, "并发数(默认8)")
	fs.Parse(args)

	enforceFilter(&f)
	s := mustConnect(ctx, g)
	vms, err := s.selectVMs(ctx, &f)
	if err != nil {
		die("%v", err)
	}
	ensureNonEmpty(vms)

	mode := "删除(含磁盘文件)"
	if *keepFiles {
		mode = "注销(保留磁盘)"
	}
	// 删除不可逆,风险最高:无论范围大小一律二次确认(仅 --yes 可跳过)
	if !confirm(vms, mode, *yes) {
		println("已取消")
		return
	}
	runBatch(vms, *workers, mode, func(v *vmRef) result {
		return deleteOne(ctx, v, *keepFiles, *force)
	})
}

// deleteOne 删除单台。
// keepFiles=true : UnregisterVM,仅从清单移除,磁盘文件保留(可日后重注册)。
// keepFiles=false: Destroy,连同磁盘文件一并删除(不可恢复)。
// Destroy/Unregister 都要求虚机非开机:force=true 先硬断电;force=false 开机的直接跳过。
func deleteOne(ctx context.Context, v *vmRef, keepFiles, force bool) result {
	if v.powerState() != string(types.VirtualMachinePowerStatePoweredOff) {
		if !force {
			return result{v.name(), false, "虚机开机中,已跳过(加 --force 先断电再删)"}
		}
		task, err := v.obj.PowerOff(ctx)
		if err != nil {
			return result{v.name(), false, "断电失败: " + err.Error()}
		}
		if err := task.Wait(ctx); err != nil {
			return result{v.name(), false, "断电失败: " + err.Error()}
		}
	}
	if keepFiles {
		if err := v.obj.Unregister(ctx); err != nil {
			return result{v.name(), false, err.Error()}
		}
		return result{v.name(), true, "已从清单注销(磁盘文件保留)"}
	}
	task, err := v.obj.Destroy(ctx)
	if err != nil {
		return result{v.name(), false, err.Error()}
	}
	if err := task.Wait(ctx); err != nil {
		return result{v.name(), false, err.Error()}
	}
	return result{v.name(), true, "已删除(含磁盘文件)"}
}

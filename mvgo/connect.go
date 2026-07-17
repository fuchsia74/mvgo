package main

import (
	"context"
	"fmt"
	"net/url"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
)

// session 打包一次连接的常用句柄。
type session struct {
	client *govmomi.Client
	finder *find.Finder
	dc     *object.Datacenter
}

// vmRef 把 object.VirtualMachine 与其已取回的属性(mo)绑在一起,
// 避免每次读 name/runtime/guest/config 都再发一次请求。
type vmRef struct {
	obj *object.VirtualMachine
	mo  mo.VirtualMachine
}

func (v *vmRef) name() string { return v.mo.Name }
func (v *vmRef) powerState() string {
	return string(v.mo.Runtime.PowerState)
}

// connect 建立到 ESXi 的 govmomi 连接,并定位默认 datacenter。
func connect(ctx context.Context, g *globalOpts) (*session, error) {
	u := &url.URL{
		Scheme: "https",
		Host:   fmt.Sprintf("%s:%d", g.host, g.port),
		Path:   "/sdk",
		User:   url.UserPassword(g.user, g.password),
	}
	c, err := govmomi.NewClient(ctx, u, g.insecure)
	if err != nil {
		return nil, fmt.Errorf("连接 ESXi 失败: %w", err)
	}
	f := find.NewFinder(c.Client, true)
	dc, err := f.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("定位 datacenter 失败: %w", err)
	}
	f.SetDatacenter(dc)
	return &session{client: c, finder: f, dc: dc}, nil
}

// allVMs 取回所有虚机及其常用属性。
func (s *session) allVMs(ctx context.Context) ([]*vmRef, error) {
	m := view.NewManager(s.client.Client)
	v, err := m.CreateContainerView(ctx, s.client.Client.ServiceContent.RootFolder,
		[]string{"VirtualMachine"}, true)
	if err != nil {
		return nil, err
	}
	defer v.Destroy(ctx)

	var mos []mo.VirtualMachine
	// 只取需要的属性,减少传输
	props := []string{"name", "runtime", "guest", "config", "snapshot", "resourcePool"}
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, props, &mos); err != nil {
		return nil, err
	}
	out := make([]*vmRef, 0, len(mos))
	for i := range mos {
		out = append(out, &vmRef{
			obj: object.NewVirtualMachine(s.client.Client, mos[i].Self),
			mo:  mos[i],
		})
	}
	return out, nil
}

// findVM 按名称精确查找单台(用于 clone 的 --source)。
func (s *session) findVM(ctx context.Context, name string) (*vmRef, error) {
	vms, err := s.allVMs(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vms {
		if v.name() == name {
			return v, nil
		}
	}
	return nil, nil
}

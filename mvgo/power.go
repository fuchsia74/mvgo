package main

import (
	"context"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

func runPowerOn(ctx context.Context, args []string) {
	var g globalOpts
	var f filterOpts
	fs := newFlagSet("power-on", &g)
	addFilters(fs, &f)
	yes := fs.Bool("yes", false, "跳过二次确认")
	workers := fs.Int("workers", 8, "并发数(默认8)")
	fs.Parse(args)

	enforceFilter(&f)
	s := mustConnect(ctx, &g)
	vms, err := s.selectVMs(ctx, &f)
	if err != nil {
		die("%v", err)
	}
	ensureNonEmpty(vms)

	// --all 或纯 --state 这类大范围,仍需确认
	big := f.all || (f.state != "" && f.prefix == "" && f.names == "")
	if big && !confirm(vms, "开机", *yes) {
		println("已取消")
		return
	}
	runBatch(vms, *workers, "开机", func(v *vmRef) result {
		return powerOnOne(ctx, v)
	})
}

func runPowerOff(ctx context.Context, args []string) {
	var g globalOpts
	var f filterOpts
	fs := newFlagSet("power-off", &g)
	addFilters(fs, &f)
	hard := fs.Bool("hard", false, "硬断电(PowerOff),默认优雅关机(ShutdownGuest)")
	yes := fs.Bool("yes", false, "跳过二次确认")
	workers := fs.Int("workers", 8, "并发数(默认8)")
	fs.Parse(args)

	enforceFilter(&f)
	s := mustConnect(ctx, &g)
	vms, err := s.selectVMs(ctx, &f)
	if err != nil {
		die("%v", err)
	}
	ensureNonEmpty(vms)

	mode := "优雅关机"
	if *hard {
		mode = "硬断电"
	}
	// 关机风险高于开机,--all/--hard 都要求确认
	needConfirm := f.all || *hard || (f.state != "" && f.prefix == "" && f.names == "")
	if needConfirm && !confirm(vms, mode, *yes) {
		println("已取消")
		return
	}
	runBatch(vms, *workers, mode, func(v *vmRef) result {
		return powerOffOne(ctx, v, *hard)
	})
}

// powerOnOne 开机单台。已开机则跳过。
func powerOnOne(ctx context.Context, v *vmRef) result {
	if v.powerState() == string(types.VirtualMachinePowerStatePoweredOn) {
		return result{v.name(), true, "已是开机状态,跳过"}
	}
	task, err := v.obj.PowerOn(ctx)
	if err != nil {
		return result{v.name(), false, err.Error()}
	}
	if err := task.Wait(ctx); err != nil {
		return result{v.name(), false, err.Error()}
	}
	return result{v.name(), true, "已开机"}
}

// powerOffOne 关机单台。已关机则跳过。
// hard=false: ShutdownGuest(需 Tools),轮询等待进入 poweredOff。
// hard=true : PowerOff 硬断电。
func powerOffOne(ctx context.Context, v *vmRef, hard bool) result {
	if v.powerState() == string(types.VirtualMachinePowerStatePoweredOff) {
		return result{v.name(), true, "已是关机状态,跳过"}
	}
	if hard {
		task, err := v.obj.PowerOff(ctx)
		if err != nil {
			return result{v.name(), false, err.Error()}
		}
		if err := task.Wait(ctx); err != nil {
			return result{v.name(), false, err.Error()}
		}
		return result{v.name(), true, "已硬断电"}
	}
	// 优雅关机需 Tools
	if v.mo.Guest == nil || v.mo.Guest.ToolsRunningStatus != "guestToolsRunning" {
		return result{v.name(), false,
			"VMware Tools 未运行,无法优雅关机(可加 --hard 硬断电)"}
	}
	if err := v.obj.ShutdownGuest(ctx); err != nil {
		return result{v.name(), false, err.Error()}
	}
	// 最多等 120s 进入 poweredOff
	for waited := 0; waited < 120; waited += 3 {
		ps, err := v.obj.PowerState(ctx)
		if err == nil && ps == types.VirtualMachinePowerStatePoweredOff {
			return result{v.name(), true, "已优雅关机"}
		}
		time.Sleep(3 * time.Second)
	}
	return result{v.name(), false, "优雅关机超时(120s),客户机可能未响应"}
}

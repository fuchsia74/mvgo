package main

import (
	"context"
	"fmt"
)

// fmtMem 把 MB 转人类可读。
func fmtMem(mb int32) string {
	if mb <= 0 {
		return "-"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.0fGB", float64(mb)/1024)
	}
	return fmt.Sprintf("%dMB", mb)
}

// vmIP 取 guest IP,没有则返回 '-'。
func (v *vmRef) vmIP() string {
	if v.mo.Guest != nil && v.mo.Guest.IpAddress != "" {
		return v.mo.Guest.IpAddress
	}
	return "-"
}

func (v *vmRef) cpuStr() string {
	if v.mo.Config != nil {
		return fmt.Sprintf("%d", v.mo.Config.Hardware.NumCPU)
	}
	return "-"
}

func (v *vmRef) memStr() string {
	if v.mo.Config != nil {
		return fmtMem(v.mo.Config.Hardware.MemoryMB)
	}
	return "-"
}

func (v *vmRef) toolsStr() string {
	if v.mo.Guest != nil && v.mo.Guest.ToolsRunningStatus != "" {
		return v.mo.Guest.ToolsRunningStatus
	}
	return "-"
}

func runList(ctx context.Context, g *globalOpts, args []string) {
	var f filterOpts
	fs := subFlagSet("list")
	addFilters(fs, &f)
	fs.Parse(args)

	s := mustConnect(ctx, g)
	vms, err := s.selectVMs(ctx, &f)
	if err != nil {
		die("%v", err)
	}
	cmdList(vms)
}

// cmdList 打印虚机表格。
func cmdList(vms []*vmRef) {
	if len(vms) == 0 {
		fmt.Println("(无匹配的虚机)")
		return
	}
	hdr := fmt.Sprintf("%-24s %-11s %4s %7s %-16s %-14s",
		"名称", "电源", "CPU", "内存", "IP", "Tools")
	fmt.Println(hdr)
	fmt.Println(dashes(len(hdr)))
	for _, v := range sortedByName(vms) {
		fmt.Printf("%-24s %-11s %4s %7s %-16s %-14s\n",
			v.name(), v.powerState(), v.cpuStr(), v.memStr(), v.vmIP(), v.toolsStr())
	}
	fmt.Printf("\n共 %d 台\n", len(vms))
}

func dashes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

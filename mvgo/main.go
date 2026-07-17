// mvgo:直连单台 ESXi(无 vCenter)的批量虚机管理工具,govmomi 实现。
// 对应 Python 版 manage_vms.py,子命令 list/power-on/power-off/delete/clone/customize。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

const usage = `mvgo — ESXi 批量虚机管理(直连单台 ESXi,无 vCenter)

用法:
  mvgo --host HOST [全局参数] <子命令> [子命令参数]

子命令:
  list        列出虚机
  power-on    批量开机
  power-off   批量关机(默认优雅关机,--hard 硬断电)
  delete      批量删除(默认连磁盘删,--keep-files 仅注销)
  clone       批量复制虚机(完整复制 / --linked 链接克隆,可选 --customize)
  customize   对单台已有虚机做 guest 定制(改主机名/配IP/跑脚本)

环境变量:
  ESXI_USER / ESXI_PASSWORD / GUEST_USER / GUEST_PASSWORD

各子命令参数见 mvgo <子命令> -h
`

// hasFilter 判断筛选条件是否给了至少一个。
func hasFilter(f *filterOpts) bool {
	return f.names != "" || f.prefix != "" || f.all || f.state != ""
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// 先剥离全局参数之外的子命令:约定 `mvgo --host x <cmd> ...`,
	// 但为兼容 `mvgo <cmd> --host x ...`,让每个子命令 FlagSet 同时带全局参数。
	cmd := os.Args[1]
	if cmd == "-h" || cmd == "--help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(0)
	}
	args := os.Args[2:]

	ctx := context.Background()
	switch cmd {
	case "list":
		runList(ctx, args)
	case "power-on":
		runPowerOn(ctx, args)
	case "power-off":
		runPowerOff(ctx, args)
	case "delete":
		runDelete(ctx, args)
	case "clone":
		runClone(ctx, args)
	case "customize":
		runCustomize(ctx, args)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// newFlagSet 建一个带全局参数的 FlagSet(每个子命令共用全局参数)。
func newFlagSet(name string, g *globalOpts) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	addGlobal(fs, g)
	return fs
}

// mustConnect:校验 host/password 后连接,失败即退出。
func mustConnect(ctx context.Context, g *globalOpts) *session {
	g.finalizeInsecure()
	if g.host == "" {
		die("必须指定 --host")
	}
	if g.password == "" {
		die("未提供密码(--password 或环境变量 ESXI_PASSWORD)")
	}
	s, err := connect(ctx, g)
	if err != nil {
		die("%v", err)
	}
	return s
}

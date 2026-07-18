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

全局参数(置于子命令前):
  --host HOST     ESXi 主机地址(必填)
  --port PORT     端口(默认 443)
  --user USER     ESXi 账号(默认读 ESXI_USER,再默认 root)
  --password PW   密码(默认读 ESXI_PASSWORD)
  --insecure      跳过SSL证书校验(默认开);--no-insecure 开启校验

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
	if os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(0)
	}

	// 两段式解析(与 Python 一致):mvgo [全局参数] <子命令> [子命令参数]。
	// 先用全局 FlagSet 解析到第一个非 flag 的 token(即子命令名),其余留给子命令。
	var g globalOpts
	gfs := flag.NewFlagSet("mvgo", flag.ExitOnError)
	addGlobal(gfs, &g)
	// 全局解析出错时打印顶层用法(含子命令列表 + 全局参数)
	gfs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := gfs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	rest := gfs.Args() // 全局参数之后的部分:[子命令, 子命令参数...]
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "错误: 缺少子命令")
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	g.finalizeInsecure()
	cmd := rest[0]
	args := rest[1:]

	ctx := context.Background()
	switch cmd {
	case "list":
		runList(ctx, &g, args)
	case "power-on":
		runPowerOn(ctx, &g, args)
	case "power-off":
		runPowerOff(ctx, &g, args)
	case "delete":
		runDelete(ctx, &g, args)
	case "clone":
		runClone(ctx, &g, args)
	case "customize":
		runCustomize(ctx, &g, args)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// subFlagSet 建一个子命令 FlagSet(不含全局参数,全局已在 main 解析)。
func subFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	setDoubleDashUsage(fs)
	return fs
}

// setDoubleDashUsage 覆盖 FlagSet 的 Usage,把内置的单破折号 "-flag"
// 显示成 "--flag"(仅改帮助/报错显示,不影响解析——解析本就单双破折号都收)。
func setDoubleDashUsage(fs *flag.FlagSet) {
	fs.Usage = func() {
		out := fs.Output()
		if fs.Name() == "" {
			fmt.Fprintf(out, "用法:\n")
		} else {
			fmt.Fprintf(out, "用法: %s [参数]\n", fs.Name())
		}
		fs.VisitAll(func(f *flag.Flag) {
			// 参数占位:非 bool 值给个 value 提示
			name, usage := flag.UnquoteUsage(f)
			line := "  --" + f.Name
			if name != "" {
				line += " " + name
			}
			fmt.Fprint(out, line+"\n")
			// 默认值不单独渲染:各参数 help 文案已自带"默认 X"说明,避免重复。
			fmt.Fprintf(out, "    \t%s\n", usage)
		})
	}
}

// mustConnect:校验 host/password 后连接,失败即退出。
func mustConnect(ctx context.Context, g *globalOpts) *session {
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

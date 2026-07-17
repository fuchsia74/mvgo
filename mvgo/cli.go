package main

import (
	"flag"
	"fmt"
	"os"
)

// 全局连接参数(所有子命令通用)。
type globalOpts struct {
	host     string
	port     int
	user     string
	password string
	insecure bool
	// flag 包无法直接表达 --no-xxx,用一个配套 bool 指针,解析后合并
	noInsecurePtr *bool
}

// finalizeInsecure 合并 --insecure/--no-insecure:显式 --no-insecure 优先关闭。
func (g *globalOpts) finalizeInsecure() {
	if g.noInsecurePtr != nil && *g.noInsecurePtr {
		g.insecure = false
	}
}

// 筛选参数(list / power-* / delete / customize 通用)。
type filterOpts struct {
	prefix string
	names  string
	all    bool
	state  string
}

// guest 定制参数(clone 与 customize 共用)。
type guestOpts struct {
	guestUser         string
	guestPassword     string
	guestReadyTimeout int
	// 网络:clone 用 startIP(批量递增),customize 用 ip(单台精确)
	startIP   string
	ip        string
	gateway   string
	dns       string
	netDevice string
	nmcliCon  string
	// 脚本
	runScript     string
	scriptInterp  string
	scriptArgs    string
	scriptTimeout int
}

// envOr 返回环境变量值,空则回退到 def。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// addGlobal 把全局连接参数注册到某个 FlagSet。
func addGlobal(fs *flag.FlagSet, g *globalOpts) {
	fs.StringVar(&g.host, "host", "", "ESXi 主机地址(必填)")
	fs.IntVar(&g.port, "port", 443, "端口")
	fs.StringVar(&g.user, "user", envOr("ESXI_USER", "root"),
		"ESXi 账号,默认读环境变量 ESXI_USER,再默认 root")
	fs.StringVar(&g.password, "password", os.Getenv("ESXI_PASSWORD"),
		"密码,默认读环境变量 ESXI_PASSWORD")
	// --insecure 默认 true;--no-insecure 关闭校验
	fs.BoolVar(&g.insecure, "insecure", true,
		"跳过SSL证书校验(自签证书需要);加 --no-insecure 则开启校验。默认跳过")
	noInsecure := fs.Bool("no-insecure", false, "开启SSL证书校验")
	// 解析后在 finalizeInsecure 里合并(flag 包无法直接表达 --no-xxx)
	g.noInsecurePtr = noInsecure
}

// addFilters 注册筛选参数。
func addFilters(fs *flag.FlagSet, f *filterOpts) {
	fs.StringVar(&f.prefix, "prefix", "", "名称前缀匹配")
	fs.StringVar(&f.names, "names", "", "精确名单,逗号分隔")
	fs.BoolVar(&f.all, "all", false, "匹配所有虚机")
	fs.StringVar(&f.state, "state", "",
		"仅匹配某电源状态(poweredOn/poweredOff/suspended)")
}

// ipMode 决定 guest 网络参数形态:批量(clone)或单台(customize)。
type ipMode int

const (
	ipBatch  ipMode = iota // clone: --start-ip
	ipSingle               // customize: --ip
)

// addGuestOpts 注册 guest 定制参数。guestUserRequired 仅影响后续校验。
func addGuestOpts(fs *flag.FlagSet, o *guestOpts, mode ipMode) {
	fs.StringVar(&o.guestUser, "guest-user", os.Getenv("GUEST_USER"),
		"guest 内账号(如 root),默认读环境变量 GUEST_USER")
	fs.StringVar(&o.guestPassword, "guest-password", os.Getenv("GUEST_PASSWORD"),
		"guest 内密码,默认读环境变量 GUEST_PASSWORD")
	fs.IntVar(&o.guestReadyTimeout, "guest-ready-timeout", 180,
		"等 VMware Tools 就绪的超时秒数(默认180)")
	if mode == ipSingle {
		fs.StringVar(&o.ip, "ip", "",
			"配IP(CIDR,如 192.168.1.5/24);单台定制,要求筛选只命中 1 台")
	} else {
		fs.StringVar(&o.startIP, "start-ip", "",
			"批量配IP:起始IP(CIDR,如 192.168.1.100/24),对新建虚机按序号依次递增分配")
	}
	fs.StringVar(&o.gateway, "gateway", "", "网关,如 192.168.1.1")
	fs.StringVar(&o.dns, "dns", "8.8.8.8", "DNS,逗号分隔,默认 8.8.8.8")
	fs.StringVar(&o.netDevice, "net-device", "",
		"要配置的网卡名(如 eth0/ens192),默认自动识别(默认路由网卡 -> 第一块物理以太网)")
	fs.StringVar(&o.nmcliCon, "nmcli-con", "",
		"nmcli 场景下要修改的连接名,默认按网卡自动取/新建(仅当 guest 用 NetworkManager 时生效)")
	fs.StringVar(&o.runScript, "run-script", "",
		"在 guest 内执行的本地脚本文件(上传后运行),可用环境变量 VM_NAME/VM_INDEX/VM_IP 做逐机差异化配置")
	fs.StringVar(&o.scriptInterp, "script-interpreter", "/bin/bash",
		"执行脚本的解释器 guest 内路径(默认 /bin/bash)")
	fs.StringVar(&o.scriptArgs, "script-args", "",
		"传给脚本的参数(整串按 shell 规则拆分)")
	fs.IntVar(&o.scriptTimeout, "script-timeout", 300,
		"等脚本执行完的超时秒数(默认300),超时判为失败")
}

// die 打印错误到 stderr 并以非0退出。
func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
	os.Exit(1)
}

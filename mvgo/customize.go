package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/vmware/govmomi/vim25/types"
)

// customizeGuest 通过 VMware Tools 在 Linux guest 内做定制。
// 步骤:等 Tools 就绪 ->(可选)改主机名 ->(可选)配静态IP ->(可选)跑用户脚本。
// setHostname=false 跳过改名;ipCIDR="" 跳过 IP 配置;runScript="" 跳过脚本。
func customizeGuest(ctx context.Context, s *session, v *vmRef, newName string,
	index int, ipCIDR string, o *guestOpts, setHostname bool) error {
	fmt.Println("  等待 VMware Tools 就绪...")
	if err := waitGuestReady(ctx, v, o.guestReadyTimeout); err != nil {
		return err
	}
	gs, err := newGuestSession(ctx, s, v, o.guestUser, o.guestPassword)
	if err != nil {
		return err
	}

	// 1) 改主机名(可选)
	if setHostname {
		fmt.Printf("  设置主机名 -> %s\n", newName)
		rc, err := gs.runInGuest(ctx, "/usr/bin/hostnamectl",
			"set-hostname "+newName, 60)
		if err != nil {
			return err
		}
		if rc != 0 {
			return fmt.Errorf("hostnamectl 返回非0退出码: %d", rc)
		}
	}

	// 2) 配静态 IP(可选)
	if ipCIDR != "" {
		fmt.Printf("  设置IP -> %s  网关 %s  DNS %s\n", ipCIDR, o.gateway, o.dns)
		script, err := buildNetcfgScript(ipCIDR, o.gateway, o.dns, o.nmcliCon, o.netDevice)
		if err != nil {
			return err
		}
		rc, output := gs.runBashCapture(ctx, script, 120)
		targetIP := ipHost(ipCIDR)
		if strings.Contains(output, targetIP) {
			fmt.Printf("    ✓ IP 已确认生效: %s\n", targetIP)
		} else {
			fmt.Printf("    ✗ IP 未确认生效(退出码 %d)。guest 内输出:\n", rc)
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				fmt.Printf("      | %s\n", line)
			}
			return fmt.Errorf("IP 配置未生效: %s", targetIP)
		}
	}

	// 3) 执行用户脚本(可选)。注入 VM_NAME/VM_INDEX/VM_IP。
	if o.runScript != "" {
		fmt.Printf("  执行脚本 %s(解释器 %s)\n", o.runScript, o.scriptInterp)
		vmIP := ""
		if ipCIDR != "" {
			vmIP = ipHost(ipCIDR)
		}
		env := map[string]string{
			"VM_NAME":  newName,
			"VM_INDEX": fmt.Sprintf("%d", index),
			"VM_IP":    vmIP,
		}
		rc, output, err := gs.runGuestScript(ctx, o.runScript, o.scriptInterp,
			o.scriptArgs, env, o.scriptTimeout)
		if strings.TrimSpace(output) != "" {
			fmt.Println("    脚本输出:")
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				fmt.Printf("      | %s\n", line)
			}
		}
		if err != nil {
			return err
		}
		if rc != 0 {
			return fmt.Errorf("脚本执行返回非0退出码: %d", rc)
		}
		fmt.Println("    ✓ 脚本执行完成(退出码 0)")
	}
	return nil
}

// validateGuestOpts 校验 guest 定制参数(clone 与 customize 共用)。
// 缺密码时交互式补输。校验通过返回,不通过直接退出。
func validateGuestOpts(o *guestOpts, hint string) {
	var missing []string
	if o.guestUser == "" {
		missing = append(missing, "--guest-user")
	}
	if o.guestPassword == "" {
		pw := promptPassword("guest 内密码: ")
		if pw == "" {
			missing = append(missing, "--guest-password / GUEST_PASSWORD")
		} else {
			o.guestPassword = pw
		}
	}
	if len(missing) > 0 {
		die("%s 需要以下参数: %s", hint, strings.Join(missing, ", "))
	}
	if o.startIP != "" {
		checkIPCIDR(o.startIP, "--start-ip", o.gateway)
	}
	if o.ip != "" {
		checkIPCIDR(o.ip, "--ip", o.gateway)
	}
	if o.runScript != "" && !isFile(o.runScript) {
		die("--run-script 指定的脚本不存在或不是文件: %s", o.runScript)
	}
}

// checkIPCIDR 校验一个 IP 参数:网关必填 + CIDR 格式合法。不合法直接退出。
func checkIPCIDR(cidr, flagName, gateway string) {
	if gateway == "" {
		die("指定 %s 时必须同时给 --gateway。", flagName)
	}
	if _, err := computeIP(cidr, 0); err != nil {
		die("%s 格式无效(应为 IP/前缀,如 192.168.1.100/24): %v", flagName, err)
	}
}

func runCustomize(ctx context.Context, g *globalOpts, args []string) {
	var f filterOpts
	var o guestOpts
	fs := subFlagSet("customize")
	addFilters(fs, &f)
	setHostname := fs.Bool("set-hostname", false, "把主机名设为虚机名(默认不改主机名)")
	yes := fs.Bool("yes", false, "跳过二次确认")
	addGuestOpts(fs, &o, ipSingle)
	fs.Parse(args)

	enforceFilter(&f)
	validateGuestOpts(&o, "customize")

	s := mustConnect(ctx, g)
	vms, err := s.selectVMs(ctx, &f)
	if err != nil {
		die("%v", err)
	}
	ensureNonEmpty(vms)

	// 单台命令:筛选必须恰好命中 1 台
	if len(vms) != 1 {
		die("customize 是单台操作,当前筛选命中 %d 台。"+
			"请用更精确的 --names/--prefix 定位到 1 台;"+
			"批量定制请循环调用,或建机时用 clone --customize。", len(vms))
	}
	v := vms[0]

	if !*setHostname && o.ip == "" && o.runScript == "" {
		die("customize 至少要指定一项动作:--set-hostname / --ip / --run-script。")
	}

	var actions []string
	if *setHostname {
		actions = append(actions, "改主机名")
	}
	if o.ip != "" {
		actions = append(actions, fmt.Sprintf("配IP(%s)", o.ip))
	}
	if o.runScript != "" {
		actions = append(actions, "跑脚本 "+baseName(o.runScript))
	}
	if !confirm([]*vmRef{v}, "定制("+strings.Join(actions, " / ")+")", *yes) {
		fmt.Println("已取消")
		return
	}

	if v.powerState() != string(types.VirtualMachinePowerStatePoweredOn) {
		die("虚机 %s 未开机(%s),定制需开机且 Tools 就绪。", v.name(), v.powerState())
	}

	if err := customizeGuest(ctx, s, v, v.name(), 0, o.ip, &o, *setHostname); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %s 定制失败: %v\n", v.name(), err)
		os.Exit(1)
	}
	fmt.Printf("\n✓ %s 定制完成\n", v.name())
}

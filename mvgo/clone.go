package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/vmware/govmomi/vim25/types"
)

func runClone(ctx context.Context, g *globalOpts, args []string) {
	var o guestOpts
	fs := subFlagSet("clone")
	source := fs.String("source", "", "源(模板)虚机名称")
	prefix := fs.String("prefix", "", "新虚机名前缀")
	count := fs.Int("count", 0, "复制数量")
	start := fs.Int("start", 1, "起始序号(默认1)")
	pad := fs.Int("pad", 2, "序号补零位数(默认2 -> 01)")
	datastore := fs.String("datastore", "", "目标datastore,默认与源相同")
	powerOn := fs.Bool("power-on", false, "创建后自动开机")
	linked := fs.Bool("linked", false,
		"链接克隆:不复制磁盘,建delta差分盘依赖源盘(秒级、省空间)")
	snapshotName := fs.String("snapshot-name", "linked-clone-base",
		"链接克隆所需的源虚机快照名,不存在则自动创建")
	customize := fs.Bool("customize", false,
		"创建后经 VMware Tools 改主机名/配IP/跑脚本(自动隐含 --power-on;需 --guest-user)")
	addGuestOpts(fs, &o, ipBatch)
	fs.Parse(args)

	if *source == "" || *prefix == "" || *count <= 0 {
		die("clone 需要 --source、--prefix、--count(且 count>0)")
	}

	doPowerOn := *powerOn
	if *customize {
		doPowerOn = true // 定制必须先开机
		validateGuestOpts(&o, "--customize")
		if o.startIP == "" && o.runScript == "" {
			fmt.Fprintln(os.Stderr,
				"提示: --customize 未指定 --start-ip 或 --run-script,将只设置主机名。")
		}
	} else if o.startIP != "" || o.runScript != "" {
		fmt.Fprintln(os.Stderr,
			"提示: 指定了 --start-ip/--run-script 但未加 --customize,这些参数将被忽略。")
	}

	s := mustConnect(ctx, g)
	src, err := s.findVM(ctx, *source)
	if err != nil {
		die("%v", err)
	}
	if src == nil {
		die("找不到源虚机 '%s'", *source)
	}

	// 完整复制需源机关机;链接克隆靠快照冻结父盘,源机可运行。
	if !*linked && src.powerState() != string(types.VirtualMachinePowerStatePoweredOff) {
		fmt.Fprintf(os.Stderr,
			"错误: 完整复制模式下源虚机 '%s' 必须关机才能安全复制磁盘。\n", *source)
		fmt.Fprintf(os.Stderr, "      当前状态: %s\n", src.powerState())
		fmt.Fprintln(os.Stderr, "      提示: 加 --linked 走链接克隆则允许源机运行。")
		os.Exit(1)
	}

	if src.mo.Config == nil {
		die("源虚机 '%s' 无 config 信息", *source)
	}
	srcDS := datastoreFromPath(src.mo.Config.Files.VmPathName)
	targetDS := *datastore
	if targetDS == "" {
		targetDS = srcDS
	}

	modeDesc := "完整复制"
	if *linked {
		modeDesc = "链接克隆(delta 差分盘)"
	}
	fmt.Printf("源虚机: %s  (datastore: %s)\n", *source, srcDS)
	fmt.Printf("目标 datastore: %s\n", targetDS)
	fmt.Printf("模式: %s\n", modeDesc)
	fmt.Printf("计划复制 %d 台,命名 %s%s ...\n\n",
		*count, *prefix, padNum(*start, *pad))

	cl := &cloner{s: s, src: src, targetDS: targetDS, linked: *linked}

	// 链接克隆:先确保源机有只读快照作为共享父盘
	if *linked {
		fmt.Println("准备链接克隆父盘快照:")
		snap, devs, err := cl.ensureSnapshot(ctx, *snapshotName)
		if err != nil {
			die("无法获取/创建源虚机快照: %v", err)
		}
		cl.snapshot = snap
		cl.snapDevices = devs
		fmt.Println()
	}

	var created []string
	for i := 0; i < *count; i++ {
		num := *start + i
		newName := fmt.Sprintf("%s%s", *prefix, padNum(num, *pad))
		fmt.Printf("[%d/%d] 复制为 %s\n", i+1, *count, newName)
		newVM, err := cl.cloneOne(ctx, newName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s 失败: %v\n\n", newName, err)
			continue
		}
		created = append(created, newName)
		if doPowerOn {
			fmt.Printf("  开机 %s\n", newName)
			task, err := newVM.obj.PowerOn(ctx)
			if err == nil {
				err = task.Wait(ctx)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %s 开机失败: %v\n\n", newName, err)
				continue
			}
		}
		if *customize {
			// IP 跟随虚机名序号(num-1),而非循环下标
			ipCIDR := ""
			if o.startIP != "" {
				ipCIDR, err = computeIP(o.startIP, num-1)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ✗ %s IP 计算失败: %v\n\n", newName, err)
					continue
				}
			}
			if err := customizeGuest(ctx, s, newVM, newName, i, ipCIDR, &o, true); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %s 定制失败: %v\n\n", newName, err)
				continue
			}
		}
		fmt.Printf("  ✓ %s 完成\n\n", newName)
	}

	fmt.Printf("\n完成: 成功 %d/%d 台\n", len(created), *count)
	if len(created) > 0 {
		fmt.Println("已创建:", strings.Join(created, ", "))
	}
}

// datastoreFromPath 从 "[datastore1] dir/vm.vmx" 提取 datastore 名。
func datastoreFromPath(path string) string {
	if i := indexOf(path, "]"); i >= 0 {
		return strings.Trim(path[:i], "[ ")
	}
	return strings.Trim(path, "[ ")
}

// padNum 序号补零(pad=2 -> 01)。
func padNum(n, pad int) string {
	return fmt.Sprintf("%0*d", pad, n)
}

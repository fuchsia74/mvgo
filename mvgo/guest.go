package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vmware/govmomi/guest"
	"github.com/vmware/govmomi/guest/toolbox"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// guestSession 打包一台虚机的 guest 操作句柄。
type guestSession struct {
	client *vim25.Client
	vm     *vmRef
	auth   *types.NamePasswordAuthentication
	pm     *guest.ProcessManager
	tb     *toolbox.Client // 仅用于文件上传下载
}

func newGuestSession(ctx context.Context, s *session, v *vmRef,
	user, password string) (*guestSession, error) {
	auth := &types.NamePasswordAuthentication{Username: user, Password: password}
	om := guest.NewOperationsManager(s.client.Client, v.obj.Reference())
	pm, err := om.ProcessManager(ctx)
	if err != nil {
		return nil, err
	}
	tb, err := toolbox.NewClient(ctx, s.client.Client, v.obj.Reference(), auth)
	if err != nil {
		return nil, err
	}
	return &guestSession{
		client: s.client.Client, vm: v, auth: auth, pm: pm, tb: tb,
	}, nil
}

// waitGuestReady 等 VMware Tools 就绪(guest 操作依赖它)。超时返回错误。
func waitGuestReady(ctx context.Context, v *vmRef, timeout int) error {
	for waited := 0; waited < timeout; waited += 5 {
		status, err := currentToolsStatus(ctx, v)
		if err == nil && status == "guestToolsRunning" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	st, _ := currentToolsStatus(ctx, v)
	return fmt.Errorf("等待 VMware Tools 就绪超时(%ds),当前状态: %s。"+
		"确认 guest 已装并运行 VMware Tools", timeout, st)
}

// currentToolsStatus 实时取 tools 运行状态。
func currentToolsStatus(ctx context.Context, v *vmRef) (string, error) {
	var vmm mo.VirtualMachine
	if err := v.obj.Properties(ctx, v.obj.Reference(), []string{"guest"}, &vmm); err != nil {
		return "", err
	}
	if vmm.Guest != nil {
		return vmm.Guest.ToolsRunningStatus, nil
	}
	return "", nil
}

// runInGuest 在 guest 内执行一条命令,轮询等待退出,返回退出码。
// timeout>0 时超时未结束返回错误(进程仍在 guest 内运行)。
func (gs *guestSession) runInGuest(ctx context.Context, program, arguments string,
	timeout int) (int32, error) {
	spec := &types.GuestProgramSpec{ProgramPath: program, Arguments: arguments}
	pid, err := gs.pm.StartProgram(ctx, gs.auth, spec)
	if err != nil {
		return -1, err
	}
	waited := 0
	for {
		procs, err := gs.pm.ListProcesses(ctx, gs.auth, []int64{pid})
		if err != nil {
			return -1, err
		}
		if len(procs) > 0 && procs[0].EndTime != nil {
			return procs[0].ExitCode, nil
		}
		if timeout > 0 && waited >= timeout {
			return -1, fmt.Errorf("guest 内进程超时(%ds)未结束: %s", timeout, program)
		}
		time.Sleep(2 * time.Second)
		waited += 2
	}
}

// downloadGuestFile 把 guest 内文件下载为字符串。
func (gs *guestSession) downloadGuestFile(ctx context.Context, path string) (string, error) {
	rc, _, err := gs.tb.Download(ctx, path)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// uploadGuestFile 把内容写入 guest 内文件(覆盖已存在)。
func (gs *guestSession) uploadGuestFile(ctx context.Context, path string, data []byte) error {
	// 必须从 DefaultUpload 起(它设了 Method="PUT");直接用 soap.Upload{}
	// 会留空 Method,net/http 默认发 GET,ESXi 文件传输端点返回 405。
	p := soap.DefaultUpload
	p.ContentLength = int64(len(data))
	attr := &types.GuestFileAttributes{}
	return gs.tb.Upload(ctx, bytes.NewReader(data), path, p, attr, true)
}

// runBashCapture 在 guest 内用 bash 跑一段脚本,输出重定向到临时文件后下载。
// 返回 (退出码, 输出文本)。timeout 秒内未结束视为失败。
func (gs *guestSession) runBashCapture(ctx context.Context, script string,
	timeout int) (int32, string) {
	outFile := "/tmp/.vm_clone_cust.out"
	// base64 传脚本,规避引号/空格/特殊字符转义
	b64 := b64encode(script)
	wrapper := fmt.Sprintf("echo %s | base64 -d | bash > %s 2>&1", b64, outFile)
	rc, err := gs.runInGuest(ctx, "/bin/bash",
		fmt.Sprintf("-c %s", shellQuote(wrapper)), timeout)
	if err != nil {
		return -1, fmt.Sprintf("(执行失败: %v)", err)
	}
	out, err := gs.downloadGuestFile(ctx, outFile)
	if err != nil {
		out = fmt.Sprintf("(无法读取命令输出: %v)", err)
	}
	return rc, out
}

// runGuestScript 把本地脚本上传到 guest 并执行,注入 env,收集输出。
func (gs *guestSession) runGuestScript(ctx context.Context, localPath, interpreter,
	scriptArgs string, env map[string]string, timeout int) (int32, string, error) {
	data, err := readFile(localPath)
	if err != nil {
		return -1, "", err
	}
	base := baseName(localPath)
	if base == "" {
		base = "user_script"
	}
	guestScript := "/tmp/.vm_clone_" + base
	outFile := "/tmp/.vm_clone_script.out"
	if err := gs.uploadGuestFile(ctx, guestScript, data); err != nil {
		return -1, "", err
	}
	// 组装:导出 env -> chmod -> 解释器执行 -> 输出重定向
	var envParts []string
	for k, v := range env {
		envParts = append(envParts, fmt.Sprintf("%s=%s", k, shellQuote(v)))
	}
	envPrefix := strings.Join(envParts, " ")
	cmd := fmt.Sprintf("chmod +x %s 2>/dev/null; %s %s %s %s",
		shellQuote(guestScript), envPrefix, shellQuote(interpreter),
		shellQuote(guestScript), scriptArgs)
	cmd = strings.TrimSpace(cmd)
	wrapper := fmt.Sprintf("%s > %s 2>&1", cmd, outFile)
	rc, err := gs.runInGuest(ctx, "/bin/bash",
		fmt.Sprintf("-c %s", shellQuote(wrapper)), timeout)
	if err != nil {
		return rc, "", err
	}
	out, derr := gs.downloadGuestFile(ctx, outFile)
	if derr != nil {
		out = fmt.Sprintf("(无法读取脚本输出: %v)", derr)
	}
	return rc, out, nil
}

#!/usr/bin/env python3
"""
ESXi 虚拟机批量管理(直连单台 ESXi,无 vCenter)。

子命令:
  list        列出虚机(名称/电源/CPU/内存/IP/Tools状态)
  power-on    批量开机
  power-off   批量关机(默认优雅关机 ShutdownGuest,--hard 走硬断电)
  clone       批量复制虚机(完整复制 / 链接克隆,可选创建后 guest 定制)
  delete      批量删除虚机(默认连磁盘文件一并删除,--keep-files 仅注销)
  customize   对单台已有虚机做 guest 定制(改主机名/配IP/跑脚本;批量循环调用或用 clone)

筛选(list / power-* / delete / customize 通用):
  --prefix web        名称前缀匹配
  --names a,b,c       精确名单(逗号分隔)
  --all               所有虚机(危险操作会二次确认)
  --state poweredOn   仅匹配某电源状态
不给筛选条件时,list 默认列全部;power-* / delete / customize 则拒绝执行(避免误操作)。

clone 原理:CloneVM_Task 在独立 ESXi 上不可用,故走文件级复制:
  完整复制(默认):复制整块 vmdk,克隆机独立,源机须关机。
  链接克隆(--linked):给源盘打只读快照,新机建 delta 差分盘指向父盘,
    秒级创建、几乎不占空间,但永久依赖源盘和该快照,源机可运行。

用法:
  export ESXI_PASSWORD='xxx'
  ./venv/bin/python manage_vms.py --host 192.168.1.10 list
  ./venv/bin/python manage_vms.py --host 192.168.1.10 list --prefix web
  ./venv/bin/python manage_vms.py --host 192.168.1.10 power-on --prefix web
  ./venv/bin/python manage_vms.py --host 192.168.1.10 power-off --prefix web --hard
  # 完整复制
  ./venv/bin/python manage_vms.py --host 192.168.1.10 clone \\
      --source base-template --prefix web --count 5 --start 1 --power-on
  # 链接克隆 + guest 定制(改名/配IP,自动识别 netplan/nmcli/networkd/ifupdown)
  ./venv/bin/python manage_vms.py --host 192.168.1.10 clone \\
      --source base-template --prefix web --count 5 --linked \\
      --customize --guest-user root --start-ip 192.168.1.100/24 --gateway 192.168.1.1
  # 定制时再跑一个本地脚本做自动化配置(脚本内可用 VM_NAME/VM_INDEX/VM_IP)
  ./venv/bin/python manage_vms.py --host 192.168.1.10 clone \\
      --source base-template --prefix web --count 5 --linked \\
      --customize --guest-user root --start-ip 192.168.1.100/24 --gateway 192.168.1.1 \\
      --run-script ./bootstrap.sh --script-args "--role web"
  # 批量删除(连磁盘,开机的先强制断电)
  ./venv/bin/python manage_vms.py --host 192.168.1.10 delete --prefix web --force
  # 仅从清单注销,保留磁盘文件(可日后重新注册)
  ./venv/bin/python manage_vms.py --host 192.168.1.10 delete --names web01 --keep-files
  # 对单台已有虚机定制(改名+精确配IP+跑脚本),筛选须只命中 1 台
  ./venv/bin/python manage_vms.py --host 192.168.1.10 customize --names db-primary \\
      --guest-user root --set-hostname --ip 192.168.1.9/24 --gateway 192.168.1.1 \\
      --run-script ./bootstrap.sh
  # 批量定制已有机器 = 循环逐台调用(或建机时直接用 clone --customize)
"""
import argparse
import atexit
import base64
import concurrent.futures
import getpass
import ipaddress
import os
import shlex
import ssl
import sys
import time
import urllib.request

from pyVim.connect import SmartConnect, Disconnect
from pyVmomi import vim


def get_args():
    p = argparse.ArgumentParser(
        description="ESXi 虚拟机批量管理",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""环境变量:
  ESXI_USER       ESXi 账号,等价于 --user;未设则默认 root。
  ESXI_PASSWORD   ESXi 登录密码,等价于 --password;未设则交互式提示输入。
  GUEST_USER      guest 内账号,等价于 --guest-user;
                  设了它,customize 子命令的 --guest-user 就不再强制必填。
  GUEST_PASSWORD  guest 内账号密码,等价于 --guest-password;
                  仅 clone --customize / customize 需要,未设则交互式提示输入。

命令行显式传参优先级高于环境变量。密码走环境变量可避免明文进命令行历史:
  export ESXI_PASSWORD='xxx'
""")
    p.add_argument("--host", required=True, help="ESXi 主机地址")
    p.add_argument("--port", type=int, default=443)
    p.add_argument("--user", default=os.environ.get("ESXI_USER", "root"),
                   help="ESXi 账号,默认读环境变量 ESXI_USER,再默认 root")
    p.add_argument("--password", default=os.environ.get("ESXI_PASSWORD"),
                   help="密码,默认读环境变量 ESXI_PASSWORD")
    p.add_argument("--insecure", action=argparse.BooleanOptionalAction,
                   default=True,
                   help="跳过SSL证书校验(自签证书需要);"
                        "加 --no-insecure 则开启校验。默认跳过")

    sub = p.add_subparsers(dest="command", required=True)

    def add_filters(sp):
        sp.add_argument("--prefix", help="名称前缀匹配")
        sp.add_argument("--names", help="精确名单,逗号分隔")
        sp.add_argument("--all", action="store_true", help="匹配所有虚机")
        sp.add_argument("--state", choices=["poweredOn", "poweredOff", "suspended"],
                        help="仅匹配某电源状态")

    def add_guest_opts(sp, guest_user_required=False, ip_mode="batch"):
        """guest 定制通用参数(clone 与 customize 共用),归入独立参数组便于 --help 阅读。
        ip_mode 决定配 IP 参数的形态:
          'batch'  -> --start-ip,按名称排序依次递增(clone 批量建机用)。
          'single' -> --ip,单台精确指定(customize 单台定制用,要求只命中 1 台)。
        批量定制已有机器 = 循环调 customize,而非在 customize 里做批量。"""
        # 凭据
        gc = sp.add_argument_group("guest 凭据(需 VMware Tools)")
        # 有 GUEST_USER 环境变量时即视为已提供,故此时不强制命令行必填
        gc.add_argument("--guest-user", default=os.environ.get("GUEST_USER"),
                        required=guest_user_required and not os.environ.get("GUEST_USER"),
                        help="guest 内账号(如 root),默认读环境变量 GUEST_USER")
        gc.add_argument("--guest-password", default=os.environ.get("GUEST_PASSWORD"),
                        help="guest 内密码,默认读环境变量 GUEST_PASSWORD")
        gc.add_argument("--guest-ready-timeout", type=int, default=180,
                        help="等 VMware Tools 就绪的超时秒数(默认180)")
        # 网络配置(自动识别 netplan/nmcli/networkd/ifupdown)
        gn = sp.add_argument_group("guest 网络配置(Linux,自动识别网络管理方式)")
        if ip_mode == "single":
            gn.add_argument("--ip", metavar="CIDR",
                            help="配IP(CIDR,如 192.168.1.5/24);单台定制,"
                                 "要求筛选只命中 1 台")
        else:
            gn.add_argument("--start-ip",
                            help="批量配IP:起始IP(CIDR,如 192.168.1.100/24),"
                                 "对新建虚机按序号依次递增分配")
        gn.add_argument("--gateway", help="网关,如 192.168.1.1")
        gn.add_argument("--dns", default="8.8.8.8",
                        help="DNS,逗号分隔,默认 8.8.8.8")
        gn.add_argument("--net-device", default=None,
                        help="要配置的网卡名(如 eth0/ens192),默认自动识别"
                             "(默认路由网卡 -> 第一块物理以太网)")
        gn.add_argument("--nmcli-con", default=None,
                        help="nmcli 场景下要修改的连接名,默认按网卡自动取/新建"
                             "(仅当 guest 用 NetworkManager 时生效)")
        # 开机后执行脚本
        gs = sp.add_argument_group("guest 脚本执行")
        gs.add_argument("--run-script", metavar="PATH",
                        help="在 guest 内执行的本地脚本文件(上传后运行),"
                             "可用环境变量 VM_NAME/VM_INDEX/VM_IP 做逐机差异化配置")
        gs.add_argument("--script-interpreter", default="/bin/bash",
                        help="执行脚本的解释器 guest 内路径(默认 /bin/bash)")
        gs.add_argument("--script-args", default="",
                        help="传给脚本的参数(整串按 shell 规则拆分)")
        gs.add_argument("--script-timeout", type=int, default=300,
                        help="等脚本执行完的超时秒数(默认300),超时判为失败")

    sp_list = sub.add_parser("list", help="列出虚机")
    add_filters(sp_list)

    sp_on = sub.add_parser("power-on", help="批量开机")
    add_filters(sp_on)
    sp_on.add_argument("--yes", action="store_true", help="跳过二次确认")
    sp_on.add_argument("--workers", type=int, default=8, help="并发数(默认8)")

    sp_off = sub.add_parser("power-off", help="批量关机")
    add_filters(sp_off)
    sp_off.add_argument("--hard", action="store_true",
                        help="硬断电(PowerOff),默认优雅关机(ShutdownGuest)")
    sp_off.add_argument("--yes", action="store_true", help="跳过二次确认")
    sp_off.add_argument("--workers", type=int, default=8, help="并发数(默认8)")

    sp_del = sub.add_parser("delete", help="批量删除虚机")
    add_filters(sp_del)
    sp_del.add_argument("--keep-files", action="store_true",
                        help="仅从清单注销(UnregisterVM),保留磁盘文件;"
                             "默认连同磁盘一并删除(Destroy)")
    sp_del.add_argument("--force", action="store_true",
                        help="删除前对开机中的虚机先硬断电;不加则跳过开机的虚机")
    sp_del.add_argument("--yes", action="store_true", help="跳过二次确认")
    sp_del.add_argument("--workers", type=int, default=8, help="并发数(默认8)")

    sp_clone = sub.add_parser("clone", help="批量复制虚机")
    sp_clone.add_argument("--source", required=True, help="源(模板)虚机名称")
    sp_clone.add_argument("--prefix", required=True, help="新虚机名前缀")
    sp_clone.add_argument("--count", type=int, required=True, help="复制数量")
    sp_clone.add_argument("--start", type=int, default=1, help="起始序号(默认1)")
    sp_clone.add_argument("--pad", type=int, default=2,
                          help="序号补零位数(默认2 -> 01)")
    sp_clone.add_argument("--datastore", default=None,
                          help="目标datastore,默认与源相同")
    sp_clone.add_argument("--power-on", action="store_true", help="创建后自动开机")
    sp_clone.add_argument("--linked", action="store_true",
                          help="链接克隆:不复制磁盘,建delta差分盘依赖源盘(秒级、省空间)")
    sp_clone.add_argument("--snapshot-name", default="linked-clone-base",
                          help="链接克隆所需的源虚机快照名,不存在则自动创建")

    sp_clone.add_argument("--customize", action="store_true",
                          help="创建后经 VMware Tools 改主机名/配IP/跑脚本"
                               "(自动隐含 --power-on;需 --guest-user)")
    add_guest_opts(sp_clone)

    sp_cust = sub.add_parser("customize",
                             help="对单台已有虚机做 guest 定制(改主机名/配IP/跑脚本);"
                                  "批量请循环调用或用 clone --customize")
    add_filters(sp_cust)
    sp_cust.add_argument("--set-hostname", action="store_true",
                         help="把主机名设为虚机名(默认不改主机名)")
    sp_cust.add_argument("--yes", action="store_true", help="跳过二次确认")
    add_guest_opts(sp_cust, guest_user_required=True, ip_mode="single")

    return p.parse_args()


def connect(args):
    ctx = ssl._create_unverified_context() if args.insecure else None
    si = SmartConnect(host=args.host, user=args.user,
                      pwd=args.password, port=args.port, sslContext=ctx)
    atexit.register(Disconnect, si)
    return si


def all_vms(content):
    """返回所有 VirtualMachine 对象。"""
    view = content.viewManager.CreateContainerView(
        content.rootFolder, [vim.VirtualMachine], True)
    try:
        return list(view.view)
    finally:
        view.Destroy()


def find_vm(content, name):
    """按名称查找虚机。"""
    for vm in all_vms(content):
        if vm.name == name:
            return vm
    return None


def select_vms(content, args):
    """按筛选条件返回虚机列表。互斥优先级: names > prefix > all。"""
    vms = all_vms(content)

    if args.names:
        wanted = [n.strip() for n in args.names.split(",") if n.strip()]
        by_name = {vm.name: vm for vm in vms}
        result = []
        for n in wanted:
            if n in by_name:
                result.append(by_name[n])
            else:
                print(f"警告: 找不到虚机 '{n}',已跳过", file=sys.stderr)
        vms = result
    elif args.prefix:
        vms = [vm for vm in vms if vm.name.startswith(args.prefix)]
    elif args.all:
        pass  # 全部
    else:
        # list 无条件 = 全部;power-*/delete/customize 无条件在 main 里已拦截
        pass

    if args.state:
        vms = [vm for vm in vms if vm.runtime.powerState == args.state]

    return vms


def wait_for_task(task, label=""):
    """轮询等待 Task 完成,返回 result 或抛异常。"""
    while task.info.state in (vim.TaskInfo.State.queued,
                              vim.TaskInfo.State.running):
        time.sleep(1)
    if task.info.state == vim.TaskInfo.State.success:
        return task.info.result
    msg = task.info.error.msg if task.info.error else "未知错误"
    raise RuntimeError(f"{label} 失败: {msg}" if label else msg)


# ============================== list ==============================

def fmt_mem(mb):
    """MB -> 人类可读。"""
    if mb is None:
        return "-"
    if mb >= 1024:
        return f"{mb/1024:.0f}GB"
    return f"{mb}MB"


def get_ip(vm):
    """取 guest IP,没有则返回 '-'。"""
    ip = vm.guest.ipAddress if vm.guest else None
    return ip or "-"


def cmd_list(vms):
    """打印虚机表格。"""
    if not vms:
        print("(无匹配的虚机)")
        return
    # 表头
    hdr = f"{'名称':<24} {'电源':<11} {'CPU':>4} {'内存':>7} {'IP':<16} {'Tools':<12}"
    print(hdr)
    print("-" * len(hdr))
    for vm in sorted(vms, key=lambda v: v.name):
        power = vm.runtime.powerState
        cpu = vm.config.hardware.numCPU if vm.config else "-"
        mem = fmt_mem(vm.config.hardware.memoryMB if vm.config else None)
        ip = get_ip(vm)
        tools = vm.guest.toolsRunningStatus if vm.guest else "-"
        print(f"{vm.name:<24} {power:<11} {str(cpu):>4} {mem:>7} "
              f"{ip:<16} {tools:<12}")
    print(f"\n共 {len(vms)} 台")


# ============================== power ==============================

def power_on_one(vm):
    """开机单台。已开机则跳过。返回 (name, ok, msg)。"""
    if vm.runtime.powerState == vim.VirtualMachinePowerState.poweredOn:
        return (vm.name, True, "已是开机状态,跳过")
    try:
        wait_for_task(vm.PowerOn())
        return (vm.name, True, "已开机")
    except Exception as e:
        return (vm.name, False, str(e))


def power_off_one(vm, hard):
    """
    关机单台。已关机则跳过。
    hard=False: ShutdownGuest(需 Tools),轮询等待进入 poweredOff。
    hard=True : PowerOff 硬断电。
    返回 (name, ok, msg)。
    """
    if vm.runtime.powerState == vim.VirtualMachinePowerState.poweredOff:
        return (vm.name, True, "已是关机状态,跳过")
    try:
        if hard:
            wait_for_task(vm.PowerOff())
            return (vm.name, True, "已硬断电")
        # 优雅关机不返回 Task,需装 Tools;轮询等待关机
        if vm.guest is None or vm.guest.toolsRunningStatus != "guestToolsRunning":
            return (vm.name, False,
                    "VMware Tools 未运行,无法优雅关机(可加 --hard 硬断电)")
        vm.ShutdownGuest()
        # 最多等 120s 进入 poweredOff
        waited = 0
        while waited < 120:
            if vm.runtime.powerState == vim.VirtualMachinePowerState.poweredOff:
                return (vm.name, True, "已优雅关机")
            time.sleep(3)
            waited += 3
        return (vm.name, False, "优雅关机超时(120s),客户机可能未响应")
    except Exception as e:
        return (vm.name, False, str(e))


def delete_one(vm, keep_files, force):
    """
    删除单台。返回 (name, ok, msg)。
    keep_files=True : UnregisterVM,仅从清单移除,磁盘文件保留(可日后重注册)。
    keep_files=False: Destroy_Task,连同磁盘文件一并删除(不可恢复)。
    Destroy/Unregister 都要求虚机非开机状态:
      force=True  开机的先硬断电再删;
      force=False 开机的直接跳过,不误删运行中的机器。
    """
    try:
        if vm.runtime.powerState != vim.VirtualMachinePowerState.poweredOff:
            if not force:
                return (vm.name, False, "虚机开机中,已跳过(加 --force 先断电再删)")
            wait_for_task(vm.PowerOff(), "断电")
        if keep_files:
            # UnregisterVM 是同步调用,不返回 Task
            vm.UnregisterVM()
            return (vm.name, True, "已从清单注销(磁盘文件保留)")
        wait_for_task(vm.Destroy_Task(), "删除")
        return (vm.name, True, "已删除(含磁盘文件)")
    except Exception as e:
        return (vm.name, False, str(e))


def run_batch(vms, worker_fn, workers, action_label):
    """并发执行批量电源操作,汇总结果。"""
    print(f"\n开始{action_label} {len(vms)} 台(并发 {workers})...\n")
    ok, fail = [], []
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as ex:
        futures = {ex.submit(worker_fn, vm): vm for vm in vms}
        for fut in concurrent.futures.as_completed(futures):
            name, success, msg = fut.result()
            mark = "✓" if success else "✗"
            print(f"  {mark} {name}: {msg}")
            (ok if success else fail).append(name)
    print(f"\n完成: 成功 {len(ok)}/{len(vms)}", end="")
    if fail:
        print(f",失败 {len(fail)}: {', '.join(fail)}")
    else:
        print()


def confirm(vms, action_label, skip):
    """危险操作二次确认。skip=True(--yes)则跳过。返回是否继续。"""
    if skip:
        return True
    print(f"\n即将{action_label}以下 {len(vms)} 台虚机:")
    for vm in sorted(vms, key=lambda v: v.name):
        print(f"  - {vm.name}  ({vm.runtime.powerState})")
    ans = input(f"\n确认{action_label}? 输入 yes 继续: ").strip().lower()
    return ans == "yes"


# ============================== clone ==============================

def get_datastore_from_path(path):
    """从 '[datastore1] dir/vm.vmx' 提取 datastore 名。"""
    return path.split("]")[0].strip("[ ")


def get_datacenter(content, vm):
    """向上找到虚机所属的 Datacenter。"""
    parent = vm.parent
    while parent is not None and not isinstance(parent, vim.Datacenter):
        parent = parent.parent
    if parent is None:
        # 兜底:取第一个 datacenter
        for child in content.rootFolder.childEntity:
            if isinstance(child, vim.Datacenter):
                return child
    return parent


def build_device_specs(source_vm, disk_backings, create_disk=False):
    """
    构建新虚机的设备 ConfigSpec 列表。
    控制器原样复制;磁盘按顺序套用传入的 disk_backings(第 i 块盘用第 i 个 backing);
    网卡保留类型/网络,MAC 自动重新生成。
    disk_backings 由调用方按模式(完整复制/链接克隆)预先构建好。

    create_disk:
      True (链接克隆) — 设 fileOperation=create,让 ESXi 现场生成 delta 差分盘;
      False(完整复制)— vmdk 已由 CopyVirtualDisk 复制好,仅引用不重建。
    """
    device_specs = []
    disk_index = 0
    for dev in source_vm.config.hardware.device:
        # 控制器(SCSI/SATA/IDE/NVMe)原样复制
        if isinstance(dev, vim.vm.device.VirtualController):
            spec = vim.vm.device.VirtualDeviceSpec()
            spec.operation = vim.vm.device.VirtualDeviceSpec.Operation.add
            spec.device = dev
            device_specs.append(spec)
        # 磁盘:套用预构建的 backing
        elif isinstance(dev, vim.vm.device.VirtualDisk):
            spec = vim.vm.device.VirtualDeviceSpec()
            spec.operation = vim.vm.device.VirtualDeviceSpec.Operation.add
            if create_disk:
                # 链接克隆:delta 盘文件尚不存在,让 ESXi 依据 backing.parent 创建
                spec.fileOperation = \
                    vim.vm.device.VirtualDeviceSpec.FileOperation.create
            spec.device = dev
            spec.device.backing = disk_backings[disk_index]
            device_specs.append(spec)
            disk_index += 1
        # 网卡:复制类型和网络,MAC 让 ESXi 自动生成
        elif isinstance(dev, vim.vm.device.VirtualEthernetCard):
            spec = vim.vm.device.VirtualDeviceSpec()
            spec.operation = vim.vm.device.VirtualDeviceSpec.Operation.add
            spec.device = dev
            spec.device.macAddress = ""
            spec.device.addressType = "generated"
            spec.device.key = 0  # 让系统重新分配
            device_specs.append(spec)
    return device_specs


def ensure_snapshot(source_vm, snap_name):
    """
    链接克隆前提:源虚机需有一个只读快照作为父盘冻结点。
    找到同名快照则复用,否则创建。返回 snapshot 对象(vim.vm.Snapshot)。
    """
    def _search(nodes):
        for n in nodes:
            if n.name == snap_name:
                return n.snapshot
            found = _search(n.childSnapshotList)
            if found:
                return found
        return None

    if source_vm.snapshot is not None:
        existing = _search(source_vm.snapshot.rootSnapshotList)
        if existing:
            print(f"  复用已有快照 '{snap_name}'")
            return existing

    print(f"  为源虚机创建快照 '{snap_name}'(链接克隆父盘冻结点)")
    wait_for_task(
        source_vm.CreateSnapshot_Task(
            name=snap_name,
            description="Base snapshot for linked clones",
            memory=False, quiesce=False),
        "创建快照")
    # 重新查找刚建的快照
    return _search(source_vm.snapshot.rootSnapshotList)


def build_linked_backings(snapshot, target_ds_name, new_name):
    """
    链接克隆:每块盘建一个 delta 差分盘,parent 指向快照时刻的只读父盘。
    从 snapshot.config.hardware.device 取父盘路径(快照后这些盘变只读)。
    """
    backings = []
    disk_num = 0
    for dev in snapshot.config.hardware.device:
        if isinstance(dev, vim.vm.device.VirtualDisk):
            parent_backing = vim.vm.device.VirtualDisk.FlatVer2BackingInfo()
            parent_backing.fileName = dev.backing.fileName
            parent_backing.datastore = dev.backing.datastore
            parent_backing.diskMode = dev.backing.diskMode

            child = vim.vm.device.VirtualDisk.FlatVer2BackingInfo()
            child.fileName = (f"[{target_ds_name}] {new_name}/"
                              f"{new_name}_{disk_num}-delta.vmdk")
            child.diskMode = "persistent"
            child.parent = parent_backing  # 关键:形成 delta -> 父盘链
            backings.append(child)
            disk_num += 1
    return backings


def clone_one(content, source_vm, new_name, target_ds_name,
              linked=False, snapshot=None):
    """复制单个虚机:构建磁盘 backing -> 建配置 -> 注册。

    linked=False: 完整复制,CopyVirtualDisk 复制每块 vmdk。
    linked=True : 链接克隆,建 delta 差分盘指向 snapshot 的父盘,不复制数据。
    """
    dc = get_datacenter(content, source_vm)
    vdm = content.virtualDiskManager
    fm = content.fileManager

    # 目标 datastore 对象
    target_ds = None
    for ds in source_vm.runtime.host.datastore:
        if ds.name == target_ds_name:
            target_ds = ds
            break
    if target_ds is None:
        raise RuntimeError(f"找不到 datastore: {target_ds_name}")

    # 新虚机目录(MakeDirectory 是同步调用,不返回 Task)
    new_dir = f"[{target_ds_name}] {new_name}"
    print(f"  创建目录 {new_dir}")
    try:
        fm.MakeDirectory(name=new_dir, datacenter=dc,
                         createParentDirectories=True)
    except vim.fault.FileAlreadyExists:
        pass

    if linked:
        # 链接克隆:delta 盘由 CreateVM 时按 backing.parent 自动生成,无需复制
        print(f"  链接克隆:基于快照建 delta 差分盘(不复制数据)")
        disk_backings = build_linked_backings(snapshot, target_ds_name, new_name)
    else:
        # 完整复制:逐块复制 vmdk,再构建 backing 指向副本
        disk_backings = []
        disk_num = 0
        for dev in source_vm.config.hardware.device:
            if isinstance(dev, vim.vm.device.VirtualDisk):
                src_vmdk = dev.backing.fileName
                dst_vmdk = (f"[{target_ds_name}] {new_name}/"
                            f"{new_name}_{disk_num}.vmdk")
                print(f"  复制磁盘 {src_vmdk} -> {dst_vmdk}")
                wait_for_task(
                    vdm.CopyVirtualDisk_Task(
                        sourceName=src_vmdk, sourceDatacenter=dc,
                        destName=dst_vmdk, destDatacenter=dc, force=True),
                    "复制磁盘")
                backing = vim.vm.device.VirtualDisk.FlatVer2BackingInfo()
                backing.datastore = target_ds
                backing.fileName = dst_vmdk
                backing.diskMode = "persistent"
                backing.thinProvisioned = True
                disk_backings.append(backing)
                disk_num += 1

    # 构建 ConfigSpec
    config = vim.vm.ConfigSpec()
    config.name = new_name
    config.memoryMB = source_vm.config.hardware.memoryMB
    config.numCPUs = source_vm.config.hardware.numCPU
    config.guestId = source_vm.config.guestId
    config.files = vim.vm.FileInfo()
    config.files.vmPathName = new_dir
    config.deviceChange = build_device_specs(source_vm, disk_backings,
                                             create_disk=linked)

    # 注册/创建虚机
    host = source_vm.runtime.host
    pool = host.parent.resourcePool if hasattr(host.parent, "resourcePool") \
        else source_vm.resourcePool
    vm_folder = dc.vmFolder
    print(f"  注册虚机 {new_name}")
    new_vm = wait_for_task(
        vm_folder.CreateVM_Task(config=config, pool=pool, host=host),
        "创建虚机")
    return new_vm


# ===================== guest 操作(VMware Tools 通道) =====================
# 依赖顺序:底层原语(wait/run/上传下载)-> 组合(跑脚本/配网)-> customize_guest。

def wait_guest_ready(vm, timeout):
    """等 VMware Tools 就绪(guest 操作依赖它)。超时抛异常。"""
    waited = 0
    while waited < timeout:
        status = vm.guest.toolsRunningStatus
        if status == "guestToolsRunning":
            return
        time.sleep(5)
        waited += 5
    raise RuntimeError(
        f"等待 VMware Tools 就绪超时({timeout}s),"
        f"当前状态: {vm.guest.toolsRunningStatus}。"
        f"确认 guest 已装并运行 VMware Tools。")


def run_in_guest(content, vm, guest_auth, program, arguments, timeout=None):
    """在 guest 内执行一条命令,轮询等待退出,返回退出码。
    timeout 非 None 时,超时未结束抛 TimeoutError(进程仍在 guest 内运行)。"""
    pm = content.guestOperationsManager.processManager
    spec = vim.vm.guest.ProcessManager.ProgramSpec(
        programPath=program, arguments=arguments)
    pid = pm.StartProgramInGuest(vm=vm, auth=guest_auth, spec=spec)
    waited = 0
    while True:
        procs = pm.ListProcessesInGuest(vm=vm, auth=guest_auth, pids=[pid])
        if procs and procs[0].endTime is not None:
            return procs[0].exitCode
        if timeout is not None and waited >= timeout:
            raise TimeoutError(f"guest 内进程超时({timeout}s)未结束: {program}")
        time.sleep(2)
        waited += 2


def download_guest_file(content, vm, guest_auth, esxi_host, guest_path):
    """通过 Tools 文件传输通道把 guest 内文件下载为字符串。"""
    fmg = content.guestOperationsManager.fileManager
    info = fmg.InitiateFileTransferFromGuest(
        vm=vm, auth=guest_auth, guestFilePath=guest_path)
    # info.url 里主机名可能是 '*',需替换为实际 ESXi 地址
    url = info.url.replace("*", esxi_host)
    ctx = ssl._create_unverified_context()
    with urllib.request.urlopen(url, context=ctx, timeout=30) as resp:
        return resp.read().decode("utf-8", errors="replace")


def upload_guest_file(content, vm, guest_auth, esxi_host, guest_path, data):
    """通过 Tools 文件传输通道把 bytes 写入 guest 内文件(覆盖已存在)。"""
    fmg = content.guestOperationsManager.fileManager
    attrs = vim.vm.guest.FileManager.FileAttributes()
    url = fmg.InitiateFileTransferToGuest(
        vm=vm, auth=guest_auth, guestFilePath=guest_path,
        fileAttributes=attrs, fileSize=len(data), overwrite=True)
    # info.url 里主机名可能是 '*',需替换为实际 ESXi 地址
    url = url.replace("*", esxi_host)
    ctx = ssl._create_unverified_context()
    req = urllib.request.Request(url, data=data, method="PUT")
    with urllib.request.urlopen(req, context=ctx, timeout=60):
        pass


def run_guest_script(content, vm, guest_auth, esxi_host, local_path,
                     interpreter, script_args, env, timeout):
    """
    把本地脚本上传到 guest 并执行,stdout+stderr 收集后下载。
    env: dict,作为环境变量前缀注入(供脚本按机器差异化配置)。
    返回 (退出码, 输出文本)。
    """
    with open(local_path, "rb") as f:
        payload = f.read()
    # 上传到 guest 临时路径(用基础名,避免本地目录结构泄漏到 guest)
    base = os.path.basename(local_path) or "user_script"
    guest_script = f"/tmp/.vm_clone_{base}"
    out_file = "/tmp/.vm_clone_script.out"
    upload_guest_file(content, vm, guest_auth, esxi_host, guest_script, payload)

    # 组装:导出 env -> chmod -> 用解释器执行 -> 输出重定向到文件
    env_prefix = " ".join(f"{k}={shlex.quote(str(v))}" for k, v in env.items())
    cmd = (f"chmod +x {shlex.quote(guest_script)} 2>/dev/null; "
           f"{env_prefix} {shlex.quote(interpreter)} "
           f"{shlex.quote(guest_script)} {script_args}".strip())
    wrapper = f"{cmd} > {out_file} 2>&1"
    rc = run_in_guest(content, vm, guest_auth, "/bin/bash",
                      f"-c {shlex.quote(wrapper)}", timeout=timeout)
    try:
        output = download_guest_file(content, vm, guest_auth,
                                     esxi_host, out_file)
    except Exception as e:
        output = f"(无法读取脚本输出: {e})"
    return rc, output


def run_bash_capture(content, vm, guest_auth, esxi_host, script, timeout=120):
    """
    在 guest 内用 bash 跑一段脚本,把 stdout+stderr 重定向到临时文件后下载。
    返回 (退出码, 输出文本)。用于需要看 guest 内实际报错的场景。
    timeout 秒内未结束抛 TimeoutError,避免 guest 内脚本挂死拖住整个流程。
    """
    out_file = "/tmp/.vm_clone_cust.out"
    # 用 base64 传脚本,彻底规避引号/空格/特殊字符的转义地狱
    b64 = base64.b64encode(script.encode("utf-8")).decode("ascii")
    wrapper = (f"echo {b64} | base64 -d | bash > {out_file} 2>&1")
    rc = run_in_guest(content, vm, guest_auth, "/bin/bash",
                      f"-c \"{wrapper}\"", timeout=timeout)
    try:
        output = download_guest_file(content, vm, guest_auth,
                                     esxi_host, out_file)
    except Exception as e:
        output = f"(无法读取命令输出: {e})"
    return rc, output


def build_netcfg_script(ip_cidr, gateway, dns, nmcli_con, net_device=None):
    """
    生成在 guest 内配置静态 IP 的 bash 脚本,自动识别网络管理方式并适配。

    检测顺序(优先级即代码顺序):
      netplan  -> Ubuntu server 等,写 /etc/netplan/*.yaml 后 netplan apply。
                  即便后端是 NetworkManager,netplan 也是 Ubuntu 上的权威配置层,
                  故排在 nmcli 前(否则重启会被 netplan 覆盖)。
      nmcli    -> NetworkManager 在管的系统(RHEL/Kylin/Fedora/Ubuntu desktop)。
      networkd -> systemd-networkd 在管,写 /etc/systemd/network/*.network。
      ifupdown -> 经典 Debian,写 /etc/network/interfaces[.d]。
    netplan 仅存在于 Debian 系,故 RHEL 系依旧走 nmcli,行为不变。

    Python 侧只负责把 IP/前缀/掩码/网关/DNS/网卡算好传下去;
    检测与差异化落在 guest 端脚本(脚本关注 ip、掩码、网卡等信息)。
    """
    iface = ipaddress.ip_interface(ip_cidr)
    ip = str(iface.ip)
    prefix = iface.network.prefixlen
    netmask = str(iface.netmask)
    dns_val = dns.replace(",", " ").strip()

    # 变量头:值经 shlex.quote 转义后作为 bash 变量注入
    header = (
        "set -x\n"
        f"IP={shlex.quote(ip)}\n"
        f"PREFIX={shlex.quote(str(prefix))}\n"
        f"NETMASK={shlex.quote(netmask)}\n"
        f"GW={shlex.quote(gateway)}\n"
        f"DNS={shlex.quote(dns_val)}\n"
        f"FORCE_DEV={shlex.quote(net_device or '')}\n"
        f"NMCLI_CON={shlex.quote(nmcli_con or '')}\n"
    )
    # 主体为原样 bash(含 awk/heredoc 的花括号),用 raw 串避免 f-string 转义
    body = r'''CIDR="$IP/$PREFIX"

# --- 解析目标网卡:优先 --net-device;否则默认路由网卡;再否则第一块物理以太网 ---
if [ -n "$FORCE_DEV" ]; then
    DEV="$FORCE_DEV"
else
    DEV="$(ip route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')"
    if [ -z "$DEV" ]; then
        for p in /sys/class/net/*; do
            n="$(basename "$p")"
            [ "$n" = "lo" ] && continue
            [ "$(cat "$p/type" 2>/dev/null)" = "1" ] || continue
            case "$n" in docker*|veth*|br-*|virbr*|vnet*|tap*) continue;; esac
            DEV="$n"; break
        done
    fi
fi
if [ -z "$DEV" ]; then echo "ERROR: 找不到可配置的以太网设备"; exit 1; fi
echo "目标设备: $DEV"

apply_nmcli() {
    if [ -n "$NMCLI_CON" ]; then
        CON="$NMCLI_CON"
    else
        CON="$(nmcli -t -f GENERAL.CONNECTION device show "$DEV" 2>/dev/null | cut -d: -f2)"
    fi
    if [ -z "$CON" ] || [ "$CON" = "--" ]; then
        CON="static-$DEV"
        nmcli connection add type ethernet ifname "$DEV" con-name "$CON" 2>/dev/null || true
    fi
    echo "nmcli 连接: $CON"
    nmcli connection modify "$CON" ipv4.addresses "$CIDR" ipv4.gateway "$GW" ipv4.dns "$DNS" ipv4.method manual
    nmcli device reapply "$DEV" 2>/dev/null || { nmcli connection down "$CON" 2>/dev/null || true; nmcli connection up "$CON"; }
}

apply_netplan() {
    DNSC="$(echo "$DNS" | tr ' ' ',')"
    F=/etc/netplan/99-clone-static.yaml
    cat > "$F" <<EOF
network:
  version: 2
  ethernets:
    $DEV:
      dhcp4: false
      addresses: [$CIDR]
      routes:
        - to: 0.0.0.0/0
          via: $GW
      nameservers:
        addresses: [$DNSC]
EOF
    chmod 600 "$F"
    netplan apply
}

apply_networkd() {
    F="/etc/systemd/network/99-clone-$DEV.network"
    {
        echo "[Match]"
        echo "Name=$DEV"
        echo ""
        echo "[Network]"
        echo "Address=$CIDR"
        echo "Gateway=$GW"
        for d in $DNS; do echo "DNS=$d"; done
    } > "$F"
    systemctl restart systemd-networkd
}

apply_ifupdown() {
    if [ -d /etc/network/interfaces.d ] && grep -qs "interfaces.d" /etc/network/interfaces; then
        F="/etc/network/interfaces.d/99-clone-$DEV"
    else
        F=/etc/network/interfaces
    fi
    {
        echo ""
        echo "auto $DEV"
        echo "iface $DEV inet static"
        echo "    address $IP"
        echo "    netmask $NETMASK"
        echo "    gateway $GW"
        echo "    dns-nameservers $DNS"
    } >> "$F"
    ifdown "$DEV" 2>/dev/null || true
    ifup "$DEV" 2>/dev/null || systemctl restart networking 2>/dev/null || service networking restart
}

# --- 识别网络管理方式并应用 ---
if command -v netplan >/dev/null 2>&1 && [ -d /etc/netplan ]; then
    METHOD=netplan
elif command -v nmcli >/dev/null 2>&1 && systemctl is-active --quiet NetworkManager 2>/dev/null; then
    METHOD=nmcli
elif systemctl is-active --quiet systemd-networkd 2>/dev/null; then
    METHOD=networkd
elif [ -e /etc/network/interfaces ]; then
    METHOD=ifupdown
else
    echo "ERROR: 无法识别网络管理方式(nmcli/netplan/networkd/ifupdown 均不适用)"; exit 1
fi
echo "网络管理方式: $METHOD"
apply_"$METHOD"

sleep 2
echo "=== 生效后的 IP ==="
ip -4 addr show "$DEV" | grep -o 'inet [0-9.]*/[0-9]*' || true
'''
    return header + body


def customize_guest(content, vm, new_name, index, ip_cidr, gateway, dns,
                    guest_user, guest_password, nmcli_con, ready_timeout,
                    esxi_host, net_device=None, run_script=None,
                    script_interpreter="/bin/bash",
                    script_args="", script_timeout=300, set_hostname=True):
    """
    通过 VMware Tools 在 Linux guest 内做定制。
    步骤:等 Tools 就绪 ->(可选)hostnamectl 改名 ->(可选)配静态IP ->(可选)跑用户脚本。
    set_hostname 为 False 时跳过改名;ip_cidr 为 None 时跳过 IP 配置;
    run_script 为 None 时跳过脚本执行。
    IP 配置自动识别 netplan/nmcli/systemd-networkd/ifupdown。
    """
    print(f"  等待 VMware Tools 就绪...")
    wait_guest_ready(vm, ready_timeout)

    auth = vim.vm.guest.NamePasswordAuthentication(
        username=guest_user, password=guest_password)

    # 1) 改主机名(可选)
    if set_hostname:
        print(f"  设置主机名 -> {new_name}")
        rc = run_in_guest(content, vm, auth, "/usr/bin/hostnamectl",
                          f"set-hostname {new_name}")
        if rc != 0:
            raise RuntimeError(f"hostnamectl 返回非0退出码: {rc}")

    # 2) 配静态 IP(可选),抓取 guest 内实际输出用于诊断
    if ip_cidr:
        print(f"  设置IP -> {ip_cidr}  网关 {gateway}  DNS {dns}")
        script = build_netcfg_script(ip_cidr, gateway, dns, nmcli_con,
                                     net_device)
        rc, output = run_bash_capture(content, vm, auth, esxi_host, script)

        # 校验:输出里应出现目标 IP
        target_ip = ip_cidr.split("/")[0]
        if target_ip in output:
            print(f"    ✓ IP 已确认生效: {target_ip}")
        else:
            print(f"    ✗ IP 未确认生效(退出码 {rc})。guest 内输出:")
            for line in output.strip().splitlines():
                print(f"      | {line}")
            raise RuntimeError(f"IP 配置未生效: {target_ip}")

    # 3) 执行用户脚本(可选)。注入 VM_NAME/VM_INDEX/VM_IP 供逐机差异化。
    if run_script:
        print(f"  执行脚本 {run_script}(解释器 {script_interpreter})")
        env = {"VM_NAME": new_name, "VM_INDEX": index,
               "VM_IP": ip_cidr.split("/")[0] if ip_cidr else ""}
        rc, output = run_guest_script(
            content, vm, auth, esxi_host, run_script,
            script_interpreter, script_args, env, script_timeout)
        # 无论成败都回显脚本输出,便于排查
        if output.strip():
            print(f"    脚本输出:")
            for line in output.strip().splitlines():
                print(f"      | {line}")
        if rc != 0:
            raise RuntimeError(f"脚本执行返回非0退出码: {rc}")
        print(f"    ✓ 脚本执行完成(退出码 0)")


def compute_ip(start_ip_cidr, index):
    """起始 CIDR(如 192.168.1.100/24)按 index 递增,返回新的 'ip/prefix'。"""
    iface = ipaddress.ip_interface(start_ip_cidr)
    new_addr = iface.ip + index
    return f"{new_addr}/{iface.network.prefixlen}"


def check_ip_cidr(cidr, flag, gateway):
    """校验一个 IP 参数:网关必填 + CIDR 格式合法。不合法直接退出。"""
    if not gateway:
        print(f"错误: 指定 {flag} 时必须同时给 --gateway。", file=sys.stderr)
        sys.exit(1)
    try:
        compute_ip(cidr, 0)  # 借 compute_ip 校验 CIDR 格式
    except ValueError as e:
        print(f"错误: {flag} 格式无效(应为 IP/前缀,如 192.168.1.100/24): {e}",
              file=sys.stderr)
        sys.exit(1)


def validate_guest_opts(args, hint):
    """
    guest 定制参数校验(clone 与 customize 共用),提前失败避免半途才发现缺参。
    缺密码时交互式补输。校验通过返回,不通过直接退出。
    hint 用于提示信息里指代当前动作(如 '--customize' / 'customize')。
    配 IP 参数按命令不同:clone 用 --start-ip(批量),customize 用 --ip(单台);
    这里对存在的那个做「网关必填 + CIDR 格式」校验。
    """
    missing = []
    if not args.guest_user:
        missing.append("--guest-user")
    if not args.guest_password:
        args.guest_password = getpass.getpass("guest 内密码: ")
        if not args.guest_password:
            missing.append("--guest-password / GUEST_PASSWORD")
    if missing:
        print(f"错误: {hint} 需要以下参数: {', '.join(missing)}", file=sys.stderr)
        sys.exit(1)
    if getattr(args, "start_ip", None):
        check_ip_cidr(args.start_ip, "--start-ip", args.gateway)
    if getattr(args, "ip", None):
        check_ip_cidr(args.ip, "--ip", args.gateway)
    # 脚本文件须存在且可读(提前失败,别等真跑起来才发现)
    if args.run_script and not os.path.isfile(args.run_script):
        print(f"错误: --run-script 指定的脚本不存在或不是文件: {args.run_script}",
              file=sys.stderr)
        sys.exit(1)


def cmd_clone(content, args):
    """clone 子命令驱动:校验参数 -> 定位源机 -> 批量复制 ->(可选)开机/定制。"""
    # guest 定制参数校验(提前失败,避免建完机才发现缺参数)
    if args.customize:
        args.power_on = True  # 定制必须先开机
        validate_guest_opts(args, "--customize")
        # clone 定制总会改主机名;配IP和跑脚本可选,都不给则只改名
        if not args.start_ip and not args.run_script:
            print("提示: --customize 未指定 --start-ip 或 --run-script,"
                  "将只设置主机名。", file=sys.stderr)
    elif args.start_ip or args.run_script:
        # 给了定制相关参数却没加 --customize:大概率是漏了,提醒避免静默忽略
        print("提示: 指定了 --start-ip/--run-script 但未加 --customize,"
              "这些参数将被忽略(clone 不会做 guest 定制)。", file=sys.stderr)

    source_vm = find_vm(content, args.source)
    if source_vm is None:
        print(f"错误: 找不到源虚机 '{args.source}'", file=sys.stderr)
        sys.exit(1)

    # 完整复制需源虚机关机(冷克隆);链接克隆靠快照冻结父盘,源机可运行。
    if not args.linked and \
            source_vm.runtime.powerState != vim.VirtualMachinePowerState.poweredOff:
        print(f"错误: 完整复制模式下源虚机 '{args.source}' 必须关机才能安全复制磁盘。",
              file=sys.stderr)
        print(f"      当前状态: {source_vm.runtime.powerState}", file=sys.stderr)
        print(f"      提示: 加 --linked 走链接克隆则允许源机运行。", file=sys.stderr)
        sys.exit(1)

    src_ds_name = get_datastore_from_path(source_vm.config.files.vmPathName)
    target_ds_name = args.datastore or src_ds_name

    mode_desc = "链接克隆(delta 差分盘)" if args.linked else "完整复制"
    print(f"源虚机: {args.source}  (datastore: {src_ds_name})")
    print(f"目标 datastore: {target_ds_name}")
    print(f"模式: {mode_desc}")
    print(f"计划复制 {args.count} 台,命名 {args.prefix}"
          f"{str(args.start).zfill(args.pad)} ...\n")

    # 链接克隆:先确保源虚机有一个只读快照作为共享父盘
    base_snapshot = None
    if args.linked:
        print("准备链接克隆父盘快照:")
        base_snapshot = ensure_snapshot(source_vm, args.snapshot_name)
        if base_snapshot is None:
            print("错误: 无法获取/创建源虚机快照", file=sys.stderr)
            sys.exit(1)
        print()

    created = []
    for i in range(args.count):
        num = args.start + i
        new_name = f"{args.prefix}{str(num).zfill(args.pad)}"
        print(f"[{i+1}/{args.count}] 复制为 {new_name}")
        try:
            new_vm = clone_one(content, source_vm, new_name, target_ds_name,
                               linked=args.linked, snapshot=base_snapshot)
            created.append(new_name)
            if args.power_on:
                print(f"  开机 {new_name}")
                wait_for_task(new_vm.PowerOn(), "开机")
            if args.customize:
                # IP 跟随虚机名序号(num-1),而非循环下标:
                # 这样 --start 5 建出的 web05 拿到 .104 而非 .100,
                # 与 --start 1 批次不冲突,也与名字对得上。
                ip_cidr = compute_ip(args.start_ip, num - 1) \
                    if args.start_ip else None
                customize_guest(
                    content, new_vm, new_name, i, ip_cidr,
                    args.gateway, args.dns,
                    args.guest_user, args.guest_password,
                    args.nmcli_con, args.guest_ready_timeout,
                    args.host,
                    net_device=args.net_device,
                    run_script=args.run_script,
                    script_interpreter=args.script_interpreter,
                    script_args=args.script_args,
                    script_timeout=args.script_timeout)
            print(f"  ✓ {new_name} 完成\n")
        except Exception as e:
            print(f"  ✗ {new_name} 失败: {e}\n", file=sys.stderr)

    print(f"\n完成: 成功 {len(created)}/{args.count} 台")
    if created:
        print("已创建:", ", ".join(created))


def cmd_customize(content, args, vms):
    """customize 子命令驱动:对**单台**已有虚机做 guest 定制。

    定位:单台操作。批量对已有机器定制 = 循环调用本命令,或建机时用 clone --customize。
    因此要求筛选恰好命中 1 台;IP 用 --ip 精确指定(无「按位置递增」的隐式分配)。
    改主机名默认关闭(--set-hostname 才开);虚机须开机(guest 操作依赖 Tools)。
    """
    validate_guest_opts(args, "customize")

    # 单台命令:筛选必须恰好命中 1 台,避免一条命令误改一批
    if len(vms) != 1:
        print(f"错误: customize 是单台操作,当前筛选命中 {len(vms)} 台。"
              f"请用更精确的 --names/--prefix 定位到 1 台;"
              f"批量定制请循环调用,或建机时用 clone --customize。", file=sys.stderr)
        sys.exit(1)
    vm = vms[0]

    if not (args.set_hostname or args.ip or args.run_script):
        print("错误: customize 至少要指定一项动作:"
              "--set-hostname / --ip / --run-script。", file=sys.stderr)
        sys.exit(1)

    actions = []
    if args.set_hostname:
        actions.append("改主机名")
    if args.ip:
        actions.append(f"配IP({args.ip})")
    if args.run_script:
        actions.append(f"跑脚本 {os.path.basename(args.run_script)}")
    if not confirm([vm], "定制(" + " / ".join(actions) + ")", args.yes):
        print("已取消")
        return

    # 定制要求虚机开机
    if vm.runtime.powerState != vim.VirtualMachinePowerState.poweredOn:
        print(f"错误: 虚机 {vm.name} 未开机({vm.runtime.powerState}),"
              f"定制需开机且 Tools 就绪。", file=sys.stderr)
        sys.exit(1)

    try:
        customize_guest(
            content, vm, vm.name, 0, args.ip,
            args.gateway, args.dns,
            args.guest_user, args.guest_password,
            args.nmcli_con, args.guest_ready_timeout,
            args.host,
            net_device=args.net_device,
            run_script=args.run_script,
            script_interpreter=args.script_interpreter,
            script_args=args.script_args,
            script_timeout=args.script_timeout,
            set_hostname=args.set_hostname)
        print(f"\n✓ {vm.name} 定制完成")
    except Exception as e:
        print(f"\n✗ {vm.name} 定制失败: {e}", file=sys.stderr)
        sys.exit(1)


def main():
    args = get_args()
    if not args.password:
        args.password = getpass.getpass("ESXi 密码: ")

    # power-* / delete / customize 必须有明确筛选条件,防止空条件误伤
    if args.command in ("power-on", "power-off", "delete", "customize"):
        if not (args.names or args.prefix or args.all or args.state):
            print("错误: 该操作必须指定筛选条件"
                  "(--prefix/--names/--all/--state),防止误操作。",
                  file=sys.stderr)
            sys.exit(1)

    si = connect(args)
    content = si.RetrieveContent()

    if args.command == "clone":
        cmd_clone(content, args)
        return

    vms = select_vms(content, args)

    if args.command == "list":
        cmd_list(vms)
        return

    if not vms:
        print("(无匹配的虚机,未执行任何操作)")
        return

    if args.command == "power-on":
        # --all 或 --state 这类大范围,仍需确认
        big = args.all or (args.state and not (args.prefix or args.names))
        if big and not confirm(vms, "开机", args.yes):
            print("已取消")
            return
        run_batch(vms, power_on_one, args.workers, "开机")

    elif args.command == "power-off":
        mode = "硬断电" if args.hard else "优雅关机"
        # 关机风险高于开机,--all/--hard 都要求确认
        need_confirm = args.all or args.hard or \
            (args.state and not (args.prefix or args.names))
        if need_confirm and not confirm(vms, mode, args.yes):
            print("已取消")
            return
        run_batch(vms, lambda vm: power_off_one(vm, args.hard),
                  args.workers, mode)

    elif args.command == "delete":
        mode = "注销(保留磁盘)" if args.keep_files else "删除(含磁盘文件)"
        # 删除不可逆,风险最高:无论范围大小一律二次确认(仅 --yes 可跳过)
        if not confirm(vms, mode, args.yes):
            print("已取消")
            return
        run_batch(vms, lambda vm: delete_one(vm, args.keep_files, args.force),
                  args.workers, mode)

    elif args.command == "customize":
        cmd_customize(content, args, vms)


if __name__ == "__main__":
    main()




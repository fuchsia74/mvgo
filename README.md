# ESXi 虚拟机批量管理工具

基于 [pyvmomi](https://github.com/vmware/pyvmomi) 的 Python 脚本,**直连单台 ESXi 主机**(无 vCenter)完成虚拟机的批量复制与批量电源管理。

单一入口 `manage_vms.py`,六个子命令:

- `list` — 列出虚机
- `power-on` / `power-off` — 批量开机 / 关机
- `clone` — 批量复制虚机(完整复制 / 链接克隆),支持创建后自动改主机名和 IP
- `delete` — 批量删除虚机(默认连磁盘删,`--keep-files` 仅注销)
- `customize` — 对**单台**已有虚机做 guest 定制(改主机名 / 配 IP / 跑脚本;批量循环调用)

> ⚠️ **前提:ESXi 需带 license。** 免费版 ESXi 的 vSphere API 写操作被禁用,只读(如 `list`)可用,但复制、开关机等写操作会返回 `RestrictedVersion` 错误。

## 环境要求

- Python 3.9+(开发环境为 3.14)
- 带 license 的 ESXi 主机,网络可达 443 端口
- ESXi 账号(如 `root`)

## 安装

```bash
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```

或不激活环境,直接用 `./venv/bin/python` 运行脚本。

## 凭据配置

账号和密码都可通过环境变量传入,密码走环境变量还能避免明文进命令行历史:

```bash
export ESXI_USER='root'                   # 等价 --user,默认 root
export ESXI_PASSWORD='你的ESXi密码'         # 等价 --password
export GUEST_USER='root'                   # 等价 --guest-user
export GUEST_PASSWORD='guest内账号密码'      # 等价 --guest-password
```

| 环境变量 | 等价参数 | 说明 |
|------|------|------|
| `ESXI_USER` | `--user` | ESXi 账号,未设默认 `root` |
| `ESXI_PASSWORD` | `--password` | ESXi 密码,未设则交互式提示 |
| `GUEST_USER` | `--guest-user` | guest 内账号;设了它,`customize` 就不再强制要 `--guest-user` |
| `GUEST_PASSWORD` | `--guest-password` | guest 内密码;`clone --customize` / `customize` 需要,未设则交互式提示 |

命令行显式传参优先级高于环境变量。密码未提供时脚本会交互式提示输入。

---

## clone — 批量复制虚机

从一个源(模板)虚机批量复制出多台,命名规则为「前缀 + 序号」(如 `web01`、`web02`)。

### 两种复制模式

| | 完整复制(默认) | 链接克隆(`--linked`) |
|---|---|---|
| 速度 | 慢,逐块复制 vmdk | 秒级 |
| 空间 | 每台一份完整磁盘 | 每台仅占增量(delta) |
| 源虚机状态 | **必须关机** | 可运行(靠快照冻结父盘) |
| 独立性 | 完全独立,源机可删 | **永久依赖源盘 + 快照** |
| 适用场景 | 长期资产、生产机 | 大量临时 / 测试机 |

链接克隆会先给源虚机打一个只读快照作为共享父盘。**该快照和源盘一旦删除或损坏,所有克隆机全部报废。**

### 示例

```bash
# 完整复制 5 台(源虚机须先关机)
./venv/bin/python manage_vms.py --host 192.168.1.10 --user root clone \
    --source base-template --prefix web --count 5 --start 1 --power-on

# 链接克隆 5 台(源虚机可运行)
./venv/bin/python manage_vms.py --host 192.168.1.10 --user root clone \
    --source base-template --prefix web --count 5 --linked --power-on

# 链接克隆 + 开机后自动改主机名和IP
./venv/bin/python manage_vms.py --host 192.168.1.10 --user root clone \
    --source base-template --prefix web --count 5 --linked \
    --customize --guest-user root \
    --start-ip 192.168.1.100/24 --gateway 192.168.1.1 \
    --dns 192.168.1.1,8.8.8.8
```

最后一例生成 `web01`(IP `.100`)、`web02`(`.101`)…`web05`(`.104`),各自改好主机名和 IP。

### 主要参数

| 参数 | 说明 |
|------|------|
| `--source` | 源(模板)虚机名称(必填) |
| `--prefix` | 新虚机名前缀(必填) |
| `--count` | 复制数量(必填) |
| `--start` | 起始序号(默认 1) |
| `--pad` | 序号补零位数(默认 2 → `01`) |
| `--datastore` | 目标 datastore,默认与源相同 |
| `--power-on` | 创建后自动开机 |
| `--linked` | 使用链接克隆 |
| `--snapshot-name` | 链接克隆的源快照名(默认 `linked-clone-base`) |

### 开机后定制(`--customize`)

通过 VMware Tools 的 Guest Operations API 在客户机内部执行命令,改主机名、配 IP,并可跑一个自定义脚本做进一步自动化配置。**支持任意 Linux**:配 IP 时会在 guest 内自动识别网络管理方式并适配,脚本执行只要 guest 自带对应解释器即可。

定制按顺序执行:改主机名 →(给了 `--start-ip` 才)配静态 IP →(给了 `--run-script` 才)跑脚本。`--start-ip` 和 `--run-script` 都不给时,只改主机名。

| 参数 | 说明 |
|------|------|
| `--customize` | 启用定制(自动隐含 `--power-on`) |
| `--guest-user` | guest 内账号(如 `root`),必填 |
| `--guest-password` | guest 内密码,默认读 `GUEST_PASSWORD` |
| `--start-ip` | 起始 IP(CIDR,如 `192.168.1.100/24`),按虚机名序号分配(`web01`→起始IP,`web05`→+4);给了则 `--gateway` 必填 |
| `--gateway` | 网关 |
| `--dns` | DNS,逗号分隔(默认 `8.8.8.8`) |
| `--net-device` | 要配置的网卡名(如 `eth0`/`ens192`),默认自动识别 |
| `--nmcli-con` | nmcli 场景下要修改的连接名,默认按网卡自动取/新建(仅 NetworkManager 生效) |
| `--guest-ready-timeout` | 等 Tools 就绪的超时秒数(默认 180) |
| `--run-script` | 本地脚本文件,上传到 guest 后执行 |
| `--script-interpreter` | 执行脚本的解释器 guest 内路径(默认 `/bin/bash`) |
| `--script-args` | 传给脚本的参数(整串按 shell 规则拆分) |
| `--script-timeout` | 等脚本执行完的超时秒数(默认 300),超时判为失败 |

要求:目标机已装并运行 VMware Tools,且提供 guest 内账号密码。命令通过 Tools 虚拟通道下发,不依赖网络,因此改 IP 断网也不影响下发。

#### 网络配置的自动适配

Python 侧只把 IP、前缀、掩码、网关、DNS、网卡名算好传进 guest;**由 guest 内脚本在运行时识别网络管理方式**并写对应配置,顺序如下:

| 优先级 | 方式 | 判定条件 | 落地方式 |
|------|------|------|------|
| 1 | netplan | 有 `netplan` 命令且存在 `/etc/netplan` | 写 `/etc/netplan/99-clone-static.yaml` 后 `netplan apply` |
| 2 | NetworkManager | 有 `nmcli` 且 `NetworkManager` 服务在跑 | `nmcli connection modify/add` + `device reapply` |
| 3 | systemd-networkd | `systemd-networkd` 服务在跑 | 写 `/etc/systemd/network/99-clone-<dev>.network` 后重启服务 |
| 4 | ifupdown | 存在 `/etc/network/interfaces` | 追加 `iface … inet static` 后 `ifup`/重启 networking |

- **netplan 排在 nmcli 前**:Ubuntu 上即便后端是 NetworkManager,netplan 也是权威配置层,先写 netplan 才不会被开机时覆盖。netplan 仅 Debian 系存在,因此 RHEL/Kylin/Fedora 依旧走 nmcli,行为与之前一致。
- 网卡默认自动识别(默认路由网卡 → 第一块物理以太网,自动排除 `lo`/`docker*`/`veth*`/`virbr*` 等虚拟口),可用 `--net-device` 显式指定。
- 四种方式都不适用时报错退出,不静默跳过。

#### 自定义脚本(`--run-script`)

指定一个本地脚本文件,会被上传到 guest 的 `/tmp` 下并用指定解释器执行,stdout/stderr 全程回显便于排查,退出码非 0 视为该机定制失败。脚本执行时注入以下环境变量,方便同一份脚本按机器差异化配置:

| 环境变量 | 含义 |
|------|------|
| `VM_NAME` | 当前克隆机名(如 `web03`) |
| `VM_INDEX` | 从 0 起的序号(第几台) |
| `VM_IP` | 本机 IP(未配 IP 时为空串) |

```bash
# 每台机建好后,配好 IP 再跑 bootstrap.sh 做安装/注册等自动化
./venv/bin/python manage_vms.py --host 192.168.1.10 clone \
    --source base-template --prefix web --count 5 --linked \
    --customize --guest-user root \
    --start-ip 192.168.1.100/24 --gateway 192.168.1.1 \
    --run-script ./bootstrap.sh --script-args "--role web"
```

`bootstrap.sh` 内即可用 `$VM_NAME` / `$VM_INDEX` / `$VM_IP` 和 `--script-args` 传入的位置参数。脚本用什么语言都行,改 `--script-interpreter`(如 `/usr/bin/python3`)即可。

#### 对单台已有虚机定制(`customize` 子命令)

`clone --customize` 在克隆一批机器时顺带批量定制;`customize` 子命令把同一套定制逻辑独立出来,作用于**单台已有虚机**——用于定制失败重试、后期改单台 IP、或给非本工具创建的机器配置。定制项与 clone 一致(改主机名 / 配 IP / 跑脚本),网络配置同样自动识别 netplan/nmcli/networkd/ifupdown。

**定位:单台操作。** 筛选必须恰好命中 1 台,否则报错退出。批量对已有机器定制请用 shell 循环逐台调用(见下),或在建机时用 `clone --customize`。这样职责清晰:**clone 管批量装机,customize 管单台改配**,也彻底避免了"按选中位置隐式分配 IP"可能刷错的风险。

```bash
# 给某一台精确改 IP + 改主机名 + 跑脚本
./venv/bin/python manage_vms.py --host 192.168.1.10 customize --names db-primary \
    --guest-user root --set-hostname \
    --ip 192.168.1.9/24 --gateway 192.168.1.1 \
    --run-script ./bootstrap.sh

# 批量 = 循环调用(IP 逐台显式给出,一目了然、不会算错)
for i in 1 2 3; do
  ./venv/bin/python manage_vms.py --host 192.168.1.10 customize --names web0$i \
      --guest-user root --set-hostname --ip 192.168.1.10$i/24 --gateway 192.168.1.1
done
```

与 clone 定制的差异:

| 点 | clone --customize | customize 子命令 |
|------|------|------|
| 作用对象 | 新克隆出的一批机器 | **单台**已有机器(筛选须命中 1 台) |
| 选机 | 按 `--prefix`+`--count` 生成 | `--prefix/--names/--all/--state` 过滤到 1 台 |
| 改主机名 | 总是改(设为克隆名) | 默认不改,`--set-hostname` 才设为虚机名 |
| 配 IP | `--start-ip` 批量按序号递增 | `--ip` 精确指定这一台 |
| 至少一项动作 | 改名总会做 | 须给 `--set-hostname`/`--ip`/`--run-script` 之一 |
| 未开机的机器 | 不涉及(刚开机) | 报错退出(定制需开机 + Tools) |

`--ip` 要求同时给 `--gateway`。

---

## list / power-on / power-off / delete — 批量管理

查询、电源与删除子命令。

### 列出虚机

```bash
./venv/bin/python manage_vms.py --host 192.168.1.10 --user root list
./venv/bin/python manage_vms.py --host 192.168.1.10 list --prefix web
```

输出:名称、电源状态、CPU、内存、IP、Tools 状态。

### 批量开机 / 关机

```bash
# 开机
./venv/bin/python manage_vms.py --host 192.168.1.10 power-on --prefix web

# 优雅关机(默认,需 VMware Tools)
./venv/bin/python manage_vms.py --host 192.168.1.10 power-off --prefix web

# 硬断电(等于拔电,会二次确认)
./venv/bin/python manage_vms.py --host 192.168.1.10 power-off --prefix web --hard
```

### 批量删除

```bash
# 删除(连磁盘文件一并删,不可恢复);开机中的默认跳过
./venv/bin/python manage_vms.py --host 192.168.1.10 delete --prefix web

# --force:开机中的先硬断电再删
./venv/bin/python manage_vms.py --host 192.168.1.10 delete --prefix web --force

# --keep-files:仅从清单注销,磁盘文件保留(可日后重新注册)
./venv/bin/python manage_vms.py --host 192.168.1.10 delete --names web01 --keep-files
```

`delete` 无论范围大小**一律二次确认**(仅 `--yes` 可跳过)。默认走 `Destroy_Task` 连磁盘删除;`--keep-files` 降级为 `UnregisterVM`,只移出清单、保留 vmdk。删除要求虚机非开机,开机中的默认跳过,`--force` 会先硬断电。

> ⚠️ **链接克隆的父机不可删。** 若某虚机的磁盘是其它链接克隆机的父盘(基准快照来源),删除它会连带损坏所有依赖它的克隆机。删除前先确认目标不是任何链接克隆链的源头。

### 筛选条件(list / power-* / delete / customize 通用)

| 参数 | 说明 |
|------|------|
| `--prefix web` | 名称前缀匹配 |
| `--names a,b,c` | 精确名单(逗号分隔) |
| `--all` | 所有虚机 |
| `--state poweredOn` | 叠加电源状态过滤 |

优先级:`--names` > `--prefix` > `--all`。`--state` 可与前三者叠加。

### 电源 / 删除操作专用参数

| 参数 | 说明 |
|------|------|
| `--hard` | 硬断电(仅 power-off) |
| `--keep-files` | 仅注销,保留磁盘(仅 delete) |
| `--force` | 删除开机中的虚机前先断电(仅 delete) |
| `--yes` | 跳过二次确认(脚本化时用) |
| `--workers` | 并发数(默认 8) |

### 安全设计

- **电源 / 删除 / 定制操作必须带筛选条件**,不给 `--prefix/--names/--all/--state` 直接拒绝,防止误伤全部。
- **大范围操作二次确认**:`--all`、`--hard`、或纯 `--state` 会列出目标清单并要求输入 `yes`。
- **删除一律二次确认**:不可逆,任意范围都要求输入 `yes`,仅 `--yes` 可跳过。
- **删除默认不碰开机的机器**:运行中的虚机跳过,需 `--force` 才先断电再删。
- **优雅关机需 Tools**:未运行 Tools 时提示改用 `--hard`,而非静默失败。
- 已处于目标状态的机器自动跳过。

---

## 注意事项

1. **免费版 ESXi 无法执行写操作**(复制、开关机、删除),仅 `list` 可用。
2. **同源复制的 IP / 主机名冲突**:完整复制和链接克隆得到的都是源机副本,IP、主机名、SSH host key 相同,批量开机会撞 IP。用 `--customize` 或开机后跑 cloud-init / sysprep 解决。
3. **链接克隆的依赖链**:源盘和基准快照不能删除或变动,否则所有克隆机损坏;高并发下共享父盘的 datastore 会成为 IO 热点。
4. **首次务必单台试**:先 `--count 1` 跑一台,进控制台确认磁盘、网卡、主机名、IP 都正确,再批量。
5. **SSL 证书**:默认跳过自签证书校验(便于自签 ESXi 开箱即用)。生产环境建议导入 ESXi 证书并加 `--no-insecure` 开启校验。
6. **凭据安全**:密码走环境变量或交互输入,不要写进命令行或提交到版本库。

## 文件说明

| 文件 | 说明 |
|------|------|
| `manage_vms.py` | 单一入口:list / power-on / power-off / clone |
| `requirements.txt` | Python 依赖(pyvmomi) |
| `venv/` | 虚拟环境(不提交版本库) |

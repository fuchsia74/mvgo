# mvgo — ESXi 批量虚机管理(Go 版)

`manage_vms.py` 的 Go 移植,基于 VMware 官方 [govmomi](https://github.com/vmware/govmomi) SDK。
**直连单台 ESXi(无 vCenter)**,子命令与 Python 版一致:

- `list` — 列出虚机
- `power-on` / `power-off` — 批量开机 / 关机
- `delete` — 批量删除(默认连磁盘删,`--keep-files` 仅注销)
- `clone` — 批量复制(完整复制 / `--linked` 链接克隆,可选 `--customize`)
- `customize` — 对单台已有虚机做 guest 定制(改主机名 / 配 IP / 跑脚本)

> 前提同 Python 版:ESXi 需带 license(免费版 API 写操作被禁,仅 `list` 可用)。

## 用容器编译(不装本机 Go)

```bash
# 在本目录 mvgo/ 下
docker run --rm -v "$PWD:/src" -w /src -v mvgo-gocache:/go golang:1.25 \
    go build -o mvgo .
# 产物 mvgo 是动态 linux/amd64 二进制,适合本机跑/调试
```

### 跨平台静态版(便于分发)

静态链接 + strip,零动态依赖,可直接 scp 到任意同架构 Linux(含 ESXi 管理机):

```bash
# amd64(x86-64)
docker run --rm -v "$PWD:/src" -w /src -v mvgo-gocache:/go golang:1.25 \
    sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o mvgo-linux-amd64 .'

# arm64(aarch64,如 ARM 管理机 / 鲲鹏 / 树莓派)
docker run --rm -v "$PWD:/src" -w /src -v mvgo-gocache:/go golang:1.25 \
    sh -c 'CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o mvgo-linux-arm64 .'
```

govmomi 为纯 Go,`CGO_ENABLED=0` 下可直接交叉编译,无需目标架构工具链。

也可跑测试:

```bash
docker run --rm -v "$PWD:/src" -w /src -v mvgo-gocache:/go golang:1.25 go test ./...
```

> `golang` 官方镜像需 ≥ 1.25(govmomi v0.55 要求)。`mvgo-gocache` 卷缓存依赖,加速二次编译。

## 凭据(环境变量,同 Python 版)

```bash
export ESXI_USER=root          # 等价 --user,默认 root
export ESXI_PASSWORD=xxx        # 等价 --password
export GUEST_USER=root          # 等价 --guest-user
export GUEST_PASSWORD=xxx        # 等价 --guest-password
```

命令行显式传参优先级高于环境变量。

## 用法示例

```bash
# 列出
./mvgo --host 10.0.0.10 list --prefix web
# 批量开机
./mvgo --host 10.0.0.10 power-on --prefix web
# 优雅关机 / 硬断电
./mvgo --host 10.0.0.10 power-off --prefix web
./mvgo --host 10.0.0.10 power-off --prefix web --hard
# 删除(连磁盘;开机的先强制断电)
./mvgo --host 10.0.0.10 delete --prefix web --force
# 完整复制 5 台
./mvgo --host 10.0.0.10 clone --source base --prefix web --count 5 --power-on
# 链接克隆 + 定制(改名/配IP,自动识别 netplan/nmcli/networkd/ifupdown)
./mvgo --host 10.0.0.10 clone --source base --prefix web --count 5 --linked \
    --customize --guest-user root --start-ip 192.168.1.100/24 --gateway 192.168.1.1
# 单台改配
./mvgo --host 10.0.0.10 customize --names db-primary \
    --guest-user root --set-hostname --ip 192.168.1.9/24 --gateway 192.168.1.1
```

参数用 `-flag` 或 `--flag` 均可(Go flag 包两者都接受)。各子命令详见 `./mvgo <子命令> -h`。

## 与 Python 版的行为一致性

- 链接克隆的 delta 盘 spec 带 `fileOperation=create`,父盘链指向源机只读快照 —— 与 Python 版一致。
- 配 IP 的 guest 端 bash 脚本**逐字等同** Python 版(已 diff 校验):同样的
  netplan→nmcli→systemd-networkd→ifupdown 识别顺序、网卡自动识别、变量注入与校验回显。
- 安全护栏一致:power-*/delete/customize 强制筛选;delete 一律二次确认;
  customize 单台命中校验;clone 完整复制要求源机关机。
- clone 批量的 IP 按虚机名序号(`num-1`)分配;customize 单台用 `--ip` 精确指定。

### 已知差异(有意为之)

- 缺密码时的交互提示为**明文回显**(未引入 `x/term` 依赖);推荐走环境变量提供口令。
- 帮助文本为 Go `flag` 风格(单破折号列示),但 `--flag` 同样可用。

## 状态

已在容器内 `go build` / `go vet` / `go test` 通过;纯函数(IP 计算、配网脚本生成)
有单测,配网脚本经 `bash -n` 语法检查且与 Python 版 diff 一致。
**连真实 ESXi 的端到端运行(建机 / 链接克隆 / guest 定制)需在实机复测** ——
govmomi 与 pyvmomi 同走 vim25 SOAP,对独立 ESXi 行为预期一致,但未在本环境连真机验证。

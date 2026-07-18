# ESXi 批量虚机管理工具

直连单台 ESXi 主机(无 vCenter)完成虚拟机的批量复制、电源管理与 guest 定制。

当前实现为 **Go 版**,基于 VMware 官方 [govmomi](https://github.com/vmware/govmomi) SDK,
源码与用法见 [`mvgo/`](mvgo/README.md)。

子命令:`list` / `power-on` / `power-off` / `delete` / `clone` / `customize`。

> ⚠️ **前提:ESXi 需带 license。** 免费版 ESXi 的 vSphere API 写操作被禁用,
> 只读(如 `list`)可用,写操作(复制、开关机、删除)会返回 `RestrictedVersion` 错误。

## 快速开始

```bash
# 用容器编译(不装本机 Go),产物输出到 build/,详见 mvgo/README.md
docker run --rm -v "$PWD:/src" -w /src/mvgo -v mvgo-gocache:/go golang:1.25 \
    go build -buildvcs=false -o /src/build/mvgo .

export ESXI_PASSWORD=xxx
./build/mvgo --host 10.0.0.10 list --prefix web
```

## 历史:Python 版

早期为 pyvmomi 单文件脚本 `manage_vms.py`。Go 版功能/参数与其完全对齐并已真机验证后,
主分支移除了 Python 版。**完整 Python 版保存在 `python-legacy` 分支**:

```bash
git checkout python-legacy   # 取回 manage_vms.py 及其文档
```

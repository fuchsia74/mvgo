#!/usr/bin/env bash
# 用 golang 容器编译 mvgo(不装本机 Go),产物输出到 build/。
#
# 用法:
#   ./build.sh                 # 编译全部:本地动态版 + amd64/arm64 静态版
#   ./build.sh local           # 仅本地动态版(调试用)
#   ./build.sh amd64           # 仅 linux/amd64 静态版
#   ./build.sh arm64           # 仅 linux/arm64 静态版
#   ./build.sh static          # amd64 + arm64 静态版
#   ./build.sh test            # 跑 go test
#   ./build.sh clean           # 清空 build/
#   IMAGE=golang:1.25 ./build.sh   # 覆盖镜像版本
set -euo pipefail

# 项目根 = 脚本所在目录
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${IMAGE:-golang:1.25}"
CACHE_VOL="${CACHE_VOL:-mvgo-gocache}"
OUT="build"

# 在容器里、mvgo/ 目录下执行 go 命令。挂载整个仓库,GOPATH 缓存走命名卷。
# -buildvcs=false:容器内 git 环境不完整,VCS 戳记会报错。
run_go() {
    docker run --rm \
        -v "$ROOT:/src" -w /src/mvgo \
        -v "$CACHE_VOL:/go" \
        "$IMAGE" "$@"
}

build_local() {
    echo ">> 本地动态版 -> $OUT/mvgo"
    run_go go build -buildvcs=false -o "/src/$OUT/mvgo" .
}

# build_static <goarch>
build_static() {
    local arch="$1"
    echo ">> 静态 linux/$arch -> $OUT/mvgo-linux-$arch"
    run_go env CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -buildvcs=false -trimpath -ldflags "-s -w" \
        -o "/src/$OUT/mvgo-linux-$arch" .
}

run_test() {
    echo ">> go test"
    run_go go test -buildvcs=false ./...
}

need_docker() {
    command -v docker >/dev/null 2>&1 || { echo "错误: 未找到 docker" >&2; exit 1; }
}

main() {
    local target="${1:-all}"
    case "$target" in
        clean)
            echo ">> 清空 $OUT/"
            rm -rf "${ROOT:?}/$OUT"
            return
            ;;
        test)
            need_docker; run_test; return
            ;;
    esac

    need_docker
    mkdir -p "$ROOT/$OUT"
    case "$target" in
        all)     build_local; build_static amd64; build_static arm64 ;;
        local)   build_local ;;
        amd64)   build_static amd64 ;;
        arm64)   build_static arm64 ;;
        static)  build_static amd64; build_static arm64 ;;
        *)
            echo "未知目标: $target" >&2
            echo "可选: all | local | amd64 | arm64 | static | test | clean" >&2
            exit 2
            ;;
    esac

    echo
    echo "完成。产物:"
    ls -la "$ROOT/$OUT" 2>/dev/null | grep mvgo || true
}

main "$@"

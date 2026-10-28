#!/usr/bin/env bash
# deploy.sh — 本仓在 soult 上的构建/上线 helper（docker compose 部署形态）
#
# 背景：本仓没有 CI 部署，历史上都是手敲 `docker compose up -d --build`。
# 每次 build 都会把上一版镜像变成 <none> 悬空镜像（实测一对 460M + 94.8M），
# 2026-10-28 在 soult 上清出 23 个悬空镜像共 2.6G —— 不清的话一个月就攒回 3G+。
# 所以构建收尾统一走 deploy.sh，由它负责 prune。
#
# 用法:
#   ./deploy.sh          构建并上线（docker compose up -d --build）
#   ./deploy.sh build    只构建，不动正在跑的容器
#   ./deploy.sh prune    只清悬空镜像
set -uo pipefail
cd "$(dirname "$0")"

prune() {
    # 只删「没有任何容器引用」的镜像。在跑的 workbuddy2api / postgres /
    # ib-gateway / workbuddy2api-gui 都不受影响。
    echo "--- 清理构建残留的悬空镜像 ---"
    docker image prune -f
    echo "--- 当前 docker 占用 ---"
    docker system df
}

case "${1:-up}" in
  up)
    docker compose up -d --build || { echo "构建/上线失败，未做 prune" >&2; exit 1; }
    prune
    docker compose ps
    ;;
  build)
    docker compose build || { echo "构建失败，未做 prune" >&2; exit 1; }
    prune
    ;;
  prune)
    prune
    ;;
  *)
    echo "用法: $0 {up|build|prune}" >&2
    exit 1
    ;;
esac

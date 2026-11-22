#!/usr/bin/env bash
# login.sh — CodeArts Agent（华为云账号）登录：浏览器 OAuth2 + PKCE → auths/codearts-{user_id}.json
#
# 用法:
#   ./login.sh              本机有浏览器：自动打开浏览器，回调 + ticket 轮询双通道
#   ./login.sh -print-only  服务器：打印登录链接，任意机器浏览器打开，ticket 轮询下发
set -euo pipefail
cd "$(dirname "$0")"

AUTH_DIR="./auths"
EXTRA=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        -auth-dir)
            AUTH_DIR="${2:?usage: -auth-dir <dir>}"
            EXTRA+=("-auth-dir" "$AUTH_DIR")
            shift 2
            ;;
        -auth-dir=*)
            AUTH_DIR="${1#*=}"
            EXTRA+=("$1")
            shift
            ;;
        *)
            EXTRA+=("$1")
            shift
            ;;
    esac
done
mkdir -p "$AUTH_DIR"

BIN=./bin/codearts2api-login
if [ ! -x "$BIN" ]; then
    echo "build login binary ..."
    mkdir -p ./bin
    go build -o "$BIN" ./cmd/login
fi

exec "$BIN" "${EXTRA[@]}"

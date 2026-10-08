#!/usr/bin/env bash
# start.sh —— 二进制部署的启动包装：先把 .env 读进环境，再启动服务。
#
# 背景：服务端只在【进程环境变量】里读 TW2A_API_KEY（os.Getenv），
# 它并不会自己去读 .env 文件 —— .env 只有 docker compose 会读。
# 所以二进制 / nohup / systemd 部署时，光写一个 .env 是不生效的，
# 必须有人把它 export 成环境变量；本脚本就是干这件事的。
#
# 用法:
#   ./start.sh                              # 前台启动
#   nohup ./start.sh > run.log 2>&1 &       # 后台启动
#   TW2A_API_KEY=xxx ./start.sh             # 临时覆盖（shell 里的值优先）
#   TW2A_BIN=./server ./start.sh            # 二进制名不是 trae2api-web 时指定
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

BIN="${TW2A_BIN:-./trae2api-web}"

# ---- 1. 加载 .env ----
# set -a: 之后 source 进来的变量自动 export，子进程才看得到。
if [ -f .env ]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env
    set +a
    echo "[start.sh] 已加载 .env"
else
    echo "[start.sh] 未找到 .env，跳过（可直接用环境变量传 Key）"
fi

# ---- 2. 自检：Key 为空就当场喊出来，别等签到程序 403 才发现 ----
if [ -z "${TW2A_API_KEY:-}" ]; then
    echo "[start.sh] 警告：TW2A_API_KEY 为空！" >&2
    echo "[start.sh]   /admin/api/accounts/export 将返回 403 api_key_required，" >&2
    echo "[start.sh]   本地签到程序会拉不到账号。" >&2
else
    echo "[start.sh] TW2A_API_KEY 已设置（长度 ${#TW2A_API_KEY}）"
fi

# ---- 3. 启动 ----
if [ ! -x "$BIN" ]; then
    echo "[start.sh] 找不到可执行文件 $BIN（用 TW2A_BIN=/path/to/bin 指定）" >&2
    exit 1
fi
echo "[start.sh] 启动 $BIN"
exec "$BIN" "$@"

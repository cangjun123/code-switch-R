#!/usr/bin/env bash
#
# 服务器一键更新脚本。
#
# 用法：在项目目录（如 /opt/codeswitch/current）执行
#     ./update.sh
#
# 说明：
# - 自动切到 deploy 分支并拉取最新构建产物（deploy 已含编译好的 codeswitch-web
#   和 frontend/dist，服务器无需编译），赋可执行权限后重启服务。
# - 需要当前用户具备 git pull 权限，以及 sudo 权限（仅用于 systemctl restart）。
# - 不会删除 ~/.code-switch/，配置和数据库保留。

set -euo pipefail

# 切到脚本所在目录（项目根）
cd "$(dirname "$0")"

# 确保在 deploy 分支（main 只有源码，deploy 才有可运行的构建产物）
BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [ "$BRANCH" != "deploy" ]; then
    echo "==> 当前在 $BRANCH 分支，切换到 deploy"
    git fetch origin
    git checkout deploy
fi

echo "==> 拉取最新代码（origin/deploy）..."
git pull origin deploy

echo "==> 赋予 codeswitch-web 可执行权限..."
chmod +x ./codeswitch-web

echo "==> 重启 codeswitch 服务（可能需要输入 sudo 密码）..."
sudo systemctl restart codeswitch

echo "==> 更新完成"
echo "    实时日志：sudo journalctl -u codeswitch -f"
echo "    服务状态：sudo systemctl status codeswitch"

#!/usr/bin/env bash
# deploy-tcmsp30.sh — P3 部署基座：交叉编译 linux/amd64 并推送至 tcmsp-30。
#
# 远端目录约定（~/promptopt-smoke/）：
#   promptopt   交叉编译出的单二进制
#   runs/       run 工件（--out 落点）
#   anchors/    锚点数据集（verify --anchor 用，本期仅建目录）
#   bench/      大基数实测的合成三件套与结果产物
#   scripts/    smoke-load.sh 等实测脚本
#
# 用法：
#   scripts/deploy-tcmsp30.sh              # 构建 + 推送 + 远端自检
#   REMOTE=other-host scripts/deploy-tcmsp30.sh
#
# 前置：ssh 别名 tcmsp-30 免密可达；本机 Go 工具链 >= go.mod 声明版本。
set -euo pipefail

REMOTE="${REMOTE:-tcmsp-30}"
SMOKE_DIR="${SMOKE_DIR:-promptopt-smoke}"

cd "$(dirname "$0")/.."

echo "==> 交叉编译 GOOS=linux GOARCH=amd64 ./cmd/promptopt"
GOOS=linux GOARCH=amd64 go build -trimpath -o promptopt-linux ./cmd/promptopt

echo "==> 断言产物为 ELF x86-64"
if ! file promptopt-linux | grep -q "ELF 64-bit.*x86-64"; then
  file promptopt-linux
  echo "FATAL: 产物不是 linux/amd64 ELF" >&2
  exit 1
fi
file promptopt-linux

echo "==> 建立远端目录 $REMOTE:~/$SMOKE_DIR/{runs,anchors,bench,scripts}"
ssh "$REMOTE" "mkdir -p ~/$SMOKE_DIR/{runs,anchors,bench,scripts}"

echo "==> 推送二进制与实测脚本"
# scp 对「已存在的目标文件」覆盖写过一次即失败（tcmsp-30 实测复现两次），
# 故先传 .new 再远端 mv 原子替换。
scp -q promptopt-linux "$REMOTE:$SMOKE_DIR/promptopt.new"
ssh "$REMOTE" "mv ~/$SMOKE_DIR/promptopt.new ~/$SMOKE_DIR/promptopt && chmod +x ~/$SMOKE_DIR/promptopt"
scp -q scripts/smoke-load.sh "$REMOTE:~/$SMOKE_DIR/scripts/smoke-load.sh"
ssh "$REMOTE" "chmod +x ~/$SMOKE_DIR/scripts/smoke-load.sh"
scp -q scripts/smoke-decision.sh "$REMOTE:~/$SMOKE_DIR/scripts/smoke-decision.sh"
ssh "$REMOTE" "chmod +x ~/$SMOKE_DIR/scripts/smoke-decision.sh"
rm -f promptopt-linux

echo "==> 远端自检"
ssh "$REMOTE" "~/$SMOKE_DIR/promptopt version && bash -n ~/$SMOKE_DIR/scripts/smoke-load.sh && bash -n ~/$SMOKE_DIR/scripts/smoke-decision.sh && echo 'deploy OK: ~/$SMOKE_DIR'"

echo "==> 下一步（在 tcmsp-30 上执行）："
echo "    ssh $REMOTE '~/$SMOKE_DIR/scripts/smoke-load.sh'      # P3 大基数实测"
echo "    ssh $REMOTE '~/$SMOKE_DIR/scripts/smoke-decision.sh'  # P7 决策级联实测"

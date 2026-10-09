#!/usr/bin/env bash
# smoke-web.sh — P10 Web 看板远端冒烟（在 tcmsp-30 执行）。
#
# 载荷：以分立参数 `-addr 127.0.0.1 -port 17000` 起 `promptopt serve`
#（顺带实测 serve 侧新的 -addr/-port 组合语义），对最近一个 run 目录
# curl 断言：
#   1. dashboard 整页 200：<style> 设计令牌同源锚点 + 关键类名
#     （.topbar/.heat/.verdict）+ 四视图锚点（view-overview/frontier/
#      trace/verify）；
#   2. 五数据端点 200 且关键 JSON 键在位（overview/trend/heatmap/
#      lineage/verify）；
#   3. adopt 端点存在性（POST 未知候选 → 404，路由在而非 405）。
#
# 像素级视觉复核留浏览器人工（本脚本只做结构/CSS 同源断言）。
# 前置：先跑过至少一次 run（~/promptopt-smoke/runs/ 非）；无 run 时
# 脚本会用 deploy-tcmsp30.sh 推送的 examples 跑一次最小 headless run
#（需要网关可达；SMOKE_SKIP_RUN=1 可跳过并只测无 run 路径的空态）。
#
# 用法（tcmsp-30 上）：
#   ~/promptopt-smoke/scripts/smoke-web.sh
# 本机干跑（不发起真实请求，仅 bash -n 与参数展开）：
#   SKIP_REMOTE=1 scripts/smoke-web.sh
#
# 可调环境变量：BIN SMOKE_ROOT RUNS_DIR ADDR PORT RUN_ID SMOKE_SKIP_RUN
set -euo pipefail

BIN="${BIN:-$HOME/promptopt-smoke/promptopt}"
SMOKE_ROOT="${SMOKE_ROOT:-$HOME/promptopt-smoke}"
RUNS_DIR="${RUNS_DIR:-$SMOKE_ROOT/runs}"
ADDR="${ADDR:-127.0.0.1}"   # 纯主机形态——与 -port 组合（P10 分立参数）
PORT="${PORT:-17000}"
BASE="http://$ADDR:$PORT"

if [[ "${SKIP_REMOTE:-0}" == "1" ]]; then
  echo "[dry-run] bash -n 自检"
  bash -n "$0" && echo "[dry-run] 语法 OK"
  echo "[dry-run] 参数展开："
  echo "  BIN=$BIN RUNS_DIR=$RUNS_DIR ADDR=$ADDR PORT=$PORT"
  echo "[dry-run] 计划断言：dashboard 200 + <style> 同源锚点 + .topbar/.heat/.verdict + 四视图锚点；"
  echo "           /runs/{id}/api/{overview,trend,heatmap,lineage,verify} 200 + 关键 JSON 键；"
  echo "           POST /runs/{id}/adopt 未知候选 → 404"
  exit 0
fi

command -v curl >/dev/null || { echo "需要 curl"; exit 1; }
[[ -x "$BIN" ]] || { echo "二进制不存在：$BIN（先跑 deploy-tcmsp30.sh）"; exit 1; }

# 需要至少一个 run 工件：没有且未明确跳过时补一次最小 run。
if [[ -z "$(ls -A "$RUNS_DIR" 2>/dev/null || true)" && "${SMOKE_SKIP_RUN:-0}" != "1" ]]; then
  echo "[smoke] runs 目录为空，跑一次最小三件套 run（examples/json_extraction）…"
  EXAMPLE_DIR="$SMOKE_ROOT/examples/json_extraction"
  if [[ ! -f "$EXAMPLE_DIR/task.yaml" ]]; then
    echo "[smoke] 缺少示例三件套：$EXAMPLE_DIR——请先 deploy 或手动 RUN_ID=… 指定"; exit 1
  fi
  (cd "$EXAMPLE_DIR" && "$BIN" run --task task.yaml --candidate candidate.yaml \
    --dataset dataset.yaml --base-url "${BASE_URL:?需要 BASE_URL 指向网关}" \
    --model "${MODEL:?需要 MODEL}" --out "$RUNS_DIR" --headless --samples 3 \
    --budget-evals 4 >/dev/null) || { echo "[smoke] 预热 run 失败"; exit 1; }
fi

# 以纯主机 -addr + 分立 -port 起 serve（分立参数的远端实测面）。
"$BIN" serve --runs-dir "$RUNS_DIR" -addr "$ADDR" -port "$PORT" &
SERVE_PID=$!
trap 'kill "$SERVE_PID" 2>/dev/null || true' EXIT

for _ in $(seq 1 50); do
  curl -sf "$BASE/" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sf "$BASE/" >/dev/null || { echo "[smoke] serve 未就绪：$BASE"; exit 1; }
echo "[smoke] serve 就绪：$BASE（-addr $ADDR -port $PORT）"

RUN_ID="${RUN_ID:-$(ls "$RUNS_DIR" | sort | tail -1)}"
[[ -n "$RUN_ID" ]] || { echo "[smoke] 无 run 目录可断言（SMOKE_SKIP_RUN=1 时属预期，跳过 per-run 断言）"; exit 0; }
echo "[smoke] 目标 run：$RUN_ID"

fails=0
check() { # check <说明> <命令…>
  local desc="$1"; shift
  if "$@"; then echo "  ok: $desc"; else echo "  FAIL: $desc"; fails=$((fails+1)); fi
}

echo "[smoke] 1) dashboard 整页"
HTML="$(curl -sf "$BASE/runs/$RUN_ID/dashboard")" || { echo "  FAIL: dashboard 页请求失败"; exit 1; }
check "<style> 设计令牌同源锚点（--bg:#F8FAFC）" grep -q -- '--bg:#F8FAFC' <<<"$HTML"
check "设计令牌同源锚点（--radius:10px）" grep -q -- '--radius:10px' <<<"$HTML"
for cls in 'class="topbar"' 'class="heat"' 'class="verdict' 'id="view-overview"' 'id="view-frontier"' 'id="view-trace"' 'id="view-verify"' 'role="tablist"'; do
  check "关键类名/锚点 $cls" grep -q "$cls" <<<"$HTML"
done
check "事件流接 SSE（EventSource）" grep -q "EventSource(" <<<"$HTML"
check "采纳接 adopt 端点（fetch）" grep -q "/adopt" <<<"$HTML"

echo "[smoke] 2) 数据端点"
api_key() { curl -sf "$BASE/runs/$RUN_ID/api/$1" | grep -q "\"$2\""; }
for kv in "overview:run_id" "overview:verdict" "overview:metric_means" \
          "trend:points" "trend:available" \
          "heatmap:members" "heatmap:rows" \
          "lineage:rows" \
          "verify:exit_codes" "verify:available"; do
  ep="${kv%%:*}"; key="${kv##*:}"
  check "GET /api/$ep 含键 \"$key\"" api_key "$ep" "$key"
done

echo "[smoke] 3) adopt 端点存在性"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/runs/$RUN_ID/adopt" --data 'candidate=__smoke_nonexistent__')"
check "POST 未知候选 → 404（路由在，非 405/501）" test "$code" = "404"

echo
if [[ "$fails" -eq 0 ]]; then
  echo "[smoke] 全部断言通过 ✔（像素级视觉复核请在浏览器打开 $BASE/runs/$RUN_ID/dashboard）"
else
  echo "[smoke] $fails 项断言失败 ✘"
  exit 1
fi

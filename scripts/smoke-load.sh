#!/usr/bin/env bash
# smoke-load.sh — V7 P3 四场实测·第一场：大样本基数实测（在 tcmsp-30 执行）。
#
# 载荷：脚本合成的 N 条单字段抽取样本（bench/fixtures/），手动三件套模式
# 跑一次 --headless run：--workers 高并发 + --rps 客户端限流 + 裁判隔离
# （--judge-model 指向第二模型，引擎级回落共享 Provider 实例 → 执行器与
# 裁判调用同经一个限流器，RoleJudge 记账单列）。
#
# 断言（全部来自本机可观测的产物，不臆测）：
#   1. 预算阀门：--budget-evals < N → 存在未派发样本，退出码 2；
#   2. RoleJudge 记账单列：summary.usage_by_role 出现 judge 键；
#   3. 裁判调用在审计 trace 中可证：stage=judge 的调用模型名 = JUDGE_MODEL；
#   4. 限流生效：全部 LLM 调用共用一个限流器，run 墙钟时长 ≥
#      (调用数−1)/rps（下界断言）；并统计 429 可见失败率（重试成功
#      的 429 不产生日志，产品无退避日志——见 notes，如实口径）。
#
# 用法（tcmsp-30 上）：
#   ~/promptopt-smoke/scripts/smoke-load.sh
# 本机干跑（不发起真实 run，仅 bash -n 与参数展开）：
#   SKIP_REMOTE=1 scripts/smoke-load.sh
#
# 可调环境变量：BIN SMOKE_ROOT BASE_URL MODEL JUDGE_MODEL WORKERS RPS
#   SAMPLES BUDGET_EVALS MAX_TOKENS JUDGE_MAX_TOKENS API_KEY EXTRA_BODY
set -euo pipefail

BIN="${BIN:-$HOME/promptopt-smoke/promptopt}"
SMOKE_ROOT="${SMOKE_ROOT:-$HOME/promptopt-smoke}"
BASE_URL="${BASE_URL:-http://127.0.0.1:11434/v1}"
MODEL="${MODEL:-RogerBen/HY-MT2-1.8B:latest}"
JUDGE_MODEL="${JUDGE_MODEL:-tev1:0.8b}"
WORKERS="${WORKERS:-16}"
RPS="${RPS:-8}"
SAMPLES="${SAMPLES:-300}"
BUDGET_EVALS="${BUDGET_EVALS:-40}"
MAX_TOKENS="${MAX_TOKENS:-8192}"
JUDGE_MAX_TOKENS="${JUDGE_MAX_TOKENS:-512}"
API_KEY="${API_KEY:-1}"
# EXTRA_BODY 缺省不传：网关是否接受网关私有键因实现而异（提案 §6-A3），
# 设置后脚本会先做一次预检探测，被拒即失败退出而不是带病压测。
EXTRA_BODY="${EXTRA_BODY:-}"

if [[ "${SKIP_REMOTE:-0}" == "1" ]]; then
  echo "[dry-run] bash -n 自检"
  bash -n "$0" && echo "[dry-run] 语法 OK"
  echo "[dry-run] 参数展开："
  echo "  BIN=$BIN BASE_URL=$BASE_URL MODEL=$MODEL JUDGE_MODEL=$JUDGE_MODEL"
  echo "  WORKERS=$WORKERS RPS=$RPS SAMPLES=$SAMPLES BUDGET_EVALS=$BUDGET_EVALS"
  echo "  MAX_TOKENS=$MAX_TOKENS JUDGE_MAX_TOKENS=$JUDGE_MAX_TOKENS EXTRA_BODY=${EXTRA_BODY:-<未设置>}"
  echo "  命令：$BIN run --task <bench>/task.yaml --candidate <bench>/candidate.yaml \\"
  echo "        --dataset <bench>/dataset.yaml --base-url $BASE_URL --model $MODEL \\"
  echo "        --api-key *** --out $SMOKE_ROOT/runs --headless --workers $WORKERS --rps $RPS \\"
  echo "        --budget-evals $BUDGET_EVALS --max-tokens $MAX_TOKENS \\"
  echo "        --judge-model $JUDGE_MODEL --judge-max-tokens $JUDGE_MAX_TOKENS${EXTRA_BODY:+ \\}"
  [[ -z "$EXTRA_BODY" ]] || echo "        --extra-body <EXTRA_BODY>"
  exit 0
fi

command -v python3 >/dev/null || { echo "FATAL: 需要 python3" >&2; exit 1; }
[[ -x "$BIN" ]] || { echo "FATAL: 找不到 $BIN（先跑 deploy-tcmsp30.sh）" >&2; exit 1; }

FIXTURES="$SMOKE_ROOT/bench/fixtures"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
OUTDIR="$SMOKE_ROOT/bench/smoke-$STAMP"
mkdir -p "$OUTDIR" "$FIXTURES"

echo "==> 预检：网关 $BASE_URL 与模型"
MODELS_JSON="$(curl -sf --max-time 10 "$BASE_URL/models")" || { echo "FATAL: 网关不可达" >&2; exit 1; }
python3 - "$MODELS_JSON" "$MODEL" "$JUDGE_MODEL" <<'PY' || { echo "FATAL: 模型缺失（见上方清单）" >&2; exit 1; }
import json, sys
models = {m["id"] for m in json.loads(sys.argv[1])["data"]}
print("  网关模型：", ", ".join(sorted(models)))
missing = [m for m in sys.argv[2:] if m not in models]
sys.exit(1 if missing else 0)
PY

if [[ -n "$EXTRA_BODY" ]]; then
  echo "==> 预检：--extra-body 网关接受度探测"
  python3 - "$BASE_URL" "$MODEL" "$API_KEY" "$EXTRA_BODY" <<'PY'
import json, sys, urllib.request
base, model, key, extra = sys.argv[1:5]
body = {"model": model, "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 8}
body.update(json.loads(extra))
req = urllib.request.Request(base + "/chat/completions",
    data=json.dumps(body).encode(),
    headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
try:
    with urllib.request.urlopen(req, timeout=60) as resp:
        print("  探测响应：", resp.status)
except urllib.error.HTTPError as e:
    print(f"FATAL: 网关拒绝 extra body（HTTP {e.code}）：{e.read()[:200]!r}", file=sys.stderr)
    sys.exit(1)
PY
fi

echo "==> 生成大基数合成三件套（$SAMPLES 条样本）→ $FIXTURES"
python3 - "$FIXTURES" "$SAMPLES" <<'PY'
import json, sys, pathlib
fixtures, n = pathlib.Path(sys.argv[1]), int(sys.argv[2])
(fixtures / "task.yaml").write_text("""name: smoke_load_extraction
description: P3 大基数实测载荷任务（负载/阀门/记账断言用，非质量评测）
prompt_template: |
  从下面的文本中抽出年龄数字，只输出 JSON：{"age": "<数字>"}

  文本：{input}
metrics: [json_validator, llm_judge]
primary_metric: llm_judge
""")
(fixtures / "candidate.yaml").write_text("""id: baseline
prompt: |
  从下面的文本中抽出年龄数字，只输出 JSON：{"age": "<数字>"}

  文本：{input}
""")
samples = []
for i in range(1, n + 1):
    age = 20 + i % 60
    samples.append({
        "id": f"s-{i:04d}",
        "input": f"患者{'男' if i % 2 else '女'}性，{age}岁，初诊。",
        "expected": {"age": str(age)},
        "split": "test",
    })
(fixtures / "dataset.yaml").write_text("name: smoke_load_ds\nsamples:\n" + "".join(
    f"""  - id: {s["id"]}
    input: {s["input"]}
    expected: {json.dumps(s["expected"], ensure_ascii=False)}
    split: test
""" for s in samples))
print(f"  生成 {len(samples)} 条样本")
PY

EXTRA_FLAGS=()
[[ -n "$EXTRA_BODY" ]] && EXTRA_FLAGS=(--extra-body "$EXTRA_BODY")

echo "==> 发起大基数 run（并发 $WORKERS / 限流 ${RPS}rps / 预算 $BUDGET_EVALS evals）"
set +e
"$BIN" run \
  --task "$FIXTURES/task.yaml" \
  --candidate "$FIXTURES/candidate.yaml" \
  --dataset "$FIXTURES/dataset.yaml" \
  --base-url "$BASE_URL" --model "$MODEL" --api-key "$API_KEY" \
  --out "$SMOKE_ROOT/runs" --headless \
  --workers "$WORKERS" --rps "$RPS" \
  --budget-evals "$BUDGET_EVALS" --max-tokens "$MAX_TOKENS" \
  --judge-model "$JUDGE_MODEL" --judge-max-tokens "$JUDGE_MAX_TOKENS" \
  "${EXTRA_FLAGS[@]}" \
  > "$OUTDIR/summary.json" 2> "$OUTDIR/run.log"
CODE=$?
set -e
echo "  进程退出码：$CODE（run.log 尾部见 $OUTDIR/run.log）"

echo "==> 断言与记录"
python3 - "$OUTDIR" "$SMOKE_ROOT/runs" "$CODE" "$RPS" "$JUDGE_MODEL" "$BASE_URL" "$MODEL" <<'PY'
import json, pathlib, re, sys, datetime

outdir, runs_root, proc_code, rps, judge_model = (
    pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), int(sys.argv[3]),
    float(sys.argv[4]), sys.argv[5])
base_url, model = sys.argv[6], sys.argv[7]

failures = []
def check(ok, label, detail=""):
    print(f"  [{'PASS' if ok else 'FAIL'}] {label}" + (f"：{detail}" if detail else ""))
    if not ok:
        failures.append(label)

summary_text = (outdir / "summary.json").read_text()
try:
    s = json.loads(summary_text)
except json.JSONDecodeError:
    print("FATAL: headless stdout 不是 JSON 运行摘要：\n" + summary_text[:400])
    sys.exit(1)

run_dir = runs_root / s["run_id"]
traces = sorted((run_dir / "calls").glob("*.json"))
calls = [json.loads(p.read_text()) for p in traces]

# 1. 预算阀门：退出码 2 + 存在未派发
check(proc_code == 2 and s.get("exit_code") == 2, "预算阀门（退出码 2）",
      f"process={proc_code} summary={s.get('exit_code')} status={s.get('status')}")
check(s.get("undispatched", 0) > 0, "存在未派发样本",
      f"undispatched={s.get('undispatched')} evaluated={s.get('evaluated_samples')}")

# 2. RoleJudge 记账单列
roles = s.get("usage_by_role") or {}
check("judge" in roles and "executor" in roles, "UsageByRole 分列（executor+judge）",
      " ".join(f"{k}={v.get('prompt_tokens',0)}+{v.get('completion_tokens',0)}" for k, v in sorted(roles.items())))

# 3. 裁判调用审计可证（stage=judge 且模型名为第二模型）
judge_calls = [c for c in calls if c.get("stage") == "judge"]
check(len(judge_calls) >= 1, "裁判调用落 trace（stage=judge）", f"judge_calls={len(judge_calls)}/{len(calls)}")
if judge_calls:
    models_on_wire = {c["request"].get("model") for c in judge_calls}
    check(models_on_wire == {judge_model}, "裁判请求模型名 = JUDGE_MODEL", str(models_on_wire))

# 4. 限流生效：全部调用共用一个限流器 → 墙钟 ≥ (调用数-1)/rps
# Go 的 RFC3339Nano 带 9 位纳秒与 Z 尾，Python <3.11 的 fromisoformat
# 只认 3/6 位小数且不认 Z——先规范化再解析。
def iso(ts):
    return datetime.datetime.fromisoformat(
        re.sub(r"\.(\d{6})\d+", r".\1", ts.replace("Z", "+00:00")))

started, finished = iso(s["started_at"]), iso(s["finished_at"])
elapsed = finished - started
elapsed_s = elapsed.total_seconds()
floor_s = (len(calls) - 1) / rps * 0.95 if calls else 0.0
check(elapsed_s >= floor_s, "限流下界（墙钟 ≥ (调用数-1)/rps×0.95）",
      f"elapsed={elapsed_s:.2f}s floor={floor_s:.2f}s calls={len(calls)} rps={rps:g}")

# 429 可见失败率（重试成功的 429 不落日志；产品无退避日志，如实口径）
n429 = sum(1 for c in calls if "status 429" in (c.get("error") or ""))
rate = n429 / len(calls) if calls else 0.0
check(rate <= 0.05, "429 可见失败率 ≤ 5%", f"{n429}/{len(calls)} = {rate:.1%}")

check(len(failures) == 0, f"结论：{'全部通过' if not failures else '未通过 ' + str(failures)}")

(outdir / "result.md").write_text(f"""# smoke-load 第一场（大基数）— {outdir.name}

- 网关：{base_url}，executor 模型 {model}，judge 模型 {judge_model}，rps={rps:g}
- 退出码：process={proc_code}，summary.exit_code={s.get("exit_code")}，status={s.get("status")}
- 样本：evaluated={s.get("evaluated_samples")}，undispatched={s.get("undispatched")}，failed={len(s.get("failed_samples") or [])}
- UsageByRole：{json.dumps(roles, ensure_ascii=False)}
- 调用数：{len(calls)}（judge={len(judge_calls)}），墙钟 {elapsed_s:.2f}s，限流下界 {floor_s:.2f}s
- 429 可见失败率：{n429}/{len(calls)} = {rate:.1%}
- 断言：{"全部通过" if not failures else "未通过 " + ", ".join(failures)}
""")
sys.exit(1 if failures else 0)
PY
RC=$?
echo "==> 产物：$OUTDIR（summary.json / run.log / result.md）"
exit $RC

#!/usr/bin/env bash
# smoke-decision.sh — V7 P7 决策模型裁判级联实测（在 tcmsp-30 执行）。
#
# 载荷：脚本合成的 llm_judge 抽取样本（小基数），手动三件套模式跑一次
# --headless run：--judge-backend decision 真连本机 systemone systemd 服务
# （http://127.0.0.1:11434/v1/systemone，勿动、不再装实例）——决策模型
# tev1:0.8b 按样本出 score，置信度/分数走级联回落生成式裁判。
#
# 【本脚本是请求 shape（含 state）的最终真值验证——本机桩只钉 shape
# 约束，远端首跑暴露语义错位（state 措辞 / criteria 升序 / 除数语义）。】
#
# 断言（全部来自本机可观测的产物，不臆测）：
#   1. 请求 shape 真值：stage=judge-decision 的 trace 之外，服务端真实
#      接受了 {model, state, questions} 请求（预检探测 + run 内全 2xx）；
#   2. 回落语义与阈值一致：confidence < CONFIDENCE 的样本恰有一条
#      stage=judge 生成式调用且诊断非占位；可信样本恰无 stage=judge
#      调用、诊断为「决策裁判采信」占位；
#   3. 归一化分数在 [0,1]：score/(levels-1) 有界（服务端 score 为
#      级别期望值，levels 取请求 criteria 数）；
#   4. usage 记账：RoleJudge 单列且 = 全部 decision 调用 usage 之和
#      （有回落时再加生成式裁判用量）；decision 调用 output_tokens 极小
#      （实测 tev1:0.8b 每问 output_tokens=1，判 >0 即可）。
#
# 用法（tcmsp-30 上）：
#   ~/promptopt-smoke/scripts/smoke-decision.sh
# 本机干跑（不发起真实 run，仅 bash -n 与参数展开）：
#   SKIP_REMOTE=1 scripts/smoke-decision.sh
#
# 可调环境变量：BIN SMOKE_ROOT BASE_URL MODEL DECISION_URL DECISION_MODEL
#   SAMPLES CONFIDENCE DIAG_BELOW WORKERS MAX_TOKENS API_KEY
set -euo pipefail

BIN="${BIN:-$HOME/promptopt-smoke/promptopt}"
SMOKE_ROOT="${SMOKE_ROOT:-$HOME/promptopt-smoke}"
BASE_URL="${BASE_URL:-http://127.0.0.1:11434/v1}"
MODEL="${MODEL:-RogerBen/HY-MT2-1.8B:latest}"
DECISION_URL="${DECISION_URL:-http://127.0.0.1:11434}"
DECISION_MODEL="${DECISION_MODEL:-tev1:0.8b}"
SAMPLES="${SAMPLES:-12}"
CONFIDENCE="${CONFIDENCE:-0.5}"
DIAG_BELOW="${DIAG_BELOW:-0.6}"
WORKERS="${WORKERS:-4}"
MAX_TOKENS="${MAX_TOKENS:-8192}"
API_KEY="${API_KEY:-1}"

if [[ "${SKIP_REMOTE:-0}" == "1" ]]; then
  echo "[dry-run] bash -n 自检"
  bash -n "$0" && echo "[dry-run] 语法 OK"
  echo "[dry-run] 参数展开："
  echo "  BIN=$BIN BASE_URL=$BASE_URL MODEL=$MODEL"
  echo "  DECISION_URL=$DECISION_URL DECISION_MODEL=$DECISION_MODEL"
  echo "  SAMPLES=$SAMPLES CONFIDENCE=$CONFIDENCE DIAG_BELOW=$DIAG_BELOW"
  echo "  WORKERS=$WORKERS MAX_TOKENS=$MAX_TOKENS"
  echo "  命令：$BIN run --task <fixtures>/task.yaml --candidate <fixtures>/candidate.yaml \\"
  echo "        --dataset <fixtures>/dataset.yaml --base-url $BASE_URL --model $MODEL \\"
  echo "        --out $SMOKE_ROOT/runs --headless --workers $WORKERS --max-tokens $MAX_TOKENS \\"
  echo "        --judge-backend decision --judge-decision-url $DECISION_URL \\"
  echo "        --judge-decision-model $DECISION_MODEL \\"
  echo "        --judge-decision-confidence $CONFIDENCE --judge-decision-diag-below $DIAG_BELOW"
  exit 0
fi

command -v python3 >/dev/null || { echo "FATAL: 需要 python3" >&2; exit 1; }
[[ -x "$BIN" ]] || { echo "FATAL: 找不到 $BIN（先跑 deploy-tcmsp30.sh）" >&2; exit 1; }

FIXTURES="$SMOKE_ROOT/bench/fixtures-decision"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
OUTDIR="$SMOKE_ROOT/bench/decision-$STAMP"
mkdir -p "$OUTDIR" "$FIXTURES"

echo "==> 预检：decision 服务 $DECISION_URL/v1/systemone 真值探测（shape 含 state）"
# 探测信息走 stderr（命令替换只捕获 stdout 的标记行），标记 202 = 预检通过。
PROBE_CODE="$(python3 - "$DECISION_URL" "$DECISION_MODEL" 1>&2 <<'PY' && echo 202
import json, sys, urllib.request
base, model = sys.argv[1:3]
body = {
    "model": model,
    "state": "你是提示词优化管线的评估裁判，只依据内容正确性对照参考答案评分，忽略格式差异",
    "questions": {
        "q1": {
            "type": "score",
            "instructions": "【样本输入】预检输入\n\n【参考答案】\"预检答案\"\n\n【候选输出】预检答案",
            "criteria": ["完全错误", "部分正确", "基本正确", "与参考答案完全等价"],
        }
    },
}
req = urllib.request.Request(base + "/v1/systemone",
    data=json.dumps(body, ensure_ascii=False).encode(),
    headers={"Content-Type": "application/json"})
try:
    with urllib.request.urlopen(req, timeout=120) as resp:
        ans = json.loads(resp.read())
        a = ans["answers"]["q1"]
        levels = 4
        norm = a["score"] / (levels - 1)
        print(f"  探测 2xx：score={a['score']} confidence={a['confidence']} "
              f"norm={norm:.4f} usage={ans['usage']}", file=sys.stderr)
        assert 0.0 <= norm <= 1.0, f"归一化越界: {norm}"
except urllib.error.HTTPError as e:
    print(f"FATAL: decision 服务拒绝探测（HTTP {e.code}）：{e.read()[:300]!r}", file=sys.stderr)
    sys.exit(1)
PY
)" || { echo "FATAL: decision 预检失败" >&2; exit 1; }
[[ "$PROBE_CODE" == "202" ]] || { echo "FATAL: 预检未通过（$PROBE_CODE）" >&2; exit 1; }

echo "==> 生成 llm_judge 三件套（$SAMPLES 条样本）→ $FIXTURES"
python3 - "$FIXTURES" "$SAMPLES" <<'PY'
import json, sys, pathlib
fixtures, n = pathlib.Path(sys.argv[1]), int(sys.argv[2])
(fixtures / "task.yaml").write_text("""name: smoke_decision_extraction
description: P7 决策级联实测载荷任务（shape/回落/记账断言用，非质量评测）
prompt_template: |
  从下面的文本中抽出年龄数字，只输出 JSON：{"age": "<数字>"}

  文本：{input}
metrics: [llm_judge]
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
    # 一半样本给错误答案（分数落需诊断线下 → 回落生成式拿诊断），
    # 一半给正确答案（分数高 → 决策分可信与否由 confidence 决定）。
    answer = str(age) if i % 2 else str(age + 1)
    samples.append({
        "id": f"s-{i:04d}",
        "input": f"患者{'男' if i % 2 else '女'}性，{age}岁，初诊。",
        "expected": {"age": str(age)},
        "split": "test",
    })
    samples[-1]["_answer"] = answer
(fixtures / "dataset.yaml").write_text("name: smoke_decision_ds\nsamples:\n" + "".join(
    f"""  - id: {s["id"]}
    input: {s["input"]}
    expected: {json.dumps(s["expected"], ensure_ascii=False)}
    split: test
""" for s in samples))
print(f"  生成 {len(samples)} 条样本")
PY

echo "==> 发起决策级联 run（--judge-backend decision / 置信阈值 $CONFIDENCE / 诊断线 $DIAG_BELOW）"
set +e
"$BIN" run \
  --task "$FIXTURES/task.yaml" \
  --candidate "$FIXTURES/candidate.yaml" \
  --dataset "$FIXTURES/dataset.yaml" \
  --base-url "$BASE_URL" --model "$MODEL" --api-key "$API_KEY" \
  --out "$SMOKE_ROOT/runs" --headless \
  --workers "$WORKERS" --max-tokens "$MAX_TOKENS" \
  --judge-backend decision \
  --judge-decision-url "$DECISION_URL" --judge-decision-model "$DECISION_MODEL" \
  --judge-decision-confidence "$CONFIDENCE" --judge-decision-diag-below "$DIAG_BELOW" \
  > "$OUTDIR/summary.json" 2> "$OUTDIR/run.log"
CODE=$?
set -e
echo "  进程退出码：$CODE（run.log 尾部见 $OUTDIR/run.log）"

echo "==> 断言与记录"
python3 - "$OUTDIR" "$SMOKE_ROOT/runs" "$CODE" "$CONFIDENCE" "$DIAG_BELOW" "$DECISION_MODEL" <<'PY'
import json, pathlib, sys

outdir, runs_root, proc_code, conf_thr, diag_thr, decision_model = (
    pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), int(sys.argv[3]),
    float(sys.argv[4]), float(sys.argv[5]), sys.argv[6])

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
calls = [json.loads(p.read_text()) for p in sorted((run_dir / "calls").glob("*.json"))]

dec = [c for c in calls if c.get("stage") == "judge-decision"]
gen = [c for c in calls if c.get("stage") == "judge"]
exec_calls = [c for c in calls if not c.get("stage")]

# 1. 决策调用真实发生且模型名正确（请求 shape 的最终真值：服务端 2xx 接受了
#    {model, state, questions}——预检已证 shape，run 内证明全样本走通）。
check(len(dec) >= 1, "决策调用落 trace（stage=judge-decision）", f"decision={len(dec)}/{len(calls)}")
check(all(c["request"].get("model") == decision_model for c in dec),
      "决策请求模型名 = DECISION_MODEL", decision_model)
check(proc_code == 0 and s.get("exit_code") == 0, "run 成功（退出码 0）",
      f"process={proc_code} summary={s.get('exit_code')}")

# 2. 回落语义与阈值一致：请求体里可重建该问题 levels 数（写死于脚本 rubric=4）。
LEVELS = 4
trusted, fell_back = [], []
for c in dec:
    ans = json.loads(c["response"]["content"])["answers"]["q1"]
    conf = ans["confidence"]
    norm = min(max(ans["score"] / (LEVELS - 1), 0.0), 1.0)
    if conf < conf_thr or norm < diag_thr:
        fell_back.append((c, conf, norm))
    else:
        trusted.append((c, conf, norm))

check(all(len([g for g in gen if g["sample_id"] == c["sample_id"]]) == 1 for c, _, _ in fell_back),
      "回落样本恰有一次生成式裁判调用",
      f"fell_back={len(fell_back)} gen_calls={len(gen)}")
check(all(not [g for g in gen if g["sample_id"] == c["sample_id"]] for c, _, _ in trusted),
      "可信样本无生成式调用", f"trusted={len(trusted)}")

# 诊断占位/真诊断二分（sample trace 侧）。
sample_traces = [json.loads(p.read_text()) for p in sorted((run_dir / "samples").glob("*.json"))]
placeholder = sum(1 for st in sample_traces
                  if (st.get("diagnosis") or {}).get("llm_judge", "").startswith("决策裁判采信"))
real_diag = sum(1 for st in sample_traces
                if (st.get("diagnosis") or {}).get("llm_judge", "")
                and not (st.get("diagnosis") or {}).get("llm_judge", "").startswith("决策裁判采信"))
check(placeholder == len(trusted), "可信样本诊断为确定性占位（含 confidence）",
      f"placeholder={placeholder} trusted={len(trusted)}")
check(real_diag == len(fell_back), "回落样本拿到生成式中文诊断",
      f"real_diag={real_diag} fell_back={len(fell_back)}")

# 3. 归一化有界 + usage 记账：RoleJudge = decision 调用之和（+ 回落的生成式）。
check(all(0.0 <= norm <= 1.0 for _, _, norm in trusted + fell_back),
      "归一化分落在 [0,1]")
dec_usage_p = sum(c["response"]["usage"]["prompt_tokens"] for c in dec)
dec_usage_c = sum(c["response"]["usage"]["completion_tokens"] for c in dec)
gen_usage_p = sum(c["response"]["usage"]["prompt_tokens"] for c in gen)
gen_usage_c = sum(c["response"]["usage"]["completion_tokens"] for c in gen)
roles = s.get("usage_by_role") or {}
j = roles.get("judge") or {}
check(j.get("prompt_tokens") == dec_usage_p + gen_usage_p and
      j.get("completion_tokens") == dec_usage_c + gen_usage_c,
      "RoleJudge = decision(+generative) usage 之和",
      f"summary={j.get('prompt_tokens')}/{j.get('completion_tokens')} "
      f"wire={dec_usage_p + gen_usage_p}/{dec_usage_c + gen_usage_c}")
check(all(c["response"]["usage"]["output_tokens"] > 0 for c in dec),
      "decision 调用 output_tokens 如实计量（>0）",
      f"min={min(c['response']['usage']['output_tokens'] for c in dec)}")

# confidence 分布与回落率（记录到产物目录，供后续阈值标定）。
confs = sorted(json.loads(c["response"]["content"])["answers"]["q1"]["confidence"] for c in dec)
report = {
    "run_id": s["run_id"], "decision_model": decision_model,
    "confidence_threshold": conf_thr, "diag_below": diag_thr,
    "samples": len(sample_traces), "decision_calls": len(dec),
    "trusted": len(trusted), "fell_back": len(fell_back),
    "fallback_rate": round(len(fell_back) / max(len(dec), 1), 4),
    "confidence_min": confs[0] if confs else None,
    "confidence_max": confs[-1] if confs else None,
    "confidence_all": confs,
}
(outdir / "confidence-report.json").write_text(
    json.dumps(report, ensure_ascii=False, indent=2) + "\n")
print(f"  confidence 分布：min={report['confidence_min']} max={report['confidence_max']} "
      f"回落率={report['fallback_rate']}（明细见 {outdir / 'confidence-report.json'}）")

if failures:
    print(f"\nFAIL：{len(failures)} 项断言未过：", *failures, sep="\n  - ", file=sys.stderr)
    sys.exit(1)
print("\n全部断言通过。")
PY
echo "==> smoke-decision 完成：$OUTDIR"

#!/usr/bin/env python3
# bench/ 基准对照脚手架——仅服务 docs/benchmark/protocol.md 的等预算对照实验：
# 不进 go build、非 PromptOpt 运行时依赖。
"""DSPy MIPROv2 等预算对照 runner（协议骨架，提案 roadmap-v7-proposal §1.3）。

等预算口径（协议 §等预算定义）：
- executor 侧双上限同时钉：评估调用次数 N（--max-metric-calls，对齐 GEPA
  max_metric_calls 语义：每次「程序执行并被判分」计 1）∥ token 预算 T
  （--token-budget，LM 历史合计）。
- 脚手架的诚实边界：N/T 在**相边界**复核（baseline 评估 / 优化 / 终评三
  相），不在调用间硬中断——MIPROv2 的内部预算旋钮随版本演化（提案 §6-B：
  正式开跑前钉死），伪造逐调用硬停是伪精度。run 结束输出 budget_actual
  的 within_n / within_t；**双上限任一越界的 run 在协议下作废**，须收窄
  旋钮（trainset 规模 / max_*_demos / auto 档位）重跑。
- 优化侧开销（proposal/candidate 生成与 bootstrap 评估）全额计量、按相
  披露，与评估侧分开——PromptOpt 侧的 core.Role 分角色计量与之对表。

判分：GSM8K 数值归一化 exact_match（不引入 llm_judge 的跨实现不可比）。

dspy 3.4.0 旋钮硬约束（tcmsp-30 冒烟实测 2026-10-08，收窄时勿越界）：
- `max_bootstrapped_demos` 下限 1（传 0 在 BootstrapFewShot 内部
  `randint(1, 0)` 崩溃）；
- MIPROv2 的 trainset ≥ 2 条（无 valset 时 `_set_and_validate_datasets`
  直接 ValueError）；auto=light、train_n=2、demos 1/1 的实测判分调用
  ≈31 次——等预算 N 低于此须改协议规模或升 auto 档说明，不能压到 0。

用法见 bench/README.md。输出 JSON：config 回显、三相计量、双上限达标
标记、baseline 与优化后程序的逐样本分数（喂 bootstrap_ci.py）。
"""
from __future__ import annotations

import argparse
import json
import random
import re
import time
from pathlib import Path

import dspy

NUM_RE = re.compile(r"-?\$?[\d.]+")


def extract_number(text: str) -> str | None:
    """取文本中最后一个数值（GSM8K 判分的通行归一化）：先剥千分位逗号，
    再对命中 token 去尾部句点、并把全零小数部归一（"72." / "72.0" 都读 72），
    避免表面形态差异误判对错。"""
    nums = NUM_RE.findall((text or "").replace(",", ""))
    if not nums:
        return None
    tok = nums[-1].rstrip(".")
    if "." in tok:
        tok = tok.rstrip("0").rstrip(".")
    return tok or None


def gsm8k_exact_match(example, pred) -> float:
    want = extract_number(getattr(example, "answer", ""))
    got = extract_number(getattr(pred, "answer", "") or str(pred))
    return 1.0 if want is not None and got == want else 0.0


class CallCounter:
    """评估调用计数器：本脚手架与 MIPROv2 内部的每一次判分都经它计数。"""

    def __init__(self) -> None:
        self.calls = 0

    def metric(self, example, pred, trace=None) -> float:
        self.calls += 1
        return gsm8k_exact_match(example, pred)


def load_jsonl(path: Path) -> list[dict]:
    rows = []
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def lm_snapshot(lm) -> dict:
    """LM 历史累计 (调用数, prompt_tokens, completion_tokens)。

    dspy 各版本 history 条目形状有差异：统一按 dict.usage 取数，取不到的
    条目计入 metering_coverage=partial（结果里如实标注，不静默补零冒充）。
    """
    calls = prompt = completion = covered = 0
    for entry in getattr(lm, "history", None) or []:
        calls += 1
        usage = entry.get("usage") if isinstance(entry, dict) else None
        if isinstance(usage, dict):
            prompt += int(usage.get("prompt_tokens") or 0)
            completion += int(usage.get("completion_tokens") or 0)
            covered += 1
    partial = calls > 0 and covered < calls
    return {"calls": calls, "prompt_tokens": prompt, "completion_tokens": completion,
            "total_tokens": prompt + completion, "metering_coverage": "partial" if partial else "full"}


def phase_delta(lm, before: dict) -> dict:
    after = lm_snapshot(lm)
    return {
        "calls": after["calls"] - before["calls"],
        "prompt_tokens": after["prompt_tokens"] - before["prompt_tokens"],
        "completion_tokens": after["completion_tokens"] - before["completion_tokens"],
        "total_tokens": after["total_tokens"] - before["total_tokens"],
        "metering_coverage": "full" if after["metering_coverage"] == "full" else "partial",
    }


def evaluate_rows(program, rows: list[dict], counter: CallCounter) -> list[dict]:
    """逐样本评估并落逐样本分数（相内不做硬中断，见模块 docstring）。"""
    per_sample = []
    for row in rows:
        ex = dspy.Example(question=row["question"], answer=row["answer"]).with_inputs("question")
        try:
            pred = program(question=row["question"])
        except Exception as exc:  # 单样本失败按 0 计并标注，与 PromptOpt 的失败语义对表
            per_sample.append({"id": row["id"], "score": 0.0, "error": str(exc)[:120]})
            continue
        per_sample.append({"id": row["id"], "score": counter.metric(ex, pred)})
    return per_sample


def mean_score(per_sample: list[dict]) -> float:
    return sum(r["score"] for r in per_sample) / len(per_sample) if per_sample else 0.0


def main() -> None:
    ap = argparse.ArgumentParser(description="dspy MIPROv2 等预算对照（脚手架）")
    ap.add_argument("--dev", required=True, help="dev 子集 jsonl（MIPROv2 trainset 来源）")
    ap.add_argument("--test", required=True, help="test 子集 jsonl（heldout 终评）")
    ap.add_argument("--base-url", required=True, help="OpenAI 兼容网关，含 /v1")
    ap.add_argument("--model", required=True)
    ap.add_argument("--api-key", default="1")
    ap.add_argument("--max-metric-calls", type=int, required=True, help="等预算上限 N")
    ap.add_argument("--token-budget", type=int, required=True, help="等预算上限 T（token）")
    ap.add_argument("--max-tokens", type=int, default=512)
    ap.add_argument("--train-n", type=int, default=8, help="MIPROv2 trainset 截取条数（从 dev 头部）")
    ap.add_argument("--test-n", type=int, default=0, help="终评截取条数（0=全量 test 子集；冒烟用）")
    ap.add_argument("--auto", default="light", help="MIPROv2 auto 档位（默认 light）")
    ap.add_argument("--max-bootstrapped-demos", type=int, default=1)
    ap.add_argument("--max-labeled-demos", type=int, default=1)
    ap.add_argument("--seed", type=int, default=20260930)
    ap.add_argument("--out", required=True, help="结果 JSON 落点")
    args = ap.parse_args()

    random.seed(args.seed)
    dev_rows, test_rows = load_jsonl(Path(args.dev)), load_jsonl(Path(args.test))
    trainset = [
        dspy.Example(question=r["question"], answer=r["answer"]).with_inputs("question")
        for r in dev_rows[: args.train_n]
    ]
    heldout = test_rows[: args.test_n] if args.test_n else test_rows

    lm = dspy.LM(
        f"openai/{args.model}", api_base=args.base_url, api_key=args.api_key,
        model_type="chat", temperature=0, max_tokens=args.max_tokens,
        cache=False,  # 基准禁缓存：缓存命中让重复调用绕过真实执行，N/T 计量失真
    )
    dspy.configure(lm=lm)
    counter = CallCounter()
    student = dspy.Predict("question -> answer")

    result = {
        "scaffold": "bench/run_dspy.py（协议骨架，非完整基准发布）",
        "dspy_version": getattr(dspy, "__version__", "unknown"),
        "config": {
            "model": args.model, "base_url": args.base_url, "temperature": 0,
            "max_tokens": args.max_tokens, "seed": args.seed,
            "train_n": len(trainset), "heldout_n": len(heldout),
            "auto": args.auto,
            "max_bootstrapped_demos": args.max_bootstrapped_demos,
            "max_labeled_demos": args.max_labeled_demos,
        },
        "budget": {"max_metric_calls": args.max_metric_calls, "token_budget": args.token_budget},
        "phases": {}, "scores": {},
    }

    # 相 1：baseline（零样本）终评——评估侧开销。
    before = lm_snapshot(lm)
    t0 = time.time()
    baseline_per = evaluate_rows(student, heldout, counter)
    result["phases"]["baseline_eval"] = {**phase_delta(lm, before), "wall_s": round(time.time() - t0, 2)}
    result["scores"]["baseline"] = round(mean_score(baseline_per), 6)

    # 相 2：MIPROv2 优化——优化侧开销（提案：不强行对齐但全额计量披露）。
    before = lm_snapshot(lm)
    t0 = time.time()
    optimizer = dspy.MIPROv2(metric=counter.metric, auto=args.auto)
    optimized = optimizer.compile(
        student, trainset=trainset,
        max_bootstrapped_demos=args.max_bootstrapped_demos,
        max_labeled_demos=args.max_labeled_demos,
        requires_permission_to_run=False,
    )
    result["phases"]["optimize"] = {**phase_delta(lm, before), "wall_s": round(time.time() - t0, 2)}

    # 相 3：优化后程序终评——评估侧开销。
    before = lm_snapshot(lm)
    t0 = time.time()
    optimized_per = evaluate_rows(optimized, heldout, counter)
    result["phases"]["final_eval"] = {**phase_delta(lm, before), "wall_s": round(time.time() - t0, 2)}
    result["scores"]["optimized"] = round(mean_score(optimized_per), 6)

    # 双上限复核（协议：任一越界的 run 作废，须收窄旋钮重跑）。
    total_calls = counter.calls
    total_tokens = lm_snapshot(lm)["total_tokens"]
    result["budget_actual"] = {
        "metric_calls": total_calls, "tokens": total_tokens,
        "within_n": bool(args.max_metric_calls <= 0 or total_calls <= args.max_metric_calls),
        "within_t": bool(args.token_budget <= 0 or total_tokens <= args.token_budget),
    }
    result["per_sample"] = {"baseline": baseline_per, "optimized": optimized_per}

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({
        "scores": result["scores"], "phases": result["phases"],
        "budget_actual": result["budget_actual"], "out": str(out),
    }, ensure_ascii=False))
    if not (result["budget_actual"]["within_n"] and result["budget_actual"]["within_t"]):
        print("WARN: 双上限越界——协议下该 run 作废，收窄 train_n / demos / auto 档位后重跑",
              flush=True)


if __name__ == "__main__":
    main()

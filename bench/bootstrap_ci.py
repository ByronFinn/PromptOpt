#!/usr/bin/env python3
# bench/ 基准对照脚手架——仅服务 docs/benchmark/protocol.md 的等预算对照实验：
# 不进 go build、非 PromptOpt 运行时依赖。
"""配对自助法置信区间（与 cmd/promptopt/verify_stats.go 同一统计口径）。

自食其力（roadmap-v7-proposal §1.3）：我们要求用户 verify 门禁达到的标准，
自己 README 的基准数字先达到。因此这里的数学逐条对照 verify_stats.go：

  - 配对差分  D_i = base_i − deliv_i（两侧同集逐样本分数，按 id 配对）；
  - B = 1000 次有放回重采样，取重采样均值分布；
  - CI = [2.5% 分位, 97.5% 分位]，分位数线性插值（同 percentile()）；
  - 判定：CI 下界 > --max-regression → regressed（默认阈 0.05，同 verify）；
          CI 上界 ≤ 0            → confident_pass；
          两者之间               → inconclusive（诚实标注「差异未超噪声区间」）。

实现差异（如实披露）：verify 用 Go math/rand/v2 的固定 PCG 种子做重采样，
本脚本用 Python random 的固定种子（默认 20260930）。重采样只是噪声测量
装置而非信息来源，两个实现的具体 CI 数值允许末位差异，判定标准完全一致。

输入协议：两个 JSONL，每行 {"id": str, "score": float}（PromptOpt 侧由
runs/<id>/samples/*.json 的 Scores[primary] 汇出；dspy 侧由 run_dspy.py
的 per_sample 落盘），两侧 id 集合必须一致（同集评估是配对的前提）。
"""
from __future__ import annotations

import argparse
import json
import random
from pathlib import Path

B_RESAMPLES = 1000
DEFAULT_SEED = 20260930
DEFAULT_MAX_REGRESSION = 0.05


def load_scores(path: Path) -> dict[str, float]:
    scores = {}
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            row = json.loads(line)
            scores[row["id"]] = float(row["score"])
    return scores


def percentile(sorted_vals: list[float], q: float) -> float:
    """线性插值分位数——与 verify_stats.go percentile 同式。"""
    n = len(sorted_vals)
    if n == 0:
        return 0.0
    if n == 1:
        return sorted_vals[0]
    pos = q * (n - 1)
    lo = int(pos)
    hi = min(lo + 1, n - 1)
    frac = pos - lo
    return sorted_vals[lo] + frac * (sorted_vals[hi] - sorted_vals[lo])


def paired_bootstrap_ci(base: list[float], deliv: list[float], b: int,
                        rng: random.Random) -> tuple[float, float]:
    """CI 上下界：与 verify_stats.go pairedBootstrapCI 同一数学。"""
    n = len(base)
    diffs = [base[i] - deliv[i] for i in range(n)]
    means = []
    for _ in range(max(b, 1)):
        total = 0.0
        for _ in range(n):
            total += diffs[rng.randrange(n)]
        means.append(total / n)
    means.sort()
    return percentile(means, 0.025), percentile(means, 0.975)


def bootstrap_verdict(lo: float, hi: float, max_regression: float) -> str:
    """三态判定：与 verify_stats.go bootstrapVerdict 同一映射。"""
    if lo > max_regression:
        return "regressed"
    if hi <= 0:
        return "confident_pass"
    return "inconclusive"


def main() -> None:
    ap = argparse.ArgumentParser(description="配对自助法 CI（verify 同口径）")
    ap.add_argument("--base", required=True, help="baseline 侧逐样本分数 jsonl")
    ap.add_argument("--deliv", required=True, help="对照侧逐样本分数 jsonl")
    ap.add_argument("--max-regression", type=float, default=DEFAULT_MAX_REGRESSION)
    ap.add_argument("--seed", type=int, default=DEFAULT_SEED)
    ap.add_argument("--b", type=int, default=B_RESAMPLES)
    args = ap.parse_args()

    base = load_scores(Path(args.base))
    deliv = load_scores(Path(args.deliv))
    if base.keys() != deliv.keys():
        only_base = sorted(base.keys() - deliv.keys())
        only_deliv = sorted(deliv.keys() - base.keys())
        raise SystemExit(f"FATAL: 两侧样本集不一致 base-only={only_base[:5]} deliv-only={only_deliv[:5]}")
    if not base:
        raise SystemExit("FATAL: 空样本集")

    ids = sorted(base.keys())
    lo, hi = paired_bootstrap_ci([base[i] for i in ids], [deliv[i] for i in ids],
                                 args.b, random.Random(args.seed))
    verdict = bootstrap_verdict(lo, hi, args.max_regression)
    mean_d = sum(base[i] - deliv[i] for i in ids) / len(ids)
    print(json.dumps({
        "n": len(ids),
        "b": args.b,
        "seed": args.seed,
        "mean_delta_base_minus_deliv": round(mean_d, 6),
        "ci95": [round(lo, 6), round(hi, 6)],
        "max_regression": args.max_regression,
        "verdict": verdict,
    }, ensure_ascii=False))
    if verdict == "inconclusive":
        print("注：差异未超噪声区间，样本量不足——报告必须如实标注（协议 §统计）",
              flush=True)


if __name__ == "__main__":
    main()

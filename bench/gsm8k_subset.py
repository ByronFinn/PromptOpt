#!/usr/bin/env python3
# bench/ 基准对照脚手架——仅服务 docs/benchmark/protocol.md 的等预算对照实验：
# 不进 go build、非 PromptOpt 运行时依赖。
"""GSM8K 固定子集抽取（协议骨架）。

协议口径（roadmap-v7-proposal §1.3）：
- dev 200 / test 500，钉 seed（默认 20260930，与提案落稿日一致），抽样
  可复现：同输入 jsonl + 同 seed → 字节相同的子集。
- 诚实标注（提案 §6-B）：GEPA / MIPROv2 / p¹ 论文各自使用的 GSM8K 子集与
  划分未在本仓核实；本脚手架只钉「本仓协议」的子集，正式发布前须从论文
  与官方 repo 逐一核对后决定是否对齐。

输入文件协议：GSM8K 官方发布格式的 JSONL，每行
  {"question": str, "answer": str}，answer 形如 "...算式...\\n#### 42"。

输出：--out-dir 下 dev.jsonl / test.jsonl，每行
  {"id": "dev-0001", "question": str, "answer": "42"}（answer 抽出 #### 后
  的标准答案，判分走 run_dspy.py 的数值归一化）。
"""
from __future__ import annotations

import argparse
import json
import random
from pathlib import Path

DEFAULT_SEED = 20260930
DEV_N = 200
TEST_N = 500


def final_answer(raw: str) -> str:
    """抽出 GSM8K answer 字段 #### 之后的标准答案。"""
    marker = "####"
    if marker in raw:
        return raw.rsplit(marker, 1)[1].strip()
    return raw.strip()


def load_jsonl(path: Path) -> list[dict]:
    rows = []
    with path.open(encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def sample_rows(rows: list[dict], n: int, split: str, rng: random.Random) -> list[dict]:
    if n > len(rows):
        raise SystemExit(f"FATAL: {split} 需 {n} 条，输入仅 {len(rows)} 条")
    picked = rng.sample(rows, n)
    out = []
    for i, row in enumerate(picked, 1):
        out.append({
            "id": f"{split}-{i:04d}",
            "question": row["question"],
            "answer": final_answer(row["answer"]),
        })
    return out


def write_jsonl(path: Path, rows: list[dict]) -> None:
    with path.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False) + "\n")


def main() -> None:
    ap = argparse.ArgumentParser(description="GSM8K 固定子集抽取（钉 seed）")
    ap.add_argument("--train", required=True, help="GSM8K 官方 train jsonl（dev 子集来源）")
    ap.add_argument("--test", required=True, help="GSM8K 官方 test jsonl（test 子集来源）")
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--dev-n", type=int, default=DEV_N)
    ap.add_argument("--test-n", type=int, default=TEST_N)
    ap.add_argument("--seed", type=int, default=DEFAULT_SEED)
    args = ap.parse_args()

    # 每个 split 独立建流：同一 seed 下 dev 与 test 的抽取互不干扰，
    # 且扩缩子集规模（--dev-n/--test-n）不改变已抽样本的构成。
    dev = sample_rows(load_jsonl(Path(args.train)), args.dev_n, "dev", random.Random(args.seed))
    test = sample_rows(load_jsonl(Path(args.test)), args.test_n, "test", random.Random(args.seed))

    out = Path(args.out_dir)
    out.mkdir(parents=True, exist_ok=True)
    write_jsonl(out / "dev.jsonl", dev)
    write_jsonl(out / "test.jsonl", test)
    print(f"dev={len(dev)} test={len(test)} seed={args.seed} -> {out}/{{dev,test}}.jsonl")


if __name__ == "__main__":
    main()

# bench/ — 基准对照脚手架（Python）

> **定位（先读）**：本目录仅服务 [docs/benchmark/protocol.md](../docs/benchmark/protocol.md) 的
> 等预算对照实验（V7 提案 §1.3），**不进 `go build`、不是 PromptOpt 的运行时依赖**。
> 仓库的 Go 门禁（build / vet / test）不扫描本目录；这里的 Python 代码不参与
> 任何发布产物。完整基准发布（README 数字 + 全协议产物）是提案 P1#7，
> 本期只交付可运行脚手架。

## 文件

| 文件 | 作用 | 依赖 |
|---|---|---|
| `requirements.txt` | 钉版清单（dspy==3.4.0，2026-10-08 取自 PyPI JSON API 的最新版；协议实际开跑前须复核一次） | - |
| `gsm8k_subset.py` | 从 GSM8K 官方 jsonl 抽固定子集（钉 seed，dev/test 划分入协议） | 纯标准库 |
| `run_dspy.py` | DSPy MIPROv2 单模块等预算对照 runner：双上限（评估调用次数 N ∥ token 预算 T）、分相计量、逐样本分数落盘 | dspy |
| `bootstrap_ci.py` | 配对自助法 CI + 三态判定，与 `cmd/promptopt/verify_stats.go` 同一口径 | 纯标准库 |

## 快速开始（冒烟规模）

```bash
python3 -m venv .venv && .venv/bin/pip install -r bench/requirements.txt

# 1) 固定子集（真实 GSM8K jsonl 由实施者下载；文件协议见 gsm8k_subset.py 头注）
python3 bench/gsm8k_subset.py --train gsm8k-train.jsonl --test gsm8k-test.jsonl --out-dir bench/data

# 2) dspy 侧等预算 run（OpenAI 兼容网关；--max-metric-calls 与 --token-budget 同时钉）
python3 bench/run_dspy.py --dev bench/data/dev.jsonl --test bench/data/test.jsonl \
  --base-url http://127.0.0.1:11434/v1 --model <model> --api-key 1 \
  --max-metric-calls 24 --token-budget 20000 --seed 0 --out bench/out/dspy-seed0.json

# 3) 与 PromptOpt 侧逐样本分数做配对自助法（与 verify 门禁同口径）
python3 bench/bootstrap_ci.py --base base-per-sample.jsonl --deliv dspy-per-sample.jsonl
```

## 边界（诚实声明）

- **单模块等预算对照**：`run_dspy.py` 用 `dspy.Predict` 单模块配置做最小对等，
  结论边界为「单模块任务的等预算对比」，不外推到多 stage program。
- **统计口径自食其力**：`bootstrap_ci.py` 复刻 verify 的配对自助法
  （B=1000、2.5%/97.5% 百分位、regressed / confident_pass / inconclusive 三态）——
  我们要求用户门禁达到的标准，自己的 README 数字先达到。重采样 RNG 实现
  不同（Go PCG vs Python random），具体 CI 值允许末位差异，判定标准一致。
- **token 计量粒度**：dspy 侧按调用历史分相合计（优化相 / 评估相），token
  预算的强制点在相边界（粗粒度软停）；PromptOpt 侧 `core.Role` 分角色计量
  是逐调用硬账。两侧口径差异在结果表中如实披露。

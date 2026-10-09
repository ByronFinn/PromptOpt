# 基准 0001：GSM8K 子集等预算对照——PromptOpt（GEPA）vs DSPy MIPROv2

> **Status**: 单 seed 试点结果（非 P1#7 正式发布）｜ **Executed**: 2026-10-09 ｜ **依据**: [roadmap-v7-proposal.md](../roadmap-v7-proposal.md) §1.3、[protocol.md](protocol.md)
>
> 一次、单 seed、小样本（test 40）的试点对照。结论边界严格限定为「本仓协议子集、该端点该模型、该预算档下的单次运行」；不外推、不进 README（理由见 §7）。

## 1. 结论速览

| 指标（GSM8K test 40，逐样本 failure=0 口径） | DSPy MIPROv2 (3.4.0) | PromptOpt GEPA (92010d9+工作树) |
|---|---|---|
| baseline 分数 | **0.925**（37/40） | **0.875**（35/40） |
| 优化后分数 | **0.950**（38/40） | **0.875**（35/40，逐样本与 baseline 全同） |
| Δ（优化后 − baseline） | +0.025 | 0.000 |
| 配对自助法判定（B=1000） | `confident_pass`（CI [−0.075, 0.0]，优化侧确证不劣） | `confident_pass`（CI [0, 0]，平凡全同） |

跨框架参照（同为 inconclusive，n=40 分辨率所限）：

| 配对（base − deliv） | Δ均值 | CI95 | 判定 |
|---|---|---|---|
| dspy-optimized − po-optimized | +0.075 | [0.0, 0.175] | **inconclusive** |
| dspy-baseline − po-baseline | +0.05 | [0.0, 0.125] | **inconclusive** |

一句话结论：**该预算档下 MIPROv2 拿到了确证不劣的 +0.025（净 +1 题）；PromptOpt 的 GEPA 交付了 baseline 原文（dev 优化集被基线饱和，无严格占优变异可交付），test 侧 Δ=0；跨框架 0.075 的差距未超噪声带，按协议必须标注 inconclusive，不构成「更优」结论。**

## 2. 等预算口径（写死）

协议同 [protocol.md](protocol.md) §3：**executor 侧评估调用数 N 为主帽，token 全额分相披露不强对齐**（机制异构，强行对齐 token 是伪精度）。

| 相 | DSPy 调用数 | DSPy tokens (P+C) | PromptOpt 调用数 | PromptOpt tokens (P+C) |
|---|---|---|---|---|
| test baseline 终评 | 41¹ | 24,185（10,587+13,598） | 40 | 16,325（5,323+11,002） |
| 优化相（GSM8K dev 侧候选判分） | 68（另有 4 次指令提议调用，非判分） | 46,115（27,308+18,807） | **68**（帽=对齐值；另 4 次探针沉没²） | executor 27,272（11,809+15,463，含探针）；optimizer 角色 13,130（4,508+8,622，合成+反思）³ |
| test optimized 终评 | 40 | 21,702（10,291+11,411） | 40 | 17,479（5,323+12,156） |
| **合计** | **149 判分**（153 次 LM 调用） | **92,002** | **152 评估** | **74,206** |

¹ DSPy baseline 相 41 次 = 40 样本 + 1 次适配器重试（test-0033 空响应，该行记 0）。
² 探针沉没：PromptOpt 零配置管线在检查点前对 LLM 现场合成的 2 条占位样本跑 2 变体探针（4 次评估）；样本注入后这些评估不接触 GSM8K 数据，作为机制成本单列。GSM8K 侧优化判分帽两边同为 68（`--budget-evals 72 = 68 + 4`，实际精确耗尽：探针 4 + kept 上 baseline 8 + GEPA 循环 60，run 以 `budget_exhausted`/退出码 2 收束）。
³ PromptOpt 工件按角色只记 token（core.Usage），optimizer 角色不分调用数；DSPy 侧提议调用数 4 = 72 LM − 68 判分，由 `lm.history` 分相计量得出。

**优化侧开销不强行对齐**（协议原文）：MIPROv2 的 bootstrap 判分 68 次与 PromptOpt 的「探针 4 + kept baseline 8 + 循环 60」结构不同，但**判分帽同值**、双方全部优化侧 token 上表全额披露。

## 3. 五同与处理方式

| 项 | 两边一致的处理 |
|---|---|
| executor 模型 | `jiuwei-tcm` @ `http://192.168.56.39:59001/v1`（llama.cpp llama-server，无密钥，api_key 任意非空） |
| 温度 | **两边线上都显式 `temperature: 0`（贪心）**。DSPy：`dspy.LM(temperature=0)`。PromptOpt：`--temperature 0` 语义是「字段不上线」（`cmd/promptopt/run.go` 旗标帮助），故用 `--extra-body '{"temperature":0}'` 顶层合并显式上线（`internal/provider/openai.go` `mergeExtraBody`：同名键用户值优先）。实测缺省温度非贪心（两次调用 completion 49/116 tokens 不同），必须显式钉。 |
| max_tokens | 2048 两边统一。jiuwei-tcm 为推理模型：思考走响应的 `reasoning_content` 独立字段、`content` 为干净正文（curl 实测），两边框架都只读 content，判分不被思考污染；但思考计入 completion tokens（usage 口径两边同取网关 usage）与 2048 预算。max_tokens 给小会导致「只出思考不出正文」——本次 40 题上两框架各出现 2~3 例 `finish_reason=length` 空正文，按统一规则记 0。 |
| 数据划分 | `bench/gsm8k_subset.py --dev-n 8 --test-n 40 --seed 20260930`，每 split 独立建流、抽样可复现；dev8 = DSPy trainset = PromptOpt 注入 kept 集；test40 两边逐题同集。 |
| 判分 | **严格 exact_match：TrimSpace 后全等**，两边同规。PromptOpt 侧为内置指标原样（`internal/eval/metrics.go` `evalExactMatch` 字符串分支）；DSPy 侧把 `bench/run_dspy.py` 的数值归一化判分补丁为同一规则（补丁内容见 §6）。expected 均为 `####` 抽出的标准答案字符串。 |

## 4. 环境

| 项 | 值 |
|---|---|
| 负载执行机 | tcmsp-30（`jiuwei@192.168.66.30`，别名免密；hostname `tcmsp-gpu`，x86_64，16 核 31GB） |
| PromptOpt | 本机 `GOOS=linux GOARCH=amd64 go build` 交叉编译（**构建自 92010d9 之上的未提交工作树**，含 V7 统计层等在途改动——非干净 release，如实声明），二进制自报 `promptopt dev (commit 92010d99…)` |
| DSPy | `dspy==3.4.0`（`pip install "dspy[optuna]==3.4.0"`，venv：远端 `python3 3.10.12` 自带 ensurepip 成功，未动用 get-pip 兜底；随装 litellm 1.104.2 / gepa 0.1.4 / optuna 5.0.0） |
| 数据 | GSM8K 官方 jsonl（GitHub raw，train 7,473 行 / test 1,319 行全量校验通过） |
| 子集 | dev 8 / test 40，seed 20260930（`gsm8k_subset.py`，每 split 独立建流） |
| 墙钟 | DSPy 全程 ≈23 min（03:09–03:32 UTC）；PromptOpt 侧合计 ≈9 min（三件套终评 2×~3.5 min + 零配置 run ~1.5 min + 微冒烟）；总墙钟在 2 小时约束内 |

## 5. PromptOpt 侧的关键接缝：检查点注入（如实说明）

**PromptOpt 当前形态下「优化循环」只存在于零配置模式**（三件套 `run` 只评估不优化，`--optimizer` 在配置模式显式报错；`cmd/promptopt/run.go`）。为在真实 GSM8K 上跑优化循环，使用了管线自带的交互检查点：

1. `run "<seed prompt>" --interactive` 启动零配置 run，合成管线（spec/2 条占位样本/探针）完成后在检查点阻塞（`internal/harness/checkpoint.go` 轮询 `synth/<id>/checkpoint.json`）；
2. 此时把 `spec.json` 换成 GSM8K 任务（`prompt_template` = 共享基线指令 + `{input}`，`metrics: [exact_match]`）、`samples.json` 换成真实 GSM8K dev8——**这是管线文档声明支持的手工编辑接缝**（`internal/harness/pipeline.go`："Re-read the artifacts so edits made while the checkpoint was open … take effect"），换入样本不在探针判定表中、按 `SelectKept` 全部保留（`internal/harness/filter.go`）；
3. 写 `checkpoint.json approved` → baseline 与 GEPA 循环都在真实 GSM8K dev8 上进行；交付候选取 `runs-opt/<id>/frontier.json` 的 `best.prompt`，再经三件套在 test40 上终评。

这使两框架的优化阶段都在**同一 dev8 真实数据**上进行；代价是 4 次合成占位样本上的探针沉没评估（§2 表已单列）。

## 6. 复现命令（全部负载在 tcmsp-30 上）

```bash
# 0) 本机：交叉编译并部署
env GOOS=linux GOARCH=amd64 go build -o /tmp/promptopt-bench ./cmd/promptopt
ssh tcmsp-30 mkdir -p ~/promptopt-bench/{bin,bench,data,out,inject,trio}
scp /tmp/promptopt-bench tcmsp-30:~/promptopt-bench/bin/
scp bench/run_dspy.py bench/gsm8k_subset.py bench/bootstrap_ci.py bench/requirements.txt tcmsp-30:~/promptopt-bench/bench/

# 1) 远端：venv + dspy（python3.10 自带 ensurepip，get-pip 兜底未触发）
ssh tcmsp-30 'cd ~/promptopt-bench && python3 -m venv venv \
  && ./venv/bin/pip install "dspy[optuna]==3.4.0"'

# 2) 远端：GSM8K 下载 + 固定子集
ssh tcmsp-30 'cd ~/promptopt-bench/data \
  && curl -sL -o gsm8k-train.jsonl https://raw.githubusercontent.com/openai/grade-school-math/master/grade_school_math/data/train.jsonl \
  && curl -sL -o gsm8k-test.jsonl  https://raw.githubusercontent.com/openai/grade-school-math/master/grade_school_math/data/test.jsonl'
ssh tcmsp-30 'cd ~/promptopt-bench && ./venv/bin/python bench/gsm8k_subset.py \
  --train data/gsm8k-train.jsonl --test data/gsm8k-test.jsonl --out-dir data \
  --dev-n 8 --test-n 40 --seed 20260930'

# 3) 远端：run_dspy.py 判分一致性补丁（严格 trim 全等，替换原数值归一化判分）
#    与共享基线指令签名（与 PromptOpt 基线提示词逐字同一句指令）：
#    - gsm8k_exact_match 改为：want/got 均 str().strip() 后全等
#    - 模块级注入 INSTRUCTION = "Answer the grade school math problem. Respond with only the final numeric answer."
#      与 class GsmSignature(dspy.Signature)（docstring 即该指令），main 内 student = dspy.Predict(GsmSignature)
#    （补丁后 ast.parse 校验通过；全文 diff 仅此两处）

# 4) DSPy 侧正式 run（23 min，结果 out/dspy-main.json）
ssh tcmsp-30 'cd ~/promptopt-bench && nohup ./venv/bin/python bench/run_dspy.py \
  --dev data/dev.jsonl --test data/test.jsonl \
  --base-url http://192.168.56.39:59001/v1 --model jiuwei-tcm --api-key 1 \
  --max-metric-calls 200 --token-budget 600000 \
  --max-tokens 2048 --train-n 8 --test-n 0 --auto light \
  --max-bootstrapped-demos 1 --max-labeled-demos 1 --seed 20260930 \
  --out out/dspy-main.json > out/dspy-main.log 2>&1 &'

# 5) PromptOpt 侧：检查点注入的零配置优化 run（驱动脚本见远端 bench/run_promptopt_opt.sh）
ssh tcmsp-30 '~/promptopt-bench/bench/run_promptopt_opt.sh 72 60000
#   = --optimizer gepa --max-rounds 5 --minibatch 4 --budget-evals 72 --budget-tokens 60000 \
#     --max-tokens 2048 --extra-body {"temperature":0} --workers 4 \
#     --samples 2 --probe-variants 2 --seed 20260930 --interactive'

# 6) PromptOpt 侧：三件套 test40 终评 ×2（baseline 与 frontier 交付候选）
ssh tcmsp-30 'cd ~/promptopt-bench && ./bin/promptopt-bench run \
  --task trio/task.yaml --candidate trio/candidate_baseline.yaml --dataset trio/dataset_test.yaml --split test \
  --base-url http://192.168.56.39:59001/v1 --model jiuwei-tcm --api-key 1 \
  --max-tokens 2048 --extra-body "{\"temperature\":0}" --workers 4 \
  --budget-evals 40 --budget-tokens 300000 --headless --out runs'
# candidate_optimized.yaml 同上（prompt 取自 runs-opt/<id>/frontier.json best.prompt）

# 7) 配对自助法 CI（与 verify 门禁同一数学：B=1000、2.5%/97.5%、阈 0.05）
ssh tcmsp-30 'cd ~/promptopt-bench \
  && ./venv/bin/python bench/bootstrap_ci.py --base out/dspy-baseline.jsonl --deliv out/dspy-optimized.jsonl \
  && ./venv/bin/python bench/bootstrap_ci.py --base out/po-baseline.jsonl  --deliv out/po-optimized.jsonl \
  && ./venv/bin/python bench/bootstrap_ci.py --base out/dspy-optimized.jsonl --deliv out/po-optimized.jsonl \
  && ./venv/bin/python bench/bootstrap_ci.py --base out/dspy-baseline.jsonl  --deliv out/po-baseline.jsonl'
```

## 7. 原始输出摘录

DSPy 三相计量（`out/dspy-main.json`，metering_coverage 全部 full）：

```json
{"scores": {"baseline": 0.925, "optimized": 0.95},
 "phases": {
  "baseline_eval": {"calls": 41, "prompt_tokens": 10587, "completion_tokens": 13598, "total_tokens": 24185, "wall_s": 516.61},
  "optimize":      {"calls": 72, "prompt_tokens": 27308, "completion_tokens": 18807, "total_tokens": 46115, "wall_s": 518.83},
  "final_eval":    {"calls": 40, "prompt_tokens": 10291, "completion_tokens": 11411, "total_tokens": 21702, "wall_s": 340.86}},
 "budget_actual": {"metric_calls": 149, "tokens": 92002, "within_n": true, "within_t": true}}
```

MIPROv2 编译日志尾部（dev8 上候选评估全 100 分——优化选择信号饱和）：

```text
INFO dspy.teleprompt.mipro_optimizer_v2: Scores so far: [100.0, 100.0, ..., 100.0]
INFO dspy.teleprompt.mipro_optimizer_v2: Best score so far: 100.0
INFO dspy.teleprompt.mipro_optimizer_v2: Returning best identified program with score 100.0!
```

PromptOpt 零配置 run（runs-opt/20261009-033331-074d6901，summary.json + stderr 结论块）：

```json
{"status": "budget_exhausted", "exit_code": 2, "total_samples": 8, "evaluated_samples": 8,
 "usage_by_role": {"executor": {"prompt_tokens": 11809, "completion_tokens": 15463},
                   "optimizer": {"prompt_tokens": 4508, "completion_tokens": 8622}},
 "metric_means": {"exact_match": 1}}
```

```text
最优候选: baseline · exact_match 1.0000
```

frontier.json：`rounds: 4`、`reason: budget_stopped`、成员仅 `baseline`（第 0 轮，exact_match 均值 1.0，独占占优 8/8）——GEPA 各轮变异在饱和的 dev8 上无一严格占优（1.0 打平即被支配/克隆规则拒之门外），交付候选即 baseline 原文。三件套终评两次 run（baseline：2 例 length 失败；optimized：3 例）逐样本分数**逐行全同**，均值均 0.875。

CI 输出原文（base − deliv 语义，B=1000，seed 20260930）：

```json
{"n": 40, "mean_delta_base_minus_deliv": -0.025, "ci95": [-0.075, 0.0], "verdict": "confident_pass"}   // dspy base→opt
{"n": 40, "mean_delta_base_minus_deliv":  0.0,   "ci95": [0.0, 0.0],     "verdict": "confident_pass"}   // po base→opt
{"n": 40, "mean_delta_base_minus_deliv":  0.075, "ci95": [0.0, 0.175],   "verdict": "inconclusive"}     // dspy-opt vs po-opt
{"n": 40, "mean_delta_base_minus_deliv":  0.05,  "ci95": [0.0, 0.125],   "verdict": "inconclusive"}     // dspy-base vs po-base
```

## 8. 诚实边界（全部如实声明）

1. **单 seed、单次运行**：协议要求 ≥3 seed 报均值±sd，本次为 2 小时墙钟内的单 run 试点，全部数字不具发布资格。
2. **子集非论文口径**：GSM8K 子集（dev8/test40，seed 20260930）为本仓协议钉的子集；GEPA/MIPROv2 论文各自使用的划分未核对（提案 §6-B），跨论文不可比。
3. **dev 优化集饱和**：模型在 dev8 上 baseline 即 1.0（MIPROv2 候选评估全 100 分、GEPA 变异全打平）——两个优化器都没有可学习的信号，这是本次 PromptOpt Δ=0 的直接机制原因，也是本次实验设计的主要教训：**优化集必须比模型能力难**。
4. **PromptOpt 侧注入路径是变通**：三件套模式原生无优化循环，真实数据经检查点手工编辑进入零配置管线（§5）；该接缝是管线声明支持的行为，但「配置模式 + 真实数据集优化」在当前产品形态下不存在。
5. **length 失败 = 模型+预算的真实边界**：思考失控烧满 2048 按统一规则记 0。分布：test-0033 在三处失败（dspy baseline 记适配器错误行、po 两次终评）；test-0025 在 po 两次终评失败（dspy 侧正常）；test-0010 仅在 po optimized 终评失败。PromptOpt optimized run 虽多 1 例失败，但该样本 baseline 时本就答错，逐样本分数与 baseline 逐行全同。
6. **MIPROv2 选中的指令文本未持久化**：`run_dspy.py` 只落分数与计量，优化后程序的 instruction/demos 结构随进程消亡，仅有日志证明候选全 100 分。
7. **PromptOpt optimizer 角色无调用数**：工件按角色只记 token（13,130），调用数未单列；DSPy 提议调用数 4 为 history 计量推得。
8. **PromptOpt 二进制构建自未提交工作树**（92010d9 + 在途 V7 改动），非干净 release 构建。
9. **README 不加数字节**：本试点单 seed 且 GEPA 侧交付无差异，不满足 README 证据门槛；README 数字按协议属 P1#7（≥3 seed、补 gepa 原版对照后）。protocol.md §0「本期只交付可运行脚手架」的边界保持不变——本文是该脚手架的首次实跑记录。
10. **远端工件即焚**：`~/promptopt-bench/` 在本文档落盘后清理，原始产物仅以本文摘录存续；复现按 §6 命令重跑。

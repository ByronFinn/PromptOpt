# sentiment_classification —— 中文电商评论情感三分类示例

PromptOpt 的指标示例：判断中文电商评论的情感倾向，输出 `positive` / `negative` / `neutral` 三者之一，用 **exact_match** 评分。与 [json_extraction](../json_extraction/) 的结构性指标互补，本例演示字符串标签类任务的完整工作流。

## 文件说明

| 文件 | 说明 |
| --- | --- |
| `task.yaml` | 任务规范：`prompt_template`（含 `{input}` 占位符）、指标 `exact_match`，主指标 `exact_match` |
| `candidate.yaml` | 候选提示词：含 `{input}` 占位符的分类指令，附带标签枚举约束 |
| `dataset.yaml` | 数据集：内联 6 条中文评论样本（train 2 / dev 2 / test 2），`expected` 为单个标签字符串 |

> 指标契约：`exact_match` 对字符串 `expected` 做去首尾空白后的**大小写敏感**比较——所以提示词要求输出英文小写标签，任何多余解释都会判 0 分。
> 提示词渲染使用字符串替换（`{input}` → 样本输入），而非模板引擎。

## 运行：基线评估（手工三件套）

```bash
promptopt run \
  --task task.yaml \
  --candidate candidate.yaml \
  --dataset dataset.yaml \
  --base-url http://192.168.56.39:59001/v1 \
  --model jiuwei-tcm \
  --api-key 1
```

`--base-url` / `--model` / `--api-key` 亦可分别由环境变量 `PROMPTOPT_BASE_URL` / `PROMPTOPT_MODEL` / `PROMPTOPT_API_KEY` 提供，命令行优先。加 `--split test` 只评估 test 切分；加 `--web` 在 `127.0.0.1:17700` 查看实时事件流。

**预期产出**：`runs/<run_id>/summary.json` 的 `metric_means.exact_match` 为基线准确率（如 `0.67` 表示 6 条中 4 条全对）；`samples/<id>.json` 逐条记录渲染后 prompt、模型响应与得分，错分样本的 `diagnosis` 会标注 `string mismatch`。

> 注意：手工三件套模式只跑**单轮基线评估**，不进入优化循环；此模式下显式传 `--optimizer` 会因用法错误直接退出 1。

## 优化：--optimizer（零配置模式）

优化循环运行在零配置模式下：用一个**位置参数提示词**替代三件套，合成管线自动生成任务规格与数据集（`--samples` 控制合成条数，默认 6；`--probe-variants` 控制方差过滤探针，默认 2），然后按 `--optimizer` 指定的范式迭代提示词。

```bash
promptopt run \
  "对中文电商评论做情感三分类，只输出 positive、negative、neutral 三个英文小写标签之一" \
  --optimizer gepa \
  --base-url http://192.168.56.39:59001/v1 \
  --model jiuwei-tcm \
  --api-key 1
```

范式说明（详见 [docs/plugins.md](../../docs/plugins.md)）：

| 范式 | 机制 | 适用 |
| --- | --- | --- |
| `gepa`（默认） | minibatch 反思生成假设、VISTA ε-greedy 选择、三级突变算子与逐样本帕累托前沿 | 通用，多指标权衡友好 |
| `auto` | 按任务特征自动路由（预算极紧 → p1；train 样本 ≥2 且主指标可自动判分 → miprov2；多指标 → gepa；默认 → protegi），未注册范式回退 gepa | 不确定时的无脑选择 |
| `protegi` / `miprov2` / `evoprompt` | 文本梯度 / 联合指令+示例搜索 / 进化提示词 | V5 并行合入中，合入前以 `--optimizer bogus` 的报错清单为准（当前可选 `auto, gepa`） |

**预期产出**：除基线评估的全部产物外，`runs/<run_id>/` 额外多出：

```
runs/<run_id>/
├── lineage.json        # 谱系：每轮候选的来源、操作、得分（增量写）
├── frontier.json       # 帕累托前沿快照
├── report.md           # 人读优化报告（最优提示词、轮次、假设列表）
└── evals/<seq>-<id>/   # 每轮候选的独立评估单元
```

`manifest.json` 记录 `optimizer`（最终范式）、`optimizer_requested`（请求值）、`optimizer_route_reason`（路由/降级原因）；显式指定 `gepa` 时 reason 为「显式指定 gepa（未走 auto 路由）」。

## 同任务多范式对比

对同一位置参数提示词分别跑不同范式，再用 `serve` 的 compare 页查看「范式」行：

```bash
promptopt run "对中文电商评论做情感三分类，只输出 positive、negative、neutral 三个英文小写标签之一" \
  --optimizer gepa --base-url … --model …
promptopt run "对中文电商评论做情感三分类，只输出 positive、negative、neutral 三个英文小写标签之一" \
  --optimizer protegi --base-url … --model …   # 合入后可用
promptopt serve
```

> 已知局限：两次零配置 run 的合成**相互独立**（非同一数据集），compare 页的 Δ 混杂范式与数据集双重差异；严格对比可在 `--interactive` 检查点人工对齐合成产物，或用手工三件套固定数据集（但手工模式无优化循环）。

## 产物结构（`runs/<run_id>/`）

```
runs/<run_id>/
├── manifest.json        # 运行元数据（任务、候选、数据集、配置、优化器路由）
├── events.jsonl         # 事件流，SSE 回放源
├── samples/<id>.json    # 逐样本 trace（渲染后 prompt、响应、得分、用量）
└── summary.json         # 运行摘要（状态、退出码、指标均值、用量）
```

退出码：`0` 成功；`1` 评估失败（任一样本重试后仍错）或用法错误；`2` 预算耗尽（存在因预算未派发的样本），优先于 `1`。

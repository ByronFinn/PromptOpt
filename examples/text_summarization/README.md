# text_summarization —— 中文资讯一句话摘要示例

PromptOpt 的 `f1` 指标示例：把中文资讯段落压缩为一句话摘要。摘要没有唯一标准答案，逐字比对（exact_match）会几乎全判 0，而 **f1** 对中文字符粒度的多重集做部分给分，是这类生成任务的合适主指标。本例同时演示 `--optimizer auto` 自动路由的用法。

## 文件说明

| 文件 | 说明 |
| --- | --- |
| `task.yaml` | 任务规范：`prompt_template`（含 `{input}` 占位符）、指标 `f1`，主指标 `f1` |
| `candidate.yaml` | 候选提示词：含 `{input}` 占位符的摘要指令，附带一句话长度约束 |
| `dataset.yaml` | 数据集：内联 4 条中文资讯样本（train 2 / dev 1 / test 1），`expected` 为参考摘要字符串 |

> 指标契约：字符串 `expected` 上的 f1 先分词再算多重集精确率/召回率——中文按**单字符**切分，英文与数字按连续片段切分。模型摘要与参考摘要有约八成字符重合即可拿到 0.7 左右的部分分；标点也参与比对，措辞与句读差异会小幅扣分。
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

**预期产出**：`runs/<run_id>/summary.json` 的 `metric_means.f1` 为基线字符级 F1 均值（如 `0.72`）；`samples/<id>.json` 逐条记录渲染后 prompt、模型摘要与得分，低分样本的 `diagnosis` 会说明重叠不足或输出非预期形态。

> 注意：手工三件套模式只跑**单轮基线评估**，不进入优化循环；此模式下显式传 `--optimizer` 会因用法错误直接退出 1。

## 优化：--optimizer auto（零配置模式）

优化循环运行在零配置模式下：用一个**位置参数提示词**替代三件套，合成管线自动生成任务规格与数据集（`--samples` 控制合成条数，默认 6），然后按 `--optimizer` 指定的范式迭代提示词。

```bash
promptopt run \
  "把中文资讯段落概括成一句话摘要（不超过 40 个字），保留主体、事件与关键结果" \
  --optimizer auto \
  --base-url http://192.168.56.39:59001/v1 \
  --model jiuwei-tcm \
  --api-key 1
```

`auto` 在合成管线产出保留集后按任务特征路由（规则详见 [docs/plugins.md](../../docs/plugins.md)）：

| 优先级 | 特征 | 请求范式 | 说明 |
| --- | --- | --- | --- |
| 1 | 预算极紧（`--budget-evals > 0` 且 `< 2×保留集大小`） | `p1` | 付不起的范式优先；p1 未注册时降级 `gepa` |
| 2 | 保留集 train 样本 ≥2 且主指标可自动判分（exact_match / f1 / json_validator） | `miprov2` | 联合搜索指令与 few-shot 示例 |
| 3 | 多指标（`len(metrics) > 1`） | `gepa` | 多指标帕累托权衡 |
| 4 | 默认 | `protegi` | 文本梯度 |

> 路由特征取自**合成产出的任务规格与保留集**（本目录的 `dataset.yaml` 不参与零配置 run）。摘要类提示词的合成规格通常为单指标 `f1`：合成保留集 train ≥2 时（f1 属可自动判分指标）命中 JointFewShot 档路由 `miprov2`，否则默认 `protegi`；实际以 manifest 记录为准。protegi/miprov2/evoprompt 由 V5 并行合入，合入前 `auto` 对未注册范式回退 `gepa` 并在 manifest 记录原因；显式范式的可选值以 `--optimizer bogus` 的报错清单为准（当前为 `auto, gepa`）。

**预期产出**：除基线评估的全部产物外，`runs/<run_id>/` 额外多出：

```
runs/<run_id>/
├── lineage.json        # 谱系：每轮候选的来源、操作、得分（增量写）
├── frontier.json       # 帕累托前沿快照
├── report.md           # 人读优化报告（最优提示词、轮次、假设列表）
└── evals/<seq>-<id>/   # 每轮候选的独立评估单元
```

`manifest.json` 记录 `optimizer`（最终范式）、`optimizer_requested`（请求值）、`optimizer_route_reason`（路由/降级原因）——`auto` run 用它复盘实际选中的范式。

## 同任务多范式对比

对同一位置参数提示词分别跑不同范式，再用 `serve` 的 compare 页查看「范式」行（标签经注册表解析，如 protegi 显示「ProTeGi 文本梯度」）：

```bash
promptopt run "把中文资讯段落概括成一句话摘要（不超过 40 个字），保留主体、事件与关键结果" \
  --optimizer gepa --base-url … --model …
promptopt run "把中文资讯段落概括成一句话摘要（不超过 40 个字），保留主体、事件与关键结果" \
  --optimizer auto --base-url … --model …
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

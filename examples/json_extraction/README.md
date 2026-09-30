# json_extraction —— 中医医疗 NER 抽取示例

PromptOpt v2 的最小示例：从中文中医医疗文本中抽取命名实体（证候、治法、方剂、中药），并以结构化 JSON 输出。适配 jiuwei-tcm 推理模型。

## 文件说明

| 文件 | 说明 |
| --- | --- |
| `task.yaml` | 任务规范：`prompt_template`（含 `{input}` 占位符）、指标 `json_validator` + `f1`，主指标 `f1` |
| `candidate.yaml` | 候选提示词：含 `{input}` 占位符的抽取指令，附带 JSON 输出契约 |
| `dataset.yaml` | 数据集：内联 4 条中文样本（train 2 / dev 1 / test 1），`expected` 为结构化 JSON 对象 |

> 提示词渲染使用字符串替换（`{input}` → 样本输入），而非模板引擎——prompt 中的 JSON 字面花括号不会被吞掉。

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

`--base-url` / `--model` / `--api-key` 亦可分别由环境变量 `PROMPTOPT_BASE_URL` / `PROMPTOPT_MODEL` / `PROMPTOPT_API_KEY` 提供，命令行优先。加 `--split test` 可只评估 test 切分；加 `--web` 在 `127.0.0.1:17700` 查看实时事件流。

> 注意：手工三件套模式只跑**单轮基线评估**，不进入优化循环；此模式下显式传 `--optimizer` 会因用法错误直接退出 1。

## 优化：--optimizer（零配置模式）

优化循环运行在零配置模式下：用一个**位置参数提示词**替代三件套（两者互斥），合成管线自动生成任务规格与数据集（`--samples` 控制合成条数，默认 6；`--probe-variants` 控制方差过滤探针，默认 2），然后按 `--optimizer` 指定的范式迭代提示词：

```bash
promptopt run \
  "从中文中医医疗文本中抽取命名实体（证候、治法、方剂、中药），以 JSON 输出" \
  --optimizer gepa \
  --base-url http://192.168.56.39:59001/v1 \
  --model jiuwei-tcm \
  --api-key 1
```

`--optimizer` 取值：注册范式名或 `auto`（默认 `gepa`）。范式机制与 auto 路由规则详见 [docs/plugins.md](../../docs/plugins.md)；可选范式以 `--optimizer bogus` 的报错清单为准（V5 三范式 protegi / miprov2 / evoprompt 合入前为 `auto, gepa`）。

路由特征取自**合成产出的任务规格与保留集**（本目录三件套不参与零配置 run）：抽取类提示词的合成规格通常声明 `json_validator` + `f1` 双指标，落入 auto 规则表的「多指标」档路由 `gepa` 做帕累托权衡；实际以 `manifest.json` 记录的 `optimizer` / `optimizer_requested` / `optimizer_route_reason` 为准，供复盘选中的范式与降级原因。也可显式指定其他范式对比。

**预期产出**：除基线评估的全部产物外，`runs/<run_id>/` 额外多出 `lineage.json`（谱系）、`frontier.json`（帕累托前沿）、`report.md`（人读优化报告，含最优提示词）与 `evals/<seq>-<id>/`（每轮候选的独立评估单元）。同任务多范式对比可对同一位置参数提示词分别跑不同范式后 `promptopt serve`，在 compare 页查看「范式」行——注意两次零配置 run 的合成相互独立（非同一数据集），Δ 混杂范式与数据集双重差异。

## 产物结构（`runs/<run_id>/`）

```
runs/<run_id>/
├── manifest.json        # 运行元数据（任务、候选、数据集、配置）
├── events.jsonl         # 事件流，SSE 回放源
├── samples/<id>.json    # 逐样本 trace（渲染后 prompt、响应、得分、用量）
└── summary.json         # 运行摘要（状态、退出码、指标均值、用量）
```

退出码：`0` 成功；`1` 评估失败（任一样本重试后仍错）或用法错误；`2` 预算耗尽（存在因预算未派发的样本），优先于 `1`。

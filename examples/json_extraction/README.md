# json_extraction —— 中医医疗 NER 抽取示例

PromptOpt v2 的最小示例：从中文中医医疗文本中抽取命名实体（证候、治法、方剂、中药），并以结构化 JSON 输出。适配 jiuwei-tcm 推理模型。

## 文件说明

| 文件 | 说明 |
| --- | --- |
| `task.yaml` | 任务规范：`prompt_template`（含 `{input}` 占位符）、指标 `json_validator` + `f1`，主指标 `f1` |
| `candidate.yaml` | 候选提示词：含 `{input}` 占位符的抽取指令，附带 JSON 输出契约 |
| `dataset.yaml` | 数据集：内联 3 条中文样本（train/dev/test 各 1 条），`expected` 为结构化 JSON 对象 |

> 提示词渲染使用字符串替换（`{input}` → 样本输入），而非模板引擎——prompt 中的 JSON 字面花括号不会被吞掉。

## 运行

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

## 产物结构（`runs/<run_id>/`）

```
runs/<run_id>/
├── manifest.json        # 运行元数据（任务、候选、数据集、配置）
├── events.jsonl         # 事件流，SSE 回放源
├── samples/<id>.json    # 逐样本 trace（渲染后 prompt、响应、得分、用量）
└── summary.json         # 运行摘要（状态、退出码、指标均值、用量）
```

退出码：`0` 成功；`1` 评估失败（任一样本重试后仍错）或用法错误；`2` 预算耗尽（存在因预算未派发的样本），优先于 `1`。

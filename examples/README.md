# PromptOpt 示例集

三个覆盖不同指标与 `--optimizer` 用法的完整示例。每个目录都含 `task.yaml` / `candidate.yaml` / `dataset.yaml` 三件套与中文 README。

| 示例 | 任务 | 指标 | `expected` 形态 | README 重点用法 |
| --- | --- | --- | --- | --- |
| [json_extraction](json_extraction/) | 中医医疗命名实体抽取 | `json_validator` + `f1`（主 `f1`）+ `tcm_f1_entity`（分维度宏平均，指标注册表试点） | JSON 对象 | 双指标多约束；auto 下路由 gepa 做帕累托权衡 |
| [sentiment_classification](sentiment_classification/) | 电商评论情感三分类 | `exact_match` | 标签字符串 | 显式 `--optimizer gepa`；标签类任务的严格判分 |
| [text_summarization](text_summarization/) | 中文资讯一句话摘要 | `f1` | 参考摘要字符串 | `--optimizer auto` 自动路由；部分给分指标 |

## 两种运行模式

- **手工三件套模式**：`promptopt run --task … --candidate … --dataset …`，对给定候选做单轮基线评估，**不进入优化循环**（显式传 `--optimizer` 是用法错误）。
- **零配置模式**：`promptopt run "<任务提示词>" --optimizer <范式>`，合成管线自动生成任务规格与数据集后按范式迭代提示词；范式与 auto 路由规则见 [docs/plugins.md](../docs/plugins.md)。

通用约定：提示词渲染是字符串替换（`{input}` → 样本输入）；`--base-url` / `--model` / `--api-key` 可由 `PROMPTOPT_BASE_URL` / `PROMPTOPT_MODEL` / `PROMPTOPT_API_KEY` 环境变量提供；退出码 `0` 成功、`1` 评估失败或用法错误、`2` 预算耗尽（优先于 `1`）。

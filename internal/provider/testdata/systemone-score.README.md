# systemone-score.json — /v1/systemone 实测响应 golden fixture

来源：P7 实施会话在 tcmsp-30 以只读 curl 对本机 systemd 服务
`http://127.0.0.1:11434/v1/systemone`（ollama 0.35.0，model `tev1:0.8b`）的两次
实测，两次响应逐字节一致。请求为 3 级 score 问题：
`criteria: ["完全错误", "部分正确", "完全正确"]`。

固化精度（诚实口径）：

- **逐字精确**：`score=1.386988025801118`（16 位全精度）、`usage`
  （123/1）、`legend`（{0,1,2} → 三级描述）、`type`、`model`。
- **截断值**：`probabilities`（0.1339/0.3450/0.5209）与
  `confidence`（0.111453）在会话记录中仅保留前缀数字，本 fixture 原样
  采用截断值、**未补造任何缺失数位**。它们只用于钉 shape 契约；
  归一化断言只依赖 `score` 字段（1.386988025801118 / (3−1) =
  0.693494012900559，见 internal/eval/judge_decision_test.go）。
- **推断值**：`selected: 2` 为 argmax 推断（p2=0.5209 最大），非逐字
  记录；解码端不消费该字段。

远端首跑的语义真值验证由 `scripts/smoke-decision.sh` 承担。

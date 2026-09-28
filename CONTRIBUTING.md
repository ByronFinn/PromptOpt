# 贡献指南

感谢关注 PromptOpt！欢迎通过 Issue 与 Pull Request 参与建设。

## 如何参与

- **报告问题 / 提建议**：在 [GitHub Issues](https://github.com/ByronFinn/PromptOpt/issues) 提交，新 Issue 默认待分流（needs-triage），请尽量附复现步骤与版本信息（`promptopt version`）。
- **认领任务**：带 `ready-for-agent` / `ready-for-human` 标签的 Issue 可直接认领，在 Issue 下留言即可。
- **提交代码**：fork → 建分支 → 提交 PR 到 `main`。

## 开发流程

构建、测试命令与代码约定统一见 [AGENTS.md](AGENTS.md)（Go 1.26，遵守 [JetBrains go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)）。

PR 合入前请确保本地通过：

```bash
go vet ./...
go test -race ./...
go build ./...
```

CI 会在 push / PR 时执行同样的检查（[ci.yml](.github/workflows/ci.yml)）。

## 文档与语言

- 面向人类的文档（PRD、ADR、Issue、评论等）一律使用中文，见 [docs/agents/language.md](docs/agents/language.md)。
- 设计决策记录于 [docs/adr/](docs/adr/)，产品路线见 [ROADMAP.md](ROADMAP.md)。

## License

提交即表示同意以 MIT 许可发布（与项目主许可一致）。

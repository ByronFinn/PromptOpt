# 0003 - Go 同仓库原地重写，Python 归档于 tag

Date: 2026-09-28

## Status

Accepted

## Context

PromptOpt v1 是 Python 实现（src/promptopt，29 文件，M1 部分完成）。v2 转向多范式平台需要：单二进制分发（开源工具 + go:embed 内嵌 Web UI）、高并发评估（goroutine）、长期可维护性。同时远程 45 个 issue 均按旧 Python 路线拆解。可选路径：新仓库重来 / 双栈并存 / 同仓库原地重写。

## Decision

**同仓库原地重写**：仓库根转 Go module（go.mod + cmd/promptopt + internal/），删除 `src/`、`tests/`、`pyproject.toml` 等 Python 资产并打 tag `v0.1-python`（指向 v1 最后状态 commit 9f9139e）留档；沿用 dev 分支 + PR 流程。工程规范遵守 JetBrains go-modern-guidelines，CI 跑 go vet / staticcheck / modernize / go test。旧 issue 批量关闭，能力按新 roadmap V0–V6 重拆。

## Consequences

### Positive

* 保留 issue/PR 历史关联与仓库资产（star、watcher）
* git 历史 + tag 即回滚锚点，Python 实现随时可查
* 单二进制 + 纯 Go SQLite 驱动（免 cgo）实现零依赖分发

### Negative

* 放弃 Python 生态：gepa 参考实现（Python）不能直接复用，litellm 的多 provider 覆盖需在 Go 侧自建（好在 OpenAI-compat 协议覆盖了绝大多数 provider）
* 已合并的 v1 代码（数据加载器/解析器）被丢弃，接受沉没成本（量小，约 3 个 PR）

## Alternatives Considered

* **新仓库 promptopt-v2**：干净但丢失 issue/PR 历史关联和 star 等仓库资产
* **双栈并存**：Python v1 继续维护 + Go v2 并行开发——个人项目维护成本翻倍，不现实

## References

* JetBrains go-modern-guidelines (github.com/JetBrains/go-modern-guidelines)
* PRD-0000（docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md）

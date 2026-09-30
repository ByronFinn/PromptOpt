# PromptOpt v2 V0+V1 纵切实施报告

> **日期**: 2026-09-28 | **范围**: commit `eced33e`（Go 纵切）+ `f990aaf`（Python 归档），即 `HEAD~2..HEAD`
> **对照文档**: [PRD-0000](docs/prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md) 与 [ROADMAP.md](ROADMAP.md) V0/V1 里程碑

## 1. 交付概述

本次交付完成 PRD-0000 的 **V0（地基）与 V1（评估闭环）** 两个里程碑的纵切实现：Go 同仓库原地重写落地（单二进制、标准库优先、go:embed Web 看板），Python v1 实现删除并打 tag `v0.1-python` 留档（ADR 0003）。`git diff HEAD~2 --stat` 合计 **77 个文件，+5185 / −4119 行**（新增 Go 代码约 5000 行，删除 Python 资产约 4100 行）。

提交清单（`git log --stat -3` 核实）：

| Commit | 内容 |
|---|---|
| `eced33e` | feat: Go 纵切——脚手架/Provider/评估引擎/Web 骨架（33 文件，+5045/−174） |
| `f990aaf` | chore: 归档 v1 Python 实现，文档切换到 Go 工作流（44 文件，+140/−3945） |

## 2. 交付内容对照（V0 / V1）

### V0：地基（ROADMAP.md:68-80）

| 条目 | 状态 | 证据 |
|---|---|---|
| Go module + `cmd/promptopt` + `internal/` 布局 + config 加载 | ✅ | `go.mod`（Go 1.26，唯一依赖 `gopkg.in/yaml.v3`）；`cmd/promptopt/main.go`（run/serve/version 三子命令，main.go:47-51）；`internal/config/config.go`（flag 默认值 + `PROMPTOPT_*` 环境变量） |
| Provider 接口 + OpenAI-compatible 实现（重试/usage 统计） | ✅ | `internal/provider/provider.go`（接口）；`internal/provider/openai.go`（246 行：重试、usage 分角色计量、reasoning 容忍，commit message 记载） |
| Provider：Anthropic 原生实现 | ❌ 未含 | `internal/provider/` 下仅有 `openai.go`，无 anthropic 实现 |
| SQLite（纯 Go 驱动）+ artifact 读写骨架 | ❌ 未含 | 无 `store/` 包；持久化目前为 artifact 文件（见 V1） |
| CI：vet / test / build | ✅ | `.github/workflows/ci.yml:26-33`：`go vet ./...` → `go test -race ./...` → `go build ./...` |
| CI：staticcheck / modernize | ❌ 未接入 | ci.yml 中无这两项（全文 34 行已核实） |
| 删除 Python 资产 + tag `v0.1-python` | ✅ | commit `f990aaf` 删除 `src/` `tests/` `pyproject.toml` 等 44 文件；`git tag -l` 确认 `v0.1-python` 存在 |
| AGENTS.md 按 Go 工作流重写 | ✅ | commit `f990aaf`（AGENTS.md −116 行改写） |
| 开源门面：README 重写 + badge + CONTRIBUTING.md | ✅ | README.md:3-6 含 CI / Go Reference / Go Report Card / License MIT 四个 badge；CONTRIBUTING.md 新增 32 行 |
| 开源门面：LICENSE（MIT）文件 | ❌ 缺失 | `find -maxdepth 2 -iname "LICENSE*"` 无结果；README badge 链接指向 `LICENSE` 为死链 |

### V1：评估闭环（ROADMAP.md:82-93）

| 条目 | 状态 | 证据 |
|---|---|---|
| core 模型（Task/Candidate/Dataset/RunResult/SampleTrace/Event） | ✅ | `internal/core/types.go`（111 行）；`internal/core/load.go`（214 行，YAML 加载 + split 过滤） |
| eval 引擎：并行 + trace 捕获 + 指标 | ✅ | `internal/eval/engine.go`（348 行，worker 池并行）；指标 `exact_match` / `f1` / `json_validator`（`internal/eval/metrics.go`，234 行） |
| 指标：LLM-judge | ❌ 未含 | metrics.go 中仅有上述三种指标 |
| 评估器返回 `(score, diagnosis)`（ASI 雏形） | ✅ | `internal/eval/metrics.go:11-15`：`MetricResult` 含 `Diagnosis` 字段，不完美得分附带文本诊断 |
| 预算阀门 + 软停 | ✅ | `internal/eval/budget.go`（81 行）；commit message 记载"预算阀门软停" |
| run 持久化 + `run --dataset` 最小闭环 | ✅ | `cmd/promptopt/run.go:77-85`：`runs/<id>/` 目录 + `events.jsonl` 事件流落盘；run.go:191 默认输出 `runs/`；`serve` 子命令只读浏览历史 run |
| Web 看板骨架页（go:embed + SSE） | ✅ | `internal/web/server.go`（522 行）+ go:embed 模板（index.html / run.html 各 131/127 行）；SSE 用原生 `EventSource`（index.html:94、run.html:107）；运行时行为经 5.1 节实测（HTTP 200 + 实时事件流） |
| Web 看板：htmx | ⚠️ 偏差 | 模板中无 htmx（grep 无命中），SSE 以原生 JS `EventSource` 实现；功能等价，与 PRD"前端维持 htmx"决议（PRD-0000:142）存在技术栈偏差 |
| 首个 demo GIF 进 README | ❌ 未录制 | README 中无 GIF 引用（grep `gif` 无命中） |
| 退出码契约（0 成功 / 1 失败 / 2 预算耗尽） | ✅ | run.go:166-169 返回 `res.ExitCode`；AGENTS.md"常见陷阱"第 3 条记载契约 |

### PRD-0000 验收标准中本阶段相关项

- ✅ "Python 源码从工作区移除，`v0.1-python` tag 存在"（PRD-0000:68）
- ✅ "V1 结束时浏览器可打开看板骨架页（实时事件流可见）"（PRD-0000:70）——由 5.1 节的 `--web` 实测支撑（看板 HTTP 200 + SSE 实时事件流）。注意：tcmsp-30 冒烟**不能**作为该项证据——其唯一产物是 stdout JSON 摘要，该格式仅在 `--headless` 下输出（[run.go:161-166](cmd/promptopt/run.go)），且 `--web` 与 `--headless` 互斥（run.go:215-217），故该次冒烟实为 headless 模式
- ⚠️ "`go vet` / `staticcheck` / `modernize` / `go test ./...` 全绿"（PRD-0000:67）——vet/test/build 本地全绿（见第 4 节），staticcheck/modernize 未接入 CI 且本次未运行
- ❌ "仓库具备开源门面：badge / 快速开始 / 贡献指南 / demo GIF 齐备"（PRD-0000:71）——demo GIF 与 LICENSE 文件缺失

## 3. 架构与文件清单

`git diff HEAD~2 --stat` 核实的完整文件清单（新增部分，行数来自 diff stat）：

```text
cmd/promptopt/
├── main.go                 (123)  # 入口：run / serve / version 分发
└── run.go                  (395)  # run 子命令：加载→评估→落盘→退出码
                           run_test.go (246)
internal/
├── config/config.go          (53) # flag 默认值 + PROMPTOPT_* 环境变量
│                config_test.go (68)
├── core/
│   ├── types.go             (111) # Task/Candidate/Dataset/RunResult/SampleTrace/Event
│   ├── load.go              (214) # YAML 加载、split 过滤
│   └── load_test.go         (366)
├── provider/
│   ├── provider.go           (71) # Provider 接口 + usage 类型
│   ├── openai.go            (246) # OpenAI 兼容实现：重试/usage 分角色/reasoning 容忍
│   └── openai_test.go       (299) # httptest mock provider
├── eval/
│   ├── engine.go            (348) # 并行评估：worker 池、事件流、预算软停
│   ├── budget.go             (81) # Budget 阀门（token/次数）
│   ├── metrics.go           (234) # exact_match / f1 / json_validator + Diagnosis
│   └── eval_test.go         (613)
└── web/
    ├── server.go            (522) # net/http：run 列表/详情 + SSE；go:embed
    ├── server_test.go       (572)
    └── templates/            # index.html (131) / run.html (127)
examples/json_extraction/     # v2 示例规范：task.yaml / candidate.yaml / dataset.yaml
.github/workflows/ci.yml      # vet + test -race + build
CONTRIBUTING.md / README.md / AGENTS.md / .zcodeignore / .gitignore
```

测试代码合计 **2164 行**（`wc -l` 实测：run_test 246 + config_test 68 + load_test 366 + openai_test 299 + eval_test 613 + server_test 572），占全部 Go 代码约 47%（非测试源码 `wc -l` 实测 2398 行 + 测试 2164 行 = 4562 行）。

## 4. 测试与门禁结果（本次实际执行）

在仓库根目录、当前 HEAD（`f990aaf`，工作区 clean）执行：

| 命令 | 结果 |
|---|---|
| `go build ./...` | ✅ 通过（无输出，echo "BUILD OK" 确认） |
| `go vet ./...` | ✅ 通过（无输出，echo "VET OK" 确认） |
| `go test -race ./...` | ✅ 6 个包全部 `ok`（cmd/promptopt、internal/config、internal/core、internal/eval、internal/provider、internal/web；部分显示 `(cached)`，Go 测试缓存按代码内容失效，等价于当前代码状态通过） |
| `go test ./... -v` | ✅ `--- PASS` 计数 62，与 `grep -E "^func Test" *_test.go` 统计的 62 个测试函数一致 |
| staticcheck / modernize | ⏸ **未运行**——未接入 CI，本次也未在本机执行，如实报告为 not run |

CI（`.github/workflows/ci.yml`）与上述本地命令一致（vet / test -race / build），本地全绿意味着 CI 预期通过；CI 远端实际运行结果本次未查询（not run）。

## 5. tcmsp-30 冒烟结论：成功

对 tcm_ner_extraction 任务（tcmsp-30，中医 NER 抽取场景）以 dev split 冒烟运行，**评估闭环端到端跑通，退出码 0，两项指标满分**。输出摘要（run 摘要 JSON 原文，由任务交办方提供，未在本会话重跑）：

```json
{"run_id":"20260928-103601-9f9ebd67","task_name":"tcm_ner_extraction","candidate_id":"baseline","dataset_name":"tcm_ner","split":"dev","status":"completed","exit_code":0,"total_samples":1,"evaluated_samples":1,"failed_samples":null,"undispatched":0,"usage_by_role":{"executor":{"prompt_tokens":181,"completion_tokens":767}},"metric_means":{"f1":1,"json_validator":1},"started_at":"2026-09-28T10:36:01.994723786Z","finished_at":"2026-09-28T10:36:25.268424017Z"}
```

要点解读：

- `status: completed`、`exit_code: 0`：评估成功，无失败样本（`failed_samples: null`）、无因预算未派发样本（`undispatched: 0`）
- `metric_means: {"f1": 1, "json_validator": 1}`：实体级 F1 与 JSON 合法性均满分（1 样本）
- `usage_by_role` 仅 `executor` 一角色（prompt 181 / completion 767 tokens）：单候选评估不触发优化侧调用，分角色计量符合 PRD 预算口径（PRD-0000:48）
- 耗时约 23.3 秒（10:36:01 → 10:36:25）

说明：

- tcmsp-30 冒烟的任务/数据集与仓库示例 `examples/json_extraction/` 同源（task 名 `tcm_ner_extraction`、dataset 名 `tcm_ner` 一致），但那次运行的具体数据文件不在仓库内（`grep -ri tcmsp` 仅命中 `.zcode/` 工作流草稿，未提交）；上述 JSON 为交办材料直接引用，未在本会话重跑
- 该次冒烟为 **headless 模式**：其唯一产物是 stdout JSON 运行摘要，此格式仅在 `--headless` 下输出（[run.go:161-166](cmd/promptopt/run.go)），而 `--web` 与 `--headless` 互斥（run.go:215-217）——故它验证的是无头 JSON 输出契约，**不**覆盖看板/SSE 可见性，后者由 5.1 节实测覆盖

### 5.1 补充实测：`--web` 看板与 SSE 可见性（2026-09-28 报告修订时执行）

以本地 mock OpenAI 端点（python3 标准库实现 `/v1/chat/completions`，每请求延迟 5s、返回固定实体 JSON）驱动仓库示例任务，完整命令（在仓库根目录、`go build -o promptopt ./cmd/promptopt` 产物）：

```bash
./promptopt run --task examples/json_extraction/task.yaml \
  --candidate examples/json_extraction/candidate.yaml \
  --dataset examples/json_extraction/dataset.yaml \
  --web --addr 127.0.0.1:18080 --base-url http://127.0.0.1:18081/v1 \
  --model mock-model --out /tmp/promptopt-webcheck/runs
```

结果（本次实际执行）：

1. **看板骨架页可打开**：`curl -s http://127.0.0.1:18080/` → `HTTP 200 text/html; charset=utf-8`，HTML 含 `<title>PromptOpt 看板</title>`（index.html 模板渲染）
2. **SSE 实时事件流可见**：run 进行中 `curl -sN http://127.0.0.1:18080/events` 依次收到（摘录，省略 prompt/response 长字段）：

   ```text
   data: {"type":"sample_done",...,"sample_id":"test-001","scores":{"f1":0.357...,"json_validator":1},"latency_ms":10020}
   data: {"type":"sample_done",...,"sample_id":"dev-001","scores":{"f1":1,"json_validator":1},"latency_ms":15031}
   data: {"type":"run_done",...,"status":"completed","metric_means":{"f1":0.5357...,"json_validator":1},"usage_by_role":{"executor":{"prompt_tokens":300,"completion_tokens":150}}}
   ```

   事件携带完整 prompt/response/scores/usage/latency。train-001 的 `sample_done` 在 curl 连接建立前已广播、未捕获——恰好证明该端点推送的是**实时流**而非回放（历史回放走 `events.jsonl`，AGENTS.md 记载）
3. **run 正常收尾**：stderr 人类摘要 `run 20260928-104419-343b9428: completed (exit 0)`，3 evaluated / 0 failed / 0 undispatched，artifacts 落盘 `/tmp/promptopt-webcheck/runs/20260928-104419-343b9428`

## 6. 已知边界与后续

**本阶段未交付（相对 PRD/ROADMAP 的缺口）：**

1. **SQLite 持久化未含**（V0 项，ROADMAP.md:74）：run 结果目前仅 artifact 文件（`runs/<id>/` + `events.jsonl`），`store/` 包未建
2. **staticcheck / modernize 未接入 CI**（PRD 工程规范，PRD-0000:56）：ci.yml 仅 vet/test/build，go-modern-guidelines 的 lint 门禁缺位
3. **LICENSE 文件缺失**：README 的 MIT badge 指向不存在 的文件，为死链
4. **Anthropic 原生 Provider 未含**（V0 项，ROADMAP.md:73）：仅 OpenAI 兼容实现
5. **LLM-judge 指标未含**（V1 项，ROADMAP.md:87）：三种确定性指标先行
6. **demo GIF 未录制**（V1 项，ROADMAP.md:91）
7. **前端为原生 JS 而非 htmx**：SSE 功能等价，但与 PRD 决议（PRD-0000:142）记录的技术栈不一致，后续引入 htmx 或修订决议二选一

**V2–V6 待实施**（ROADMAP.md:95 起）：V2 Harness Builder（AI 合成评测集 + p¹ 方差过滤 + 检查点）、V3 GEPA 引擎（Reflector/Mutator/Frontier/Budget/VistaGuard）、V4 Web UI 完整化、V5 多范式扩展、V6 工程化（goreleaser/回归门禁）。

## 7. 下一步建议

1. **补齐开源门面硬缺口**：添加 LICENSE（MIT）文件——当前 badge 死链直接影响开源发布；录制首个 demo GIF（`examples/json_extraction` + `--web` 实时事件流是现成素材）
2. **接入 staticcheck + modernize 到 CI**：PRD 工程规范明确要求，go-modern-guidelines 的 modern习语检查依赖它落地；建议作为独立小 PR
3. **Anthropic Provider + LLM-judge**：两者都是 V3 反思循环的前置（优化侧调用不止 OpenAI 兼容端点；diagnosis 质量需要 judge 类指标），可在 V2 期间并行补
4. **SQLite store**：V2 检查点干预与 V3 lineage 查询都以结构化存储为前提，建议 V2 开工前落地 `store/` 包骨架（modernc.org/sqlite 纯 Go 驱动，保持免 cgo 单二进制）
5. **htmx 决议对齐**：在 V2 补合成集审核页之前决定——引入 htmx（交互密度上升，符合决议）或修订 PRD 改为原生 JS（当前骨架已可用），避免 V4 完整化时返工
6. **冒烟扩展**：tcmsp-30 仅 1 样本 dev split，建议扩到多样本 + test split；预算耗尽（退出码 2）路径仍未验证——tcmsp-30（headless）与 5.1 节实测（--web）均为无预算限制的成功路径

---

*报告内所有结论均基于本会话实际读取的文件、执行的命令（第 4、5.1 节）或交办材料（第 5 节 JSON）；未验证项已逐条标注。*

---

# V5+V6 实施报告（2026-09-30 追加）

> **日期**: 2026-09-30 | **范围**: V5 多范式扩展（commit `ff51f5c`，分支 `feat/v5-multi-paradigm`，52 文件 +7283/−604，已推送）+ V6 工程化与回归门禁（commit `6e50140`，分支 `feat/v6-engineering`，21 文件，已推送）
> **对照文档**: [ROADMAP.md](ROADMAP.md) V5/V6 里程碑、[docs/plugins.md](docs/plugins.md)、[docs/release.md](docs/release.md)、[CHANGELOG.md](CHANGELOG.md)
> **报告体系说明**: V2/V3 报告见 [docs/reports/](docs/reports/)；V4 以提交序列与 README/AGENTS 文档收尾（`35daf7a`）承载。本章节按既有风格（交付清单 / 验证证据 / 已知边界）追加，编号接续上文。

## 8. V5 交付清单（commit `ff51f5c`）

| 条目 | 状态 | 证据 |
|---|---|---|
| Optimizer 插件接口固化（注册、能力声明、路由元数据） | ✅ | `internal/optimizers/optimizers.go`：`Factory` / `Capabilities` / `Descriptor` / `Registry`（Register 重名/空名/nil panic、`Get` / `Names` 排序 / `Build` 未知名报错列可选）；`Request.Opts map[string]string` + `req.Opt("<paradigm>.<key>")`（internal/engine/optimizer.go）；ADR 0002 的"接口留在 engine"落地 |
| engine 共享骨架公开化 + Loop 脚手架 | ✅ | `internal/engine/loop.go:48`（`NewLoop`：校验 + baseline 播种，Best 永不为空）、`loop.go:196`（`Admit`）、`calls.go:53`（`NewAdvisor`）、`candidate.go:43`（`ProduceCandidate` = Call + Defend + 质量门）；GEPA 重构为仅保留决策（rng / VISTA 梯子 / 算子阶梯），golden 测试护栏全绿 |
| 范式路由器 | ✅ | `internal/optimizers/router.go:61-90`：五级有序规则（Pipeline→textgrad、TightBudget→p1、JointFewShot→miprov2、MultiConstraint→gepa、默认→protegi）+ 降级链 `degradeTo{textgrad→protegi, p1→gepa}` + 未注册范式可用性回退 gepa；决策三字段落 manifest（run.go:800-802 `optimizer` / `optimizer_requested` / `optimizer_route_reason`） |
| ProTeGi 风格（文本梯度定向纠偏） | ⚠️ 实现✅ / 接线缺一行 | `internal/optimizers/protegi/`（protegi.go / bandit.go / gradient.go + 测试）：UCB1 折臂 bandit + 文本梯度 + 沿梯度改写。**builtin 注册行待补**（builtin.go:37-47 仍为注释示例）——`--optimizer protegi` 在 flag 校验被拒（run.go:635-637），auto 默认路由回退 gepa 并在 manifest 记录原因 |
| MIPROv2 风格（指令+Few-shot 联合搜索） | ✅ | `internal/optimizers/miprov2/`（miprov2.go / demos.go / prompts.go + 测试）：一次提议 I=4 指令变体 + train 样本 demo 子集联合搜索，去泄漏 minibatch 打分→胜者全量准入；等价口径（seeded 联合采样 + 单级 promotion 替代 TPE 逐半淘汰）已在包 doc 记档；已注册（builtin.go:48-59） |
| EvoPrompt 风格（GA/DE 群体搜索） | ✅ | `internal/optimizers/evoprompt/`（evoprompt.go / operators.go + 测试）：P=6 种群、锦标赛交叉/变异/差分变异（`--evo-variant ga|de`）、精英保留；已注册（builtin.go:60-71），Route 不指向它、仅显式可达 |
| CLI 接线 | ✅ | `--optimizer`（默认 gepa，config.go:46）/ `--provider` / `--evo-variant` 三 flag（run.go:594-596）；合法集 = builtin.Names() ∪ {auto} 前置校验（run.go:628-637）；`--optimizer` 仅零配置模式生效（run.go:663-664）；路由特征派生 `routeFeatures`（run.go:389-405） |
| Anthropic 原生 Provider | ⚠️ 实现✅ / CLI 桩 | `internal/provider/anthropic.go`（原生 `/v1/messages` + `x-api-key` + `anthropic-version`、system 抽取、thinking block→ReasoningContent、共享重试核心 `retry.go`）+ anthropic_test.go；**CLI `--provider anthropic` 仍为拒绝桩**（cmd/promptopt/provider.go:18-22），接线待合入一行 |
| Web 范式接线 | ✅ | compare 概览表"范式"行经注册表解析 Label（未注册回退原名）；run.html live 事件 default 分支渲染 `[paradigm] type：stage`（范式专属事件可见） |
| 插件开发文档 + examples 体系 | ✅ | [docs/plugins.md](docs/plugins.md)：接口与助手签名表、注册表字段、路由规则与升级路径、in-repo 接入三步、marker fake LLM 测试规范、事件契约、预算与产物契约；examples 扩至 3 个（json_extraction / sentiment_classification / text_summarization + examples/README.md 索引），全部含 ≥4 样本 expected 与中文 README |

## 9. V6 交付清单（commit `6e50140`）

| 条目 | 状态 | 证据 |
|---|---|---|
| 退出码规范（0/1/2/3） | ✅ | `cmd/promptopt/main.go:23-28`：`exitRegression = 3`，优先级 `2 > 1 > 3`；verify.go:60-66 doc comment 声明四态语义 |
| `promptopt verify` 锚点验证 | ✅ | `cmd/promptopt/verify.go`：`--anchor`（≥3 条真实样本 dataset YAML，ADR 0001，写 anchor-dataset.json 留痕，verify.go:136-138）；无锚点降级为按原 run spec 独立重合成的同契约保留集，报告强制标注"结论未经真实数据验证"（verify.go:600）；交付=baseline 时零 LLM 调用跳过（verify.go:111-115）；产物落 `runs/<id>/verify/<时间戳>/` |
| 回归判定 + 约束检查 | ✅ | 主指标均值退化阈值 `--max-regression`（默认 0.05，verify.go:43）；`json_validator` 任务交付侧 JSON 合法率 100% 硬检查；`--max-avg-tokens` / `--max-avg-latency-ms` 成本延迟上限（口径含 judge 开销，verify.go:198-199）；连接参数 flag > `PROMPTOPT_*` env > run manifest 回退 |
| `promptopt rollback` | ✅ | `cmd/promptopt/rollback.go`：缺省回退上一不同采纳、兜底 baseline（旧工件缺 Prompt 时回退 synth spec 的 prompt_template），旧采纳追加 `adopted-history.jsonl`，`--to` 指定目标、`--emit` 导出 candidate.yaml |
| `promptopt replay` | ✅ | `cmd/promptopt/replay.go`：按时间合并 events.jsonl 与四类调用留痕（baseline / opt-eval / optimizer / synthesis，replay.go:20-26）为审计时间线 + 覆盖率摘要；`--full` 展开全文、`--headless` JSONL |
| `llm_judge` 指标 | ✅ | `internal/eval/judge.go:19`（`MetricLLMJudge`）：内置中文 rubric 0~1 打分 + ≤80 字诊断入 trace；Usage 口径改为样本总评估开销、judge 延迟以 `judge_ms` 单列 |
| GitHub Actions 集成模板 | ✅ | `.github/workflows/optimize.yml`：PR 触发零配置优化 → verify 门禁（退出码 3 = PR 检查失败），缺 secrets 自动跳过，指标对照表渲染 PR 摘要页，产物上传 |
| CI staticcheck 门禁 | ✅ | `.github/workflows/ci.yml`：vet / staticcheck（钉版 2026.2.1）/ modernize / test -race / build 五道齐备——闭合本报告第 6.2 节缺口 |
| Release 规范 | ✅ | `.github/workflows/release.yml`（v* tag 触发，goreleaser 钉 v2.13.0）+ `.goreleaser.yaml`（darwin/linux × amd64/arm64，CGO_ENABLED=0，tar.gz + checksums，changelog 禁用自动生成）+ `CHANGELOG.md`（Keep a Changelog 1.1.0，人工维护为权威）+ `LICENSE`（MIT，闭合第 6.3 节死链缺口）+ `docs/release.md`（发布与门禁 runbook） |
| 锚点用法示例 | ✅ | `examples/json_extraction/anchor.yaml`：3 条人工编写样本 |

## 10. 验证证据（tcmsp-30 门禁）

**本会话实际执行（2026-09-30）**：在 tcmsp-30（go1.26.5 linux/amd64，staticcheck 于 `~/go/bin`）独立沙盒 `~/PromptOpt-w-docsync`（rsync 同步本仓库 `6e50140` 提交树，参数 `--exclude .git --exclude runs --exclude synth --exclude dist --exclude .zcode --exclude .DS_Store`）依次执行五道门禁，**全部通过**：

| 命令 | 结果 |
|---|---|
| `go vet ./...` | ✅ 无输出 |
| `staticcheck ./...` | ✅ 无输出 |
| `go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -test ./...`（过滤 `go: downloading`） | ✅ 无诊断输出 |
| `go test -race ./...` | ✅ 12 个包全部 `ok`（cmd/promptopt、internal/config、internal/core、internal/engine、internal/eval、internal/harness、internal/optimizers、internal/optimizers/{evoprompt,miprov2,protegi}、internal/provider、internal/web；builtin 无测试文件） |
| `go build ./...` | ✅ 无输出 |

**佐证**：V5/V6 实现与提交代理按分工未执行 go 命令，交付材料记载门禁由工作流脚本在字节一致的树上跑绿；本会话在提交树上复跑结果一致。CI 远端（GitHub Actions）实际运行结果本次未查询（not run）；`optimize.yml` 的 PR 优化对比依赖仓库 secrets 配置，未在真实 PR 上演练（not run）。

## 11. 已知边界与未覆盖项

1. **protegi 注册缺口（一行式接线待补）**：`internal/optimizers/builtin/builtin.go:37-47` 的注册行仍为注释示例——`--optimizer protegi` 在 flag 校验被拒（可选值为 `auto, gepa, miprov2, evoprompt`）；auto 默认路由（请求 protegi）经可用性回退落 gepa 并在 manifest 记录原因。docs/plugins.md 头注已按此更新。
2. **`--provider anthropic` CLI 拒绝桩**：`internal/provider/anthropic.go` 及测试已落地，`cmd/promptopt/provider.go:18-22` 的拒绝文案待替换为 `provider.NewAnthropic(baseURL, apiKey, provider.AnthropicConfig{})` 一行。
3. **demo GIF 未录制**：环境受限待录（README 已留占位注释；PRD-0000 验收该项相应保持未勾）。
4. **SQLite 未引入（决议修订）**：artifact 文件承担全部持久化；PRD-0000 已按"难逆转条件不满足"记 PRD 级决议修订（2026-09-30），非 ADR。
5. **harness 合成无 seed**：多次零配置合成相互独立，严格 A/B 对比需 `--interactive` 检查点或手工三件套（examples/README.md 已如实标注对比局限）。
6. **TightBudget 路由边界**：走完合成的零配置 run 探针 + baseline 至少消耗 `2×kept` 次评估，故 p1→gepa 路由记录只可能出现在预算耗尽（跳过优化循环）的 run 上（docs/plugins.md §3 已记载）。

---

*本章结论基于 2026-09-30 会话实际读取的仓库文件（提交 `6e50140`）、tcmsp-30 沙盒门禁执行（第 10 节）与 `gh issue list` 实测；未验证项已逐条标注。*

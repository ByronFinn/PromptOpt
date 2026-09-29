# PromptOpt v2 V2 纵切实施报告

> **日期**: 2026-09-29 | **范围**: V2（Harness Builder + Web UI v0），基于 `f498d57` 之上的未提交工作区（`git diff --stat` 9 个修改文件 +1025/−96，另新增 11 个未跟踪文件）
> **对照文档**: [ROADMAP.md](../../ROADMAP.md) V2 里程碑（第 95–104 行）、[PRD-0000](../prd/PRD-0000-promptopt-v2-gepa-go-rewrite.md)、上期 [IMPLEMENTATION-REPORT.md](../../IMPLEMENTATION-REPORT.md)（V0+V1）

## 1. 交付概述

本次交付兑现 ROADMAP V2 的全部四项：零配置模式（`promptopt run "<自然语言提示词>"`）跑通 **合成 → p¹ 方差过滤 → 检查点 → baseline 评估** 全链路，配套合成集 Web 审核页（htmx）。测试函数从上期的 62 个增至 **103 个**（`go test ./... -v` 实测 103 个 `--- PASS`）：新增 41 个 = harness 26（harness_test 22 + variance_test 4）+ server_test 10（现 22）+ run_test 5（现 11，零配置 flag 解析与 flagsFirst 重排），其余 44 个为上期已有（与 `git show HEAD:*_test.go | grep -c "func Test"` 对照核实）。

配套文档更新：[ROADMAP.md](../../ROADMAP.md) V2 四项复选框勾选；[README.md](../../README.md) 快速开始新增零配置模式示例、"当前状态"改为 V0→V2 已落地。

## 2. 交付清单（对照 ROADMAP V2 四项）

| 条目 | 状态 | 证据 |
|---|---|---|
| Harness Builder：从提示词合成任务规格 + 评测集 + 指标（默认全托管直跑） | ✅ | `internal/harness/synthesize.go`：spec / samples / probes 三段独立 optimizer 角色调用（synthesize.go:55-141），三级 JSON 防御（本地抽取→单次修复调用→原文摘录报错，synthesize.go:224-255）、空内容退避阶梯（cap 逐次翻倍，synthesize.go:147-181）、全部调用留痕 `synth/<id>/calls/NNN-<stage>.json`；`pipeline.go:56-162` 编排全流程。默认全托管=autopilot（run.go:140-143，checkpoint.go:37-43 自动放行），`--interactive` 才暂停 |
| p¹ 方差过滤：筛掉死样本与高随机性样本，保留高区分度最小辨识集 | ✅ | `internal/harness/variance.go`：五态判定 keep / dead_easy / dead_hard / noisy / unmeasured（variance.go:13-19），阈值带默认 `[0.01, 0.99]`（variance.go:31）；`filter.go:38-94` 逐探针变体重放全部样本，证据不完整的样本保守判 unmeasured 保留（filter.go:78-82）；`SelectKept`（filter.go:101-114）支持检查点期间新增样本。真实数据过滤结果见第 4 节 |
| 检查点机制：事件总线（SSE 推送）+ 合成集审核页 | ✅ | `internal/harness/checkpoint.go`：checkpoint.json 为唯一事实源，pending 先落盘，interactive 每 300ms 轮询（checkpoint.go:31-64）；`synth_done` / `filter_done` / `checkpoint` 三类 harness 事件走与 eval 相同的事件封套（harness/pipeline.go:18-22），经 `eventFanout`（run.go:464-504）同时落 `events.jsonl` 与 SSE Bus（web/server.go:54-127）；run 列表页渲染检查点横幅并给审核页链接（index.html:44-45） |
| Web 审核页：在 V1 骨架页基础上补齐合成集审核交互 | ✅ | `internal/web/review.go`：6 个端点（GET 页面/行局部/状态轮询、POST 编辑、DELETE 样本、POST 批准，review.go:19-29）；`review.html` + `review_partials.html` 以 htmx 局部交换实现（保存/删除/批准均 `hx-*`），htmx.min.js 落 `internal/web/static/`——同时了结了上期报告的"原生 JS 而非 htmx"偏差（V2 交互页全面使用 htmx）；synthDir 非空才挂载（server.go:352），run 详情页带审核页入口（run.html:41） |

**导入自有数据的通道**（条目三括注的兑现方式）：审核页提供编辑（替换 input/expected/split）、删除、批准；**新增/导入**走产品形态定义的第二干预通道——检查点暂停期间直接编辑 `synth/<id>/samples.json`（LoadSamples 全量校验防脏写，artifact.go:179-191），管线放行后重读工件（pipeline.go:122-144），报告中缺席的样本按保留计（filter.go:101-114），审核页将其显示为"新增（未测）"（review.go:274-279）。审核页本身无新增表单端点，此为有意的双通道设计，非缺口。

**新增文件**（`wc -l` 实测）：

```text
internal/harness/          # 8 文件 2228 行（其中测试 1067 行）
├── synthesize.go   (445)  # 三段合成 + JSON 防御/修复/退避
├── pipeline.go     (169)  # 零配置全流程编排
├── filter.go       (136)  # p¹ 重放与 SelectKept
├── variance.go      (97)  # 判定/阈值/方差
├── checkpoint.go    (91)  # 检查点门
├── artifact.go     (223)  # synth 工件读写（原子写、样本全量校验）
├── harness_test.go (992) + variance_test.go (75)
internal/web/
├── review.go       (310)  # 审核端点 + 视图组装
├── templates/review.html (81) + review_partials.html (39)
└── static/htmx.min.js     # htmx 1.x 嵌入源
```

修改文件（`git diff --stat`）：run.go（+354 区间：零配置模式、flag 重排 `flagsFirst` 使 `promptopt run "<提示词>" --samples 3` 两种顺序等价解析，run.go:379-402）、run_test.go（+321）、server.go/server_test.go、config.go（`DefaultSamples=6 / DefaultProbeVariants=2 / DefaultSynthMaxTokens=4096`，config.go:31-33）等 9 个文件。

## 3. 退出码与产物契约（零配置模式）

- 退出码沿用契约：0 成功 / 1 合成或评估或用法失败 / 2 预算耗尽（探针与 baseline 共享预算，探针吃穿预算时 baseline 全部未派发，pipeline.go:52-54 注释）
- 合成产物双落盘：`synth/<run_id>/`（manifest / spec / samples / filter / checkpoint / calls / probes）与评估产物 `runs/<run_id>/`（manifest / events.jsonl / samples / summary.json + run.json 双名，run.go:243-252）

## 4. p¹ 过滤在真实数据上的过滤结果（tcmsp-30）

**结论：5 条合成样本过滤后保留 1 条——剔除 3 条死样本（全对）+ 1 条高噪声样本；仅用保留样本跑 baseline，f1 / json_validator 双满分，退出码 0。**

以下数据全部为本会话经 `ssh tcmsp-30` 读取 `/home/jiuwei/PromptOpt/` 下工件所得（命令：`cat runs/20260929-035902-41aa9f76/run.json`、`cat synth/20260929-035902-41aa9f76/{manifest,filter,checkpoint}.json`，及 python3 提取 spec/samples 摘录与 events 类型计数），非转述材料。

### 4.1 运行输入（synth/20260929-035902-41aa9f76/manifest.json）

```json
{"prompt": "从中医病历文本中抽取症状、证型与方剂，输出JSON", "model": "jiuwei-tcm",
 "synth_samples": 5, "probe_variants": 2, "created_at": "2026-09-29T03:59:02Z"}
```

合成任务规格（spec.json 摘录）：`task.name = tcm_record_extraction`，指标 `[json_validator, f1]`、主指标 `f1`，prompt_template 要求输出含 `symptoms` / `zheng` / `fangji` 字段的 JSON 对象；2 条语义等价探针变体（措辞与结构不同，均含 `{input}`）；samples.json 共 5 条样本（anchors 槽位 0，ADR 0001 预留）。

### 4.2 过滤报告（filter.json 原文摘录）

| 样本 | 探针分（probe-1 / probe-2） | 方差 | 判定 |
|---|---|---|---|
| s01 | 1 / 0.9 | 0.0025 | **keep（保留）** |
| s02 | 1 / 1 | 0 | dead_easy（剔除） |
| s03 | 0.9412 / 0.9412 | 0 | noisy（剔除） |
| s04 | 1 / 1 | 0 | dead_easy（剔除） |
| s05 | 1 / 1 | 0 | dead_easy（剔除） |

聚合：`"kept": 1`，`"dropped_by_verdict": {"dead_easy": 3, "noisy": 1}`；阈值 `Low=0.01, High=0.99`。解读：s02/s04/s05 两条探针变体都全对（无区分度）；s03 两变体得分相同且落在阈值带内（任何改写都动不了它）；唯 s01 对措辞敏感（0.9 vs 1），是能区分提示词好坏的样本——p¹ 的"最小辨识集"意图在真实模型（jiuwei-tcm）上成立。

### 4.3 baseline 评估（runs/20260929-035902-41aa9f76/run.json）

```json
{"run_id": "20260929-035902-41aa9f76", "task_name": "tcm_record_extraction",
 "candidate_id": "baseline", "dataset_name": "synth", "status": "completed",
 "exit_code": 0, "total_samples": 1, "evaluated_samples": 1, "failed_samples": null,
 "undispatched": 0,
 "usage_by_role": {"executor": {"prompt_tokens": 2337, "completion_tokens": 7780},
                   "optimizer": {"prompt_tokens": 807,  "completion_tokens": 5961}},
 "metric_means": {"f1": 1, "json_validator": 1},
 "started_at": "2026-09-29T04:06:53.75Z", "finished_at": "2026-09-29T04:07:11.99Z"}
```

- **只评估了 1 条**（`total_samples: 1`）：baseline 评估集即过滤后的保留集（pipeline.go:142-161），过滤器实际省掉了 4 条无信号样本的评估开销
- 分角色用量：optimizer 807+5961 = 3 次合成调用（spec/samples/probes，pipeline.go:95 记入 optimizer 角色）；executor 2337+7780 = 2 探针 × 5 样本的重放 + 1 次 baseline
- checkpoint.json：`{"status": "approved", "mode": "autopilot"}`（04:06:53.75 自动放行后立即开跑，baseline 耗时约 18.2s）
- 样本 trace（samples/001-s01.json）实测字段含 prompt / reasoning / response / scores / usage（baseline 调用 212+595 tokens，f1=1）

### 4.4 事件流完整性

`runs/<id>/events.jsonl` 类型计数：`synth_done ×1、filter_done ×1、checkpoint ×2（pending+approved）、run_start ×1、sample_start ×1、sample_done ×1、run_done ×1`——合成、过滤、检查点、评估全生命周期事件同流落盘，与 SSE 同源（AGENTS.md 输出约定）。

另：远端还有一个更早的 run `20260929-031204-a5a30ec3`，其 runs/ 下仅有 manifest.json 且 events.jsonl 为 0 行、无 synth 目录——系早期一次未跑通即中止的尝试，对上述结论无影响，如实记录。

## 5. 门禁结果（本次实际执行）

在仓库根目录、当前工作区（V2 未提交变更之上）执行：

| 命令 | 结果 |
|---|---|
| `go build ./...` | ✅ 通过（BUILD_OK） |
| `go vet ./...` | ✅ 通过（VET_OK） |
| modernize（与 CI 同命令：`go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -test ./...`） | ✅ 通过（无输出，退出码 0） |
| `go test -race ./...` | ✅ 7 个包全部 `ok`（cmd/promptopt、config、core、eval、**harness**、provider、web） |
| `go test ./... -v` | ✅ `--- PASS` 计数 103，与 `grep -c "func Test"` 统计的 103 个测试函数一致 |
| staticcheck | ⏸ 未接入 CI 也未在本机运行（如实报告 not run）；ci.yml 现为 vet → modernize → test -race → build 四步（.github/workflows/ci.yml 实读），与 ROADMAP 架构总览所述"CI 跑 vet / staticcheck / modernize / test"相比 staticcheck 仍缺位 |

## 6. e2e 结论：成功

两层端到端，均通过：

### 6.1 本地零配置 e2e（当前未提交工作区，本会话执行）

mock OpenAI 端点（python3 标准库，按 synthesize.go:24-28 的阶段标记词区分 spec/samples/probes/评估调用；探针变体 2 号刻意答错以制造区分度）驱动：

```bash
go build -o /tmp/promptopt-v2-e2e/promptopt ./cmd/promptopt
/tmp/promptopt-v2-e2e/promptopt run "从文本中抽取关键词，输出 JSON 数组" \
  --samples 3 --probe-variants 2 \
  --base-url http://127.0.0.1:18099/v1 --model mock-model \
  --headless --out /tmp/promptopt-v2-e2e/runs
# → stdout JSON 摘要 + E2E_EXIT:0
```

结果：`status: completed, exit_code: 0, evaluated_samples: 3, metric_means: {"f1": 1}`；synth 工件齐全（manifest/spec/samples/filter/checkpoint/calls/probes），filter.json 三样本判定均为 keep（探针分 [1, 0]、方差 0.25），checkpoint.json 为 approved/autopilot——**零配置链路（合成→过滤→检查点→baseline）在工作区代码上端到端复现**。

### 6.2 tcmsp-30 真实模型 e2e（第 4 节同一 run）

零配置模式在真实模型 jiuwei-tcm（OpenAI 兼容端点 192.168.56.39:59001/v1，manifest 记载）上完整跑通：合成 5 样本 + 2 探针 → p¹ 过滤 5→1 → autopilot 检查点放行 → baseline f1/json_validator 双满分退出码 0。远端二进制 manifest 与本仓库 runManifest 字段一一对应（`version: "dev"`，未打版本号构建），为同一代码线的产物。

### 6.3 对 V2 验收口径的如实说明

ROADMAP V2 验收句为"零配置跑通合成→过滤→确认→**进入优化队列**"。已验证的是到"确认（检查点）→ baseline 评估"为止的全链路；"优化队列"最后一环依赖 V3 GEPA 引擎（尚未实现），当前流程以 baseline 评估收尾（run.go:41-46 注释明确此切面）。即：**V2 四个复选框项全部完成且验收链路已通到 V3 边界；"进入优化队列"待 V3 落地闭合**——复选框勾选不含验收句，此差异在本节显式声明。

## 7. 已知边界与后续

1. **V3 GEPA 引擎**（Reflector / Mutator / Frontier / Budget / VistaGuard）未动工——零配置流程的终点目前是 baseline 评估
2. **staticcheck 未接入 CI**（第 5 节）；ROADMAP 架构总览承诺的四件套缺一
3. **审核页无新增表单**：导入自有数据走 artifact 文件编辑通道（第 2 节"导入自有数据的通道"），与 PRD 干预双通道一致，但若希望页面内闭环，可在 V4 补 `POST /synth/{id}/samples` 端点
4. **上期遗留未清**（非本期范围，仍开放）：LICENSE 文件缺失（README badge 死链）、Anthropic 原生 Provider、SQLite store、LLM-judge 指标、demo GIF
5. **探针重放的预算语义**：探针与 baseline 共享预算且探针优先（filter.go:19-22），`--budget-evals` 极小时会出现 baseline 全部未派发（退出码 2）——该路径有测试覆盖，但未在真实模型上复演

---

*报告内所有结论均基于本会话实际读取的文件（含 `ssh tcmsp-30` 远端工件，命令见第 4 节）、实际执行的命令（第 5、6.1 节）；未验证项已逐条标注 not run。*

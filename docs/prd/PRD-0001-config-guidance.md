# PRD-0001：配置引导与参数配置面（配置文件 / config 子命令 / 必填引导 / spec-metrics）

> **Status**: Grilled（红队 R1/R2 修订已落账）| **PRD**: PRD-0001 | **Created**: 2026-10-09 | **Last updated**: 2026-10-09（R2）
> 父文档：[PRD-0000](PRD-0000-promptopt-v2-gepa-go-rewrite.md)；裁判面依据：[research 0001](../research/0001-llm-judge-independent-config.md)

## Goal

用户为什么配不起来：PromptOpt 的必须参数（base_url/model）分散在 flag 与 `PROMPTOPT_*` 两层里，**没有持久化层**——每次调用都要重复给端点；没有引导——缺参报错只说「is required」不说去哪配；零配置合成的指标菜单被钉死为确定性三件套（`synthesize.go:412`），裁判面已全线铺好却永远吃不到；五范式与两裁判后端的选择只能靠 `--optimizer`/`--judge-backend` flag，无文件层。

本 PRD 给出**最小而完整**的配置体验设计：一个 YAML 配置文件层、一个 `config` 子命令（init/list/get/set/unset）、缺参指路报错、一个只读 Web 引导页、`--spec-metrics` 指标钉死，以及上一轮实测遗留问题的处置方案。

**第一性原理**：PromptOpt 是治理型工具，一切结论建立在「这次 run 的语义 = 命令行显式意图 + 环境身份」之上。配置面必须保证：① 同一条命令行在无文件时行为逐字节不变（可复现性不被隐藏状态破坏）；② 文件只承载**环境身份**（这台机器跟哪个 LLM 说话、用什么范式），不承载**单次实验意图**（花多少钱、跑几轮）——否则上次 run 的残留会悄悄改写下次 run 的语义，verify 复现链（manifest 快照）的信任基础被掏空。

## 证据基础（本会话实读/实跑）

以下事实由本会话与红队复核会话亲自 Read/grep/实跑核实（标注「勘察」的为勘察材料，其余均有本会话或红队会话的现场核对记录；行号为 2026-10-09 现场）：

- **解析链**：`resolve(flag, env, fallback) = cmp.Or(...)` 严格 flag > env > 默认（`internal/config/config.go:90-92`）；12 个 env 常量（config.go:19-38）；`BaseURL/Model` 无默认空值报用法错（config.go:96-103）；`APIKey` 默认 `"1"`（config.go:42,106-108）；judge 五 resolver 全空默认逐字段回落执行器（config.go:115-153）；decision 两连接字段有 env、backend 与两阈值 flag-only（config.go:32-37）；`ParseTimeout` flag 侧严格、env 侧宽容（config.go:169-209）；workers/addr flag-only（config.go:14-15 注释）。
- **flag 面与汇聚点**：`parseRunFlags`（`cmd/promptopt/run.go:1105-1370`）一次解析全部 flag，校验全部**内联**在尾部（run.go:1197-1369，无可复用 seam）；`fs.Visit` 显式标记 addr/port/optimizer/evo-variant（run.go:1177-1188）；required 检查 run.go:1335-1342；范式 flag 「configured 模式不得静默通过」成文于 run.go:1259-1261 注释与 :1262-1267 报错；judge/decision resolver 调用点 run.go:1351-1360。`resolveVerifyConn` 是 verify 汇聚点（`cmd/promptopt/verify.go:450-499`）：provider=flag>manifest>openai、base_url/model=flag>env>manifest、rps/extra_body=flag>manifest（`fs.Visit` 标记显式性 verify.go:347-354）、judge 五字段=flag>env>manifest、决策阈值 `cmp.Or` 浮点合并（verify.go:493-494，显式 0 与未给不可分的先例）。
- **manifest 快照**：两处写入（run.go:183-200 手动模式、run.go:320-345 零配置），api_key 绝不入快照（勘察核实 run.go:1490-1493 注释）。
- **Provider 构造**：`newProvider`/`newJudgeProvider`（`cmd/promptopt/provider.go:20-29,54-65`）；三客户端构造器字面量 `cmp.Or(cfg.Timeout, 180*time.Second)`（openai.go:57、anthropic.go:60）。
- **命令分发与输出通道**：`dispatch` 纯 switch（`cmd/promptopt/main.go:44-76`）；usage main.go:78-91；serve 独立小 flag 面且无 env 回落（main.go:137-176，`web.NewServer(*runsDir, "", nil)` main.go:171）；**anchor list 全走 stderr**（`cmd/promptopt/anchor.go:151-154`，本会话 grep+sed 亲核）；version/help 走 stdout（main.go:66/:69）；run `--headless` JSON 走 stdout（输出约定：AGENTS.md「人类可读输出走 stderr；--headless 时 stdout 仅输出 JSON」）。
- **spec 钉死现场**：`buildSpecPrompt` 指标菜单写死（`internal/harness/synthesize.go:405-417`，菜单行 :412）；`buildSamplesPrompt` 整体 marshal spec（synthesize.go:419-431，:420）；`Pipeline.Run` 顺序 = SynthesizeSpec（pipeline.go:106）→ SynthesizeSamples（:110）→ SaveSpec（:122）→ checkpoint 重读再 Validate（:168-174）→ 基线；Filter 全量透传 judge 面（pipeline.go:130-142）；`chat()` 不传 Temperature、走网关默认（synthesize.go:151-175 + 勘察）。
- **指标双表**：`core.ValidMetrics` 静态白名单（`internal/core/load.go:28-35`）是 Task.Validate 唯一准入（load.go:88-89）；eval 注册表运行期分发，`llm_judge` 保留名禁注册（`internal/eval/metrics.go`，本会话 sed :25-95 核实）；`Task.Primary()` 缺省回落 `Metrics[0]`（`internal/core/types.go:43`）。
- **reflector 空池现场**：`Reflect` 的 check 空池报错（`internal/engine/reflector.go:58-77`，check :66-71）；normalize 清洗可把非空池清空——text 全为 `{input}` 字面量时 `CleanInputLiteral`+TrimSpace 后为空被丢弃（reflector.go:88-90）；`Defend` 三级防线，tryParse 首次成功且 check 通过即直接返回、**不进修复**（reflector.go:306-311，红队会话复核+本会话亲读 :297-328）；gepa 对 Reflect 错误优雅跳轮（`internal/engine/gepa.go:111-121`），「没有可验证的假设」分支 gepa.go:158-171 现为死代码（勘察 overlay 实测）；生产调用方仅 gepa.go:111 一处（红队会话 `grep -rn '.Reflect('` 复核）。
- **Web 面**：`Handler()` pattern 路由 + register 分层 + `originGuard` 统一包裹（`internal/web/server.go:210-231`）；adopt 是唯一写端点先例（勘察核实 frontier.go:290-330）；serve 只读章程（AGENTS.md:70 + main.go:171）。
- **依赖与惯例**：go.mod 仅 `gopkg.in/yaml.v3 v3.0.1`；MCP stdin 可注入（`cmd/promptopt/mcp.go:40`）；`.gitignore` 无 promptopt.yaml 条目；CHANGELOG `grep -c "V7"` = **0**（文档债）。
- **实跑**：本会话 `go test ./internal/config/ -count=1` → `ok ... 0.894s`；红队会话 `go build ./...` OK、`go vet ./...` OK、`go test ./internal/config/ -count=1` ok 0.886s，并逐行核对本文档全部行号引用吻合。**未跑**：全量 `go test ./... -race`（实施验收项）、D9③ 远端四断言（无 tcmsp-30 环境）。
- research 0001 结论亲读：**裁判配置面「可选（默认回退执行器）」**（§TL;DR v2；§3.4 manifest 快照是配置面的配套要求）。

## 修订记录（红队 R1，2026-10-09）

| # | 攻击 | 裁决 |
|---|------|------|
| 1 | D1「存在且可解析才生效」与「损坏硬错」互斥，损坏文件可被低优先级好文件静默遮蔽 | **采纳**。钉死「首个**存在**的文件生效，该文件损坏即硬错退出 1，不继续发现链」；补遮蔽测试切口（§D1） |
| 2 | 显式性清单漏 judge_max_tokens/judge_decision_confidence/judge_decision_diag_below 三个「0=默认」typed 键，文件值会压过显式 flag 0 | **采纳**。显式性标记清单扩为 10 键；补测试切口：文件 confidence=0.7 + argv 显式 0 → 生效 0（§D2） |
| 3 | D8 权限检测按整数比较会把自家 0600 输出误报 | **采纳**。钉死位运算 `Perm() & 0o077 != 0`，测试 0600 不告警/0644 告警；配置目录 MkdirAll 0700（§D8） |
| 4 | config list 打 stdout 与「人类可读走 stderr」章程及 anchor list 先例冲突 | **裁决（折中）**：list 人类可读走 **stderr**（与 anchor list 同构，anchor.go:151-154），`--headless` JSON 走 **stdout**（与 run --headless 同规）——双通道均有既有先例，**章程零修订**；撤销 R0 的「list 默认 stdout」论证（§D4） |
| 5 | 文件提供的 optimizer/evo_variant/spec_metrics 在 configured 模式被静默忽略，违反 run.go:1259-1261 成文哲学 | **采纳（提示不硬错）**：configured 模式文件含这三键 → stderr 一行中文提示，三键同规；不硬错保 CI 复用机器文件（§D3） |
| 6 | 「同一套校验器」缺前置 seam：校验全部内联在 parseRunFlags | **采纳**。切分 1 增加 helper 抽取前置；config set 只做单键校验、跨键完整性留 run/verify 汇聚点（§D4、切分） |
| 7 | Web 页数据源弱于进程真相（run --web 模式显示错误值） | **采纳**。run 侧起看板前 `config.PublishSnapshot(o)` 包级原子快照（仿 sink 注入法），handler 快照优先、serve 回落全局链；零 NewServer 签名变更。红队裁决落账：**不加配置写端点 = 同意设计**（§D6） |
| 8 | verify 侧 rps/extra_body 文件层归属两读（D2 总式 vs D3 表备注矛盾） | **采纳**。钉死 flag > manifest > **file 补位**（与「manifest 没有的键文件层才补位」原则一致）；D3 表备注同步（§D2/§D3） |
| 9 | 密钥经 argv 进 shell history / ps 可见，四道闸未覆盖 | **采纳**。补第五道闸：`config set api_key -` / `init --api-key -` 从 stdin 读值（kubectl --from-file 惯例）（§D8） |
| 10 | 来源标注缺命中文件路径与被遮蔽文件可见性 | **采纳**。list 首行打印命中文件路径并检测列出被遮蔽的低优先级文件；Web 设置页同（§D4） |
| 11 | D9① 断言裁决 + 「normalize 后为空」语义外溢 | **采纳**。恰 1 次调用断言获红队复核确认（Defend :306-311 短路；现有断言无冲突——TestReflectorRepairsInvalidJSON 的 2 次断言属非法 JSON 用例、TestReflectorEmptyResponseShortCircuits 测空 raw 非空池）；语义钉死为「**normalizeHypotheses 后为空**」（空数组与全清洗池同规）；**独立 commit + 独立测试切口评审**，不搭配置面便车（§D9、切分） |
| 12 | 范围体量：引擎行为变更（D9①）与配置面零耦合 | **采纳（不阻塞）**：D9① 独立成 commit（可独立成 PR），其余按切分走（切分） |
| 13 | 「字节级不变」未限定范围：新增 --config flag 必然改帮助文本 | **采纳**。验收限定为 run 语义不变（manifest 字段值/stdout JSON/退出码/stderr 叙述），帮助文本用法行新增除外（§D2） |

### 修订记录（红队 R2，2026-10-09）

| # | 攻击 | 裁决 |
|---|------|------|
| 1 | verify 侧 file 补位会改写复现现场：manifest 的 rps/extra_body/judge_max_tokens/两阈值全部 omitempty（run.go:1441-1521，本会话亲核；注释原文「0 = the eval-package defaults, omitted by omitempty」），「manifest 缺失」无法区分「run 显式 0」与「run 未给」；extra_body 补位让 verify 执行器调用带上 run 时没有的网关参数→分数不可比→回归门禁判定被污染 | **采纳，推翻 R1 #8 的「补位」裁决**。verify 文件层收窄为**连接身份键白名单**（11 键，见 §D2）：白名单外的行为键（extra_body/rps/judge_max_tokens/judge_backend/两阈值）verify 一律不从 file 取值——run 现场真值要么在 manifest 要么是 eval/构造器默认，补位只引入偏差不恢复信息。测试切口：manifest 无 extra_body + 文件有 extra_body → verify 执行器请求不带该参数（§D2） |
| 2 | init 的 stdin `-` 通道与九步行协议共用 stdin 且读取顺序未钉：串行错位会把 base_url 答案行当 key 写入，或前置读取致向导 EOF 全默认——两形态行为完全不同，实现者两可 | **采纳**。钉死「`-` 通道值在向导开始前从 stdin **前置读取**（读至首个换行，无换行则读至 EOF），行协议从剩余 stdin 继续」；补文档示例与 httptest 读取顺序切口（§D4/§D8） |
| 3 | D2「stderr 叙述逐字节不变」与 D5 有意升级缺参报错文案表述不自洽（同属无文件场景 stderr）；实证 `grep -rn "is required" cmd/promptopt/*_test.go` 零命中——现无测试钉缺参文案，纯表述缺口 | **采纳**。D2 验收补例外句「D5 有意升级的缺参/decision 缺参文案除外，新文案快照归切分 2 验收钉」；切分 2 验收加缺参文案断言（§D2/切分；grep 零命中本会话亲跑确认，exit 1） |
| 4 | set/unset/init 对已存在文件的写回语义未钉：损坏文件不严格解码写回会静默丢用户其它键；init 覆盖已存在文件无提示 | **采纳**。set/unset 读现有文件走与 Load 同一严格解码（损坏即错退出 1，测试切口）；init 目标已存在 → stderr 列出将被覆盖的键并要求 `--force` 或交互确认，非 tty 下默认拒绝覆盖（§D4） |
| 5 | 快照 provenance 与 10 键显式性标记是两套收集，会漂移（页面来源标注与实际合并决策不一致，重演 R1 #7 的「页面撒谎」） | **采纳**。一套 per-key 来源收集器，10 键显式性判定从其派生（Source==flag 即显式）；切分 4 写明复用约束（§D6/切分） |
| 6 | 杂项三则：① shadowed 检测须仅标注存在性不读内容；② D1 遮蔽切口的 temp HOME 位置随 GOOS 不同；③ 显式性清单的测试期保障（反射 yaml tag 断言登记表） | **全部采纳**。① file.go 注释钉明 shadowed 仅存在性语义；② 切口文本注明按 `runtime.GOOS` 放文件（darwin=`$HOME/Library/Application Support/promptopt/`，linux=`$HOME/.config/promptopt/`）；③ 切分 1 验收加反射完整性测试——config_test 反射 File 结构体 yaml tag，断言每字段在合并登记表（键名→是否需显式标记）登记，新增文件键忘登记即测试红（§D1/切分 1）。另采纳降忧：`T.Chdir` 在 `t.Parallel` 用例中直接 panic（Go 1.24+），漏隔离炸测试而非静默污染 |

## 决策

## D1 配置文件：格式、发现顺序、损坏语义

### 决策

1. **格式 = YAML**，经 `gopkg.in/yaml.v3` 解码——它是 go.mod 唯一依赖（go.mod:5），core 的 task/candidate/dataset 加载同源，零新增依赖。不选 TOML/JSON：YAML 已是仓库任务定义的既方语言。
2. **发现顺序**（R1 修订：**首个存在的文件生效**，与可解析性解耦）：
   1. `--config <path>`（run/verify 新增 flag；显式指定但文件不存在/不可读 → 硬错退出 1）
   2. `PROMPTOPT_CONFIG` 环境变量（同上，显式即严格；空串视为未设）
   3. `./promptopt.yaml`（当前目录，非点文件）
   4. `os.UserConfigDir()/promptopt/config.yaml`（stdlib：Linux 尊重 `XDG_CONFIG_HOME`，macOS 落 `~/Library/Application Support/promptopt/config.yaml`）
   **命中即止，不跨文件合并；命中的文件若损坏（语法错/类型错/未知键）→ 硬错退出 1，绝不跳过继续发现链。**
3. **损坏语义**：YAML 语法错误、类型错（如 `max_tokens: "abc"`）、**未知键**，一律硬错（用法错退出 1），错误信息带文件路径与行位置。解码用 `yaml.v3` `Decoder.KnownFields(true)`。

### 依据

- **R1 遮蔽攻击的正面回答**：R0 的「首个存在**且可解析**」自相矛盾——按字面实现，项目级损坏文件会被用户级好文件静默遮蔽，恰是本设计自己声讨的「run 以用户不认可的语义烧预算」。存在性决定命中，命中后的可解析性决定生死，两件事必须分开判定。
- **不合并**：两文件叠加时「哪个键来自哪层」不可解释，`config list` 的来源标注会撒谎。KUBECONFIG 多路径合并是多集群运营的复杂度，单机单用户工具不欠这笔债。
- **显式 vs 隐含不对称**：`--config` 是「配置在这」的宣称，缺失即宣称失败，必须响（同构先例：`--judge-backend decision` 缺 url/model 即用法错，run.go:1361-1363）；隐含发现缺失是常态，静默跳过。
- **损坏/未知键硬错**：文件是书写的持久意图，静默忽略 = 程序以用户不认可的语义烧预算且 manifest 与用户认知脱节。仓库对「显式书写面严格、可选宽松面宽容」已有成文区分（`ParseTimeout` 注释，config.go:169-200）；未知键硬错防 `baseurl:` 类拼写静默吞掉。
- **`PROMPTOPT_CONFIG`**：没有它 CI 想用文件层只能改 cwd 或每条命令塞 `--config`。先例 KUBECONFIG。

### 落地要点与测试切口

- 新文件 `internal/config/file.go`：`Load(path string) (*File, error)`（严格解码）；`Discover(explicit string) (path string, shadowed []string, file *File, err error)` 封装四层发现并**返回被遮蔽的低优先级存在文件列表**（供 D4 来源可见性）。**shadowed 语义（R2 #6①）：仅标注存在性（os.Stat 可命中即入列），不读内容、不校验、不解析——被遮蔽文件损坏不得影响 Discover/list，命中文件才负硬错义务**；该语义以 file.go 注释钉明。
- 路径展开：`~` 前缀用 `os.UserHomeDir()`。
- **测试切口（R1 #1，R2 #6② 补 GOOS 注）**：`t.Chdir` temp dir 放语法错误的 `./promptopt.yaml`，同时在 temp HOME 放合法用户级配置 → run 必须退出 1（不得静默使用用户级文件）。用户级文件位置按 `runtime.GOOS` 放置：darwin 为 `$TMPHOME/Library/Application Support/promptopt/config.yaml`（`os.UserConfigDir()` 实现），linux 为 `$TMPHOME/.config/promptopt/config.yaml`——切口不得写死单一路径。

## D2 单一解析链与字节级不变

### 决策

**单一链，文件层插在 env 与默认之间：**

- **run / mcp**：`flag > env > 文件 > 默认`（mcp 复用 parseRunFlags/parseVerifyFlags 自动获得）。
- **verify（R2 #1 再修，推翻 R1 #8 的「补位」裁决）**：`flag > env > manifest > 默认` 为主链；文件层收窄为**连接身份键白名单**，仅对 manifest 缺失/omitempty 的白名单键补位（旧工件）：
  **白名单（11 键，全部连接身份）**：`provider / base_url / model / api_key / timeout / judge_provider / judge_base_url / judge_model / judge_api_key / judge_decision_url / judge_decision_model`。api_key 永不入 manifest，链 = flag > env > file > `"1"`；timeout_seconds 缺省 180 时 omitted（run.go:1489-1493 注释），file 补位仅是重设 deadline，不改分数语义。
  **白名单外（行为键）verify 一律不从 file 取值**：`extra_body / rps / judge_max_tokens / judge_backend / judge_decision_confidence / judge_decision_diag_below`。理由（R2 #1 的测量语义分析，本会话亲核 manifest 结构佐证）：这些键全部 omitempty（run.go:1441-1521），「manifest 缺失」无法区分「run 显式 0/未给」与「run 给过值」；而 run 现场真值要么在 manifest、要么就是 eval/构造器默认——file 补位只引入偏差不恢复信息。最实质的是 extra_body：它作用于执行器调用（如 enable_thinking 改变模型输出），旧工件 verify 时文件新配 extra_body = verify 在认证一个 run 从未跑过的场景，回归门禁判定被污染；两阈值补位改变裁判级联回落样本集，judge_max_tokens 补位改变裁判输出预算——同污染。`rpsSet/extraBodySet` 显式性标记已有（verify.go:347-354），flag 显式覆盖 manifest 的既有行为不动。
  **测试切口**：manifest 无 extra_body + 文件有 extra_body → verify 执行器请求体**不带**该参数（httptest 桩断言 wire payload）；同理断言 rps/judge_max_tokens/两阈值不补位。
- **显式性标记清单（R1 扩为 10 键）**：无 env 的 7 键 `provider/max_tokens/rps/extra_body/optimizer/evo_variant/spec_metrics`，加 3 个「0 = 有意义默认」的 typed 键 `judge_max_tokens / judge_decision_confidence / judge_decision_diag_below`。后者链序：**flag 显式给定（含显式 0）→ 用 flag 值**；未给 → env（仅 judge_max_tokens 有）→ 文件 → 0。`cmp.Or` 类合并区分不了「显式 0」与「未给」（verify.go:493-494 浮点即此先例），无标记则文件值压过用户显式 `--judge-decision-confidence 0`——违反本设计自己的公理。
- **实现机制**：config 包公开 resolver 增加 fileVal 参数，内部 `cmp.Or(flag, env, file, fallback)`——文件层排序由 config 包统一拥有，cmd 层不得自行拼链；10 键清单由 parseRunFlags/parseVerifyFlags 的 fs.Visit 标记驱动合并（现仅 4 键，run.go:1177-1188）。
- **字节级不变验收（R1 限定范围，R2 #3 补例外句）**：**run 语义**不变 = manifest 字段值、stdout JSON、退出码、stderr 叙述逐字节不变；**两项例外**：① 帮助文本（新增 `--config` flag 必然改用法行）；② **D5 有意升级的缺参/decision 缺参报错文案**——同属无文件场景的 stderr，但属本设计的主动改进而非回归，其新文案快照归切分 2 验收钉（现状无测试钉旧文案：`grep -rn "is required" cmd/promptopt/*_test.go` 零命中，本会话实跑 exit 1）。三层钉死：
  1. config 包：resolver 表补 `fileVal == ""` 列，断言与旧三层语义逐字段相等；
  2. cmd 包：固定 argv 调 parseRunFlags/parseVerifyFlags（t.Chdir 无配置文件 temp dir + 隔离 `PROMPTOPT_CONFIG`），全结构体现值比对（勘察材料：run_test.go:656-665、run_addr_port_test.go:38 范式）；
  3. e2e：httptest 桩无文件 run，manifest.json 逐字段比对（勘察材料：run_timeout_test.go:24-123 模式）。

### 依据

- **env > 文件**：12-factor——env 是本次进程注入的意图，文件是机器静态身份；这是既有链的纯延长（把 fallback 拆成 file+default），不改变既有两层相对顺序。
- **verify 文件层 = 连接身份白名单（R2 #1 终裁）**：verify 的使命是复现那次 run；manifest 是「当时的值」，文件是「现在的状态」。R1 #8 的「行为键 file 补位」经 R2 复核是错误裁决——补位支持者只看了键归属，没看测量语义：extra_body/裁判参数直接改变被测行为与分数标尺，verify 补位等于静默认证一个 run 从未跑过的场景，正中本设计自己的第一公理。连接身份键（网关在哪、用哪个模型、哪个裁判服务）补位无此害：它们不改写测量语义，只是让旧工件在「env 也没有」的机器上仍可运行——这正是文件层「环境身份」的本职。
- **typed 键显式性（R1 #2）**：显式 `--judge-decision-confidence 0` 的语义是「用 eval 默认 0.5」，文件 0.7 不得压过；显式 `--judge-max-tokens 0` 的语义是「回落 --max-tokens」，文件 512 不得压过。链序「显式即终判」是公理的直接推论。
- **manifest 无 provenance（维持 R0）**：文件来源值与 flag 来源在 manifest 字节不可区分；「值从哪来」归 `config list` 来源标注管。

## D3 键集分界

### 决策

**入文件（环境身份：连接 + 范式选择）**——全键表，YAML 键 = flag 名的 `-`→`_`：

| YAML 键 | 对应 flag | env 层 | 默认 | 备注 |
|---|---|---|---|---|
| `provider` | `--provider` | — | openai | 校验同 flag 白名单；显式性标记 |
| `base_url` | `--base-url` | PROMPTOPT_BASE_URL | 无（必填） | |
| `model` | `--model` | PROMPTOPT_MODEL | 无（必填） | |
| `api_key` | `--api-key` | PROMPTOPT_API_KEY | `"1"` | 见 D8 |
| `max_tokens` | `--max-tokens` | — | 8192 | 模型家族属性；显式性标记 |
| `timeout` | `--timeout` | PROMPTOPT_TIMEOUT | 180s | 文法同 ParseTimeout（`90s`/`300`）；verify file 白名单内 |
| `rps` | `--rps` | — | 0 | 显式性标记；**verify 不从 file 补位（R2 行为键）** |
| `extra_body` | `--extra-body` | — | 空 | map 透传；显式性标记；**verify 不从 file 补位（R2 行为键）** |
| `out` | `--out` | PROMPTOPT_OUT | runs | |
| `judge_provider` | `--judge-provider` | PROMPTOPT_JUDGE_PROVIDER | 空=回落执行器 | |
| `judge_base_url` | `--judge-base-url` | PROMPTOPT_JUDGE_BASE_URL | 空=回落执行器 | |
| `judge_model` | `--judge-model` | PROMPTOPT_JUDGE_MODEL | 空=回落执行器 | |
| `judge_api_key` | `--judge-api-key` | PROMPTOPT_JUDGE_API_KEY | 空=回落执行器链 | 见 D8 |
| `judge_max_tokens` | `--judge-max-tokens` | PROMPTOPT_JUDGE_MAX_TOKENS | 0=回落 | **显式性标记（R1）**：显式 0 = 回落，压过文件；**verify 不从 file 补位（R2）** |
| `judge_backend` | `--judge-backend` | — | 空=llm | 维持无 env（config.go:32-35）；**verify 不从 file 补位（R2 行为键）** |
| `judge_decision_url` | `--judge-decision-url` | PROMPTOPT_JUDGE_DECISION_URL | 无 | decision 后端必填（run.go:1361-1363）；verify file 白名单内（连接身份） |
| `judge_decision_model` | `--judge-decision-model` | PROMPTOPT_JUDGE_DECISION_MODEL | 无 | 同上 |
| `judge_decision_confidence` | `--judge-decision-confidence` | — | 0（=eval 0.5） | **显式性标记（R1）**：显式 0 = eval 默认，压过文件；**verify 不从 file 补位（R2）** |
| `judge_decision_diag_below` | `--judge-decision-diag-below` | — | 0（=eval 0.6） | **显式性标记（R1）**：同上 |
| `optimizer` | `--optimizer` | — | gepa | 显式性标记；见下方 configured 模式策略 |
| `evo_variant` | `--evo-variant` | — | ga | 显式性标记；同上 |
| `spec_metrics` | `--spec-metrics` | — | 空=LLM 自选 | 显式性标记；见 D7 |

**绝不入文件（单次 run 的成本与行为旋钮）**：`budget_tokens/budget_evals/budget_opt_tokens`、`max_rounds/minibatch/epsilon/stagnation_limit/seed`、`samples/probe_variants`、`temperature/reps`、`workers`、`addr/port/web/headless/interactive`、`task/candidate/dataset/split/task-key`、位置参数 prompt。

**configured（三件套）模式下的范式键策略（R1 #5）**：文件含 `optimizer/evo_variant/spec_metrics` 且本次为三件套模式 → **stderr 一行中文提示**（如「promptopt run: 配置文件中的 optimizer/evo_variant/spec_metrics 在配置模式（task/candidate/dataset 三件套）下不生效（该模式无优化循环）」），**不硬错**——CI 复用机器文件跑三件套不该被炸；提示而非静默，延续 run.go:1259-1261「the paradigm flags must not pass silently」哲学到文件层（文件层更隐蔽：用户未必记得机器文件里有什么）。

**保持 flag-only 不动**：`workers`（config.go:14-15 明文）、`addr/port`（看板显隐语义与显式性标记强耦合）。

### 依据

- **分界判据**（第一性原理）：配置文件回答**这台机器/工作区是谁**，不是**这次实验怎么做**。预算类键入文件后残留会静默改写下次 run 语义，掏空 manifest 复现链信任。最佳实践对照：kubectl config 存集群身份不存每命令行为。
- **optimizer/evo_variant/spec_metrics 破例入文件**：范式与合成指标是**工作区策略**且被 manifest 快照（复现链不受文件漂移影响）；reps/temperature 同样影响语义却不入文件，因为它们改变**测量语义本身**（方差、噪声），范式只改变到达路径。

## D4 `promptopt config` 子命令

### 决策

挂载：`dispatch` 加 `case "config": return configCommand(rest)`，usage() 加中文行，实现落 `cmd/promptopt/config.go`。退出码只用 0/1。五个动作：

- **`config init`**：交互式向导，写入配置文件。问题序（每题给默认值与一句中文解释，回车取默认）：
  1. 执行器 base_url（必填，非空校验；示例 `http://localhost:8080/v1`）
  2. 执行器 model（必填）
  3. api_key（可选，默认 "1"；**支持 `-` 从 stdin 读值**——见 D8 第五道闸）
  4. provider（openai|anthropic，默认 openai）
  5. optimizer（可选，gepa/protegi/miprov2/evoprompt/p1/auto，默认 gepa；注明零配置模式专用）
  6. spec_metrics（可选，逗号列表；说明见 D7）
  7. 独立裁判 LLM（默认**跳过**——research 0001「可选（默认回退执行器）」）：yes 则追问 provider/base_url/model/api_key/max_tokens
  8. 决策后端（默认 llm；选 decision 则追问 url/model 必填 + 两阈值可选）
  9. timeout / rps（可选，回车默认）
  - **作用域**：默认写 user 级；`--scope project` 写 `./promptopt.yaml`（含 api_key 时触发 D8 .gitignore 处理）；`--config <path>` 显式落点。文件权限 0600，所在目录 MkdirAll 0700。
  - **已存在文件的覆盖保护（R2 #4）**：init 目标文件已存在时，先按严格 Load 读出，stderr **列出将被覆盖的已有键**，要求 `--force` 或（tty 下）交互确认；**非 tty 下默认拒绝覆盖**退出 1——静默覆盖手工编辑过的文件（如用户手加的 judge 键）= 静默丢用户意图。
  - **非 tty（ssh 管道/CI）**：按问题序从 stdin 逐行读答案（`bufio.Scanner`，一行一答案；空行=跳过取默认；EOF=余下全默认），io.Reader 可注入字段（默认 os.Stdin，仿 mcp.go:40），httptest 可覆盖。也支持纯 flag 一次性完成（flagsFirst 惯例），flag 已给的问题不追问。**`-` 通道读取顺序（R2 #2 钉死）**：`--api-key -`（或其它 `-` 值）在向导开始**前**从 stdin 前置读取（读至首个换行，无换行则读至 EOF），行协议从剩余 stdin 继续——不钉顺序则串行错位会把 base_url 答案行当 key 写入，或前置读取致向导 EOF 全默认，两形态必须择一钉死，此处钉前者。
- **`config list [--headless]`**（R1 #4/#10 修订）：
  - **输出通道**：人类可读 → **stderr**（与 anchor list 同构先例，anchor.go:151-154，本会话亲核）；`--headless` → JSON → **stdout**（与 run `--headless` 同规）。双通道均有既有先例，**AGENTS.md 输出约定零修订**——R0 的「list 默认打 stdout」论证撤销：它是数据不假，但仓库已有的解法是「人类 stderr / headless JSON stdout」双通道，照搬即可，不必动章程。
  - **来源可见性**：首行打印**命中文件路径**（如 `# config: /Users/x/.config/promptopt/config.yaml`）；随后列出**存在但被遮蔽**的低优先级文件（如 `# shadowed: ./promptopt.yaml（被上述文件遮蔽）`）——发现序首个命中生效、被遮蔽文件不参与合并，用户必须能看见这件事。
  - 逐键打印生效值 + 来源（flag/env/file/default）+ 对应 flag 名；api_key/judge_api_key 恒掩码。
- **`config get <key>`**：单键生效值 + 来源 + （file 来源时）命中路径；未知键报错列全键集。
- **`config set <key> <value> [--scope user|project]`**：**只做单键校验**（provider 白名单、judge_backend 枚举、timeout 文法、metrics 白名单复用 core.ValidMetrics 等）；**跨键完整性（如 judge_backend=decision 缺 url/model）不在 set 里把关**——那是 run/verify 汇聚点的职责（run.go:1361-1363 既有检查，文件值经合并后走同一检查），报错走 D5 新文案。默认写 user 级。`value` 为 `-` 时从 stdin 读值（密钥通道，D8）。**写回语义（R2 #4）**：读现有文件走与 `Load` 同一**严格解码**——目标文件损坏即报错退出 1（带路径与行位置），**绝不**宽松解码后写回（那会静默丢掉用户其它键）；测试切口：手写损坏的 `./promptopt.yaml` + `config set model x` → 退出 1 且文件内容不变。
- **`config unset <key> [--scope user|project]`**：删除键，回落链自然接管；键不存在=幂等成功；写回语义同 set（严格解码，损坏即错）。
- `help/-h/--help` → stdout + 0；未知子命令 → stderr + usage + 1（anchor.go:20-41 先例，勘察材料）。

### 前置重构（R1 #6，切分 1 的一部分）

现状校验全部内联在 parseRunFlags 尾部（run.go:1197-1369），无可复用 seam。「config set 与 flag 同一套校验器、不出现第二条校验语义」的红线要求先抽取：`validateProvider / validateOptimizer / validateJudgeBackend / validateJudgeProvider / validateTimeout / validateMetricsList` 等 helper（签名收值返 error），parseRunFlags 改调 helper（行为保持），config set/init 复用同一批 helper。**config set 永不实现跨键检查**——那会复制出第二条完整性语义，违反红线。

### 依据

- **为什么是子命令而不是 web 表单**：配置变更需要校验反馈与来源可见性，CLI 天然具备；web 写端点引入 provenance 与 CSRF 面问题（D6）。CLI 是变更主通道，web 是诊断副通道。
- **非 tty 行协议**：仓库「等待用户」既有模式不是终端问答而是磁盘工件轮询；ssh 管道下 stdin 技术可用但无遮蔽先例（勘察 §4）。最小面：行协议 stdin + flag 直给双通道，零新增依赖。
- **来源标注 + 路径可见性（R1 #10）**：用户先 `init --scope user` 再 `init --scope project` 后 user 文件被静默遮蔽——「为什么生效的是这个值」的诚实义务要求把命中与遮蔽都摆出来。

## D5 必填缺失引导

### 决策

`run`/`verify` 缺 base_url/model 时，报错文本升级为三途径指路（退出码仍 1，errors.Join 结构不动）：

```text
promptopt run: --base-url is required. 可通过三种途径配置（任选其一）：
  1. 命令行 flag：promptopt run --base-url http://localhost:8080/v1 ...
  2. 环境变量：export PROMPTOPT_BASE_URL=http://localhost:8080/v1
  3. 配置文件：promptopt config init  （或手写 promptopt.yaml）
```

- run 侧（run.go:1336-1341 两处）、verify 侧（verify.go:453-458，追加第四途径「manifest 快照」）同步升级。
- `--judge-backend decision` 缺 url/model（run.go:1361-1363）追加「或写入 config 文件 judge_decision_url/judge_decision_model」；**config set 不拦跨键完整性，这里就是完整性报错的落点**（R1 #6 配套）。

### 依据

报错职责是把全部合法出路一次性给全。`config init` 作为第一条可执行出路写进报错，把「缺参数」从死路变成一步引导。

## D6 Web 引导页

### 决策

**只读引导页 `GET /settings`**，run `--web` 与 serve 两模式均挂载（顶层路径，`registerSettingsRoutes(mux)` 分层，Handler 骨架不动；originGuard 自动罩护）。页面内容：生效配置表（值+来源+flag）、必须参数状态、按缺口生成的可复制 YAML 片段/等价命令行、键解释。

**数据源（R1 #7 修订）：双模式各有真相**
- **run `--web` 模式**：起看板前调 `config.PublishSnapshot(o)`——config 包级原子快照（mutex/atomic 保护，**仿 sink 注入法**：run.go:173 `startSink` 先例），快照携带合并后 runOptions 的逐键值与来源标注（**含 flag 层**——进程手里有 argv 显式性）。settings handler **优先读快照**。修复 R0 缺陷：run --web 下页面曾只能读全局链，「文件 0.7 + flag 显式 0」场景会显示 0.7 而 run 实际用 0——不是乐观是错误值，且发生在真相在手的进程里。**单一来源收集器（R2 #5）**：快照的逐键来源标注与 D2 的 10 键显式性标记**必须同一套 per-key 来源收集器**（收集器记每键 Source ∈ {flag, env, file, default}，显式性判定派生自 Source==flag），禁止 parseRunFlags 里另立一套 bool 与快照 map 并存——两套收集必然漂移，重演「页面撒谎」。
- **serve 模式**：无 runOptions，回落全局发现链+env（显示 env/file/default 来源）。页面文案注明语义是「**当前环境**的配置」而非某次 run 的现场（历史 run 的现场归 manifest/`runs/{id}` 页管）。
- `web.NewServer` 签名零变更（设计约束不动）。

**写端点（R1 #7 红队裁决落账）**：**不加配置写端点 = 红队同意设计**。理由成立：config 文件不在任何审计/回滚链内（adopt 有 adopted.json+事件+rollback，配置写入没有）、serve 无鉴权、操作者无从确认下次 run 用哪个值。D6 R0 所列条件（POST+表单+mutex+原子写+originGuard+默认关闭 opt-in+复用 config set 单键校验器）保留为**未来加回的门槛**。

index.html 加看板卡片式入口链接（index.html:61 先例，勘察材料）。

## D7 `--spec-metrics`：零配置合成指标钉死

### 决策

1. **flag**：`--spec-metrics "llm_judge,f1"`——逗号列表，仅零配置模式合法（与 --task/--candidate/--dataset/--split 同组互斥，run.go:1244-1254 先例）。校验：逐项 ∈ `core.ValidMetrics`（load.go:28-35）、无重复、非空。**不新增 `--spec-primary`**：primary 缺省取列表首项（types.go:43 回落语义）；非首项 primary 的高级场景走 `--interactive` 手编 spec.json（pipeline.go:168-174 重读再 Validate 兜底）。
2. **config 键**：`spec_metrics`（列表），链 flag > env（无）> file > 默认（空 = 现状 LLM 自选）；显式性标记清单内。
3. **覆盖机制**（插入点钉死）：`Pipeline.Run` 里 SynthesizeSpec 返回之后（pipeline.go:106）、SynthesizeSamples（:110）/SaveSpec（:122）之前——覆盖 `spec.Metrics`/`spec.PrimaryMetric` 并**立即 `spec.Validate()`** 快速失败；新列表不含原 primary 时置空交回落。顺序关键：`buildSamplesPrompt` 整体 marshal spec（synthesize.go:420），覆盖必须在样本合成之前，expected 才按新指标出题；下游探针/基线/优化读同一 spec 零改动（Filter judge 面透传 pipeline.go:130-142 已就绪）。
4. **manifest**：`spec_metrics` 入快照（omitempty）。
5. **默认菜单不动**：不给 flag 时 buildSpecPrompt 菜单仍是确定性三件套（synthesize.go:412）——llm_judge 不进默认菜单（默认静默追加裁判调用=成本+方差，违背成本治理默认最小化）。
6. **合成 temperature 不修**：`chat()` 省略 Temperature 走网关默认，与执行器默认语义**逐字一致**（`--temperature` 0 = 字段省略网关默认接管，run.go:1119 原文）；单独加合成温度面制造第一处分叉。本 PRD 不修。
7. **configured 模式**：文件含 spec_metrics → stderr 提示（D3 策略），flag 显式给仍按既有互斥报错（run.go:1244-1254）。

## D8 安全：密钥落盘

### 决策

1. **掩码**：`config list`/`config get`/Web settings 页对 `api_key`/`judge_api_key` 恒显 `******`，不做部分回显。
2. **落盘权限**：`config init`/`config set` 写文件一律 **0600**；所在目录 MkdirAll **0700**（R1 #3 补）。
3. **.gitignore 策略**：`config init --scope project` 且结果含 api_key 时：`./.gitignore` 存在且无 `promptopt.yaml` 条目 → 幂等追加并 stderr 一行说明；不存在 → 打印指引。user 级在仓库外，无指引需求。
4. **读取警告（R1 #3 修订：位运算钉死）**：发现的文件含 api_key 且权限检测 `perm := info.Mode().Perm(); perm & 0o077 != 0` → stderr 警告一行（**警告不报错**：CI umask 千差万别）。**必须位运算**：若按整数比较 `perm > 0o077`，本设计自家 0600 输出（0o600=384 > 63）每次读取都误报，告警变噪声。测试切口：0600 不告警、0644 告警。
5. **manifest 红线不动**：api_key 任何来源绝不入 manifest 快照；verify 复现 key 走 flag > env > file > `"1"`。
6. **第五道闸（R1 #9，R2 #2 钉读取顺序）：argv/history/ps 通道**：`config set api_key -` 与 `config init --api-key -` 从 **stdin** 读值（kubectl `--from-file -` 惯例）——argv 明文会进 shell history、对同机 ps 可见，恰属本设计自述的「意外泄露」威胁模型。argv 直接传值**不禁用**（交互场景成本更低）但文档引导 `-` 通道；与 ssh 非 pty 会话下 api_key 无遮蔽回显的场景（D4 自认）同用此通道解决。**读取顺序钉死**：`-` 通道值在向导开始前从 stdin **前置读取**（读至首个换行，无换行则读至 EOF），行协议从剩余 stdin 继续——`set` 无向导，`-` 即整读 stdin。文档示例（README config 小节必含）：`printf '%s\n' "$KEY" | promptopt config set api_key -` 与 `promptopt config init --api-key - < keyfile`。测试切口：httptest 桩喂 `"key\nhttp://x\nm1\n"` 给 `config init --api-key -`，断言 api_key=key 且 base_url=http://x、model=m1（前置读取不吞后续行）。

### 依据

威胁模型 = 意外泄露（git 提交、同机他用户、日志、**shell history**），不是主动攻击（单机单用户章程）。掩码断「诊断通道不泄露」，0600/0700 断「落盘即收权」，gitignore 断「不入版本库」，manifest 红线断「不入复现链」，stdin `-` 断「不入 history/ps」——五道闸各堵一条意外通道，全部廉价。

## D9 遗留问题处置（上一轮大基数实测）

### ① reflector 空假设池（缺陷，独立 commit + 独立测试切口评审）

现状：空池 `{"hypotheses":[]}` 是合法 JSON，走 check 失败 → 恰好一次修复调用，修复仍空则报错（reflector.go:297-328；勘察 overlay 实测双路径 PASS）；gepa 每轮烧 1 次修复调用、0 进度收尾，gepa.go:158-171 跳轮分支死代码。生产调用方仅 gepa.go:111 一处（红队 `grep -rn '.Reflect('` 复核），爆炸半径成立。

**设计（R1 #11 语义钉死）**：把「**normalizeHypotheses 后为空**」视为模型的有意义回答而非协议错误——这覆盖两种形态：**空数组** `{"hypotheses":[]}` 与**非空但全被清洗**（如 text 全为 `{input}` 字面量，`CleanInputLiteral`+TrimSpace 后为空被丢弃，reflector.go:88-90）。最小改动点在 `Reflect`（reflector.go:58-77）：check 闭包放行 normalize 后为空的池（不再以「响应中没有可用假设」触发修复），`Defend` 返回后 normalize 为空 → `return nil, nil`。语义边界钉死：

- 垃圾 JSON（parse 失败）仍走 Defend 修复——**修复通道从「parse+语义」缩为「parse-only」，这是有意的语义外溢，两形态均直接跳轮不修复**，实现者不得对空数组与全清洗池做两套行为；
- gepa 侧 `hyps==0` 自然落进 ：158-171 既有分支——vista.Record(false) + EventRoundDone skipped「没有可验证的假设」+ continue，死分支复活；全程空池以 ReasonRoundsDone 收尾、Best=baseline；
- 预算/取消语义零改动（:161-168 预算截断分支不动）。

**断言裁决（R1 #11 红队复核确认）**：恰 **1** 次调用、0 次修复——Defend 短路路径 `tryParse` 首次成功且 check 通过即返回、不进修复（reflector.go:306-311）。勘察材料的「恰 2 次」对应**现状**（1 reflect + 1 修复），勘正落账。现有断言无冲突：`TestReflectorRepairsInvalidJSON` 的 2 次断言属非法 JSON 用例（新行为下 parse 失败仍 2 次，不变）；`TestReflectorEmptyResponseShortCircuits` 测空 raw 非空池（:299-304 守卫不动）。

测试切口：reflector_mutator_test.go 加「reflect 返回空池 → Reflect 返 (nil,nil)，恰好 1 次调用、0 次修复」与「reflect 返回全 `{input}` 字面量池 → 同上」两用例；gepa_test.go 用 startOptLLM 标记路由 + goldenRequest 造全轮空池（两形态各一）变体：断言 ReasonRoundsDone、Best=baseline、每轮 EventRoundDone skipped 带「没有可验证的假设」、opt-calls = 轮数×1。

**范围纪律（R1 #12）**：D9① 改的是优化器每轮调用语义，与配置面零耦合——**独立 commit**（可独立成 PR），不得搭配置面 PRD 的便车过审；评审以本节测试切口为准。

### ② spec 指标钉死（缺陷，= D7）

D7 即修复设计。勘察给出的插入点与双表互动分析本会话逐行核实无误。

### ③ 裁判记账链（非缺陷，实证成立；部署实测断言清单）

勘察实跑 `go test ./internal/eval/ -run 'Judge|Decision' -v` 19 项全 PASS（勘察材料）。**本 PRD 不改代码**。部署（tcmsp-30）实测按四条断言清单执行：① summary.json/run.json `usage_by_role.judge` 存在且 tokens>0 + `metric_means.llm_judge` 存在；② calls/NNN-*.json 含 `"role":"judge"` 且 `"stage":"judge"|"judge-decision"`；③ samples/NNN-*.json judge_ms>0 且 usage 严大于执行器单边；④ events.jsonl run_done 带 usage_by_role.judge。caveat：usage_by_role.judge 是共享 Budget 跨单元累计，单条 call trace 才是逐调用证据。

## 实施切分与验收清单

1. **前置重构（R1 #6）+ 文件层**：抽取 `validateProvider/validateOptimizer/validateJudgeBackend/validateJudgeProvider/validateTimeout/validateMetricsList` 等 helper，parseRunFlags 改调 helper（**行为保持**，验收钉错误文案与 errors.Join 顺序快照）；文件层 resolver fileVal 参数化 + `internal/config/file.go`（Load/Discover/KnownFields/shadowed 仅存在性检测）+ 显式性清单 10 键（fs.Visit 推广，**由 per-key 来源收集器派生**，R2 #5）+ verify 连接身份白名单（11 键，R2 #1）+ `--config`/`PROMPTOPT_CONFIG`。验收：D2 三层字节级不变（run 语义限定，R1 #13，D5 文案例外）+ D1 遮蔽测试切口（GOOS 感知，R2 #6②）+ R1 #2 显式 0 测试切口（文件 confidence=0.7 + argv 0 → 生效 0）+ R2 #1 verify 补位切口（manifest 无 extra_body + 文件有 → 请求不带）+ **R2 #6③ 反射完整性测试**（config_test 反射 File 结构体 yaml tag，断言每字段在合并登记表登记——登记表含两列：是否需显式性标记、是否 verify 连接白名单（R2 #1 的 11 键清单即登记表的一列，同样测试覆盖）——新增文件键忘登记即测试红）全绿。
2. **config 子命令**：init/list/get/set/unset（set 仅单键校验，复用切分 1 helper）+ D5 报错升级 + list 路径/遮蔽可见性 + stdin `-` 密钥通道。验收：向导非 tty httptest 覆盖（含 R2 #2 `-` 前置读取顺序切口：`init --api-key -` 喂 `"key\nhttp://x\nm1\n"` 断言三键各就各位）；**缺参文案断言（R2 #3：D5 新文案快照钉死）**；init 覆盖保护切口（已存在文件非 tty 无 --force → 拒绝退出 1，R2 #4）；set/unset 损坏文件切口（严格解码报错、文件不变，R2 #4）；list 输出通道（人类 stderr / --headless JSON stdout）与掩码断言；权限位测试（0600 不告警/0644 告警）。
3. **spec-metrics**：flag + config 键 + Pipeline.Run 覆盖点 + manifest 字段 + configured 模式三键 stderr 提示。验收：白名单校验、primary 回落、样本合成吃新指标、llm_judge 规格端到端可达。
4. **Web settings 页**：只读页 + `config.PublishSnapshot`（**单一 per-key 来源收集器**，显式性判定派生自 Source==flag，禁止并存第二套 bool——R2 #5 复用约束）+ index 入口。验收：run --web 模式快照含 flag 来源且「文件 0.7 + flag 0 → 页面显示 0」断言；serve 模式回落全局链；httptest 桩断言页 200、掩码出现、无写端点路由。
5. **D9① reflector（独立 commit，可独立 PR）**：D9① 改动 + 四个测试切口（空池/全清洗池 × reflector/gepa）。验收：gepa 全空池变体断言全绿；既有 TestReflector* 不回退。
6. **文档**：AGENTS.md:36 架构图命令清单与 :70 命令面 bullet 补 config 子命令与文件层（R2 后勘正：初稿所记 :23 已漂移——:23 是「可干预」Web 仪表盘 bullet，实施会话 `grep 'run / serve / verify' AGENTS.md` 定位为准）；README「验证、回退与审计」下 config 小节与 Web 仪表盘小节；**输出约定不需修订**（list 人类 stderr / headless JSON stdout 已合既有章程，R1 #4 落账）；CHANGELOG 开 V7 子节补账（含既有文档债：grep -c "V7" = 0）。
7. **部署与提交**：tcmsp-30 部署 + D9③ 四断言清单实测 + smoke 脚本；全量 `go test ./... -race`、`go vet`、staticcheck/modernize 五门禁绿后提交推送（用户指令：部署测试通过后及时提交推送）。

## Out of Scope

- serve 的文件层（serve 无连接参数；本轮不动）。
- Web 配置写端点（红队裁决不加；条件留作未来门槛，D6）。
- `--spec-primary`、合成温度面（D7 论证不做）。
- 跨文件合并、配置热重载、toml/json 格式。
- anchor list 输出通道迁移（数据走 stdout 的章程化——涉及既有命令行为变更，不在本 PRD；config list 已按先例走 stderr）。
- 池准入去重、池 UI（提案 §3.3 最小版边界不动）。

## Open Questions

（无——八项决策 + R1 十三点攻击全部钉死；写端点已经红队裁决关闭。）

// 设置引导页（PRD-0001 切分 4 / D6）：只读 GET /settings——生效配置表
// （值 + 来源 + flag）、必须参数状态、按缺口生成的可复制 YAML 片段与等
// 价命令行（与 D5 缺参报错的三途径指路同语义）、api_key/judge_api_key
// 恒掩码（config.MaskText）。
//
// 数据源双模式（R1 #7），各有真相：
//   - run --web：startSink 起看板前 config.PublishSnapshot 发布的包级原
//     子快照优先——快照携带合并后 runOptions 的逐键值与来源（含 flag
//     层），页面值即该 run 实际生效值（「文件 0.7 + flag 显式 0」显示
//     0，不显示 0.7）。
//   - serve：无快照回落 Discover+env 全局链（与 config list 同一
//     EffectiveRows 同源口径），页面注明语义是「当前环境」的配置而非某
//     次 run 的现场（历史 run 的现场归各自 manifest 快照）。
//
// 红队裁决（R1 #7 落账）：不加配置写端点——本页只读，POST 无路由；改
// 配置走 promptopt config 子命令。originGuard 照常罩护本路由。
package web

import (
	"fmt"
	"net/http"

	"github.com/ByronFinn/PromptOpt/internal/config"
)

// registerSettingsRoutes mounts the read-only settings page（Handler 分
// 层先例：registerDashboardRoutes / registerSynthRoutes 同构）。synthDir
// 空 / bus nil 均不影响本页挂载。
func (s *Server) registerSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings", s.handleSettings)
}

// settingsView backs settings.html.
type settingsView struct {
	// Snapshot marks the data source: true = 本次 run 的快照（含 flag 来
	// 源）；false = serve 回落的「当前环境」链。
	Snapshot bool
	// Note 是页顶语义横幅：两种模式各有措辞，serve 模式必须含「当前环
	// 境」语义（验收切口）。
	Note string
	// HitPath / Shadowed 是发现序命中现场（R1 #10 来源可见性——与
	// config list 同源展示）。
	HitPath  string
	Shadowed []string
	// Error 非空 = Discover 硬错（命中文件损坏）：页面诚实展示错误与出
	// 路——run/verify 会因同一文件硬错退出 1（D1 语义，引导而非掩盖）。
	Error string
	Keys  []settingsKeyRow
	// Required 是必须参数状态（base_url/model，D3 表「无（必填）」两键）。
	Required []requiredStatus
	Gaps     []gapGuide
}

// settingsKeyRow 是生效配置表的一行（值已展示化：掩码/-）。
type settingsKeyRow struct{ Key, Value, Source, Flag string }

// requiredStatus 是一个必须参数的就位状态。
type requiredStatus struct {
	Key, Value string
	OK         bool
}

// gapGuide 是一个缺口的引导卡片：三种途径的可复制内容（与 D5 报错文案
// 同语义——flag / 环境变量 / 配置文件）。
type gapGuide struct {
	Key, Flag, EnvKey string
	Command           string // ① 命令行 flag 途径
	Export            string // ② 环境变量途径
	YAML              string // ③ 配置文件片段（promptopt.yaml）
	Wizard            string // ③ 配套向导/键指引
}

// handleSettings renders the read-only page. 损坏文件不 500：把错误摆上
// 页面正是引导职责（命中文件损坏在 run/verify 侧是硬错退出 1）。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	render(w, "settings.html", s.settingsPage())
}

// settingsPage assembles the view: run --web 的快照优先，serve 回落全局
// 发现链。
func (s *Server) settingsPage() settingsView {
	if snap := config.CurrentSnapshot(); snap != nil {
		v := newSettingsView(snap.Keys, true)
		v.Note = "本次 run 的生效配置快照——值与来源即该 run 实际使用的合并结果（含命令行 flag 层）。页面只读；修改配置请用 promptopt config 子命令。"
		v.HitPath, v.Shadowed = snap.HitPath, snap.Shadowed
		return v
	}
	disc, err := config.Discover("")
	if err != nil {
		return settingsView{
			Note:  "当前环境的配置引导页。",
			Error: err.Error(),
		}
	}
	v := newSettingsView(config.EffectiveRows(disc), false)
	v.Note = "当前环境的配置（发现序命中文件 + 环境变量 + 默认值）——不是某次 run 的现场，历史 run 的现场以各自 manifest 快照为准。页面只读；修改配置请用 promptopt config 子命令。"
	v.HitPath, v.Shadowed = disc.Path, disc.Shadowed
	return v
}

// newSettingsView converts the per-key rows into the display view and
// derives the required-param status plus the gap guidance.
func newSettingsView(rows []config.SnapshotKey, snapshot bool) settingsView {
	v := settingsView{Snapshot: snapshot}
	value := func(k config.SnapshotKey) string {
		s := fmt.Sprintf("%v", k.Value)
		if s == "" {
			return "-"
		}
		return s
	}
	for _, k := range rows {
		val := value(k)
		v.Keys = append(v.Keys, settingsKeyRow{Key: k.Key, Value: val, Source: string(k.Source), Flag: k.Flag})
		switch k.Key {
		case "base_url", "model":
			v.Required = append(v.Required, requiredStatus{Key: k.Key, Value: val, OK: val != "-"})
		}
	}
	v.Gaps = gapGuides(rows, value)
	return v
}

// gapGuides 生成缺口引导（D5 三途径同语义）：base_url/model 缺一即出卡
// 片；judge_backend=decision 且连接字段缺一时出组合卡片（跨键完整性的
// 报错落点在 run/verify 汇聚点，这里只指路）。
func gapGuides(rows []config.SnapshotKey, value func(config.SnapshotKey) string) []gapGuide {
	byKey := make(map[string]config.SnapshotKey, len(rows))
	for _, k := range rows {
		byKey[k.Key] = k
	}
	set := func(key string) bool {
		k, ok := byKey[key]
		return ok && value(k) != "-"
	}
	var gaps []gapGuide
	if !set("base_url") {
		gaps = append(gaps, gapGuide{
			Key:     "base_url",
			Flag:    "--base-url",
			EnvKey:  config.EnvBaseURL,
			Command: "promptopt run --base-url http://localhost:8080/v1 ...",
			Export:  "export PROMPTOPT_BASE_URL=http://localhost:8080/v1",
			YAML:    "base_url: http://localhost:8080/v1",
			Wizard:  "promptopt config init（或手写 promptopt.yaml）",
		})
	}
	if !set("model") {
		gaps = append(gaps, gapGuide{
			Key:     "model",
			Flag:    "--model",
			EnvKey:  config.EnvModel,
			Command: "promptopt run --model <model-name> ...",
			Export:  "export PROMPTOPT_MODEL=<model-name>",
			YAML:    "model: <model-name>",
			Wizard:  "promptopt config init（或手写 promptopt.yaml）",
		})
	}
	if value(byKey["judge_backend"]) == "decision" && (!set("judge_decision_url") || !set("judge_decision_model")) {
		gaps = append(gaps, gapGuide{
			Key:     "judge_decision_url / judge_decision_model",
			Flag:    "--judge-decision-url  --judge-decision-model",
			EnvKey:  config.EnvJudgeDecisionURL + " / " + config.EnvJudgeDecisionModel,
			Command: "promptopt run --judge-decision-url http://decision-svc:9000 --judge-decision-model <model>",
			Export:  "export PROMPTOPT_JUDGE_DECISION_URL=... PROMPTOPT_JUDGE_DECISION_MODEL=...",
			YAML:    "judge_decision_url: http://decision-svc:9000\njudge_decision_model: <model>",
			Wizard:  "promptopt config set judge_decision_url <url> 与 judge_decision_model <model>",
		})
	}
	return gaps
}

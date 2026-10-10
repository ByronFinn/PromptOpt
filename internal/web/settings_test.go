package web

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/config"
)

// --- 设置引导页（PRD-0001 切分 4 / D6）----------------------------------------
//
// httptest 桩（NewServer(runsDir, "", nil)——synthDir 空 / bus nil 不影
// 响挂载），绝不触网。快照位是包级原子（config.PublishSnapshot）：发布
// 快照的用例必须经 cleanup 复位，防泄漏进同包其它用例。

// isolateSettingsEnv 把环境收敛到「无任何配置文件、无任何 PROMPTOPT_*
// env」的确定性状态（与 cmd 侧 isolateRunConfigEnv 同构）：临时工作目
// 录、临时 HOME（darwin 用户级路径随之落空）、空 XDG_CONFIG_HOME、清
// 空发现层 env 与全部 PROMPTOPT_* env。t.Chdir 不得与 t.Parallel 同用。
func isolateSettingsEnv(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		config.EnvBaseURL, config.EnvModel, config.EnvAPIKey, config.EnvOutDir,
		config.EnvTimeout, config.EnvJudgeProvider, config.EnvJudgeModel,
		config.EnvJudgeBaseURL, config.EnvJudgeAPIKey, config.EnvJudgeMaxTokens,
		config.EnvJudgeDecisionURL, config.EnvJudgeDecisionModel,
	} {
		t.Setenv(k, "")
	}
}

// TestSettingsServeFallback is the serve-side cut (D6 R1 #7 修订的回落
// 半边）：无快照时 handler 走 Discover+env 全局链，页面注明「当前环境」
// 语义；synthDir 空 / bus nil 不影响挂载；api_key/judge_api_key 恒掩
// 码；逐键来源标注与对应 flag 名出现；缺 model 时缺口引导给可复制
// YAML 片段与等价命令行（D5 三途径同语义），已就位的 base_url 不出卡。
func TestSettingsServeFallback(t *testing.T) {
	isolateSettingsEnv(t)
	// env 层供一个非密钥键：来源列必须如实标 env（标注与解析链同一收集
	// 器——页面不可能与链漂移）；base_url 就位后必须参数表走「已配置」
	// 态、缺口卡只剩 model。
	t.Setenv(config.EnvBaseURL, "http://env/v1")

	res := get(t, mustURL(t, NewServer(t.TempDir(), "", nil).Handler(), "/settings"))

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200", res.StatusCode)
	}
	if n := strings.Count(res.Body, "<style>"); n != 1 {
		t.Errorf("settings page carries %d <style> blocks, want 1 (每页一块惯例)", n)
	}
	// serve 语义横幅：必须是「当前环境」而非某次 run 的现场。
	if !strings.Contains(res.Body, "当前环境") {
		t.Errorf("serve-mode page lacks the 当前环境 semantics note")
	}
	if strings.Contains(res.Body, "生效配置快照") {
		t.Errorf("serve-mode page must not claim a run snapshot: %s", noteLine(res.Body))
	}
	// 恒掩码（D8①）：api_key 默认值为 "1"（本地网关约定）也必须掩码，
	// judge_api_key 无值同样掩码展示。
	if !strings.Contains(res.Body, config.MaskText) {
		t.Errorf("page lacks the %q mask for api_key/judge_api_key", config.MaskText)
	}
	if strings.Contains(res.Body, ">1</td>") {
		t.Errorf("api_key default value \"1\" leaked unmasked")
	}
	// 逐键来源与 flag 名：env 层的 base_url 标 env；default 层的 provider
	// 标 default；flag 拼写列恒出现。
	if !strings.Contains(res.Body, "--base-url") || !strings.Contains(res.Body, "--judge-decision-model") {
		t.Errorf("page lacks flag-spelling columns (--base-url / --judge-decision-model)")
	}
	for _, row := range []string{"<td class=\"mono\">base_url</td><td class=\"mono\">http://env/v1</td><td>env</td>",
		"<td class=\"mono\">provider</td><td class=\"mono\">openai</td><td>default</td>"} {
		if !strings.Contains(res.Body, row) {
			t.Errorf("page lacks effective-config row %q", row)
		}
	}
	// 必须参数状态：model 无值 → 缺失。
	if !strings.Contains(res.Body, "缺失") {
		t.Errorf("page lacks the missing-required marker for model")
	}
	// 缺口引导（D5 三途径同语义）：model 的可复制 YAML 片段与等价命令
	// 行；已就位的 base_url 不出卡。
	for _, want := range []string{
		"model: &lt;model-name&gt;",
		"export PROMPTOPT_MODEL=&lt;model-name&gt;",
		"promptopt config init",
		"promptopt run --model",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("gap guidance lacks %q", want)
		}
	}
	if strings.Contains(res.Body, "base_url: http://localhost:8080/v1") {
		t.Errorf("configured base_url must not get a gap card")
	}
}

// TestSettingsReadOnlyNoWriteRoute pins the red-team ruling (R1 #7): the
// page is read-only — POST /settings has no route (the method-scoped
// pattern answers 405, not a handler).
func TestSettingsReadOnlyNoWriteRoute(t *testing.T) {
	isolateSettingsEnv(t)
	url := mustURL(t, NewServer(t.TempDir(), "", nil).Handler(), "/settings")

	res, err := http.Post(url, "application/x-www-form-urlencoded", strings.NewReader("base_url=http://x"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed && res.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /settings = %d, want 405 (method-scoped pattern) or 404", res.StatusCode)
	}
}

// TestSettingsSnapshotOverridesChain is the run --web half of the D6
// data-source split: a published snapshot wins over the global chain,
// its per-key rows carry the MERGED values (the run's actual effective
// configuration) with flag sources included. Rows are built through the
// same config.NewSnapshotKey the run side uses — the display cannot
// disagree with the merge.
func TestSettingsSnapshotOverridesChain(t *testing.T) {
	isolateSettingsEnv(t)
	// 全局链上有 env 值：快照在场时页面必须显示快照值而非链值。
	t.Setenv(config.EnvModel, "env-m")
	config.PublishSnapshot(&config.Snapshot{
		HitPath: "/tmp/fake/promptopt.yaml",
		Keys: []config.SnapshotKey{
			config.NewSnapshotKey("base_url", "http://snap/v1", config.SourceFlag),
			config.NewSnapshotKey("model", "snap-m", config.SourceEnv),
			config.NewSnapshotKey("api_key", "supersecret", config.SourceEnv),
			// R1 #7 反面用例的 handler 半边：合并后值 0（文件 0.7 被显式
			// flag 0 压过）+ 来源 flag——页面必须显示 0，不是 0.7。
			config.NewSnapshotKey("judge_decision_confidence", float64(0), config.SourceFlag),
			config.NewSnapshotKey("provider", "", config.SourceDefault),
		},
	})
	t.Cleanup(func() { config.PublishSnapshot(nil) })

	res := get(t, mustURL(t, NewServer(t.TempDir(), "", nil).Handler(), "/settings"))

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200", res.StatusCode)
	}
	// 快照语义横幅在、serve 语义不在。
	if !strings.Contains(res.Body, "生效配置快照") {
		t.Errorf("snapshot page lacks the snapshot note")
	}
	if strings.Contains(res.Body, "当前环境") {
		t.Errorf("snapshot page must not carry the serve-mode note")
	}
	// 命中现场（R1 #10）随快照可见。
	if !strings.Contains(res.Body, "/tmp/fake/promptopt.yaml") {
		t.Errorf("snapshot page lacks the hit path")
	}
	for _, row := range []string{
		"<td class=\"mono\">base_url</td><td class=\"mono\">http://snap/v1</td><td>flag</td>",
		"<td class=\"mono\">model</td><td class=\"mono\">snap-m</td><td>env</td>",
		// env 里躺过 supersecret：快照行也必须掩码。
		"<td class=\"mono\">api_key</td><td class=\"mono\">" + config.MaskText + "</td><td>env</td>",
		// R1 #7：合并值 0 + 来源 flag（不是文件值 0.7）。
		"<td class=\"mono\">judge_decision_confidence</td><td class=\"mono\">0</td><td>flag</td>",
	} {
		if !strings.Contains(res.Body, row) {
			t.Errorf("snapshot page lacks row %q", row)
		}
	}
	if strings.Contains(res.Body, "0.7") || strings.Contains(res.Body, "supersecret") || strings.Contains(res.Body, "env-m") {
		t.Errorf("snapshot page leaked chain values (0.7 / secret / env-m)")
	}
	// base_url 已就位：必须参数表不得再报缺失、缺口卡不得出现。
	if strings.Contains(res.Body, "缺失") || strings.Contains(res.Body, "缺口引导") {
		t.Errorf("fully-configured snapshot must not show missing-required or gap cards")
	}
}

// TestSettingsCorruptFileShowsError pins the honest-error path: a
// corrupt discovered file hard-errors run/verify (D1), so the page must
// surface the error instead of 500-ing or silently showing defaults.
func TestSettingsCorruptFileShowsError(t *testing.T) {
	isolateSettingsEnv(t)
	writeFileT(t, "promptopt.yaml", "base_url: [unclosed\n")

	res := get(t, mustURL(t, NewServer(t.TempDir(), "", nil).Handler(), "/settings"))

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200 (corrupt file renders an error page, not a 500)", res.StatusCode)
	}
	if !strings.Contains(res.Body, "配置文件读取失败") || !strings.Contains(res.Body, config.ProjectFileName) {
		t.Errorf("page lacks the corrupt-file error banner: %s", noteLine(res.Body))
	}
}

// noteLine extracts the subtitle line (the page Note) for failure
// messages — keeps assertion output readable.
func noteLine(body string) string {
	if !strings.Contains(body, "<p class=\"sub\">") {
		return body[:min(len(body), 200)]
	}
	i := strings.Index(body, "<p class=\"sub\">") + len("<p class=\"sub\">")
	if j := strings.Index(body[i:], "</p>"); j >= 0 {
		return body[i : i+j]
	}
	return body[i : i+min(200, len(body)-i)]
}

// writeFileT is the settings tests' tiny file fixture helper (cmd 侧
// writeRunFile 同构，web 包不依赖 cmd）。
func writeFileT(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

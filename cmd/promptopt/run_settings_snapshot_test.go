package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/web"
)

// --- 设置页快照切口（PRD-0001 切分 4 / D6，R1 #7 反面用例全链钉死）----------
//
// 「文件 judge_decision_confidence=0.7 + argv 显式 0」→ 合并生效 0 →
// run --web 起看板前 runConfigSnapshot 发布 → 页面必须显示 0 且来源
// flag。页面显示文件的 0.7 = 页面撒谎（run 实际用 0），此测试钉死它不
// 可能发生。快照与 P1 单一收集器同一张表（o.sources），无第二套 bool。

func TestRunSettingsSnapshotShowsMergedTruth(t *testing.T) {
	isolateRunConfigEnv(t)
	task, cand, ds := fixtureYAMLs(t)
	writeRunFile(t, config.ProjectFileName, `base_url: http://file/v1
model: file-m
api_key: file-secret
judge_decision_confidence: 0.7
`, 0o600)

	o, err := parseRunFlags([]string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--judge-decision-confidence", "0"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 合并层真相（P1 单一收集器的判定，快照只是搬运它）。
	if o.judgeDecisionConfidence != 0 {
		t.Errorf("merged judge_decision_confidence = %v, want 0 (explicit flag beats file 0.7)", o.judgeDecisionConfidence)
	}
	if o.sources["judge_decision_confidence"] != config.SourceFlag {
		t.Errorf("sources[judge_decision_confidence] = %s, want flag", o.sources["judge_decision_confidence"])
	}

	// run --web 起看板前的发布切口（startSink 同一调用序列）。
	config.PublishSnapshot(runConfigSnapshot(o))
	t.Cleanup(func() { config.PublishSnapshot(nil) })

	ts := httptest.NewServer(web.NewServer(t.TempDir(), "", nil).Handler())
	t.Cleanup(ts.Close)
	res, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d", res.StatusCode)
	}

	// 快照语义（不是 serve 的「当前环境」回落）。
	if !strings.Contains(page, "生效配置快照") || strings.Contains(page, "当前环境") {
		t.Errorf("page must present the run-snapshot semantics, got note: %.200s", page)
	}
	// R1 #7：页面显示合并值 0 + 来源 flag，绝不显示文件的 0.7。
	if !strings.Contains(page, "<td class=\"mono\">judge_decision_confidence</td><td class=\"mono\">0</td><td>flag</td>") {
		t.Error("page must show judge_decision_confidence = 0 with flag source (R1 #7)")
	}
	if strings.Contains(page, "0.7") {
		t.Error("page leaked the file value 0.7 that the explicit flag overrode — the page lied about the run")
	}
	// D8①：文件携带的 api_key 进了 runOptions，也必须恒掩码。
	if strings.Contains(page, "file-secret") {
		t.Error("page leaked the raw api_key")
	}
	if !strings.Contains(page, config.MaskText) {
		t.Errorf("page lacks the %q mask", config.MaskText)
	}
	// 命中现场（R1 #10）随快照可见。
	if !strings.Contains(page, config.ProjectFileName) {
		t.Errorf("page lacks the config hit path %s", config.ProjectFileName)
	}
}

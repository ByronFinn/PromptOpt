package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
)

// traceUnitDir resolves one trace unit to its directory:
//
//	unit ""         → runs/<id>            (manual mode / zero-config baseline)
//	unit "opt-calls" → runs/<id>/opt-calls (optimizer-side dials)
//	unit <name>      → runs/<id>/evals/<name> (one optimization evaluation unit)
func traceUnitDir(runDir, unit string) string {
	if unit == "" {
		return runDir
	}
	if unit == "opt-calls" {
		return filepath.Join(runDir, "opt-calls")
	}
	return filepath.Join(runDir, "evals", unit)
}

// traceUnitView is one unit tab.
type traceUnitView struct {
	Name  string // "" renders as 顶层
	Label string
}

// traceRowView is one row of the sample/call file table.
type traceRowView struct {
	File, Sample                   string
	Status                         string
	StatusClass                    string
	Scores, Tokens, Latency, Error string
}

// traceDetailRow is one label/value line of the detail view.
type traceDetailRow struct{ K, V string }

// tracePageView backs trace.html.
type tracePageView struct {
	ID      string
	Units   []traceUnitView
	Unit    string
	Kind    string // samples | calls (opt-calls renders as calls)
	IsOpt   bool
	Rows    []traceRowView
	Detail  []traceDetailRow
	File    string
	Dataset map[string]core.Sample // sample id → joined dataset row
}

// handleTracePage serves the trace browser:
// /runs/{id}/trace?unit=&kind=&file=. unit and file names go through
// the same single-segment validation as run ids, so path traversal is
// rejected before any filesystem access.
func (s *Server) handleTracePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	runDir := filepath.Join(s.runsDir, id)
	if _, err := os.Stat(runDir); err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	unit, kind, file := q.Get("unit"), q.Get("kind"), q.Get("file")
	if unit != "" && unit != "opt-calls" && !safeRunID(unit) {
		http.NotFound(w, r)
		return
	}
	if file != "" && !safeRunID(file) {
		http.NotFound(w, r)
		return
	}
	if kind != "calls" {
		kind = "samples"
	}

	v := tracePageView{ID: id, Unit: unit, Kind: kind}
	v.Units = []traceUnitView{{Name: "", Label: "顶层（baseline/手动模式）"}}
	if entries, err := os.ReadDir(filepath.Join(runDir, "evals")); err == nil {
		for _, e := range entries {
			if e.IsDir() && safeRunID(e.Name()) {
				v.Units = append(v.Units, traceUnitView{Name: e.Name(), Label: e.Name()})
			}
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "opt-calls")); err == nil {
		v.Units = append(v.Units, traceUnitView{Name: "opt-calls", Label: "opt-calls（优化侧调用）"})
	}
	if slices.IndexFunc(v.Units, func(u traceUnitView) bool { return u.Name == unit }) < 0 {
		http.NotFound(w, r)
		return
	}

	v.Dataset = loadTraceDataset(runDir)
	unitDir := traceUnitDir(runDir, unit)
	v.IsOpt = unit == "opt-calls"
	if v.IsOpt {
		v.Rows = listOptCalls(unitDir)
		if file != "" {
			v.Detail = readOptCall(unitDir, file)
			v.File = file
		}
	} else {
		v.Rows = listTraces(unitDir, kind)
		if file != "" {
			v.Detail = readTraceDetail(unitDir, kind, file, v.Dataset)
			v.File = file
		}
	}
	render(w, "trace.html", v)
}

// loadTraceDataset joins sample ids against runs/<id>/dataset.json.
func loadTraceDataset(runDir string) map[string]core.Sample {
	b, err := os.ReadFile(filepath.Join(runDir, "dataset.json"))
	if err != nil {
		return nil
	}
	var ds core.Dataset
	if err := json.Unmarshal(b, &ds); err != nil {
		return nil
	}
	m := make(map[string]core.Sample, len(ds.Samples))
	for _, s := range ds.Samples {
		m[s.ID] = s
	}
	return m
}

// listTraces enumerates samples/*.json or calls/*.json of one unit.
func listTraces(unitDir, kind string) []traceRowView {
	dir := filepath.Join(unitDir, kind)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var rows []traceRowView
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		row := traceRowView{File: e.Name(), Status: "—", StatusClass: "dim"}
		if kind == "samples" {
			var t core.SampleTrace
			if readTraceFile(filepath.Join(dir, e.Name()), &t) {
				row.Sample = t.SampleID
				fillSampleRow(&row, t)
			}
		} else {
			var c eval.CallTrace
			if readTraceFile(filepath.Join(dir, e.Name()), &c) {
				row.Sample = c.SampleID
				fillCallRow(&row, c)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// listOptCalls enumerates NNN-stage.json optimizer dials.
func listOptCalls(unitDir string) []traceRowView {
	entries, err := os.ReadDir(unitDir)
	if err != nil {
		return nil
	}
	var rows []traceRowView
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		row := traceRowView{File: e.Name(), Status: "—", StatusClass: "dim"}
		var c eval.CallTrace
		if readTraceFile(filepath.Join(unitDir, e.Name()), &c) {
			row.Sample = c.SampleID // the stage name
			fillCallRow(&row, c)
		}
		rows = append(rows, row)
	}
	return rows
}

func fillSampleRow(row *traceRowView, t core.SampleTrace) {
	if t.Error != "" {
		row.Status, row.StatusClass = "失败", "bad"
		row.Error = t.Error
	} else {
		row.Status, row.StatusClass = "完成", "ok"
	}
	row.Scores = joinScores(t.Scores)
	row.Tokens = fmt.Sprintf("%d+%d", t.Usage.PromptTokens, t.Usage.CompletionTokens)
	if t.DurationMS > 0 {
		row.Latency = fmt.Sprintf("%.1fs", float64(t.DurationMS)/1000)
	}
}

func fillCallRow(row *traceRowView, c eval.CallTrace) {
	if c.Error != "" {
		row.Status, row.StatusClass = "失败", "bad"
		row.Error = c.Error
	} else if c.Response.Content != "" {
		row.Status, row.StatusClass = "完成", "ok"
	} else {
		row.Status, row.StatusClass = "空响应", "warn"
	}
	row.Tokens = fmt.Sprintf("%d+%d", c.Response.Usage.PromptTokens, c.Response.Usage.CompletionTokens)
	if c.LatencyMS > 0 {
		row.Latency = fmt.Sprintf("%.1fs", float64(c.LatencyMS)/1000)
	}
}

// readTraceFile decodes one trace file, ignoring malformed entries.
func readTraceFile(path string, dst any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

// readTraceDetail renders one sample or call detail as label/value
// rows, joining the dataset for input/expected when available.
func readTraceDetail(unitDir, kind, file string, dataset map[string]core.Sample) []traceDetailRow {
	if kind == "samples" {
		var t core.SampleTrace
		if !readTraceFile(filepath.Join(unitDir, "samples", file), &t) {
			return nil
		}
		rows := []traceDetailRow{{K: "样本", V: t.SampleID}}
		if s, ok := dataset[t.SampleID]; ok {
			rows = append(rows,
				traceDetailRow{K: "输入（dataset.json）", V: s.Input},
				traceDetailRow{K: "期望（dataset.json）", V: expectedJSON(s.Expected)},
			)
		} else {
			rows = append(rows, traceDetailRow{K: "输入/期望", V: "（dataset.json 缺失或样本不在其中）"})
		}
		rows = append(rows, traceDetailRow{K: "状态", V: traceStatus(t.Error)})
		rows = append(rows, traceDetailRow{K: "prompt", V: t.Prompt})
		if t.Reasoning != "" {
			rows = append(rows, traceDetailRow{K: "reasoning", V: t.Reasoning})
		}
		rows = append(rows, traceDetailRow{K: "response", V: t.Response})
		if sc := joinScores(t.Scores); sc != "" {
			rows = append(rows, traceDetailRow{K: "得分", V: sc})
		}
		for _, cell := range diagnosisCells(t.Diagnosis) {
			rows = append(rows, traceDetailRow{K: "诊断 " + cell.Name, V: cell.Value})
		}
		rows = append(rows,
			traceDetailRow{K: "tokens", V: fmt.Sprintf("%d+%d", t.Usage.PromptTokens, t.Usage.CompletionTokens)},
			traceDetailRow{K: "延迟", V: fmt.Sprintf("%.1fs", float64(t.DurationMS)/1000)},
		)
		if t.Error != "" {
			rows = append(rows, traceDetailRow{K: "错误", V: t.Error})
		}
		return rows
	}
	var c eval.CallTrace
	if !readTraceFile(filepath.Join(unitDir, "calls", file), &c) {
		return nil
	}
	return callDetailRows(c, dataset)
}

// readOptCall renders one optimizer dial trace.
func readOptCall(unitDir, file string) []traceDetailRow {
	var c eval.CallTrace
	if !readTraceFile(filepath.Join(unitDir, file), &c) {
		return nil
	}
	return callDetailRows(c, nil)
}

// callDetailRows renders one LLM call trace: request messages,
// finish_reason, response and error.
func callDetailRows(c eval.CallTrace, dataset map[string]core.Sample) []traceDetailRow {
	rows := []traceDetailRow{
		{K: "样本/阶段", V: c.SampleID},
		{K: "角色", V: string(c.Role)},
		{K: "模型", V: fmt.Sprintf("%s（max_tokens=%d）", c.Request.Model, c.Request.MaxTokens)},
	}
	if s, ok := dataset[c.SampleID]; ok {
		rows = append(rows,
			traceDetailRow{K: "输入（dataset.json）", V: s.Input},
			traceDetailRow{K: "期望（dataset.json）", V: expectedJSON(s.Expected)},
		)
	}
	var msgs strings.Builder
	for i, m := range c.Request.Messages {
		fmt.Fprintf(&msgs, "[%s]\n%s", m.Role, m.Content)
		if i < len(c.Request.Messages)-1 {
			msgs.WriteString("\n\n")
		}
	}
	rows = append(rows, traceDetailRow{K: "请求消息", V: msgs.String()})
	rows = append(rows, traceDetailRow{K: "finish_reason", V: cmp.Or(c.Response.FinishReason, "—")})
	if c.Response.ReasoningContent != "" {
		rows = append(rows, traceDetailRow{K: "reasoning", V: c.Response.ReasoningContent})
	}
	rows = append(rows, traceDetailRow{K: "response", V: c.Response.Content})
	rows = append(rows, traceDetailRow{K: "tokens", V: fmt.Sprintf("%d+%d",
		c.Response.Usage.PromptTokens, c.Response.Usage.CompletionTokens)})
	rows = append(rows, traceDetailRow{K: "延迟", V: fmt.Sprintf("%.1fs", float64(c.LatencyMS)/1000)})
	if c.Error != "" {
		rows = append(rows, traceDetailRow{K: "错误", V: c.Error})
	}
	return rows
}

func traceStatus(errMsg string) string {
	if errMsg != "" {
		return "失败：" + errMsg
	}
	return "完成"
}

func joinScores(scores map[string]float64) string {
	names := make([]string, 0, len(scores))
	for m := range scores {
		names = append(names, m)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, m := range names {
		parts = append(parts, fmt.Sprintf("%s=%.4f", m, scores[m]))
	}
	return strings.Join(parts, " ")
}

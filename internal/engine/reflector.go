package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Markers embedded in the optimizer meta-prompts — the seam fake LLM
// servers key on to tell reflection and mutation stages apart
// (evaluation prompts never contain them).
const (
	MarkerReflect   = "生成优化假设"
	MarkerRewrite   = "改写提示词"
	MarkerMerge     = "合并两个提示词"
	MarkerFresh     = "重新设计提示词"
	MarkerHypRepair = "修复为合法的假设 JSON"
	MarkerCandFix   = "修复为包含占位符的提示词 JSON"
)

// Context truncation bounds (runes). They keep the reflection input
// far below the model context (jiuwei-tcm serves n_ctx=20736): with
// defaults a 4-sample batch stays well under ~6k characters.
const (
	maxSampleInputRunes  = 600
	maxResponseRunes     = 600
	maxHypoTextRunes     = 400
	maxParentPromptRunes = 3000
)

// Hypothesis is one reflection output: a natural-language guess about
// what to change, the samples it came from and a confidence.
type Hypothesis struct {
	ID         string   `json:"id"`
	Text       string   `json:"text"`
	SampleIDs  []string `json:"sample_ids,omitempty"`
	Confidence float64  `json:"confidence"`
}

// Reflector turns a parent's minibatch evidence into at most n
// hypotheses via one optimizer-role call (plus at most one repair).
type Reflector struct {
	adv *Advisor
}

// NewReflector returns a reflector dialing through adv.
func NewReflector(adv *Advisor) *Reflector { return &Reflector{adv: adv} }

// Reflect builds the reflection context (task spec, parent prompt,
// per-sample input/expected/response/scores/ASI diagnosis, ancestor
// lessons) and parses the hypothesis pool.
//
// normalize 后为空的池（空数组或全 {input} 字面量清洗池）是模型的有意义
// 回答而非协议错误：返回 (nil, nil)，不触发修复调用（PRD-0001 D9①）。
func (r *Reflector) Reflect(ctx context.Context, task core.Task, parent core.Candidate, lessons []string, batch []SampleRecord, n int) ([]Hypothesis, error) {
	raw, err := r.adv.Call(ctx, "reflect", buildReflectPrompt(task, parent, lessons, batch, n))
	if err != nil {
		return nil, err
	}
	type hypothesesPayload struct {
		Hypotheses []Hypothesis `json:"hypotheses"`
	}
	// 空假设池是模型的有意义回答而非协议错误（PRD-0001 D9①）：空数组与
	// 全 {input} 字面量清洗池两形态同规——语义校验放行空池，池为空的裁决
	// 统一移到 Defend 返回之后的 normalize 处。修复通道随之从「parse+语义」
	// 缩为 parse-only：只有垃圾 JSON 才触发恰好一次修复调用。
	payload, err := Defend(ctx, r.adv, "reflect", MarkerHypRepair, raw,
		func(hypothesesPayload) error { return nil })
	if err != nil {
		return nil, err
	}
	hyps := normalizeHypotheses(payload.Hypotheses, n)
	if len(hyps) == 0 {
		return nil, nil
	}
	return hyps, nil
}

// normalizeHypotheses clips the pool to n, assigns missing ids,
// strips {input} literals, truncates texts, clamps confidence and
// drops empty entries.
func normalizeHypotheses(pool []Hypothesis, n int) []Hypothesis {
	out := make([]Hypothesis, 0, min(len(pool), n))
	for _, h := range pool {
		if len(out) >= n {
			break
		}
		text := TruncateRunes(strings.TrimSpace(CleanInputLiteral(h.Text)), maxHypoTextRunes)
		if text == "" {
			continue
		}
		id := strings.TrimSpace(h.ID)
		if id == "" {
			id = fmt.Sprintf("h%d", len(out)+1)
		}
		out = append(out, Hypothesis{
			ID:         id,
			Text:       text,
			SampleIDs:  h.SampleIDs,
			Confidence: clamp01(h.Confidence),
		})
	}
	return out
}

// CleanInputLiteral removes {input} literals from model text before it
// is spliced into a prompt: rendering is plain replacement
// (core.RenderPrompt), so a stray literal would inject sample text
// into guidance sections. Paradigms embedding model output into
// guidance sections share it.
func CleanInputLiteral(s string) string {
	return strings.ReplaceAll(s, core.InputPlaceholder, "")
}

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

// truncateRunes caps s to n runes without splitting one.
func TruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// --- meta prompts ---------------------------------------------------------

func buildReflectPrompt(task core.Task, parent core.Candidate, lessons []string, batch []SampleRecord, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `你是提示词优化专家。请分析父提示词在样本上的表现，%s。

## 任务规格
- 名称：%s
- 描述：%s
- 指标：%s（主指标：%s）
- 原始模板：%s

## 父提示词
%s

## 逐样本表现（共 %d 条）
`, MarkerReflect, task.Name, task.Description, strings.Join(task.Metrics, ", "), task.Primary(),
		TruncateRunes(task.PromptTemplate, maxParentPromptRunes),
		TruncateRunes(parent.Prompt, maxParentPromptRunes), len(batch))
	for _, rec := range batch {
		expected, _ := json.Marshal(rec.Sample.Expected)
		fmt.Fprintf(&b, `
### %s
- 输入：%s
- 期望：%s
- 父输出：%s
- 分数：%s
- 诊断：%s
`,
			rec.Sample.ID,
			TruncateRunes(rec.Sample.Input, maxSampleInputRunes),
			TruncateRunes(string(expected), maxSampleInputRunes),
			TruncateRunes(rec.Response, maxResponseRunes),
			formatScores(rec.Scores),
			formatDiagnosis(rec.Diagnosis),
		)
	}
	if len(lessons) > 0 {
		b.WriteString("\n## 祖先教训\n")
		for _, lesson := range lessons {
			fmt.Fprintf(&b, "- %s\n", TruncateRunes(lesson, maxHypoTextRunes))
		}
	}
	fmt.Fprintf(&b, `
请针对父提示词的失败模式，输出至多 %d 条具体的改写假设。严格输出 JSON，除 JSON 外不要输出任何其他内容，不要使用代码围栏：
{"hypotheses": [{"id": "h1", "text": "指出问题与具体改法", "sample_ids": ["相关样本id"], "confidence": 0.8}]}

要求：
- text 不得包含 {input} 占位符字面量
- confidence 取 0~1
`, n)
	return b.String()
}

func formatScores(scores map[string]float64) string {
	if len(scores) == 0 {
		return "无（记 0）"
	}
	parts := make([]string, 0, len(scores))
	for _, m := range slices.Sorted(maps.Keys(scores)) {
		parts = append(parts, fmt.Sprintf("%s=%.4f", m, scores[m]))
	}
	return strings.Join(parts, " ")
}

func formatDiagnosis(diagnosis map[string]string) string {
	if len(diagnosis) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(diagnosis))
	for _, k := range slices.Sorted(maps.Keys(diagnosis)) {
		parts = append(parts, fmt.Sprintf("%s: %s", k, TruncateRunes(diagnosis[k], 200)))
	}
	return strings.Join(parts, "; ")
}

// --- JSON defense (private copy) ------------------------------------------
// tryParse/extractJSON below mirror harness synthesize.go:224-351.
// They are package-private copies: the engine must not depend on
// harness in reverse. Consolidation candidate for V5.

// tryParse extracts and decodes a JSON object from raw model output.
// A response that is already valid JSON passes through byte-for-byte;
// only a failed strict parse enters the clip/repair path.
func tryParse[T any](raw string) (T, bool) {
	var out T
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err == nil {
		return out, true
	}
	payload, ok := extractJSON(raw)
	if !ok {
		return out, false
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return out, false
	}
	return out, true
}

// extractJSON recovers a JSON object from a raw model response: it
// strips one enclosing code fence, clips the outermost brace span and
// repairs trailing commas. ok=false when no brace span exists.
func extractJSON(raw string) (string, bool) {
	s := stripOneFence(strings.TrimSpace(raw))
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", false
	}
	return stripTrailingCommas(s[start : end+1]), true
}

// stripTrailingCommas drops commas that directly precede a closing
// brace or bracket, scanning with string-literal and escape awareness
// so byte sequences inside JSON strings are copied verbatim.
func stripTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
			b.WriteByte(c)
		case inString:
			b.WriteByte(c)
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == ',':
			j := i + 1
			for j < len(s) && isJSONSpace(s[j]) {
				j++
			}
			if !(j < len(s) && (s[j] == '}' || s[j] == ']')) {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// stripOneFence removes a single enclosing Markdown code fence.
func stripOneFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	lines := strings.Split(t, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return t
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

// Defend runs the three-level JSON defense: local extraction, then one
// LLM repair call (parse and semantic failures share it), then an
// error carrying a raw excerpt.
func Defend[T any](ctx context.Context, a *Advisor, stage, repairMarker, raw string, check func(T) error) (T, error) {
	var zero T
	if strings.TrimSpace(raw) == "" {
		// An empty response cannot be repaired — the escalation ladder
		// in call() already handled the reasoning-burn case; this guard
		// keeps a nonsense repair call from ever firing.
		return zero, errors.New("模型响应内容为空")
	}
	var problems []string
	payload, ok := tryParse[T](raw)
	if ok {
		err := check(payload)
		if err == nil {
			return payload, nil
		}
		problems = []string{err.Error()}
	} else {
		problems = []string{"响应不是合法的 JSON 对象"}
	}
	repaired, err := a.Call(ctx, stage+"-repair", buildJSONRepairPrompt(raw, repairMarker, problems...))
	if err != nil {
		return zero, excerptError(stage, raw, err)
	}
	payload, ok = tryParse[T](repaired)
	if !ok {
		return zero, excerptError(stage, raw, errors.New("修复后仍不是合法 JSON"))
	}
	if err := check(payload); err != nil {
		return zero, excerptError(stage, raw, fmt.Errorf("修复后仍不合规: %w", err))
	}
	return payload, nil
}

// buildJSONRepairPrompt asks for a corrected JSON payload.
func buildJSONRepairPrompt(raw, marker string, problems ...string) string {
	return fmt.Sprintf(`下面这段内容本应是符合要求的 JSON，但存在问题：
%s

问题列表：
%s

请%s，严格输出 JSON 本身，除 JSON 外不要输出任何其他内容，不要使用代码围栏。`,
		raw, strings.Join(problems, "\n"), marker)
}

// excerptError attaches a raw-output excerpt so a failed call can be
// diagnosed from the error alone.
func excerptError(stage, raw string, cause error) error {
	return fmt.Errorf("%s: %w（原文摘录：%.200s）", stage, cause, strings.TrimSpace(raw))
}

package mcp

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Tool is one tools/list entry. InputSchema is a JSON-Schema object
// good enough for clients to render forms — this server validates the
// subset it declares (required fields, trio-vs-prompt exclusivity) and
// hands the rest to the CLI flag layer.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// DefaultTools is the frozen minimal set (提案 §2.4): optimize, verify,
// runs. Descriptions are Chinese per the repo's documentation
// convention.
func DefaultTools() []Tool {
	return []Tool{
		{
			Name: "optimize",
			Description: "同步执行一次 headless 提示优化 run（任务三件套模式或零配置 prompt 模式二选一），" +
				"阻塞至 run 结束并返回 run 摘要（summary.json 全量载荷）。单机无鉴权；预算上限建议显式给出。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":          map[string]any{"type": "string", "description": "task.yaml 路径（三件套模式）"},
					"candidate":     map[string]any{"type": "string", "description": "candidate.yaml 路径（三件套模式）"},
					"dataset":       map[string]any{"type": "string", "description": "dataset.yaml 路径（三件套模式）"},
					"prompt":        map[string]any{"type": "string", "description": "自然语言任务描述（零配置模式，与三件套互斥）"},
					"base_url":      map[string]any{"type": "string", "description": "OpenAI 兼容 base URL（缺省回落 PROMPTOPT_BASE_URL）"},
					"model":         map[string]any{"type": "string", "description": "模型名（缺省回落 PROMPTOPT_MODEL）"},
					"api_key":       map[string]any{"type": "string", "description": "API key（缺省回落 PROMPTOPT_API_KEY，默认 \"1\"）"},
					"out":           map[string]any{"type": "string", "description": "runs 输出目录（缺省 = 服务端 --runs-dir）"},
					"budget_evals":  map[string]any{"type": "integer", "description": "评估调用次数上限（0 = 不限）"},
					"budget_tokens": map[string]any{"type": "integer", "description": "token 预算上限（0 = 不限）"},
				},
			},
		},
		{
			Name: "verify",
			Description: "对一个已完成的 run 执行回归门禁验证（--anchor 锚点集 / --anchor-lib 锚点库 / 合成保留集），" +
				"返回三态回归结论与配对自助法 CI（verify.json 全量载荷）。退出码 0/2/3 都是有效结论（3 = 回归）。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"run_id":   map[string]any{"type": "string", "description": "待验证的 run id"},
					"runs_dir": map[string]any{"type": "string", "description": "runs 目录（缺省 = 服务端 --runs-dir）"},
				},
				"required": []string{"run_id"},
			},
		},
		{
			Name:        "runs",
			Description: "列出 runs 目录的运行摘要卡片（run id、任务、状态、主指标均值等），按时间新到旧排序。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"runs_dir": map[string]any{"type": "string", "description": "runs 目录（缺省 = 服务端 --runs-dir）"},
				},
			},
		},
	}
}

// Handlers carries what the embedding binary executes. The runs tool
// needs no injection (pure artifact reading); optimize/verify run the
// CLI-internal paths and return the artifact dir whose on-disk file is
// the payload's source of truth.
type Handlers struct {
	// RunsDir is the default runs root for every tool (the server's
	// --runs-dir). Per-call args may override it.
	RunsDir string
	// Optimize executes one synchronous headless run and returns the
	// run dir plus the run's exit code. err != nil only when no
	// artifacts exist at all (usage/loading failures) — the payload
	// assembly then degrades to an in-band error result.
	Optimize func(args OptimizeArgs) (runDir string, exit int, err error)
	// Verify mirrors Optimize for the regression gate; the returned
	// dir holds verify.json.
	Verify func(args VerifyArgs) (verifyDir string, exit int, err error)
}

// OptimizeArgs is the optimize tool's arguments. Exactly one of
// (task+candidate+dataset) and prompt must be present.
type OptimizeArgs struct {
	Task      string `json:"task"`
	Candidate string `json:"candidate"`
	Dataset   string `json:"dataset"`
	Prompt    string `json:"prompt"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKey    string `json:"api_key"`
	Out       string `json:"out"`

	BudgetEvals  int `json:"budget_evals"`
	BudgetTokens int `json:"budget_tokens"`
}

// VerifyArgs is the verify tool's arguments.
type VerifyArgs struct {
	RunID   string `json:"run_id"`
	RunsDir string `json:"runs_dir"`
}

// ToolResult is the tools/call in-band result. structuredContent holds
// the disk artifact verbatim (decoded); content[0].text carries the
// same payload as text so text-only clients still see it.
type ToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Content is one MCP content block (text only in this minimal face).
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// runArtifactName / verifyArtifactName pin the disk source-of-truth
// files: the MCP payload for optimize is <runDir>/summary.json (what
// finishRun persists) and for verify <verifyDir>/verify.json (what
// finishVerify persists) — never an in-memory re-encode.
const (
	runArtifactName    = "summary.json"
	verifyArtifactName = "verify.json"
)

// toolOptimize executes the optimize tool: validate args, run, read
// runDir/summary.json from disk as the payload.
func (s *Server) toolOptimize(args json.RawMessage) (any, *rpcError) {
	var a OptimizeArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, &rpcError{codeParams, fmt.Sprintf("invalid params: %v", err)}
	}
	if err := validateOptimizeArgs(a); err != nil {
		return nil, &rpcError{codeParams, "invalid params: " + err.Error()}
	}
	if s.Handlers.Optimize == nil {
		return nil, &rpcError{codeInternal, "internal error: optimize 工具未接线"}
	}
	runDir, exit, err := s.Handlers.Optimize(a)
	if err != nil {
		return errorResult(fmt.Sprintf("optimize 执行失败（退出码 %d）：%v", exit, err)), nil
	}
	return artifactResult(runDir, runArtifactName, exit)
}

// toolVerify executes the verify tool. Exit codes 0/2/3 are valid
// verdicts (3 = regression — the payload IS the answer); only 1 turns
// into an in-band error.
func (s *Server) toolVerify(args json.RawMessage) (any, *rpcError) {
	var a VerifyArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, &rpcError{codeParams, fmt.Sprintf("invalid params: %v", err)}
	}
	if a.RunID == "" {
		return nil, &rpcError{codeParams, "invalid params: run_id 必填"}
	}
	if s.Handlers.Verify == nil {
		return nil, &rpcError{codeInternal, "internal error: verify 工具未接线"}
	}
	verifyDir, exit, err := s.Handlers.Verify(a)
	if err != nil {
		return errorResult(fmt.Sprintf("verify 执行失败（退出码 %d）：%v", exit, err)), nil
	}
	return artifactResult(verifyDir, verifyArtifactName, exit)
}

// toolRuns lists the runs directory summary cards.
func (s *Server) toolRuns(args json.RawMessage) (any, *rpcError) {
	var a struct {
		RunsDir string `json:"runs_dir"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, &rpcError{codeParams, fmt.Sprintf("invalid params: %v", err)}
	}
	dir := cmp.Or(a.RunsDir, s.Handlers.RunsDir)
	if dir == "" {
		return nil, &rpcError{codeParams, "invalid params: runs_dir 必填（或服务端配置 --runs-dir）"}
	}
	cards, err := ListRunCards(dir)
	if err != nil {
		return errorResult(fmt.Sprintf("runs 读取失败：%v", err)), nil
	}
	type runsResult struct {
		Runs []RunCard `json:"runs"`
	}
	return ToolResult{
		Content:           []Content{{Type: "text", Text: renderRunsText(cards)}},
		StructuredContent: runsResult{Runs: cards},
	}, nil
}

// decodeArgs decodes tool arguments; absent arguments decode cleanly
// into zero values so validation stays in one place.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// validateOptimizeArgs enforces the declared schema: trio complete XOR
// prompt, connection fields required (mirroring what the CLI flag
// layer will demand, with a tool-shaped message).
func validateOptimizeArgs(a OptimizeArgs) error {
	trio := a.Task != "" && a.Candidate != "" && a.Dataset != ""
	switch {
	case trio && a.Prompt != "":
		return errors.New("task/candidate/dataset 三件套与 prompt（零配置模式）互斥")
	case trio:
	case a.Prompt != "":
	default:
		return errors.New("需要 task+candidate+dataset 三件套，或 prompt（零配置模式）之一")
	}
	if a.BaseURL == "" && os.Getenv("PROMPTOPT_BASE_URL") == "" {
		return errors.New("base_url 必填（或设 PROMPTOPT_BASE_URL）")
	}
	if a.Model == "" && os.Getenv("PROMPTOPT_MODEL") == "" {
		return errors.New("model 必填（或设 PROMPTOPT_MODEL）")
	}
	if a.BudgetEvals < 0 || a.BudgetTokens < 0 {
		return errors.New("budget_evals/budget_tokens 不可为负")
	}
	return nil
}

// artifactResult assembles the tool result from the on-disk artifact:
// structuredContent is the decoded file, content text is the file
// bytes verbatim. isError marks a failed execution (exit 1) whose
// payload still exists; exits 0/2 (optimize) and 0/2/3 (verify) are
// valid answers and stay isError-free.
func artifactResult(dir, name string, exit int) (any, *rpcError) {
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		return errorResult(fmt.Sprintf("读取 %s 失败：%v", path, err)), nil
	}
	var payload any
	if err := json.Unmarshal(b, &payload); err != nil {
		return errorResult(fmt.Sprintf("解码 %s 失败：%v", path, err)), nil
	}
	return ToolResult{
		Content:           []Content{{Type: "text", Text: string(b)}},
		StructuredContent: payload,
		IsError:           exit == 1,
	}, nil
}

// errorResult is the in-band execution failure (no artifacts or
// unreadable payload): the call executed, so it is a tool result with
// isError, not a protocol error.
func errorResult(msg string) ToolResult {
	return ToolResult{Content: []Content{{Type: "text", Text: msg}}, IsError: true}
}

// RunCard is one runs-directory summary card.
type RunCard struct {
	RunID     string             `json:"run_id"`
	CreatedAt *time.Time         `json:"created_at,omitempty"`
	Task      string             `json:"task,omitempty"`
	Candidate string             `json:"candidate,omitempty"`
	Model     string             `json:"model,omitempty"`
	Status    string             `json:"status,omitempty"`
	ExitCode  *int               `json:"exit_code,omitempty"`
	Means     map[string]float64 `json:"metric_means,omitempty"`
}

// ListRunCards reads every run dir under root and projects the card
// fields from manifest.json (created_at/task/candidate/model) and
// summary.json (status/exit_code/metric_means). A directory with
// neither artifact is not a run and is skipped; newest first (run ids
// embed a UTC timestamp, so lexical order is chronological).
func ListRunCards(root string) ([]RunCard, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	cards := make([]RunCard, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		card := RunCard{RunID: e.Name()}
		var mf struct {
			CreatedAt *time.Time `json:"created_at"`
			Task      string     `json:"task"`
			Candidate string     `json:"candidate"`
			Model     string     `json:"model"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
			_ = json.Unmarshal(b, &mf)
			card.CreatedAt, card.Task, card.Candidate, card.Model = mf.CreatedAt, mf.Task, mf.Candidate, mf.Model
		}
		if b, err := os.ReadFile(filepath.Join(dir, runArtifactName)); err == nil {
			var sum struct {
				Status   string             `json:"status"`
				ExitCode *int               `json:"exit_code"`
				Means    map[string]float64 `json:"metric_means"`
			}
			if json.Unmarshal(b, &sum) == nil {
				card.Status, card.ExitCode, card.Means = sum.Status, sum.ExitCode, sum.Means
			}
		}
		if card.CreatedAt == nil && card.Status == "" {
			continue // neither manifest nor summary: not a run dir
		}
		cards = append(cards, card)
	}
	slices.SortFunc(cards, func(a, b RunCard) int { return cmp.Compare(b.RunID, a.RunID) })
	return cards, nil
}

// renderRunsText renders the human-readable card list (Chinese per the
// output convention; it goes to the JSON-RPC peer, not a terminal).
func renderRunsText(cards []RunCard) string {
	if len(cards) == 0 {
		return "runs 目录为空"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "共 %d 个 run：\n", len(cards))
	for _, c := range cards {
		var line strings.Builder
		fmt.Fprintf(&line, "- %s", c.RunID)
		if c.Task != "" {
			fmt.Fprintf(&line, "（%s", c.Task)
			if c.Candidate != "" {
				fmt.Fprintf(&line, " / %s", c.Candidate)
			}
			line.WriteString("）")
		}
		if c.Status != "" {
			fmt.Fprintf(&line, " 状态=%s", c.Status)
		}
		if len(c.Means) > 0 {
			names := slices.Sorted(maps.Keys(c.Means))
			for _, m := range names {
				fmt.Fprintf(&line, " %s=%.4f", m, c.Means[m])
			}
		}
		out.WriteString(line.String())
		out.WriteByte('\n')
	}
	return out.String()
}

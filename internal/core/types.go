// Package core defines the v2 data model: tasks, candidates, datasets
// and the run artifacts produced by the evaluation pipeline.
package core

import (
	"cmp"
	"time"
)

// Role identifies which pipeline participant consumed tokens.
// Executor is the candidate evaluation path, optimizer is reserved for
// reflection/mutation calls, and judge is the llm_judge metric's
// grading call — metered separately in UsageByRole while still arming
// the executor token soft stop (total evaluation cost stays one valve).
type Role string

const (
	RoleExecutor  Role = "executor"
	RoleOptimizer Role = "optimizer"
	RoleJudge     Role = "judge"
)

// Usage reports token consumption for one LLM call.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens" yaml:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens" yaml:"completion_tokens"`
}

// Total returns prompt plus completion tokens.
func (u Usage) Total() int64 { return u.PromptTokens + u.CompletionTokens }

// Task is the v2 task specification (task.yaml).
type Task struct {
	Name           string   `json:"name" yaml:"name"`
	Description    string   `json:"description" yaml:"description"`
	PromptTemplate string   `json:"prompt_template" yaml:"prompt_template"`
	Metrics        []string `json:"metrics" yaml:"metrics"`
	PrimaryMetric  string   `json:"primary_metric,omitempty" yaml:"primary_metric"`
}

// Primary returns the primary metric, defaulting to the first declared
// metric. Validate guarantees Metrics is non-empty.
func (t Task) Primary() string { return cmp.Or(t.PrimaryMetric, t.Metrics[0]) }

// Candidate is a prompt candidate (candidate.yaml).
type Candidate struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
	Prompt      string `json:"prompt" yaml:"prompt"`
}

// Sample is one dataset example. Expected may be a string, an array or
// an object; metric semantics differ per kind.
type Sample struct {
	ID       string `json:"id" yaml:"id"`
	Input    string `json:"input" yaml:"input"`
	Expected any    `json:"expected" yaml:"expected"`
	Split    string `json:"split" yaml:"split"`
}

// Dataset is the v2 dataset specification (dataset.yaml).
type Dataset struct {
	Name    string   `json:"name" yaml:"name"`
	Samples []Sample `json:"samples" yaml:"samples"`
}

// RunStatus is the terminal state of a run.
type RunStatus string

const (
	StatusCompleted       RunStatus = "completed"
	StatusFailed          RunStatus = "failed"
	StatusBudgetExhausted RunStatus = "budget_exhausted"
	// StatusAborted marks a run interrupted by context cancellation
	// (Ctrl-C); exit code stays 1 — cancellation is not budget
	// exhaustion.
	StatusAborted RunStatus = "aborted"
)

// RunResult summarizes one evaluation run; it is serialized to
// summary.json.
type RunResult struct {
	RunID         string             `json:"run_id"`
	TaskName      string             `json:"task_name"`
	CandidateID   string             `json:"candidate_id"`
	DatasetName   string             `json:"dataset_name"`
	Split         string             `json:"split,omitempty"`
	Status        RunStatus          `json:"status"`
	ExitCode      int                `json:"exit_code"`
	TotalSamples  int                `json:"total_samples"`
	Evaluated     int                `json:"evaluated_samples"`
	FailedSamples []string           `json:"failed_samples"`
	Undispatched  int                `json:"undispatched"`
	UsageByRole   map[Role]Usage     `json:"usage_by_role"`
	MetricMeans   map[string]float64 `json:"metric_means"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    time.Time          `json:"finished_at"`
}

// SampleTrace records the evaluation of a single sample; it is
// serialized to samples/<id>.json. Usage is the sample's total
// evaluation spend — the executor call plus the judge call when the
// llm_judge metric is declared; DurationMS stays the executor call
// alone and JudgeMS, when set, is the judge call's latency. With
// multi-rep sampling (Reps > 1) Scores are per-metric means over the
// reps, ScoresSD the per-metric in-sample standard deviation and
// RepScores the per-rep per-metric scores in rep order (the per-rep
// bar data behind the dashboard's rep strip); artifacts written before
// Reps existed read as reps=1.
type SampleTrace struct {
	SampleID   string               `json:"sample_id"`
	Role       Role                 `json:"role"`
	Prompt     string               `json:"prompt"`
	Response   string               `json:"response"`
	Reasoning  string               `json:"reasoning,omitempty"`
	Scores     map[string]float64   `json:"scores,omitempty"`
	ScoresSD   map[string]float64   `json:"scores_sd,omitempty"`
	RepScores  []map[string]float64 `json:"rep_scores,omitempty"`
	Reps       int                  `json:"reps,omitempty"`
	Diagnosis  map[string]string    `json:"diagnosis,omitempty"`
	Error      string               `json:"error,omitempty"`
	Usage      Usage                `json:"usage"`
	DurationMS int64                `json:"duration_ms"`
	JudgeMS    int64                `json:"judge_ms,omitempty"`
}

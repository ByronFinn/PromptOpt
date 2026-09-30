package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// CandidatePayload is the candidate wire format shared by every
// paradigm's proposal prompts: one JSON object with id/name/
// description/prompt, defended by Defend and gated by
// CandidateQualityGate.
type CandidatePayload struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

// CandidateQualityGate validates one proposed candidate: a non-empty
// prompt carrying the {input} placeholder (rendering is plain string
// replacement, core.RenderPrompt — a prompt without the placeholder
// can never see the sample input).
func CandidateQualityGate(p CandidatePayload) error {
	text := strings.TrimSpace(p.Prompt)
	if text == "" {
		return errors.New("prompt 为空")
	}
	if !strings.Contains(text, core.InputPlaceholder) {
		return errors.New("prompt 缺少 {input} 占位符")
	}
	return nil
}

// ProduceCandidate runs one optimizer-role proposal call and defends
// its JSON through the quality gate with a single repair attempt.
// Paradigms that propose whole candidates (mutators, population
// operators, crossover) share this scaffold instead of re-implementing
// the dial/parse/repair/gate sequence.
func ProduceCandidate(ctx context.Context, adv *Advisor, stage, prompt string) (core.Candidate, error) {
	raw, err := adv.Call(ctx, stage, prompt)
	if err != nil {
		return core.Candidate{}, fmt.Errorf("%s: %w", stage, err)
	}
	payload, err := Defend(ctx, adv, stage, MarkerCandFix, raw, CandidateQualityGate)
	if err != nil {
		return core.Candidate{}, err
	}
	return core.Candidate{
		ID:          strings.TrimSpace(payload.ID),
		Name:        strings.TrimSpace(payload.Name),
		Description: strings.TrimSpace(payload.Description),
		Prompt:      strings.TrimSpace(payload.Prompt),
	}, nil
}

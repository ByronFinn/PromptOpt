package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/eval"
)

// defaultPollInterval is how often the interactive gate re-reads
// checkpoint.json.
const defaultPollInterval = 300 * time.Millisecond

// Checkpoint is the human gate between synthesis/filtering and the
// baseline evaluation. checkpoint.json is always written pending
// first (visible on the artifact and web channels alike); autopilot
// then flips it to approved immediately, interactive mode polls the
// artifact until it reads approved — the web approve button and a
// manual file edit converge on the same state.
type Checkpoint struct {
	Dir      string
	RunID    string
	Interval time.Duration    // poll interval; default 300ms
	Now      func() time.Time // injectable clock
	OnEvent  func(eval.Event)
}

// Gate writes the checkpoint artifact and blocks per mode. A canceled
// context aborts the interactive wait with an error (the run exits 1).
func (c *Checkpoint) Gate(ctx context.Context, mode GateMode) error {
	if err := c.save(CheckpointPending, mode); err != nil {
		return err
	}
	c.emit(CheckpointPending, mode)

	if mode == ModeAutopilot {
		if err := c.save(CheckpointApproved, mode); err != nil {
			return err
		}
		c.emit(CheckpointApproved, mode)
		return nil
	}

	interval := c.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	for {
		// Parse failures count as unresolved: the artifact file is the
		// only source of truth, never the last successful read.
		if st, err := LoadCheckpoint(c.Dir); err == nil && st.Status == CheckpointApproved {
			c.emit(CheckpointApproved, mode)
			return nil
		}
		wait := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			wait.Stop()
			return fmt.Errorf("checkpoint wait aborted: %w", ctx.Err())
		case <-wait.C:
		}
	}
}

func (c *Checkpoint) save(status CheckpointStatus, mode GateMode) error {
	if err := SaveCheckpoint(c.Dir, CheckpointState{Status: status, Mode: mode, UpdatedAt: c.clock()}); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	return nil
}

func (c *Checkpoint) emit(status CheckpointStatus, mode GateMode) {
	if c.OnEvent == nil {
		return
	}
	c.OnEvent(eval.Event{
		Type:   EventCheckpoint,
		Time:   time.Now(),
		RunID:  c.RunID,
		Status: string(status),
		Detail: map[string]any{"status": string(status), "mode": string(mode)},
	})
}

func (c *Checkpoint) clock() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

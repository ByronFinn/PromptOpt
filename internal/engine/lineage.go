package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// LineageRecord is one candidate's provenance entry in lineage.json:
// parents, producing operator, the hypothesis it followed, its score
// row and whether it entered the frontier. A budget-interrupted child
// keeps its partial score row (undispatched cells zero-filled) but is
// flagged Incomplete and never enters the frontier — unknown cells
// must not read as real zeros in dominance.
type LineageRecord struct {
	ID          string       `json:"id"`
	Parents     []string     `json:"parents,omitempty"`
	Operator    string       `json:"operator"` // baseline|rewrite|merge|restart
	Hypotheses  []Hypothesis `json:"hypotheses,omitempty"`
	Round       int          `json:"round"`
	Scores      []float64    `json:"scores"`
	PrimaryMean float64      `json:"primary_mean"`
	Admitted    bool         `json:"admitted"`
	Incomplete  bool         `json:"incomplete,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

// Lineage is the append-only candidate flow of one run, persisted as
// an atomic full rewrite of runs/<id>/lineage.json after every append.
type Lineage struct {
	path string
	recs []LineageRecord
}

// LoadOrInitLineage returns the lineage at path, an empty one when the
// artifact does not exist yet.
func LoadOrInitLineage(path string) (*Lineage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Lineage{path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []LineageRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &Lineage{path: path, recs: recs}, nil
}

// Append adds one record and rewrites the artifact atomically.
func (l *Lineage) Append(rec LineageRecord) error {
	l.recs = append(l.recs, rec)
	return l.flush()
}

// Records returns a copy of all records in append order.
func (l *Lineage) Records() []LineageRecord { return slices.Clone(l.recs) }

// Lessons walks the parent chain of id breadth-first (nearest
// ancestors first) and collects their selected hypothesis texts,
// capped at limit. It feeds the reflection and mutation context.
func (l *Lineage) Lessons(id string, limit int) []string {
	byID := make(map[string]*LineageRecord, len(l.recs))
	for i := range l.recs {
		byID[l.recs[i].ID] = &l.recs[i]
	}
	visited := map[string]bool{id: true}
	var lessons []string
	frontier := parentsOf(byID, id)
	for len(frontier) > 0 && len(lessons) < limit {
		var next []string
		for _, pid := range frontier {
			if visited[pid] {
				continue
			}
			visited[pid] = true
			rec, ok := byID[pid]
			if !ok {
				continue
			}
			for _, h := range rec.Hypotheses {
				if len(lessons) >= limit {
					break
				}
				if text := strings.TrimSpace(h.Text); text != "" {
					lessons = append(lessons, fmt.Sprintf("%s：%s", rec.ID, truncateRunes(text, maxHypoTextRunes)))
				}
			}
			next = append(next, rec.Parents...)
		}
		frontier = next
	}
	return lessons
}

func parentsOf(byID map[string]*LineageRecord, id string) []string {
	if rec, ok := byID[id]; ok {
		return rec.Parents
	}
	return nil
}

func (l *Lineage) flush() error { return saveJSON(l.path, l.recs) }

// saveJSON serializes v as indented JSON and writes it atomically.
func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// writeAtomic writes b through a unique temp file and a rename, so
// concurrent readers never observe a partial artifact and concurrent
// writers never collide on one temp name — the same contract as
// harness.SaveJSON (artifact.go).
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

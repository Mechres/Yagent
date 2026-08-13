package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Baseline records the last `yagent bench` pass score per model so a model or
// sampling change that silently regresses the agent loop can be caught: the
// bench writes its result to the data dir, and `yagent doctor` warns when the
// configured model's last recorded score is below its own best.
type Baseline struct {
	// Best pass/total per model (normalized model name -> score).
	Best map[string]Score `json:"best"`
	// Last run per model, for context in the doctor report.
	Last map[string]LastRun `json:"last"`
}

// Score is one recorded best.
type Score struct {
	Pass  int       `json:"pass"`
	Total int       `json:"total"`
	When  time.Time `json:"when"`
}

// LastRun is the most recent bench run.
type LastRun struct {
	Pass  int       `json:"pass"`
	Total int       `json:"total"`
	When  time.Time `json:"when"`
}

// BaselinePath is the bench baseline file under the data dir.
func BaselinePath(dataDir string) string {
	return filepath.Join(dataDir, "bench-baseline.json")
}

// LoadBaseline reads the baseline file (missing/corrupt = empty).
func LoadBaseline(dataDir string) *Baseline {
	b := &Baseline{Best: map[string]Score{}, Last: map[string]LastRun{}}
	data, err := os.ReadFile(BaselinePath(dataDir))
	if err != nil {
		return b
	}
	_ = json.Unmarshal(data, b)
	if b.Best == nil {
		b.Best = map[string]Score{}
	}
	if b.Last == nil {
		b.Last = map[string]LastRun{}
	}
	return b
}

// Record stores a bench result for model: updates the per-model best and the
// last run, and persists the file. Returns (prevBest, isNewBest) so the caller
// can warn on regression.
func (b *Baseline) Record(dataDir, model string, pass, total int) (prevBest int, ok bool) {
	prev, had := b.Best[model]
	now := Score{Pass: pass, Total: total, When: time.Now()}
	if !had || pass > prev.Pass {
		b.Best[model] = now
	} else {
		now = prev // keep the best
	}
	b.Last[model] = LastRun{Pass: pass, Total: total, When: time.Now()}
	_ = os.MkdirAll(dataDir, 0o755)
	if data, err := json.MarshalIndent(b, "", "  "); err == nil {
		_ = os.WriteFile(BaselinePath(dataDir), data, 0o600)
	}
	if !had {
		return prev.Pass, false
	}
	return prev.Pass, pass >= prev.Pass
}

// Regression returns a human-readable warning when the model's last recorded
// score is worse than its best, or "" when there is nothing to flag.
func (b *Baseline) Regression(model string) string {
	best, ok := b.Best[model]
	if !ok {
		return ""
	}
	last, ok := b.Last[model]
	if !ok || last.Pass >= best.Pass {
		return ""
	}
	return fmt.Sprintf("model %q last benched %d/%d but its best is %d/%d (%s) — run `yagent bench --repeat 3` to re-check",
		model, last.Pass, last.Total, best.Pass, best.Total, best.When.Format("2006-01-02"))
}

// ScoreString renders a model's recorded best ("" when never benched).
func (b *Baseline) ScoreString(model string) string {
	s, ok := b.Best[model]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d/%d (%s)", s.Pass, s.Total, s.When.Format("2006-01-02"))
}

package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
)

// suiteVersion identifies the task-suite composition. Bump it whenever Tasks()
// changes, so baselines recorded against a different suite are never compared
// as if they were the same measurement.
const suiteVersion = 3

// Fingerprint captures what a bench run was measured against: the model, the
// server, the context window and the sampling profile. Baselines are keyed by
// fingerprint, so a model/template/sampling change records a NEW baseline
// instead of being flagged as a regression against an incomparable one.
type Fingerprint struct {
	Model     string `json:"model"`
	ServerURL string `json:"server_url"`
	Window    int    `json:"window"`
	Sampling  string `json:"sampling"` // e.g. "0.6/0.95/20/1.05/0/0"
	Suite     string `json:"suite"`    // "3"
}

// NewFingerprint builds a fingerprint from a config.
func NewFingerprint(model, serverURL string, window int, sampling llm.Sampling) Fingerprint {
	return Fingerprint{
		Model:     model,
		ServerURL: serverURL,
		Window:    window,
		Sampling:  samplingKey(sampling),
		Suite:     fmt.Sprintf("%d", suiteVersion),
	}
}

// Key is the stable comparison key for a fingerprint.
func (f Fingerprint) Key() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", f.Model, f.ServerURL, f.Window, f.Sampling, f.Suite)
}

// samplingKey renders the sampling profile fields that affect tool reliability.
func samplingKey(s llm.Sampling) string {
	return fmt.Sprintf("%g/%g/%d/%g/%g/%d",
		s.Temperature, s.TopP, s.TopK, s.RepetitionPenalty, s.MinP, s.ReasoningMaxTokens)
}

// TaskStat is one task's recorded pass count (for variance / per-task context).
type TaskStat struct {
	Name   string  `json:"name"`
	Passed int     `json:"passed"`
	Runs   int     `json:"runs"`
	Tps    float64 `json:"tps"`
}

// Score is one recorded best.
type Score struct {
	Pass  int       `json:"pass"`
	Total int       `json:"total"`
	When  time.Time `json:"when"`
	// Fingerprint of the run that set this best (for like-for-like diagnosis).
	Fingerprint Fingerprint `json:"fingerprint,omitempty"`
}

// LastRun is the most recent bench run.
type LastRun struct {
	Pass        int         `json:"pass"`
	Total       int         `json:"total"`
	When        time.Time   `json:"when"`
	Tps         float64     `json:"tps"`             // median tokens/sec across tasks
	WallMS      int64       `json:"wall_ms"`         // median task wall time
	Fingerprint Fingerprint `json:"fingerprint"`     // what this run was measured against
	Tasks       []TaskStat  `json:"tasks,omitempty"` // per-task pass counts (variance)
}

// Baseline records bench pass scores keyed by fingerprint so a model or
// sampling change that silently regresses the agent loop can be caught:
// `yagent doctor` warns when the configured model's last recorded score is
// below its own best under the SAME fingerprint.
type Baseline struct {
	// Best pass/total per fingerprint key.
	Best map[string]Score `json:"best"`
	// Last run per fingerprint key, for context in the doctor report.
	Last map[string]LastRun `json:"last"`
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

// Record stores a bench result keyed by its fingerprint: updates the per-
// fingerprint best and last run, and persists the file. Returns
// (prevBest, isNewBest, prevWasComparable): prevWasComparable is false when
// the previous best under this model name was recorded against a DIFFERENT
// fingerprint (changed model/sampling/window) — in that case the new run is a
// new baseline, not a regression.
func (b *Baseline) Record(dataDir string, fp Fingerprint, pass, total int, tps float64, wallMS int64, tasks []TaskStat) (prevBest int, isNewBest, comparable bool) {
	key := fp.Key()
	prev, had := b.Best[key]
	now := Score{Pass: pass, Total: total, When: time.Now(), Fingerprint: fp}
	if !had || pass > prev.Pass {
		b.Best[key] = now
	} else {
		now = prev
	}
	b.Last[key] = LastRun{Pass: pass, Total: total, When: time.Now(), Tps: tps, WallMS: wallMS, Fingerprint: fp, Tasks: tasks}
	_ = os.MkdirAll(dataDir, 0o755)
	if data, err := json.MarshalIndent(b, "", "  "); err == nil {
		_ = os.WriteFile(BaselinePath(dataDir), data, 0o600)
	}
	if !had {
		return prev.Pass, false, false
	}
	return prev.Pass, pass >= prev.Pass, true
}

// FingerprintBest returns the recorded best for a fingerprint ("" when none).
func (b *Baseline) FingerprintBest(fp Fingerprint) string {
	s, ok := b.Best[fp.Key()]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d/%d (%s)", s.Pass, s.Total, s.When.Format("2006-01-02"))
}

// Regression returns a human-readable warning when the model's last recorded
// score under fp is worse than its best under the SAME fingerprint, or ""
// when there is nothing to flag. A changed fingerprint (new model/sampling/
// window) is a new baseline, never a regression.
func (b *Baseline) Regression(fp Fingerprint) string {
	key := fp.Key()
	best, ok := b.Best[key]
	if !ok {
		return ""
	}
	last, ok := b.Last[key]
	if !ok || last.Pass >= best.Pass {
		return ""
	}
	return fmt.Sprintf("model %q last benched %d/%d but its best is %d/%d (%s) — run `yagent bench --repeat 3` to re-check",
		fp.Model, last.Pass, last.Total, best.Pass, best.Total, best.When.Format("2006-01-02"))
}

// ScoreString renders a fingerprint's recorded best ("" when never benched).
func (b *Baseline) ScoreString(fp Fingerprint) string {
	s, ok := b.Best[fp.Key()]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d/%d (%s)", s.Pass, s.Total, s.When.Format("2006-01-02"))
}

// RecentFingerprints lists the fingerprints that have ever been benched under
// a model name (for the doctor report: shows what changed).
func (b *Baseline) RecentFingerprints(model string) []Fingerprint {
	seen := map[string]Fingerprint{}
	for key, s := range b.Best {
		if s.Fingerprint.Model == model && s.Fingerprint.Key() != "" {
			seen[key] = s.Fingerprint
		}
	}
	for key, r := range b.Last {
		if r.Fingerprint.Model == model && r.Fingerprint.Key() != "" {
			seen[key] = r.Fingerprint
		}
	}
	out := make([]Fingerprint, 0, len(seen))
	for _, fp := range seen {
		out = append(out, fp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// SamplingString is a compact human rendering of a sampling profile.
func SamplingString(s llm.Sampling) string {
	parts := []string{
		fmt.Sprintf("temp=%g", s.Temperature),
		fmt.Sprintf("top_p=%g", s.TopP),
	}
	if s.TopK > 0 {
		parts = append(parts, fmt.Sprintf("top_k=%d", s.TopK))
	}
	if s.RepetitionPenalty > 0 {
		parts = append(parts, fmt.Sprintf("rep=%.2f", s.RepetitionPenalty))
	}
	if s.MinP > 0 {
		parts = append(parts, fmt.Sprintf("min_p=%g", s.MinP))
	}
	if s.ReasoningMaxTokens > 0 {
		parts = append(parts, fmt.Sprintf("reason=%d", s.ReasoningMaxTokens))
	}
	return strings.Join(parts, " ")
}

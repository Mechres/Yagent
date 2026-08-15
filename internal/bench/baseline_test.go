package bench

import (
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/llm"
)

func testFP(model string) Fingerprint {
	return NewFingerprint(model, "http://localhost:8089", 16384, llm.Sampling{Temperature: 0.6, TopP: 0.95})
}

func TestBaselineRecordAndRegression(t *testing.T) {
	dir := t.TempDir()
	b := LoadBaseline(dir)
	fp := testFP("model-x")

	// first run: recorded as best, no regression
	prev, ok, comparable := b.Record(dir, fp, 16, 18, 30, 5000, nil)
	if ok {
		t.Error("first run should not report regression")
	}
	if comparable {
		t.Error("first run should not be comparable (no prior)")
	}
	if prev != 0 {
		t.Errorf("prev = %d, want 0", prev)
	}
	if s := b.ScoreString(fp); !strings.HasPrefix(s, "16/18") {
		t.Errorf("ScoreString = %q, want 16/18 prefix", s)
	}
	if reg := b.Regression(fp); reg != "" {
		t.Errorf("no regression expected: %q", reg)
	}

	// equal score: no regression
	prev, ok, _ = b.Record(dir, fp, 16, 18, 30, 5000, nil)
	if !ok || prev != 16 {
		t.Errorf("equal run: ok=%v prev=%d, want ok=true prev=16", ok, prev)
	}

	// worse score under the SAME fingerprint: regression flagged, best preserved
	prev, ok, _ = b.Record(dir, fp, 12, 18, 30, 5000, nil)
	if ok {
		t.Error("worse run should report regression (ok=true)")
	}
	if prev != 16 {
		t.Errorf("prev = %d, want 16", prev)
	}
	if s := b.ScoreString(fp); !strings.HasPrefix(s, "16/18") {
		t.Errorf("best preserved = %q, want 16/18 prefix", s)
	}
	if reg := b.Regression(fp); reg == "" {
		t.Error("regression not flagged")
	}

	// better score: new best
	prev, ok, _ = b.Record(dir, fp, 18, 18, 30, 5000, nil)
	if !ok || prev != 16 {
		t.Errorf("better run: ok=%v prev=%d, want ok=true prev=16", ok, prev)
	}
	if s := b.ScoreString(fp); !strings.HasPrefix(s, "18/18") {
		t.Errorf("new best = %q, want 18/18 prefix", s)
	}

	// persistence: reload sees the same data
	b2 := LoadBaseline(dir)
	if s := b2.ScoreString(fp); !strings.HasPrefix(s, "18/18") {
		t.Errorf("reloaded best = %q, want 18/18 prefix", s)
	}

	// unknown model: no baseline
	if s := b.ScoreString(testFP("nope")); s != "" {
		t.Errorf("unknown model baseline = %q, want empty", s)
	}
}

// TestBaselineFingerprintIsNewBaseline: a changed sampling profile (or window)
// records a NEW baseline, never a regression against the old config's score.
func TestBaselineFingerprintIsNewBaseline(t *testing.T) {
	dir := t.TempDir()
	b := LoadBaseline(dir)

	base := testFP("model-x")
	baseCold := NewFingerprint("model-x", "http://localhost:8089", 16384, llm.Sampling{Temperature: 0.3, TopP: 0.95})

	// 16/18 under the base recipe
	_, _, _ = b.Record(dir, base, 16, 18, 30, 5000, nil)
	if reg := b.Regression(base); reg != "" {
		t.Fatalf("base recipe regression: %q", reg)
	}

	// 10/18 under a different (colder) recipe: NOT a regression — different
	// fingerprint, so it's a new baseline slot.
	prev, ok, comparable := b.Record(dir, baseCold, 10, 18, 25, 6000, nil)
	if ok {
		t.Error("cold recipe 10/18 must not report regression (different fingerprint)")
	}
	if comparable {
		t.Error("cold recipe must not be comparable to the base recipe's best")
	}
	if prev != 0 {
		t.Errorf("prev under cold = %d, want 0 (fresh slot)", prev)
	}
	if reg := b.Regression(baseCold); reg != "" {
		t.Errorf("cold recipe must not flag regression: %q", reg)
	}
	// the base recipe's best is untouched
	if reg := b.Regression(base); reg != "" {
		t.Errorf("base recipe best must survive: %q", reg)
	}
	if s := b.ScoreString(base); !strings.HasPrefix(s, "16/18") {
		t.Errorf("base best = %q, want 16/18", s)
	}
}

func TestBaselineRecentFingerprints(t *testing.T) {
	dir := t.TempDir()
	b := LoadBaseline(dir)
	base := testFP("model-x")
	baseCold := NewFingerprint("model-x", "http://localhost:8089", 16384, llm.Sampling{Temperature: 0.3, TopP: 0.95})
	_, _, _ = b.Record(dir, base, 16, 18, 30, 5000, nil)
	_, _, _ = b.Record(dir, baseCold, 10, 18, 25, 6000, nil)
	if got := len(b.RecentFingerprints("model-x")); got != 2 {
		t.Errorf("RecentFingerprints = %d, want 2", got)
	}
	if got := len(b.RecentFingerprints("other")); got != 0 {
		t.Errorf("RecentFingerprints(other) = %d, want 0", got)
	}
}

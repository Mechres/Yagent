package bench

import (
	"strings"
	"testing"
)

func TestBaselineRecordAndRegression(t *testing.T) {
	dir := t.TempDir()
	b := LoadBaseline(dir)

	// first run: recorded as best, no regression
	prev, ok := b.Record(dir, "model-x", 16, 18)
	if ok {
		t.Error("first run should not report regression")
	}
	if prev != 0 {
		t.Errorf("prev = %d, want 0", prev)
	}
	if s := b.ScoreString("model-x"); !strings.HasPrefix(s, "16/18") {
		t.Errorf("ScoreString = %q, want 16/18 prefix", s)
	}
	if reg := b.Regression("model-x"); reg != "" {
		t.Errorf("no regression expected: %q", reg)
	}

	// equal score: no regression
	prev, ok = b.Record(dir, "model-x", 16, 18)
	if !ok || prev != 16 {
		t.Errorf("equal run: ok=%v prev=%d, want ok=true prev=16", ok, prev)
	}

	// worse score: regression flagged, best preserved
	prev, ok = b.Record(dir, "model-x", 12, 18)
	if ok {
		t.Error("worse run should report regression (ok=true)")
	}
	if prev != 16 {
		t.Errorf("prev = %d, want 16", prev)
	}
	if s := b.ScoreString("model-x"); !strings.HasPrefix(s, "16/18") {
		t.Errorf("best preserved = %q, want 16/18 prefix", s)
	}
	if reg := b.Regression("model-x"); reg == "" {
		t.Error("regression not flagged")
	}

	// better score: new best
	prev, ok = b.Record(dir, "model-x", 18, 18)
	if !ok || prev != 16 {
		t.Errorf("better run: ok=%v prev=%d, want ok=true prev=16", ok, prev)
	}
	if s := b.ScoreString("model-x"); !strings.HasPrefix(s, "18/18") {
		t.Errorf("new best = %q, want 18/18 prefix", s)
	}

	// persistence: reload sees the same data
	b2 := LoadBaseline(dir)
	if s := b2.ScoreString("model-x"); !strings.HasPrefix(s, "18/18") {
		t.Errorf("reloaded best = %q, want 18/18 prefix", s)
	}

	// unknown model: no baseline
	if s := b.ScoreString("nope"); s != "" {
		t.Errorf("unknown model baseline = %q, want empty", s)
	}
}

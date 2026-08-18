package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOffloadResult(t *testing.T) {
	before := CompressionStats()
	ws := t.TempDir()
	big := strings.Repeat("line of output\n", 100)
	got := offloadResult(ws, big, 100)
	// must not be the plain truncated tail; must point at the scratch file
	if !strings.Contains(got, "saved to .yagent/scratch/") {
		t.Errorf("offloadResult did not offload: %q", got[:min(len(got), 120)])
	}
	// the scratch file must actually contain the full output
	files, _ := os.ReadDir(filepath.Join(ws, ".yagent", "scratch"))
	if len(files) != 1 {
		t.Fatalf("scratch files = %d, want 1", len(files))
	}
	data, _ := os.ReadFile(filepath.Join(ws, ".yagent", "scratch", files[0].Name()))
	if string(data) != big {
		t.Errorf("scratch file content mismatch (len %d vs %d)", len(data), len(big))
	}
	after := CompressionStats()
	if after.Offloaded <= before.Offloaded || after.OriginalBytes <= before.OriginalBytes {
		t.Fatalf("compression metrics did not record offload: before=%+v after=%+v", before, after)
	}
	// the return shows the head lines (capped at maxBytes), not the tail
	if !strings.HasPrefix(got, "line of output") {
		t.Errorf("return should start with the head lines: %q", got[:min(len(got), 60)])
	}
	// small output passes through untouched (no offload side effect)
	small := "hello"
	if got := offloadResult(ws, small, 1000); got != small {
		t.Errorf("small output should pass through: %q", got)
	}
}

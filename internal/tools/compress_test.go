package tools

import (
	"strings"
	"testing"
)

func TestCompressPreviewJSONPreservesData(t *testing.T) {
	s := "{\n" + strings.Repeat("  \"answer\": \"value\",\n", 20) + "  \"last\": true\n}"
	p, ok := compactJSON(s)
	if !ok || len(p) >= len(s) {
		t.Fatalf("compactJSON = %v, %v", p, ok)
	}
	if !strings.Contains(p, `"last":true`) {
		t.Errorf("compact JSON lost data: %s", p)
	}
}

func TestCompressPreviewLogKeepsDiagnostics(t *testing.T) {
	lines := make([]string, 0, 100)
	for i := 0; i < 90; i++ {
		lines = append(lines, "INFO progress tick")
	}
	lines = append(lines, "ERROR compiler failed at main.go:4")
	lines = append(lines, strings.Repeat("INFO progress tick\n", 20))
	p, ok := compactLog(strings.Join(lines, "\n"))
	if !ok || !strings.Contains(p, "ERROR compiler failed") || !strings.Contains(p, "full output") {
		t.Fatalf("compactLog = %v, %v", p, ok)
	}
}

func TestCompressPreviewDiffKeepsChangedLines(t *testing.T) {
	s := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n" + strings.Repeat(" context\n", 40) + "+new line\n-old line\n"
	p, ok := compactDiff(s)
	if !ok || !strings.Contains(p, "+new line") || !strings.Contains(p, "-old line") || strings.Contains(p, "context") {
		t.Fatalf("compactDiff = %v, %v", p, ok)
	}
}

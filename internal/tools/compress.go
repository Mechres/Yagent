package tools

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync/atomic"
)

// CompressionMetrics describes the process-local recoverable-output savings.
// It is intentionally aggregate: callers can sample it for a status line or
// benchmark without adding tool-name plumbing to every implementation.
type CompressionMetrics struct {
	Offloaded     int64
	Compressed    int64
	OriginalBytes int64
	PreviewBytes  int64
}

var compressionMetrics struct {
	offloaded  atomic.Int64
	compressed atomic.Int64
	original   atomic.Int64
	preview    atomic.Int64
}

// CompressionStats returns a consistent-enough snapshot for display and eval
// reporting. It is not a transaction boundary.
func CompressionStats() CompressionMetrics {
	return CompressionMetrics{
		Offloaded:     compressionMetrics.offloaded.Load(),
		Compressed:    compressionMetrics.compressed.Load(),
		OriginalBytes: compressionMetrics.original.Load(),
		PreviewBytes:  compressionMetrics.preview.Load(),
	}
}

func recordCompression(original, preview int, compressed bool) {
	compressionMetrics.offloaded.Add(1)
	compressionMetrics.original.Add(int64(original))
	compressionMetrics.preview.Add(int64(preview))
	if compressed {
		compressionMetrics.compressed.Add(1)
	}
}

// compressPreview applies only loss-tolerant presentation changes. The full
// payload is saved by offloadResult before this preview is returned, so this
// function never becomes the sole copy of tool data.
func compressPreview(s string) (string, bool) {
	if p, ok := compactJSON(s); ok {
		return p, true
	}
	if p, ok := compactLog(s); ok {
		return p, true
	}
	if p, ok := compactDiff(s); ok {
		return p, true
	}
	return s, false
}

func compactJSON(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false
	}
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(trimmed)); err != nil || out.Len() >= len(s)*9/10 {
		return "", false
	}
	return out.String(), true
}

func compactLog(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) < 32 {
		return "", false
	}
	interesting := make([]string, 0, len(lines))
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") ||
			strings.Contains(lower, "panic") || strings.Contains(lower, "failed") ||
			strings.Contains(lower, "warning") {
			interesting = append(interesting, line)
		}
	}
	if len(interesting) == 0 || len(interesting) >= len(lines)*3/4 {
		return "", false
	}
	keep := append([]string{}, lines[:minInt(8, len(lines))]...)
	keep = append(keep, "[non-diagnostic log lines concealed; full output is recoverable from the scratch file]")
	keep = append(keep, interesting...)
	if len(lines) > 8 {
		keep = append(keep, lines[maxInt(8, len(lines)-8):]...)
	}
	p := strings.Join(keep, "\n")
	return p, len(p) < len(s)*9/10
}

func compactDiff(s string) (string, bool) {
	if !strings.Contains(s, "diff --git ") && !strings.HasPrefix(strings.TrimSpace(s), "--- ") {
		return "", false
	}
	lines := strings.Split(s, "\n")
	keep := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "+") ||
			(strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ")) {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		return "", false
	}
	p := strings.Join(keep, "\n")
	return p, len(p) < len(s)*9/10
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

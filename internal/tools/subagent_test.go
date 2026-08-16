package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRunParallelCapsBatchSize(t *testing.T) {
	// codex audit (2026-08-16): a weak model emitting a huge tasks[] array
	// must not fan out to hundreds of child agent loops. runParallel caps the
	// batch and returns a validation error guiding the model to prioritize.
	tool := &subagentTool{
		ws: t.TempDir(),
		run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
			return "result for " + task, nil
		},
	}
	ctx := context.Background()

	// Over-cap batch is rejected before any child runs.
	big := make([]string, maxParallelSubagents+5)
	for i := range big {
		big[i] = fmt.Sprintf("subtask %d", i)
	}
	if _, err := tool.runParallel(ctx, big, nil, SubagentRole{}); err == nil {
		t.Fatal("runParallel accepted an over-cap batch; want a validation error")
	}

	// At-cap batch succeeds.
	ok := make([]string, maxParallelSubagents)
	for i := range ok {
		ok[i] = fmt.Sprintf("subtask %d", i)
	}
	out, err := tool.runParallel(ctx, ok, nil, SubagentRole{})
	if err != nil {
		t.Fatalf("runParallel(at cap): %v", err)
	}
	if strings.Count(out, "### ") != maxParallelSubagents {
		t.Errorf("combined output has %d sections, want %d", strings.Count(out, "### "), maxParallelSubagents)
	}

	// Blank tasks are dropped (a batch of blanks + a few real ones is fine).
	mixed := []string{"", "real-1", "   ", "real-2"}
	out, err = tool.runParallel(ctx, mixed, nil, SubagentRole{})
	if err != nil {
		t.Fatalf("runParallel(mixed blanks): %v", err)
	}
	if strings.Count(out, "### ") != 2 {
		t.Errorf("after dropping blanks, combined output has %d sections, want 2", strings.Count(out, "### "))
	}
}

func TestRunParallelRejectsOversizedTask(t *testing.T) {
	tool := &subagentTool{
		ws: t.TempDir(),
		run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
			return "ok", nil
		},
	}
	huge := strings.Repeat("x", maxSubagentTaskBytes+1)
	if _, err := tool.runParallel(context.Background(), []string{huge}, nil, SubagentRole{}); err == nil {
		t.Fatal("runParallel accepted an oversized task; want a validation error")
	}
}

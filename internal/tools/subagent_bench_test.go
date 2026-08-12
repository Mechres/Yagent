package tools

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkSubagentFanOut measures the parallel dispatch + combine overhead of
// the subagent tasks[] path (the child run fn is a stub, so this is the pure
// fan-out/fan-in floor the parent pays per delegation).
func BenchmarkSubagentFanOut(b *testing.B) {
	tool := &subagentTool{
		ws: b.TempDir(),
		run: func(ctx context.Context, task, ws string, toolset []string, role SubagentRole) (string, error) {
			return "result for " + task, nil
		},
	}
	ctx := context.Background()
	for _, n := range []int{4, 8, 32} {
		tasks := make([]string, n)
		for i := range tasks {
			tasks[i] = fmt.Sprintf("subtask %d", i)
		}
		b.Run(fmt.Sprintf("tasks=%d", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := tool.runParallel(ctx, tasks, nil, SubagentRole{})
				if err != nil || out == "" {
					b.Fatalf("fan-out: %v", err)
				}
			}
		})
	}
}

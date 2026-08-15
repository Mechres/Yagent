package ui

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/tools"
)

type countingApprover struct {
	inner agent.Approver
	calls int
}

func (c *countingApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	c.calls++
	return c.inner.Approve(ctx, call, risk)
}

func TestRememberingApprover(t *testing.T) {
	var inner countingApprover
	inner.inner = autoApprover{}
	ra := newRememberingApprover(&inner)
	call := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_write", Arguments: json.RawMessage(`{"path":"a"}`)}}

	// first call prompts (calls inner), subsequent identical calls auto-approve
	appr, err := ra.Approve(context.Background(), call, tools.RiskWrite)
	if err != nil || !appr.OK {
		t.Fatalf("first: %v %v", appr, err)
	}
	for i := 0; i < 3; i++ {
		appr, err = ra.Approve(context.Background(), call, tools.RiskWrite)
		if err != nil || !appr.OK {
			t.Fatalf("repeat %d: %v %v", i, appr, err)
		}
	}
	if inner.calls != 1 {
		t.Errorf("inner prompted %d times, want 1 (identical repeats auto-approved)", inner.calls)
	}
	// a different call prompts again
	other := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_write", Arguments: json.RawMessage(`{"path":"b"}`)}}
	_, _ = ra.Approve(context.Background(), other, tools.RiskWrite)
	if inner.calls != 2 {
		t.Errorf("inner calls = %d, want 2 (different args prompt)", inner.calls)
	}
}

package bench

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/llm"
)

// fakeChatLLM answers every request with a fixed final message; used to verify
// the measurement-expansion task wiring deterministically.
type fakeChatLLM struct {
	n atomic.Int32
}

func (f *fakeChatLLM) ChatStream(ctx context.Context, messages []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	f.n.Add(1)
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "done"}}, nil
}

func TestTasksIncludesMeasurementExpansion(t *testing.T) {
	names := map[string]bool{}
	for _, tk := range Tasks() {
		names[tk.Name] = true
	}
	for _, want := range []string{
		"edit-recover", "denied-write", "plan-mode", "truncated-recover", "multi-file-refactor",
	} {
		if !names[want] {
			t.Errorf("measurement-expansion task %q missing from Tasks()", want)
		}
	}
}

func TestTruncatingLLMWrapper(t *testing.T) {
	// GPT sol #5 bench wiring: the wrapper cuts the first stream, then passes
	// through — so the agent must nudge and recover on a second request.
	inner := &fakeChatLLM{}
	w := &truncatingLLM{inner: inner}
	_, err := w.ChatStream(context.Background(), nil, nil, func(string) {}, nil)
	if !errors.Is(err, llm.ErrStreamTruncated) {
		t.Fatalf("first call err = %v, want ErrStreamTruncated", err)
	}
	resp, err := w.ChatStream(context.Background(), nil, nil, func(string) {}, nil)
	if err != nil || resp.Message.Content != "done" {
		t.Errorf("second call = %+v, %v; want pass-through 'done'", resp, err)
	}
}

func TestDenyFirstApprover(t *testing.T) {
	d := &denyFirstApprover{}
	a1, _ := d.Approve(context.Background(), llm.ToolCall{}, 0)
	if a1.OK {
		t.Error("first write was not denied")
	}
	a2, _ := d.Approve(context.Background(), llm.ToolCall{}, 0)
	if !a2.OK {
		t.Error("second write was not approved")
	}
}

func TestRunTaskTruncatedRecover(t *testing.T) {
	// Deterministic (no real model): the wrapper truncates request 1, the fake
	// answers request 2 — but the task Check needs the fact, so a fixed "done"
	// fails. This test verifies RunTask survives the truncation path (no panic,
	// returns a Result) rather than asserting a pass.
	var tk Task
	for _, t2 := range Tasks() {
		if t2.Name == "truncated-recover" {
			tk = t2
			break
		}
	}
	if tk.Name == "" {
		t.Fatal("truncated-recover task not found")
	}
	res := RunTask(&fakeChatLLM{}, tk)
	if res.Pass {
		t.Log("note: fake model happened to pass (fact text coincidental)")
	}
	if strings.Contains(res.Detail, "panic") {
		t.Errorf("truncated-recover path panicked: %s", res.Detail)
	}
	if res.Detail == "" {
		t.Error("expected a detail (either pass or a fail reason)")
	}
}

// agent.ChatLLM compile guard: fakeChatLLM and truncatingLLM both satisfy it.
var (
	_ agent.ChatLLM = (*fakeChatLLM)(nil)
	_ agent.ChatLLM = (*truncatingLLM)(nil)
)

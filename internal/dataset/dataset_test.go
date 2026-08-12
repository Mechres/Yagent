package dataset

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
)

func writeSession(t *testing.T, st *memory.Store, msgs []memory.Message) string {
	t.Helper()
	ctx := context.Background()
	sess, err := st.NewSession(ctx, "/tmp/repo")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	for _, m := range msgs {
		if _, err := st.Append(ctx, sess.ID, m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return sess.ID
}

func TestExportOpenAI(t *testing.T) {
	dir := t.TempDir()
	st, err := memory.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	writeSession(t, st, []memory.Message{
		{Role: "user", Content: "where is helper?"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.ToolCallFunction{Name: "index_search", Arguments: []byte(`{"q":"helper"}`)},
		}}},
		{Role: "tool", Content: "found it in pkg/a.go", ToolCallID: "c1"},
		{Role: "assistant", Content: "helper is in pkg/a.go"},
	})

	var buf bytes.Buffer
	n, err := Export(context.Background(), st, &buf, Options{Format: FormatOpenAI})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}
	var line struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls int    `json:"-"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(line.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(line.Messages))
	}
	if line.Messages[3].Role != "assistant" || !strings.Contains(line.Messages[3].Content, "pkg/a.go") {
		t.Errorf("final message = %+v", line.Messages[3])
	}
}

func TestExportFiltersFailedTurns(t *testing.T) {
	dir := t.TempDir()
	st, err := memory.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Two sessions: one healthy, one poisoned with redacted/empty turns.
	writeSession(t, st, []memory.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})
	writeSession(t, st, []memory.Message{
		{Role: "user", Content: "what is the token? api_key=secret123"},
		{Role: "assistant", Content: "here it is: api_key=[redacted]"},
	})

	var buf bytes.Buffer
	n, err := Export(context.Background(), st, &buf, Options{Format: FormatOpenAI})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Only the healthy session survives (the redacted one is dropped entirely).
	if n != 1 {
		t.Fatalf("n = %d, want 1 (redacted session dropped)", n)
	}
	if strings.Contains(buf.String(), "[redacted]") {
		t.Error("dataset leaks a redaction marker")
	}
}

func TestExportShareGPT(t *testing.T) {
	dir := t.TempDir()
	st, err := memory.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	writeSession(t, st, []memory.Message{
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: "done"},
	})

	var buf bytes.Buffer
	n, err := Export(context.Background(), st, &buf, Options{Format: FormatShareGPT})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}
	if !strings.Contains(buf.String(), `"from":"human"`) || !strings.Contains(buf.String(), `"from":"gpt"`) {
		t.Errorf("sharegpt line = %s", buf.String())
	}
}

func TestExportEmptyAssistantDropped(t *testing.T) {
	dir := t.TempDir()
	st, err := memory.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// assistant reply with no content and no tool_calls = aborted turn
	writeSession(t, st, []memory.Message{
		{Role: "user", Content: "anything"},
		{Role: "assistant", Content: ""},
	})

	var buf bytes.Buffer
	n, err := Export(context.Background(), st, &buf, Options{Format: FormatOpenAI})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 (empty assistant turn dropped)", n)
	}
}

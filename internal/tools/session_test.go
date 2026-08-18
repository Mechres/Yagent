package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
)

func TestSessionSearchToolReturnsBoundedHistoricalHits(t *testing.T) {
	st, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, err := st.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(context.Background(), sess.ID, llm.Message{Role: "user", Content: "Remember that deployment uses the staging cluster."}); err != nil {
		t.Fatal(err)
	}
	tool := &sessionSearchTool{store: st}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"staging cluster","k":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, sess.ID) || !strings.Contains(got, "staging") {
		t.Fatalf("session search result = %q", got)
	}
}

func TestSessionSearchToolValidatesLimit(t *testing.T) {
	tool := &sessionSearchTool{}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"x","k":11}`)); err == nil {
		t.Fatal("expected k validation error")
	}
}

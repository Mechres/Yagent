package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yagent/internal/llm"
)

func TestConsultTool(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
			t.Errorf("messages = %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"advisor says yes: do it carefully.\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	reg := NewRegistry(t.TempDir(), Options{Consult: llm.NewClient(ts.URL, "advisor")})
	got := execTool(t, reg, "consult", map[string]any{"question": "should I refactor this?"})
	if !strings.Contains(got, "advisor says yes") {
		t.Errorf("consult = %q", got)
	}
	if got := execTool(t, reg, "consult", map[string]any{"question": ""}); !strings.Contains(got, "validation-error") {
		t.Errorf("consult empty question = %q", got)
	}
}

func TestConsultToolUnconfigured(t *testing.T) {
	tool := &consultTool{client: nil}
	res, err := tool.Execute(ctx(), argsJSON(t, map[string]any{"question": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "not configured") {
		t.Errorf("unconfigured consult = %q", res)
	}
}

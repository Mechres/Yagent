package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yagent/internal/llm"
	"yagent/internal/tools"
)

// ---------- fake LLM server ----------

// scriptedLLM serves one scripted SSE response per request, in order, and
// records every request body so tests can assert what was fed back.
type scriptedLLM struct {
	ts        *httptest.Server
	mu        sync.Mutex
	responses [][]string // per-request: raw SSE data payloads
	requests  [][]byte
}

func newScriptedLLM(t *testing.T, responses [][]string) *scriptedLLM {
	t.Helper()
	s := &scriptedLLM{responses: responses}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, body)
		var resp []string
		if len(s.responses) > 0 {
			resp = s.responses[0]
			s.responses = s.responses[1:]
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, data := range resp {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// finalContent sends the whole answer as a single content delta.
func finalContent(text string) []string {
	return []string{fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text)}
}

// toolCall builds a one-payload response with a full tool call.
func toolCall(id, name, args string) []string {
	return []string{fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, id, name, args)}
}

// chatCompletionRequest mirrors the wire shape for inspecting captured bodies.
type chatCompletionRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// ---------- helpers ----------

type stubApprover struct {
	allow bool
	n     int
}

func (s *stubApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error) {
	s.n++
	return s.allow, nil
}

type captureTokens struct {
	mu  sync.Mutex
	got []string
}

func (c *captureTokens) write(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, s)
}
func (c *captureTokens) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.got, "")
}

func setup(t *testing.T, s *scriptedLLM, allow bool, maxIter int) (*Agent, *stubApprover, *captureTokens, string) {
	t.Helper()
	ws := t.TempDir()
	reg := tools.NewRegistry(ws)
	ap := &stubApprover{allow: allow}
	tok := &captureTokens{}
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, ap, Config{MaxIterations: maxIter, OnToken: tok.write}, ws)
	return a, ap, tok, ws
}

func writeWorkspaceFile(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func toolResultText(a *Agent) []string {
	var out []string
	for _, m := range a.History() {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

// ---------- tests ----------

func TestRunReadsFileAndAnswers(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("call_1", "fs_read", `{"path": "main.go"}`),
		finalContent("found it"),
	})
	a, _, _, ws := setup(t, s, true, 10)
	writeWorkspaceFile(t, ws, "main.go", "package main\n")

	answer, err := a.Run(context.Background(), "read main.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "found it" {
		t.Errorf("answer = %q", answer)
	}
	results := toolResultText(a)
	if len(results) != 1 || !strings.Contains(results[0], "package main") {
		t.Errorf("fs_read result = %q", results)
	}
}

func TestRunDeniedWriteDoesNotExecute(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("call_1", "fs_write", `{"path": "evil.txt", "content": "boom"}`),
		finalContent("ok, i will not"),
	})
	a, ap, _, ws := setup(t, s, false, 10)

	if _, err := a.Run(context.Background(), "write evil.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.n != 1 {
		t.Errorf("approver called %d times, want 1", ap.n)
	}
	if _, err := os.Stat(filepath.Join(ws, "evil.txt")); !os.IsNotExist(err) {
		t.Error("denied fs_write still created the file")
	}
	results := toolResultText(a)
	if len(results) != 1 || !strings.Contains(results[0], "user denied") {
		t.Errorf("denial result = %q", results)
	}
}

func TestRunValidationRetry(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("call_1", "fs_read", `{}`), // missing path
		toolCall("call_2", "fs_read", `{"path": "main.go"}`),
		finalContent("done"),
	})
	a, _, _, ws := setup(t, s, true, 10)
	writeWorkspaceFile(t, ws, "main.go", "package main\n")

	if _, err := a.Run(context.Background(), "read main.go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := toolResultText(a)
	if len(results) != 2 {
		t.Fatalf("tool results = %q, want 2 (validation error + success)", results)
	}
	if !strings.Contains(results[0], `"path" is required`) {
		t.Errorf("validation error text = %q", results[0])
	}
	if !strings.Contains(results[1], "package main") {
		t.Errorf("second result = %q", results[1])
	}
}

func TestRunBlocksToolAfterThreeValidationFails(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{}`),
		toolCall("c2", "fs_read", `{}`),
		toolCall("c3", "fs_read", `{}`),
		toolCall("c4", "fs_read", `{}`), // fourth: blocked
		finalContent("ok"),
	})
	a, _, _, _ := setup(t, s, true, 10)

	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := toolResultText(a)
	if len(results) != 4 {
		t.Fatalf("tool results = %d, want 4", len(results))
	}
	if !strings.Contains(results[2], "failed validation 3 times") {
		t.Errorf("3rd result = %q", results[2])
	}
	if !strings.Contains(results[3], "blocked") {
		t.Errorf("4th result = %q", results[3])
	}
}

func TestRunMaxIterations(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.go"}`),
		toolCall("c2", "fs_read", `{"path": "a.go"}`),
		toolCall("c3", "fs_read", `{"path": "a.go"}`),
		toolCall("c4", "fs_read", `{"path": "a.go"}`),
	})
	a, _, _, ws := setup(t, s, true, 2)
	writeWorkspaceFile(t, ws, "a.go", "x")

	_, err := a.Run(context.Background(), "loop")
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
}

func TestRunUnknownTool(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "nope", `{}`),
		finalContent("ok"),
	})
	a, _, _, _ := setup(t, s, true, 10)

	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := toolResultText(a)
	if len(results) != 1 || !strings.Contains(results[0], "unknown tool") {
		t.Errorf("unknown tool result = %q", results)
	}
}

func TestRunBatchReadOnlyCalls(t *testing.T) {
	// two fs_read calls in one response → executed (concurrently), both results
	// come back.
	both := fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"fs_read","arguments":"{\"path\": \"a.txt\"}"}},{"index":1,"id":"c2","type":"function","function":{"name":"fs_read","arguments":"{\"path\": \"b.txt\"}"}}]}}]}`)
	s := newScriptedLLM(t, [][]string{
		{both},
		finalContent("done"),
	})
	a, _, _, ws := setup(t, s, true, 10)
	writeWorkspaceFile(t, ws, "a.txt", "AAA")
	writeWorkspaceFile(t, ws, "b.txt", "BBB")

	if _, err := a.Run(context.Background(), "read both"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := toolResultText(a)
	if len(results) != 2 {
		t.Fatalf("tool results = %q, want 2", results)
	}
	if !strings.Contains(results[0], "AAA") || !strings.Contains(results[1], "BBB") {
		t.Errorf("batch results = %q", results)
	}
}

func TestRunStreamsAnswer(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("hello world answer")})
	a, _, tok, _ := setup(t, s, true, 10)

	answer, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "hello world answer" {
		t.Errorf("answer = %q", answer)
	}
	if tok.all() != "hello world answer" {
		t.Errorf("streamed tokens = %q", tok.all())
	}
}

func TestRunFeedsToolResultsBack(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{}`), // validation error
		toolCall("c2", "fs_read", `{"path": "a.txt"}`),
		finalContent("ok"),
	})
	a, _, _, ws := setup(t, s, true, 10)
	writeWorkspaceFile(t, ws, "a.txt", "data")

	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// request 2 must contain the validation error text so the model can fix
	s.mu.Lock()
	req2 := string(s.requests[1])
	s.mu.Unlock()
	var req chatCompletionRequest
	if err := json.Unmarshal([]byte(req2), &req); err != nil {
		t.Fatalf("decode req2: %v", err)
	}
	var sawError bool
	for _, m := range req.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, `"path" is required`) {
			sawError = true
		}
	}
	if !sawError {
		t.Error("validation error was not fed back to the model in request 2")
	}
}

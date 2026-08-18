package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mechres/Yagent/internal/capsule"
	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
	"github.com/Mechres/Yagent/internal/web"
)

func TestGrillMutationAllowedOnlyForDocumentation(t *testing.T) {
	tests := []struct {
		name string
		args string
		want bool
	}{
		{"context", `{"path":"CONTEXT.md"}`, true},
		{"adr", `{"path":"docs/adr/0001-choice.md"}`, true},
		{"source", `{"path":"internal/main.go"}`, false},
		{"traversal", `{"path":"docs/adr/../../main.go"}`, false},
		{"patch", `{"path":"docs/adr/choice.md"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := grillMutationAllowed(map[string]string{
				"context": "fs_write", "adr": "fs_write", "source": "fs_write",
				"traversal": "fs_write", "patch": "fs_patch",
			}[tt.name], json.RawMessage(tt.args)); got != tt.want {
				t.Fatalf("grillMutationAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGrillClarifyLimit(t *testing.T) {
	if !grillClarifyAllowed(0) || !grillClarifyAllowed(7) {
		t.Fatal("questions below the limit should be allowed")
	}
	if grillClarifyAllowed(8) || grillClarifyAllowed(9) {
		t.Fatal("questions at or beyond the limit should be rejected")
	}
}

// memoryOpen / memoryOpenVector are thin wrappers so agent tests can use the
// memory package without cluttering call sites.
func memoryOpen(dir string) (*memory.Store, error) { return memory.Open(dir) }

func memoryOpenVector(dir, url, model string) (*memory.VectorStore, error) {
	return memory.OpenVectorStore(dir, url, model)
}

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

// fencedCallJSON builds a content-only response carrying a tool call inside a
// markdown JSON fence (AGY #3) — the "no tool_calls field" slip a 3B-7B model
// makes without a grammar template.
func fencedCallJSON(name, args string) string {
	fence := "```json\n"
	inner := fmt.Sprintf(`{"name": %q, "arguments": %s}`, name, args)
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, fence+inner+"\n```\n")
}

// chatCompletionRequest mirrors the wire shape for inspecting captured bodies.
type chatCompletionRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// ---------- helpers ----------

// fixedSummaryLLM is a ChatLLM that always returns a fixed message; used as
// the summarizer so budget math is deterministic.
type fixedSummaryLLM struct {
	summary string
	calls   int
}

func (f *fixedSummaryLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	f.calls++
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: f.summary}}, nil
}

// TestTypedNilSummarizerDoesNotPanic: a *llm.Client(nil) stored in the
// Summarizer interface is non-nil to == nil but panics on method call. New
// must fall back to the main model (regression: the budget summarizer panicked
// on a typed-nil env.summ from the UI).
func TestTypedNilSummarizerDoesNotPanic(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	main := &fixedSummaryLLM{summary: "unused"}
	var typedNil *llm.Client // nil *llm.Client in the interface
	a := New(main, reg, &stubApprover{allow: true}, Config{MaxIterations: 5, Summarizer: typedNil}, ws)
	if a.summ != main {
		t.Errorf("summ = %T, want the main model (typed-nil must fall back)", a.summ)
	}
}

// failingSummLLM is a summarizer that always errors (a dead offload server).
type failingSummLLM struct{ calls int }

func (f *failingSummLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	f.calls++
	return nil, errors.New("summarizer server unreachable")
}

// truncatingLLM fails the first request with a truncated stream, then answers
// normally (GPT sol #5 — the agent must nudge and continue, not abort).
type truncatingLLM struct {
	mu   sync.Mutex
	n    int
	body string
}

// failAfterNLLM succeeds for the first n requests, then errors — used to force
// the research DONE-check (a separate request) to fail so the unverified-run
// path can be asserted.
type failAfterNLLM struct {
	mu   sync.Mutex
	n    int
	max  int
	body string
}

func (f *failAfterNLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	if n > f.max {
		return nil, errors.New("server hiccup")
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: f.body}}, nil
}

func (t *truncatingLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	t.mu.Lock()
	t.n++
	n := t.n
	t.mu.Unlock()
	if n == 1 {
		return nil, llm.ErrStreamTruncated
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: t.body}}, nil
}

// lengthLLM returns a prose answer with finish_reason=length once, then a full
// one — the agent must nudge the model to continue instead of accepting the
// truncated reply as final.
type lengthLLM struct {
	mu    sync.Mutex
	n     int
	first string
	body  string
}

func (l *lengthLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	l.mu.Lock()
	l.n++
	n := l.n
	l.mu.Unlock()
	if n == 1 {
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: l.first}, FinishReason: "length"}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: l.body}}, nil
}

type stubApprover struct {
	allow bool
	n     int
}

func (s *stubApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (Approval, error) {
	s.n++
	return Approval{OK: s.allow}, nil
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
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	ap := &stubApprover{allow: allow}
	tok := &captureTokens{}
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, ap, Config{MaxIterations: maxIter, OnToken: tok.write}, ws)
	return a, ap, tok, ws
}

func writeWorkspaceFile(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestGreenfieldProfileHidesVerificationSchemasUntilManifestWrite(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("call_1", "fs_write", `{"path":"go.mod","content":"module example.com/new\n\ngo 1.25\n"}`),
		finalContent("scaffolded"),
	})
	a, _, _, _ := setup(t, s, true, 10)

	// An empty directory is a supported greenfield workspace. It gets a
	// profile hint and does not see verification tools that can only return
	// "no project" before a manifest exists.
	if p := a.WorkspaceProfile(); !p.Greenfield() {
		t.Fatalf("initial profile = %#v, want greenfield", p)
	}
	for _, schema := range a.activeToolSchemas("create a Go project", nil) {
		if schema.Function.Name == "workspace_diagnostics" || schema.Function.Name == "test_runner" || schema.Function.Name == "runtime_smoke" {
			t.Fatalf("greenfield schema set unexpectedly includes %q", schema.Function.Name)
		}
	}
	a.SetPlanMode(true)
	for _, schema := range a.activeToolSchemas("create a Go project", nil) {
		if schema.Function.Name == "workspace_diagnostics" || schema.Function.Name == "test_runner" || schema.Function.Name == "runtime_smoke" {
			t.Fatalf("greenfield plan schema set unexpectedly includes %q", schema.Function.Name)
		}
	}
	a.SetPlanMode(false)
	msgs := a.assembleContext("", "")
	if !strings.Contains(msgs[0].Content, "[WORKSPACE PROFILE]\nstate: greenfield") {
		t.Fatalf("system prompt missing greenfield profile: %q", msgs[0].Content)
	}

	if _, err := a.Run(context.Background(), "create a Go project"); err != nil {
		t.Fatal(err)
	}
	if p := a.WorkspaceProfile(); p.Kind != "Go" || !p.VerificationReady {
		t.Fatalf("profile after go.mod write = %#v, want verified Go project", p)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(s.requests))
	}
	second := string(s.requests[1])
	if !strings.Contains(second, `"workspace_diagnostics"`) || !strings.Contains(second, `"test_runner"`) || !strings.Contains(second, `"runtime_smoke"`) {
		t.Errorf("second request did not refresh verification schemas: %s", second)
	}
	if !strings.Contains(second, "project: Go (go.mod)") {
		t.Errorf("second request did not refresh workspace profile: %s", second)
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

// ---------- M3: budget, persistence, recall ----------

// newEmbedServer returns an httptest server with deterministic 2-d embeddings:
// "tab" → (0,1), else (1,0); accepts input as string or array.
func newEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var inputs []string
		if err := json.Unmarshal(req.Input, &inputs); err != nil {
			var one string
			if err2 := json.Unmarshal(req.Input, &one); err2 != nil {
				http.Error(w, "bad input", http.StatusBadRequest)
				return
			}
			inputs = []string{one}
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, 0, len(inputs))
		for i, text := range inputs {
			vec := []float32{1, 0}
			if strings.Contains(text, "tab") {
				vec = []float32{0, 1}
			}
			data = append(data, item{Object: "embedding", Index: i, Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
}

func TestBudgetSummarizesOldestHalf(t *testing.T) {
	// Tiny window forces a summarization at the start of turn 2. The budget
	// must never summarize the current user turn (the Qwythos template 400s on
	// a request with no plain user message), so the running summary covers
	// turn 1 while turn 2's user message stays in the request.
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "big.txt"}`),
		finalContent("done"),
		finalContent("second answer"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "big.txt", strings.Repeat("x", 800)+"\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	summ := &fixedSummaryLLM{summary: "SUM: user prefers tabs"}

	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Window: 300, Reserve: 50, Summarizer: summ},
		ws)

	if _, err := a.Run(context.Background(), "first message that must disappear"); err != nil {
		t.Fatalf("Run turn 1: %v", err)
	}
	if _, err := a.Run(context.Background(), "second message"); err != nil {
		t.Fatalf("Run turn 2: %v", err)
	}
	if summ.calls == 0 {
		t.Fatal("summarizer was never called")
	}
	// The summarized message must be gone and the summary present in turn 2.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) < 3 {
		t.Fatalf("only %d requests captured", len(s.requests))
	}
	req2 := string(s.requests[2]) // turn 2's first request
	if !strings.Contains(req2, "SUM: user prefers tabs") {
		t.Errorf("turn 2 missing running summary: %q", req2[:200])
	}
	if strings.Contains(req2, "first message that must disappear") {
		t.Error("summarized message still in turn 2 context")
	}
	// regression: the request must still contain a plain user message
	// (Qwythos rejects tool-only message lists)
	var req chatCompletionRequest
	if err := json.Unmarshal([]byte(req2), &req); err != nil {
		t.Fatalf("decode req2: %v", err)
	}
	hasUser := false
	for _, m := range req.Messages {
		if m.Role == "user" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Error("turn 2 request lost its user message during budgeting")
	}
}

func TestRunPersistsMessages(t *testing.T) {
	st, err := memoryOpen(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	sess, err := st.NewSession(ctx, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}

	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.txt"}`),
		finalContent("ok"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Store: st, SessionID: sess.ID}, ws)

	if _, err := a.Run(ctx, "read a.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	hist, err := st.History(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant (tool call), tool result, assistant (final answer)
	if len(hist) != 4 {
		t.Fatalf("persisted %d messages, want 4", len(hist))
	}
	if hist[0].Content != "read a.txt" || hist[1].Role != "assistant" || hist[2].Role != "tool" || hist[3].Role != "assistant" {
		t.Errorf("history = %+v", hist)
	}
	if hist[2].ToolCallID != "c1" {
		t.Errorf("tool result tool_call_id = %q", hist[2].ToolCallID)
	}
}

func TestBudgetPersistsSummary(t *testing.T) {
	st, _ := memoryOpen(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/ws")

	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "big.txt"}`),
		finalContent("done"),
		finalContent("second"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "big.txt", strings.Repeat("y", 800)+"\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	summ := &fixedSummaryLLM{summary: "persisted summary text"}
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Window: 300, Reserve: 50, Summarizer: summ, Store: st, SessionID: sess.ID}, ws)

	if _, err := a.Run(ctx, "turn one"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// turn 2 overflows the tiny window and forces the persistence
	if _, err := a.Run(ctx, "turn two"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, until, err := st.Summary(ctx, sess.ID)
	if err != nil || got != "persisted summary text" || until == 0 {
		t.Errorf("stored summary = %q/%d/%v", got, until, err)
	}
}

func TestDedupIdenticalWrite(t *testing.T) {
	// Model calls the SAME write tool+args twice in a row; the second must be
	// skipped, not applied twice.
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_edit", `{"path":"a.txt","old_string":"data","new_string":"dat2"}`),
		toolCall("c2", "fs_edit", `{"path":"a.txt","old_string":"data","new_string":"dat2"}`),
		finalContent("done"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 10}, ws)

	if _, err := a.Run(context.Background(), "edit a.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// the second identical fs_edit must be skipped, not re-applied
	s.mu.Lock()
	defer s.mu.Unlock()
	last := string(s.requests[len(s.requests)-1])
	if !strings.Contains(last, "duplicate of the previous tool call") {
		t.Errorf("skip notice not fed back to the model: %q", last[:400])
	}
}

func TestRepeatedReadNotSkipped(t *testing.T) {
	// A re-read of the same file must NOT be agent-deduped (verify-don't-trust
	// makes re-reads legitimate; a "skipped" notice makes the model retry
	// forever — observed loop on the edit-verify task). The fs_read tool's own
	// cache returns an informative [cached] marker instead.
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.txt"}`),
		toolCall("c2", "fs_read", `{"path": "a.txt"}`),
		finalContent("done"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 10}, ws)

	if _, err := a.Run(context.Background(), "read a.txt twice"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// no "duplicate" skip was injected; the second read yielded the [cached]
	// marker from the fs_read tool cache
	s.mu.Lock()
	defer s.mu.Unlock()
	last := string(s.requests[len(s.requests)-1])
	if strings.Contains(last, "duplicate of the previous tool call") {
		t.Error("a repeated read should not be agent-deduped")
	}
	if !strings.Contains(last, "[cached]") {
		t.Errorf("expected the fs_read [cached] marker on the repeat read: %q", last[:400])
	}
}

func TestRecallInjectedAndSessionDedup(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	dir := t.TempDir()
	vs, err := memoryOpenVector(dir, ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("open vector store: %v", err)
	}
	ctx := context.Background()
	// memory from a past session
	if err := vs.Save(ctx, "user prefers tabs over spaces", "tool", "s-past", 0.5); err != nil {
		t.Fatal(err)
	}
	// memory from THIS session must be deduped out of the injection
	if err := vs.Save(ctx, "current session fact", "tool", "s-current", 0.5); err != nil {
		t.Fatal(err)
	}

	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Vectors: vs, SessionID: "s-current"}, ws)

	if _, err := a.Run(ctx, "what about tabs?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 1 {
		t.Fatalf("requests = %d", len(s.requests))
	}
	req := string(s.requests[0])
	if !strings.Contains(req, "Relevant memories") || !strings.Contains(req, "tabs") {
		t.Errorf("recall not injected: %q", req[:300])
	}
	if strings.Contains(req, "current session fact") {
		t.Error("current-session memory was not deduped")
	}
}

// ---------- M3.5: skills ----------

// openSkills creates a skills store wired to a scratch workspace.
func openSkills(t *testing.T) *skills.Store {
	t.Helper()
	sk, err := skills.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("open skills store: %v", err)
	}
	return sk
}

func validSkillBody(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" +
		"## When to Use\nwhen asked\n## Procedure\n1. do it\n## Verification\nok\n"
}

func TestRunOffersSkillCreationAfterComplexTurn(t *testing.T) {
	sk := openSkills(t)
	createArgs, _ := json.Marshal(map[string]any{
		"action": "create", "name": "read-a",
		"content": validSkillBody("read-a", "read file a when needed"),
	})
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.go"}`),
		toolCall("c2", "fs_read", `{"path": "a.go"}`),
		toolCall("c3", "fs_read", `{"path": "a.go"}`),
		toolCall("c4", "fs_read", `{"path": "a.go"}`),
		toolCall("c5", "fs_read", `{"path": "a.go"}`),
		finalContent("done"),
		toolCall("s1", "skill_manage", string(createArgs)),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.go", "package main\n")
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})
	ap := &stubApprover{allow: true}
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, ap, Config{MaxIterations: 10, Skills: sk}, ws)

	answer, err := a.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "done" {
		t.Errorf("answer = %q", answer)
	}
	// skill_manage is self-gated: it must not hit the generic y/n approver
	if ap.n != 0 {
		t.Errorf("approver called %d times for a self-gated skill write", ap.n)
	}
	pending, err := sk.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Action != "create" || pending[0].Name != "read-a" {
		t.Fatalf("pending = %+v, want the proposed skill staged", pending)
	}
	if sk.Exists("read-a") {
		t.Error("skill applied before approval")
	}
}

func TestRunNoSkillOpportunityForSimpleTurn(t *testing.T) {
	sk := openSkills(t)
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.go"}`),
		finalContent("done"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.go", "x")
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 10, Skills: sk}, ws)

	if _, err := a.Run(context.Background(), "small task"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 2 {
		t.Errorf("requests = %d, want 2 (task + answer); a sub-5-call turn must not add an opportunity request", len(s.requests))
	}
	if pl, _ := sk.ListPending(); len(pl) != 0 {
		t.Errorf("unexpected staged writes: %+v", pl)
	}
}

func TestRunInjectsSkillIndex(t *testing.T) {
	sk := openSkills(t)
	if _, err := sk.Apply(skills.Op{Action: skills.ActionCreate, Name: "code-review-go",
		Content: validSkillBody("code-review-go", "review Go code")}); err != nil {
		t.Fatal(err)
	}
	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 5, Skills: sk}, ws)

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[0])
	if !strings.Contains(req, "Available skills") || !strings.Contains(req, "code-review-go") {
		t.Errorf("L0 skills index not injected: %q", req[:300])
	}
}

func TestFinishOffersSkillCreation(t *testing.T) {
	sk := openSkills(t)
	createArgs, _ := json.Marshal(map[string]any{
		"action": "create", "name": "session-skill",
		"content": validSkillBody("session-skill", "session skill"),
	})
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.go"}`),
		finalContent("done"),
		toolCall("s1", "skill_manage", string(createArgs)),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.go", "x")
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 10, Skills: sk}, ws)

	if _, err := a.Run(context.Background(), "task"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := a.Finish(context.Background()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	pending, _ := sk.ListPending()
	if len(pending) != 1 || pending[0].Name != "session-skill" {
		t.Errorf("pending after Finish = %+v", pending)
	}
}

func TestInjectSystemAppearsInNextRequest(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	a, _, _, _ := setup(t, s, true, 5)
	a.InjectSystem("SKILL CONTENT: run the checklist")

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[0])
	if !strings.Contains(req, "SKILL CONTENT: run the checklist") {
		t.Error("injected skill content missing from the request")
	}
}

func TestInjectSystemScopedCleansUp(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("one"), finalContent("two")})
	a, _, _, _ := setup(t, s, true, 5)
	cleanup := a.InjectSystemScoped("TEMPORARY GRILL RULE")
	if _, err := a.Run(context.Background(), "first"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	cleanup()
	if _, err := a.Run(context.Background(), "second"); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(string(s.requests[0]), "TEMPORARY GRILL RULE") {
		t.Fatal("scoped injection missing from first request")
	}
	if strings.Contains(string(s.requests[1]), "TEMPORARY GRILL RULE") {
		t.Fatal("scoped injection leaked into second request")
	}
}

// ---------- M4: code index ----------

func TestRunInjectsCodeIndex(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, ws, "pkg/tool.go", `package pkg

// validateToolInput checks tool arguments before dispatch.
func validateToolInput(name string) error {
	return nil
}
`)
	idx, err := index.Open(ws, t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	if _, err := idx.Index(context.Background()); err != nil {
		t.Fatalf("Index: %v", err)
	}

	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	reg := tools.NewRegistry(ws, tools.Options{Index: idx})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Index: idx, IndexAutoInject: true}, ws)

	if _, err := a.Run(context.Background(), "where is tool validation implemented?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[0])
	if !strings.Contains(req, "Relevant code from the workspace index") || !strings.Contains(req, "pkg/tool.go") {
		t.Errorf("code index not injected: %q", req[:400])
	}
}

func TestRunSkipsEmptyIndexInjection(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	ws := t.TempDir()
	idx, err := index.Open(ws, t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	reg := tools.NewRegistry(ws, tools.Options{Index: idx})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Index: idx, IndexAutoInject: true}, ws)

	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[0])
	if strings.Contains(req, "Relevant code from the workspace index") {
		t.Error("empty index must not be injected")
	}
}

// ---------- M3.5: verification harness ----------

func TestRepoInstructionsInSystemPrompt(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "AGENTS.md", "REPO-RULE: always use tabs")
	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	joined := ""
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	if !strings.Contains(joined, "REPO-RULE: always use tabs") {
		t.Errorf("AGENTS.md not folded into the system prompt: %q", joined[:min(len(joined), 200)])
	}
}

func TestRepoInstructionsPrecedenceAndCap(t *testing.T) {
	// precedence: .yagent/instructions.md > AGENTS.md
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".yagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, ws, ".yagent/instructions.md", "PROJECT-RULE")
	writeWorkspaceFile(t, ws, "AGENTS.md", "REPO-RULE")
	prompt := buildSystemPrompt(ws)
	if !strings.Contains(prompt, "PROJECT-RULE") || strings.Contains(prompt, "REPO-RULE") {
		t.Errorf("precedence wrong: %q", prompt)
	}

	// oversized instructions are capped with a marker
	ws2 := t.TempDir()
	writeWorkspaceFile(t, ws2, "AGENTS.md", strings.Repeat("x", maxInstructionsBytes+1000))
	prompt2 := buildSystemPrompt(ws2)
	if !strings.Contains(prompt2, "truncated") {
		t.Errorf("cap marker missing (len %d)", len(prompt2))
	}
	if idx := strings.Index(prompt2, "Developer instructions from"); idx >= 0 {
		if n := strings.Count(prompt2[idx:], "x"); n > maxInstructionsBytes {
			t.Errorf("instructions not capped: %d x chars", n)
		}
	} else {
		t.Error("instructions section missing from the prompt")
	}
}

func TestNestedRepoInstructionsArriveAfterTouchedPath(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "src/AGENTS.md", "SRC-RULE: keep this package small")
	writeWorkspaceFile(t, ws, "src/main.go", "package main\n")
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path":"src/main.go"}`),
		finalContent("ok"),
	})
	a := New(llm.NewClient(s.ts.URL, "test-model"), tools.NewRegistry(ws, tools.Options{}), &stubApprover{allow: true}, Config{MaxIterations: 4}, ws)
	if _, err := a.Run(context.Background(), "inspect src/main.go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) < 2 || !strings.Contains(string(s.requests[1]), "SRC-RULE: keep this package small") {
		t.Fatalf("nested instructions missing from follow-up context; requests=%d", len(s.requests))
	}
}

func TestCodegenModeAppendsPromptSuffix(t *testing.T) {
	// The system prompt carries the greenfield-code strategy only when Codegen
	// is enabled — the loop can't accidentally codegen a pure chat turn.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	client := llm.NewClient("http://127.0.0.1:1", "test-model")

	off := New(client, reg, nil, Config{MaxIterations: 4}, ws)
	if strings.Contains(off.systemPrompt, "Codegen mode") {
		t.Error("codegen suffix present without Codegen enabled")
	}

	on := New(client, reg, nil, Config{MaxIterations: 4, Codegen: true}, ws)
	if !strings.Contains(on.systemPrompt, "Codegen mode") {
		t.Error("codegen suffix missing with Codegen enabled")
	}
	if !strings.Contains(on.systemPrompt, "SINGLE complete fs_write") {
		t.Error("whole-file write rule missing from the codegen suffix")
	}
}

func TestBudgetPrunesToolOutputsBeforeSummarizing(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Window: 8000, Reserve: 1000}, ws)
	bigTool := strings.Repeat("tool result line\n", 1600) // ~32 KiB of output
	a.mu.Lock()
	a.history = []historyEntry{
		{msg: llm.Message{Role: "user", Content: "first instruction keep me"}},
		{msg: llm.Message{Role: "tool", Content: bigTool, ToolCallID: "c1"}},
		{msg: llm.Message{Role: "tool", Content: bigTool, ToolCallID: "c2"}},
		{msg: llm.Message{Role: "user", Content: "current turn"}},
	}
	for i := range a.history {
		a.history[i].tokens = len(a.history[i].msg.Content)/4 + len(a.history[i].msg.ToolCallID)/4
	}
	a.mu.Unlock()

	if err := a.budget(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	// tool messages are pruned to markers; the summarizer must NOT have run
	toolPruned := 0
	for _, h := range a.history {
		if h.msg.Role == "tool" {
			if !strings.Contains(h.msg.Content, "tool output concealed") {
				t.Errorf("tool message not pruned: %q…", h.msg.Content[:min(len(h.msg.Content), 40)])
			}
			toolPruned++
		}
	}
	if toolPruned != 2 {
		t.Errorf("pruned %d tool messages, want 2", toolPruned)
	}
	// the user's instruction survives (no summarization happened)
	found := false
	for _, h := range a.history {
		if strings.Contains(h.msg.Content, "first instruction keep me") {
			found = true
		}
	}
	if !found {
		t.Error("user instruction was summarized away instead of preserved")
	}
}

func TestVramPressureDetectAndPrune(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Window: 8000, Reserve: 1000, VramThresholdTPS: 5.0}, ws)

	// A slow stream (enough tokens over a long wall time) must flag pressure.
	a.detectVramPressure(time.Now().Add(-10*time.Second), 70, 30) // 100/10=10 t/s > 5: not flagged
	if a.ContextPressure() {
		t.Fatal("10 t/s flagged VRAM pressure")
	}
	// a genuinely slow, sustained stream: 40 tokens over 20s = 2 t/s < 5
	a.detectVramPressure(time.Now().Add(-20*time.Second), 40, 0)
	if !a.ContextPressure() {
		t.Fatal("slow sustained stream did not flag VRAM pressure")
	}
	// a tiny stream (warm-up / one-liner) must NOT flag even if slow
	a.mu.Lock()
	a.pressure = false
	a.mu.Unlock()
	a.detectVramPressure(time.Now().Add(-10*time.Second), 10, 0) // 1 t/s but < 32 tokens
	if a.ContextPressure() {
		t.Fatal("tiny slow stream flagged VRAM pressure (warm-up false positive)")
	}

	// A fast stream clears nothing but never flags.
	a.mu.Lock()
	a.pressure = false
	a.mu.Unlock()
	a.detectVramPressure(time.Now(), 2000, 1000) // very high t/s
	if a.ContextPressure() {
		t.Fatal("fast stream flagged VRAM pressure")
	}

	// Now prime a small history and a real pressure flag: budget() must prune
	// tool output even though the context is UNDER the window (the force-prune
	// path is the whole point of the feature).
	bigTool := strings.Repeat("tool result line\n", 1600)
	a.mu.Lock()
	a.history = []historyEntry{
		{msg: llm.Message{Role: "user", Content: "first instruction keep me"}},
		{msg: llm.Message{Role: "tool", Content: bigTool, ToolCallID: "c1"}},
		{msg: llm.Message{Role: "tool", Content: bigTool, ToolCallID: "c2"}},
		{msg: llm.Message{Role: "user", Content: "current turn"}},
	}
	for i := range a.history {
		a.history[i].tokens = len(a.history[i].msg.Content)/4 + len(a.history[i].msg.ToolCallID)/4
	}
	a.pressure = true
	a.mu.Unlock()

	if err := a.budget(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pressure {
		t.Error("budget did not consume the pressure flag")
	}
	pruned := 0
	for _, h := range a.history {
		if h.msg.Role == "tool" && strings.Contains(h.msg.Content, "tool output concealed") {
			pruned++
		}
	}
	if pruned != 2 {
		t.Errorf("force-pruned %d tool messages, want 2 (pressure prune under window)", pruned)
	}
}

func TestReadResultCache(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{}, reg, &stubApprover{allow: true},
		Config{MaxIterations: 3, Window: 8000, Reserve: 1000}, ws)

	// canonical key: key order / whitespace in args must share a cache entry
	a.cacheReadResult("grep", json.RawMessage(`{"pattern":"foo","path":"."}`), "a.go:1")
	if got, ok := a.cachedReadResult("grep", json.RawMessage("{\"path\":\".\",\"pattern\":\"foo\"}")); !ok || got != "a.go:1" {
		t.Errorf("cache miss on canonical args: got=%q ok=%t", got, ok)
	}
	// different args = different entry
	if _, ok := a.cachedReadResult("grep", json.RawMessage(`{"pattern":"bar"}`)); ok {
		t.Error("different args hit the cache")
	}
	// invalidate drops everything
	a.invalidateReadCache()
	if _, ok := a.cachedReadResult("grep", json.RawMessage(`{"pattern":"foo","path":"."}`)); ok {
		t.Error("cache survived invalidation")
	}
}

func TestDispatchCachesAndInvalidatesReads(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "content-xyz\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	// grep twice, then a write (which must invalidate), then grep again — the
	// post-write grep must see the new file.
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "grep", `{"pattern":"content-xyz"}`),
		toolCall("c2", "grep", `{"pattern":"content-xyz"}`),
		toolCall("c3", "fs_write", `{"path":"b.txt","content":"content-xyz\n"}`),
		toolCall("c4", "grep", `{"pattern":"content-xyz"}`),
		finalContent("found after write"),
	})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true},
		Config{MaxIterations: 12, Window: 8000, Reserve: 1000}, ws)

	if _, err := a.Run(context.Background(), "search twice then write then search"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// grep #2 should have hit the cache (result "a.txt:1"), grep #4 after the
	// write must re-run and see both files.
	a.mu.RLock()
	found := false
	for _, h := range a.history {
		if h.msg.Role == "tool" && strings.Contains(h.msg.Content, "b.txt") {
			found = true
		}
	}
	a.mu.RUnlock()
	if !found {
		t.Error("post-write grep did not see the new file (cache not invalidated)")
	}
}

func TestCompactDistillsWholeHistory(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	summ := &fixedSummaryLLM{summary: "[SESSION LEDGER]\n- fact: tabs preferred\n- active: fixing bug"}
	a := New(summ, reg, &stubApprover{allow: true},
		Config{MaxIterations: 3, Window: 8000, Reserve: 1000, Summarizer: summ}, ws)

	a.mu.Lock()
	a.history = []historyEntry{
		{msg: llm.Message{Role: "user", Content: "turn 1: remember I prefer tabs"}},
		{msg: llm.Message{Role: "assistant", Content: "ok, tabs noted"}},
		{msg: llm.Message{Role: "tool", Content: strings.Repeat("big tool output\n", 200), ToolCallID: "c1"}},
		{msg: llm.Message{Role: "user", Content: "turn 2: now fix the bug in parse()"}},
		{msg: llm.Message{Role: "assistant", Content: "working on it"}},
		{msg: llm.Message{Role: "user", Content: "current turn: please report"}},
	}
	for i := range a.history {
		a.history[i].tokens = len(a.history[i].msg.Content)/4 + len(a.history[i].msg.ToolCallID)/4
	}
	a.mu.Unlock()

	note, err := a.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if summ.calls != 1 {
		t.Errorf("summarizer called %d times, want 1", summ.calls)
	}
	if !strings.Contains(note, "compacted 5 historical message") {
		t.Errorf("note = %q", note)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	// only the current user turn remains in history
	if len(a.history) != 1 || a.history[0].msg.Role != "user" || a.history[0].msg.Content != "current turn: please report" {
		t.Errorf("history after compact = %+v", a.history)
	}
	if a.runningSummary != summ.summary {
		t.Errorf("running summary = %q", a.runningSummary)
	}
}

func TestVerifySkillPass(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.txt"}`),
		finalContent("PASS the file exists"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	skillContent := "---\nname: read-a\ndescription: read file a\n---\n## When to Use\nwhen asked\n## Procedure\n1. read a.txt\n## Verification\nread a.txt\n"

	answer, err := VerifySkill(context.Background(), client, reg, &stubApprover{allow: true}, skillContent, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "PASS") {
		t.Errorf("answer = %q, want PASS", answer)
	}
	if ParseVerdict(answer) != "PASS" {
		t.Errorf("verdict = %q (answer %q)", ParseVerdict(answer), answer)
	}
	// the staged skill content must be in the request context
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[0])
	if !strings.Contains(req, "Staged skill to verify") || !strings.Contains(req, "## Verification") {
		t.Errorf("skill content not injected: %q", req[:300])
	}
}

func TestRunSubagentRolePrompt(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("architect summary")})
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{ReadOnly: true, SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	role, _ := tools.RoleByName("architect")
	answer, _, err := RunSubagent(context.Background(), client, reg, "review the design", ws, role)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "architect summary" {
		t.Errorf("answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[0])
	if !strings.Contains(req, role.Prompt) || !strings.Contains(req, "You are an ARCHITECT") {
		t.Errorf("role prompt not injected into the child context: %q", req[:min(len(req), 200)])
	}
}

func TestFencedToolCallExtractor(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, &stubApprover{allow: true}, Config{}, ws)

	// a fenced JSON tool call -> extracted
	tc := a.fencedToolCallExtractor("I will read it:\n```json\n{\"name\": \"fs_read\", \"arguments\": {\"path\": \"main.go\"}}\n```\n")
	if tc == nil {
		t.Fatal("fenced tool call not extracted")
	}
	if tc.Function.Name != "fs_read" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	if !strings.Contains(string(tc.Function.Arguments), "main.go") {
		t.Errorf("args = %s", tc.Function.Arguments)
	}
	// "tool"/"parameters" alternate shape
	tc = a.fencedToolCallExtractor("```\n{\"tool\": \"glob\", \"parameters\": {\"pattern\": \"*.go\"}}\n```")
	if tc == nil || tc.Function.Name != "glob" {
		t.Errorf("alternate shape not extracted: %+v", tc)
	}
	// unknown tool name -> not extracted (left as content)
	if tc := a.fencedToolCallExtractor("```json\n{\"name\": \"not_a_tool\", \"arguments\": {}}\n```"); tc != nil {
		t.Errorf("unknown tool extracted: %+v", tc)
	}
	// no fence -> not extracted
	if tc := a.fencedToolCallExtractor(`{"name": "fs_read", "arguments": {"path": "main.go"}}`); tc != nil {
		t.Errorf("unfenced JSON extracted: %+v", tc)
	}
	// plain prose -> not extracted
	if tc := a.fencedToolCallExtractor("I will fs_read main.go."); tc != nil {
		t.Errorf("prose extracted: %+v", tc)
	}
}

func TestFencedToolCallExecutedInTurn(t *testing.T) {
	// AGY #3: a fenced tool call executes on the same turn (approval applies),
	// instead of burning a round-trip on the prose nudge.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "main.go", "package main\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	s := newScriptedLLM(t, [][]string{
		{fencedCallJSON("fs_read", `{"path": "main.go"}`)},
		finalContent("done reading"),
	})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	if _, err := a.Run(context.Background(), "read main.go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := toolResultText(a)
	if len(results) != 1 || !strings.Contains(results[0], "package main") {
		t.Errorf("tool results = %q, want the fs_read result", results)
	}
}

func TestProseToolNudge(t *testing.T) {
	// narrated intent -> nudge
	if n := proseToolNudge("I will fs_read main.go and look at it."); !strings.Contains(n, "fs_read") {
		t.Errorf("nudge missing for narrated read: %q", n)
	}
	if n := proseToolNudge("Let me use grep to search for the symbol."); !strings.Contains(n, "grep") {
		t.Errorf("nudge missing for use-grep: %q", n)
	}
	// past tense (already did it) -> no nudge
	if n := proseToolNudge("I used fs_read to read the file and it worked."); n != "" {
		t.Errorf("past-tense narration should not nudge: %q", n)
	}
	// no intent word -> no nudge
	if n := proseToolNudge("The fs_read tool returns file contents."); n != "" {
		t.Errorf("no-intent mention should not nudge: %q", n)
	}
	// a tool name inside a code fence -> no nudge
	if n := proseToolNudge("Here is an example:\n```go\n// we will call fs_read here\n```"); n != "" {
		t.Errorf("fenced code should not nudge: %q", n)
	}
}

func TestPlanNarrationStall(t *testing.T) {
	// future-tense remaining-work narration -> stall
	cases := []struct {
		in   string
		want bool
	}{
		{"I wrote main.go. Next steps: add input handling.", true},
		{"To finish, you can add collision detection.", true},
		{"Done. Remaining work: wire up the game loop.", true},
		{"From here, implement the rotate method, then add scoring.", true},
		{"The program is complete and compiles cleanly.", false},
		{"Here is the finished main.go:\n```go\nfunc main() {}\n```", false},
		{"I used fs_write to write the whole file in one call.", false},
	}
	for _, c := range cases {
		if got := planNarrationStall(c.in); got != c.want {
			t.Errorf("planNarrationStall(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRunNudgesProseToolCall(t *testing.T) {
	// the model narrates fs_read in prose; the loop nudges, then the model
	// emits the real call and answers.
	s := newScriptedLLM(t, [][]string{
		finalContent("I will fs_read a.txt now."),
		toolCall("c1", "fs_read", `{"path": "a.txt"}`),
		finalContent("a.txt contains payload."),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "payload")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 10}, ws)

	answer, err := a.Run(context.Background(), "read a.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(answer, "payload") {
		t.Errorf("answer = %q", answer)
	}
	// the nudge must have been fed back as a user message
	s.mu.Lock()
	defer s.mu.Unlock()
	req := string(s.requests[1])
	if !strings.Contains(req, "You narrated a tool call") {
		t.Errorf("nudge not in the second request: %q", req[:min(len(req), 300)])
	}
}

func TestVerifyBarrierRunsDiagnostics(t *testing.T) {
	// The model writes a syntax-valid but compile-broken Go file (missing fmt
	// import — pre-flight syntax validation can't catch this) and claims done.
	// The deterministic verify barrier runs go vet, injects the failure, and
	// only then lets the turn finish.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module bench\n\ngo 1.22\n")
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_write", `{"path":"main.go","content":"package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"}`),
		finalContent("done with the change"),
		finalContent("fixed the import"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 8, VerifyWrites: true}, ws)

	answer, err := a.Run(context.Background(), "write a go program that prints hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "fixed the import" {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "Deterministic verification ran it now") {
		t.Error("verify barrier did not inject the diagnostics result")
	}
	if !strings.Contains(joined, "undefined") {
		t.Error("the injected verification did not carry the go vet failure")
	}
}

func TestTaskLedgerTracksProgress(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "x")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	a.mu.Lock()
	a.touchedPaths = []string{"a.txt"}
	a.lastToolError = "error: old_string not found [class=old_string_not_found]"
	a.mu.Unlock()

	ledger := a.taskLedger()
	if !strings.Contains(ledger, "changed: a.txt") || !strings.Contains(ledger, "old_string_not_found") {
		t.Errorf("ledger = %q", ledger)
	}
	joined := ""
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	if !strings.Contains(joined, "TASK STATE") || !strings.Contains(joined, "changed: a.txt") {
		t.Errorf("ledger not injected into the context: %q", joined[:min(len(joined), 300)])
	}
	// a fresh agent with no work has no ledger
	a2 := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	if l := a2.taskLedger(); l != "" {
		t.Errorf("empty agent should have no ledger: %q", l)
	}
}

func TestAgentLoopGuardStopsRepetition(t *testing.T) {
	// The model emits a repeating answer; the agent-side loop guard cancels it,
	// feeds back a stop-repeating nudge, and the next response becomes final.
	repeat := strings.Repeat("I need to check the file. ", 5) // 25-char unit ×5
	s := newScriptedLLM(t, [][]string{
		finalContent(repeat),
		finalContent("final answer"),
	})
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 8}, ws)

	answer, err := a.Run(context.Background(), "do something")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "final answer" {
		t.Errorf("final answer = %q", answer)
	}
	// the stop-repeating nudge must have been fed back
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "began repeating") {
		t.Error("loop guard nudge not injected")
	}
}

func TestProsePermissionNudge(t *testing.T) {
	if n := prosePermissionNudge("Should I review the code first?"); !strings.Contains(n, "clarify") {
		t.Errorf("should-I nudge missing: %q", n)
	}
	if n := prosePermissionNudge("I need to ask you whether you want me to implement it."); n == "" {
		t.Error("need-to-ask nudge missing")
	}
	if n := prosePermissionNudge("Here is the complete report."); n != "" {
		t.Errorf("plain answer should not nudge: %q", n)
	}
	// A quoted phrase ("Should I Use?" as a paper title) must NOT trip the
	// detector — quoted content is a citation, not a permission ask.
	if n := prosePermissionNudge(`The paper "Which Quantization Should I Use?" recommends q4_0 — source: http://arxiv.org/abs/1`); n != "" {
		t.Errorf("quoted should-I must not nudge: %q", n)
	}
	if n := prosePermissionNudge("See 'How should I tune top_k?' in the docs at https://example.com"); n != "" {
		t.Errorf("single-quoted should-I must not nudge: %q", n)
	}
	if n := prosePermissionNudge("```\nshould i = question\n```"); n != "" {
		t.Errorf("fenced should-I must not nudge: %q", n)
	}
	// Unquoted question marks still fire.
	if n := prosePermissionNudge("Should I review the code first?"); !strings.Contains(n, "clarify") {
		t.Errorf("unquoted should-I nudge missing: %q", n)
	}
}

func TestRunNudgesProsePermissionAsk(t *testing.T) {
	// the model stalls asking permission in prose; the nudge makes it finish.
	s := newScriptedLLM(t, [][]string{
		finalContent("Should I review the code first?"),
		finalContent("Here is the review."),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.go", "package demo\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 8}, ws)

	answer, err := a.Run(context.Background(), "review the code and list improvements")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "Here is the review." {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "asking for permission") {
		t.Error("stall nudge not injected")
	}
}

func TestToolLoopBreaker(t *testing.T) {
	// the model re-runs glob 6+ times instead of converging; the breaker nudges
	// it to answer.
	// varying args so tool-call dedup doesn't swallow them (a real stuck model
	// varies glob patterns too).
	steps := [][]string{}
	for i := 0; i < 6; i++ {
		steps = append(steps, toolCall(fmt.Sprintf("g%d", i), "glob", fmt.Sprintf(`{"pattern":"**/%d*.go"}`, i)))
	}
	steps = append(steps, finalContent("done"))
	s := newScriptedLLM(t, steps)
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 15}, ws)

	answer, err := a.Run(context.Background(), "explore the repo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "done" {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "without converging") {
		t.Error("tool-loop nudge not injected")
	}
}

func TestConvergenceNudge(t *testing.T) {
	// 12+ read-only calls with no write and no answer -> convergence nudge.
	steps := [][]string{}
	for i := 0; i < 12; i++ {
		steps = append(steps, toolCall(fmt.Sprintf("r%d", i), "fs_read", fmt.Sprintf(`{"path":"file%d.txt"}`, i)))
	}
	steps = append(steps, finalContent("done"))
	s := newScriptedLLM(t, steps)
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 20}, ws)

	answer, err := a.Run(context.Background(), "review everything")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "done" {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "extensive exploration") {
		t.Error("convergence nudge not injected")
	}
}

func TestNearCapConvergenceNudge(t *testing.T) {
	// Residual stress-test failure (2026-08-13): the model does all the work
	// (writes a file) but keeps requesting tools until the iteration cap — it
	// never emits the closing answer. With a small MaxIterations, the loop must
	// nudge it to stop and summarize (a write happened, so the read-only
	// convergence nudge never fires).
	steps := [][]string{
		toolCall("w1", "fs_write", `{"path":"a.txt","content":"new content"}`),
		toolCall("r1", "fs_read", `{"path":"a.txt"}`),
		toolCall("r2", "fs_read", `{"path":"a.txt"}`),
		toolCall("r3", "grep", `{"pattern":"content"}`),
		finalContent("done the work"),
	}
	s := newScriptedLLM(t, steps)
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	// MaxIterations 5: the near-cap branch (i >= 5-2=3) fires after the grep
	// dispatches, before the final answer.
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	answer, err := a.Run(context.Background(), "update the file and verify")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "done the work" {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "near the iteration limit") {
		t.Error("near-cap convergence nudge not injected")
	}
}

// TestReReadLoopNudge: a heavy-reasoning model re-reads the SAME file 4+ times
// (each re-read returns the "[cached] unchanged" marker, so it gathers nothing
// new) and never writes — the re-read loop nudge must fire and steer it to act.
func TestReReadLoopNudge(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "main.cpp", "int main() { return 0; }\n")
	steps := [][]string{
		toolCall("r1", "fs_read", `{"path":"main.cpp"}`),
		toolCall("r2", "fs_read", `{"path":"main.cpp"}`),
		toolCall("r3", "fs_read", `{"path":"main.cpp"}`),
		toolCall("r4", "fs_read", `{"path":"main.cpp"}`),
		finalContent("Editing main.cpp now — the nudge worked."),
	}
	s := newScriptedLLM(t, steps)
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 12}, ws)

	answer, err := a.Run(context.Background(), "understand main.cpp and then act")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "Editing main.cpp now — the nudge worked." {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "re-read") && !strings.Contains(joined, "unchanged files return") {
		t.Error("re-read loop nudge not injected")
	}
}

// TestNearCapReadOnlyNudge: a planning-only loop (reads, no writes) near the
// iteration cap must still be nudged — the old near-cap branch only fired when
// a write happened, so this pattern (Nemotron-3-Nano planning) burned the whole
// budget silently.
func TestNearCapReadOnlyNudge(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "content\n")
	steps := [][]string{
		toolCall("r1", "fs_read", `{"path":"a.txt"}`),
		toolCall("r2", "grep", `{"pattern":"content"}`),
		toolCall("r3", "fs_read", `{"path":"a.txt"}`),
		finalContent("found it"),
	}
	s := newScriptedLLM(t, steps)
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 4}, ws)

	answer, err := a.Run(context.Background(), "check the file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "found it" {
		t.Errorf("final answer = %q", answer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "only been planning and reading") {
		t.Error("read-only near-cap nudge not injected")
	}
}

func TestFailedWriteLoopNudge(t *testing.T) {
	// Real-use loop (2026-08-13): the model repeatedly attempts the same
	// fs_edit with a wrong old_string, interleaving fs_read between attempts
	// (which defeats the consecutive dedup). After N identical failures the
	// loop must nudge the model to re-read the exact text.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "main.cpp", "int main() { return 0; }\n")
	steps := [][]string{}
	for i := 0; i < 5; i++ {
		steps = append(steps,
			toolCall(fmt.Sprintf("r%d", i), "fs_read", `{"path":"main.cpp"}`),
			toolCall(fmt.Sprintf("e%d", i), "fs_edit", `{"path":"main.cpp","old_string":"int main() { returrn 0; }","new_string":"int main() { return 0; }"}`),
		)
	}
	steps = append(steps, finalContent("done"))
	s := newScriptedLLM(t, steps)
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 20}, ws)

	if _, err := a.Run(context.Background(), "fix the typo in main.cpp"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "failed to edit") {
		t.Error("failed-write loop nudge not injected")
	}
}

func TestSubagentOffloadNudge(t *testing.T) {
	// Proposal #8 (2026-08-13): heavy read-only exploration at high context
	// usage must nudge the model to delegate to a subagent instead of reading
	// more files inline. A tiny window forces >75% usage after a few reads.
	steps := [][]string{}
	for i := 0; i < 8; i++ {
		steps = append(steps, toolCall(fmt.Sprintf("r%d", i), "fs_read", fmt.Sprintf(`{"path":"file%d.txt"}`, i)))
	}
	steps = append(steps, finalContent("done"))
	s := newScriptedLLM(t, steps)
	ws := t.TempDir()
	for i := 0; i < 8; i++ {
		writeWorkspaceFile(t, ws, fmt.Sprintf("file%d.txt", i), "x")
	}
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	// small window + lots of injected history per read -> context pressure
	a := New(client, reg, &stubApprover{allow: true}, Config{MaxIterations: 20, Window: 400, Reserve: 40}, ws)

	if _, err := a.Run(context.Background(), "explore all files"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	joined := ""
	for _, b := range s.requests {
		joined += string(b)
	}
	if !strings.Contains(joined, "Context utilization is high") {
		t.Error("subagent offload nudge not injected")
	}
	if !strings.Contains(joined, "subagent(") {
		t.Error("offload nudge should mention the subagent tool")
	}
}

func TestSummarizerFallbackToMain(t *testing.T) {
	// A configured-but-unreachable summarizer (e.g. a laptop that went offline)
	// must not break the turn — budget() falls back to the main model (2026-08-13
	// bugfix: every turn errored with "summarize history: connection refused").
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	main := &fixedSummaryLLM{summary: "main-model summary"}
	fail := &failingSummLLM{}
	a := New(main, reg, &stubApprover{allow: true},
		Config{MaxIterations: 3, Window: 8000, Reserve: 1000, Summarizer: fail}, ws)

	// Force budget to summarize: history over the limit even AFTER tool-output
	// pruning (assistant turns aren't pruned, so they keep it over).
	a.mu.Lock()
	big := strings.Repeat("tool result line with padding for budget overflow\n", 2500)
	a.history = []historyEntry{
		{msg: llm.Message{Role: "user", Content: "turn 1"}},
		{msg: llm.Message{Role: "tool", Content: big, ToolCallID: "c1"}},
		{msg: llm.Message{Role: "assistant", Content: strings.Repeat("assistant reasoning padding\n", 1500)}},
		{msg: llm.Message{Role: "user", Content: "current turn"}},
	}
	for i := range a.history {
		a.history[i].tokens = len(a.history[i].msg.Content) / 4
	}
	a.mu.Unlock()

	if err := a.budget(context.Background()); err != nil {
		t.Fatalf("budget with dead summarizer: %v", err)
	}
	if fail.calls != 1 {
		t.Errorf("summarizer calls = %d, want 1 (attempted first)", fail.calls)
	}
	if main.calls != 1 {
		t.Errorf("main-model fallback calls = %d, want 1", main.calls)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.runningSummary != "main-model summary" {
		t.Errorf("running summary = %q, want the main-model fallback summary", a.runningSummary)
	}
}

func TestStripInstructionEcho(t *testing.T) {
	cases := []struct{ in, want string }{
		// real answer + trailing acknowledgment filler -> answer only
		{"Hello! How can I assist you today?Understood. I will proceed without unnecessary pauses or requests for confirmation unless explicitly required.",
			"Hello! How can I assist you today?"},
		{"Here is the fix.Let me know if you'd like to explore these ideas further!",
			"Here is the fix."},
		{"The parser is in internal/parser. I hope this helps!",
			"The parser is in internal/parser."},
		// no filler -> unchanged
		{"Just 4.", "Just 4."},
		{"I moved Config into pkg/config and updated the imports.", "I moved Config into pkg/config and updated the imports."},
		// filler only -> empty (nothing but filler)
		{"Understood. I will complete tasks directly without pausing for confirmation.", ""},
		// URLs / domains must NOT be split by the sentence boundary detector
		{"gfx1031 is supported (source: https://example.com/rocm).Let me know if you need more.",
			"gfx1031 is supported (source: https://example.com/rocm)."},
		{"it costs 1.5 dollars. I will proceed without pausing.", "it costs 1.5 dollars."},
		{"Hello! What would you like me to work on?Understood. Proceeding with the task without requiring user input unless explicitly needed.",
			"Hello! What would you like me to work on?"},
	}
	for _, c := range cases {
		got := stripInstructionEcho(c.in)
		if got != c.want {
			t.Errorf("stripInstructionEcho(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPatchTargetFiles(t *testing.T) {
	// agy #1: fs_patch results must expose the patched file list for the
	// progress ledger / goal memory.
	if got := patchTargetFiles("patched 2 file(s): a.go, b.go"); len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Errorf("patchTargetFiles = %v", got)
	}
	if got := patchTargetFiles("patched 1 file(s): pkg/main.cpp"); len(got) != 1 || got[0] != "pkg/main.cpp" {
		t.Errorf("patchTargetFiles single = %v", got)
	}
	if got := patchTargetFiles("error: something"); got != nil {
		t.Errorf("patchTargetFiles error = %v, want nil", got)
	}
	if got := patchTargetFiles("no changes applied"); got != nil {
		t.Errorf("patchTargetFiles noop = %v, want nil", got)
	}
}

func TestMemoryOverlapsLedger(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{}, reg, &stubApprover{allow: true},
		Config{MaxIterations: 3}, ws)

	a.mu.Lock()
	a.touchedPaths = []string{"internal/parser/lex.go"}
	a.lastToolError = "fs_edit: old_string not found in main.go"
	a.mu.Unlock()

	// a memory restating a touched path is redundant -> overlapped
	if !a.memoryOverlapsLedger("goal work touched file internal/parser/lex.go") {
		t.Error("memory restating a touched path should be deduped")
	}
	// a memory about the current failure is redundant
	if !a.memoryOverlapsLedger("goal attempt failed: fs_edit: old_string not found in main.go") {
		t.Error("memory restating the failure should be deduped")
	}
	// unrelated facts are kept
	if a.memoryOverlapsLedger("the user prefers tabs over spaces") {
		t.Error("unrelated memory should not be deduped")
	}
}

func TestCodeIntendedGating(t *testing.T) {
	// agy #2: pure conversational continuations must not trigger semantic
	// lookup (they'd waste an embedding call + pollute context).
	for _, c := range []string{"ok", "yes", "continue", "go ahead", "thanks", "looks good"} {
		if codeIntended(c) {
			t.Errorf("codeIntended(%q) = true, want false", c)
		}
	}
	// code-related queries still trigger
	for _, c := range []string{"where is the parser package", "fix the bug in main.go", "what does validateToolInput do"} {
		if !codeIntended(c) {
			t.Errorf("codeIntended(%q) = false, want true", c)
		}
	}
}

func TestCodeEvidence(t *testing.T) {
	// lexical evidence: any FTS5 (keyword) hit justifies injection
	if !codeEvidence("where is the parser package", []index.Result{{Path: "pkg/parser/lex.go", Content: "package parser", Lexical: true}}) {
		t.Error("lexical hit should justify injection")
	}
	// symbol/path evidence: query token appears in path even without a lexical hit
	if !codeEvidence("where is the parser package", []index.Result{{Path: "internal/parser/parse.go", Content: "package parse"}}) {
		t.Error("path token overlap should justify injection")
	}
	// query token in content
	if !codeEvidence("what does validateToolInput do", []index.Result{{Path: "tools/tools.go", Content: "func validateToolInput(x string) error"}}) {
		t.Error("content token overlap should justify injection")
	}
	// vector-only false positive: no lexical hit, no token overlap -> no injection
	if codeEvidence("the weather in berlin today", []index.Result{{Path: "pkg/db/schema.go", Content: "CREATE TABLE sessions (id TEXT)"}}) {
		t.Error("unrelated vector-only match must NOT be injected")
	}
	// empty results -> no injection
	if codeEvidence("anything", nil) {
		t.Error("no results must not inject")
	}
	// stopword-only input -> no meaningful tokens
	if codeEvidence("the and for with", []index.Result{{Path: "a.go", Content: "func main(){}"}}) {
		t.Error("stopword-only query must not inject")
	}
}

func TestOscillationDetection(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{}, reg, &stubApprover{allow: true},
		Config{MaxIterations: 10}, ws)

	// no oscillation yet
	a.recordEditTarget("a.go")
	a.recordEditTarget("b.go")
	if osc := a.oscillationTargets(); osc != "" {
		t.Errorf("oscillation too early: %q", osc)
	}
	// A-B-A-B pattern detected
	a.recordEditTarget("a.go")
	a.recordEditTarget("b.go")
	osc := a.oscillationTargets()
	if osc != "a.go and b.go" {
		t.Errorf("oscillation = %q, want 'a.go and b.go'", osc)
	}
	// a non-alternating sequence is not flagged
	a.recordEditTarget("a.go")
	a.recordEditTarget("a.go")
	if osc := a.oscillationTargets(); osc != "" {
		t.Errorf("non-alternating flagged: %q", osc)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := map[string]string{
		"PASS it works":                        "PASS",
		"everything ok\nFAIL the file is gone": "FAIL",
		"no verdict here":                      "",
	}
	for in, want := range cases {
		if got := ParseVerdict(in); got != want {
			t.Errorf("ParseVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}

// fixedCounter returns a fixed token count for every non-empty text, so a test
// can prove the accurate Counter is wired into ContextUsage.
type fixedCounter struct{ n int }

func (f fixedCounter) CountTokens(ctx context.Context, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	return f.n, nil
}

func TestContextUsageUsesAccurateCounter(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("hello world")})
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Counter: fixedCounter{n: 1000}}, ws)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	used, _ := a.ContextUsage()
	// System prompt, workspace profile, user message, assistant message, and
	// the tool schemas each count as 1000 through the configured counter.
	if used != 5000 {
		t.Errorf("ContextUsage = %d, want 5000 (system/profile/history/schemas, accurate counter)", used)
	}
	a.mu.RLock()
	if a.schemaTokens == 0 {
		t.Error("schemaTokens not recorded (GPT sol #6)")
	}
	a.mu.RUnlock()
}

func TestTraceSegmentsSumToContextUsage(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("hello world")})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	var trace bytes.Buffer
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Counter: fixedCounter{n: 1000}, Trace: &trace}, ws)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	used, _ := a.ContextUsage()
	// ContextUsage = the trace's non-history sections + the LIVE history
	// totals (history grows after the last assembly) + the tool-schema cost
	// (GPT sol #6). Verify the trace file's section estimates are exactly the
	// ones the gauge accounts from.
	nonHist, histTrace := lastTraceSections(trace.String())
	a.mu.RLock()
	liveHist := 0
	for _, h := range a.history {
		liveHist += h.tokens
	}
	schemaTokens := a.schemaTokens
	a.mu.RUnlock()
	if got := nonHist + liveHist + schemaTokens; got != used {
		t.Errorf("ContextUsage = %d, want %d (non-history %d + live history %d + schemas %d)\n%s",
			used, got, nonHist, liveHist, schemaTokens, trace.String())
	}
	if histTrace > liveHist {
		t.Errorf("trace history %d exceeds live history %d", histTrace, liveHist)
	}
	if !strings.Contains(trace.String(), "===== context #1 =====") {
		t.Errorf("trace missing context marker:\n%s", trace.String())
	}
}

// lastTraceSections parses the final trace block's per-section token counts and
// returns (non-history total, history count). Each line is "  <name> N tok";
// the "total" line is excluded (it is the sum of the section lines).
func lastTraceSections(trace string) (nonHist, history int) {
	blocks := strings.Split(trace, "===== context #")
	if len(blocks) == 0 {
		return 0, 0
	}
	re := regexp.MustCompile(`\s+(\S+)\s+(\d+)\s+tok`)
	for _, m := range re.FindAllStringSubmatch(blocks[len(blocks)-1], -1) {
		if m[1] == "total" {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if m[1] == "history" {
			history = n
		} else {
			nonHist += n
		}
	}
	return nonHist, history
}

func TestGrowthForecast(t *testing.T) {
	// Build an agent, seed turnUsage, and check the forecast math.
	a := &Agent{
		turnUsage: []int{1000, 1600, 2200, 2800}, // +600/turn, window 8192
		cfg:       Config{Window: 8192},
	}
	got := a.GrowthForecast()
	// remaining = 8192-2800 = 5392; 5392/600 ≈ 9
	if got != 9 {
		t.Errorf("GrowthForecast = %d, want 9", got)
	}

	// too few turns -> -1
	if (&Agent{cfg: Config{Window: 8192}, turnUsage: []int{1000, 1600}}).GrowthForecast() != -1 {
		t.Error("forecast with 2 turns should be -1")
	}

	// nearly full -> 1 (a partial turn still fits before the budget kicks in)
	a2 := &Agent{turnUsage: []int{7000, 7600, 8100}, cfg: Config{Window: 8192}}
	if got := a2.GrowthForecast(); got != 1 {
		t.Errorf("nearly-full forecast = %d, want 1", got)
	}

	// median robustness: one huge turn shouldn't dominate
	a3 := &Agent{turnUsage: []int{1000, 2000, 2600, 3200, 3800, 4400, 9000}, cfg: Config{Window: 16000}}
	// deltas: 1000, 600, 600, 600, 600, 4600 -> median 600; remaining 16000-9000=7000; 7000/600≈12
	if got := a3.GrowthForecast(); got != 12 {
		t.Errorf("median-robust forecast = %d, want 12", got)
	}
}

func TestContextUsageConcurrent(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.txt"}`),
		toolCall("c2", "fs_read", `{"path": "a.txt"}`),
		finalContent("done"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 10}, ws)

	ctx := context.Background()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				used, limit := a.ContextUsage()
				if used < 0 || limit <= 0 {
					t.Errorf("ContextUsage = %d/%d", used, limit)
					return
				}
			}
		}
	}()
	if _, err := a.Run(ctx, "read a.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(stop)
	<-done
}

func TestMemorySaveSelfGated(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := memoryOpenVector(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "memory_save", `{"text":"the user's name is yagiz"}`),
		finalContent("ok"),
	})
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{Vectors: vs, SessionID: "s1", SkillsWriteApproval: true})
	ap := &stubApprover{allow: true}
	client := llm.NewClient(s.ts.URL, "test-model")
	a := New(client, reg, ap, Config{MaxIterations: 5, Vectors: vs, SessionID: "s1"}, ws)

	if _, err := a.Run(context.Background(), "remember my name"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.n != 0 {
		t.Errorf("memory_save prompted for approval %d times, want 0 (self-gated)", ap.n)
	}
	mem, err := vs.Search(context.Background(), "name", 3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mem {
		if strings.Contains(m.Text, "yagiz") {
			found = true
		}
	}
	if !found {
		t.Errorf("memory not saved: %+v", mem)
	}
}

func TestSkillFsWriteSelfGated(t *testing.T) {
	// The model may create a SKILL.md via fs_write directly into the skills
	// dir — that write is governed by the skills gate, so it must NOT hit the
	// generic y/n approver (2026-08-13: fs_write into .yagent/skills prompted).
	ws := t.TempDir()
	sk, err := skills.Open(filepath.Join(ws, "data"), ws)
	if err != nil {
		t.Fatal(err)
	}
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_write", `{"path":".yagent/skills/my-skill/SKILL.md","content":"---\nname: my-skill\ndescription: test\n---\n## Procedure\n1. do it\n"}`),
		finalContent("done"),
	})
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})
	ap := &stubApprover{allow: true}
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, ap,
		Config{MaxIterations: 5, Skills: sk}, ws)

	if _, err := a.Run(context.Background(), "create a skill my-skill"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.n != 0 {
		t.Errorf("fs_write to skills dir prompted for approval %d times, want 0", ap.n)
	}
	// the file actually landed
	data, err := os.ReadFile(filepath.Join(ws, ".yagent", "skills", "my-skill", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "my-skill") {
		t.Errorf("SKILL.md not written: %v", err)
	}

	// a NON-skill fs_write still prompts
	s2 := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_write", `{"path":"notes.txt","content":"hello"}`),
		finalContent("done"),
	})
	reg2 := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})
	ap2 := &stubApprover{allow: true}
	a2 := New(llm.NewClient(s2.ts.URL, "test-model"), reg2, ap2,
		Config{MaxIterations: 5, Skills: sk}, ws)
	if _, err := a2.Run(context.Background(), "write notes.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap2.n != 1 {
		t.Errorf("plain fs_write prompted %d times, want 1 (should still approve)", ap2.n)
	}
}

func TestActiveToolSchemasFilters(t *testing.T) {
	ws := t.TempDir()
	sk, err := skills.Open(ws, ws)
	if err != nil {
		t.Fatal(err)
	}
	wc, err := web.New(web.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(ws, ws, "http://127.0.0.1:1", "e")
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(ws, tools.Options{
		Skills: sk, Web: wc, Index: idx,
		Consult: llm.NewClient("http://127.0.0.1:1", "advisor"),
		Subagent: func(ctx context.Context, task, workspace string, tools []string, role tools.SubagentRole) (string, error) {
			return "ok", nil
		},
		SkillsWriteApproval: true,
		AskUser:             func(ctx context.Context, q string, choices []string) (string, error) { return "ok", nil },
	})
	writeWorkspaceFile(t, ws, "go.mod", "module schemafilter\n\ngo 1.25\n")
	a := New(nil, reg, nil, Config{MaxIterations: 5}, ws)

	has := func(schemas []llm.ToolSchema, name string) bool {
		for _, s := range schemas {
			if s.Function.Name == name {
				return true
			}
		}
		return false
	}

	// no signal -> web/index/skill_manage excluded, core present
	schemas := a.activeToolSchemas("hi there, how are you doing today", map[string]bool{})
	if has(schemas, "web_search") || has(schemas, "index_search") || has(schemas, "skill_manage") {
		t.Error("domain tools offered without a signal")
	}
	for _, c := range coreToolNames {
		if !has(schemas, c) {
			t.Errorf("core tool %s missing", c)
		}
	}
	// research signal -> web included
	if !has(a.activeToolSchemas("search the web for the latest news", map[string]bool{}), "web_search") {
		t.Error("web_search missing with a research signal")
	}
	// code signal -> index included
	if !has(a.activeToolSchemas("where is the validate function in the repo", map[string]bool{}), "index_search") {
		t.Error("index_search missing with a code signal")
	}
	// used this turn -> included regardless of signal
	if !has(a.activeToolSchemas("read the file", map[string]bool{"web_search": true}), "web_search") {
		t.Error("web_search missing after being used this turn")
	}
	if !has(a.activeToolSchemas("read the file", map[string]bool{"skill_manage": true}), "skill_manage") {
		t.Error("skill_manage missing after being used this turn")
	}
}

func TestRunGoalGateRefusesDoneOnFailingBuild(t *testing.T) {
	// The gate is unit-testable without a live model: a real diagnostics tool
	// (go.mod present) against a broken workspace returns failures, so a DONE
	// verdict must be refused. Round 2 actually FIXES the file via fs_edit
	// (real tool), so the gate then accepts DONE.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module gate\n\ngo 1.22\n")
	writeWorkspaceFile(t, ws, "main.go", "package main\nimport \"nonexistent/pkg\"\nfunc main() {}\n")

	s := newScriptedLLM(t, [][]string{
		finalContent("I moved the code"),
		finalContent("DONE the refactor is complete"),
		toolCall("c1", "fs_edit", `{"path":"main.go","old_string":"import \"nonexistent/pkg\"\n","new_string":""}`),
		finalContent("I fixed the import"),
		finalContent("DONE now it compiles"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, GoalGate: true}, ws)

	var rounds []int
	answer, err := a.RunGoal(context.Background(), "move Config to pkg/config", 5, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	// Round 1 declared DONE but the build failed -> gate refused, round 2 ran,
	// actually fixed the import, and the gate accepted the new DONE.
	if len(rounds) < 2 {
		t.Errorf("gate did not force a second round: rounds=%v", rounds)
	}
	if answer != "I fixed the import" {
		t.Errorf("answer = %q", answer)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "main.go"))
	if strings.Contains(string(data), "nonexistent") {
		t.Errorf("main.go was not actually fixed: %q", data)
	}
}

func TestCodegenSmokeGateCatchesCrash(t *testing.T) {
	// codegen mode: the model writes a Go program that COMPILES but panics at
	// runtime (the classic 9B greenfield failure). The deterministic smoke gate
	// must feed the crash back and refuse the final answer until it is fixed.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module smoke\n\ngo 1.22\n")

	s := newScriptedLLM(t, [][]string{
		// write a program that panics (index out of range)
		toolCall("c1", "fs_write", `{"path":"main.go","content":"package main\n\nfunc main() {\n\tvar s []int\n\t_ = s[5]\n}\n"}`),
		finalContent("the program is complete"),
		// gate refuses with the crash report; model fixes it
		toolCall("c2", "fs_write", `{"path":"main.go","content":"package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"fixed\")\n}\n"}`),
		finalContent("fixed the crash, program runs now"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Codegen: true}, ws)

	answer, err := a.Run(context.Background(), "build a hello program")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "fixed the crash, program runs now" {
		t.Errorf("answer = %q", answer)
	}
	// the crash report must have been fed back as a user message
	s.mu.Lock()
	defer s.mu.Unlock()
	var sawCrash bool
	for _, req := range s.requests {
		if strings.Contains(string(req), "FAILED runtime_smoke") {
			sawCrash = true
			break
		}
	}
	if !sawCrash {
		t.Error("smoke gate did not feed the crash report back")
	}
}

func TestCodegenSmokeGateBehavioralSteps(t *testing.T) {
	// The model writes a todo app with dead persistence (never reloads on a
	// fresh run — the exact bug from the v0.1.58 live test drive). It compiles
	// and runs, so the crash-only gate can't see it; the steps probe must.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module todo\n\ngo 1.22\n")

	s := newScriptedLLM(t, [][]string{
		// write the broken todo app
		toolCall("c1", "fs_write", `{"path":"main.go","content":"package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"os\"\n)\n\ntype Todo struct {\n\tID int\n\tText string\n}\n\nvar ts []Todo\n\nfunc save() {\n\tb, _ := json.Marshal(ts)\n\t_ = os.WriteFile(\"todos.json\", b, 0o644)\n}\n\nfunc main() {\n\tswitch os.Args[1] {\n\tcase \"add\":\n\t\tts = append(ts, Todo{len(ts) + 1, os.Args[2]})\n\t\tsave()\n\t\tfmt.Println(\"Added:\", os.Args[2])\n\tcase \"list\":\n\t\tfor _, t := range ts {\n\t\t\tfmt.Println(t.ID, t.Text)\n\t\t}\n\t}\n}\n"}`),
		// the model runs smoke with steps: add then a fresh list
		toolCall("c2", "runtime_smoke", `{"steps":[{"args":["add","buy milk"],"expect":"Added: buy milk"},{"args":["list"],"expect":"buy milk"}]}`),
		// probe FAILS (missing "buy milk" in the fresh list) -> gate refuses
		finalContent("the todo app is complete"),
		// model fixes persistence by loading on start
		toolCall("c3", "fs_write", `{"path":"main.go","content":"package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"os\"\n)\n\ntype Todo struct {\n\tID int\n\tText string\n}\n\nfunc load() []Todo {\n\tvar ts []Todo\n\tif b, err := os.ReadFile(\"todos.json\"); err == nil {\n\t\t_ = json.Unmarshal(b, &ts)\n\t}\n\treturn ts\n}\n\nfunc main() {\n\tts := load()\n\tswitch os.Args[1] {\n\tcase \"add\":\n\t\tts = append(ts, Todo{len(ts) + 1, os.Args[2]})\n\t\tb, _ := json.Marshal(ts)\n\t\t_ = os.WriteFile(\"todos.json\", b, 0o644)\n\t\tfmt.Println(\"Added:\", os.Args[2])\n\tcase \"list\":\n\t\tfor _, t := range ts {\n\t\t\tfmt.Println(t.ID, t.Text)\n\t\t}\n\t}\n}\n"}`),
		toolCall("c4", "runtime_smoke", `{"steps":[{"args":["add","buy milk"],"expect":"Added: buy milk"},{"args":["list"],"expect":"buy milk"}]}`),
		finalContent("fixed persistence, now the fresh list shows the item"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 12, Codegen: true}, ws)

	answer, err := a.Run(context.Background(), "build a todo app")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "fixed persistence, now the fresh list shows the item" {
		t.Errorf("answer = %q", answer)
	}
	// the behavioral FAIL must have been fed back (as the probe result, which
	// the gate re-runs at the final answer too)
	s.mu.Lock()
	defer s.mu.Unlock()
	var sawBehavioralFail bool
	for _, req := range s.requests {
		// JSON bodies escape quotes, so match the unescaped parts.
		if strings.Contains(string(req), "FAILED runtime_smoke") &&
			strings.Contains(string(req), "step 2 output missing") {
			sawBehavioralFail = true
			break
		}
	}
	if !sawBehavioralFail {
		t.Error("behavioral probe FAIL was not fed back")
	}
}

func TestCodegenSmokeNudgesBehavioralSteps(t *testing.T) {
	// The model writes a working program and crash-smokes it ({} — PASS). The
	// gate nudges it ONCE to assert real behavior; the model then probes with
	// steps and the final answer is accepted. Verifies the smokeStepsUsed
	// nudge fires without a hard refusal loop.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module smoke\n\ngo 1.22\n")

	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_write", `{"path":"main.go","content":"package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"}`),
		// crash-only smoke: PASS (program survives)
		toolCall("c2", "runtime_smoke", `{}`),
		// tries to end -> gate nudges to add behavioral steps
		finalContent("done, the program works"),
		// model probes with real steps
		toolCall("c3", "runtime_smoke", `{"steps":[{"expect":"hi"}]}`),
		finalContent("verified the output"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Codegen: true}, ws)

	answer, err := a.Run(context.Background(), "build a hello program")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "verified the output" {
		t.Errorf("answer = %q", answer)
	}
	// the nudge must have been fed back (it persists in later request bodies
	// via history, so >=1 request carrying it proves it fired; the once-per-turn
	// guarantee is enforced by the nudged flag in the loop itself)
	s.mu.Lock()
	defer s.mu.Unlock()
	sawNudge := false
	for _, req := range s.requests {
		if strings.Contains(string(req), "run only proved the program doesn't crash") {
			sawNudge = true
			break
		}
	}
	if !sawNudge {
		t.Error("behavioral nudge was not fed back")
	}
	// and the turn must still have ended with the steps-verified answer
	if answer != "verified the output" {
		t.Errorf("answer = %q, want the steps-verified final answer", answer)
	}
}

func TestCodegenSmokeGatePassesClean(t *testing.T) {
	// A program that compiles AND runs cleanly must not be gated: one
	// write + final answer, no refusal.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module smoke\n\ngo 1.22\n")

	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_write", `{"path":"main.go","content":"package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"}`),
		finalContent("all done"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, Codegen: true}, ws)

	answer, err := a.Run(context.Background(), "build a hello program")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "all done" {
		t.Errorf("answer = %q", answer)
	}
}

func TestRunGoalGateCleanBuildPasses(t *testing.T) {
	// A workspace that passes diagnostics (empty main package) must NOT be
	// gated: DONE is accepted on round 1.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module gate\n\ngo 1.22\n")
	writeWorkspaceFile(t, ws, "main.go", "package main\nfunc main() {}\n")

	s := newScriptedLLM(t, [][]string{
		finalContent("done it"),
		finalContent("DONE all good"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, GoalGate: true}, ws)

	var rounds []int
	answer, err := a.RunGoal(context.Background(), "do something", 5, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	if len(rounds) != 1 {
		t.Errorf("clean build should pass on round 1, got rounds=%v", rounds)
	}
	if answer != "done it" {
		t.Errorf("answer = %q", answer)
	}
}

func TestMemorizeGoalRoundSavesFacts(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := memoryOpenVector(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{}, reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Vectors: vs, SessionID: "s1", GoalMemorize: true}, ws)

	a.mu.Lock()
	a.history = []historyEntry{{msg: llm.Message{Role: "user", Content: "refactor the parser package"}}}
	a.touchedPaths = []string{"internal/parser/parse.go", "internal/parser/lex.go"}
	a.lastToolError = "fs_edit: old_string not found in internal/parser/lex.go"
	a.mu.Unlock()

	a.memorizeGoalRound(context.Background())

	// The facts are persisted and searchable.
	mem, err := vs.Search(context.Background(), "refactor parser touched file", 10)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range mem {
		texts = append(texts, m.Text)
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"parse.go", "lex.go", "old_string not found"} {
		if !strings.Contains(joined, want) {
			t.Errorf("memory missing %q: %s", want, joined)
		}
	}

	// Dedup: a second round with the same facts saves nothing new.
	before := vs.Count()
	a.memorizeGoalRound(context.Background())
	after := vs.Count()
	if after != before {
		t.Errorf("round 2 re-saved facts: count %d -> %d", before, after)
	}
}

func TestMemorizeGoalRoundOnFailedRun(t *testing.T) {
	// A round that hits max-iterations (Run returns an error) must still
	// persist its touched paths and last failure — a failed round's failure
	// facts are the most valuable ones to remember (bug fixed 2026-08-13).
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := memoryOpenVector(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	// scripted LLM that keeps requesting a failing edit -> max-iterations, and
	// the edit's tool error sets lastToolError (a fact worth remembering).
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_edit", `{"path":"a.txt","old_string":"x","new_string":"y"}`),
	})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 2, Vectors: vs, SessionID: "s1", GoalMemorize: true}, ws)

	_, err = a.RunGoal(context.Background(), "find the bug", 2, nil)
	if err == nil {
		t.Fatal("expected RunGoal to fail on max-iterations")
	}
	// The failure facts must be in memory even though the round errored.
	mem, err := vs.Search(context.Background(), "find the bug goal touched", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem) == 0 {
		t.Fatal("no goal facts memorized after a failed round")
	}
}

func TestErrorFixHints(t *testing.T) {
	cases := []struct {
		in   string
		want string // substring that must appear; "" = no hints expected
	}{
		{"vet: ./main.go:3:15: undefined: fmt\n", "HINT (Go)"},
		{"main.go:5:9: cannot use x (type string) as int\n", "HINT (Go)"},
		{"a.go:4:2: imported and not used: \"fmt\"\n", "HINT (Go)"},
		{"src/x.ts:2:1 - error TS2304: Cannot find name 'Foo'.\n", "HINT (TS)"},
		{"error[E0432]: unresolved import `missing`\n", "HINT (Rust)"},
		{"error[E0425]: cannot find value `foo` in this scope\n", "HINT (Rust)"},
		{"ModuleNotFoundError: No module named 'requests'\n", "HINT (Python)"},
		{"ImportError: cannot import name 'x' from 'y'\n", "HINT (Python)"},
		{"/usr/bin/ld: undefined reference to `gladLoadGL'\n", "HINT (C/C++)"},
		{"main.c:3:10: fatal error: texture.h: No such file or directory\n", "HINT (C/C++)"},
		{"# pkg\nFAIL\tpkg [build failed]\n", "HINT:"},
		{"all checks passed\n", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := errorFixHints(c.in)
		if c.want == "" {
			if got != "" {
				t.Errorf("errorFixHints(%q) = %q, want empty", c.in, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("errorFixHints(%q) missing %q: %q", c.in, c.want, got)
		}
	}
}

func TestDiagnosticsFailed(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "clean"},
		{"go vet: no issues\n", "clean"},
		{"main.go:6:2: package stress/pkg is not in std\n", "fail"},
		{"FAIL\texample/x [build failed]\n", "fail"},
		{"# stress\nmain.go:3:8: undefined: fmt\n", "fail"},
		{"exit status 1\n", "fail"},
		{"no diagnostics configured for this project\n", "clean (handled by caller)"},
	}
	for _, c := range cases {
		if c.want == "fail" && !DiagnosticsFailed(c.in) {
			t.Errorf("DiagnosticsFailed(%q) = false, want true", c.in)
		}
		if c.want == "clean" && DiagnosticsFailed(c.in) {
			t.Errorf("DiagnosticsFailed(%q) = true, want false", c.in)
		}
	}
}

func TestTestsFailed(t *testing.T) {
	cases := []struct{ in, want string }{
		{"all tests passed\n", "clean"},
		{"ok  \tgithub.com/x/y\t0.123s\n", "clean"},
		{"PASS\nok  \tgithub.com/x/y\t0.111s\n", "clean"},
		{"--- FAIL: TestThing (0.01s)\n", "fail"},
		{"FAIL\n", "fail"},
		{"FAIL\tgithub.com/x/y [build failed]\n", "fail"},
		{"error: tests failed\n", "fail"},
		{"tests failed: exit status 1\n", "fail"},
		{"3 failed, 5 passed\n", "fail"},
	}
	for _, c := range cases {
		if c.want == "fail" && !TestsFailed(c.in) {
			t.Errorf("TestsFailed(%q) = false, want true", c.in)
		}
		if c.want == "clean" && TestsFailed(c.in) {
			t.Errorf("TestsFailed(%q) = true, want false", c.in)
		}
	}
}

func TestRunGoalSuccessChecksRefuseCopyInsteadOfMove(t *testing.T) {
	// The stress-test finding (run 2, 2026-08-14): a "copy instead of move"
	// refactor — the model adds the new package but leaves the old one and the
	// callers untouched, so everything still compiles and the compile/test
	// gates are BLIND to it. A --check predicate ("main.go contains config.New")
	// must refuse that DONE.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module stress\n\ngo 1.22\n")
	// fixture: old package + caller + test all intact (the "copy" state)
	writeWorkspaceFile(t, ws, "pkg/config.go", "package pkg\n\ntype Config struct{ Host string }\n")
	writeWorkspaceFile(t, ws, "main.go", "package main\n\nimport \"stress/pkg\"\n\nfunc main() {\n\tvar c pkg.Config\n\t_ = c\n}\n")
	writeWorkspaceFile(t, ws, "pkg_test.go", "package main\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {}\n")

	s := newScriptedLLM(t, [][]string{
		// the model does a "copy": creates the new package but never rewires
		toolCall("c1", "fs_write", `{"path":"pkg/config/config.go","content":"package config\n\ntype Config struct {\n\tHost string\n}\n\nfunc New() Config {\n\treturn Config{Host: \"localhost\"}\n}\n"}`),
		finalContent("moved Config to pkg/config with New()"),
		finalContent("DONE the refactor is complete"),
		// the success predicate fires: main.go still uses stress/pkg
		toolCall("c2", "fs_edit", `{"path":"main.go","old_string":"import \"stress/pkg\"\n","new_string":"import \"stress/pkg/config\"\n"}`),
		toolCall("c3", "fs_edit", `{"path":"main.go","old_string":"var c pkg.Config","new_string":"var c config.Config"}`),
		finalContent("rewired main.go to the new package"),
		finalContent("DONE now main.go uses config.New"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, GoalGate: true, TestGate: true,
			SuccessChecks: []SuccessCheck{{FileContains: "main.go:config.Config"}}}, ws)

	var rounds []int
	answer, err := a.RunGoal(context.Background(), "move Config to pkg/config", 5, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	if len(rounds) < 2 {
		t.Errorf("success predicate did not force a second round: rounds=%v", rounds)
	}
	// RunGoal returns the final round's Run answer (the DONE verdict is consumed
	// by the gate, not returned).
	if answer != "rewired main.go to the new package" {
		t.Errorf("answer = %q", answer)
	}
	// the predicate failure must have been fed back
	s.mu.Lock()
	defer s.mu.Unlock()
	sawCheck := false
	for _, req := range s.requests {
		if strings.Contains(string(req), "success predicates are not satisfied") {
			sawCheck = true
			break
		}
	}
	if !sawCheck {
		t.Error("success predicate failure was not fed back")
	}
}

func TestSuccessCheckEval(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "main.go", "package main\n\nimport \"stress/pkg\"\nfunc main() { var c pkg.Config; _ = c }\n")

	if msg := (SuccessCheck{FileContains: "main.go:config.New"}).Eval(ws); msg == "" {
		t.Error("missing config.New should fail the contains check")
	}
	if msg := (SuccessCheck{FileContains: "main.go:stress/pkg"}).Eval(ws); msg != "" {
		t.Errorf("present text wrongly failed: %q", msg)
	}
	if msg := (SuccessCheck{FileNotContains: "main.go:stress/pkg"}).Eval(ws); msg == "" {
		t.Error("present forbidden text should fail !contains")
	}
	if msg := (SuccessCheck{FileExists: "main.go"}).Eval(ws); msg != "" {
		t.Errorf("existing file wrongly failed: %q", msg)
	}
	if msg := (SuccessCheck{FileExists: "missing.go"}).Eval(ws); msg == "" {
		t.Error("missing file should fail exists")
	}
}

func TestRunGoalTestGateRefusesOnFailingTests(t *testing.T) {
	// The workspace compiles (go vet passes) but a unit test fails. With
	// TestGate on, DONE must be refused and the failure fed back until the
	// model fixes the test.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module testgate\n\ngo 1.22\n")
	writeWorkspaceFile(t, ws, "calc.go", "package calc\n\nfunc Add(a, b int) int { return a - b } // wrong: subtracts\n")
	writeWorkspaceFile(t, ws, "calc_test.go", `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Fatal("Add(1,1) != 2")
	}
}
`)

	s := newScriptedLLM(t, [][]string{
		finalContent("done the work"),
		finalContent("DONE it is complete"),
		// gate refuses (test fails); model fixes the function
		toolCall("c1", "fs_write", `{"path":"calc.go","content":"package calc\n\nfunc Add(a, b int) int { return a + b }\n"}`),
		finalContent("fixed the bug"),
		finalContent("DONE tests pass now"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, GoalGate: true, TestGate: true}, ws)

	var rounds []int
	answer, err := a.RunGoal(context.Background(), "make Add correct", 5, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	if len(rounds) < 2 {
		t.Errorf("test gate did not force a second round: rounds=%v", rounds)
	}
	if answer != "fixed the bug" {
		t.Errorf("answer = %q", answer)
	}
	// the test-failure report must have been fed back
	s.mu.Lock()
	defer s.mu.Unlock()
	sawTestFail := false
	for _, req := range s.requests {
		if strings.Contains(string(req), "unit tests FAIL") {
			sawTestFail = true
			break
		}
	}
	if !sawTestFail {
		t.Error("test gate did not feed the failure back")
	}
}

func TestRunGoalTestGateSkipsNoTests(t *testing.T) {
	// No test files: the test gate must skip (no framework / nothing to run)
	// rather than refusing a valid DONE.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module testgate\n\ngo 1.22\n")
	writeWorkspaceFile(t, ws, "calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")

	s := newScriptedLLM(t, [][]string{
		finalContent("done"),
		finalContent("DONE complete"),
	})
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 10, GoalGate: true, TestGate: true}, ws)

	var rounds []int
	answer, err := a.RunGoal(context.Background(), "do something", 5, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	if len(rounds) != 1 {
		t.Errorf("no tests should pass on round 1, got rounds=%v", rounds)
	}
	if answer != "done" {
		t.Errorf("answer = %q", answer)
	}
}

func TestRunGoalDoneFirstRound(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		finalContent("I fixed the build"),
		finalContent("DONE the build passes now"),
	})
	a, _, _, _ := setup(t, s, true, 10)
	answer, err := a.RunGoal(context.Background(), "make the build pass", 5, nil)
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	if answer != "I fixed the build" {
		t.Errorf("answer = %q", answer)
	}
}

func TestRunGoalContinuesUntilDone(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		finalContent("first attempt done"),
		finalContent("CONTINUE the tests still fail"),
		finalContent("second attempt done"),
		finalContent("DONE all tests pass now"),
	})
	a, _, _, _ := setup(t, s, true, 10)
	var rounds []int
	answer, err := a.RunGoal(context.Background(), "make the tests pass", 5, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunGoal: %v", err)
	}
	if answer != "second attempt done" {
		t.Errorf("answer = %q", answer)
	}
	if len(rounds) != 2 || rounds[0] != 1 || rounds[1] != 2 {
		t.Errorf("rounds = %v, want [1 2]", rounds)
	}
}

func TestRunGoalCaps(t *testing.T) {
	// never DONE -> capped at 2 rounds
	s := newScriptedLLM(t, [][]string{
		finalContent("a1"),
		finalContent("CONTINUE still going"),
		finalContent("a2"),
		finalContent("CONTINUE still going"),
	})
	a, _, _, _ := setup(t, s, true, 10)
	_, err := a.RunGoal(context.Background(), "goal", 2, nil)
	if err == nil || !strings.Contains(err.Error(), "after 2 rounds") {
		t.Errorf("err = %v, want cap error", err)
	}
}

func TestRunSubagent(t *testing.T) {
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "a.txt"}`),
		finalContent("SUMMARY: the file exists"),
	})
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{ReadOnly: true})
	client := llm.NewClient(s.ts.URL, "test-model")
	answer, _, err := RunSubagent(context.Background(), client, reg, "check a.txt", ws, tools.SubagentRole{})
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if !strings.Contains(answer, "SUMMARY") {
		t.Errorf("answer = %q", answer)
	}
}

func TestCompactChunk(t *testing.T) {
	full := "func helper(x int) int {\n    return x + 1\n}\n"
	c := compactChunk(full)
	if !strings.Contains(c, "func helper(x int) int {") || !strings.Contains(c, "// ...") {
		t.Errorf("compact = %q", c)
	}
	// brace-less content keeps the top few lines
	py := "def f(a):\n    return a + 1\n    # more\n    # more\n    # more\n"
	c = compactChunk(py)
	if strings.Contains(c, "return a + 1") {
		t.Errorf("python body not collapsed: %q", c)
	}
}

func TestLoadSessionSwapsContext(t *testing.T) {
	s := newScriptedLLM(t, [][]string{finalContent("ok")})
	a, _, _, _ := setup(t, s, true, 5)
	a.Run(context.Background(), "first turn")
	// swap to a fresh session context
	a.LoadSession([]llm.Message{{Role: "user", Content: "resumed turn"}}, "resumed summary")
	a.SetSessionID("new-session-id")
	if got := a.History(); len(got) != 1 || got[0].Content != "resumed turn" {
		t.Errorf("history after load = %+v", got)
	}
	if a.runningSummary != "resumed summary" {
		t.Errorf("summary = %q", a.runningSummary)
	}
}

func TestPlanModeRestrictsSchemasAndApprovalExits(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.txt", "data")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, nil, Config{MaxIterations: 5}, ws)

	// plan mode on: only read-only tools + plan/consult offered
	a.SetPlanMode(true)
	schemas := a.activeToolSchemas("explore", map[string]bool{})
	names := map[string]bool{}
	for _, s := range schemas {
		names[s.Function.Name] = true
	}
	if !names["fs_read"] || !names["glob"] {
		t.Errorf("plan mode missing read-only tools: %v", names)
	}
	if names["fs_write"] || names["fs_edit"] || names["shell_exec"] {
		t.Error("plan mode offered a write tool")
	}
	if !a.PlanMode() {
		t.Error("plan mode not reported on")
	}

	// simulating the plan tool's approved result must flip it off — exercised
	// through dispatch is complex; verify the setter + the active schemas flip.
	a.SetPlanMode(false)
	if a.PlanMode() {
		t.Error("plan mode not off")
	}
	schemas = a.activeToolSchemas("now edit", map[string]bool{})
	hasWrite := false
	for _, s := range schemas {
		if s.Function.Name == "fs_write" {
			hasWrite = true
		}
	}
	if !hasWrite {
		t.Error("write tools not restored after plan mode off")
	}
}

func TestPlanModeEnforcedInDispatch(t *testing.T) {
	// GPT sol #3: plan mode hides write schemas, but a model can still
	// hallucinate an fs_write call — dispatch must reject it, not run it.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	a.SetPlanMode(true)

	result := a.dispatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_write", Arguments: []byte(`{"path":"x.txt","content":"hi"}`)}}, map[string]int{}, map[string]bool{})
	if !strings.Contains(result, "plan mode") {
		t.Errorf("plan mode did not reject the write: %q", result)
	}
	if _, err := os.Stat(filepath.Join(ws, "x.txt")); !os.IsNotExist(err) {
		t.Error("plan mode rejected write still created the file")
	}
	// Read-only tools still work in plan mode.
	writeWorkspaceFile(t, ws, "r.txt", "data")
	r := a.dispatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_read", Arguments: []byte(`{"path":"r.txt"}`)}}, map[string]int{}, map[string]bool{})
	if strings.Contains(r, "error") {
		t.Errorf("plan mode blocked a read: %q", r)
	}
}

func TestFailedWriteDoesNotArmWriteState(t *testing.T) {
	// GPT sol #2: only a SUCCESSFUL write marks the turn unverified, records a
	// touched path, or flips turnWrote. A failed fs_edit must arm nothing.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	// 1. A FAILED fs_edit (old_string not found) arms nothing.
	res := a.dispatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit", Arguments: []byte(`{"path":"calc.go","old_string":"func Add(a, b int) int { return a + b } // does not exist","new_string":"func Add(a, b int) int { return a * b }"}`)}}, map[string]int{}, map[string]bool{})
	if !strings.HasPrefix(res, "error") {
		t.Fatalf("expected failed edit, got %q", res)
	}
	a.mu.RLock()
	unv, touched, wrote := a.unverifiedWrite, len(a.touchedPaths), a.turnWrote
	a.mu.RUnlock()
	if unv || touched != 0 || wrote {
		t.Errorf("failed write armed state: unverified=%v touched=%d turnWrote=%v", unv, touched, wrote)
	}

	// 2. A SUCCESSFUL fs_write arms unverified + touched + turnWrote.
	res = a.dispatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_write", Arguments: []byte(`{"path":"out.txt","content":"done"}`)}}, map[string]int{}, map[string]bool{})
	if strings.HasPrefix(res, "error") {
		t.Fatalf("expected successful write, got %q", res)
	}
	a.mu.RLock()
	unv, touched, wrote = a.unverifiedWrite, len(a.touchedPaths), a.turnWrote
	a.mu.RUnlock()
	if !unv || touched != 1 || !wrote {
		t.Errorf("successful write did not arm state: unverified=%v touched=%d turnWrote=%v", unv, touched, wrote)
	}
}

func TestGateMarkersTrustExitStatus(t *testing.T) {
	// GPT sol #1: the [FAIL]/[PASS] markers are authoritative even when output
	// prose is empty or unusual — a non-zero exit can't be hidden by sparse text.
	cases := []struct {
		in   string
		want bool
	}{
		{"[PASS] go vet passed", false},
		{"[FAIL]\n", true},
		{"[FAIL] (exited exit status 1)", true},
		{"go vet passed\nFAIL", true},
		{"all tests passed", false},
		{"--- FAIL: TestAdd\nAdd(1,1) != 2", true},
	}
	for _, tc := range cases {
		if got := TestsFailed(tc.in); got != tc.want {
			t.Errorf("TestsFailed(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if got := DiagnosticsFailed(tc.in); got != tc.want {
			t.Errorf("DiagnosticsFailed(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTruncatedStreamRecoversWithNudge(t *testing.T) {
	// GPT sol #5: a stream that dies mid-generation must NOT abort the turn.
	// The agent feeds the partial reply back and lets the model finish.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	tllm := &truncatingLLM{body: "the answer is 42"}
	a := New(tllm, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	answer, err := a.Run(context.Background(), "what is the answer?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "the answer is 42" {
		t.Errorf("answer = %q", answer)
	}
	tllm.mu.Lock()
	calls := tllm.n
	tllm.mu.Unlock()
	if calls != 2 {
		t.Errorf("llm called %d times, want 2 (truncated + recovery)", calls)
	}
}

func TestLengthFinishReasonRecoversWithNudge(t *testing.T) {
	// GPT sol #5: finish_reason=length on a prose reply means the generation
	// hit the cap — the agent must nudge continuation, not accept it as final.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	llm2 := &lengthLLM{first: "let me explain...", body: "the full explanation"}
	a := New(llm2, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	answer, err := a.Run(context.Background(), "explain it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if answer != "the full explanation" {
		t.Errorf("answer = %q", answer)
	}
	llm2.mu.Lock()
	calls := llm2.n
	llm2.mu.Unlock()
	if calls != 2 {
		t.Errorf("llm called %d times, want 2 (length + continuation)", calls)
	}
}

func TestDependencyFixHint(t *testing.T) {
	// AGY #5: a multi-file compile failure names the upstream package first so
	// the model fixes the definition before the callers that break against it.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "go.mod", "module example.com/acme\n")
	writeWorkspaceFile(t, ws, "internal/api/types.go", "package api\n\nfunc Config() {}\n")
	writeWorkspaceFile(t, ws, "cmd/server/main.go", "package main\n\nimport \"example.com/acme/internal/api\"\n\nfunc main() { api.Config() }\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	hint := a.dependencyFixHint("cmd/server/main.go:3:16: undefined: Config\ninternal/api/types.go:5:1: undefined: Config\n")
	if !strings.Contains(hint, "dependency order") {
		t.Errorf("no dependency hint: %q", hint)
	}
	// internal/api must be named before cmd/server (upstream-first).
	if !strings.Contains(hint, "internal/api") || !strings.Contains(hint, "cmd/server") {
		t.Errorf("hint missing packages: %q", hint)
	}
	apiIdx := strings.Index(hint, "internal/api")
	cmdIdx := strings.Index(hint, "cmd/server")
	if apiIdx < 0 || cmdIdx < 0 || apiIdx > cmdIdx {
		t.Errorf("upstream package not ranked first: %q", hint)
	}
	// a clean result gets no hint
	if hint := a.dependencyFixHint("[PASS] go vet passed"); hint != "" {
		t.Errorf("clean diagnostics got a hint: %q", hint)
	}
	// a single failing file has no ordering to teach
	if hint := a.dependencyFixHint("main.go:3:1: undefined: X"); hint != "" {
		t.Errorf("single-file failure got a hint: %q", hint)
	}
}

func TestSteerPinnedInTaskState(t *testing.T) {
	// AGY #6 / luna #1: /steer pins a course-correction at the top of TASK
	// STATE on every subsequent request, surviving across goal rounds until
	// cleared or replaced.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	a.Steer("focus on the tests, not the UI")
	joined := ""
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	if !strings.Contains(joined, "USER STEER") || !strings.Contains(joined, "focus on the tests") {
		t.Errorf("steer not pinned in TASK STATE:\n%s", joined)
	}
	// persists (still there on a second assembly — no per-turn clear)
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	_ = joined
	if got := a.ActivePlan(); len(got) != 0 {
		t.Errorf("active plan should be empty: %v", got)
	}
	// empty steer clears it
	a.Steer("")
	joined = ""
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	if strings.Contains(joined, "USER STEER") {
		t.Errorf("cleared steer still pinned:\n%s", joined)
	}
}

func TestApprovedPlanTrackedInTaskState(t *testing.T) {
	// AGY #6: an approved plan tool call records its steps into TASK STATE so
	// a small model can't skip intermediate steps once it starts executing.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	reg.SetAskUser(func(ctx context.Context, q string, choices []string) (string, error) {
		return "Approve plan", nil
	})
	a := New(llm.NewClient("http://127.0.0.1:1", "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	steps := `{"steps": ["create pkg/config/config.go", "rewire main.go imports", "run diagnostics"]}`
	res := a.dispatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Name: "plan", Arguments: []byte(steps)}}, map[string]int{}, map[string]bool{})
	if !strings.Contains(res, "plan approved") {
		t.Fatalf("plan not approved: %q", res)
	}
	plan := a.ActivePlan()
	if len(plan) != 3 || plan[1] != "rewire main.go imports" {
		t.Errorf("active plan = %v, want the 3 approved steps", plan)
	}
	joined := ""
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	if !strings.Contains(joined, "ACTIVE PLAN") || !strings.Contains(joined, "create pkg/config/config.go") {
		t.Errorf("plan not in TASK STATE:\n%s", joined)
	}
}

func TestProactivePruneToolOutputs(t *testing.T) {
	// AGY #2: read-tool results older than the current and immediately
	// preceding turn collapse to a marker even when under the budget; errors
	// and recent outputs are kept intact.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&truncatingLLM{body: "x"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)

	big := strings.Repeat("output line\n", 100)
	mk := func(role, content string) llm.Message {
		return llm.Message{Role: role, Content: content}
	}
	a.mu.Lock()
	// turn 1 (old): user + big read result + error + assistant
	a.history = append(a.history,
		historyEntry{msg: mk("user", "first task"), tokens: 4},
		historyEntry{msg: mk("tool", big), tokens: 400},
		historyEntry{msg: mk("tool", "error: failed"), tokens: 40},
		historyEntry{msg: mk("assistant", "did it"), tokens: 4},
		// turn 2 (previous): user + big read result + assistant
		historyEntry{msg: mk("user", "second task"), tokens: 4},
		historyEntry{msg: mk("tool", big), tokens: 400},
		historyEntry{msg: mk("assistant", "done"), tokens: 4},
		// turn 3 (current): user + big read result (must stay full)
		historyEntry{msg: mk("user", "current task"), tokens: 4},
		historyEntry{msg: mk("tool", big), tokens: 400},
	)
	a.mu.Unlock()

	a.proactivePruneToolOutputs()

	a.mu.RLock()
	defer a.mu.RUnlock()
	if got := a.history[1].msg.Content; !strings.Contains(got, "concealed") {
		t.Errorf("old read not collapsed: %q", got)
	}
	if got := a.history[2].msg.Content; got != "error: failed" {
		t.Errorf("old error collapsed: %q", got)
	}
	// the immediately preceding turn stays full (AGY #2 keeps current +
	// previous turn; only older results collapse)
	if got := a.history[5].msg.Content; !strings.Contains(got, "output line") {
		t.Errorf("previous-turn read collapsed: %q", got)
	}
	if got := a.history[8].msg.Content; !strings.Contains(got, "output line") {
		t.Errorf("current-turn read collapsed: %q", got)
	}
}

func TestResearchLedgerRendersSources(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	a.mu.Lock()
	a.researchSources = []string{"https://example.com/a", "https://example.com/b"}
	a.researchQueries = []string{"go modules"}
	a.researchFindings = []string{"llama.cpp supports ROCm [https://example.com/a]"}
	a.mu.Unlock()

	ledger := a.taskLedger()
	if !strings.Contains(ledger, "SOURCES (fetched)") || !strings.Contains(ledger, "https://example.com/a") {
		t.Errorf("ledger missing sources: %q", ledger)
	}
	if !strings.Contains(ledger, "searched: go modules") {
		t.Errorf("ledger missing queries: %q", ledger)
	}
	if !strings.Contains(ledger, "RESEARCH NOTES") || !strings.Contains(ledger, "ROCm") {
		t.Errorf("ledger missing findings: %q", ledger)
	}
	joined := ""
	for _, m := range a.assembleContext("", "") {
		joined += m.Content
	}
	if !strings.Contains(joined, "SOURCES (fetched)") || !strings.Contains(joined, "https://example.com/a") {
		t.Errorf("sources not injected into context: %q", joined[:min(len(joined), 400)])
	}
}

func TestResearchNoteRecordsFinding(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	a.recordResearchFinding("fact one [https://example.com/1]")
	a.recordResearchFinding("fact one [https://example.com/1]") // dedup
	a.recordResearchFinding("fact two [https://example.com/2]")
	if got := a.ResearchFindings(); len(got) != 2 {
		t.Errorf("findings = %v, want 2 deduped", got)
	}
	if got := a.ResearchSources(); len(got) != 0 {
		t.Errorf("sources = %v, want none", got)
	}
}

func TestRunResearchGateRequiresReport(t *testing.T) {
	// The model fetches pages and claims DONE, but writes no report file: the
	// research gate refuses. Round 2 writes the report with cited URLs and
	// DONE is accepted.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	s := newScriptedLLM(t, [][]string{
		finalContent("research done"),
		finalContent("DONE research complete"),
		toolCall("r1", "fs_write", `{"path":".yagent/research/rocm.md","content":"# ROCm\n\nllama.cpp supports ROCm on gfx1031.\n\n## Sources\n- https://example.com/a\n- https://example.com/b\n"}`),
		finalContent("wrote the report"),
		finalContent("DONE report written"),
	})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	// Pre-seed fetched sources so the gate's source floor is already met and
	// only the report requirement is exercised.
	a.mu.Lock()
	a.researchSources = []string{"https://example.com/a", "https://example.com/b"}
	a.mu.Unlock()

	var rounds []int
	answer, err := a.RunResearch(context.Background(), "does llama.cpp support ROCm", 3, func(r int, _ string) {
		rounds = append(rounds, r)
	})
	if err != nil {
		t.Fatalf("RunResearch: %v", err)
	}
	if len(rounds) != 2 {
		t.Errorf("expected 2 rounds (1 refused, 1 accepted), got %v", rounds)
	}
	if answer != "wrote the report" {
		t.Errorf("answer = %q", answer)
	}
	if report := a.ResearchReport(); !strings.HasSuffix(report, ".yagent/research/rocm.md") {
		t.Errorf("report = %q", report)
	}
}

func TestRunResearchUnverifiedDoneErrors(t *testing.T) {
	// The research turn completes, but the DONE-check request fails (server
	// hiccup): the run must return an error, not report an unverified success.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	// request 1 = Run (answer), request 2 = goalDone DONE-check -> fails
	llm := &failAfterNLLM{max: 1, body: "research findings here"}
	a := New(llm, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	if _, err := a.RunResearch(context.Background(), "topic", 2, nil); err == nil {
		t.Fatal("RunResearch must return an error when the DONE check fails")
	} else if !strings.Contains(err.Error(), "DONE check") {
		t.Errorf("error should mention the unverified DONE check: %v", err)
	}
}

func TestResearchGateRefusesWithoutSources(t *testing.T) {
	// No pages fetched at all: even with a report claim, DONE is refused.
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	s := newScriptedLLM(t, [][]string{
		finalContent("did nothing but claim done"),
		finalContent("DONE"),
		finalContent("actually research"),
		finalContent("DONE for real"),
	})
	a := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	// no researchSources, no report
	verify := a.researchGateCheck(context.Background())
	if verify == "" {
		t.Fatal("gate should refuse a research DONE with no sources and no report")
	}
	if !strings.Contains(verify, "web_fetch at least 2") {
		t.Errorf("gate message = %q", verify)
	}
	if !strings.Contains(verify, "no research report") {
		t.Errorf("gate message = %q", verify)
	}
}

func TestCountSourcesSection(t *testing.T) {
	// URLs outside a Sources section must not count
	body := "see https://example.com/a and https://example.com/b in the body\n\n## Sources\n- https://example.com/a\n- https://example.com/c\n"
	cited, found := countSourcesSection(body)
	if !found {
		t.Error("Sources section not found")
	}
	if cited != 2 {
		t.Errorf("countSourcesSection = %d, want 2 (only the Sources section URLs count)", cited)
	}
	// no Sources heading -> found=false even with URLs present
	if _, found := countSourcesSection("body with https://example.com/x and https://example.com/y only"); found {
		t.Error("no Sources heading should report found=false")
	}
	// References heading also recognized, next heading terminates the section
	body2 := "intro https://example.com/a\n\n## References\n- https://example.com/r1\n- https://example.com/r2\n\n## More\n- https://example.com/ignored\n"
	cited2, found2 := countSourcesSection(body2)
	if !found2 || cited2 != 2 {
		t.Errorf("references section = %d found=%v, want 2 found=true", cited2, found2)
	}
}

func TestFailureCapsuleHintAcrossRuns(t *testing.T) {
	// A recurring fs_edit failure on the same file: the second failure's tool
	// result must carry a "known recurring failure" hint from the capsule
	// store, and a later fs_write success on that path must be recorded as the
	// recovery.
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "main.go", "package main\nfunc main() {}\n")
	capsPath := filepath.Join(t.TempDir(), "capsules.json")
	cs, err := capsule.Open(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5, Capsules: cs}, ws)

	// Simulate two failed fs_edit calls (via dispatchAll on scripted calls)
	// then a successful fs_write, and assert the capsule behavior.
	call := func(name, args string) llm.ToolCall {
		return llm.ToolCall{Function: llm.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
	}
	fail1 := a.dispatch(context.Background(), call("fs_edit", `{"path":"main.go","old_string":"missing","new_string":"x"}`), map[string]int{}, map[string]bool{})
	if !strings.HasPrefix(fail1, "error:") {
		t.Fatalf("first edit should fail, got %q", fail1)
	}
	if strings.Contains(fail1, "recurring failure") {
		t.Errorf("first failure must not carry a hint: %q", fail1)
	}
	// vary the args so the write-dedup doesn't skip it (a real model varies
	// old_string on retry)
	fail2 := a.dispatch(context.Background(), call("fs_edit", `{"path":"main.go","old_string":"missing","new_string":"y"}`), map[string]int{}, map[string]bool{})
	if !strings.Contains(fail2, "known recurring failure") {
		t.Errorf("second failure should carry the capsule hint: %q", fail2)
	}
	// a successful write on the path records the recovery
	okRes := a.dispatch(context.Background(), call("fs_write", `{"path":"main.go","content":"package main\nfunc main() {}\n"}`), map[string]int{}, map[string]bool{})
	if strings.HasPrefix(okRes, "error:") {
		t.Fatalf("fs_write should succeed: %q", okRes)
	}
	m, _ := cs.Match("fs_edit", "old_string_not_found", "main.go")
	if m.RecoveredBy != "fs_write" {
		t.Errorf("recovered_by = %q, want fs_write", m.RecoveredBy)
	}
	// a third failure now names the recovery
	fail3 := a.dispatch(context.Background(), call("fs_edit", `{"path":"main.go","old_string":"missing","new_string":"z"}`), map[string]int{}, map[string]bool{})
	if !strings.Contains(fail3, "fs_write") {
		t.Errorf("third failure should name the recovery tool: %q", fail3)
	}
}

// TestNoCapsulesDisabled: without a capsule store, failures pass through
// unchanged (feature off by default).
func TestNoCapsulesDisabled(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, "a.go", "package a\n")
	reg := tools.NewRegistry(ws, tools.Options{SkillsWriteApproval: true})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	res := a.dispatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit", Arguments: json.RawMessage(`{"path":"a.go","old_string":"x","new_string":"y"}`)}}, map[string]int{}, map[string]bool{})
	if strings.Contains(res, "recurring failure") {
		t.Errorf("capsule disabled must not hint: %q", res)
	}
}

func TestResearchProvenanceBundle(t *testing.T) {
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{})
	a := New(&fixedSummaryLLM{summary: "unused"}, reg, &stubApprover{allow: true}, Config{MaxIterations: 5}, ws)
	// write a report + record sources/queries/findings
	dir := filepath.Join(ws, ".yagent", "research")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "topic.md")
	if err := os.WriteFile(report, []byte("# T\n\n## Sources\n- https://example.com/a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.researchSources = []string{"https://example.com/a"}
	a.researchQueries = []string{"llama.cpp quantization"}
	a.researchFindings = []string{"found a thing [https://example.com/a]"}
	a.mu.Unlock()

	if prov := a.WriteResearchProvenance(); prov == "" {
		t.Fatal("WriteResearchProvenance returned empty")
	} else if prov != report+".provenance.json" {
		t.Errorf("provenance path = %q", prov)
	}
	if got := a.ResearchProvenance(); got != report+".provenance.json" {
		t.Errorf("ResearchProvenance = %q", got)
	}
	data, err := os.ReadFile(report + ".provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"queries"`, "llama.cpp quantization", `"findings"`, "found a thing", `"sources"`, "https://example.com/a", `"sha256"`} {
		if !strings.Contains(s, want) {
			t.Errorf("provenance bundle missing %q:\n%s", want, s)
		}
	}
	// no report in a fresh workspace -> no bundle
	ws2 := t.TempDir()
	a2 := New(&fixedSummaryLLM{summary: "unused"}, tools.NewRegistry(ws2, tools.Options{}), &stubApprover{allow: true}, Config{MaxIterations: 5}, ws2)
	if p := a2.WriteResearchProvenance(); p != "" {
		t.Errorf("no report should produce no bundle, got %q", p)
	}
}

func TestSkillCreationSuppressedInGoalMode(t *testing.T) {
	// A 5+ tool-call turn inside an autonomous goal loop must NOT trigger the
	// end-of-turn skill-creation opportunity — it diverts the model into skill
	// planning mid-goal (observed: Nemotron-3-30B burned all 8 goal rounds on
	// skill deliberation instead of the task). The session-end Finish() is the
	// single distillation point.
	ws := t.TempDir()
	sk, err := skills.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})

	// Goal mode: 5 calls -> suppressed (offerSkillCreation returns early via the
	// autonomous guard). Set goalMode and confirm the offer is skipped by
	// checking no extra request was made to the LLM.
	s := newScriptedLLM(t, [][]string{finalContent("done")})
	goal := New(llm.NewClient(s.ts.URL, "test-model"), reg, &stubApprover{allow: true},
		Config{MaxIterations: 5, Skills: sk}, ws)
	goal.mu.Lock()
	goal.goalMode = true
	goal.totalToolCalls = 6
	goal.mu.Unlock()
	if err := goal.maybeOfferSkillCreation(context.Background(), 6); err != nil {
		t.Fatalf("maybeOfferSkillCreation in goal mode: %v", err)
	}
	// The scripted server would have been hit by offerSkillCreation's ChatStream
	// if it ran; the suppression means zero requests were made.
	if n := len(s.requests); n != 0 {
		t.Errorf("goal-mode skill offer made %d LLM request(s), want 0 (suppressed)", n)
	}
}

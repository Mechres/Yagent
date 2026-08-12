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

	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
	"github.com/Mechres/Yagent/internal/web"
)

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

func TestDedupIdenticalToolCalls(t *testing.T) {
	// Model calls the SAME tool+args twice in a row; the second must be
	// skipped, not executed twice.
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

	answer, err := a.Run(context.Background(), "read a.txt twice")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = answer
	// the model's final request must show the dedup-skip notice for the
	// second identical call (proving it was skipped, not re-executed)
	s.mu.Lock()
	defer s.mu.Unlock()
	last := string(s.requests[len(s.requests)-1])
	if !strings.Contains(last, "duplicate of the previous tool call") {
		t.Errorf("skip notice not fed back to the model: %q", last[:400])
	}
	// and the file content must appear only once as a tool result
	if n := strings.Count(last, "data"); n != 1 {
		t.Errorf("fs_read appears %d times in the final context, want 1", n)
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
		t.Fatalf("VerifySkill: %v", err)
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
	// system (1) + user message (1) + assistant message (1) = 3, each 1000
	if used != 3000 {
		t.Errorf("ContextUsage = %d, want 3000 (accurate counter not wired)", used)
	}
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
	// totals (history grows after the last assembly). Verify the trace file's
	// section estimates are exactly the ones the gauge accounts from.
	nonHist, histTrace := lastTraceSections(trace.String())
	a.mu.RLock()
	liveHist := 0
	for _, h := range a.history {
		liveHist += h.tokens
	}
	a.mu.RUnlock()
	if got := nonHist + liveHist; got != used {
		t.Errorf("ContextUsage = %d, want %d (non-history %d + live history %d)\n%s",
			used, got, nonHist, liveHist, trace.String())
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
		Consult:             llm.NewClient("http://127.0.0.1:1", "advisor"),
		SkillsWriteApproval: true,
		AskUser:             func(ctx context.Context, q string, choices []string) (string, error) { return "ok", nil },
	})
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

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

	"yagent/internal/index"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/skills"
	"yagent/internal/tools"
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

func (f *fixedSummaryLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta func(string)) (*llm.Response, error) {
	f.calls++
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: f.summary}}, nil
}

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
	// Tiny window so the tool result forces a summarization after turn 1.
	// Turn 1: tool call fs_read on a file with a big-ish content; turn 2: final.
	s := newScriptedLLM(t, [][]string{
		toolCall("c1", "fs_read", `{"path": "big.txt"}`),
		finalContent("done"),
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
		t.Fatalf("Run: %v", err)
	}
	if summ.calls == 0 {
		t.Fatal("summarizer was never called")
	}
	// The summarized message must be gone and the summary present in turn 2.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) < 2 {
		t.Fatalf("only %d requests captured", len(s.requests))
	}
	req2 := string(s.requests[1])
	if !strings.Contains(req2, "SUM: user prefers tabs") {
		t.Errorf("turn 2 missing running summary: %q", req2[:200])
	}
	if strings.Contains(req2, "first message that must disappear") {
		t.Error("summarized message still in turn 2 context")
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
	got, until, err := st.Summary(ctx, sess.ID)
	if err != nil || got != "persisted summary text" || until == 0 {
		t.Errorf("stored summary = %q/%d/%v", got, until, err)
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

// Package eval implements the M6 eval harness: golden YAML tasks that run the
// agent loop against a scripted fake LLM server (plus fake embed/web servers)
// and assert on tool results and the final answer. Used by `go test` (see
// eval_test.go); evals live in testdata/evals/*.yaml.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
	"github.com/Mechres/Yagent/internal/web"
)

// Task is one golden eval.
type Task struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Input       string `yaml:"input"`
	// Inputs runs the agent for each line in sequence (multi-turn flows).
	Inputs []string          `yaml:"inputs"`
	Files  map[string]string `yaml:"files"`
	Steps  []Step            `yaml:"steps"`
	// Subsystem toggles.
	Memory bool `yaml:"memory"`
	Skills bool `yaml:"skills"`
	Index  bool `yaml:"index"`
	Web    bool `yaml:"web"`
	// Window shrinks the context budget when set.
	Window int `yaml:"window"`
	// Summary makes budget summarization deterministic (the value is returned
	// as the running summary) instead of consuming a scripted step.
	Summary string `yaml:"summary"`
	// Goal runs the agent in loop mode toward a goal (DONE/CONTINUE verdicts);
	// Rounds caps it. Takes precedence over Input/Inputs.
	Goal   string `yaml:"goal"`
	Rounds int    `yaml:"rounds"`
	// Subagent wires the subagent tool to a child agent (M7).
	Subagent bool `yaml:"subagent"`

	// DenyFirst denies the first N write/destructive approvals (the model
	// must recover from a user saying no). 0 = allow everything.
	DenyFirst int `yaml:"deny_first"`
	// VerifyWrites enables the deterministic verify-don't-trust barrier.
	VerifyWrites bool `yaml:"verify_writes"`
	// PatchFilter exercises the fs_patch per-hunk approval path: when set, the
	// harness approves the patch with rewritten args containing only the named
	// hunk subset ("first_hunk" | "last_hunk"), so the eval can assert that
	// only the kept hunks were applied.
	PatchFilter string `yaml:"patch_filter"`

	Assert Assertions `yaml:"assert"`
}

// Step is one scripted LLM response.
type Step struct {
	// ToolCall sets the response to a tool call.
	ToolCall *ToolCallStep `yaml:"tool_call"`
	// Answer sets the response to a final answer.
	Answer *string `yaml:"answer"`
}

// ToolCallStep names the tool and the raw JSON argument object.
type ToolCallStep struct {
	Name string `yaml:"name"`
	Args string `yaml:"args"`
}

// Assertions check the run outcome.
type Assertions struct {
	// FinalAnswerContains requires the final answer to contain the text.
	FinalAnswerContains string `yaml:"final_answer_contains"`
	// ToolResultsContain requires at least one tool result to contain each.
	ToolResultsContain []string `yaml:"tool_results_contain"`
	// MinToolResults requires at least this many tool executions.
	MinToolResults int `yaml:"min_tool_results"`
	// StagedSkills requires exactly this many staged skill writes.
	StagedSkills int `yaml:"staged_skills"`
	// NoToolResults requires zero tool executions.
	NoToolResults bool `yaml:"no_tool_results"`
	// AllRequestsHaveUser requires every request sent to the model to contain
	// a plain user message (regression: the budget must never summarize the
	// current user turn away — Qwythos rejects tool-only message lists).
	AllRequestsHaveUser bool `yaml:"all_requests_have_user"`
	// FileContains / FileNotContains assert on workspace file contents after
	// the run (used by the fs_patch partial-approval and denial evals).
	FileContains    []FileAssert `yaml:"file_contains"`
	FileNotContains []FileAssert `yaml:"file_not_contains"`
	// RequestsContain requires at least one request body sent to the model to
	// contain each text (used for the verify-barrier, prose-nudge and ledger
	// injections, which appear in the request stream, not in tool results).
	RequestsContain []string `yaml:"requests_contain"`
}

// FileAssert is one post-run file-content assertion.
type FileAssert struct {
	Path string `yaml:"path"`
	Text string `yaml:"text"`
}

// Run executes one task end-to-end and reports failures via t.
func Run(t *testing.T, task Task) {
	t.Helper()
	if task.Window == 0 {
		task.Window = 16384
	}

	// Fake LLM server serving the scripted steps; also records every request
	// body so the harness can assert on what was actually sent.
	llmServer, reqLog := scriptedServer(t, task.Steps)

	// Fake embed server (neutral vectors) for memory/index.
	var embedTS *httptest.Server
	if task.Memory || task.Index {
		embedTS = embedServer(t)
	}

	// Fake web server (SearXNG JSON + an article page) for web tasks.
	var webTS *httptest.Server
	if task.Web {
		webTS = webServer(t)
	}

	ws := t.TempDir()
	for rel, content := range task.Files {
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// {{WEB_BASE}} in scripted args resolves to the fake web server, so
	// web_fetch stays off the real network.
	if webTS != nil {
		for i := range task.Steps {
			if tc := task.Steps[i].ToolCall; tc != nil {
				tc.Args = strings.ReplaceAll(tc.Args, "{{WEB_BASE}}", webTS.URL)
			}
		}
	}

	dataDir := t.TempDir()
	opts := tools.Options{}
	var vs *memory.VectorStore
	var sk *skills.Store
	var idx *index.Store
	var wc *web.Client

	if task.Memory {
		vs, _ = memory.OpenVectorStore(dataDir, embedTS.URL, "test-embed")
		opts.Vectors = vs
		opts.SessionID = "eval"
	}
	if task.Skills {
		sk, _ = skills.Open(dataDir, ws)
		opts.Skills = sk
		opts.SkillsWriteApproval = true
	}
	if task.Index {
		idx, _ = index.Open(ws, dataDir, embedTS.URL, "test-embed")
		opts.Index = idx
	}
	if task.Web {
		wc, _ = web.New(web.Config{Provider: "searxng", SearxngURL: webTS.URL})
		opts.Web = wc
	}

	reg := tools.NewRegistry(ws, opts)
	client := llm.NewClient(llmServer.URL, "test-model")
	if task.Subagent {
		// The subagent tool delegates to an isolated read-only child agent
		// that consumes the next scripted response. An optional tools slice
		// scopes the child registry (M7 beyond v2).
		opts.Subagent = func(ctx context.Context, subtask, workspace string, toolset []string, role tools.SubagentRole) (string, error) {
			subReg := tools.NewRegistry(workspace, tools.Options{ReadOnly: true, Web: wc, Index: idx, Skills: sk})
			if len(toolset) > 0 {
				var err error
				subReg, err = subReg.Restrict(toolset)
				if err != nil {
					return err.Error(), nil
				}
			}
			answer, tokens, err := agent.RunSubagent(ctx, client, subReg, subtask, workspace, role)
			if err != nil {
				return "error: subagent failed: " + err.Error(), nil
			}
			return fmt.Sprintf("%s\n\n(subagent used ~%d tokens)", answer, tokens), nil
		}
		reg = tools.NewRegistry(ws, opts)
	}
	var summ agent.ChatLLM
	if task.Summary != "" {
		summ = &fixedSummaryLLM{summary: task.Summary}
	}
	a := agent.New(client, reg, newTaskApprover(task, ws), agent.Config{
		MaxIterations:   20,
		Window:          task.Window,
		Vectors:         vs,
		SessionID:       "eval",
		Skills:          sk,
		Index:           idx,
		IndexAutoInject: false,
		Summarizer:      summ,
		VerifyWrites:    task.VerifyWrites,
	}, ws)

	inputs := task.Inputs
	if len(inputs) == 0 {
		inputs = []string{task.Input}
	}
	var answer string
	var err error
	if task.Goal != "" {
		answer, err = a.RunGoal(context.Background(), task.Goal, task.Rounds, nil)
		if err != nil {
			t.Fatalf("RunGoal: %v", err)
		}
	} else {
		for _, in := range inputs {
			answer, err = a.Run(context.Background(), in)
			if err != nil {
				t.Fatalf("Run(%q): %v", in, err)
			}
		}
	}
	history := a.History()
	var toolResults []string
	for _, m := range history {
		if m.Role == "tool" {
			toolResults = append(toolResults, m.Content)
		}
	}

	if task.Assert.FinalAnswerContains != "" &&
		!strings.Contains(answer, task.Assert.FinalAnswerContains) {
		t.Errorf("final answer %q missing %q", answer, task.Assert.FinalAnswerContains)
	}
	for _, want := range task.Assert.ToolResultsContain {
		found := false
		for _, r := range toolResults {
			if strings.Contains(r, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no tool result contains %q (results: %d)", want, len(toolResults))
		}
	}
	if task.Assert.NoToolResults && len(toolResults) != 0 {
		t.Errorf("expected no tool results, got %d", len(toolResults))
	}
	if task.Assert.MinToolResults > 0 && len(toolResults) < task.Assert.MinToolResults {
		t.Errorf("tool results = %d, want >= %d", len(toolResults), task.Assert.MinToolResults)
	}
	if task.Assert.StagedSkills > 0 {
		pending, err := sk.ListPending()
		if err != nil || len(pending) != task.Assert.StagedSkills {
			t.Errorf("staged skills = %d (%v), want %d", len(pending), err, task.Assert.StagedSkills)
		}
	}
	if task.Assert.AllRequestsHaveUser {
		for _, body := range reqLog.bodies() {
			var c struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}
			if json.Unmarshal(body, &c) != nil {
				t.Errorf("could not decode a captured request")
				continue
			}
			hasUser := false
			for _, m := range c.Messages {
				if m.Role == "user" {
					hasUser = true
					break
				}
			}
			if !hasUser {
				t.Error("a request was sent with no user message (budget swallowed the current turn?)")
			}
		}
	}
	for _, want := range task.Assert.RequestsContain {
		found := false
		for _, body := range reqLog.bodies() {
			if strings.Contains(string(body), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no request contained %q", want)
		}
	}
	for _, fa := range task.Assert.FileContains {
		data, err := os.ReadFile(filepath.Join(ws, fa.Path))
		if err != nil {
			t.Errorf("file %s: %v", fa.Path, err)
			continue
		}
		if !strings.Contains(string(data), fa.Text) {
			t.Errorf("file %s missing %q", fa.Path, fa.Text)
		}
	}
	for _, fa := range task.Assert.FileNotContains {
		data, err := os.ReadFile(filepath.Join(ws, fa.Path))
		if err != nil {
			continue // file absent = the assertion holds
		}
		if strings.Contains(string(data), fa.Text) {
			t.Errorf("file %s contains %q (should not)", fa.Path, fa.Text)
		}
	}
}

// taskApprover implements the scripted user for evals: it allows read-only
// tools, denies the first DenyFirst write/destructive calls, and — when
// PatchFilter is set — approves fs_patch with rewritten args that keep only
// the selected hunk subset (exercising the real per-hunk approval path).
type taskApprover struct {
	denyFirst   int
	patchFilter string
	ws          string
	mu          sync.Mutex
	writes      int
}

func newTaskApprover(task Task, ws string) *taskApprover {
	return &taskApprover{denyFirst: task.DenyFirst, patchFilter: task.PatchFilter, ws: ws}
}

func (a *taskApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	if risk == tools.RiskReadOnly {
		return agent.Approval{OK: true}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if call.Function.Name == "fs_patch" && a.patchFilter != "" {
		return a.filterPatch(call)
	}
	a.writes++
	if a.denyFirst > 0 && a.writes <= a.denyFirst {
		return agent.Approval{OK: false}, nil
	}
	return agent.Approval{OK: true}, nil
}

// filterPatch approves an fs_patch with only the selected hunk subset applied,
// mirroring the TUI's hunk walker (RebuildPatch + rewritten Args).
func (a *taskApprover) filterPatch(call llm.ToolCall) (agent.Approval, error) {
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(call.Function.Arguments, &args); err != nil {
		return agent.Approval{OK: true}, nil // unreadable; let the tool handle it
	}
	hunks, err := tools.PatchHunks(args.Patch)
	if err != nil || len(hunks) < 2 {
		return agent.Approval{OK: true}, nil // nothing to filter
	}
	keep := make([]bool, len(hunks))
	switch a.patchFilter {
	case "first_hunk":
		keep[0] = true
	case "last_hunk":
		keep[len(hunks)-1] = true
	default:
		return agent.Approval{OK: true}, nil
	}
	rebuilt, err := tools.RebuildPatch(args.Patch, keep)
	if err != nil {
		return agent.Approval{OK: false}, nil
	}
	out, _ := json.Marshal(map[string]string{"patch": rebuilt})
	return agent.Approval{OK: true, Args: out}, nil
}

// fixedSummaryLLM always returns a fixed message; used as the budget
// summarizer so budget math is deterministic and doesn't consume scripted
// steps.
type fixedSummaryLLM struct{ summary string }

func (f *fixedSummaryLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: f.summary}}, nil
}

// reqLog records every request body the fake server received.
type reqLog struct {
	mu   sync.Mutex
	body [][]byte
}

func (l *reqLog) bodies() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]byte, len(l.body))
	copy(out, l.body)
	return out
}

// scriptedServer serves one scripted SSE response per request and records the
// request bodies for assertions.
func scriptedServer(t *testing.T, steps []Step) (*httptest.Server, *reqLog) {
	t.Helper()
	var mu sync.Mutex
	log := &reqLog{}
	remaining := make([]Step, len(steps))
	copy(remaining, steps)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.mu.Lock()
		log.body = append(log.body, body)
		log.mu.Unlock()
		mu.Lock()
		var step Step
		if len(remaining) > 0 {
			step = remaining[0]
			remaining = remaining[1:]
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
		if step.ToolCall != nil {
			args := step.ToolCall.Args
			write(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, step.ToolCall.Name, args))
		} else {
			text := "no skill"
			if step.Answer != nil {
				text = *step.Answer
			}
			write(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text))
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	})), log
}

// embedServer returns neutral 4-dim vectors for everything.
func embedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var inputs []string
		if err := json.Unmarshal(req.Input, &inputs); err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, 0, len(inputs))
		for i := range inputs {
			data = append(data, item{Object: "embedding", Index: i, Embedding: []float32{1, 0, 0, 0}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
}

// webServer serves SearXNG JSON and a target article. The search result URL
// points back at this same fake server so web_fetch stays offline.
func webServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{
					{"title": "ROCm guide", "url": "http://" + r.Host + "/rocm", "content": "llama.cpp works on gfx1031 with ROCm."},
				},
			})
		case "/rocm":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><h1>ROCm</h1><p>gfx1031 is supported by llama.cpp.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
}

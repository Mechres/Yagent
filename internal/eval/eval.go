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

	"yagent/internal/agent"
	"yagent/internal/index"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/skills"
	"yagent/internal/tools"
	"yagent/internal/web"
)

// Task is one golden eval.
type Task struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Input       string            `yaml:"input"`
	Files       map[string]string `yaml:"files"`
	Steps       []Step            `yaml:"steps"`
	// Subsystem toggles.
	Memory bool `yaml:"memory"`
	Skills bool `yaml:"skills"`
	Index  bool `yaml:"index"`
	Web    bool `yaml:"web"`
	// Window shrinks the context budget when set.
	Window int `yaml:"window"`

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
}

// Run executes one task end-to-end and reports failures via t.
func Run(t *testing.T, task Task) {
	t.Helper()
	if task.Window == 0 {
		task.Window = 16384
	}

	// Fake LLM server serving the scripted steps.
	llmServer := scriptedServer(t, task.Steps)

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
	a := agent.New(client, reg, &stubApprover{allow: true}, agent.Config{
		MaxIterations:   20,
		Window:          task.Window,
		Vectors:         vs,
		SessionID:       "eval",
		Skills:          sk,
		Index:           idx,
		IndexAutoInject: false,
	}, ws)

	answer, err := a.Run(context.Background(), task.Input)
	if err != nil {
		t.Fatalf("Run: %v", err)
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
}

// stubApprover auto-approves every write (evals script the model, not the
// user).
type stubApprover struct{ allow bool }

func (s *stubApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error) {
	return s.allow || risk == tools.RiskReadOnly, nil
}

// scriptedServer serves one scripted SSE response per request.
func scriptedServer(t *testing.T, steps []Step) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	remaining := make([]Step, len(steps))
	copy(remaining, steps)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
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
	}))
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

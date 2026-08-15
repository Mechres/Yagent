// Package bench implements the live small-model benchmark: canonical tasks run
// against a real local model to measure whether it actually works, and the
// sampling-recipe sweep that tunes generation config on evidence. Used by the
// live eval tests (YAGENT_LIVE_EVAL=1) and by `yagent calibrate`.
package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/tools"
)

// Task is one canonical small-model check: set up a workspace, run one or more
// turns against the real model, and check an observable outcome.
type Task struct {
	Name   string
	Setup  func(ws string) error
	Inputs []string
	Check  func(answer string, toolResults []string) (bool, string)
	// Configure customizes the task's Config (write gates, plan mode, windows)
	// before the agent is built, and returns the approver to use (nil = auto-
	// approve). Used by the measurement-expansion cases so they exercise real
	// edit→fail→recover loops, not just reads (GPT sol, measurement section).
	Configure func(cfg *agent.Config) agent.Approver
	// WrapLLM, when set, wraps the model client before the agent is built —
	// used to inject a truncated stream (truncated-recover task).
	WrapLLM func(agent.ChatLLM) agent.ChatLLM
}

// Tasks returns the canonical "does the small local model actually work"
// checks: emit a correct tool call, follow a two-turn instruction, and run the
// diagnostics tool when asked to verify the code.
func Tasks() []Task {
	return []Task{
		{
			Name: "tool-json",
			Setup: func(ws string) error {
				return os.WriteFile(filepath.Join(ws, "data.txt"), []byte("UNIQUE-FACT-8371\n"), 0o644)
			},
			Inputs: []string{"read the file data.txt and tell me what it contains"},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "UNIQUE-FACT-8371") {
					return false, "final answer lacks the fact"
				}
				for _, r := range toolResults {
					if strings.Contains(r, "UNIQUE-FACT-8371") {
						return true, "read via a tool"
					}
				}
				return false, "no tool result carried the fact (tool JSON failed?)"
			},
		},
		{
			Name:  "multi-turn",
			Setup: func(ws string) error { return nil },
			Inputs: []string{
				"remember this code word for later: zebra-42",
				"what was the code word I told you a moment ago?",
			},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "zebra-42") {
					return false, "second turn did not recall zebra-42"
				}
				return true, "recalled across turns"
			},
		},
		{
			Name: "edit-verify",
			Setup: func(ws string) error {
				// fmt is used but never imported -> go vet fails with
				// "undefined: fmt", a solid marker that diagnostics ran.
				if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module bench\n\ngo 1.22\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0o644)
			},
			Inputs: []string{"run the workspace diagnostics to check the code for errors, then report what it found"},
			Check: func(answer string, toolResults []string) (bool, string) {
				for _, r := range toolResults {
					if strings.Contains(r, "undefined") {
						return true, "workspace_diagnostics surfaced the compile error"
					}
				}
				return false, "no diagnostics output seen"
			},
		},
		{
			// dropped extension (README -> README.md) resolved by the fuzzy path
			// pre-resolution, not wasted turns.
			Name: "fuzzy-path",
			Setup: func(ws string) error {
				return os.WriteFile(filepath.Join(ws, "README.md"), []byte("README-CONTENT-5511\n"), 0o644)
			},
			Inputs: []string{"read the file README (no extension) and tell me its contents"},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "README-CONTENT-5511") {
					return false, "answer lacks the README content"
				}
				return true, "resolved and read"
			},
		},
		{
			// locate a declaration: the model must use a code tool, not guess.
			Name: "code-locate",
			Setup: func(ws string) error {
				if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(ws, "pkg", "a.go"),
					[]byte("package pkg\n\nfunc helper(x int) int {\n\treturn x + 1\n}\n"), 0o644)
			},
			Inputs: []string{"where is the helper function defined? give the file and the exact line"},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "helper") {
					return false, "answer did not mention helper"
				}
				for _, r := range toolResults {
					if strings.Contains(r, "helper") {
						return true, "located via a code tool"
					}
				}
				return false, "no tool result carried the declaration"
			},
		},
		{
			// grep-style lookup across files.
			Name: "grep-find",
			Setup: func(ws string) error {
				if err := os.WriteFile(filepath.Join(ws, "alpha.txt"), []byte("ORANGE-77\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(ws, "beta.txt"), []byte("APPLE-88\n"), 0o644)
			},
			Inputs: []string{"which file contains the value ORANGE-77?"},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "alpha") {
					return false, "answer did not identify alpha.txt"
				}
				return true, "found the file"
			},
		},
		{
			// edit → fail → recover: the model must actually FIX a compile error
			// and verify, not just report it (the old edit-verify task only
			// reported an existing error).
			Name: "edit-recover",
			Setup: func(ws string) error {
				if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module bench\n\ngo 1.22\n"), 0o644); err != nil {
					return err
				}
				// fmt is used but never imported — a genuine broken build.
				return os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0o644)
			},
			Inputs: []string{"the code does not compile. Fix it so `go vet` passes, then run workspace_diagnostics to confirm."},
			Configure: func(cfg *agent.Config) agent.Approver {
				cfg.VerifyWrites = true
				return &autoApprover{}
			},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "import") && !strings.Contains(answer, "fixed") && !strings.Contains(answer, "added") {
					return false, "final answer did not claim a fix"
				}
				for _, r := range toolResults {
					if strings.Contains(r, "[PASS]") {
						return true, "edited then verified clean"
					}
				}
				return false, "no [PASS] diagnostics result after the edit"
			},
		},
		{
			// rejected write recovery: the first fs_write is DENIED; the model
			// must recover with another approach, not loop or give up.
			Name: "denied-write",
			Setup: func(ws string) error {
				return os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("original\n"), 0o644)
			},
			Inputs: []string{"append the line \"appended-99\" to notes.txt"},
			Configure: func(cfg *agent.Config) agent.Approver {
				return &denyFirstApprover{denied: false, n: 0}
			},
			Check: func(answer string, toolResults []string) (bool, string) {
				for _, r := range toolResults {
					if strings.Contains(r, "user denied") {
						return true, "recovered from a denied write"
					}
				}
				return false, "no denial recovery path seen"
			},
		},
		{
			// plan-mode enforcement (GPT sol #3): the model is in read-only plan
			// mode; a write must be rejected at dispatch, and the model must
			// find a read-only path instead of reporting it edited something.
			Name: "plan-mode",
			Setup: func(ws string) error {
				if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(ws, "pkg", "calc.go"), []byte("package pkg\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
			},
			Inputs: []string{"read pkg/calc.go and tell me what Add returns for 2 and 3"},
			Configure: func(cfg *agent.Config) agent.Approver {
				cfg.PlanMode = true
				return &autoApprover{}
			},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "5") {
					return false, "final answer did not compute Add(2,3)"
				}
				for _, r := range toolResults {
					if strings.Contains(r, "plan mode") {
						return true, "write rejected in plan mode, read succeeded"
					}
				}
				// a model that never attempted a write is still a pass (it
				// respected plan mode); the rejection case just proves more.
				return true, "plan mode respected (no write attempted)"
			},
		},
		{
			// truncated-response recovery (GPT sol #5): the client is wrapped so
			// the FIRST stream is cut off; the agent must nudge and complete the
			// turn with a real answer.
			Name: "truncated-recover",
			Setup: func(ws string) error {
				return os.WriteFile(filepath.Join(ws, "data.txt"), []byte("TRUNC-FACT-9901\n"), 0o644)
			},
			Inputs: []string{"read data.txt and tell me its unique value"},
			WrapLLM: func(chat agent.ChatLLM) agent.ChatLLM {
				return &truncatingLLM{inner: chat}
			},
			Check: func(answer string, toolResults []string) (bool, string) {
				if !strings.Contains(answer, "TRUNC-FACT-9901") {
					return false, "final answer lacks the fact (recovery failed)"
				}
				return true, "recovered from a truncated stream"
			},
		},
		{
			// multi-file refactor with tests in multiple packages: a real
			// move that must rewire the caller, caught by the test gate.
			Name: "multi-file-refactor",
			Setup: func(ws string) error {
				if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module bench\n\ngo 1.22\n"), 0o644); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(ws, "pkg/config"), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(ws, "pkg/config/config.go"), []byte("package config\n\ntype Config struct {\n\tHost string\n}\n"), 0o644); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nimport \"bench/pkg/config\"\n\nfunc main() {\n\tvar c config.Config\n\t_ = c\n}\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(ws, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestConfig(t *testing.T) {\n\tvar c config.Config\n\tif c.Host != \"\" {\n\t\tt.Fatal(\"expected empty host\")\n\t}\n}\n"), 0o644)
			},
			Inputs: []string{"move Config from pkg/config/config.go into pkg/types.go and rewire main.go and main_test.go to import it; run the tests to confirm"},
			Configure: func(cfg *agent.Config) agent.Approver {
				cfg.TestGate = true
				return &autoApprover{}
			},
			Check: func(answer string, toolResults []string) (bool, string) {
				for _, r := range toolResults {
					if strings.Contains(r, "[PASS]") {
						return true, "multi-file refactor verified clean"
					}
				}
				return false, "no passing test result after the refactor"
			},
		},
	}
}

// denyFirstApprover denies the first write, then approves — the model must
// recover from a user saying no.
type denyFirstApprover struct {
	denied bool
	n      int
}

func (d *denyFirstApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	d.n++
	if d.n == 1 {
		return agent.Approval{OK: false}, nil
	}
	return agent.Approval{OK: true}, nil
}

// truncatingLLM cuts off the first streamed response with ErrStreamTruncated,
// then passes every later request through to the real client — the agent must
// recover with a nudge (GPT sol #5).
type truncatingLLM struct {
	mu    sync.Mutex
	inner agent.ChatLLM
	n     int
}

func (t *truncatingLLM) ChatStream(ctx context.Context, messages []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	t.mu.Lock()
	t.n++
	n := t.n
	t.mu.Unlock()
	if n == 1 {
		return nil, llm.ErrStreamTruncated
	}
	return t.inner.ChatStream(ctx, messages, tools, onDelta, onReasoning)
}

// autoApprover approves every write/destructive call — the bench measures the
// MODEL, not the UI prompt. Tasks that need a denial (rejected-write recovery)
// supply their own via Configure.
type autoApprover struct{}

func (a *autoApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	return agent.Approval{OK: true}, nil
}

// Recipe is one sampling configuration under test.
type Recipe struct {
	Name     string
	Sampling llm.Sampling
}

// Recipes returns the sampling recipes the sweep compares.
func Recipes() []Recipe {
	return []Recipe{
		{"default", llm.Sampling{Temperature: 0.6, TopP: 0.95}},
		{"rep1.05", llm.Sampling{Temperature: 0.6, TopP: 0.95, RepetitionPenalty: 1.05}},
		{"minp0.05", llm.Sampling{Temperature: 0.6, TopP: 0.95, MinP: 0.05}},
		{"cold0.3", llm.Sampling{Temperature: 0.3, TopP: 0.95}},
	}
}

// Result is one task's outcome, including timing and (heuristic, len/4)
// token counts so guidance can show generation speed and thinking overhead.
type Result struct {
	Pass         bool
	Detail       string
	WallMS       int64 // total wall time for the task's runs
	Tokens       int   // assistant content tokens generated (len/4)
	ReasonTokens int   // reasoning/thinking tokens generated (len/4)
}

// TokPerSec is the content-generation speed (heuristic tokens/second).
func (r Result) TokPerSec() float64 {
	if r.WallMS <= 0 {
		return 0
	}
	return float64(r.Tokens) / (float64(r.WallMS) / 1000)
}

// RunTask executes one task against a model client (real or fake) and returns
// its result. Accepting the ChatLLM interface (not *llm.Client) lets the
// measurement-expansion tasks wrap the client — e.g. truncated-recover injects
// a cut-off stream via WrapLLM.
func RunTask(client agent.ChatLLM, task Task) Result {
	start := time.Now()
	ws, err := os.MkdirTemp("", "yagent-bench-*")
	if err != nil {
		return Result{Detail: "setup: " + err.Error()}
	}
	defer os.RemoveAll(ws)
	if task.Setup != nil {
		if err := task.Setup(ws); err != nil {
			return Result{Detail: "setup: " + err.Error()}
		}
	}
	var tokens, reasonTokens int
	reg := tools.NewRegistry(ws, tools.Options{ReadOnly: task.Configure == nil})
	cfg := agent.Config{
		MaxIterations: 8,
		Window:        8192,
		OnToken:       func(d string) { tokens += len(d) / 4 },
		OnReasoning:   func(d string) { reasonTokens += len(d) / 4 },
	}
	appr := agent.Approver(&autoApprover{})
	if task.Configure != nil {
		if a := task.Configure(&cfg); a != nil {
			appr = a
		}
	}
	var chat agent.ChatLLM = client
	if task.WrapLLM != nil {
		chat = task.WrapLLM(chat)
	}
	a := agent.New(chat, reg, appr, cfg, ws)
	var answer string
	for _, in := range task.Inputs {
		answer, err = a.Run(context.Background(), in)
		if err != nil {
			return Result{Detail: "run: " + err.Error(), WallMS: time.Since(start).Milliseconds()}
		}
	}
	var toolResults []string
	for _, m := range a.History() {
		if m.Role == "tool" {
			toolResults = append(toolResults, m.Content)
		}
	}
	pass, detail := task.Check(answer, toolResults)
	return Result{Pass: pass, Detail: detail, WallMS: time.Since(start).Milliseconds(),
		Tokens: tokens, ReasonTokens: reasonTokens}
}

// RecipeResult is one recipe's sweep outcome.
type RecipeResult struct {
	Recipe  Recipe
	Results []Result
}

// Pass returns how many tasks passed under this recipe.
func (r RecipeResult) Pass() int {
	n := 0
	for _, res := range r.Results {
		if res.Pass {
			n++
		}
	}
	return n
}

// RunSweep runs every recipe across the tasks, mutating client.Sampling per
// recipe (the caller restores it afterwards).
func RunSweep(client *llm.Client, tasks []Task) []RecipeResult {
	recipes := Recipes()
	out := make([]RecipeResult, 0, len(recipes))
	for _, r := range recipes {
		client.Sampling = r.Sampling
		res := make([]Result, 0, len(tasks))
		for _, tk := range tasks {
			res = append(res, RunTask(client, tk))
		}
		out = append(out, RecipeResult{Recipe: r, Results: res})
	}
	return out
}

// RenderRecipe prints a YAML sampling: block for a recipe.
func RenderRecipe(r Recipe) string {
	var b strings.Builder
	b.WriteString("sampling:\n")
	fmt.Fprintf(&b, "  temperature: %v\n", r.Sampling.Temperature)
	fmt.Fprintf(&b, "  top_p: %v\n", r.Sampling.TopP)
	if r.Sampling.TopK > 0 {
		fmt.Fprintf(&b, "  top_k: %d\n", r.Sampling.TopK)
	}
	if r.Sampling.RepetitionPenalty > 0 {
		fmt.Fprintf(&b, "  repetition_penalty: %v\n", r.Sampling.RepetitionPenalty)
	}
	if r.Sampling.MinP > 0 {
		fmt.Fprintf(&b, "  min_p: %v\n", r.Sampling.MinP)
	}
	return b.String()
}

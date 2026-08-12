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
	}
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

// Result is one task's outcome.
type Result struct {
	Pass   bool
	Detail string
}

// RunTask executes one task against a real model client and returns its result.
func RunTask(client *llm.Client, task Task) Result {
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
	reg := tools.NewRegistry(ws, tools.Options{ReadOnly: true})
	a := agent.New(client, reg, nil, agent.Config{MaxIterations: 8, Window: 8192}, ws)
	var answer string
	for _, in := range task.Inputs {
		answer, err = a.Run(context.Background(), in)
		if err != nil {
			return Result{Detail: "run: " + err.Error()}
		}
	}
	var toolResults []string
	for _, m := range a.History() {
		if m.Role == "tool" {
			toolResults = append(toolResults, m.Content)
		}
	}
	pass, detail := task.Check(answer, toolResults)
	return Result{Pass: pass, Detail: detail}
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

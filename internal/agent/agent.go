// Package agent implements the agent loop: linear, explicit and defensive by
// design (see docs/design/agent-loop.md). It drives the LLM, dispatches tool
// calls through the registry with validation/approval/truncation, and never
// grows history without a budget check.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"yagent/internal/llm"
	"yagent/internal/tools"
)

// ErrMaxIterations is returned when the loop hits the iteration cap without a
// final answer.
var ErrMaxIterations = errors.New("reached max iterations without a final answer")

// DefaultMaxIterations caps a single Run when Config.MaxIterations is 0.
const DefaultMaxIterations = 25

// maxValidationFails is how many argument-validation failures on the same tool
// are tolerated before the loop blocks that tool for the rest of the turn.
const maxValidationFails = 3

// Config holds loop knobs.
type Config struct {
	MaxIterations int
	// OnToken is called with each content delta as it streams from the model
	// (used by the UI to print tokens live). Optional.
	OnToken func(string)
	// OnTool is called before a tool executes (used by the UI to show
	// activity). Optional.
	OnTool func(llm.ToolCall)
}

// Approver gates Write/Destructive tool calls; implemented by the UI.
type Approver interface {
	Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error)
}

// ChatLLM is the model client the loop needs. *llm.Client satisfies it.
type ChatLLM interface {
	ChatStream(ctx context.Context, messages []llm.Message, tools []llm.ToolSchema, onDelta func(string)) (*llm.Response, error)
}

// Agent runs one conversation against a model, dispatching tools.
type Agent struct {
	cfg      Config
	llm      ChatLLM
	registry *tools.Registry
	approver Approver

	workspace    string
	systemPrompt string
	history      []llm.Message
}

// New constructs an Agent bound to a workspace and tool registry.
func New(llm ChatLLM, reg *tools.Registry, approver Approver, cfg Config, workspace string) *Agent {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	if cfg.OnToken == nil {
		cfg.OnToken = func(string) {}
	}
	return &Agent{
		cfg:          cfg,
		llm:          llm,
		registry:     reg,
		approver:     approver,
		workspace:    workspace,
		systemPrompt: buildSystemPrompt(workspace),
	}
}

// Reset clears conversation history (the /clear command).
func (a *Agent) Reset() { a.history = nil }

// History exposes the conversation for inspection/tests.
func (a *Agent) History() []llm.Message { return a.history }

// Run processes one user input through the loop and returns the final answer
// text (which was also streamed via cfg.OnToken).
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	a.history = append(a.history, llm.Message{Role: "user", Content: input})

	valFails := make(map[string]int)
	blocked := make(map[string]bool)

	for i := 0; i < a.cfg.MaxIterations; i++ {
		resp, err := a.llm.ChatStream(ctx, a.assembleContext(), a.registry.Schemas(), a.cfg.OnToken)
		if err != nil {
			return "", err
		}
		a.history = append(a.history, resp.Message)

		if len(resp.ToolCalls) == 0 {
			return resp.Message.Content, nil // final answer, done
		}

		results := a.dispatchAll(ctx, resp.ToolCalls, valFails, blocked)
		for i, call := range resp.ToolCalls {
			a.history = append(a.history, llm.Message{
				Role:       "tool",
				Content:    results[i],
				ToolCallID: call.ID,
			})
		}
		if err := a.budget(ctx); err != nil {
			return "", err
		}
	}
	return "", ErrMaxIterations
}

// assembleContext prepends the system prompt to the raw history. Tool schemas
// are sent via the API's tools field, never in the prompt.
func (a *Agent) assembleContext() []llm.Message {
	msgs := make([]llm.Message, 0, len(a.history)+1)
	msgs = append(msgs, llm.Message{Role: "system", Content: a.systemPrompt})
	return append(msgs, a.history...)
}

// dispatchAll runs tool calls: read-only batches execute concurrently, any
// write/destructive call forces sequential execution after approval.
func (a *Agent) dispatchAll(ctx context.Context, calls []llm.ToolCall, valFails map[string]int, blocked map[string]bool) []string {
	results := make([]string, len(calls))
	if len(calls) > 1 && a.allReadOnly(calls) {
		var wg sync.WaitGroup
		for i, call := range calls {
			wg.Add(1)
			go func(i int, call llm.ToolCall) {
				defer wg.Done()
				results[i] = a.dispatch(ctx, call, valFails, blocked)
			}(i, call)
		}
		wg.Wait()
		return results
	}
	for i, call := range calls {
		results[i] = a.dispatch(ctx, call, valFails, blocked)
	}
	return results
}

func (a *Agent) allReadOnly(calls []llm.ToolCall) bool {
	for _, call := range calls {
		tool, ok := a.registry.Get(call.Function.Name)
		if !ok || tool.Risk() != tools.RiskReadOnly {
			return false
		}
	}
	return true
}

// dispatch validates, approves and executes one tool call, returning the text
// the model sees.
func (a *Agent) dispatch(ctx context.Context, call llm.ToolCall, valFails map[string]int, blocked map[string]bool) string {
	name := call.Function.Name

	tool, ok := a.registry.Get(name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q, available: %s", name, strings.Join(a.registry.Names(), ", "))
	}
	if blocked[name] {
		return fmt.Sprintf("error: tool %q is blocked for this turn (repeated validation failures)", name)
	}

	if tool.Risk() != tools.RiskReadOnly {
		allowed, err := a.approver.Approve(ctx, call, tool.Risk())
		if err != nil {
			return fmt.Sprintf("error: approval failed: %v", err)
		}
		if !allowed {
			return "error: user denied this action; find another approach or explain why you cannot proceed"
		}
	}

	if a.cfg.OnTool != nil {
		a.cfg.OnTool(call)
	}

	result, err := tool.Execute(ctx, call.Function.Arguments)
	if err != nil {
		// Only argument-validation failures land here (tool contract).
		valFails[name]++
		if valFails[name] >= maxValidationFails {
			blocked[name] = true
			return fmt.Sprintf("error: tool %q failed validation %d times; last error: %v. Do not call %q again this turn.",
				name, valFails[name], err, name)
		}
		return "error: " + err.Error()
	}
	return result
}

// budget guards history growth. M2 relies on per-tool result caps; real
// summarization (memory.md L1) lands in M3.
func (a *Agent) budget(ctx context.Context) error { return nil }

func buildSystemPrompt(workspace string) string {
	return fmt.Sprintf(`You are Yagent, a local-first AI coding agent running in the workspace:

%s

Rules:
- Be concise. Answer in the fewest words that fully address the request.
- Inspect the workspace with tools instead of guessing: use fs_read / grep / glob to read code, git_status / git_diff / git_log for git state.
- All tool arguments must be valid JSON matching the tool schema; paths are relative to the workspace root.
- To use a tool, emit the tool call now. Never just describe a tool call you intend to make; if your turn ends without a tool call, that text is treated as your final answer.
- If a tool returns an error, read it, fix your arguments, and retry — do not repeat the same failing call.
- Never claim you ran a tool you did not run, and never invent file contents or command output.
- Side-effecting tools (fs_write, fs_edit, shell_exec) prompt the user for approval. If the user denies, find another approach or explain why you cannot proceed.
- When you have the final answer, reply with plain text and no tool calls.`, workspace)
}

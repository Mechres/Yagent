// Package agent implements the agent loop: linear, explicit and defensive by
// design (see docs/design/agent-loop.md). It drives the LLM, dispatches tool
// calls through the registry with validation/approval/truncation, persists
// messages to the session store, budgets context with a running summary, and
// injects recalled semantic memories.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"yagent/internal/index"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/skills"
	"yagent/internal/tools"
)

// ErrMaxIterations is returned when the loop hits the iteration cap without a
// final answer.
var ErrMaxIterations = errors.New("reached max iterations without a final answer")

// Defaults when Config fields are zero.
const (
	DefaultMaxIterations = 25
	DefaultWindow        = 16384
	DefaultReserve       = 2048
	DefaultMemoryTokens  = 1000
	DefaultRecallK       = 5
)

// maxValidationFails is how many argument-validation failures on the same tool
// are tolerated before the loop blocks that tool for the rest of the turn.
const maxValidationFails = 3

// summaryPrompt condenses old history into the running summary (memory.md L1).
const summaryPrompt = `Condense this conversation segment into at most 400 words. Preserve: decisions made, file paths touched, errors encountered, user preferences, open tasks. Drop: pleasantries, repeated code, verbose tool output.`

// Skills constants (docs/design/skills.md).
const (
	// skillTriggerMinCalls is how many tool calls in a turn make the
	// end-of-turn skill-creation opportunity worth offering.
	skillTriggerMinCalls = 5
	// maxL0Skills caps the skills index in the system context.
	maxL0Skills = 40
	// maxL0Tokens caps the L0 index budget (heuristic chars = tokens*4).
	maxL0Tokens = 3000
	// maxIndexChunks / maxIndexTokens cap per-turn code retrieval (M4).
	maxIndexChunks = 6
	maxIndexTokens = 2000
)

// skillCreationPrompt is the one-shot end-of-turn opportunity: it offers only
// the skills tools so the model proposes a gated skill write or nothing.
const skillCreationPrompt = `One-shot skill-creation opportunity (procedural memory).
A skill is a reusable procedure saved as SKILL.md that you can load later with skill_view. Create one IF this turn met a trigger:
- You completed a complex task (5+ tool calls) successfully.
- You hit errors or dead ends and found the working path.
- The user corrected your approach.
- You discovered a non-trivial workflow worth reusing.

If NONE apply, reply with plain text "no skill".

If one applies:
1. Call skills_list FIRST; if an existing skill already covers the procedure, propose a patch to it instead of creating a duplicate.
2. Propose exactly ONE skill_manage write (create or patch).

Authoring rules:
- name: lowercase slug [a-z][a-z0-9_-]*, one procedure per skill.
- description: one line, at most 60 characters, stating the trigger ("when X ...").
- Sections in order: ## When to Use, ## Procedure, ## Pitfalls, ## Verification.
- Steps must be concrete, with real paths and commands from this session. Never invent tools or commands the skill cannot actually run.
- SKILL.md under 8 KiB; reference files under 16 KiB.

Writes are gated: they are staged and applied only after review. Use only skills_list, skill_view or skill_manage in this turn.`

// historyEntry pairs a persisted message with its store row id (0 when not
// persisted), so the budget manager knows which messages a summary covers.
type historyEntry struct {
	id  int64
	msg llm.Message
}

// Config holds loop knobs.
type Config struct {
	MaxIterations int
	// OnToken is called with each content delta as it streams from the model
	// (used by the UI to print tokens live). Optional.
	OnToken func(string)
	// OnTool is called before a tool executes (used by the UI to show
	// activity). Optional.
	OnTool func(llm.ToolCall)

	// Window and Reserve bound the context budget (tokens, heuristic len/4).
	Window  int
	Reserve int
	// MemoryMaxTokens caps the recall injection (default 1000).
	MemoryMaxTokens int

	// Summarizer is used to condense old history; when nil, the main LLM is
	// used. Tools are never offered to the summarizer.
	Summarizer ChatLLM

	// Store/SessionID enable L2 persistence; Vectors enables L3 recall.
	Store     *memory.Store
	SessionID string
	Vectors   *memory.VectorStore

	// Skills enables the L0 skills index and the autonomous skill-creation
	// opportunity (M3.5). May be nil.
	Skills *skills.Store

	// Index enables per-turn code retrieval (M4); set IndexAutoInject to
	// inject the top matching chunks into context each turn.
	Index           *index.Store
	IndexAutoInject bool

	// InitialHistory/InitialSummary seed a resumed session (chat --continue).
	InitialHistory []llm.Message
	InitialSummary string
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
	summ     ChatLLM
	registry *tools.Registry
	approver Approver

	workspace      string
	systemPrompt   string
	history        []historyEntry
	runningSummary string
	injected       []string
	totalToolCalls int
}

// New constructs an Agent bound to a workspace and tool registry.
func New(llm ChatLLM, reg *tools.Registry, approver Approver, cfg Config, workspace string) *Agent {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.Reserve <= 0 {
		cfg.Reserve = DefaultReserve
	}
	if cfg.MemoryMaxTokens <= 0 {
		cfg.MemoryMaxTokens = DefaultMemoryTokens
	}
	if cfg.OnToken == nil {
		cfg.OnToken = func(string) {}
	}
	if cfg.Summarizer == nil {
		cfg.Summarizer = llm
	}
	a := &Agent{
		cfg:            cfg,
		llm:            llm,
		summ:           cfg.Summarizer,
		registry:       reg,
		approver:       approver,
		workspace:      workspace,
		systemPrompt:   buildSystemPrompt(workspace),
		runningSummary: cfg.InitialSummary,
	}
	for _, m := range cfg.InitialHistory {
		a.history = append(a.history, historyEntry{msg: m})
	}
	return a
}

// Reset clears conversation history and the running summary (/clear).
func (a *Agent) Reset() {
	a.history = nil
	a.runningSummary = ""
	a.injected = nil
}

// History exposes the conversation for inspection/tests.
func (a *Agent) History() []llm.Message {
	out := make([]llm.Message, 0, len(a.history))
	for _, h := range a.history {
		out = append(out, h.msg)
	}
	return out
}

// Run processes one user input through the loop and returns the final answer
// text (which was also streamed via cfg.OnToken).
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	if _, err := a.appendMessage(ctx, llm.Message{Role: "user", Content: input}); err != nil {
		return "", err
	}
	recall := a.recall(ctx, input)
	code := a.codeIndex(ctx, input)

	valFails := make(map[string]int)
	blocked := make(map[string]bool)
	turnCalls := 0

	for i := 0; i < a.cfg.MaxIterations; i++ {
		// Budget before every request: plain chat turns (no tool calls)
		// would otherwise never trigger summarization.
		if err := a.budget(ctx); err != nil {
			return "", err
		}
		resp, err := a.llm.ChatStream(ctx, a.assembleContext(recall, code), a.registry.Schemas(), a.cfg.OnToken)
		if err != nil {
			return "", err
		}
		if _, err := a.appendMessage(ctx, resp.Message); err != nil {
			return "", err
		}

		if len(resp.ToolCalls) == 0 {
			a.totalToolCalls += turnCalls
			_ = a.maybeOfferSkillCreation(ctx, turnCalls) // best-effort
			return resp.Message.Content, nil              // final answer, done
		}
		turnCalls += len(resp.ToolCalls)

		results := a.dispatchAll(ctx, resp.ToolCalls, valFails, blocked)
		for i, call := range resp.ToolCalls {
			if _, err := a.appendMessage(ctx, llm.Message{
				Role:       "tool",
				Content:    results[i],
				ToolCallID: call.ID,
			}); err != nil {
				return "", err
			}
		}
	}
	return "", ErrMaxIterations
}

// Finish runs the one-shot skill-creation opportunity at session end (M3.5),
// unless the session did no tool work or the staging cap is already reached.
func (a *Agent) Finish(ctx context.Context) error {
	if a.cfg.Skills == nil || a.totalToolCalls == 0 {
		return nil
	}
	return a.offerSkillCreation(ctx)
}

// InjectSystem records a system-message chunk (e.g. a SKILL.md loaded via
// /skill-name) that is folded into the single leading system message on the
// next request. It is never persisted and does not disturb conversation order.
func (a *Agent) InjectSystem(content string) {
	if content == "" {
		return
	}
	a.injected = append(a.injected, content)
}

// maybeOfferSkillCreation gates the end-of-turn opportunity on trigger size
// and the per-session staging cap.
func (a *Agent) maybeOfferSkillCreation(ctx context.Context, turnCalls int) error {
	if a.cfg.Skills == nil || turnCalls < skillTriggerMinCalls {
		return nil
	}
	return a.offerSkillCreation(ctx)
}

// offerSkillCreation appends the one-shot opportunity prompt, offers only the
// skills tools, and dispatches any proposed skill_manage write through the
// normal path (validation + gate). The exchange is persisted only when the
// model actually proposes a write, so quiet turns leave no trace.
func (a *Agent) offerSkillCreation(ctx context.Context) error {
	if a.cfg.Skills == nil || a.cfg.Skills.StagedCount() >= skills.MaxStagedPerSession {
		return nil
	}
	if err := a.budget(ctx); err != nil {
		return nil // best-effort; a full window must not break the turn
	}
	prompt := llm.Message{Role: "user", Content: skillCreationPrompt}
	msgs := append(a.assembleContext("", ""), prompt)
	resp, err := a.llm.ChatStream(ctx, msgs, a.registry.SchemasFor(skillToolNames), func(string) {})
	if err != nil {
		return nil // best-effort: a dead server must not break the turn
	}
	if len(resp.ToolCalls) == 0 {
		return nil // model declined; nothing to record
	}
	if _, err := a.appendMessage(ctx, prompt); err != nil {
		return err
	}
	if _, err := a.appendMessage(ctx, resp.Message); err != nil {
		return err
	}
	results := a.dispatchAll(ctx, resp.ToolCalls, make(map[string]int), make(map[string]bool))
	for i, call := range resp.ToolCalls {
		if _, err := a.appendMessage(ctx, llm.Message{
			Role:       "tool",
			Content:    results[i],
			ToolCallID: call.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

var skillToolNames = []string{"skills_list", "skill_view", "skill_manage"}

// appendMessage records a message in memory and persists it when a store is
// configured, returning its store row id (0 when not persisted).
func (a *Agent) appendMessage(ctx context.Context, msg llm.Message) (int64, error) {
	a.history = append(a.history, historyEntry{msg: msg})
	if a.cfg.Store == nil || a.cfg.SessionID == "" {
		return 0, nil
	}
	id, err := a.cfg.Store.Append(ctx, a.cfg.SessionID, msg)
	if err != nil {
		return 0, fmt.Errorf("persist message: %w", err)
	}
	a.history[len(a.history)-1].id = id
	return id, nil
}

// recall injects the top-k semantic memories for the user input (L3),
// budgeted and deduplicated against the current session.
func (a *Agent) recall(ctx context.Context, input string) string {
	if a.cfg.Vectors == nil {
		return ""
	}
	memories, err := a.cfg.Vectors.Search(ctx, input, DefaultRecallK)
	if err != nil {
		// Recall is best-effort; a broken memory store must not kill the turn.
		return ""
	}
	var b strings.Builder
	for _, m := range memories {
		if m.SessionID != "" && m.SessionID == a.cfg.SessionID {
			continue // dedupe against this session's own messages
		}
		fmt.Fprintf(&b, "- [%.2f] %s\n", m.Score, m.Text)
	}
	if b.Len() == 0 {
		return ""
	}
	// Cap at the memory budget (heuristic tokens = chars/4).
	maxChars := a.cfg.MemoryMaxTokens * 4
	if b.Len() > maxChars {
		return "Relevant memories:\n" + b.String()[:maxChars] + "\n…"
	}
	return "Relevant memories:\n" + b.String()
}

// assembleContext prepends ONE system message — system prompt + L0 skills
// index + code retrieval + running summary + recall + injected chunks — then
// the history. All system content is merged into a single leading system
// message because the llama.cpp chat templates in use (Qwythos-9B) reject a
// request whose system messages are not contiguous at the start (see
// docs/models.md). Tool schemas are sent via the API's tools field, never in
// the prompt.
func (a *Agent) assembleContext(recall, code string) []llm.Message {
	var sys strings.Builder
	sys.WriteString(a.systemPrompt)
	if l0 := a.skillIndex(); l0 != "" {
		sys.WriteString("\n\n" + l0)
	}
	if code != "" {
		sys.WriteString("\n\n" + code)
	}
	if a.runningSummary != "" {
		sys.WriteString("\n\nSummary of the earlier part of this conversation:\n" + a.runningSummary)
	}
	if recall != "" {
		sys.WriteString("\n\n" + recall)
	}
	for _, chunk := range a.injected {
		sys.WriteString("\n\n" + chunk)
	}
	msgs := []llm.Message{{Role: "system", Content: sys.String()}}
	for _, h := range a.history {
		msgs = append(msgs, h.msg)
	}
	return msgs
}

// codeIndex retrieves the top chunks matching the user input from the codebase
// index (M4) and formats them as path:start-end blocks, budget-capped. Skipped
// when the index is empty or auto-injection is off.
func (a *Agent) codeIndex(ctx context.Context, input string) string {
	if a.cfg.Index == nil || !a.cfg.IndexAutoInject || a.cfg.Index.Count() == 0 {
		return ""
	}
	results, err := a.cfg.Index.Search(ctx, input, maxIndexChunks)
	if err != nil || len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant code from the workspace index (path:start-end):\n")
	for _, r := range results {
		fmt.Fprintf(&b, "- %s:%d-%d\n", r.Path, r.StartLine, r.EndLine)
		snippet := r.Content
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		b.WriteString("  " + strings.ReplaceAll(snippet, "\n", "\n  ") + "\n")
	}
	if b.Len() > maxIndexTokens*4 {
		s := b.String()[:maxIndexTokens*4]
		return s + "\n…"
	}
	return b.String()
}

// skillIndex renders the L0 list (skills.md progressive disclosure): name +
// one-line description, capped at maxL0Skills and maxL0Tokens, evicting by
// last_used desc (the store already sorts that way).
func (a *Agent) skillIndex() string {
	if a.cfg.Skills == nil {
		return ""
	}
	metas := a.cfg.Skills.List()
	if len(metas) > maxL0Skills {
		metas = metas[:maxL0Skills]
	}
	var b strings.Builder
	b.WriteString("Available skills (procedural memory) — call skill_view <name> when a trigger matches:\n")
	for _, m := range metas {
		if m.Category != "" {
			fmt.Fprintf(&b, "- %s [%s, %s]: %s\n", m.Name, m.Category, m.Source, m.Description)
		} else {
			fmt.Fprintf(&b, "- %s [%s]: %s\n", m.Name, m.Source, m.Description)
		}
	}
	if b.Len() > maxL0Tokens*4 {
		s := b.String()[:maxL0Tokens*4]
		return s + "\n…"
	}
	return b.String()
}

// estTokens is the heuristic len/4 token estimate for the whole context.
func (a *Agent) estTokens() int {
	total := len(a.systemPrompt)/4 + len(a.runningSummary)/4
	if a.cfg.Skills != nil {
		total += maxL0Tokens // L0 skills index is always in context
	}
	if a.cfg.Index != nil && a.cfg.IndexAutoInject {
		total += maxIndexTokens // per-turn code retrieval
	}
	for _, chunk := range a.injected {
		total += len(chunk) / 4
	}
	for _, h := range a.history {
		total += len(h.msg.Content)/4 + len(h.msg.ToolCallID)/4
	}
	return total
}

// budget summarizes the oldest half of history when the window is exceeded,
// so context never grows unbounded (memory.md L1). The summarized segment is
// replaced by the running summary, and the store records what is covered.
func (a *Agent) budget(ctx context.Context) error {
	limit := a.cfg.Window - a.cfg.Reserve
	if limit < a.cfg.Window/2 {
		limit = a.cfg.Window / 2 // keep the limit sane for tiny windows
	}
	if a.estTokens() <= limit {
		return nil
	}
	// summarize the oldest half of history (excluding the current user turn
	// is unnecessary — half by count is what memory.md prescribes)
	seg := a.history[:len(a.history)/2]
	if len(seg) == 0 {
		return nil
	}

	var b strings.Builder
	if a.runningSummary != "" {
		fmt.Fprintf(&b, "Previous summary:\n%s\n\n", a.runningSummary)
	}
	for _, h := range seg {
		fmt.Fprintf(&b, "%s: %s\n", h.msg.Role, h.msg.Content)
	}
	prompt := []llm.Message{
		{Role: "system", Content: "You are a conversation summarizer. " + summaryPrompt},
		{Role: "user", Content: b.String()},
	}
	resp, err := a.summ.ChatStream(ctx, prompt, nil, func(string) {})
	if err != nil {
		return fmt.Errorf("summarize history: %w", err)
	}
	a.runningSummary = strings.TrimSpace(resp.Message.Content)
	a.history = append([]historyEntry{}, a.history[len(seg):]...)

	if a.cfg.Store != nil && a.cfg.SessionID != "" && len(seg) > 0 {
		until := seg[len(seg)-1].id
		if err := a.cfg.Store.SetSummary(ctx, a.cfg.SessionID, a.runningSummary, until); err != nil {
			return fmt.Errorf("persist summary: %w", err)
		}
	}
	return nil
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

	// Self-gated tools (skill_manage) run their own approval: writes are
	// staged or applied per the skills gate, never a generic y/n prompt.
	if tool.Risk() != tools.RiskReadOnly {
		if sg, ok := tool.(interface{ SelfGated() bool }); ok && sg.SelfGated() {
			// skills gate inside the tool
		} else {
			allowed, err := a.approver.Approve(ctx, call, tool.Risk())
			if err != nil {
				return fmt.Sprintf("error: approval failed: %v", err)
			}
			if !allowed {
				return "error: user denied this action; find another approach or explain why you cannot proceed"
			}
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

func buildSystemPrompt(workspace string) string {
	return fmt.Sprintf(`You are Yagent, a local-first AI coding agent running in the workspace:

%s

Rules:
- Be concise. Answer in the fewest words that fully address the request.
- Inspect the workspace with tools instead of guessing: use fs_read / grep / glob to read code, index_search for semantic code search, git_status / git_diff / git_log for git state.
- All tool arguments must be valid JSON matching the tool schema; paths are relative to the workspace root.
- To use a tool, emit the tool call now. Never just describe a tool call you intend to make; if your turn ends without a tool call, that text is treated as your final answer.
- If a tool returns an error, read it, fix your arguments, and retry — do not repeat the same failing call.
- Never claim you ran a tool you did not run, and never invent file contents or command output.
- Side-effecting tools (fs_write, fs_edit, shell_exec) prompt the user for approval. If the user denies, find another approach or explain why you cannot proceed.
- When you have the final answer, reply with plain text and no tool calls.`, workspace)
}

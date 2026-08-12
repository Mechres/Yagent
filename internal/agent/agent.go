// Package agent implements the agent loop: linear, explicit and defensive by
// design (see docs/design/agent-loop.md). It drives the LLM, dispatches tool
// calls through the registry with validation/approval/truncation, persists
// messages to the session store, budgets context with a running summary, and
// injects recalled semantic memories.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
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

Writes are checked by the safety scanner and apply immediately (they may be staged for review if the skills approval gate is enabled). Use only skills_list, skill_view or skill_manage in this turn.`

// historyEntry pairs a persisted message with its store row id (0 when not
// persisted), so the budget manager knows which messages a summary covers.
// tokens caches the message's token estimate (accurate via the server
// tokenizer when a Counter is configured, else the len/4 heuristic).
type historyEntry struct {
	id     int64
	msg    llm.Message
	tokens int
}

// TokenCounter estimates the token count of a text with the model server's own
// tokenizer. *llm.Client implements it (C1: llama.cpp /tokenize, Ollama
// /api/tokenize); a nil counter falls back to the len/4 heuristic.
type TokenCounter interface {
	CountTokens(ctx context.Context, text string) (int, error)
}

// traceSection is one context segment of an assembled request: its name, the
// full content (used only for the --trace dump; never retained), and its token
// estimate. The estimates are the same numbers the context gauge reports, so a
// trace's segments always sum to ContextUsage.
type traceSection struct {
	Name    string
	Content string
	Tokens  int
}

// Config holds loop knobs.
type Config struct {
	MaxIterations int
	// OnToken is called with each content delta as it streams from the model
	// (used by the UI to print tokens live). Optional.
	OnToken func(string)
	// OnReasoning is called with each thinking delta (reasoning_content) as it
	// streams. Display-only; reasoning never enters history or context.
	// Optional.
	OnReasoning func(string)
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

	// Store/SessionID enable L2 persistence; Vectors enables L3 recall;
	// ProjectVectors is a repo-shared memory store also consulted for recall.
	Store          *memory.Store
	SessionID      string
	Vectors        *memory.VectorStore
	ProjectVectors *memory.VectorStore

	// Skills enables the L0 skills index and the autonomous skill-creation
	// opportunity (M3.5). May be nil.
	Skills *skills.Store

	// Index enables per-turn code retrieval (M4); set IndexAutoInject to
	// inject the top matching chunks into context each turn.
	Index           *index.Store
	IndexAutoInject bool

	// Counter estimates token counts with the server tokenizer (C1). When nil
	// the len/4 heuristic is used everywhere.
	Counter TokenCounter

	// Trace, when set, receives a per-section dump of every assembled context
	// with token estimates (B2, `yagent chat --trace <file>`). The segments sum
	// to ContextUsage.
	Trace io.Writer

	// InitialHistory/InitialSummary seed a resumed session (chat --continue).
	InitialHistory []llm.Message
	InitialSummary string
}

// Approval is an approver's verdict. Args, when non-nil, overrides the tool
// arguments the agent will execute — used by the TUI's fs_patch hunk reviewer
// to apply only the accepted hunks.
type Approval struct {
	OK   bool
	Args json.RawMessage
}

// Approver gates Write/Destructive tool calls; implemented by the UI.
type Approver interface {
	Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (Approval, error)
}

// ChatLLM is the model client the loop needs. *llm.Client satisfies it.
type ChatLLM interface {
	ChatStream(ctx context.Context, messages []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error)
}

// Agent runs one conversation against a model, dispatching tools. The history
// is protected by mu because the TUI reads it (status line, /clear, /skill-name)
// while a turn runs in a separate goroutine.
type Agent struct {
	mu sync.RWMutex

	cfg      Config
	llm      ChatLLM
	summ     ChatLLM
	registry *tools.Registry
	approver Approver

	// lastCallSig dedups identical consecutive tool calls (small-model habit).
	// Guarded by dedupMu because read-only batches dispatch concurrently.
	lastCallSig string
	dedupMu     sync.Mutex

	workspace      string
	systemPrompt   string
	history        []historyEntry
	runningSummary string
	injected       []string
	totalToolCalls int

	// Cached accurate token estimates (C1). sysTokens/summaryTokens/
	// injectedTokens are counted at the point each piece is set; lastCtx holds
	// the token-only section summary of the most recent assembled context, so
	// the gauge and budget math need no network calls under the lock.
	sysTokens      int
	summaryTokens  int
	injectedTokens []int
	lastCtx        []traceSection
	traceSeq       int
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
	if cfg.OnReasoning == nil {
		cfg.OnReasoning = func(string) {}
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
	a.sysTokens = a.tokensFor(shortCtx(), a.systemPrompt)
	a.summaryTokens = a.tokensFor(shortCtx(), cfg.InitialSummary)
	for _, m := range cfg.InitialHistory {
		a.history = append(a.history, historyEntry{msg: m, tokens: len(m.Content) / 4})
	}
	return a
}

// shortCtx bounds tokenizer probe/startup calls (New, InjectSystem) so a
// dead or slow server can never hang session startup.
func shortCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cancel // the agent loop runs past this; only the call is bounded
	return ctx
}

// Reset clears conversation history and the running summary (/clear).
func (a *Agent) Reset() {
	a.mu.Lock()
	a.history = nil
	a.runningSummary = ""
	a.summaryTokens = 0
	a.injected = nil
	a.injectedTokens = nil
	a.lastCtx = nil
	a.mu.Unlock()
}

// LoadSession replaces the conversation context with another session's history
// and running summary (used by the TUI session browser to resume/fork in-place).
func (a *Agent) LoadSession(history []llm.Message, summary string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = nil
	for _, m := range history {
		a.history = append(a.history, historyEntry{msg: m, tokens: len(m.Content) / 4})
	}
	a.runningSummary = summary
	a.summaryTokens = len(summary) / 4
	a.injectedTokens = nil
	a.lastCtx = nil
	a.totalToolCalls = 0
}

// SetSessionID switches the session that new messages persist to.
func (a *Agent) SetSessionID(id string) {
	a.mu.Lock()
	a.cfg.SessionID = id
	a.mu.Unlock()
}

// History exposes the conversation for inspection/tests.
func (a *Agent) History() []llm.Message {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]llm.Message, 0, len(a.history))
	for _, h := range a.history {
		out = append(out, h.msg)
	}
	return out
}

// ContextUsage reports the current heuristic token estimate and the configured
// window (limit). Used by the UI status line; safe to call while a turn runs.
func (a *Agent) ContextUsage() (used, limit int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.estTokensLocked(), a.cfg.Window
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
	used := make(map[string]bool)
	a.lastCallSig = ""

	for i := 0; i < a.cfg.MaxIterations; i++ {
		// Budget before every request: plain chat turns (no tool calls)
		// would otherwise never trigger summarization.
		if err := a.budget(ctx); err != nil {
			return "", err
		}
		resp, err := a.llm.ChatStream(ctx, a.assembleContext(recall, code), a.activeToolSchemas(input, used), a.cfg.OnToken, a.cfg.OnReasoning)
		if err != nil {
			return "", err
		}
		if _, err := a.appendMessage(ctx, resp.Message); err != nil {
			return "", err
		}

		if len(resp.ToolCalls) == 0 {
			a.totalToolCalls += turnCalls
			_ = a.maybeOfferSkillCreation(ctx, turnCalls) // best-effort
			slog.Debug("final answer", "tokens", len(resp.Message.Content)/4)
			return resp.Message.Content, nil // final answer, done
		}
		turnCalls += len(resp.ToolCalls)
		for _, tc := range resp.ToolCalls {
			used[tc.Function.Name] = true
		}
		slog.Debug("model requested tools", "n", len(resp.ToolCalls), "iteration", i)

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
	tokens := a.tokensFor(shortCtx(), content)
	a.mu.Lock()
	a.injected = append(a.injected, content)
	a.injectedTokens = append(a.injectedTokens, tokens)
	a.mu.Unlock()
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
	resp, err := a.llm.ChatStream(ctx, msgs, a.registry.SchemasFor(skillToolNames), func(string) {}, nil)
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

// verifySkillPrompt drives the verification harness: the staged skill's
// SKILL.md is injected into context and the model must execute its
// "## Verification" section with the workspace tools, then report a verdict.
const verifySkillPrompt = `You are verifying a staged skill (procedural memory). The skill's SKILL.md is loaded in your context.

Execute its "## Verification" section exactly, using the available tools, in this workspace. To inspect anything, call a tool NOW — never just describe what you would run. Base your verdict on actual tool output. If the Verification section cannot be run (missing files, unknown commands, contradictions), report FAIL.

End your reply with a single verdict line:
PASS <one-line reason>
or
FAIL <one-line reason>`

// VerifySkill runs one verification pass for a staged skill and returns the
// model's final answer. It uses a fresh, self-contained agent (no session
// store), so the verification does not pollute the active conversation.
func VerifySkill(ctx context.Context, client ChatLLM, reg *tools.Registry, approver Approver, skillContent, workspace string) (string, error) {
	a := New(client, reg, approver, Config{MaxIterations: 12, Window: 8000, Reserve: 1000}, workspace)
	a.InjectSystem("Staged skill to verify (SKILL.md):\n\n" + skillContent)
	return a.Run(ctx, verifySkillPrompt)
}

// subagentSystemPrompt scopes a child agent to read-only investigation; %s is
// replaced with the tools actually available to it (possibly a subagent tool
// subset, M7 beyond v2).
const subagentSystemPrompt = `You are a subagent of Yagent working on a delegated subtask in a workspace. You have read-only tools (%s). Investigate, gather evidence (paths, lines, URLs), and finish with a concise summary or conclusion. Never modify files.`

// RunSubagent executes a self-contained subtask in an isolated read-only agent
// and returns its final message plus a heuristic token count. The caller
// supplies a read-only registry; the child runs with its own context window,
// so long investigations do not pollute the parent conversation (M7 v1).
func RunSubagent(ctx context.Context, client ChatLLM, reg *tools.Registry, task, workspace string) (string, int, error) {
	var tokens int
	a := New(client, reg, nil, Config{
		MaxIterations: 15, Window: 8000, Reserve: 1000,
		OnToken: func(d string) { tokens += len(d) / 4 },
	}, workspace)
	a.InjectSystem(fmt.Sprintf(subagentSystemPrompt, strings.Join(reg.Names(), ", ")))
	answer, err := a.Run(ctx, task)
	return answer, tokens, err
}

// ParseVerdict extracts PASS/FAIL from a verification answer.
func ParseVerdict(answer string) string {
	for _, line := range strings.Split(answer, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "PASS") {
			return "PASS"
		}
		if strings.HasPrefix(t, "FAIL") {
			return "FAIL"
		}
	}
	return ""
}

// DefaultGoalRounds caps an autonomous goal loop.
const DefaultGoalRounds = 8

// goalDonePrompt asks the model whether the goal is fully achieved; the reply
// is parsed for a DONE/CONTINUE verdict.
const goalDonePrompt = `You set out to fully achieve this goal: %s

Have you achieved it completely? Reply with exactly one line, nothing else:
DONE <what you verified>
or
CONTINUE <what still remains>`

// RunGoal drives the agent autonomously toward a goal: each round runs the
// normal loop with the goal as the instruction, then a verification pass asks
// for a DONE/CONTINUE verdict. History persists across rounds, so each round
// builds on the last (the budget condenses old rounds). onRound, when set, is
// called with the 1-based round and its final answer. The verdict is model
// self-reported; maxRounds caps the loop.
func (a *Agent) RunGoal(ctx context.Context, goal string, maxRounds int, onRound func(round int, answer string)) (string, error) {
	if maxRounds <= 0 {
		maxRounds = DefaultGoalRounds
	}
	var last string
	for round := 1; round <= maxRounds; round++ {
		var err error
		last, err = a.Run(ctx, goal)
		if err != nil {
			return last, fmt.Errorf("round %d: %w", round, err)
		}
		if onRound != nil {
			onRound(round, last)
		}
		done, err := a.goalDone(ctx, goal)
		if err != nil {
			return last, nil // can't verify (server hiccup); stop cleanly
		}
		if done {
			return last, nil
		}
	}
	return last, fmt.Errorf("goal not achieved after %d rounds", maxRounds)
}

// goalDone asks the model for a DONE/CONTINUE verdict without touching history
// (the exchange is not persisted).
func (a *Agent) goalDone(ctx context.Context, goal string) (bool, error) {
	prompt := llm.Message{Role: "user", Content: fmt.Sprintf(goalDonePrompt, goal)}
	msgs := append(a.assembleContext("", ""), prompt)
	resp, err := a.llm.ChatStream(ctx, msgs, nil, func(string) {}, nil)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(resp.Message.Content), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "DONE") {
			return true, nil
		}
		if strings.HasPrefix(t, "CONTINUE") {
			return false, nil
		}
	}
	return false, nil // no verdict: assume not done
}

// appendMessage records a message in memory and persists it when a store is
// configured, returning its store row id (0 when not persisted).
func (a *Agent) appendMessage(ctx context.Context, msg llm.Message) (int64, error) {
	var id int64
	if a.cfg.Store != nil && a.cfg.SessionID != "" {
		var err error
		id, err = a.cfg.Store.Append(ctx, a.cfg.SessionID, msg)
		if err != nil {
			return 0, fmt.Errorf("persist message: %w", err)
		}
	}
	tokens := a.tokensFor(ctx, messageTokenText(msg))
	a.mu.Lock()
	a.history = append(a.history, historyEntry{id: id, msg: msg, tokens: tokens})
	a.mu.Unlock()
	return id, nil
}

// messageTokenText approximates the text a message contributes to the prompt
// (content plus any tool-call names/arguments), for token counting.
func messageTokenText(m llm.Message) string {
	var b strings.Builder
	b.WriteString(m.Content)
	for _, tc := range m.ToolCalls {
		b.WriteString(tc.Function.Name)
		b.WriteString(string(tc.Function.Arguments))
	}
	return b.String()
}

// recall injects the top-k semantic memories for the user input (L3),
// budgeted and deduplicated against the current session. Personal and project
// stores are both consulted and merged.
func (a *Agent) recall(ctx context.Context, input string) string {
	if a.cfg.Vectors == nil && a.cfg.ProjectVectors == nil {
		return ""
	}
	var memories []memory.Memory
	for _, vs := range []*memory.VectorStore{a.cfg.Vectors, a.cfg.ProjectVectors} {
		if vs == nil {
			continue
		}
		ms, err := vs.Search(ctx, input, DefaultRecallK)
		if err != nil {
			continue // recall is best-effort; a broken store must not kill the turn
		}
		memories = append(memories, ms...)
	}
	var b strings.Builder
	for _, m := range memories {
		if m.SessionID != "" && m.SessionID == a.cfg.SessionID {
			continue // dedupe against this session's own messages
		}
		fmt.Fprintf(&b, "- user fact: %s\n", m.Text)
	}
	if b.Len() == 0 {
		return ""
	}
	// Cap at the memory budget (heuristic tokens = chars/4).
	maxChars := a.cfg.MemoryMaxTokens * 4
	header := "Relevant memories from past sessions — attribute these to the USER, never to yourself:\n"
	if b.Len() > maxChars {
		return header + b.String()[:maxChars] + "\n…"
	}
	return header + b.String()
}

// Tool groups for dynamic schema filtering (docs/design/agent-loop.md).
// The core set is always offered; the web/index/skill_manage schemas are added
// only when the input signals that domain or the model already used them this
// turn. Filtering only shrinks what the model *sees* — the registry still
// holds every tool, so a tool the model calls anyway still works.
var (
	coreToolNames = []string{
		"fs_read", "fs_write", "fs_edit", "glob", "grep", "shell_exec",
		"git_status", "git_diff", "git_log", "memory_save", "memory_search",
		"skills_list", "skill_view", "consult",
	}
	webToolNames    = []string{"web_search", "web_fetch"}
	indexToolNames  = []string{"index_search", "index_repo"}
	skillManageName = []string{"skill_manage"}
)

// activeToolSchemas returns the tool schemas to offer for the next request:
// the core set plus domain tools the input signals or the model already used
// this turn.
func (a *Agent) activeToolSchemas(input string, used map[string]bool) []llm.ToolSchema {
	names := coreToolNames
	if used["web_search"] || used["web_fetch"] || researchSignal(input) {
		names = append(names, webToolNames...)
	}
	if used["index_search"] || used["index_repo"] || codeSignal(input) {
		names = append(names, indexToolNames...)
	}
	if used["skill_manage"] || strings.Contains(strings.ToLower(input), "skill") {
		names = append(names, skillManageName...)
	}
	return a.registry.SchemasFor(names)
}

func researchSignal(s string) bool {
	l := strings.ToLower(s)
	for _, kw := range []string{
		"web", "internet", "online", "search", "latest", "news", "research",
		"find out", "look up", "how do i", "how to", "url", "http",
		"weather", "price", "compare", "supported", "install", "tutorial", "guide",
	} {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

func codeSignal(s string) bool {
	l := strings.ToLower(s)
	for _, kw := range []string{
		"code", "function", "repo", "source", "implement", "bug", "fix",
		"where is", "where does", "find", "index", "file", "method",
	} {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
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
	var sections []traceSection

	// system prompt
	sections = append(sections, traceSection{Name: "system", Content: a.systemPrompt, Tokens: a.sysTokens})
	sys.WriteString(a.systemPrompt)

	// skills L0 index
	if l0 := a.skillIndex(); l0 != "" {
		sections = append(sections, traceSection{Name: "skills L0", Content: l0, Tokens: len(l0) / 4})
		sys.WriteString("\n\n" + l0)
	}

	// code retrieval
	if code != "" {
		sections = append(sections, traceSection{Name: "code index", Content: code, Tokens: len(code) / 4})
		sys.WriteString("\n\n" + code)
	}

	a.mu.RLock()
	runningSummary := a.runningSummary
	injected := append([]string(nil), a.injected...)
	injectedTokens := append([]int(nil), a.injectedTokens...)
	hist := make([]llm.Message, 0, len(a.history))
	histTokens := 0
	for _, h := range a.history {
		hist = append(hist, h.msg)
		histTokens += h.tokens
	}
	a.mu.RUnlock()

	// running summary
	if runningSummary != "" {
		sections = append(sections, traceSection{Name: "summary", Content: runningSummary, Tokens: a.summaryTokens})
		sys.WriteString("\n\nSummary of the earlier part of this conversation:\n" + runningSummary)
	}

	// recall
	if recall != "" {
		sections = append(sections, traceSection{Name: "recall", Content: recall, Tokens: len(recall) / 4})
		sys.WriteString("\n\n" + recall)
	}

	// injected skill/system chunks
	for i, chunk := range injected {
		tk := len(chunk) / 4
		if i < len(injectedTokens) {
			tk = injectedTokens[i]
		}
		sections = append(sections, traceSection{Name: "injected", Content: chunk, Tokens: tk})
		sys.WriteString("\n\n" + chunk)
	}

	// history
	histDump := ""
	if a.cfg.Trace != nil {
		histDump = renderHistoryDump(hist)
	}
	sections = append(sections, traceSection{Name: "history", Content: histDump, Tokens: histTokens})

	// Cache the token-only section summary for the context gauge/budget (no
	// content is retained, so this does not duplicate the whole context), and
	// dump the full trace when configured.
	tokenOnly := make([]traceSection, len(sections))
	for i, s := range sections {
		tokenOnly[i] = traceSection{Name: s.Name, Tokens: s.Tokens}
	}
	a.mu.Lock()
	a.lastCtx = tokenOnly
	a.mu.Unlock()
	if a.cfg.Trace != nil {
		a.writeTrace(sections)
	}

	msgs := []llm.Message{{Role: "system", Content: sys.String()}}
	msgs = append(msgs, hist...)
	return msgs
}

// renderHistoryDump renders the history messages as text for the --trace dump.
func renderHistoryDump(hist []llm.Message) string {
	var b strings.Builder
	for _, m := range hist {
		body := m.Content
		if body == "" && len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			body = "(tool calls: " + strings.Join(names, ", ") + ")"
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, body)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeTrace dumps one assembled context with per-section token estimates. The
// section totals are exactly what ContextUsage reports, so a trace's segments
// always sum to the gauge.
func (a *Agent) writeTrace(sections []traceSection) {
	a.traceSeq++
	w := a.cfg.Trace
	fmt.Fprintf(w, "\n===== context #%d =====\n", a.traceSeq)
	total := 0
	for _, s := range sections {
		total += s.Tokens
		fmt.Fprintf(w, "  %-14s %7d tok %9d ch\n", s.Name, s.Tokens, len(s.Content))
	}
	fmt.Fprintf(w, "  %-14s %7d tok\n", "total", total)
	for _, s := range sections {
		if s.Content == "" {
			continue
		}
		fmt.Fprintf(w, "\n--- %s ---\n%s\n", s.Name, s.Content)
	}
	fmt.Fprintln(w)
}

// codeIndex retrieves the top chunks matching the user input from the codebase
// index (M4) and formats them as path:start-end blocks, budget-capped. When the
// block would overflow, chunk bodies are collapsed to their signature headers
// (// ...) so more relevant declarations fit in context.
func (a *Agent) codeIndex(ctx context.Context, input string) string {
	if a.cfg.Index == nil || !a.cfg.IndexAutoInject || a.cfg.Index.Count() == 0 {
		return ""
	}
	results, err := a.cfg.Index.Search(ctx, input, maxIndexChunks)
	if err != nil || len(results) == 0 {
		return ""
	}
	block := renderIndexBlock(results, false)
	if len(block) > maxIndexTokens*4 {
		block = renderIndexBlock(results, true) // collapse bodies
	}
	if len(block) > maxIndexTokens*4 {
		return block[:maxIndexTokens*4] + "\n…"
	}
	return block
}

func renderIndexBlock(results []index.Result, compact bool) string {
	var b strings.Builder
	b.WriteString("Relevant code from the workspace index (path:start-end):\n")
	for _, r := range results {
		fmt.Fprintf(&b, "- %s:%d-%d\n", r.Path, r.StartLine, r.EndLine)
		snippet := r.Content
		if compact {
			snippet = compactChunk(snippet)
		} else if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		b.WriteString("  " + strings.ReplaceAll(snippet, "\n", "\n  ") + "\n")
	}
	return b.String()
}

// compactChunk keeps a declaration's header (through its opening brace, or the
// signature lines for brace-less languages) and collapses the body to // ....
func compactChunk(content string) string {
	lines := strings.Split(content, "\n")
	brace := -1
	for i, l := range lines {
		if strings.Contains(l, "{") {
			brace = i
			break
		}
	}
	keep := 0
	if brace >= 0 {
		keep = brace + 1
	} else {
		// brace-less languages: keep decorators / "def f(a):" / "func f()" /
		// "class X" header lines, stop at the first body line.
		for i, l := range lines {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "@") || strings.HasSuffix(l, ":") ||
				strings.HasPrefix(t, "def ") || strings.HasPrefix(t, "class ") || strings.HasPrefix(t, "func ") {
				keep = i + 1
				continue
			}
			break
		}
		if keep == 0 {
			keep = 1
		}
	}
	out := append([]string{}, lines[:keep]...)
	if keep < len(lines) {
		out = append(out, "// ...")
	}
	return strings.Join(out, "\n")
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
		stale := ""
		if m.Failures >= skills.MaxSkillFailures {
			stale = " (stale — verification failed repeatedly)"
		}
		if m.Category != "" {
			fmt.Fprintf(&b, "- %s [%s, %s]%s: %s\n", m.Name, m.Category, m.Source, stale, m.Description)
		} else {
			fmt.Fprintf(&b, "- %s [%s]%s: %s\n", m.Name, m.Source, stale, m.Description)
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
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.estTokensLocked()
}

// estTokensLocked is estTokens with the history lock already held. It sums the
// non-history sections of the most recently assembled context (accurate via the
// server tokenizer when a Counter is configured; the cached per-piece counts
// before anything has been assembled) plus the LIVE history token totals, so
// the gauge and the budget always see the current context — including messages
// appended since the last assembly.
func (a *Agent) estTokensLocked() int {
	total := 0
	if len(a.lastCtx) > 0 {
		for _, s := range a.lastCtx {
			if s.Name == "history" {
				continue // history is counted live below
			}
			total += s.Tokens
		}
	} else {
		total = a.sysTokens + a.summaryTokens
		if a.cfg.Skills != nil {
			total += maxL0Tokens // L0 skills index is always in context
		}
		if a.cfg.Index != nil && a.cfg.IndexAutoInject {
			total += maxIndexTokens // per-turn code retrieval
		}
		for _, t := range a.injectedTokens {
			total += t
		}
	}
	for _, h := range a.history {
		total += h.tokens
	}
	return total
}

// tokensFor estimates the token count of text: accurate via the configured
// server tokenizer (C1) when available, else the len/4 heuristic.
func (a *Agent) tokensFor(ctx context.Context, text string) int {
	if text == "" {
		return 0
	}
	if a.cfg.Counter != nil {
		if n, err := a.cfg.Counter.CountTokens(ctx, text); err == nil && n >= 0 {
			return n
		}
	}
	return len(text) / 4
}

// budget summarizes the oldest half of history when the window is exceeded,
// so context never grows unbounded (memory.md L1). The summarized segment is
// replaced by the running summary, and the store records what is covered.
func (a *Agent) budget(ctx context.Context) error {
	limit := a.cfg.Window - a.cfg.Reserve
	if limit < a.cfg.Window/2 {
		limit = a.cfg.Window / 2 // keep the limit sane for tiny windows
	}
	a.mu.RLock()
	over := a.estTokensLocked() > limit
	a.mu.RUnlock()
	if !over {
		return nil
	}
	// Never summarize the current user turn (or anything after it): the
	// Qwythos chat template rejects a request whose message list has no plain
	// user query, so the running summary must only cover messages that precede
	// the last user message. Otherwise a long tool-loop turn would leave a
	// history that starts mid-exchange and the server 400s.
	a.mu.Lock()
	cutoff := len(a.history)
	for i := len(a.history) - 1; i >= 0; i-- {
		if a.history[i].msg.Role == "user" {
			cutoff = i
			break
		}
	}
	if cutoff == 0 {
		a.mu.Unlock()
		return nil
	}
	// summarize the oldest half of the messages before the current user turn
	seg := a.history[:cutoff/2]
	if len(seg) == 0 {
		a.mu.Unlock()
		return nil
	}
	segCopy := append([]historyEntry(nil), seg...)
	previousSummary := a.runningSummary
	a.mu.Unlock()

	var b strings.Builder
	if previousSummary != "" {
		fmt.Fprintf(&b, "Previous summary:\n%s\n\n", previousSummary)
	}
	for _, h := range segCopy {
		fmt.Fprintf(&b, "%s: %s\n", h.msg.Role, h.msg.Content)
	}
	prompt := []llm.Message{
		{Role: "system", Content: "You are a conversation summarizer. " + summaryPrompt},
		{Role: "user", Content: b.String()},
	}
	resp, err := a.summ.ChatStream(ctx, prompt, nil, func(string) {}, nil)
	if err != nil {
		return fmt.Errorf("summarize history: %w", err)
	}
	summary := strings.TrimSpace(resp.Message.Content)

	summaryTokens := a.tokensFor(ctx, summary)
	a.mu.Lock()
	a.runningSummary = summary
	a.summaryTokens = summaryTokens
	if len(a.history) >= len(segCopy) {
		a.history = append([]historyEntry{}, a.history[len(segCopy):]...)
	}
	a.mu.Unlock()
	slog.Info("summarized history", "covered_messages", len(segCopy), "summary_len", len(summary))

	if a.cfg.Store != nil && a.cfg.SessionID != "" && len(segCopy) > 0 {
		until := segCopy[len(segCopy)-1].id
		if err := a.cfg.Store.SetSummary(ctx, a.cfg.SessionID, summary, until); err != nil {
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

	// Dedup: small models occasionally repeat an identical tool call back to
	// back; skip it instead of running the same side effect twice. Only
	// successful calls arm the dedup — repeated *failing* calls still count
	// toward the validation block.
	sig := name + " " + string(call.Function.Arguments)
	a.dedupMu.Lock()
	dup := a.lastCallSig != "" && sig == a.lastCallSig
	a.dedupMu.Unlock()
	if dup {
		return "skipped: duplicate of the previous tool call (same tool and arguments); do not repeat it"
	}
	armed := false
	defer func() {
		if armed {
			a.dedupMu.Lock()
			a.lastCallSig = sig
			a.dedupMu.Unlock()
		}
	}()

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
			appr, err := a.approver.Approve(ctx, call, tool.Risk())
			if err != nil {
				return fmt.Sprintf("error: approval failed: %v", err)
			}
			if !appr.OK {
				return "error: user denied this action; find another approach or explain why you cannot proceed"
			}
			if appr.Args != nil {
				// e.g. the hunk reviewer filtered the patch down to accepted
				// hunks; execute with the rewritten arguments.
				call.Function.Arguments = appr.Args
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
		slog.Debug("tool validation error", "tool", name, "error", err)
		if valFails[name] >= maxValidationFails {
			blocked[name] = true
			return fmt.Sprintf("error: tool %q failed validation %d times; last error: %v. Do not call %q again this turn.",
				name, valFails[name], err, name)
		}
		return "error: " + err.Error()
	}
	slog.Debug("tool executed", "tool", name)
	armed = true // a successful call arms the dedup for an identical repeat
	return result
}

func buildSystemPrompt(workspace string) string {
	return fmt.Sprintf(`You are Yagent, a local-first AI coding agent running in the workspace:

%s

Rules:
- Be concise. Answer in the fewest words that fully address the request.
- Inspect the workspace with tools instead of guessing: use fs_read / grep / glob to read code, index_search for semantic code search, git_status / git_diff / git_log for git state.
- Identity: you are the assistant. The user is the human you are talking to. When asked about the user's own identity (their name, preferences), refer to them as "your name"/"the user's name" — never "my name". If you don't know the user's name, say so rather than guessing.
- Never self-identify: do not mention a model name, version, or creator (never say "I am <model>" or "I was created by <company>"). You are just "the assistant"; if asked who you are, say you are Yagent, a local coding agent.
- All tool arguments must be valid JSON matching the tool schema; paths may be relative to the workspace root or absolute paths inside the workspace.
- To use a tool, emit the tool call now. Do not narrate your plan or describe a tool call you intend to make; if your turn ends without a tool call, that text is treated as your final answer.
- If a tool returns an error, read it, fix your arguments, and retry — do not repeat the same failing call.
- Never claim you ran a tool you did not run, and never invent file contents or command output.
- Side-effecting tools (fs_write, fs_edit, shell_exec) prompt the user for approval. If the user denies, find another approach or explain why you cannot proceed.
- When you answer from web_search / web_fetch results, cite the source URLs.
- When stuck, unsure, or before a risky change, you may use the consult tool to ask a second AI advisor model for a second opinion.
- When you have the final answer, reply with plain text and no tool calls.`, workspace)
}

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/checkpoint"
	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/gitops"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
)

// isTerminal reports whether f is a character device (a real terminal).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// RunTUI drives the bubbletea interface: streaming answers, a scrollable
// transcript, tool lines, approval prompts and a status line.
func RunTUI(ctx context.Context, client *llm.Client, cfg *config.Config, continueID string, opts Options) error {
	env, err := newChatEnv(ctx, cfg, continueID, opts.Fork)
	if err != nil {
		return err
	}
	defer env.st.Close()
	defer env.vs.Close()
	defer env.projVS.Close()
	defer env.idx.Close()
	defer env.jobs.StopAll()

	incoming := make(chan tea.Msg, 4096)
	inputCh := make(chan turnRequest, 1)
	runnerCtx, runnerCancel := context.WithCancel(ctx)
	runnerDone := make(chan struct{})
	ap := newToggleableApprover(newRememberingApprover(&tuiApprover{incoming: incoming, ctx: runnerCtx}))
	ap.SetYOLO(opts.YOLO)
	if opts.YOLO {
		env.registry.SetSkillsWriteApproval(false)
	}

	// Stream/progress messages are best-effort (dropped if the buffer is full)
	// so a synchronous command like /goal can never deadlock the UI. Approval
	// requests and turn completion stay blocking — they carry semantics.
	send := func(msg tea.Msg) {
		select {
		case incoming <- msg:
		default:
		}
	}
	ag := newAgent(client, cfg, env, ap,
		func(delta string) { send(tokenMsg{delta: delta}) },
		func(delta string) { send(reasoningMsg{delta: delta}) },
		func(call llm.ToolCall) { send(toolMsg{call: call}) },
		opts.Trace)
	m := tuiModel{
		cfg: cfg, env: env, ag: ag, client: client,
		incoming: incoming, inputCh: inputCh,
		runnerCtx: runnerCtx, runnerCancel: runnerCancel, runnerDone: runnerDone,
		msgInput: newInput(), yoloToggler: ap, trace: opts.Trace,
	}
	if env.gitEnabled {
		env.commitDirtyStart()
	}
	// The clarify/plan tools route through the TUI modal: block on the user's
	// answer and hand it back to the agent as tool data.
	env.registry.SetAskUser(func(ctx context.Context, question string, choices []string) (string, error) {
		respond := make(chan string, 1)
		select {
		case incoming <- clarifyRequestMsg{question: question, choices: choices, respond: respond}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		select {
		case ans := <-respond:
			return ans, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	env.registry.SetIndexProgress(func(line string) { send(progressMsg{text: line}) })
	startBackgroundIndex(runnerCtx, env, func(line string) { send(progressMsg{text: line}) })

	// Agent runner: one turn per input line; on cancel, wraps up the session.
	// Reads the live client/agent through the model so a /model provider switch
	// swaps the runtime without restarting the loop.
	go func() {
		defer close(runnerDone)
		for {
			select {
			case <-runnerCtx.Done():
				wrapCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				ag := m.currentAgent()
				if err := ag.Finish(wrapCtx); err != nil {
					slog.Warn("session-end skill review", "error", err)
				}
				if err := memory.SummarizeSession(wrapCtx, m.currentClient(), env.st, env.vs, env.sessionID); err != nil {
					slog.Warn("session summary", "error", err)
				}
				cancel()
				return
			case req := <-inputCh:
				env.undo.StartTurn()
				answer, err := m.currentAgent().Run(req.ctx, req.text)
				env.undo.EndTurn()
				env.turnSeq++
				if ws, werr := os.Getwd(); werr == nil {
					env.commitTurn(ws, fmt.Sprintf("turn %d", env.turnSeq))
				}
				incoming <- turnDoneMsg{answer: answer, err: err, seq: req.seq}
			}
		}
	}()

	if ws, err := os.Getwd(); err == nil {
		m.workspace = ws
	}
	m.branch = gitBranch(m.workspace)
	m.th = themeByName(cfg.Theme)
	m.spinner = spinner.New(spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(m.th.Primary)))
	m.viewport = viewport.New(80, 20)
	m.viewport.KeyMap.Up.SetEnabled(false) // avoid clashing with the text input
	m.viewport.KeyMap.Down.SetEnabled(false)
	// Mouse starts OFF so drag-selecting transcript text stays with the
	// terminal. Ctrl+M (or /mouse) enables capture to click the thinking block
	// and wheel-scroll; Ctrl+M again hands selection back.
	p := tea.NewProgram(&m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return err
	}
	runnerCancel()
	<-runnerDone
	// Leave the alt-screen, then print the session so the user can resume.
	if env.forkSource != "" {
		fmt.Fprintf(os.Stdout, "\nsession: %s (forked from %s; resume with: yagent chat --continue %s)\n",
			env.sessionID, env.forkSource, env.sessionID)
	} else {
		fmt.Fprintf(os.Stdout, "\nsession: %s (resume with: yagent chat --continue %s)\n", env.sessionID, env.sessionID)
	}
	return m.err
}

// ---------- messages ----------

type tokenMsg struct{ delta string }
type reasoningMsg struct{ delta string }
type toolMsg struct{ call llm.ToolCall }
type progressMsg struct{ text string }

// modelListMsg carries the live model list fetched from a Dynamic provider
// (local llama.cpp/Ollama) or a models.dev-backed cloud provider when the
// /model selector opens.
type modelListMsg struct {
	models []string
	ok     bool
	dev    bool // true = from models.dev (cloud)
}

type turnDoneMsg struct {
	answer string
	err    error
	seq    int // turn sequence; stale messages from a cancelled turn are ignored
}

// currentClient returns the live LLM client (safe during a running turn).
func (m *tuiModel) currentClient() *llm.Client {
	m.runtimeMu.RLock()
	defer m.runtimeMu.RUnlock()
	return m.client
}

// currentAgent returns the live agent (safe during a running turn).
func (m *tuiModel) currentAgent() *agent.Agent {
	m.runtimeMu.RLock()
	defer m.runtimeMu.RUnlock()
	return m.ag
}

// swapRuntime replaces the client+agent after a provider/model switch. Only
// called when no turn is running (the /model command refuses while busy).
func (m *tuiModel) swapRuntime(client *llm.Client, ag *agent.Agent) {
	m.runtimeMu.Lock()
	m.client = client
	m.ag = ag
	m.runtimeMu.Unlock()
}

// turnRequest carries one submitted turn plus its own context, so the model can
// cancel just this turn (Esc / loop guard) without killing the session.
type turnRequest struct {
	text string
	ctx  context.Context
	seq  int
}
type approvalRequestMsg struct {
	call    llm.ToolCall
	risk    tools.RiskLevel
	respond chan agent.Approval
}

// clarifyRequestMsg asks the user a question (clarify/plan tools): the modal
// renders the question + choices and respond receives the picked answer.
type clarifyRequestMsg struct {
	question string
	choices  []string
	respond  chan string
}

// reasoningCap bounds the per-turn thinking buffer so a verbose model can't
// flood the terminal (the tail is kept, prefixed with an omission marker).
const reasoningCap = 4 << 10

// tuiApprover prompts for approval through the TUI and blocks for the answer.
type tuiApprover struct {
	incoming chan tea.Msg
	ctx      context.Context
}

func (a *tuiApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	notifyOS("yagent — approval needed", fmt.Sprintf("%s (%s)", call.Function.Name, risk))
	respond := make(chan agent.Approval, 1)
	select {
	case a.incoming <- approvalRequestMsg{call: call, risk: risk, respond: respond}:
	case <-ctx.Done():
		return agent.Approval{}, ctx.Err()
	}
	select {
	case appr := <-respond:
		return appr, nil
	case <-ctx.Done():
		return agent.Approval{}, ctx.Err()
	}
}

// ---------- model ----------

type tuiModel struct {
	cfg         *config.Config
	env         *chatEnv
	ag          *agent.Agent
	client      *llm.Client
	workspace   string
	yoloToggler *toggleableApprover
	trace       io.Writer

	// runtimeMu guards client/ag so a provider switch (/model) can swap them
	// live without racing the running turn's reader.
	runtimeMu sync.RWMutex

	incoming     chan tea.Msg
	inputCh      chan turnRequest
	runnerCtx    context.Context
	runnerCancel context.CancelFunc
	runnerDone   chan struct{}

	// Per-turn cancellation: turnCancel aborts the running turn (Esc, or the
	// loop guard); turnCancelled/cancelReason describe why it stopped.
	turnCancel    context.CancelFunc
	turnCancelled bool
	cancelReason  string
	turnSeq       int

	// Loop-guard self-heal: when a repetition loop is auto-cancelled, the same
	// input is retried once with repetition_penalty applied.
	lastTurnText string
	retriedLoop  bool

	msgInput textarea.Model
	viewport viewport.Model
	spinner  spinner.Model

	width  int
	height int
	branch string
	th     Theme

	transcript         []string
	stream             strings.Builder
	reasoning          string
	reasoningTruncated bool
	busy               bool
	turnTokens         int
	turnStart          time.Time
	toolCalls          int
	pending            chan agent.Approval
	approveArg         string
	confirmQuit        bool
	follow             bool // follow the bottom of the transcript
	tabIndex           int
	lastInput          string
	err                error
	mouseOn            bool // mouse capture enabled (click thinking / wheel scroll)

	// fs_patch per-hunk review state (see handleHunkKey).
	hunkHunks   []tools.PatchHunk
	hunkIdx     int
	hunkKeep    []bool
	hunkRespond chan agent.Approval
	hunkOpen    bool
	hunkSummary string
	hunkPatch   string

	// Expandable thinking: the last committed thinking block can be toggled
	// between a collapsed header and the full dimmed text with the 't' key.
	thinkingOpen     bool   // a committed block exists and can be toggled
	thinkingExpanded bool   // persists across turns (user preference)
	thinkingText     string // styled full text of the last committed block
	thinkingHeader   string // collapsed header line for that block
	lastThinkIdx     int    // transcript index where the block starts
	thinkingLines    int    // lines the block occupies in the transcript

	settingsOpen bool
	settingsIdx  int
	editing      bool
	editInput    textinput.Model
	choosing     bool
	choosingIdx  int

	// /model provider selector: two panes (provider | model), index tracked.
	modelOpen     bool
	modelProvider int  // index into config.Providers
	modelModelIdx int  // index into the selected provider's Models
	modelOnModels bool // true = the model pane is active
	modelConfirm  bool
	modelLive     []string // live models from a Dynamic provider (local)
	modelLoading  bool     // a live fetch is in flight
	// modelKeyEntry is true when the confirm step is asking for an API key
	// (the selected cloud provider has none configured).
	modelKeyEntry bool
	modelKeyInput textinput.Model

	// /diff review modal (cumulative session diff vs baseline).
	diffOpen   bool
	diffText   string
	diffScroll int
	diffStat   string

	sessionsOpen    bool
	sessionsIdx     int
	sessionsConfirm bool
	sessionsAction  string
	sessions        []memory.SessionSummary

	// Skills manager modal (P6): pending skill writes with diff/verify/
	// approve/reject actions.
	skillsOpen bool
	skillsIdx  int
	skills     []skills.PendingSummary
	skillsMsg  string
	skillsCmd  *skillsHandler

	// Clarify/plan modal (Hermes review #1/#4): a question with choices (or
	// free text) that returns the user's answer to the agent as tool data.
	clarifyOpen        bool
	clarifyQuestion    string
	clarifyChoices     []string
	clarifyIdx         int
	clarifyFree        bool
	clarifyRespond     chan string
	clarifyAnswerInput textinput.Model

	// In-viewport transcript search (Ctrl+F): findOpen captures keys into
	// findQuery; findMatches are byte offsets into the joined transcript and
	// findMatch is the current one (jumped to in the viewport).
	findOpen    bool
	findQuery   string
	findMatches []int
	findMatch   int

	// Command and prompt history (Up/Down arrow navigation).
	history    []string
	historyIdx int
	draftInput string

	// Help modal (? or /help or F1).
	helpOpen bool

	// Checkpoints modal (/checkpoint or /checkpoints).
	checkpointsOpen    bool
	checkpointsIdx     int
	checkpointsConfirm bool
	checkpointsAction  string
	checkpoints        []checkpointSummary

	// Active workflow indicator (goal or playbook) in header.
	activeWorkflow string
}

type checkpointSummary struct {
	Name    string
	ModTime time.Time
}

func newInput() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "message (enter to send, alt+enter for a newline, ctrl-f to search)"
	ta.CharLimit = 8000
	ta.Prompt = iconCommand + " "
	ta.ShowLineNumbers = false
	// Plain enter submits (handled before the textarea sees it); alt+enter
	// inserts a literal newline. Multi-line pastes wrap instead of overflowing
	// horizontally.
	ta.KeyMap.InsertNewline.SetKeys("alt+enter")
	return ta
}

// gitBranch returns the current git branch name, or "" when not a repo.
func gitBranch(ws string) string {
	out, err := exec.Command("git", "-C", ws, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (m *tuiModel) Init() tea.Cmd {
	m.follow = true
	return tea.Batch(
		m.msgInput.Focus(),
		waitIncoming(m.incoming),
	)
}

// append adds a transcript line and re-renders the viewport, following the
// bottom unless the user has scrolled up.
func (m *tuiModel) append(line string) {
	m.transcript = append(m.transcript, line)
	m.syncViewport()
}

// refreshViewport pushes just the committed transcript into the viewport.
func (m *tuiModel) refreshViewport() {
	m.syncViewport()
}

// openFind enters in-viewport transcript search: further keys are captured into
// the query until esc or Ctrl+F closes it.
func (m *tuiModel) openFind() tea.Model {
	m.findOpen = true
	m.findQuery = ""
	m.findMatches = nil
	m.findMatch = 0
	return m
}

// handleFindKey drives transcript search: printable runes extend the query,
// enter/ctrl+g jump to the next match, esc/Ctrl+F close it.
func (m *tuiModel) handleFindKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+f":
		m.findOpen = false
		m.findQuery = ""
		m.findMatches = nil
		m.findMatch = 0
		return m, m.nextCmd()
	case "enter", "ctrl+g", "down":
		if len(m.findMatches) > 0 {
			m.findMatch = (m.findMatch + 1) % len(m.findMatches)
			m.scrollToFind(m.findMatches[m.findMatch])
		}
		return m, m.nextCmd()
	case "backspace":
		if len(m.findQuery) > 0 {
			r := []rune(m.findQuery)
			m.findQuery = string(r[:len(r)-1])
			m.findUpdate()
		}
		return m, m.nextCmd()
	}
	if len(msg.Runes) > 0 {
		for _, r := range msg.Runes {
			if r < 32 {
				return m, m.nextCmd() // control rune leaked into the query
			}
		}
		m.findQuery += string(msg.Runes)
		m.findUpdate()
	}
	return m, m.nextCmd()
}

// findUpdate recomputes the match offsets for the current query and jumps to
// the first (or re-clamps) match. Searching is over the committed transcript
// (the live stream tail is below it, so row offsets stay stable).
func (m *tuiModel) findUpdate() {
	hay := strings.Join(m.transcript, "\n")
	q := strings.ToLower(m.findQuery)
	m.findMatches = m.findMatches[:0]
	if q != "" && hay != "" {
		lower := strings.ToLower(hay)
		for start := 0; ; {
			idx := strings.Index(lower[start:], q)
			if idx < 0 {
				break
			}
			abs := start + idx
			m.findMatches = append(m.findMatches, abs)
			start = abs + len(q)
		}
	}
	if len(m.findMatches) == 0 {
		m.findMatch = 0
		return
	}
	if m.findMatch >= len(m.findMatches) {
		m.findMatch = 0
	}
	m.scrollToFind(m.findMatches[m.findMatch])
}

// scrollToFind scrolls the viewport so the transcript row containing the match
// offset is visible. The row index is the newline count before the offset in
// the joined transcript, which the viewport content mirrors (committed lines
// are joined with "\n"; the live stream hangs off the bottom).
func (m *tuiModel) scrollToFind(offset int) {
	hay := strings.Join(m.transcript, "\n")
	row := strings.Count(hay[:offset], "\n")
	m.follow = false
	target := row - m.viewport.Height/3
	if target < 0 {
		target = 0
	}
	// SetYOffset clamps to the available scroll range internally.
	m.viewport.SetYOffset(target)
	m.refreshViewport()
}

// findView renders the search bar: the query, the match position, and a hint.
func (m *tuiModel) findView() string {
	th := m.th
	label := lipgloss.NewStyle().Bold(true).Foreground(th.Primary).Render("find:")
	query := lipgloss.NewStyle().Foreground(th.Foreground).Render(m.findQuery + "▍")
	status := ""
	switch {
	case m.findQuery == "":
		status = lipgloss.NewStyle().Foreground(th.Muted).Render("type to search")
	case len(m.findMatches) == 0:
		status = lipgloss.NewStyle().Foreground(th.Error).Render("no matches")
	default:
		status = lipgloss.NewStyle().Foreground(th.Muted).
			Render(fmt.Sprintf("%d/%d · enter next · esc close", m.findMatch+1, len(m.findMatches)))
	}
	return label + " " + query + "  " + status
}

// handleSettingsKey drives the settings page: up/down to choose a row, enter
// to edit it (persisted immediately), esc to leave editing or the page.
// Choice fields (Options set) open a left/right chooser instead of free text.
func (m *tuiModel) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.choosing {
		entry := config.Settings()[m.settingsIdx]
		switch msg.String() {
		case "left":
			m.choosingIdx = (m.choosingIdx + len(entry.Options) - 1) % len(entry.Options)
		case "right":
			m.choosingIdx = (m.choosingIdx + 1) % len(entry.Options)
		case "enter":
			m.saveChoice(entry)
			m.choosing = false
		case "esc":
			m.choosing = false
		}
		return m, nil
	}
	if m.editing {
		switch msg.String() {
		case "enter":
			entry := config.Settings()[m.settingsIdx]
			value := m.editInput.Value()
			if err := config.Set(m.cfg.Path, entry.Key, value); err != nil {
				m.append("  error: " + err.Error())
			} else if err := applySetting(m.cfg, m.env.registry, entry.Key, value); err != nil {
				m.append("  error: " + err.Error())
			} else {
				m.applyThemeLive(entry.Key, value)
				m.append(fmt.Sprintf("  %s = %s (saved)", entry.Key, value))
			}
			m.editing = false
			return m, nil
		case "esc":
			m.editing = false
			return m, nil
		}
		var cmd tea.Cmd
		m.editInput, cmd = m.editInput.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "esc", "q":
		m.settingsOpen = false
		return m, nil
	case "up":
		if m.settingsIdx > 0 {
			m.settingsIdx--
		}
	case "down":
		if m.settingsIdx < len(config.Settings())-1 {
			m.settingsIdx++
		}
	case "enter":
		entry := config.Settings()[m.settingsIdx]
		if len(entry.Options) > 0 {
			m.choosing = true
			m.choosingIdx = indexOf(entry.Options, m.cfg.Get(entry.Key))
			if m.choosingIdx < 0 {
				m.choosingIdx = 0
			}
			return m, nil
		}
		m.editing = true
		in := textinput.New()
		in.Placeholder = "type value, enter to save, esc to cancel"
		in.CharLimit = 2000
		in.Width = 60
		in.SetValue(m.cfg.Get(entry.Key))
		in.Focus()
		m.editInput = in
	}
	return m, nil
}

// saveChoice persists the selected option for a choice field.
func (m *tuiModel) saveChoice(entry config.SettingKey) {
	if m.choosingIdx < 0 || m.choosingIdx >= len(entry.Options) {
		return
	}
	value := entry.Options[m.choosingIdx]
	if err := config.Set(m.cfg.Path, entry.Key, value); err != nil {
		m.append("  error: " + err.Error())
		return
	}
	if err := applySetting(m.cfg, m.env.registry, entry.Key, value); err != nil {
		m.append("  error: " + err.Error())
		return
	}
	m.applyThemeLive(entry.Key, value)
	m.append(fmt.Sprintf("  %s = %s (saved)", entry.Key, value))
}

// applyThemeLive switches the rendered palette immediately when the theme
// setting changes from the settings page.
func (m *tuiModel) applyThemeLive(key, value string) {
	if key == "theme" {
		m.th = themeByName(value)
	}
}

// openKeyEntry opens the /model selector's inline API-key entry for the current
// provider (matching the configured server_url), or the first cloud provider
// with a key env when no local config matches. The entered key is saved as
// config api_key.
func (m *tuiModel) openKeyEntry() {
	if m.busy {
		m.append("cannot change the API key while a turn is running")
		return
	}
	// find the provider matching the current server_url (or the first cloud one)
	idx := -1
	for i, p := range config.Providers {
		if p.BaseURL == m.cfg.ServerURL {
			idx = i
			break
		}
	}
	if idx < 0 {
		for i, p := range config.Providers {
			if p.KeyEnv != "" {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		m.append("no cloud provider to attach a key to — use /model first")
		return
	}
	prov := config.Providers[idx]
	m.modelOpen = true
	m.modelProvider = idx
	m.modelOnModels = false
	m.modelConfirm = false
	m.modelKeyInput = textinput.New()
	m.modelKeyInput.Placeholder = "paste API key for " + prov.Name + " (" + prov.KeyEnv + ")"
	m.modelKeyInput.Focus()
	m.modelKeyEntry = true
}

// openModelSelector initializes the /model two-pane picker. It refuses while a
// turn is running (the swap must not race the runner). For a Dynamic provider
// (local llama.cpp/Ollama) it fires an async /v1/models fetch so the model
// pane shows what is actually loaded.
func (m *tuiModel) openModelSelector() (tea.Model, tea.Cmd) {
	if m.busy {
		m.append("cannot switch model while a turn is running (wait for it to finish)")
		return m, nil
	}
	m.modelOpen = true
	m.modelProvider = 0
	m.modelModelIdx = 0
	m.modelOnModels = false
	m.modelConfirm = false
	m.modelKeyEntry = false
	m.modelLive = nil
	m.modelLoading = false
	// pre-select the current provider if its base URL matches
	for i, p := range config.Providers {
		if p.BaseURL == m.cfg.ServerURL {
			m.modelProvider = i
			if idx := indexOf(p.Models, m.cfg.Model); idx >= 0 {
				m.modelModelIdx = idx
			}
			break
		}
	}
	// fire the live fetch: local /v1/models for Dynamic providers, models.dev
	// for cloud providers that support it.
	prov := config.Providers[m.modelProvider]
	switch {
	case prov.Dynamic:
		m.modelLoading = true
		return m, m.fetchLocalModels()
	case prov.ModelsDev != "":
		m.modelLoading = true
		return m, m.fetchModelsDev(prov.ModelsDev)
	}
	return m, nil
}

// fetchLocalModels queries the selected Dynamic provider's /v1/models endpoint
// in a tea.Cmd so the TUI stays responsive; the result lands as a modelListMsg.
func (m *tuiModel) fetchLocalModels() tea.Cmd {
	base := config.Providers[m.modelProvider].BaseURL
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.runnerCtx, 4*time.Second)
		defer cancel()
		models, ok := config.FetchModels(ctx, base)
		return modelListMsg{models: models, ok: ok}
	}
}

// fetchModelsDev fetches a cloud provider's current model list from models.dev.
func (m *tuiModel) fetchModelsDev(providerKey string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.runnerCtx, 10*time.Second)
		defer cancel()
		models, ok := config.FetchModelsDev(ctx, providerKey)
		return modelListMsg{models: models, ok: ok, dev: true}
	}
}

// handleModelKey drives the /model picker: tab toggles the active pane,
// left/right move within a pane, enter confirms (applies + rebuilds the
// runtime), esc closes. modelOnModels selects the model pane.
func (m *tuiModel) handleModelKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modelKeyEntry {
		switch msg.String() {
		case "enter":
			// persist the entered key as api_key, then apply the selection
			key := strings.TrimSpace(m.modelKeyInput.Value())
			m.modelKeyEntry = false
			m.modelConfirm = false
			m.modelOpen = false
			if key != "" {
				if err := config.Set(m.cfg.Path, "api_key", key); err != nil {
					m.append("  error: " + err.Error())
					return m, nil
				}
				m.cfg.APIKey = key
			}
			m.applyModelSelection()
		case "esc":
			m.modelKeyEntry = false
			m.modelConfirm = false
		}
		var cmd tea.Cmd
		m.modelKeyInput, cmd = m.modelKeyInput.Update(msg)
		return m, cmd
	}
	if m.modelConfirm {
		switch msg.String() {
		case "y", "enter":
			m.modelConfirm = false
			// a cloud provider with no configured key goes to key-entry first
			prov := config.Providers[m.modelProvider]
			if prov.KeyEnv != "" && m.cfg.KeyFor(prov) == "" {
				m.modelKeyInput = textinput.New()
				m.modelKeyInput.Placeholder = "paste API key for " + prov.Name
				m.modelKeyInput.Focus()
				m.modelKeyEntry = true
				return m, nil
			}
			m.modelOpen = false
			m.applyModelSelection()
		case "n", "esc":
			m.modelConfirm = false
		}
		return m, nil
	}
	switch msg.String() {
	case "esc", "q":
		m.modelOpen = false
		return m, nil
	case "tab":
		m.modelOnModels = !m.modelOnModels
		return m, nil
	case "left", "right":
		if !m.modelOnModels {
			n := len(config.Providers)
			if msg.String() == "left" {
				m.modelProvider = (m.modelProvider + n - 1) % n
			} else {
				m.modelProvider = (m.modelProvider + 1) % n
			}
			m.modelModelIdx = 0
		} else {
			prov := config.Providers[m.modelProvider]
			names := prov.Models
			if prov.Dynamic {
				names = m.modelLive
			}
			if len(names) > 0 {
				if msg.String() == "left" {
					m.modelModelIdx = (m.modelModelIdx + len(names) - 1) % len(names)
				} else {
					m.modelModelIdx = (m.modelModelIdx + 1) % len(names)
				}
			}
		}
		return m, nil
	case "enter":
		if !m.modelOnModels {
			m.modelOnModels = true
		} else {
			m.modelConfirm = true
		}
		return m, nil
	}
	return m, nil
}

// applyModelSelection persists the chosen provider/model and rebuilds the
// client + agent so the next turn uses the new endpoint.
func (m *tuiModel) applyModelSelection() {
	prov := config.Providers[m.modelProvider]
	names := prov.Models
	if prov.Dynamic {
		names = m.modelLive
	}
	model := ""
	if m.modelModelIdx >= 0 && m.modelModelIdx < len(names) {
		model = names[m.modelModelIdx]
	}
	key := m.cfg.KeyFor(prov)
	if err := config.SetProvider(m.cfg.Path, prov, model, key); err != nil {
		m.append("  error: " + err.Error())
		return
	}
	applied := m.cfg.SelectProvider(prov, model)
	m.append(fmt.Sprintf("  switched to %s / %s (%s)", prov.Name, model, prov.BaseURL))

	// Rebuild the runtime: fresh client + agent over the same session.
	client := newLLMClient(m.cfg)
	if applied != "" {
		client.BearerToken = applied
	}
	ag := newAgent(client, m.cfg, m.env, m.yoloToggler,
		func(delta string) { m.incoming <- tokenMsg{delta: delta} },
		func(delta string) { m.incoming <- reasoningMsg{delta: delta} },
		func(call llm.ToolCall) { m.incoming <- toolMsg{call: call} },
		m.trace)
	// carry the conversation context into the new agent
	ag.LoadSession(m.ag.History(), m.ag.RunningSummary())
	ag.SetSessionID(m.env.sessionID)
	m.swapRuntime(client, ag)
}

// openDiffModal opens the /diff review view: the agent's cumulative changes
// since the session baseline (git), colorized and scrollable.
func (m *tuiModel) openDiffModal() {
	if !m.env.gitEnabled {
		m.append("/diff needs the git layer — set git_auto_commit: true in a git repo")
		return
	}
	stat, diff := m.env.sessionDiff()
	m.diffStat = stat
	m.diffText = diff
	m.diffScroll = 0
	m.diffOpen = true
}

// handleDiffKey scrolls the diff modal and offers discard.
func (m *tuiModel) handleDiffKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.diffOpen = false
		return m, nil
	case "up":
		if m.diffScroll > 0 {
			m.diffScroll--
		}
		return m, nil
	case "down":
		m.diffScroll++
		return m, nil
	case "d", "D":
		// discard all session changes via git undo
		ws, _ := os.Getwd()
		commits, _ := gitops.AgentCommits(ws)
		if len(commits) == 0 {
			m.append("nothing to discard")
			return m, nil
		}
		msg2, err := m.env.maybeUndo(ws, len(commits))
		if err != nil {
			m.append("error: " + err.Error())
			return m, nil
		}
		m.append(msg2)
		m.diffOpen = false
		return m, nil
	}
	return m, nil
}

// diffView renders the /diff modal: a stat header plus the colorized diff,
// scrolled to diffScroll.
func (m *tuiModel) diffView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
		Render(iconGear + " Session diff (vs baseline)")
	var body string
	if m.diffStat != "" {
		body += lipgloss.NewStyle().Foreground(m.th.Secondary).Render(m.diffStat) + "\n\n"
	}
	if m.diffText == "" {
		body += "(no tracked changes yet this session)"
	} else {
		lines := strings.Split(renderPatchPreview(m.th, m.diffText), "\n")
		if m.diffScroll > len(lines) {
			m.diffScroll = len(lines)
		}
		view := lines
		if m.diffScroll > 0 {
			if m.diffScroll < len(lines) {
				view = lines[m.diffScroll:]
			} else {
				view = nil
			}
		}
		body += strings.Join(view, "\n")
	}
	hint := lipgloss.NewStyle().Foreground(m.th.Muted).
		Render("↑/↓ scroll   ·   d discard session changes   ·   esc close")
	page := title + "\n\n" + body + "\n\n" + hint
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).Padding(0, 1).Render(page)
}

func indexOf(list []string, value string) int {
	for i, v := range list {
		if v == value {
			return i
		}
	}
	return -1
}

// handleMouse reacts to mouse events: left-click on the thinking block toggles
// it, the wheel scrolls the transcript. Mouse capture is enabled so clicks
// reach the app (drag-selecting transcript text is handed to the terminal no
// longer; wheel scrolling is the trade).
func (m *tuiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress {
			li := m.clickContentLine(msg.Y)
			if li >= 0 && m.thinkingHit(li) {
				m.toggleThinking()
			}
		}
	case tea.MouseButtonWheelUp:
		m.scroll(true)
	case tea.MouseButtonWheelDown:
		m.scroll(false)
	}
	return m, m.nextCmd()
}

// clickContentLine maps a terminal row (msg.Y) to a content-line index in the
// transcript, accounting for the viewport's scroll offset and line wrapping.
func (m *tuiModel) clickContentLine(y int) int {
	if m.width <= 0 || m.viewport.Height <= 0 {
		return -1
	}
	topRow := 1 // the header bar occupies row 0
	if y < topRow || y > topRow+m.viewport.Height-1 {
		return -1
	}
	target := m.viewport.YOffset + (y - topRow)
	rendered := 0
	for li, line := range strings.Split(m.contentString(), "\n") {
		rows := m.renderedRows(line)
		if target >= rendered && target < rendered+rows {
			return li
		}
		rendered += rows
	}
	return -1
}

// renderedRows estimates how many terminal rows a content line occupies at the
// current viewport width (lipgloss wrapping, matching the viewport's renderer).
func (m *tuiModel) renderedRows(line string) int {
	if m.viewport.Width <= 0 {
		return 1
	}
	wrapped := lipgloss.NewStyle().Width(m.viewport.Width).Render(line)
	return strings.Count(wrapped, "\n") + 1
}

// thinkingHit reports whether a content-line index lands on the thinking block
// (committed or live) — the click target for expand/collapse.
func (m *tuiModel) thinkingHit(li int) bool {
	if m.thinkingOpen && m.lastThinkIdx >= 0 && li >= m.lastThinkIdx && li < m.lastThinkIdx+m.thinkingLines {
		return true
	}
	if m.reasoning != "" {
		start := len(m.transcript)
		rows := strings.Count(m.thinkingBlock(), "\n") + 1
		if li >= start && li < start+rows {
			return true
		}
	}
	return false
}

// scroll moves the viewport and drops follow mode when scrolling up.
func (m *tuiModel) scroll(up bool) {
	if up {
		m.follow = false
		m.viewport.ViewUp()
	} else {
		m.viewport.ViewDown()
		if m.viewport.AtBottom() {
			m.follow = true
		}
	}
}

// quitCmd quits, disabling mouse capture first if it's on (so the terminal
// doesn't keep swallowing clicks after the TUI exits).
func (m *tuiModel) quitCmd() tea.Cmd {
	if m.mouseOn {
		return tea.Batch(mouseCmd(false), tea.Quit)
	}
	return tea.Quit
}

// mouseCmd returns a Cmd that enables or disables terminal mouse reporting at
// runtime (EnableMouseCellMotion/DisableMouse are Msg values, so they're
// wrapped as commands).
func mouseCmd(on bool) tea.Cmd {
	if on {
		return func() tea.Msg { return tea.EnableMouseCellMotion() }
	}
	return func() tea.Msg { return tea.DisableMouse() }
}

// toggleMouse switches mouse capture on/off at runtime: off (default) keeps
// drag-select with the terminal; on enables clicking the thinking block and
// wheel-scrolling the transcript.
func (m *tuiModel) toggleMouse() tea.Cmd {
	m.mouseOn = !m.mouseOn
	if m.mouseOn {
		m.append("  mouse capture on — click 🧠 to expand thinking, wheel to scroll (Ctrl+M to release)")
		return mouseCmd(true)
	}
	m.append("  mouse capture off — drag-select text in the terminal works again")
	return mouseCmd(false)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = m.layoutHeight()
		m.refreshViewport()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, tea.Batch(cmd, waitIncoming(m.incoming))

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		if m.helpOpen {
			return m.handleHelpKey(msg)
		}
		if m.checkpointsOpen {
			return m.handleCheckpointsKey(msg)
		}
		if m.sessionsOpen {
			return m.handleSessionsKey(msg)
		}
		if m.settingsOpen {
			return m.handleSettingsKey(msg)
		}
		if m.modelOpen {
			return m.handleModelKey(msg)
		}
		if m.diffOpen {
			return m.handleDiffKey(msg)
		}
		if m.hunkOpen {
			return m.handleHunkKey(msg)
		}
		if m.skillsOpen {
			return m.handleSkillsKey(msg)
		}
		if m.clarifyOpen {
			return m.handleClarifyKey(msg)
		}
		if m.pending != nil {
			switch msg.String() {
			case "y", "Y":
				m.pending <- agent.Approval{OK: true}
				m.pending = nil
				m.append("  " + iconOK + " approved " + m.approveArg)
				return m, m.nextCmd()
			case "n", "N":
				m.pending <- agent.Approval{OK: false}
				m.pending = nil
				m.append("  " + iconBad + " rejected " + m.approveArg)
				return m, m.nextCmd()
			}
		}
		if m.findOpen {
			return m.handleFindKey(msg)
		}
		switch msg.String() {
		case "f1":
			m.helpOpen = true
			return m, nil
		case "?":
			if m.msgInput.Value() == "" {
				m.helpOpen = true
				return m, nil
			}
		case "ctrl+s":
			if m.env != nil && m.env.sessionID != "" {
				md, err := m.env.st.RenderMarkdown(context.Background(), m.env.sessionID)
				if err != nil {
					m.append("  error exporting session: " + err.Error())
				} else {
					path := "session-" + m.env.sessionID + ".md"
					if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
						m.append("  error writing " + path + ": " + err.Error())
					} else {
						m.append("  " + iconOK + " session saved to " + path)
					}
				}
				return m, m.nextCmd()
			}
		case "ctrl+f":
			return m.openFind(), nil
		case "ctrl+c":
			if m.busy && !m.confirmQuit {
				m.confirmQuit = true
				m.append("  turn still running — quit anyway? (y/n)")
				return m, m.nextCmd()
			}
			return m, m.quitCmd()
		case "esc":
			// Cancel the running turn (keep the session): the model stops
			// generating, the partial reasoning/answer is dropped, and the
			// next message starts a fresh turn.
			if m.busy && m.turnCancel != nil {
				m.turnCancelled = true
				m.cancelReason = "turn cancelled (esc) — send another message"
				m.turnCancel()
				m.append("  cancelling turn…")
				return m, m.nextCmd()
			}
		case "ctrl+m":
			return m, m.toggleMouse()
		case "t", "T":
			// Toggle the last thinking block when the input is empty (so
			// typing isn't hijacked) and a toggleable block exists.
			if m.thinkingOpen && m.msgInput.Value() == "" {
				m.toggleThinking()
				return m, m.nextCmd()
			}
		case "enter":
			if m.confirmQuit {
				return m, m.quitCmd()
			}
			// Auto-complete a partial "/..." command before sending it: if the
			// input is a prefix of exactly one slash command, expand to the full
			// command and run it (e.g. "/ex" -> "/export"). If the user
			// navigated the palette with arrows/tab, run the highlighted item.
			if val := m.msgInput.Value(); strings.HasPrefix(val, "/") {
				matches := m.slashMatches()
				if len(matches) > 0 && m.tabIndex > 0 {
					m.msgInput.SetValue(matches[m.tabIndex%len(matches)])
					m.msgInput.CursorEnd()
					m.tabIndex = 0
				} else if len(matches) == 1 && matches[0] != val {
					m.msgInput.SetValue(matches[0])
					m.msgInput.CursorEnd()
				}
			}
			return m.submitLine()
		case "tab":
			if strings.HasPrefix(m.msgInput.Value(), "/") {
				m.completeCommand()
			}
			return m, nil
		case "pgup", "ctrl+u":
			m.scroll(true)
			return m, nil
		case "pgdown", "ctrl+d":
			m.scroll(false)
			return m, nil
		case "up":
			// When the "/" command palette is open, arrows navigate it.
			if matches := m.slashMatches(); len(matches) > 1 {
				m.tabIndex = (m.tabIndex - 1 + len(matches)) % len(matches)
				return m, nil
			}
			// Single-line prompt history navigation
			if !strings.Contains(m.msgInput.Value(), "\n") && len(m.history) > 0 {
				if m.historyIdx == len(m.history) {
					m.draftInput = m.msgInput.Value()
				}
				if m.historyIdx > 0 {
					m.historyIdx--
					m.msgInput.SetValue(m.history[m.historyIdx])
					m.msgInput.CursorEnd()
					return m, nil
				}
			}
			if m.msgInput.Value() == "" {
				m.scroll(true)
				return m, nil
			}
		case "down":
			// When the "/" command palette is open, arrows navigate it.
			if matches := m.slashMatches(); len(matches) > 1 {
				m.tabIndex = (m.tabIndex + 1) % len(matches)
				return m, nil
			}
			// Single-line prompt history navigation
			if !strings.Contains(m.msgInput.Value(), "\n") && len(m.history) > 0 {
				if m.historyIdx < len(m.history)-1 {
					m.historyIdx++
					m.msgInput.SetValue(m.history[m.historyIdx])
					m.msgInput.CursorEnd()
					return m, nil
				} else if m.historyIdx == len(m.history)-1 {
					m.historyIdx = len(m.history)
					m.msgInput.SetValue(m.draftInput)
					m.msgInput.CursorEnd()
					return m, nil
				}
			}
			if m.msgInput.Value() == "" {
				m.scroll(false)
				return m, nil
			}
		case "y", "Y":
			if m.confirmQuit {
				return m, m.quitCmd()
			}
		case "n", "N":
			if m.confirmQuit {
				m.confirmQuit = false
				m.append("  continuing")
				return m, m.nextCmd()
			}
		}
		var cmd tea.Cmd
		m.msgInput, cmd = m.msgInput.Update(msg)
		if m.msgInput.Value() != m.lastInput {
			m.tabIndex = -1 // typing resets the completion cycle
		}
		m.lastInput = m.msgInput.Value()
		return m, cmd

	case tokenMsg:
		m.stream.WriteString(msg.delta)
		m.turnTokens += len(msg.delta) / 4
		m.syncViewport()
		m.checkLoop()
		return m, m.nextCmd()

	case reasoningMsg:
		// Thinking streams before the answer; buffered for display only and
		// never enters history. Honors ui.show_reasoning and caps the buffer
		// so a verbose model can't flood the terminal.
		if m.cfg != nil && !m.cfg.UI.ShowReasoning {
			return m, m.nextCmd()
		}
		m.reasoning += msg.delta
		if len(m.reasoning) > reasoningCap {
			m.reasoningTruncated = true
			m.reasoning = "[… earlier reasoning omitted]\n" + m.reasoning[len(m.reasoning)-reasoningCap:]
		}
		m.syncViewport()
		m.checkLoop()
		return m, m.nextCmd()

	case toolMsg:
		m.flushStream()
		m.toolCalls++
		m.append("  " + iconTool + " " + msg.call.Function.Name + " " + previewArgs(msg.call.Function.Arguments))
		return m, m.nextCmd()

	case progressMsg:
		m.flushStream()
		m.append("  [index] " + msg.text)
		return m, m.nextCmd()

	case modelListMsg:
		// live model list from a Dynamic (local) provider
		m.modelLoading = false
		if msg.ok && len(msg.models) > 0 {
			m.modelLive = msg.models
			if m.modelModelIdx >= len(msg.models) {
				m.modelModelIdx = 0
			}
		}
		return m, nil

	case clarifyRequestMsg:
		m.flushStream()
		m.clarifyOpen = true
		m.clarifyQuestion = msg.question
		m.clarifyChoices = msg.choices
		m.clarifyIdx = 0
		m.clarifyFree = len(msg.choices) == 0
		m.clarifyRespond = msg.respond
		if m.clarifyFree {
			m.clarifyAnswerInput = textinput.New()
			m.clarifyAnswerInput.Placeholder = "type your answer, enter to send"
			m.clarifyAnswerInput.CharLimit = 1000
			m.clarifyAnswerInput.Focus()
		}
		return m, m.nextCmd()

	case approvalRequestMsg:
		m.flushStream()
		if msg.call.Function.Name == "fs_patch" {
			if patch := argsPatch(msg.call); patch != "" {
				if hunks, err := tools.PatchHunks(patch); err == nil && len(hunks) > 1 {
					// Multi-hunk patch: walk hunks one by one so the user can
					// accept or skip each before the whole thing applies.
					m.startHunkReview(msg.respond, hunks, fmt.Sprintf("%v", msg.risk), patch)
					return m, m.nextCmd()
				}
			}
		}
		m.pending = msg.respond
		m.approveArg = msg.call.Function.Name + " " + previewArgs(msg.call.Function.Arguments)
		m.append(fmt.Sprintf("  %s [%s] %s", iconWarn, msg.risk, m.approveArg))
		if d := fsApprovalDiff(m.th, m.workspace, msg.call); d != "" {
			m.append(d)
		}
		m.append("  allow? (y/n)")
		return m, m.nextCmd()

	case turnDoneMsg:
		if msg.seq != m.turnSeq {
			return m, m.nextCmd() // stale turn (cancelled; a new one started)
		}
		if m.turnCancelled {
			// The turn was stopped (Esc or the loop guard); drop the partial
			// reasoning/stream instead of committing it as an answer.
			m.reasoning = ""
			m.reasoningTruncated = false
			m.stream.Reset()
			m.busy = false
			m.turnCancelled = false
			m.turnCancel = nil
			reason := m.cancelReason
			if strings.Contains(reason, "repeating") && !m.retriedLoop && m.lastTurnText != "" {
				// Self-heal: a repetition loop is retried once with the
				// repetition penalty applied (persisted for the session).
				m.retriedLoop = true
				if m.client != nil && m.client.Sampling.RepetitionPenalty == 0 {
					m.client.Sampling.RepetitionPenalty = 1.05
				}
				m.append("  " + reason)
				m.append("  retrying with sampling.repetition_penalty 1.05")
				m.busy = true
				m.submitTurn(m.lastTurnText)
				return m, nil
			}
			m.append("  " + reason)
			return m, m.nextCmd()
		}
		m.flushStream()
		m.busy = false
		m.turnCancel = nil
		if msg.err != nil {
			m.append(fmt.Sprintf("  error: %v", msg.err))
		}
		return m, m.nextCmd()
	}
	return m, m.nextCmd()
}

// submitLine handles the submitted line: local slash commands (/exit, /clear,
// /help, /skills, /skill-name) or a normal agent turn.
func (m *tuiModel) submitLine() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.msgInput.Value())
	if text == "" {
		return m, nil
	}
	if len(m.history) == 0 || m.history[len(m.history)-1] != text {
		m.history = append(m.history, text)
	}
	m.historyIdx = len(m.history)
	m.draftInput = ""

	// An incomplete "/..." is a command prefix, not a message: if it's a strict
	// prefix of at least one command (e.g. "/ex" -> /exit and /export), keep
	// the palette open instead of sending the fragment to the model. Tab/arrows
	// pick the intended command.
	if matches := m.slashMatches(); len(matches) > 0 && matches[0] != text {
		if val := m.msgInput.Value(); strings.HasPrefix(val, "/") {
			m.tabIndex = -1
			m.msgInput.SetValue(val)
			return m, nil
		}
	}

	switch text {
	case "/exit":
		return m, m.quitCmd()
	case "/mouse":
		m.msgInput.Reset()
		return m, m.toggleMouse()
	case "/help":
		m.msgInput.Reset()
		m.helpOpen = true
		return m, nil
	case "/checkpoint", "/checkpoints":
		m.msgInput.Reset()
		return m.openCheckpointsModal(), nil
	case "/settings":
		m.msgInput.Reset()
		m.settingsOpen = true
		m.settingsIdx = 0
		m.editing = false
		return m, nil
	case "/model":
		m.msgInput.Reset()
		return m.openModelSelector()
	case "/diff":
		m.msgInput.Reset()
		m.openDiffModal()
		return m, nil
	case "/key":
		m.msgInput.Reset()
		m.openKeyEntry()
		return m, nil
	case "/sessions":
		m.msgInput.Reset()
		m.sessions, _ = m.env.st.ListSessions(context.Background())
		m.sessionsOpen = true
		m.sessionsIdx = 0
		m.sessionsConfirm = false
		return m, nil
	case "/skills":
		m.msgInput.Reset()
		return m.openSkillsModal(), nil
	case "/retry":
		m.msgInput.Reset()
		if m.lastTurnText == "" {
			m.append("nothing to retry")
			return m, m.nextCmd()
		}
		if m.busy {
			return m, m.nextCmd()
		}
		// A single loop/malformed call is usually sampling instability: retry
		// the last input with a stable profile (temp 0.3 + repetition penalty).
		if m.client != nil {
			m.client.Sampling.Temperature = 0.3
			m.client.Sampling.RepetitionPenalty = 1.05
		}
		m.append("retrying with a stable sampling profile (temp 0.3, repetition_penalty 1.05)")
		m.startTurn(m.lastTurnText)
		return m, nil
	case "/clear":
		m.msgInput.Reset()
		m.ag.Reset()
		m.transcript = nil
		m.resetThinking()
		m.follow = true
		m.refreshViewport()
		m.activeWorkflow = ""
		m.append("history cleared")
		return m, m.nextCmd()
	case "/compact":
		m.msgInput.Reset()
		if m.busy {
			return m, m.nextCmd()
		}
		m.append("compacting conversation…")
		note, err := m.ag.Compact(context.Background())
		if err != nil {
			m.append("error: " + err.Error())
		} else {
			m.append(note)
		}
		return m, m.nextCmd()
	}
	if strings.HasPrefix(text, "/") {
		m.msgInput.Reset()
		if strings.HasPrefix(text, "/goal ") {
			goal := strings.TrimSpace(strings.TrimPrefix(text, "/goal"))
			m.activeWorkflow = "goal: " + shorten(goal, 20)
		} else if strings.HasPrefix(text, "/playbook ") {
			parts := strings.Fields(text)
			if len(parts) > 1 {
				m.activeWorkflow = "playbook: " + parts[1]
			}
		}
		skillsCmd := &skillsHandler{
			store:       m.env.sk,
			reg:         m.env.registry,
			cfg:         m.cfg,
			w:           &appendWriter{m: m},
			approval:    &m.cfg.Skills.WriteApproval,
			ctx:         context.Background(),
			client:      m.client,
			env:         m.env,
			yoloToggler: m.yoloToggler,
		}
		if handled, err := skillsCmd.handle(text, m.ag); handled || err != nil {
			if err != nil {
				m.append("error: " + err.Error())
			}
			return m, m.nextCmd()
		}
		// unknown slash command: fall through and let the model see it
		m.append("> " + text)
	}
	if m.busy {
		return m, nil
	}
	m.msgInput.Reset()
	m.append("> " + text)
	m.startTurn(text)
	return m, nil
}

// startTurn begins a fresh turn for text: resets the stream/reasoning buffers,
// records the input for /retry, and launches it under a cancelable context.
func (m *tuiModel) startTurn(text string) {
	m.busy = true
	m.stream.Reset()
	m.reasoning = ""
	m.reasoningTruncated = false
	m.turnTokens = 0
	m.turnStart = time.Now()
	m.toolCalls = 0
	m.turnCancelled = false
	m.cancelReason = ""
	m.lastTurnText = text
	m.retriedLoop = false
	m.follow = true // new input snaps back to the bottom
	m.submitTurn(text)
}

// submitTurn launches a turn under a fresh cancelable context (so Esc / the
// loop guard stop just this turn while the session stays alive).
func (m *tuiModel) submitTurn(text string) {
	parent := m.runnerCtx
	if parent == nil {
		parent = context.Background() // e.g. in tests that drive the model directly
	}
	turnCtx, turnCancel := context.WithCancel(parent)
	m.turnCancel = turnCancel
	m.turnSeq++
	m.inputCh <- turnRequest{text: text, ctx: turnCtx, seq: m.turnSeq}
}

// fsApprovalDiff renders a colorized before/after preview for fs_edit,
// fs_write and fs_patch approval prompts. Returns "" when the call isn't one
// of those or can't be read.
func fsApprovalDiff(th Theme, ws string, call llm.ToolCall) string {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Patch     string `json:"patch"`
	}
	if err := json.Unmarshal(call.Function.Arguments, &args); err != nil {
		return ""
	}
	switch call.Function.Name {
	case "fs_edit":
		if args.Path == "" {
			return ""
		}
		if _, err := approvePath(ws, args.Path); err != nil {
			return ""
		}
		return renderApprovalDiff(th, args.OldString, args.NewString)
	case "fs_write":
		if args.Path == "" {
			return ""
		}
		full, err := approvePath(ws, args.Path)
		if err != nil {
			return ""
		}
		oldText := ""
		if data, err := os.ReadFile(full); err == nil {
			oldText = string(data)
		}
		return renderApprovalDiff(th, oldText, args.Content)
	case "fs_patch":
		// The patch is already a unified diff; render its lines colorized
		// (add/remove/hunk markers) so the user sees exactly what will change.
		return renderPatchPreview(th, args.Patch)
	default:
		return ""
	}
}

// argsPatch extracts the "patch" argument of an fs_patch call.
func argsPatch(call llm.ToolCall) string {
	var a struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(call.Function.Arguments, &a); err != nil {
		return ""
	}
	return a.Patch
}

// startHunkReview begins the interactive per-hunk walk for a multi-hunk
// fs_patch: the current hunk is rendered and the user steps through with
// y (accept) / n (skip) / q (finish). Only accepted hunks are applied.
func (m *tuiModel) startHunkReview(respond chan agent.Approval, hunks []tools.PatchHunk, risk, patch string) {
	m.hunkRespond = respond
	m.hunkHunks = hunks
	m.hunkIdx = 0
	m.hunkKeep = make([]bool, len(hunks))
	m.hunkOpen = true
	m.hunkPatch = patch
	m.append(fmt.Sprintf("  %s [%s] fs_patch — %d hunks (%s)", iconWarn, risk, len(hunks), hunks[0].File))
	m.renderHunk(m.hunkIdx)
}

// renderHunk shows the current hunk and the accept/skip prompt.
func (m *tuiModel) renderHunk(i int) {
	h := m.hunkHunks[i]
	m.append(fmt.Sprintf("  hunk %d/%d — %s (line %d):", i+1, len(m.hunkHunks), h.File, h.Start))
	for _, ln := range h.Lines {
		m.append("    " + ln)
	}
	m.append("  accept this hunk? (y/n) — q to finish with accepted hunks")
}

// handleHunkKey drives the per-hunk walk.
func (m *tuiModel) handleHunkKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.hunkKeep[m.hunkIdx] = true
		m.hunkIdx++
	case "n", "N":
		m.hunkIdx++
	case "q", "Q", "esc":
		m.finishHunkReview()
		return m, m.nextCmd()
	}
	if m.hunkIdx >= len(m.hunkHunks) {
		m.finishHunkReview()
		return m, m.nextCmd()
	}
	m.renderHunk(m.hunkIdx)
	return m, m.nextCmd()
}

// handleClarifyKey drives the clarify/plan modal: with choices, up/down picks
// and enter confirms (a trailing option types a free answer); without choices,
// free text. esc cancels with an empty answer.
func (m *tuiModel) handleClarifyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.clarifyFree {
		switch msg.String() {
		case "enter":
			m.resolveClarify(m.clarifyAnswerInput.Value())
		case "esc":
			m.resolveClarify("")
		default:
			var cmd tea.Cmd
			m.clarifyAnswerInput, cmd = m.clarifyAnswerInput.Update(msg)
			return m, cmd
		}
		return m, m.nextCmd()
	}
	n := len(m.clarifyChoices)
	switch msg.String() {
	case "up":
		if m.clarifyIdx > 0 {
			m.clarifyIdx--
		}
	case "down":
		if m.clarifyIdx < n { // index n = the "type your own" option
			m.clarifyIdx++
		}
	case "enter":
		if m.clarifyIdx < n {
			m.resolveClarify(m.clarifyChoices[m.clarifyIdx])
		} else {
			m.clarifyFree = true
			m.clarifyAnswerInput = textinput.New()
			m.clarifyAnswerInput.Placeholder = "type your answer, enter to send"
			m.clarifyAnswerInput.CharLimit = 1000
			m.clarifyAnswerInput.Focus()
		}
	case "esc":
		m.resolveClarify("")
	}
	return m, m.nextCmd()
}

// resolveClarify sends the user's answer back to the agent and closes the modal.
func (m *tuiModel) resolveClarify(answer string) {
	if m.clarifyRespond != nil {
		if strings.TrimSpace(answer) == "" {
			answer = "(no answer)"
		}
		m.clarifyRespond <- answer
	}
	m.clarifyOpen = false
	m.clarifyRespond = nil
}

// clarifyView renders the clarify/plan modal (centered over the transcript).
func (m *tuiModel) clarifyView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
		Render(iconWarn + " The agent needs your input")
	marker := lipgloss.NewStyle().Foreground(m.th.Primary).Render("▸")
	dim := lipgloss.NewStyle().Foreground(m.th.Muted)

	body := lipgloss.NewStyle().Foreground(m.th.Foreground).Render(m.clarifyQuestion)
	if m.clarifyFree {
		body += "\n\n" + m.clarifyAnswerInput.View()
		hint := dim.Render("enter to send · esc to cancel")
		return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(m.th.Primary).Padding(0, 1).
			Render(title + "\n\n" + body + "\n\n" + hint)
	}
	var rows []string
	for i, c := range m.clarifyChoices {
		line := c
		if i == m.clarifyIdx {
			rows = append(rows, marker+" "+lipgloss.NewStyle().Background(m.th.Surface).Bold(true).Render(line))
		} else {
			rows = append(rows, "  "+line)
		}
	}
	if m.clarifyIdx == len(m.clarifyChoices) {
		rows = append(rows, marker+" "+lipgloss.NewStyle().Background(m.th.Surface).
			Bold(true).Render("(type your own answer)"))
	} else {
		rows = append(rows, "  "+dim.Render("(type your own answer)"))
	}
	body += "\n\n" + strings.Join(rows, "\n")
	hint := dim.Render("↑/↓ choose · enter confirm · esc cancel")
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).Padding(0, 1).
		Render(title + "\n\n" + body + "\n\n" + hint)
}

// finishHunkReview resolves the walk: apply only accepted hunks (rewriting the
// patch arguments), or deny entirely when none were accepted.
func (m *tuiModel) finishHunkReview() {
	accepted := 0
	for _, k := range m.hunkKeep {
		if k {
			accepted++
		}
	}
	respond := m.hunkRespond
	if accepted == 0 {
		respond <- agent.Approval{OK: false}
		m.append("  " + iconBad + " denied — no hunks accepted")
	} else {
		filtered, err := tools.RebuildPatch(m.hunkPatch, m.hunkKeep)
		if err != nil {
			respond <- agent.Approval{OK: false}
			m.append("  " + iconBad + " denied — could not rebuild patch: " + err.Error())
		} else {
			respond <- agent.Approval{OK: true, Args: mustJSON(map[string]string{"patch": filtered})}
			m.append(fmt.Sprintf("  "+iconOK+" approved %d/%d hunks", accepted, len(m.hunkHunks)))
		}
	}
	m.hunkOpen = false
	m.hunkHunks = nil
	m.hunkKeep = nil
	m.hunkRespond = nil
	m.hunkPatch = ""
}

// hunkView renders the review progress above the input while a hunk walk runs.
func (m *tuiModel) hunkView() string {
	if !m.hunkOpen || m.hunkIdx >= len(m.hunkHunks) {
		return ""
	}
	accepted := 0
	for _, k := range m.hunkKeep {
		if k {
			accepted++
		}
	}
	return lipgloss.NewStyle().Foreground(m.th.Accent).Bold(true).
		Render(fmt.Sprintf("  hunk %d/%d · %d accepted · y accept · n skip · q finish", m.hunkIdx+1, len(m.hunkHunks), accepted))
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// renderPatchPreview colorizes a unified-diff string for approval previews.
func renderPatchPreview(th Theme, patch string) string {
	if patch == "" {
		return "(empty patch)"
	}
	add := lipgloss.NewStyle().Foreground(th.Success)
	del := lipgloss.NewStyle().Foreground(th.Error)
	hunk := lipgloss.NewStyle().Foreground(th.Secondary)
	meta := lipgloss.NewStyle().Foreground(th.Muted)
	var lines []string
	for _, ln := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
			lines = append(lines, meta.Render(ln))
		case strings.HasPrefix(ln, "@@"):
			lines = append(lines, hunk.Render(ln))
		case strings.HasPrefix(ln, "+"):
			lines = append(lines, add.Render(ln))
		case strings.HasPrefix(ln, "-"):
			lines = append(lines, del.Render(ln))
		default:
			lines = append(lines, ln)
		}
	}
	return strings.Join(lines, "\n")
}

// approvePath resolves a model path against the workspace, rejecting escapes
// (mirrors tools.resolvePath, including symlink containment). Absolute paths
// are accepted when they stay inside the workspace.
func approvePath(ws, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("bad path %q", p)
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(ws, p)
	}
	abs = filepath.Clean(abs)
	root := filepath.Clean(ws)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace", p)
	}
	resolved, err := tools.ResolveSymlinks(abs)
	if err != nil {
		return "", err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside the workspace (symlink?)", p)
	}
	return resolved, nil
}

// handleHelpKey drives the interactive help modal: any dismiss key closes it.
func (m *tuiModel) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "enter", "?", "f1":
		m.helpOpen = false
		return m, nil
	}
	return m, nil
}

// helpView renders the interactive help modal (shown as a centered modal over
// the transcript).
func (m *tuiModel) helpView() string {
	th := m.th
	title := lipgloss.NewStyle().Bold(true).Foreground(th.Primary).
		Render(iconGear + " Yagent Keyboard Shortcuts & Commands")

	secStyle := lipgloss.NewStyle().Bold(true).Foreground(th.Secondary)
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(th.Foreground)
	descStyle := lipgloss.NewStyle().Foreground(th.Muted)

	col1 := []string{
		secStyle.Render("Keyboard Shortcuts"),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Enter"), descStyle.Render("Send prompt / execute")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Alt+Enter"), descStyle.Render("Insert newline")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("↑ / ↓"), descStyle.Render("Prompt history (or scroll)")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("PgUp / PgDn"), descStyle.Render("Scroll transcript (Ctrl+U/D)")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Ctrl+F"), descStyle.Render("Search transcript")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Ctrl+M"), descStyle.Render("Toggle mouse capture")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Ctrl+S"), descStyle.Render("Quick export session markdown")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("t"), descStyle.Render("Toggle thinking block expand")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Esc"), descStyle.Render("Cancel turn (keeps session)")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("Ctrl+C"), descStyle.Render("Quit")),
	}

	col2 := []string{
		secStyle.Render("Slash Commands"),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/settings"), descStyle.Render("Interactive config editor")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/model"), descStyle.Render("Switch provider/model (local or cloud)")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/sessions"), descStyle.Render("Session browser (resume/fork)")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/checkpoint"), descStyle.Render("Workspace snapshots")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/playbook"), descStyle.Render("Declarative workflows")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/skills"), descStyle.Render("Procedural skills manager")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/goal <desc>"), descStyle.Render("Autonomous goal loop")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/undo [list|<N>]"), descStyle.Render("Revert previous file changes")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/retry"), descStyle.Render("Retry with stable sampling")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/compact"), descStyle.Render("Condense conversation ledger")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/yolo [on|off]"), descStyle.Render("Toggle auto-approval mode")),
		fmt.Sprintf("  %-16s %s", keyStyle.Render("/clear"), descStyle.Render("Reset transcript history")),
	}

	left := strings.Join(col1, "\n")
	right := strings.Join(col2, "\n")
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, "    ", right)

	hint := descStyle.Render("esc / q / enter to close")
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).
		Padding(0, 1).Render(title + "\n\n" + body + "\n\n" + hint)
}

// openCheckpointsModal opens the interactive workspace snapshots browser.
func (m *tuiModel) openCheckpointsModal() tea.Model {
	m.checkpointsOpen = true
	m.checkpointsIdx = 0
	m.checkpointsConfirm = false
	m.checkpointsAction = ""
	m.refreshCheckpoints()
	return m
}

func (m *tuiModel) refreshCheckpoints() {
	names := checkpoint.List(m.workspace)
	m.checkpoints = make([]checkpointSummary, 0, len(names))
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(m.workspace, ".yagent/checkpoints", n))
		mt := time.Now()
		if err == nil {
			mt = fi.ModTime()
		}
		m.checkpoints = append(m.checkpoints, checkpointSummary{Name: n, ModTime: mt})
	}
}

// handleCheckpointsKey drives the checkpoints manager modal.
func (m *tuiModel) handleCheckpointsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.checkpoints) == 0 {
		switch msg.String() {
		case "esc", "q":
			m.checkpointsOpen = false
			return m, nil
		}
		return m, nil
	}
	if m.checkpointsIdx < 0 || m.checkpointsIdx >= len(m.checkpoints) {
		m.checkpointsIdx = 0
	}
	switch msg.String() {
	case "esc", "q":
		m.checkpointsOpen = false
		return m, nil
	case "up":
		if m.checkpointsIdx > 0 {
			m.checkpointsIdx--
		}
	case "down":
		if m.checkpointsIdx < len(m.checkpoints)-1 {
			m.checkpointsIdx++
		}
	case "r":
		cp := m.checkpoints[m.checkpointsIdx]
		if err := checkpoint.Restore(m.workspace, cp.Name); err != nil {
			m.checkpointsAction = "error: " + err.Error()
			return m, nil
		}
		m.checkpointsOpen = false
		m.append(fmt.Sprintf("  "+iconOK+" restored workspace to checkpoint %q", cp.Name))
		return m, m.nextCmd()
	case "d", "x":
		cp := m.checkpoints[m.checkpointsIdx]
		if m.checkpointsConfirm {
			if err := checkpoint.Delete(m.workspace, cp.Name); err != nil {
				m.checkpointsAction = "error: " + err.Error()
			} else {
				m.append(fmt.Sprintf("  deleted checkpoint %q", cp.Name))
			}
			m.checkpointsConfirm = false
			m.refreshCheckpoints()
			if m.checkpointsIdx >= len(m.checkpoints) && len(m.checkpoints) > 0 {
				m.checkpointsIdx = len(m.checkpoints) - 1
			}
			return m, nil
		}
		m.checkpointsConfirm = true
		return m, nil
	}
	if msg.String() != "d" && msg.String() != "x" {
		m.checkpointsConfirm = false
	}
	m.checkpointsAction = ""
	return m, nil
}

// checkpointsView renders the checkpoints manager modal.
func (m *tuiModel) checkpointsView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
		Render(iconSession + " Workspace Checkpoints")
	marker := lipgloss.NewStyle().Foreground(m.th.Primary).Render("▸")
	dim := lipgloss.NewStyle().Foreground(m.th.Muted)
	rows := make([]string, 0, len(m.checkpoints))
	for i, c := range m.checkpoints {
		timeStr := c.ModTime.Format("2006-01-02 15:04:05")
		line := fmt.Sprintf("%-20s  %s", c.Name, timeStr)
		if i == m.checkpointsIdx {
			rows = append(rows, marker+" "+lipgloss.NewStyle().Background(m.th.Surface).
				Bold(true).Render(line))
		} else {
			rows = append(rows, "  "+dim.Render(line))
		}
	}
	if len(m.checkpoints) == 0 {
		rows = append(rows, "  "+dim.Render("no checkpoints saved in this workspace"))
	}
	body := strings.Join(rows, "\n")
	hint := dim.Render("↑/↓ pick · r restore · d delete (twice) · esc close")
	if m.checkpointsConfirm {
		hint = lipgloss.NewStyle().Foreground(m.th.Error).Render("  delete this checkpoint? press d again to confirm, any key to cancel")
	}
	if m.checkpointsAction != "" {
		action := lipgloss.NewStyle().Foreground(m.th.Primary).Render(m.checkpointsAction)
		body += "\n\n" + action
	}
	bodyStyle := lipgloss.NewStyle().Foreground(m.th.Foreground).Render(body)
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).
		Padding(0, 1).Render(title + "\n\n" + bodyStyle + "\n\n" + hint)
}

// handleSessionsKey drives the session browser: up/down pick a session,
// enter shows actions, r resume, f fork, e export, d delete (twice), esc closes.
func (m *tuiModel) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.sessionsIdx < 0 || (len(m.sessions) > 0 && m.sessionsIdx >= len(m.sessions)) {
		m.sessionsIdx = 0
	}
	switch msg.String() {
	case "esc", "q":
		m.sessionsOpen = false
		return m, nil
	case "up":
		if m.sessionsIdx > 0 {
			m.sessionsIdx--
		}
	case "down":
		if m.sessionsIdx < len(m.sessions)-1 {
			m.sessionsIdx++
		}
	case "d", "x":
		if m.sessionsConfirm {
			id := m.sessions[m.sessionsIdx].ID
			_ = m.env.st.DeleteSession(context.Background(), id)
			m.sessionsConfirm = false
			m.sessions, _ = m.env.st.ListSessions(context.Background())
			if m.sessionsIdx >= len(m.sessions) {
				m.sessionsIdx = len(m.sessions) - 1
			}
			m.append("  deleted session " + id[:8])
			if len(m.sessions) == 0 {
				m.sessionsOpen = false
			}
			return m, nil
		}
		m.sessionsConfirm = true
		return m, nil
	case "enter":
		if len(m.sessions) == 0 {
			return m, nil
		}
		id := m.sessions[m.sessionsIdx].ID
		m.sessionsAction = fmt.Sprintf("r resume   ·   f fork   ·   e export   (resume: yagent chat --continue %s)", id)
		return m, nil
	case "r":
		if len(m.sessions) == 0 {
			return m, nil
		}
		id := m.sessions[m.sessionsIdx].ID
		summary, until, _ := m.env.st.Summary(context.Background(), id)
		history, _ := m.env.st.HistoryAfter(context.Background(), id, until)
		m.ag.LoadSession(history, summary)
		m.ag.SetSessionID(id)
		m.env.sessionID = id
		m.loadHistoryIntoTranscript(history, summary)
		m.sessionsOpen = false
		m.append(fmt.Sprintf("  resumed session %s — continuing it now", id))
		return m, m.nextCmd()
	case "f":
		if len(m.sessions) == 0 {
			return m, nil
		}
		id := m.sessions[m.sessionsIdx].ID
		sid, history, summary, _, err := forkSession(context.Background(), m.env.st, m.workspace, id)
		if err != nil {
			m.sessionsAction = "error: " + err.Error()
			return m, nil
		}
		m.ag.LoadSession(history, summary)
		m.ag.SetSessionID(sid)
		m.env.sessionID = sid
		m.loadHistoryIntoTranscript(history, summary)
		m.sessionsOpen = false
		m.append(fmt.Sprintf("  forked %s -> %s; continuing the fork now", id[:8], sid))
		return m, m.nextCmd()
	case "e":
		if len(m.sessions) == 0 {
			return m, nil
		}
		id := m.sessions[m.sessionsIdx].ID
		md, err := m.env.st.RenderMarkdown(context.Background(), id)
		if err != nil {
			m.sessionsAction = "error: " + err.Error()
			return m, nil
		}
		path := "session-" + id + ".md"
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			m.sessionsAction = "error: " + err.Error()
			return m, nil
		}
		m.sessionsAction = "exported to " + path
		return m, nil
	}
	if msg.String() != "d" && msg.String() != "x" {
		m.sessionsConfirm = false
	}
	m.sessionsAction = ""
	return m, nil
}

// settingsView renders the interactive settings page (shown as a centered
// modal over the transcript).
func (m *tuiModel) settingsView() string {
	keys := config.Settings()
	keyW := 24
	valueW := 44

	marker := lipgloss.NewStyle().Foreground(m.th.Primary).Render("▸")
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(m.th.Foreground)
	valStyle := lipgloss.NewStyle().Foreground(m.th.Muted)

	var rows []string
	for i, s := range keys {
		value := m.cfg.Get(s.Key)
		if value == "" {
			value = "(unset)"
		}
		if len(value) > valueW {
			value = value[:valueW-1] + "…"
		}
		if i == m.settingsIdx {
			line := fmt.Sprintf("%-*s %s", keyW, s.Key, value)
			rows = append(rows, marker+" "+lipgloss.NewStyle().
				Background(m.th.Surface).
				Bold(true).
				Render(line))
		} else {
			rows = append(rows, "  "+keyStyle.Render(fmt.Sprintf("%-*s", keyW, s.Key))+" "+valStyle.Render(value))
		}
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
		Render(iconGear + " Yagent settings")
	body := lipgloss.NewStyle().Foreground(m.th.Foreground).Render(strings.Join(rows, "\n"))
	hint := lipgloss.NewStyle().Foreground(m.th.Muted).
		Render("↑/↓ move   ·   enter edit (saved immediately)   ·   esc close")

	page := title + "\n\n" + body + "\n\n" + hint
	if m.editing {
		field := keys[m.settingsIdx].Key
		prompt := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
			Render("edit " + field + ": ")
		page += "\n\n" + prompt + m.editInput.View()
	}
	if m.choosing {
		field := keys[m.settingsIdx].Key
		cur := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary)
		var opts []string
		for i, o := range keys[m.settingsIdx].Options {
			if i == m.choosingIdx {
				opts = append(opts, cur.Render("<"+o+">"))
			} else {
				opts = append(opts, o)
			}
		}
		page += "\n\n" + lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
			Render("choose "+field+": ") + strings.Join(opts, "   ") +
			"\n" + lipgloss.NewStyle().Foreground(m.th.Muted).
			Render("←/→ change   ·   enter save   ·   esc cancel")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).
		Padding(0, 1).Render(page)
}

// modelView renders the /model provider selector: two side-by-side panes
// (providers | models). The active pane is highlighted; confirm shows the
// chosen endpoint before applying.
func (m *tuiModel) modelView() string {
	marker := lipgloss.NewStyle().Foreground(m.th.Primary).Render("▸")
	provStyle := lipgloss.NewStyle().Foreground(m.th.Foreground)
	modelStyle := lipgloss.NewStyle().Foreground(m.th.Foreground)
	activeStyle := lipgloss.NewStyle().Background(m.th.Surface).Bold(true).Foreground(m.th.Foreground)

	// provider pane
	var provRows []string
	for i, p := range config.Providers {
		row := p.Name
		if i == m.modelProvider && !m.modelOnModels {
			provRows = append(provRows, marker+" "+activeStyle.Render(row))
		} else if i == m.modelProvider {
			provRows = append(provRows, marker+" "+provStyle.Render(row))
		} else {
			provRows = append(provRows, "  "+provStyle.Render(row))
		}
	}
	provPane := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).Padding(0, 1).Render(
		lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).Render("Provider") + "\n\n" +
			strings.Join(provRows, "\n"))

	// model pane — Dynamic providers (local) show the live /v1/models list
	// with a refresh hint; static providers use the catalog list.
	prov := config.Providers[m.modelProvider]
	modelNames := prov.Models
	status := ""
	if prov.Dynamic || prov.ModelsDev != "" {
		modelNames = m.modelLive
		if m.modelLoading {
			status = " (detecting…)"
		} else if m.modelLive == nil {
			status = " (server unreachable — showing defaults)"
		} else if prov.Dynamic {
			status = " (detected)"
		} else {
			status = " (live from models.dev)"
		}
	}
	var modelRows []string
	if len(modelNames) == 0 {
		modelRows = append(modelRows, "  (none detected — set model in /settings)")
	}
	for i, mo := range modelNames {
		row := mo
		if i == m.modelModelIdx && m.modelOnModels {
			modelRows = append(modelRows, marker+" "+activeStyle.Render(row))
		} else if i == m.modelModelIdx {
			modelRows = append(modelRows, marker+" "+modelStyle.Render(row))
		} else {
			modelRows = append(modelRows, "  "+modelStyle.Render(row))
		}
	}
	modelPane := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).Padding(0, 1).Render(
		lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).Render("Model"+status) + "\n\n" +
			strings.Join(modelRows, "\n"))

	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).Render(iconGear + " Model provider")
	hint := lipgloss.NewStyle().Foreground(m.th.Muted).
		Render("tab switch pane   ·   ←/→ choose   ·   enter select   ·   esc close")
	page := title + "\n\n" + lipgloss.JoinHorizontal(lipgloss.Top, provPane, modelPane) + "\n\n" + hint

	if m.modelKeyEntry {
		prov := config.Providers[m.modelProvider]
		page += "\n\n" + lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
			Render("API key for "+prov.Name+" ("+prov.KeyEnv+"):") + "\n" +
			m.modelKeyInput.View() + "\n" +
			lipgloss.NewStyle().Foreground(m.th.Muted).
				Render("enter save (stored in config as api_key)   ·   esc cancel")
		return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(m.th.Primary).Padding(0, 1).Render(page)
	}

	if m.modelConfirm {
		key := m.cfg.KeyFor(prov)
		auth := "no key — enter one below"
		if key != "" {
			auth = "key from config/env"
		}
		chosen := ""
		names := prov.Models
		if prov.Dynamic || prov.ModelsDev != "" {
			names = m.modelLive
		}
		if m.modelModelIdx >= 0 && m.modelModelIdx < len(names) {
			chosen = names[m.modelModelIdx]
		}
		page += "\n\n" + lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
			Render(fmt.Sprintf("switch to %s / %s at %s (%s)?", prov.Name, chosen, prov.BaseURL, auth)) +
			"  " + lipgloss.NewStyle().Foreground(m.th.Muted).Render("y = apply, n = cancel")
		if warn := config.ModelWarning(chosen); warn != "" {
			page += "\n\n" + lipgloss.NewStyle().Foreground(m.th.Error).Render("⚠ "+warn)
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).Padding(0, 1).Render(page)
}

// loadHistoryIntoTranscript renders a loaded session's past messages into the
// visible transcript so resuming shows the prior conversation (and it is
// scrollable). The running summary, if any, is noted at the top.
func (m *tuiModel) loadHistoryIntoTranscript(history []llm.Message, summary string) {
	m.transcript = nil
	m.stream.Reset()
	m.resetThinking()
	if summary != "" {
		m.append("(resumed — the earlier part of this session is condensed into a running summary)")
	}
	for _, h := range history {
		switch h.Role {
		case "user":
			m.append("> " + h.Content)
		case "assistant":
			body := h.Content
			if body == "" {
				body = "(tool calls)"
			}
			m.append(renderMarkdown(body, m.width))
		case "tool":
			snippet := h.Content
			if len(snippet) > 200 {
				snippet = snippet[:200] + "…"
			}
			m.append("  [tool] " + snippet)
		}
	}
	m.follow = true
	m.refreshViewport()
}

// openSkillsModal opens the skills manager: pending staged skill writes with
// diff / verify / approve / reject actions.
func (m *tuiModel) openSkillsModal() tea.Model {
	m.skillsOpen = true
	m.skillsIdx = 0
	m.skillsMsg = ""
	m.skills, _ = m.env.sk.ListPending()
	m.skillsCmd = &skillsHandler{
		store:       m.env.sk,
		reg:         m.env.registry,
		cfg:         m.cfg,
		w:           &appendWriter{m: m},
		approval:    &m.cfg.Skills.WriteApproval,
		ctx:         context.Background(),
		client:      m.client,
		env:         m.env,
		yoloToggler: m.yoloToggler,
	}
	return m
}

// handleSkillsKey drives the skills manager modal (P6): up/down pick a staged
// write, d shows its diff, v runs the verification harness, a/r approve or
// reject it, esc closes.
func (m *tuiModel) handleSkillsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.skills) == 0 {
		switch msg.String() {
		case "esc", "q":
			m.skillsOpen = false
			return m, nil
		}
		return m, nil
	}
	if m.skillsIdx < 0 || m.skillsIdx >= len(m.skills) {
		m.skillsIdx = 0
	}
	p := &m.skills[m.skillsIdx]
	switch msg.String() {
	case "esc", "q":
		m.skillsOpen = false
		m.skillsMsg = ""
		return m, nil
	case "up":
		if m.skillsIdx > 0 {
			m.skillsIdx--
		}
	case "down":
		if m.skillsIdx < len(m.skills)-1 {
			m.skillsIdx++
		}
	case "d":
		diff, err := m.env.sk.PendingDiff(p.ID)
		if err != nil {
			m.skillsMsg = "error: " + err.Error()
		} else {
			m.skillsMsg = capSkillMsg(diff)
		}
	case "v":
		m.append("  verifying " + shortID(p.ID) + " …")
		if err := m.skillsCmd.verifyPending(p.ID); err != nil {
			m.append("  verify error: " + err.Error())
		}
		m.skills, _ = m.env.sk.ListPending()
		m.skillsMsg = ""
	case "a":
		warning, err := m.env.sk.ApprovePending(p.ID)
		if err != nil {
			m.skillsMsg = "error: " + err.Error()
		} else {
			m.append("  approved " + shortID(p.ID))
			if warning != "" {
				m.append("  " + warning)
			}
			m.skills, _ = m.env.sk.ListPending()
			m.skillsMsg = ""
			if m.skillsIdx >= len(m.skills) {
				m.skillsIdx = len(m.skills) - 1
			}
		}
	case "r":
		if err := m.env.sk.RejectPending(p.ID); err != nil {
			m.skillsMsg = "error: " + err.Error()
		} else {
			m.append("  rejected " + shortID(p.ID))
			m.skills, _ = m.env.sk.ListPending()
			m.skillsMsg = ""
			if m.skillsIdx >= len(m.skills) {
				m.skillsIdx = len(m.skills) - 1
			}
		}
	}
	return m, nil
}

// capSkillMsg bounds a diff shown inside the skills modal.
func capSkillMsg(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 40 {
		lines = append(lines[:40], "…")
	}
	return strings.Join(lines, "\n")
}

// skillsView renders the skills manager modal (centered over the transcript).
func (m *tuiModel) skillsView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
		Render(iconGear + " Pending skill writes")
	marker := lipgloss.NewStyle().Foreground(m.th.Primary).Render("▸")
	dim := lipgloss.NewStyle().Foreground(m.th.Muted)
	var rows []string
	if len(m.skills) == 0 {
		rows = append(rows, "  "+dim.Render("no pending skill writes (approval gate off: writes apply immediately)"))
	}
	for i, p := range m.skills {
		note := ""
		if p.Failures >= skills.MaxSkillFailures {
			note = fmt.Sprintf("  (stale — failed verification %d×)", p.Failures)
		} else if p.Failures > 0 {
			note = fmt.Sprintf("  (verification FAIL %d×)", p.Failures)
		}
		line := fmt.Sprintf("%s  %-11s %s%s", shortID(p.ID), p.Action, p.Name, note)
		if i == m.skillsIdx {
			rows = append(rows, marker+" "+lipgloss.NewStyle().Background(m.th.Surface).
				Bold(true).Render(line))
		} else {
			rows = append(rows, "  "+dim.Render(line))
		}
	}
	body := strings.Join(rows, "\n")
	hint := dim.Render("↑/↓ pick · d diff · v verify · a approve · r reject · esc close")
	if m.skillsMsg != "" {
		body += "\n\n" + lipgloss.NewStyle().Foreground(m.th.Foreground).Render(m.skillsMsg)
	}
	bodyStyle := lipgloss.NewStyle().Foreground(m.th.Foreground).Render(body)
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).
		Padding(0, 1).Render(title + "\n\n" + bodyStyle + "\n\n" + hint)
}

// sessionsView renders the session browser (shown as a centered modal over the
// transcript).
func (m *tuiModel) sessionsView() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.th.Primary).
		Render(iconSession + " Sessions")
	marker := lipgloss.NewStyle().Foreground(m.th.Primary).Render("▸")
	dim := lipgloss.NewStyle().Foreground(m.th.Muted)
	rows := make([]string, 0, len(m.sessions))
	for i, s := range m.sessions {
		titleTxt := s.Title
		if titleTxt == "" {
			titleTxt = "(untitled)"
		}
		if len(titleTxt) > 40 {
			titleTxt = titleTxt[:39] + "…"
		}
		line := fmt.Sprintf("%s  %4d msgs  %s", s.ID[:8], s.Messages, titleTxt)
		if i == m.sessionsIdx {
			rows = append(rows, marker+" "+lipgloss.NewStyle().Background(m.th.Surface).
				Bold(true).Render(line))
		} else {
			rows = append(rows, "  "+dim.Render(line))
		}
	}
	if len(m.sessions) == 0 {
		rows = append(rows, "  "+dim.Render("no sessions yet"))
	}
	body := strings.Join(rows, "\n")
	hint := dim.Render("↑/↓ pick · enter commands · r resume · f fork · e export · d delete (twice) · esc close")
	if m.sessionsConfirm {
		hint = lipgloss.NewStyle().Foreground(m.th.Error).Render("  delete this session? press d again to confirm, any key to cancel")
	}
	if m.sessionsAction != "" {
		action := lipgloss.NewStyle().Foreground(m.th.Primary).Render(m.sessionsAction)
		body += "\n\n" + action
	}
	bodyStyle := lipgloss.NewStyle().Foreground(m.th.Foreground).Render(body)
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(m.th.Primary).
		Padding(0, 1).Render(title + "\n\n" + bodyStyle + "\n\n" + hint)
}

// checkLoop cancels the running turn when the streamed text (reasoning or
// answer) visibly repeats itself — the model stuck in a generation loop. Gated
// by ui.loop_guard so users can turn it off.
func (m *tuiModel) checkLoop() {
	if !m.busy || m.turnCancel == nil || m.turnCancelled {
		return
	}
	if m.cfg != nil && !m.cfg.UI.LoopGuard {
		return
	}
	if agent.RepeatLoop(m.reasoning) || agent.RepeatLoop(m.stream.String()) {
		m.turnCancelled = true
		m.cancelReason = "stopped: the model was repeating itself (thinking loop) — /set sampling.repetition_penalty 1.05 often fixes it, or re-ask"
		m.turnCancel()
	}
}

// flushStream commits the current reasoning block and streamed answer into the
// transcript. The thinking block is committed collapsed (a header line) unless
// the user has expanded it; the full text is kept so 't' can toggle it.
func (m *tuiModel) flushStream() {
	if m.reasoning != "" {
		m.commitThinking()
	}
	if m.stream.Len() > 0 {
		m.append(renderMarkdown(strings.TrimRight(m.stream.String(), "\n"), m.width))
		m.stream.Reset()
	}
	m.syncViewport()
}

// commitThinking appends the current reasoning buffer to the transcript as an
// expandable block (collapsed header or full text, per thinkingExpanded).
func (m *tuiModel) commitThinking() {
	styled := renderThinking(strings.TrimSpace(m.reasoning), m.width)
	header := "  " + iconCtx + " thought (" + fmt.Sprint(len(m.reasoning)/4) + " tok) — click or press t to expand"
	m.reasoning = ""
	m.thinkingOpen = true
	m.thinkingText = styled
	m.thinkingHeader = header
	m.lastThinkIdx = len(m.transcript)
	if m.thinkingExpanded {
		lines := strings.Split(styled, "\n")
		m.transcript = append(m.transcript, lines...)
		m.thinkingLines = len(lines)
	} else {
		m.transcript = append(m.transcript, header)
		m.thinkingLines = 1
	}
}

// toggleThinking expands or collapses the thinking display. Applies live (the
// streaming block respects thinkingExpanded) and, for a committed block,
// splices the header/full text in place in the transcript.
func (m *tuiModel) toggleThinking() {
	if m.thinkingOpen && m.lastThinkIdx >= 0 && m.lastThinkIdx < len(m.transcript) {
		if !m.thinkingExpanded {
			// collapse -> expand: replace the header line with the full text
			lines := strings.Split(m.thinkingText, "\n")
			m.replaceTranscript(m.lastThinkIdx, m.thinkingLines, lines...)
			m.thinkingLines = len(lines)
			m.thinkingExpanded = true
		} else {
			// expand -> collapse
			m.replaceTranscript(m.lastThinkIdx, m.thinkingLines, m.thinkingHeader)
			m.thinkingLines = 1
			m.thinkingExpanded = false
		}
	} else if m.reasoning != "" {
		// live block: flip the preference; thinkingBlock() renders accordingly
		m.thinkingExpanded = !m.thinkingExpanded
	}
	m.syncViewport()
}

// replaceTranscript swaps transcript[start:start+n] for the given lines.
func (m *tuiModel) replaceTranscript(start, n int, lines ...string) {
	out := make([]string, 0, len(m.transcript)-n+len(lines))
	out = append(out, m.transcript[:start]...)
	out = append(out, lines...)
	out = append(out, m.transcript[start+n:]...)
	m.transcript = out
}

// resetThinking forgets the toggleable thinking block (transcript reset).
func (m *tuiModel) resetThinking() {
	m.thinkingOpen = false
	m.thinkingText = ""
	m.thinkingHeader = ""
	m.lastThinkIdx = -1
	m.thinkingLines = 0
	m.reasoning = ""
	m.reasoningTruncated = false
}

// thinkingBlock renders the LIVE streaming reasoning: a header with a token
// count, and the full dimmed text only while expanded.
func (m *tuiModel) thinkingBlock() string {
	th := m.th
	header := lipgloss.NewStyle().Foreground(th.Muted).
		Render("  " + iconCtx + " thinking (" + fmt.Sprint(len(m.reasoning)/4) + " tok) — click or press t to expand")
	if !m.thinkingExpanded {
		return header
	}
	body := lipgloss.NewStyle().Foreground(th.Muted).Italic(true).Render(strings.TrimSpace(m.reasoning))
	return header + "\n" + hardWrap(body, m.width)
}

// slashCommands lists the commands offered by the "/" menu: the fixed set plus
// the names of all saved skills (so "/<skill>" completes too).
func (m *tuiModel) slashCommands() []string {
	cmds := []string{
		"/exit", "/clear", "/compact", "/help", "/retry", "/export [file]", "/yolo", "/goal <what>", "/settings", "/set <key> <value>", "/model", "/key",
		"/undo", "/undo list", "/undo <N>", "/diff", "/plan", "/sessions", "/checkpoint", "/checkpoint save <name>", "/checkpoint restore <name>", "/checkpoint delete <name>",
		"/playbook", "/mouse",
		"/skills", "/skills list", "/skills pending", "/skills diff <id>",
		"/skills verify <id>", "/skills approve <id|all>", "/skills reject <id|all>", "/skills approval on|off",
	}
	if m.env != nil && m.env.sk != nil {
		for _, s := range m.env.sk.List() {
			cmds = append(cmds, "/"+s.Name)
		}
	}
	return cmds
}

// slashMatches filters the "/" menu by the current input prefix.
func (m *tuiModel) slashMatches() []string {
	prefix := m.msgInput.Value()
	var out []string
	for _, c := range m.slashCommands() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// completeCommand implements Tab completion: if the input is already a full
// command, cycle through ALL commands; otherwise complete to the next prefix
// match.
func (m *tuiModel) completeCommand() {
	cmds := m.slashCommands()
	val := m.msgInput.Value()
	for i, c := range cmds {
		if c == val {
			m.msgInput.SetValue(cmds[(i+1)%len(cmds)])
			m.msgInput.CursorEnd()
			return
		}
	}
	matches := m.slashMatches()
	if len(matches) > 0 {
		m.tabIndex = (m.tabIndex + 1) % len(matches)
		m.msgInput.SetValue(matches[m.tabIndex])
		m.msgInput.CursorEnd()
	}
}

func (m *tuiModel) View() string {
	if m.err != nil {
		return m.err.Error() + "\n"
	}
	if m.width > 0 {
		// A rounded input bar: leave room for the border + padding so the
		// rendered box exactly matches the window width. Width must be set
		// before layoutHeight so the wrapped input height is accurate.
		base := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(m.th.Border).
			Foreground(m.th.Foreground).
			Background(m.th.Surface).
			Padding(0, 1)
		m.msgInput.FocusedStyle.Base = base
		m.msgInput.BlurredStyle.Base = base
		m.msgInput.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(m.th.Primary)
		m.msgInput.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(m.th.Muted)
		m.msgInput.SetWidth(m.width - 4)
	}
	m.viewport.Height = m.layoutHeight()
	if m.width > 0 {
		m.viewport.Width = m.width // wrap the transcript at the window width
	}
	out := m.headerView() + "\n"
	out += m.viewport.View() + "\n"
	if m.hunkOpen {
		out += m.hunkView() + "\n"
	}
	if m.showPopover() {
		out += m.popoverView() + "\n"
	}
	if m.findOpen {
		out += m.findView() + "\n"
	} else {
		out += m.msgInput.View() + "\n"
	}
	out += m.statusView()
	if m.settingsOpen {
		out = overlayModal(m.th, m.settingsView(), m.width, m.height)
	}
	if m.modelOpen {
		out = overlayModal(m.th, m.modelView(), m.width, m.height)
	}
	if m.diffOpen {
		out = overlayModal(m.th, m.diffView(), m.width, m.height)
	}
	if m.sessionsOpen {
		out = overlayModal(m.th, m.sessionsView(), m.width, m.height)
	}
	if m.checkpointsOpen {
		out = overlayModal(m.th, m.checkpointsView(), m.width, m.height)
	}
	if m.skillsOpen {
		out = overlayModal(m.th, m.skillsView(), m.width, m.height)
	}
	if m.clarifyOpen {
		out = overlayModal(m.th, m.clarifyView(), m.width, m.height)
	}
	if m.helpOpen {
		out = overlayModal(m.th, m.helpView(), m.width, m.height)
	}
	return out
}

// syncViewport pushes the live streaming content (reasoning block + answer
// tail) INTO the viewport so the layout never shifts while a turn streams —
// only the viewport's scroll position changes. The viewport wraps long lines,
// so a stable layout means text can never "grow upward" past the input line.
func (m *tuiModel) syncViewport() {
	if m.width > 0 {
		m.viewport.Width = m.width
	}
	m.viewport.Height = m.layoutHeight()
	m.viewport.SetContent(m.contentString())
	if m.follow {
		m.viewport.GotoBottom()
	}
}

// contentString is the full transcript viewport content: committed lines plus
// the live reasoning block and answer tail.
func (m *tuiModel) contentString() string {
	content := strings.Join(m.transcript, "\n")
	if m.reasoning != "" {
		content += "\n" + m.thinkingBlock()
	}
	if m.stream.Len() > 0 {
		content += "\n" + strings.TrimRight(m.stream.String(), "\n")
	}
	return content
}

// layoutHeight is the transcript viewport height given the current window
// (header + status each take one line; the message input takes its rendered
// height — it wraps multi-line input, capped at a third of the screen; the "/"
// popover borrows two). The streaming content lives inside the viewport, so
// its height is stable for the whole turn.
func (m *tuiModel) layoutHeight() int {
	m.resizeInput()
	in := min(m.inputHeight(), max(1, m.height/3))
	h := m.height - 3 - in
	if m.showPopover() {
		h -= 2
	}
	return max(5, h)
}

// inputHeight is the number of terminal rows the message input occupies
// (multi-line input wraps and grows with its content, capped at a third of the
// screen).
func (m *tuiModel) inputHeight() int {
	return strings.Count(m.msgInput.View(), "\n") + 1
}

// resizeInput sizes the textarea to its wrapped content so the input grows with
// a multi-line paste instead of staying a fixed-height box.
func (m *tuiModel) resizeInput() {
	width := m.msgInput.Width()
	if width < 10 {
		width = 10
	}
	rows := 0
	for _, ln := range strings.Split(m.msgInput.Value(), "\n") {
		n := len([]rune(ln))
		rows += 1
		if n > 0 {
			rows += (n - 1) / width
		}
	}
	maxRows := max(1, m.height/3)
	if rows > maxRows {
		rows = maxRows
	}
	m.msgInput.SetHeight(rows)
}

// showPopover reports whether the "/" command palette should be rendered.
func (m *tuiModel) showPopover() bool {
	if m.helpOpen || m.checkpointsOpen || m.settingsOpen || m.sessionsOpen || m.skillsOpen || m.clarifyOpen || m.findOpen {
		return false
	}
	return strings.HasPrefix(m.msgInput.Value(), "/") && len(m.slashMatches()) > 0
}

// headerView is the persistent top bar: app, workspace, model, session, branch.
func (m *tuiModel) headerView() string {
	th := m.th
	title := th.pill(th.Primary, lipgloss.Color("#15161e"), true).Render(iconYOLO + " YAGENT")
	parts := []string{title}
	if m.activeWorkflow != "" {
		parts = append(parts, th.pill(th.Secondary, lipgloss.Color("#15161e"), true).Render("🎯 "+m.activeWorkflow))
	}
	if m.workspace != "" {
		parts = append(parts, th.pill(th.Surface, th.Foreground, false).Render(shorten(m.workspace, 40)))
	}
	parts = append(parts, th.pill(th.Surface, th.Foreground, false).Render(iconAgent+" "+shorten(m.cfg.Model, 28)))
	if m.env != nil && m.env.sessionID != "" {
		parts = append(parts, th.pill(th.Surface, th.Accent, false).Render(iconSession+" "+shorten(m.env.sessionID, 8)))
	}
	if m.branch != "" {
		parts = append(parts, th.pill(th.Surface, th.Secondary, false).Render(iconBranch+" "+shorten(m.branch, 20)))
	}
	return fitPills(m.width, parts)
}

// statusView is the bottom pill bar: state, context gauge, tokens, tools, yolo.
func (m *tuiModel) statusView() string {
	th := m.th
	state, color := m.statusText()
	var parts []string
	parts = append(parts, th.pill(th.Surface, color, true).Render(state))
	used, limit := m.ag.ContextUsage()
	parts = append(parts, th.pill(th.Surface, color, false).Render(iconCtx+" "+m.ctxGauge(used, limit)))
	// Context-growth forecast: ~N turns until the window would be exhausted,
	// shown once there are enough turns to estimate (median per-turn growth).
	if turns := m.ag.GrowthForecast(); turns >= 0 {
		parts = append(parts, th.pill(th.Surface, th.Muted, false).Render("~"+fmt.Sprint(turns)+" turns"))
	}
	parts = append(parts, th.pill(th.Surface, th.Muted, false).Render(iconTool+" "+fmt.Sprint(m.toolCalls)))
	if m.ag.ContextPressure() {
		parts = append(parts, th.pill(th.Error, "#ffffff", true).Render("⚠ VRAM"))
	}
	if m.yoloToggler != nil && m.yoloToggler.IsYOLO() {
		parts = append(parts, th.pill(th.Error, "#ffffff", true).Render(iconYOLO+" YOLO"))
	}
	return fitPills(m.width, parts)
}

// fitPills joins styled pill strings, greedily dropping trailing pills that
// would exceed the available width (the joined result stays single-line).
func fitPills(width int, pills []string) string {
	if width <= 0 {
		return lipgloss.JoinHorizontal(lipgloss.Center, pills...)
	}
	avail := width - 1 // keep a trailing column as breathing room
	var out []string
	total := 0
	for _, p := range pills {
		w := lipgloss.Width(p)
		if len(out) > 0 && total+w+1 > avail {
			break
		}
		if len(out) > 0 {
			total++ // separator column
		}
		total += w
		out = append(out, p)
	}
	if len(out) == 0 {
		return ""
	}
	return lipgloss.JoinHorizontal(lipgloss.Center, out...)
}

// ctxGauge renders the context gauge: "used/limit ██████░░░░ 38%".
func (m *tuiModel) ctxGauge(used, limit int) string {
	th := m.th
	pct := 0.0
	if limit > 0 {
		pct = float64(used) / float64(limit)
	}
	color := th.Success
	switch {
	case pct > 0.9:
		color = th.Error
	case pct > 0.75:
		color = th.Warning
	}
	cells := 10
	filled := int(pct * float64(cells))
	if filled > cells {
		filled = cells
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", cells-filled)
	pctTxt := fmt.Sprintf("%d%%", int(pct*100))
	if limit > 0 {
		return fmt.Sprintf("%d/%d %s %s", used, limit, lipgloss.NewStyle().Foreground(color).Render(bar), pctTxt)
	}
	return fmt.Sprintf("%d %s", used, pctTxt)
}

// renderThinking styles a committed reasoning block dimmed/italic and wraps it.
func renderThinking(text string, width int) string {
	body := lipgloss.NewStyle().Foreground(tokyoNight.Muted).Italic(true).Render(text)
	return hardWrap(body, width)
}

// statusText is the state segment of the status bar: a spinner/kaomoji marker,
// the state word and the current turn's token count.
func (m *tuiModel) statusText() (string, lipgloss.Color) {
	th := m.th
	state := "ready"
	marker := "(◕‿◕)"
	color := th.Muted
	switch {
	case m.busy:
		state = "working"
		marker = m.spinner.View()
		color = th.Primary
	case m.pending != nil:
		state = "awaiting approval"
		marker = "(；一_一)"
		color = th.Warning
	case m.clarifyOpen:
		state = "awaiting answer"
		marker = "(・_・;)"
		color = th.Warning
	}
	if m.yoloToggler != nil && m.yoloToggler.IsYOLO() {
		state += " · yolo"
	}
	if m.mouseOn {
		state += " · mouse"
	}
	base := marker + " " + state
	if m.busy {
		base += fmt.Sprintf("  ·  %d tok", m.turnTokens)
		if el := time.Since(m.turnStart).Seconds(); el >= 1 && m.turnTokens > 0 {
			base += fmt.Sprintf("  ·  %.1f t/s", float64(m.turnTokens)/el)
		}
	}
	return base, color
}

// popoverView renders the "/" command palette as a bordered box above the
// input, listing the commands that match what the user is typing.
func (m *tuiModel) popoverView() string {
	th := m.th
	matches := m.slashMatches()
	shown := matches
	if len(shown) > 6 {
		shown = shown[:6]
	}
	rows := make([]string, 0, len(shown))
	for i, c := range shown {
		if i == m.tabIndex%len(matches) {
			rows = append(rows, th.pill(th.Primary, "#15161e", true).Render(c))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(th.Foreground).Render("  "+c))
		}
	}
	body := strings.Join(rows, "\n")
	hint := lipgloss.NewStyle().Foreground(th.Muted).Render("  ↑/↓ or tab to select · enter to run · esc to clear")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(th.Border).
		Background(th.Background).
		Padding(0, 1).
		Render(body + "\n" + hint)
}

// overlayModal centers a bordered modal, filling the rest of the screen with
// the theme background.
func overlayModal(th Theme, modal string, width, height int) string {
	if width == 0 || height == 0 {
		return modal + "\n"
	}
	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(th.Background),
	)
}

// shorten truncates s to n runes (n includes the ellipsis).
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// appendWriter routes an io.Writer's output into the transcript (used by the
// /skills handler in the TUI).
type appendWriter struct{ m *tuiModel }

func (a *appendWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			a.m.append(line)
		}
	}
	return len(p), nil
}

// waitIncoming re-arms the channel watcher.
func waitIncoming(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

// nextCmd is the standard re-arm after handling a message: it watches for the
// next incoming message, and keeps the spinner ticking while a turn runs.
func (m *tuiModel) nextCmd() tea.Cmd {
	if m.busy {
		return tea.Batch(waitIncoming(m.incoming), m.spinner.Tick)
	}
	return waitIncoming(m.incoming)
}

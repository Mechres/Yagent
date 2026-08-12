package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/config"
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
	ap := newToggleableApprover(&tuiApprover{incoming: incoming, ctx: runnerCtx})
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
	env.registry.SetIndexProgress(func(line string) { send(progressMsg{text: line}) })
	startBackgroundIndex(runnerCtx, env, func(line string) { send(progressMsg{text: line}) })

	// Agent runner: one turn per input line; on cancel, wraps up the session.
	go func() {
		defer close(runnerDone)
		for {
			select {
			case <-runnerCtx.Done():
				wrapCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				if err := ag.Finish(wrapCtx); err != nil {
					slog.Warn("session-end skill review", "error", err)
				}
				if err := memory.SummarizeSession(wrapCtx, client, env.st, env.vs, env.sessionID); err != nil {
					slog.Warn("session summary", "error", err)
				}
				cancel()
				return
			case req := <-inputCh:
				env.undo.StartTurn()
				answer, err := ag.Run(req.ctx, req.text)
				env.undo.EndTurn()
				incoming <- turnDoneMsg{answer: answer, err: err, seq: req.seq}
			}
		}
	}()

	m := tuiModel{
		cfg: cfg, env: env, ag: ag, client: client,
		incoming: incoming, inputCh: inputCh,
		runnerCtx: runnerCtx, runnerCancel: runnerCancel, runnerDone: runnerDone,
		input: newInput(), yoloToggler: ap,
	}
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
type turnDoneMsg struct {
	answer string
	err    error
	seq    int // turn sequence; stale messages from a cancelled turn are ignored
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

// reasoningCap bounds the per-turn thinking buffer so a verbose model can't
// flood the terminal (the tail is kept, prefixed with an omission marker).
const reasoningCap = 4 << 10

// tuiApprover prompts for approval through the TUI and blocks for the answer.
type tuiApprover struct {
	incoming chan tea.Msg
	ctx      context.Context
}

func (a *tuiApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
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

	input    textinput.Model
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

	// In-viewport transcript search (Ctrl+F): findOpen captures keys into
	// findQuery; findMatches are byte offsets into the joined transcript and
	// findMatch is the current one (jumped to in the viewport).
	findOpen    bool
	findQuery   string
	findMatches []int
	findMatch   int
}

func newInput() textinput.Model {
	in := textinput.New()
	in.Placeholder = "message (enter to send, esc to cancel, ctrl-f to search, pgup/dn to scroll)"
	in.CharLimit = 4000
	in.Prompt = iconCommand + " "
	return in
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
		m.input.Focus(),
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
		if m.sessionsOpen {
			return m.handleSessionsKey(msg)
		}
		if m.settingsOpen {
			return m.handleSettingsKey(msg)
		}
		if m.hunkOpen {
			return m.handleHunkKey(msg)
		}
		if m.skillsOpen {
			return m.handleSkillsKey(msg)
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
			if m.thinkingOpen && m.input.Value() == "" {
				m.toggleThinking()
				return m, m.nextCmd()
			}
		case "enter":
			if m.confirmQuit {
				return m, m.quitCmd()
			}
			return m.submitLine()
		case "tab":
			if strings.HasPrefix(m.input.Value(), "/") {
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
			if m.input.Value() == "" {
				m.scroll(true)
				return m, nil
			}
		case "down":
			if m.input.Value() == "" {
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
		m.input, cmd = m.input.Update(msg)
		if m.input.Value() != m.lastInput {
			m.tabIndex = -1 // typing resets the completion cycle
		}
		m.lastInput = m.input.Value()
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
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	switch text {
	case "/exit":
		return m, m.quitCmd()
	case "/mouse":
		m.input.Reset()
		return m, m.toggleMouse()
	case "/help":
		m.input.Reset()
		m.append("commands: /exit /clear /help /yolo /export [file] /settings /set /goal <what> /undo /mouse /skills list|pending|diff|approve|reject|approval /skill-name")
		m.append("scroll: PgUp/PgDn or Ctrl-U/D, or up/down arrows when the input is empty; search: Ctrl+F; mouse capture: Ctrl+M")
		m.append("esc cancels the running turn; a repeating-thinking loop is auto-stopped (ui.loop_guard)")
		return m, m.nextCmd()
	case "/settings":
		m.input.Reset()
		m.settingsOpen = true
		m.settingsIdx = 0
		m.editing = false
		return m, nil
	case "/sessions":
		m.input.Reset()
		m.sessions, _ = m.env.st.ListSessions(context.Background())
		m.sessionsOpen = true
		m.sessionsIdx = 0
		m.sessionsConfirm = false
		return m, nil
	case "/skills":
		m.input.Reset()
		return m.openSkillsModal(), nil
	case "/clear":
		m.input.Reset()
		m.ag.Reset()
		m.transcript = nil
		m.resetThinking()
		m.follow = true
		m.refreshViewport()
		m.append("history cleared")
		return m, m.nextCmd()
	}
	if strings.HasPrefix(text, "/") {
		m.input.Reset()
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
	m.input.Reset()
	m.append("> " + text)
	m.busy = true
	m.stream.Reset()
	m.reasoning = ""
	m.reasoningTruncated = false
	m.turnTokens = 0
	m.toolCalls = 0
	m.turnCancelled = false
	m.cancelReason = ""
	m.lastTurnText = text
	m.retriedLoop = false
	m.follow = true // new input snaps back to the bottom
	m.submitTurn(text)
	return m, nil
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

// renderApprovalDiff is a colorized line diff (additions in theme green,
// removals in theme red) for approval previews.
func renderApprovalDiff(th Theme, oldText, newText string) string {
	oldLines := splitKeepEmpty(oldText)
	newLines := splitKeepEmpty(newText)
	green := lipgloss.NewStyle().Foreground(th.Success)
	red := lipgloss.NewStyle().Foreground(th.Error)
	hunk := lipgloss.NewStyle().Foreground(th.Secondary)
	var b strings.Builder
	max := len(oldLines)
	if len(newLines) > max {
		max = len(newLines)
	}
	for i := 0; i < max; i++ {
		o, n := "", ""
		hasO, hasN := i < len(oldLines), i < len(newLines)
		if hasO {
			o = oldLines[i]
		}
		if hasN {
			n = newLines[i]
		}
		switch {
		case hasO && hasN && o == n:
			b.WriteString("  " + o + "\n")
		case hasO && hasN:
			b.WriteString(red.Render("- "+o) + "\n")
			b.WriteString(green.Render("+ "+n) + "\n")
		case hasO:
			b.WriteString(red.Render("- "+o) + "\n")
		default:
			b.WriteString(green.Render("+ "+n) + "\n")
		}
	}
	if b.Len() == 0 {
		return "(no changes)"
	}
	return hunk.Render("── diff ──") + "\n" + strings.TrimRight(b.String(), "\n")
}

func splitKeepEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
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
	if repeatLoop(m.reasoning) || repeatLoop(m.stream.String()) {
		m.turnCancelled = true
		m.cancelReason = "stopped: the model was repeating itself (thinking loop) — /set sampling.repetition_penalty 1.05 often fixes it, or re-ask"
		m.turnCancel()
	}
}

// repeatLoop reports whether the tail of s shows any unit (20–160 chars)
// repeated at least three times in a row — a strong signal of a model stuck in
// a generation loop. Units shorter than ~20 chars are too common to trust;
// legitimate reasoning rarely repeats a 20+ char unit three times verbatim.
func repeatLoop(s string) bool {
	const (
		minUnit = 20
		maxUnit = 160
		reps    = 3
	)
	for unit := minUnit; unit <= maxUnit && len(s) >= unit*reps; unit++ {
		tail := s[len(s)-unit*reps:]
		if tail[:unit] == tail[unit:unit*2] && tail[unit:unit*2] == tail[unit*2:] {
			return true
		}
	}
	return false
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
		"/exit", "/clear", "/help", "/export [file]", "/yolo", "/goal <what>", "/settings", "/set <key> <value>", "/undo", "/sessions",
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
	prefix := m.input.Value()
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
	val := m.input.Value()
	for i, c := range cmds {
		if c == val {
			m.input.SetValue(cmds[(i+1)%len(cmds)])
			m.input.CursorEnd()
			return
		}
	}
	matches := m.slashMatches()
	if len(matches) > 0 {
		m.tabIndex = (m.tabIndex + 1) % len(matches)
		m.input.SetValue(matches[m.tabIndex])
		m.input.CursorEnd()
	}
}

func (m *tuiModel) View() string {
	if m.err != nil {
		return m.err.Error() + "\n"
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
	if m.width > 0 {
		m.input.Width = m.width // cap the visible input so long text/placeholder scrolls, never overflows
	}
	if m.findOpen {
		out += m.findView() + "\n"
	} else {
		out += m.input.View() + "\n"
	}
	out += m.statusView()
	if m.settingsOpen {
		out = overlayModal(m.th, m.settingsView(), m.width, m.height)
	}
	if m.sessionsOpen {
		out = overlayModal(m.th, m.sessionsView(), m.width, m.height)
	}
	if m.skillsOpen {
		out = overlayModal(m.th, m.skillsView(), m.width, m.height)
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
// (header + status + input always take three lines; the "/" popover borrows
// two). The streaming content lives inside the viewport, so its height is
// fixed — the layout is stable for the whole turn.
func (m *tuiModel) layoutHeight() int {
	h := m.height - 4
	if m.showPopover() {
		h -= 2
	}
	return max(5, h)
}

// showPopover reports whether the "/" command palette should be rendered.
func (m *tuiModel) showPopover() bool {
	if m.settingsOpen || m.sessionsOpen || m.skillsOpen || m.findOpen {
		return false
	}
	return strings.HasPrefix(m.input.Value(), "/") && len(m.slashMatches()) > 0
}

// headerView is the persistent top bar: app, workspace, model, session, branch.
func (m *tuiModel) headerView() string {
	th := m.th
	title := th.pill(th.Primary, lipgloss.Color("#15161e"), true).Render(iconYOLO + " YAGENT")
	parts := []string{title}
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
	parts = append(parts, th.pill(th.Surface, th.Muted, false).Render(iconTool+" "+fmt.Sprint(m.toolCalls)))
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
	hint := lipgloss.NewStyle().Foreground(th.Muted).Render("  tab to cycle · esc to clear · enter to run")
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

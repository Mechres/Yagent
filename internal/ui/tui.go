package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"yagent/internal/agent"
	"yagent/internal/config"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/tools"
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
func RunTUI(ctx context.Context, client *llm.Client, cfg *config.Config, continueID string, yolo bool, forkID string) error {
	env, err := newChatEnv(ctx, cfg, continueID, forkID)
	if err != nil {
		return err
	}
	defer env.st.Close()
	defer env.vs.Close()
	defer env.idx.Close()

	incoming := make(chan tea.Msg, 4096)
	inputCh := make(chan string, 1)
	runnerCtx, runnerCancel := context.WithCancel(ctx)
	runnerDone := make(chan struct{})
	ap := newToggleableApprover(&tuiApprover{incoming: incoming, ctx: runnerCtx})
	ap.SetYOLO(yolo)
	if yolo {
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
		func(call llm.ToolCall) { send(toolMsg{call: call}) })
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
			case line := <-inputCh:
				answer, err := ag.Run(runnerCtx, line)
				incoming <- turnDoneMsg{answer: answer, err: err}
			}
		}
	}()

	m := tuiModel{
		cfg: cfg, env: env, ag: ag, client: client,
		incoming: incoming, inputCh: inputCh,
		runnerCancel: runnerCancel, runnerDone: runnerDone,
		input: newInput(), yoloToggler: ap,
	}
	if ws, err := os.Getwd(); err == nil {
		m.workspace = ws
	}
	m.viewport = viewport.New(80, 20)
	m.viewport.KeyMap.Up.SetEnabled(false) // avoid clashing with the text input
	m.viewport.KeyMap.Down.SetEnabled(false)
	// No mouse capture: wheel scroll is replaced by keyboard scroll (PgUp/PgDn,
	// arrows when the input is empty, Ctrl-U/D), and leaving the mouse to the
	// terminal keeps text selection/copy working normally.
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
type toolMsg struct{ call llm.ToolCall }
type progressMsg struct{ text string }
type turnDoneMsg struct {
	answer string
	err    error
}
type approvalRequestMsg struct {
	call    llm.ToolCall
	risk    tools.RiskLevel
	respond chan bool
}

// tuiApprover prompts for approval through the TUI and blocks for the answer.
type tuiApprover struct {
	incoming chan tea.Msg
	ctx      context.Context
}

func (a *tuiApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error) {
	respond := make(chan bool, 1)
	select {
	case a.incoming <- approvalRequestMsg{call: call, risk: risk, respond: respond}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case ok := <-respond:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
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
	inputCh      chan string
	runnerCancel context.CancelFunc
	runnerDone   chan struct{}

	input    textinput.Model
	viewport viewport.Model

	transcript  []string
	stream      strings.Builder
	busy        bool
	turnTokens  int
	toolCalls   int
	pending     chan bool
	approveArg  string
	confirmQuit bool
	follow      bool // follow the bottom of the transcript
	tabIndex    int
	lastInput   string
	err         error

	settingsOpen bool
	settingsIdx  int
	editing      bool
	editInput    textinput.Model
}

func newInput() textinput.Model {
	in := textinput.New()
	in.Placeholder = "message (enter to send, ctrl-c to quit, pgup/dn to scroll)"
	in.CharLimit = 4000
	return in
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
	m.refreshViewport()
}

func (m *tuiModel) refreshViewport() {
	m.viewport.SetContent(strings.Join(m.transcript, "\n"))
	if m.follow {
		m.viewport.GotoBottom()
	}
}

// handleSettingsKey drives the settings page: up/down to choose a row, enter
// to edit it (persisted immediately), esc to leave editing or the page.
func (m *tuiModel) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		m.editing = true
		entry := config.Settings()[m.settingsIdx]
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

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = msg.Width
		m.viewport.Height = max(5, msg.Height-4)
		m.refreshViewport()
		return m, nil

	case tea.KeyMsg:
		if m.settingsOpen {
			return m.handleSettingsKey(msg)
		}
		if m.pending != nil {
			switch msg.String() {
			case "y", "Y":
				m.pending <- true
				m.pending = nil
				m.append("  ✓ approved " + m.approveArg)
				return m, waitIncoming(m.incoming)
			case "n", "N":
				m.pending <- false
				m.pending = nil
				m.append("  ✗ rejected " + m.approveArg)
				return m, waitIncoming(m.incoming)
			}
		}
		switch msg.String() {
		case "ctrl+c":
			if m.busy && !m.confirmQuit {
				m.confirmQuit = true
				m.append("  turn still running — quit anyway? (y/n)")
				return m, waitIncoming(m.incoming)
			}
			return m, tea.Quit
		case "enter":
			if m.confirmQuit {
				return m, tea.Quit
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
				return m, tea.Quit
			}
		case "n", "N":
			if m.confirmQuit {
				m.confirmQuit = false
				m.append("  continuing")
				return m, waitIncoming(m.incoming)
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
		return m, waitIncoming(m.incoming)

	case toolMsg:
		m.flushStream()
		m.toolCalls++
		m.append("  → " + msg.call.Function.Name + " " + previewArgs(msg.call.Function.Arguments))
		return m, waitIncoming(m.incoming)

	case progressMsg:
		m.flushStream()
		m.append("  [index] " + msg.text)
		return m, waitIncoming(m.incoming)

	case approvalRequestMsg:
		m.flushStream()
		m.pending = msg.respond
		m.approveArg = msg.call.Function.Name + " " + previewArgs(msg.call.Function.Arguments)
		m.append(fmt.Sprintf("  ⚠ [%s] %s", msg.risk, m.approveArg))
		if d := fsApprovalDiff(m.workspace, msg.call); d != "" {
			m.append(d)
		}
		m.append("  allow? (y/n)")
		return m, waitIncoming(m.incoming)

	case turnDoneMsg:
		m.flushStream()
		m.busy = false
		if msg.err != nil {
			m.append(fmt.Sprintf("  error: %v", msg.err))
		}
		return m, waitIncoming(m.incoming)
	}
	return m, waitIncoming(m.incoming)
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
		return m, tea.Quit
	case "/help":
		m.input.Reset()
		m.append("commands: /exit /clear /help /yolo /export [file] /settings /set /goal <what> /skills list|pending|diff|approve|reject|approval /skill-name")
		m.append("scroll: PgUp/PgDn or Ctrl-U/D, or up/down arrows when the input is empty")
		return m, waitIncoming(m.incoming)
	case "/settings":
		m.input.Reset()
		m.settingsOpen = true
		m.settingsIdx = 0
		m.editing = false
		return m, nil
	case "/clear":
		m.input.Reset()
		m.ag.Reset()
		m.transcript = nil
		m.follow = true
		m.refreshViewport()
		m.append("history cleared")
		return m, waitIncoming(m.incoming)
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
			return m, waitIncoming(m.incoming)
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
	m.turnTokens = 0
	m.toolCalls = 0
	m.follow = true // new input snaps back to the bottom
	m.inputCh <- text
	return m, nil
}

// fsApprovalDiff renders a colorized before/after preview for fs_edit and
// fs_write approval prompts. Returns "" when the call isn't one of those or
// can't be read.
func fsApprovalDiff(ws string, call llm.ToolCall) string {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(call.Function.Arguments, &args); err != nil || args.Path == "" {
		return ""
	}
	full, err := approvePath(ws, args.Path)
	if err != nil {
		return ""
	}
	var oldText, newText string
	switch call.Function.Name {
	case "fs_edit":
		oldText, newText = args.OldString, args.NewString
	case "fs_write":
		if data, err := os.ReadFile(full); err == nil {
			oldText = string(data)
		}
		newText = args.Content
	default:
		return ""
	}
	return renderApprovalDiff(oldText, newText)
}

// approvePath resolves a model path relative to the workspace, rejecting
// escapes (mirrors tools.resolvePath).
func approvePath(ws, p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", fmt.Errorf("bad path %q", p)
	}
	abs := filepath.Clean(filepath.Join(ws, p))
	root := filepath.Clean(ws)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace", p)
	}
	return abs, nil
}

// renderApprovalDiff is a crude colorized line diff (additions green,
// removals red) for approval previews.
func renderApprovalDiff(oldText, newText string) string {
	oldLines := splitKeepEmpty(oldText)
	newLines := splitKeepEmpty(newText)
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
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
	return strings.TrimRight(b.String(), "\n")
}

func splitKeepEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// settingsView renders the interactive settings page.
func (m *tuiModel) settingsView() string {
	keys := config.Settings()
	keyW := 24
	valueW := 44

	marker := lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Render("▸")
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

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
				Background(lipgloss.Color("237")).
				Bold(true).
				Render(line))
		} else {
			rows = append(rows, "  "+keyStyle.Render(fmt.Sprintf("%-*s", keyW, s.Key))+" "+valStyle.Render(value))
		}
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).
		Render("⚙ Yagent settings")
	body := lipgloss.NewStyle().Foreground(lipgloss.Color("188")).Render(strings.Join(rows, "\n"))
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render("↑/↓ move   ·   enter edit (saved immediately)   ·   esc close")

	page := title + "\n\n" + body + "\n\n" + hint
	if m.editing {
		field := keys[m.settingsIdx].Key
		prompt := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).
			Render("edit " + field + ": ")
		page += "\n\n" + prompt + m.editInput.View()
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).Render(page)
}

// flushStream commits the current streamed answer into the transcript.
func (m *tuiModel) flushStream() {
	if m.stream.Len() > 0 {
		m.append(strings.TrimRight(m.stream.String(), "\n"))
		m.stream.Reset()
	}
}

// slashCommands lists the commands offered by the "/" menu: the fixed set plus
// the names of all saved skills (so "/<skill>" completes too).
func (m *tuiModel) slashCommands() []string {
	cmds := []string{
		"/exit", "/clear", "/help", "/export [file]", "/yolo", "/goal <what>", "/settings", "/set <key> <value>",
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
	if m.settingsOpen {
		return m.settingsView()
	}
	// The viewport holds the transcript; the streaming tail renders as its own
	// line below it so scrolling is never reset by per-frame content updates.
	out := m.viewport.View() + "\n"
	if m.stream.Len() > 0 {
		out += strings.TrimRight(m.stream.String(), "\n") + "\n"
	}
	used, limit := m.ag.ContextUsage()
	ctxColor := lipgloss.Color("212")
	if used >= limit {
		ctxColor = lipgloss.Color("196") // over budget
	}
	status := lipgloss.NewStyle().
		Bold(true).
		Foreground(ctxColor).
		Render(fmt.Sprintf(" %-24s │ ctx %d/%d │ %s ", m.cfg.Model, used, limit, m.statusText()))
	// "/" menu: show the matching slash commands while typing a command
	if strings.HasPrefix(m.input.Value(), "/") {
		if matches := m.slashMatches(); len(matches) > 0 {
			shown := matches
			if len(shown) > 6 {
				shown = shown[:6]
			}
			hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
				Render("  " + strings.Join(shown, "   ") + "   (tab to cycle)")
			out += hint + "\n"
		}
	}
	out += m.input.View() + "\n" + status
	return out
}

func (m *tuiModel) statusText() string {
	kaomoji := "(◕‿◕)"
	state := "ready"
	switch {
	case m.busy:
		kaomoji, state = "(・_・)ノ", "working"
	case m.pending != nil:
		kaomoji, state = "(；一_一)", "awaiting approval"
	}
	if m.yoloToggler != nil && m.yoloToggler.IsYOLO() {
		state += " · yolo"
	}
	return fmt.Sprintf("%s %s  │ session %s  │ %d tok  │ %d tools", kaomoji, state, m.env.sessionID[:8], m.turnTokens, m.toolCalls)
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

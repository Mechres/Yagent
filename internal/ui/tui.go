package ui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
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

// RunTUI drives the bubbletea interface: streaming answers, tool cards,
// approval prompts and a status line.
func RunTUI(ctx context.Context, client *llm.Client, cfg *config.Config, continueID string) error {
	env, err := newChatEnv(ctx, cfg, continueID)
	if err != nil {
		return err
	}
	defer env.st.Close()
	defer env.vs.Close()
	defer env.idx.Close()

	incoming := make(chan tea.Msg, 256)
	inputCh := make(chan string, 1)
	runnerCtx, runnerCancel := context.WithCancel(ctx)
	runnerDone := make(chan struct{})
	approver := &tuiApprover{incoming: incoming, ctx: runnerCtx}

	ag := newAgent(client, cfg, env, approver,
		func(delta string) { incoming <- tokenMsg{delta: delta} },
		func(call llm.ToolCall) { incoming <- toolMsg{call: call} })
	env.registry.SetIndexProgress(func(line string) {
		incoming <- toolMsg{call: llm.ToolCall{Function: llm.ToolCallFunction{Name: "index_repo"}, ID: "progress", Type: "function"}}
		incoming <- progressMsg{text: line}
	})

	// Agent runner: one turn per input line; on cancel, wraps up the session.
	go func() {
		defer close(runnerDone)
		for {
			select {
			case <-runnerCtx.Done():
				// Session-end summary is best-effort and must not block the
				// TUI exit for long.
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
		cfg: cfg, env: env, ag: ag,
		incoming: incoming, inputCh: inputCh,
		runnerCancel: runnerCancel, runnerDone: runnerDone,
		input: newInput(),
	}
	p := tea.NewProgram(&m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return err
	}
	runnerCancel()
	<-runnerDone
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
type quitMsg struct{}

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
	cfg *config.Config
	env *chatEnv
	ag  *agent.Agent

	incoming     chan tea.Msg
	inputCh      chan string
	runnerCancel context.CancelFunc
	runnerDone   chan struct{}

	input       textinput.Model
	lines       []string
	busy        bool
	stream      strings.Builder
	turnTokens  int
	toolCalls   int
	pending     chan bool
	approveCall string
	err         error
}

func newInput() textinput.Model {
	in := textinput.New()
	in.Placeholder = "message (enter to send, ctrl-c to quit)"
	in.CharLimit = 4000
	return in
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(
		m.input.Focus(),
		waitIncoming(m.incoming),
	)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.pending != nil {
			switch msg.String() {
			case "y", "Y":
				m.pending <- true
				m.pending = nil
				m.lines = append(m.lines, "  ✓ approved "+m.approveCall)
				m.approveCall = ""
				return m, waitIncoming(m.incoming)
			case "n", "N":
				m.pending <- false
				m.pending = nil
				m.lines = append(m.lines, "  ✗ rejected "+m.approveCall)
				m.approveCall = ""
				return m, waitIncoming(m.incoming)
			}
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			text := m.input.Value()
			if strings.TrimSpace(text) == "" || m.busy {
				return m, nil
			}
			m.input.Reset()
			m.lines = append(m.lines, "> "+text)
			m.busy = true
			m.stream.Reset()
			m.turnTokens = 0
			m.toolCalls = 0
			m.inputCh <- text
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tokenMsg:
		m.stream.WriteString(msg.delta)
		m.turnTokens += len(msg.delta) / 4
		return m, waitIncoming(m.incoming)

	case toolMsg:
		m.flushStream()
		m.toolCalls++
		m.lines = append(m.lines, "  → "+msg.call.Function.Name+" "+previewArgs(msg.call.Function.Arguments))
		return m, waitIncoming(m.incoming)

	case progressMsg:
		m.flushStream()
		m.lines = append(m.lines, "  [index] "+msg.text)
		return m, waitIncoming(m.incoming)

	case approvalRequestMsg:
		m.flushStream()
		m.pending = msg.respond
		m.approveCall = msg.call.Function.Name + " " + previewArgs(msg.call.Function.Arguments)
		m.lines = append(m.lines, fmt.Sprintf("  ⚠ [%s] %s — allow? (y/n)", msg.risk, m.approveCall))
		return m, waitIncoming(m.incoming)

	case turnDoneMsg:
		m.flushStream()
		m.busy = false
		if msg.err != nil {
			m.lines = append(m.lines, fmt.Sprintf("  error: %v", msg.err))
		}
		return m, waitIncoming(m.incoming)

	case quitMsg:
		return m, tea.Quit
	}
	return m, waitIncoming(m.incoming)
}

// flushStream commits the current streamed answer into the transcript.
func (m *tuiModel) flushStream() {
	if m.stream.Len() > 0 {
		m.lines = append(m.lines, strings.TrimRight(m.stream.String(), "\n"))
		m.stream.Reset()
	}
}

func (m *tuiModel) View() string {
	if m.err != nil {
		return m.err.Error() + "\n"
	}
	height := m.viewHeight()
	transcript := m.renderLines(height)
	status := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("212")).
		Render(fmt.Sprintf(" %-24s │ %s ", m.cfg.Model, m.statusText()))
	return transcript + "\n" + m.input.View() + "\n" + status
}

func (m *tuiModel) viewHeight() int {
	return 20
}

func (m *tuiModel) renderLines(height int) string {
	all := m.lines
	// show the streaming tail on top of the transcript
	var tail []string
	if m.stream.Len() > 0 {
		tail = append(tail, strings.TrimRight(m.stream.String(), "\n"))
	}
	total := append(all, tail...)
	if len(total) > height {
		total = total[len(total)-height:]
	}
	return strings.Join(total, "\n")
}

func (m *tuiModel) statusText() string {
	state := "ready"
	if m.busy {
		state = "busy"
	}
	if m.pending != nil {
		state = "awaiting approval"
	}
	return fmt.Sprintf("session %s  │ %s  │ %d tok  │ %d tools", m.env.sessionID[:8], state, m.turnTokens, m.toolCalls)
}

// waitIncoming re-arms the channel watcher.
func waitIncoming(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

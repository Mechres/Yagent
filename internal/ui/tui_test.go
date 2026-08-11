package ui

import (
	"context"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
)

func testModel(t *testing.T) *tuiModel {
	t.Helper()
	sk, err := skills.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &tuiModel{env: &chatEnv{sk: sk}, input: newInput()}
}

func TestTabCyclesCommands(t *testing.T) {
	m := testModel(t)
	m.input.SetValue("/")
	m.completeCommand()
	first := m.input.Value()
	if first == "/" || !strings.HasPrefix(first, "/") {
		t.Fatalf("first completion = %q", first)
	}
	m.completeCommand()
	second := m.input.Value()
	if second == first {
		t.Errorf("tab did not cycle: %q", second)
	}
	// an exact command must cycle to a different one (regression: stuck on
	// the first match)
	m.input.SetValue("/exit")
	m.completeCommand()
	if m.input.Value() == "/exit" {
		t.Errorf("tab stuck on /exit, should cycle to the next command")
	}
}

func TestSlashMatchesIncludeSkills(t *testing.T) {
	m := testModel(t)
	if _, err := m.env.sk.Apply(skills.Op{
		Action: skills.ActionCreate, Name: "smoke-test",
		Content: "---\nname: smoke-test\ndescription: smoke test skill\n---\n## When to Use\nx\n## Procedure\ny\n",
	}); err != nil {
		t.Fatal(err)
	}
	m.input.SetValue("/smoke")
	matches := m.slashMatches()
	if len(matches) != 1 || matches[0] != "/smoke-test" {
		t.Errorf("matches = %v, want [/smoke-test]", matches)
	}
}

func TestRenderApprovalDiff(t *testing.T) {
	d := renderApprovalDiff("line one\nold line\nline three", "line one\nnew line\nline three")
	if !strings.Contains(d, "- old line") || !strings.Contains(d, "+ new line") {
		t.Errorf("diff missing changes: %q", d)
	}
	if !strings.Contains(d, "line one") {
		t.Errorf("unchanged line missing: %q", d)
	}
}

func TestFsApprovalDiff(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit",
		Arguments: []byte(`{"path":"a.txt","old_string":"old content","new_string":"new content"}`)}}
	d := fsApprovalDiff(ws, edit)
	if !strings.Contains(d, "- old content") || !strings.Contains(d, "+ new content") {
		t.Errorf("fs_edit diff = %q", d)
	}
	// path traversal -> no diff
	evil := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit",
		Arguments: []byte(`{"path":"../evil","old_string":"a","new_string":"b"}`)}}
	if d := fsApprovalDiff(ws, evil); d != "" {
		t.Errorf("traversal should yield no diff, got %q", d)
	}
	// non-fs tool -> no diff
	other := llm.ToolCall{Function: llm.ToolCallFunction{Name: "shell_exec", Arguments: []byte(`{"command":"ls"}`)}}
	if d := fsApprovalDiff(ws, other); d != "" {
		t.Errorf("non-fs tool should yield no diff, got %q", d)
	}
}

type recordingApprover struct{ n int }

func (r *recordingApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error) {
	r.n++
	return true, nil
}

func TestToggleableApprover(t *testing.T) {
	inner := &recordingApprover{}
	a := newToggleableApprover(inner)
	call := llm.ToolCall{}
	// yolo off -> delegates
	if ok, _ := a.Approve(context.Background(), call, tools.RiskDestructive); !ok || inner.n != 1 {
		t.Errorf("off mode: ok=%v n=%d, want delegate", ok, inner.n)
	}
	// yolo on -> auto-approves without touching the inner approver
	a.SetYOLO(true)
	if ok, _ := a.Approve(context.Background(), call, tools.RiskDestructive); !ok || inner.n != 1 {
		t.Errorf("yolo mode: ok=%v n=%d, want auto (no delegate)", ok, inner.n)
	}
	if !a.IsYOLO() {
		t.Error("IsYOLO should be true")
	}
	a.SetYOLO(false)
	if a.IsYOLO() {
		t.Error("IsYOLO should be false after off")
	}
}

func TestSettingsPageOpensAndEdits(t *testing.T) {
	m := testModel(t)
	m.cfg = &config.Config{
		ServerURL: "http://localhost:8089", Model: "m", ContextWindow: 16384,
		Path: "/tmp/fake.yaml", Skills: config.SkillsConfig{},
	}
	m.input.SetValue("/settings")
	if got, _ := m.submitLine(); got.(*tuiModel) != m {
		t.Fatal("submitLine returned a different model")
	}
	if !m.settingsOpen {
		t.Fatal("/settings did not open the page")
	}
	v := m.settingsView()
	if !strings.Contains(v, "Yagent settings") || !strings.Contains(v, "server_url") {
		t.Errorf("settingsView = %q", v)
	}
	// navigate down and start editing
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.editing {
		t.Fatal("enter did not start editing")
	}
	if !m.editInput.Focused() {
		t.Fatal("edit input is not focused (typing would be ignored)")
	}
	// typing updates the value
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mojeek")})
	if !strings.Contains(m.editInput.Value(), "mojeek") {
		t.Errorf("edit value = %q, want to contain mojeek", m.editInput.Value())
	}
	// esc closes editing
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.editing {
		t.Error("esc did not stop editing")
	}
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.settingsOpen {
		t.Error("esc did not close the settings page")
	}
}

func TestSettingsPageChooser(t *testing.T) {
	m := testModel(t)
	m.cfg = &config.Config{
		ServerURL: "http://x", Model: "m", ContextWindow: 16384,
		Path: filepath.Join(t.TempDir(), "config.yaml"),
		Web:  config.WebConfig{Provider: "duckduckgo"},
	}
	m.settingsOpen = true

	idx := -1
	for i, s := range config.Settings() {
		if s.Key == "web_search.provider" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("web_search.provider not in settings catalog")
	}
	m.settingsIdx = idx
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.choosing {
		t.Fatal("chooser did not open for a choice field")
	}
	if m.choosingIdx != 0 {
		t.Errorf("choosingIdx = %d, want 0 (duckduckgo)", m.choosingIdx)
	}
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyRight})
	if m.choosingIdx != 1 {
		t.Errorf("after right = %d, want 1 (mojeek)", m.choosingIdx)
	}
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.choosing {
		t.Error("chooser still open after enter")
	}
	if m.cfg.Web.Provider != "mojeek" {
		t.Errorf("provider = %q, want mojeek", m.cfg.Web.Provider)
	}
	// text fields still open the text editor
	m.settingsIdx = 0 // server_url
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.editing == false || m.choosing {
		t.Error("text field should open the text editor")
	}
}

type stubChatLLM struct{}

func (stubChatLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta func(string)) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "ok"}}, nil
}

func TestViewLayout(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.env.sessionID = "0123456789abcdef"
	m.cfg = &config.Config{Model: "qwen2.5-coder:14b"}
	m.workspace = "/tmp/ws"
	m.width, m.height = 100, 30
	m.branch = "main"
	m.busy = true
	m.turnTokens = 300
	m.toolCalls = 2

	v := m.View()
	for _, want := range []string{"YAGENT", "qwen2.5-coder:14b", "main", "16384"} {
		if !strings.Contains(v, want) {
			t.Errorf("View() missing %q in:\n%s", want, v)
		}
	}
	// command palette popover while typing a slash command
	m.busy = false
	m.input.SetValue("/skills")
	if !m.showPopover() {
		t.Fatal("popover should show while typing a slash command")
	}
	pv := m.popoverView()
	if !strings.Contains(pv, "/skills") {
		t.Errorf("popoverView = %q", pv)
	}
	// settings modal overlays the whole screen
	m.settingsOpen = true
	ov := m.View()
	if !strings.Contains(ov, "Yagent settings") {
		t.Errorf("settings modal missing in:\n%s", ov)
	}
	m.settingsOpen = false
	// sessions modal
	m.sessionsOpen = true
	if ov := m.View(); !strings.Contains(ov, "Sessions") {
		t.Errorf("sessions modal missing")
	}
}

func TestRenderMarkdown(t *testing.T) {
	out := renderMarkdown("hello **bold** world")
	if !strings.Contains(out, "hello") || !strings.Contains(out, "bold") {
		t.Errorf("markdown lost content: %q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("markdown output should carry ANSI styles: %q", out)
	}
	// malformed markdown (unclosed fence) must not panic; glamour degrades
	// to a blank line rather than echoing raw marker text
	if out := renderMarkdown("```"); len(out) == 0 {
		t.Errorf("unclosed fence returned empty output")
	}
}

func TestStatusPills(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	state, color := m.statusText()
	if !strings.Contains(state, "ready") {
		t.Errorf("idle state = %q", state)
	}
	_ = color
	// gauge over budget turns red
	gauge := m.ctxGauge(17000, 16384)
	if !strings.Contains(gauge, "103%") {
		t.Errorf("over-budget gauge = %q", gauge)
	}
	m.busy = true
	state, _ = m.statusText()
	if !strings.Contains(state, "working") {
		t.Errorf("busy state = %q", state)
	}
}

func TestSessionsBrowser(t *testing.T) {
	st, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := st.NewSession(context.Background(), "/tmp/ws")
	if _, err := st.Append(context.Background(), sess.ID, llm.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	m := testModel(t)
	m.env.st = st
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.input.SetValue("/sessions")
	m.submitLine()
	if !m.sessionsOpen {
		t.Fatal("/sessions did not open the browser")
	}
	if len(m.sessions) != 1 {
		t.Fatalf("sessions = %d", len(m.sessions))
	}
	v := m.sessionsView()
	if !strings.Contains(v, "Sessions") || !strings.Contains(v, sess.ID[:8]) {
		t.Errorf("sessionsView = %q", v)
	}
	// delete (twice to confirm)
	m.handleSessionsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !m.sessionsConfirm {
		t.Error("first d should arm confirmation")
	}
	m.handleSessionsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if len(m.sessions) != 0 {
		t.Errorf("session not deleted: %d remain", len(m.sessions))
	}
	// create a fresh session, re-open the browser and resume it
	sess2, _ := m.env.st.NewSession(context.Background(), "/tmp/ws")
	if _, err := m.env.st.Append(context.Background(), sess2.ID, llm.Message{Role: "user", Content: "resume me please"}); err != nil {
		t.Fatal(err)
	}
	m.sessions, _ = m.env.st.ListSessions(context.Background())
	m.sessionsOpen = true
	m.handleSessionsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if len(m.transcript) == 0 || !strings.Contains(strings.Join(m.transcript, "\n"), "resume me please") {
		t.Errorf("resume did not load history into the transcript: %q", m.transcript)
	}
	m.handleSessionsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessionsOpen {
		t.Error("esc should close the browser")
	}
}

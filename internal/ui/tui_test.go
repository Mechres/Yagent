package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/checkpoint"
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
	return &tuiModel{env: &chatEnv{sk: sk}, msgInput: newInput()}
}

func TestTabCyclesCommands(t *testing.T) {
	m := testModel(t)
	m.msgInput.SetValue("/")
	m.completeCommand()
	first := m.msgInput.Value()
	if first == "/" || !strings.HasPrefix(first, "/") {
		t.Fatalf("first completion = %q", first)
	}
	m.completeCommand()
	second := m.msgInput.Value()
	if second == first {
		t.Errorf("tab did not cycle: %q", second)
	}
	// an exact command must cycle to a different one (regression: stuck on
	// the first match)
	m.msgInput.SetValue("/exit")
	m.completeCommand()
	if m.msgInput.Value() == "/exit" {
		t.Errorf("tab stuck on /exit, should cycle to the next command")
	}
}

func TestSlashPaletteArrowNavigation(t *testing.T) {
	m := testModel(t)
	m.msgInput.SetValue("/c") // matches /clear, /compact, /checkpoint...
	// palette is open; up/down navigate the highlight, not the prompt history
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	sel := m.tabIndex
	if sel <= 0 {
		t.Errorf("down did not advance the palette selection (tabIndex=%d)", sel)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.tabIndex != (sel+1)%len(m.slashMatches()) {
		t.Errorf("tabIndex after second down = %d", m.tabIndex)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.tabIndex != sel {
		t.Errorf("up did not go back (tabIndex=%d, want %d)", m.tabIndex, sel)
	}
	// typing a key resets the selection (set to -1 = no active selection)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if m.tabIndex != -1 {
		t.Errorf("typing did not reset tabIndex: %d, want -1", m.tabIndex)
	}
}

func TestSlashAutoCompleteOnEnter(t *testing.T) {
	m := testModel(t)
	m.inputCh = make(chan turnRequest, 10)
	// /h matches exactly /help — enter auto-completes, then /help opens the
	// help modal (no agent needed, so no panic).
	m.msgInput.SetValue("/h")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.helpOpen {
		t.Errorf("/h did not auto-complete into /help and open the modal (helpOpen=%v)", m.helpOpen)
	}
	// a prefix matching several commands is NOT auto-completed and is NOT sent
	// to the model — it stays in the input so the palette remains open (this
	// fixes "/ex" being submitted to the model instead of completing).
	m.msgInput.SetValue("/ex")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.msgInput.Value() != "/ex" {
		t.Errorf("ambiguous /ex should stay in the input, got %q", m.msgInput.Value())
	}
	if m.busy {
		t.Error("/ex was submitted to the model (should be held for completion)")
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
	m.msgInput.SetValue("/smoke")
	matches := m.slashMatches()
	if len(matches) != 1 || matches[0] != "/smoke-test" {
		t.Errorf("matches = %v, want [/smoke-test]", matches)
	}
}

func TestRenderApprovalDiff(t *testing.T) {
	d := renderApprovalDiff(tokyoNight, "line one\nold line\nline three", "line one\nnew line\nline three")
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
	d := fsApprovalDiff(tokyoNight, ws, edit)
	if !strings.Contains(d, "- old content") || !strings.Contains(d, "+ new content") {
		t.Errorf("fs_edit diff = %q", d)
	}
	// path traversal -> no diff
	evil := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_edit",
		Arguments: []byte(`{"path":"../evil","old_string":"a","new_string":"b"}`)}}
	if d := fsApprovalDiff(tokyoNight, ws, evil); d != "" {
		t.Errorf("traversal should yield no diff, got %q", d)
	}
	// non-fs tool -> no diff
	other := llm.ToolCall{Function: llm.ToolCallFunction{Name: "shell_exec", Arguments: []byte(`{"command":"ls"}`)}}
	if d := fsApprovalDiff(tokyoNight, ws, other); d != "" {
		t.Errorf("non-fs tool should yield no diff, got %q", d)
	}
}

type recordingApprover struct{ n int }

func (r *recordingApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	r.n++
	return agent.Approval{OK: true}, nil
}

func TestToggleableApprover(t *testing.T) {
	inner := &recordingApprover{}
	a := newToggleableApprover(inner)
	call := llm.ToolCall{}
	// yolo off -> delegates
	if appr, _ := a.Approve(context.Background(), call, tools.RiskDestructive); !appr.OK || inner.n != 1 {
		t.Errorf("off mode: ok=%v n=%d, want delegate", appr.OK, inner.n)
	}
	// yolo on -> auto-approves without touching the inner approver
	a.SetYOLO(true)
	if appr, _ := a.Approve(context.Background(), call, tools.RiskDestructive); !appr.OK || inner.n != 1 {
		t.Errorf("yolo mode: ok=%v n=%d, want auto (no delegate)", appr.OK, inner.n)
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
	m.msgInput.SetValue("/settings")
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

func (stubChatLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
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
	m.msgInput.SetValue("/skills")
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
	out := renderMarkdown("hello **bold** world", 80)
	if !strings.Contains(out, "hello") || !strings.Contains(out, "bold") {
		t.Errorf("markdown lost content: %q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("markdown output should carry ANSI styles: %q", out)
	}
	// malformed markdown (unclosed fence) must not panic; glamour degrades
	// to a blank line rather than echoing raw marker text
	if out := renderMarkdown("```", 80); len(out) == 0 {
		t.Errorf("unclosed fence returned empty output")
	}
}

func TestThemeByName(t *testing.T) {
	if got := themeByName("tokyo"); got.Primary != tokyoNight.Primary {
		t.Errorf("tokyo primary = %v", got.Primary)
	}
	if got := themeByName("catppuccin"); got.Primary == tokyoNight.Primary {
		t.Error("catppuccin should differ from tokyo")
	}
	if got := themeByName("nord"); got.Background == tokyoNight.Background {
		t.Error("nord should differ from tokyo")
	}
	// unknown names fall back to tokyo (config validates, but be safe)
	if got := themeByName("beige"); got.Primary != tokyoNight.Primary {
		t.Error("unknown theme should fall back to tokyo")
	}
}

func TestThemeRendersDistinctPalettes(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.ANSI256) })
	seen := map[string]string{}
	for _, name := range config.ThemeOptions {
		// tuiModel now embeds a sync.RWMutex (runtime swap guard), so it must
		// not be copied by value — build a fresh model with just the header
		// fields and the theme under test.
		m := testModel(t)
		m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
		m.cfg = &config.Config{Model: "m"}
		m.width, m.height = 100, 30
		m.branch = "main"
		m.th = themeByName(name)
		// extract the primary RGB from the rendered header (first color sequence)
		out := m.headerView()
		seen[name] = out
	}
	// every theme must render a non-empty header with its own primary color
	primaries := map[string]string{}
	for _, name := range config.ThemeOptions {
		v := seen[name]
		if v == "" {
			t.Fatalf("theme %s rendered empty header", name)
		}
		// primary is the title pill background: look for the 48;2;r;g;b escape
		idx := strings.Index(v, "48;2;")
		if idx < 0 {
			t.Fatalf("theme %s header has no truecolor bg: %q", name, v)
		}
		end := strings.Index(v[idx:], "m")
		primaries[name] = v[idx : idx+end]
	}
	if primaries["tokyo"] == primaries["catppuccin"] || primaries["tokyo"] == primaries["nord"] {
		t.Errorf("theme primaries collide: %v", primaries)
	}
}

func TestThemeSwitchAppliesLive(t *testing.T) {
	m := testModel(t)
	m.cfg = &config.Config{
		ServerURL: "http://x", Model: "m", ContextWindow: 16384,
		Path:  filepath.Join(t.TempDir(), "config.yaml"),
		Theme: "tokyo",
	}
	m.settingsOpen = true
	m.th = themeByName("tokyo")

	idx := -1
	for i, s := range config.Settings() {
		if s.Key == "theme" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("theme not in settings catalog")
	}
	m.settingsIdx = idx
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEnter}) // open chooser
	if !m.choosing {
		t.Fatal("theme chooser did not open")
	}
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyRight}) // tokyo -> catppuccin
	m.handleSettingsKey(tea.KeyMsg{Type: tea.KeyEnter}) // save
	if m.choosing {
		t.Fatal("chooser still open")
	}
	if m.cfg.Theme != "catppuccin" {
		t.Errorf("cfg.Theme = %q, want catppuccin", m.cfg.Theme)
	}
	if m.th.Primary == tokyoNight.Primary {
		t.Error("m.th did not switch live (still tokyo)")
	}
	if m.th.Primary != catppuccinMocha.Primary {
		t.Error("m.th should now be catppuccin")
	}
}

func TestRenderMarkdownWrapsLongTokens(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.ANSI256) })
	long := strings.Repeat("x", 200)
	out := renderMarkdown("prefix "+long, 40)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Errorf("line width %d > 40: %q…", w, line[:20])
		}
	}
}

func TestStreamTailWraps(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.ANSI256) })
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.workspace = "/home/u/projects/workspace"
	m.env = &chatEnv{}
	m.env.sessionID = "0123456789abcdef"
	m.branch = "main"
	m.yoloToggler = newToggleableApprover(&recordingApprover{})
	m.yoloToggler.SetYOLO(true)
	m.width, m.height = 40, 20
	m.stream.WriteString(strings.Repeat("abc ", 50)) // 200 chars of live stream
	m.syncViewport()                                 // stream lives inside the viewport
	for _, line := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Errorf("viewport line width %d > 40: %q…", w, line[:20])
		}
	}
	// the pill bars themselves must drop pills on a narrow window
	if w := lipgloss.Width(m.headerView()); w > 40 {
		t.Errorf("header width %d > 40", w)
	}
	if w := lipgloss.Width(m.statusView()); w > 40 {
		t.Errorf("status width %d > 40", w)
	}
}

func TestFsPatchApprovalPreview(t *testing.T) {
	ws := t.TempDir()
	patch := `--- a/a.go
+++ b/a.go
@@ -1,2 +1,2 @@
 func old() int {
-    return 1
+    return 2
 }`
	args, _ := json.Marshal(map[string]string{"patch": patch})
	call := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_patch", Arguments: args}}
	d := fsApprovalDiff(tokyoNight, ws, call)
	ansi := ansiStrip(d)
	if !strings.Contains(ansi, "+    return 2") || !strings.Contains(ansi, "-    return 1") || !strings.Contains(ansi, "@@") {
		t.Errorf("fs_patch preview = %q", ansi)
	}
	// empty patch -> explicit marker
	empty := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_patch", Arguments: []byte(`{"patch":""}`)}}
	if got := ansiStrip(fsApprovalDiff(tokyoNight, ws, empty)); !strings.Contains(got, "empty patch") {
		t.Errorf("empty patch preview = %q", got)
	}
}

func TestHunkReviewFiltersPatch(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24

	patch := `--- a/a.go
+++ b/a.go
@@ -1,1 +1,1 @@
-func first() {}
+func first2() {}
@@ -5,1 +5,1 @@
-func second() {}
+func second2() {}
`
	args, _ := json.Marshal(map[string]string{"patch": patch})
	call := llm.ToolCall{Function: llm.ToolCallFunction{Name: "fs_patch", Arguments: args}}
	respond := make(chan agent.Approval, 1)

	// deliver the approval request like the agent runner would
	m.handleIncomingForTest(approvalRequestMsg{call: call, risk: tools.RiskWrite, respond: respond})
	if !m.hunkOpen {
		t.Fatal("hunk review did not start")
	}
	if len(m.hunkHunks) != 2 {
		t.Fatalf("hunks = %d", len(m.hunkHunks))
	}
	// accept hunk 1, skip hunk 2 -> review finishes with a filtered patch
	m.handleHunkKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if !m.hunkOpen || m.hunkIdx != 1 {
		t.Fatal("review should advance to hunk 2")
	}
	m.handleHunkKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.hunkOpen {
		t.Fatal("review should finish after last hunk")
	}
	select {
	case appr := <-respond:
		if !appr.OK {
			t.Fatal("expected approval")
		}
		got := argsPatch(llm.ToolCall{Function: llm.ToolCallFunction{Arguments: appr.Args}})
		if strings.Contains(got, "second2") {
			t.Errorf("skipped hunk leaked: %q", got)
		}
		if !strings.Contains(got, "first2") {
			t.Errorf("accepted hunk missing: %q", got)
		}
	default:
		t.Fatal("no approval sent")
	}
}

// handleIncomingForTest routes an approval request into the model's Update.
func (m *tuiModel) handleIncomingForTest(msg tea.Msg) {
	m.Update(msg)
}

func TestReasoningToggleAndCap(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m", UI: config.UIConfig{ShowReasoning: false}}
	m.width, m.height = 80, 24

	m.Update(reasoningMsg{delta: "secret thinking"})
	if m.reasoning != "" {
		t.Error("reasoning buffered even though ui.show_reasoning is off")
	}

	// with it on, the buffer is capped
	m.cfg.UI.ShowReasoning = true
	long := strings.Repeat("x", reasoningCap+100)
	m.Update(reasoningMsg{delta: long})
	if !m.reasoningTruncated {
		t.Error("reasoning not marked truncated")
	}
	if len(m.reasoning) > reasoningCap+len("[…] earlier reasoning omitted\n") {
		t.Errorf("reasoning buffer over cap: %d", len(m.reasoning))
	}
	if !strings.Contains(m.reasoning, "earlier reasoning omitted") {
		t.Errorf("truncation marker missing: %q", m.reasoning[:60])
	}
}

func TestMouseToggle(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24
	if m.mouseOn {
		t.Fatal("mouse must start off so drag-select works")
	}
	m.toggleMouse()
	if !m.mouseOn {
		t.Fatal("toggle should enable mouse")
	}
	if st, _ := m.statusText(); !strings.Contains(st, "mouse") {
		t.Error("status should flag mouse capture when on")
	}
	m.toggleMouse()
	if m.mouseOn {
		t.Fatal("toggle should disable mouse")
	}
	if st, _ := m.statusText(); strings.Contains(st, "mouse") {
		t.Error("status should clear the mouse marker when off")
	}
}

func TestThinkingClickToggles(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m", UI: config.UIConfig{ShowReasoning: true}}
	m.width, m.height = 80, 24

	m.Update(reasoningMsg{delta: "hidden reasoning text"})
	// collapsed: header at content line 0, screen row 1 (below the header bar)
	if m.thinkingExpanded {
		t.Fatal("should start collapsed")
	}
	m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: 1})
	if !m.thinkingExpanded {
		t.Fatal("click on the header should expand")
	}
	if !strings.Contains(ansiStrip(m.View()), "hidden reasoning text") {
		t.Error("expanded content not visible after click")
	}
	// click again collapses
	m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: 1})
	if m.thinkingExpanded {
		t.Fatal("second click should collapse")
	}
	// clicking the answer area (below the thinking block) must NOT toggle
	m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: 5})
	if m.thinkingExpanded {
		t.Error("click outside the thinking block toggled it")
	}
}

func TestReasoningDisplay(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m", UI: config.UIConfig{ShowReasoning: true}}
	m.width, m.height = 80, 24
	m.branch = "main"

	// reasoning streams first -> shown as a "thinking" block, not the answer.
	// Collapsed by default: the header is visible, the content is not.
	m.Update(reasoningMsg{delta: "let me think about this carefully"})
	v := ansiStrip(m.View())
	if !strings.Contains(v, "thinking") {
		t.Errorf("thinking header missing from view: %q", v)
	}
	if strings.Contains(v, "carefully") {
		t.Errorf("collapsed thinking should not show content: %q", v)
	}
	// toggle the live block -> expanded content appears
	m.toggleThinking()
	if !strings.Contains(ansiStrip(m.View()), "carefully") {
		t.Errorf("expanded thinking should show content")
	}
	// flush commits it to the transcript (expanded) and NOT as answer content
	m.stream.WriteString("final answer here")
	m.flushStream()
	joined := ansiStrip(strings.Join(m.transcript, "\n"))
	if !strings.Contains(joined, "carefully") || !strings.Contains(joined, "final answer here") {
		t.Errorf("flush transcript = %q", joined)
	}
	if m.reasoning != "" || m.stream.Len() != 0 {
		t.Error("buffers not reset after flush")
	}
	// the committed block toggles in place: collapse -> header only
	m.toggleThinking()
	if strings.Contains(ansiStrip(strings.Join(m.transcript, "\n")), "carefully") {
		t.Error("collapse did not remove the thinking text")
	}
	// re-expand
	m.toggleThinking()
	if !strings.Contains(ansiStrip(strings.Join(m.transcript, "\n")), "carefully") {
		t.Error("expand did not inline the thinking text")
	}
	if !m.thinkingOpen || m.lastThinkIdx < 0 {
		t.Error("committed thinking block should remain toggleable")
	}
}

// ansiStrip removes ANSI escape sequences so transcript assertions can match
// plain text (glamour output is styled).
func ansiStrip(s string) string {
	return regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(s, "")
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

func TestTranscriptFind(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24
	m.viewport = viewport.New(80, 20)
	m.viewport.KeyMap.Up.SetEnabled(false)
	m.viewport.KeyMap.Down.SetEnabled(false)

	m.transcript = []string{
		"> explain the budget",
		"the budget summarizes old history",
		"> where does budget live",
		"budget is in agent.go",
	}
	m.refreshViewport()

	// Ctrl+F opens find; typing narrows matches (case-insensitive)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	if !m.findOpen {
		t.Fatal("ctrl+f did not open find")
	}
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("budget")})
	if len(m.findMatches) != 4 {
		t.Fatalf("matches = %d, want 4", len(m.findMatches))
	}
	// findView shows the match position
	if v := ansiStrip(m.findView()); !strings.Contains(v, "1/4") {
		t.Errorf("findView = %q, want 1/4", v)
	}
	// enter cycles to the next match
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.findMatch != 1 {
		t.Errorf("findMatch = %d, want 1 after enter", m.findMatch)
	}
	// no matches state
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz")})
	if len(m.findMatches) != 0 {
		t.Errorf("matches = %d, want 0", len(m.findMatches))
	}
	if !strings.Contains(ansiStrip(m.findView()), "no matches") {
		t.Errorf("findView = %q", ansiStrip(m.findView()))
	}
	// backspace restores matches
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyBackspace})
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyBackspace})
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(m.findMatches) == 0 {
		t.Error("backspace should have restored matches")
	}
	// esc closes and hands back to the input
	m.handleFindKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.findOpen {
		t.Fatal("esc did not close find")
	}
	// the find bar is gone from the view, the input line is back
	if v := ansiStrip(m.View()); strings.Contains(v, "find:") || !strings.Contains(v, "message (enter") {
		t.Errorf("find bar not cleared from view:\n%s", v)
	}
}

func TestRepeatLoopDetector(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"short": false,
		strings.Repeat("I need to check the file. ", 3):              true,  // 24-char unit ×3
		strings.Repeat("still verifying the output here ", 4):        true,  // 33-char unit
		strings.Repeat("x", 300):                                     true,  // mono run
		"the quick brown fox jumps over the lazy dog and back again": false, // no repetition
	}
	for in, want := range cases {
		if got := agent.RepeatLoop(in); got != want {
			t.Errorf("agent.RepeatLoop(%q…) = %v, want %v", in[:min(len(in), 40)], got, want)
		}
	}
}

func TestEscCancelsRunningTurn(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24

	// a running turn can be cancelled, keeping the session alive
	m.busy = true
	_, cancel := context.WithCancel(context.Background())
	m.turnCancel = cancel
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.turnCancelled {
		t.Fatal("esc did not mark the turn cancelled")
	}
	if !strings.Contains(m.cancelReason, "cancelled") {
		t.Errorf("cancelReason = %q", m.cancelReason)
	}
	// esc when idle must not cancel anything
	m.busy = false
	m.turnCancelled = false
	m.turnCancel = cancel
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.turnCancelled {
		t.Error("esc when idle should not cancel")
	}
}

func TestLoopGuardRetriesWithRepetitionPenalty(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.client = llm.NewClient("http://x", "m")
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24
	m.inputCh = make(chan turnRequest, 4)
	m.busy = true
	m.lastTurnText = "the original input"
	m.turnCancelled = true
	m.cancelReason = "stopped: the model was repeating itself (thinking loop)"
	_, cancel := context.WithCancel(context.Background())
	m.turnCancel = cancel
	m.turnSeq = 1

	m.handleIncomingForTest(turnDoneMsg{seq: 1, err: context.Canceled})
	select {
	case req := <-m.inputCh:
		if req.text != "the original input" {
			t.Errorf("retry text = %q", req.text)
		}
	default:
		t.Fatal("loop guard did not resubmit a retry")
	}
	if m.client.Sampling.RepetitionPenalty != 1.05 {
		t.Errorf("repetition penalty = %v, want 1.05", m.client.Sampling.RepetitionPenalty)
	}
	if m.retriedLoop != true {
		t.Error("retriedLoop not set")
	}

	// an explicit esc cancel must NOT auto-retry
	m2 := testModel(t)
	m2.client = llm.NewClient("http://x", "m")
	m2.inputCh = make(chan turnRequest, 4)
	m2.busy = true
	m2.lastTurnText = "x"
	m2.turnCancelled = true
	m2.cancelReason = "turn cancelled (esc) — send another message"
	_, cancel2 := context.WithCancel(context.Background())
	m2.turnCancel = cancel2
	m2.turnSeq = 1
	m2.handleIncomingForTest(turnDoneMsg{seq: 1, err: context.Canceled})
	select {
	case <-m2.inputCh:
		t.Error("esc cancel should not auto-retry")
	default:
	}
}

func TestLoopGuardCancelsRepeatingTurn(t *testing.T) {
	rep := strings.Repeat("I need to check the file. ", 3)

	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m", UI: config.UIConfig{ShowReasoning: true, LoopGuard: true}}
	m.width, m.height = 80, 24
	m.busy = true
	_, cancel := context.WithCancel(context.Background())
	m.turnCancel = cancel
	m.Update(reasoningMsg{delta: rep})
	if !m.turnCancelled {
		t.Fatal("loop guard did not cancel a repeating turn")
	}
	if !strings.Contains(m.cancelReason, "repeating") {
		t.Errorf("cancelReason = %q", m.cancelReason)
	}

	// ui.loop_guard off -> no auto-cancel
	m2 := testModel(t)
	m2.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m2.cfg = &config.Config{Model: "m", UI: config.UIConfig{ShowReasoning: true, LoopGuard: false}}
	m2.width, m2.height = 80, 24
	m2.busy = true
	_, cancel2 := context.WithCancel(context.Background())
	m2.turnCancel = cancel2
	m2.Update(reasoningMsg{delta: rep})
	if m2.turnCancelled {
		t.Error("loop guard off still cancelled the turn")
	}
}

func TestSkillsModalApproveAndClose(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24
	content := "---\nname: smoke\ndescription: smoke skill\n---\n## When to Use\nx\n## Procedure\ny\n## Verification\ncheck\n"
	if _, _, err := m.env.sk.Stage(skills.Op{Action: skills.ActionCreate, Name: "smoke", Content: content}); err != nil {
		t.Fatal(err)
	}

	m.msgInput.SetValue("/skills")
	m.submitLine()
	if !m.skillsOpen {
		t.Fatal("/skills did not open the modal")
	}
	if len(m.skills) != 1 || m.skills[0].Name != "smoke" {
		t.Fatalf("skills = %+v", m.skills)
	}
	v := ansiStrip(m.skillsView())
	if !strings.Contains(v, "Pending skill writes") || !strings.Contains(v, "smoke") {
		t.Errorf("skillsView = %q", v)
	}
	// diff action populates the message pane
	m.handleSkillsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if m.skillsMsg == "" {
		t.Error("diff action left skillsMsg empty")
	}
	// approve removes the staged write
	m.handleSkillsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if len(m.skills) != 0 {
		t.Errorf("skills after approve = %d, want 0", len(m.skills))
	}
	// esc closes
	m.handleSkillsKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.skillsOpen {
		t.Fatal("esc did not close the modal")
	}
}

func TestClarifyModal(t *testing.T) {
	m := testModel(t)
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 24
	respond := make(chan string, 1)
	m.handleIncomingForTest(clarifyRequestMsg{question: "which target?", choices: []string{"linux", "mac"}, respond: respond})
	if !m.clarifyOpen || m.clarifyFree {
		t.Fatal("chooser modal did not open")
	}
	// down -> pick "mac", enter confirms
	m.handleClarifyKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleClarifyKey(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case ans := <-respond:
		if ans != "mac" {
			t.Errorf("answer = %q, want mac", ans)
		}
	default:
		t.Fatal("no answer delivered")
	}
	if m.clarifyOpen {
		t.Error("modal still open after answering")
	}

	// no choices -> free text
	m2 := testModel(t)
	m2.cfg = &config.Config{Model: "m"}
	m2.width, m2.height = 80, 24
	respond2 := make(chan string, 1)
	m2.handleIncomingForTest(clarifyRequestMsg{question: "describe the issue", respond: respond2})
	if !m2.clarifyFree {
		t.Fatal("no-choice question should open free text")
	}
	m2.clarifyAnswerInput.SetValue("the parser crashes")
	m2.handleClarifyKey(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case ans := <-respond2:
		if ans != "the parser crashes" {
			t.Errorf("free answer = %q", ans)
		}
	default:
		t.Fatal("no free answer delivered")
	}

	// esc cancels with an empty answer (agent treats it as no-answer)
	m3 := testModel(t)
	m3.cfg = &config.Config{Model: "m"}
	respond3 := make(chan string, 1)
	m3.handleIncomingForTest(clarifyRequestMsg{question: "q", choices: []string{"a"}, respond: respond3})
	m3.handleClarifyKey(tea.KeyMsg{Type: tea.KeyEsc})
	select {
	case ans := <-respond3:
		if ans != "(no answer)" {
			t.Errorf("cancel answer = %q", ans)
		}
	default:
		t.Fatal("no cancel answer delivered")
	}
}

func TestRetryCommand(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.client = llm.NewClient("http://x", "m")
	m.inputCh = make(chan turnRequest, 4)
	m.lastTurnText = "the failing input"
	m.width, m.height = 80, 24

	m.msgInput.SetValue("/retry")
	m.submitLine()
	select {
	case req := <-m.inputCh:
		if req.text != "the failing input" {
			t.Errorf("retry text = %q", req.text)
		}
	default:
		t.Fatal("retry did not resubmit")
	}
	if m.client.Sampling.Temperature != 0.3 || m.client.Sampling.RepetitionPenalty != 1.05 {
		t.Errorf("retry sampling = %+v", m.client.Sampling)
	}

	// nothing to retry -> no turn starts
	m2 := testModel(t)
	m2.cfg = &config.Config{Model: "m"}
	m2.width, m2.height = 80, 24
	m2.msgInput.SetValue("/retry")
	m2.submitLine()
	if m2.busy {
		t.Error("retry with nothing should not start a turn")
	}
}

func TestStatusTokensPerSecond(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.busy = true
	m.turnTokens = 300
	m.turnStart = time.Now().Add(-10 * time.Second)
	state, _ := m.statusText()
	if !strings.Contains(state, "30.0 t/s") {
		t.Errorf("status = %q, want a 30.0 t/s reading", state)
	}
}

func TestMsgInputHandlesMultilinePaste(t *testing.T) {
	m := testModel(t)
	m.msgInput = newInput()
	m.msgInput.SetWidth(40)
	m.msgInput.Focus()
	m.th = themeByName("tokyo")

	// a multi-line paste (newlines in the runes) must keep its lines, not
	// flatten horizontally.
	updated, _ := m.msgInput.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line one\nline two\nline three")})
	m.msgInput = updated
	if got := m.msgInput.Value(); !strings.Contains(got, "\n") {
		t.Errorf("paste lost newlines: %q", got)
	}
	if h := m.inputHeight(); h < 3 {
		t.Errorf("inputHeight = %d, want >= 3 for a three-line paste", h)
	}

	// a long single line wraps to multiple rows instead of overflowing.
	m2 := testModel(t)
	m2.msgInput = newInput()
	m2.msgInput.SetWidth(40)
	m2.th = themeByName("tokyo")
	m2.msgInput.SetValue(strings.Repeat("x", 200))
	if h := m2.inputHeight(); h < 3 {
		t.Errorf("long line inputHeight = %d, want wrapping", h)
	}
}

func TestLayoutShrinksForMultilineInput(t *testing.T) {
	m := testModel(t)
	m.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	m.cfg = &config.Config{Model: "m"}
	m.width, m.height = 80, 30
	m.msgInput = newInput()
	m.msgInput.SetWidth(76)
	m.msgInput.SetValue("one\ntwo\nthree")
	m.th = themeByName("tokyo")

	if h := m.inputHeight(); h < 3 {
		t.Errorf("inputHeight = %d, want >= 3 for three lines", h)
	}
	multi := m.layoutHeight()
	m.msgInput.SetValue("single")
	single := m.layoutHeight()
	if multi >= single {
		t.Errorf("multi-line input should shrink the viewport: multi=%d single=%d", multi, single)
	}
}

func TestOfferDistillationGatedOnWork(t *testing.T) {
	// no tool work in history -> distillation is skipped entirely (if it ran,
	// the stub model would append the distillation prompt as a user message).
	ag := agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 3}, t.TempDir())
	offerDistillation(context.Background(), ag, &chatEnv{}, &bytes.Buffer{})
	if len(ag.History()) != 0 {
		t.Errorf("distillation ran despite no tool work: history = %d messages", len(ag.History()))
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
	m.msgInput.SetValue("/sessions")
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

func TestPromptHistoryNavigation(t *testing.T) {
	m := testModel(t)
	m.inputCh = make(chan turnRequest, 10)

	// submit two inputs
	m.msgInput.SetValue("first command")
	m.submitLine()
	m.msgInput.SetValue("second command")
	m.submitLine()

	if len(m.history) != 2 {
		t.Fatalf("history len = %d, want 2", len(m.history))
	}

	// input is currently empty draft; pressing up loads "second command"
	m.msgInput.SetValue("my draft")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.msgInput.Value() != "second command" {
		t.Errorf("got %q, want 'second command'", m.msgInput.Value())
	}

	// pressing up again loads "first command"
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.msgInput.Value() != "first command" {
		t.Errorf("got %q, want 'first command'", m.msgInput.Value())
	}

	// pressing down loads "second command"
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.msgInput.Value() != "second command" {
		t.Errorf("got %q, want 'second command'", m.msgInput.Value())
	}

	// pressing down again restores the draft
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.msgInput.Value() != "my draft" {
		t.Errorf("got %q, want 'my draft'", m.msgInput.Value())
	}
}

func TestHelpModal(t *testing.T) {
	m := testModel(t)

	// /help opens modal
	m.msgInput.SetValue("/help")
	m.submitLine()
	if !m.helpOpen {
		t.Fatal("/help did not open help modal")
	}
	view := m.helpView()
	if !strings.Contains(view, "Keyboard Shortcuts") || !strings.Contains(view, "Slash Commands") {
		t.Errorf("helpView = %q", view)
	}

	// esc closes
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.helpOpen {
		t.Error("esc did not close help modal")
	}

	// ? on empty input opens modal
	m.msgInput.SetValue("")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if !m.helpOpen {
		t.Error("? did not open help modal")
	}
}

func TestCheckpointsModal(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "file.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Save(ws, "snap1"); err != nil {
		t.Fatal(err)
	}

	m := testModel(t)
	m.workspace = ws
	m.msgInput.SetValue("/checkpoint")
	m.submitLine()
	if !m.checkpointsOpen {
		t.Fatal("/checkpoint did not open checkpoints modal")
	}
	if len(m.checkpoints) != 1 || m.checkpoints[0].Name != "snap1" {
		t.Fatalf("checkpoints = %+v", m.checkpoints)
	}
	view := m.checkpointsView()
	if !strings.Contains(view, "snap1") {
		t.Errorf("checkpointsView = %q", view)
	}

	// modify file
	_ = os.WriteFile(filepath.Join(ws, "file.txt"), []byte("v2"), 0o644)

	// restore via 'r'
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.checkpointsOpen {
		t.Error("restore should close checkpoints modal")
	}
	data, _ := os.ReadFile(filepath.Join(ws, "file.txt"))
	if string(data) != "v1" {
		t.Errorf("restored content = %q, want v1", data)
	}

	// delete via 'd' (confirm)
	m.openCheckpointsModal()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !m.checkpointsConfirm {
		t.Error("first d should arm confirmation")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if len(m.checkpoints) != 0 {
		t.Errorf("checkpoint not deleted, remain = %d", len(m.checkpoints))
	}
}

func TestQuickSaveSessionShortcut(t *testing.T) {
	st, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, _ := st.NewSession(context.Background(), "/tmp/ws")
	_, _ = st.Append(context.Background(), sess.ID, llm.Message{Role: "user", Content: "quick save test"})

	m := testModel(t)
	m.env.st = st
	m.env.sessionID = sess.ID

	// press ctrl+s
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	path := "session-" + sess.ID + ".md"
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session file not saved: %v", err)
	}
	if !strings.Contains(string(data), "quick save test") {
		t.Errorf("saved markdown content = %q", data)
	}
}

func TestLCSDiffHunks(t *testing.T) {
	oldText := "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10"
	newText := "line 1\nline 2\nline 3\nline 4\nMODIFIED 5\nline 6\nline 7\nline 8\nline 9\nline 10"

	diff := renderApprovalDiff(tokyoNight, oldText, newText)
	if !strings.Contains(diff, "- line 5") || !strings.Contains(diff, "+ MODIFIED 5") {
		t.Fatalf("diff missing change: %q", diff)
	}
	// should contain surrounding context lines (line 3, line 4, line 6, line 7)
	if !strings.Contains(diff, "line 3") || !strings.Contains(diff, "line 7") {
		t.Errorf("diff missing context lines: %q", diff)
	}
}

func TestModelSelectorOpenAndNavigate(t *testing.T) {
	m := testModel(t)
	m.cfg = &config.Config{
		ServerURL: "http://localhost:8089", Model: "Qwen3VL-8B-Instruct-Q4_K_M.gguf",
		ContextWindow: 16384, Path: filepath.Join(t.TempDir(), "config.yaml"),
	}
	m.busy = false
	m.msgInput.SetValue("/model")
	got, _ := m.submitLine()
	if got.(*tuiModel) != m {
		t.Fatal("submitLine returned a different model")
	}
	if !m.modelOpen {
		t.Fatal("/model did not open the selector")
	}
	// starts on the provider pane, pre-selected to the matching local provider
	if m.modelProvider != 0 || m.modelOnModels {
		t.Errorf("initial state: provider=%d onModels=%v", m.modelProvider, m.modelOnModels)
	}
	// navigate to the OpenCode Zen provider (index 2 in the catalog)
	for i := 0; i < 2; i++ {
		m.handleModelKey(tea.KeyMsg{Type: tea.KeyRight})
	}
	if m.modelProvider != 2 {
		t.Errorf("provider = %d, want 2 (OpenCode Zen)", m.modelProvider)
	}
	// enter moves to the model pane
	m.handleModelKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.modelOnModels {
		t.Fatal("enter did not move to the model pane")
	}
	// right cycles the model list
	m.handleModelKey(tea.KeyMsg{Type: tea.KeyRight})
	if m.modelModelIdx != 1 {
		t.Errorf("modelIdx = %d, want 1 (deepseek-v4-flash)", m.modelModelIdx)
	}
	// esc closes without applying
	m.handleModelKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelOpen {
		t.Error("esc did not close the selector")
	}
	// the view renders both panes
	m.modelOpen = true
	v := m.modelView()
	if !strings.Contains(v, "Provider") || !strings.Contains(v, "Model") || !strings.Contains(v, "OpenCode Zen") {
		t.Errorf("modelView = %q", v)
	}
}

func TestModelSelectorRefusesWhileBusy(t *testing.T) {
	m := testModel(t)
	m.busy = true
	m.openModelSelector()
	if m.modelOpen {
		t.Error("selector opened while a turn was running")
	}
}

func TestModelSelectorKeyEntryForCloudProvider(t *testing.T) {
	m := testModel(t)
	dir := t.TempDir()
	m.cfg = &config.Config{
		ServerURL: "http://localhost:8089", Model: "m", ContextWindow: 16384,
		Path: filepath.Join(dir, "config.yaml"),
	}
	m.busy = false
	m.modelProvider = 2 // OpenCode Zen (has a KeyEnv, no key set)
	m.modelModelIdx = 0
	m.modelConfirm = true

	// confirming with no key must open key-entry instead of applying
	m.handleModelKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.modelKeyEntry {
		t.Fatal("confirm with no key did not open key entry")
	}
	// typing updates the input value (the enter+apply path needs a full env,
	// so we assert the input and the persist primitive instead of pressing enter)
	for _, r := range []rune("sk-test-123") {
		m.handleModelKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.modelKeyInput.Value(); got != "sk-test-123" {
		t.Errorf("key input = %q", got)
	}
	// esc abandons entry without touching config
	m.handleModelKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelKeyEntry {
		t.Error("esc did not close key entry")
	}
	// and the persist primitive writes api_key to the config file
	if err := config.Set(m.cfg.Path, "api_key", "sk-test-123"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sk-test-123") {
		t.Errorf("api_key not persisted:\n%s", data)
	}
}

func TestModelKeyEntryViewRenders(t *testing.T) {
	m := testModel(t)
	m.modelOpen = true
	m.modelProvider = 2 // OpenCode Zen
	m.modelKeyEntry = true
	m.modelKeyInput = textinput.New()
	m.modelKeyInput.Focus()
	v := m.modelView()
	if !strings.Contains(v, "API key for") || !strings.Contains(v, "OPENCODE_ZEN_API_KEY") {
		t.Errorf("key-entry view missing prompt: %q", v)
	}
}

func TestDiffModalOpenAndRender(t *testing.T) {
	m := testModel(t)
	m.cfg = &config.Config{ServerURL: "http://x", Model: "m", ContextWindow: 16384}
	// a git-backed env with a session baseline
	ws := t.TempDir()
	gitInit(t, ws)
	m.env.gitEnabled = true
	m.env.gitBaseline = "HEAD"
	m.workspace = ws
	m.openDiffModal()
	if !m.diffOpen {
		t.Fatal("diff modal did not open")
	}
	v := m.diffView()
	if !strings.Contains(v, "Session diff") {
		t.Errorf("diffView = %q", v)
	}
	// esc closes
	m.handleDiffKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.diffOpen {
		t.Error("esc did not close diff modal")
	}
}

func gitInit(t *testing.T, ws string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = ws
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

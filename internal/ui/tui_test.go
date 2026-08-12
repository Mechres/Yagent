package ui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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
	base := testModel(t)
	base.ag = agent.New(stubChatLLM{}, tools.NewRegistry(t.TempDir(), tools.Options{}), nil, agent.Config{MaxIterations: 1}, t.TempDir())
	base.cfg = &config.Config{Model: "m"}
	base.width, base.height = 100, 30
	base.branch = "main"

	seen := map[string]string{}
	for _, name := range config.ThemeOptions {
		m := *base
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
		if got := repeatLoop(in); got != want {
			t.Errorf("repeatLoop(%q…) = %v, want %v", in[:min(len(in), 40)], got, want)
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

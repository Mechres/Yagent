package ui

import "github.com/charmbracelet/lipgloss"

// Theme is a 24-bit color palette shared by every TUI component. Tokyo Night
// inspired: dark slate backgrounds, ice-blue accents, semantic status colors.
type Theme struct {
	Primary    lipgloss.Color // accents, active markers
	Secondary  lipgloss.Color // highlights, secondary accents
	Accent     lipgloss.Color // cyan tints
	Background lipgloss.Color // app background
	Surface    lipgloss.Color // panel fills, selected rows
	Muted      lipgloss.Color // dim text, hints
	Border     lipgloss.Color // subtle borders
	Foreground lipgloss.Color // body text
	Success    lipgloss.Color // approved / ok
	Warning    lipgloss.Color // tool calls, near-budget
	Error      lipgloss.Color // rejected / over-budget / high risk
}

// tokyoNight is the default palette.
var tokyoNight = Theme{
	Primary:    lipgloss.Color("#7aa2f7"),
	Secondary:  lipgloss.Color("#bb9af7"),
	Accent:     lipgloss.Color("#7dcfff"),
	Background: lipgloss.Color("#1a1b26"),
	Surface:    lipgloss.Color("#24283b"),
	Muted:      lipgloss.Color("#565f89"),
	Border:     lipgloss.Color("#3b4261"),
	Foreground: lipgloss.Color("#c0caf5"),
	Success:    lipgloss.Color("#9ece6a"),
	Warning:    lipgloss.Color("#e0af68"),
	Error:      lipgloss.Color("#f7768e"),
}

// Icons: emoji render on virtually every terminal emulator; these replace the
// ASCII markers throughout the TUI (kept short so the layout stays stable).
const (
	iconAgent   = "🤖"
	iconFolder  = "📁"
	iconSession = "💬"
	iconCtx     = "🧠"
	iconTool    = "🛠"
	iconOK      = "✅"
	iconBad     = "❌"
	iconWarn    = "⚠"
	iconGear    = "⚙"
	iconYOLO    = "⚡"
	iconBranch  = "🌿"
	iconCommand = "/"
)

// style helpers shared across views.
func (th Theme) pill(bg, fg lipgloss.Color, bold bool) lipgloss.Style {
	s := lipgloss.NewStyle().
		Background(bg).
		Foreground(fg).
		Padding(0, 1)
	if bold {
		s = s.Bold(true)
	}
	return s
}

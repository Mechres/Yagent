package ui

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// renderMarkdown formats an assistant message body for the transcript, wrapping
// at the given column width. glamour's own word wrap breaks prose but not long
// unbreakable tokens (URLs, code), so over-wide lines are hard-wrapped after.
// Falls back to the raw text when rendering fails.
func renderMarkdown(body string, width int) string {
	if width <= 0 {
		width = 80
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return body
	}
	out, err := r.Render(body)
	if err != nil {
		return body
	}
	return hardWrap(out, width)
}

// hardWrap breaks any line wider than width (ANSI-aware via lipgloss), leaving
// already-fitting lines untouched so markdown block styling is preserved.
func hardWrap(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = lipgloss.NewStyle().Width(width).Render(ln)
		}
	}
	return strings.Join(lines, "\n")
}

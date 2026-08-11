package ui

import "github.com/charmbracelet/glamour"

// renderMarkdown formats an assistant message body for the transcript. Falls
// back to the raw text when rendering fails (glamour keeps the ANSI escapes
// for the viewport to lay out; word wrap is left to the viewport itself).
func renderMarkdown(body string) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(0),
	)
	if err != nil {
		return body
	}
	out, err := r.Render(body)
	if err != nil {
		return body
	}
	return out
}

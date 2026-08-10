package memory

import (
	"context"
	"fmt"
	"strings"

	"yagent/internal/llm"
)

// sessionSummaryPrompt condenses a finished session for long-term recall
// (memory.md L3 implicit write path).
const sessionSummaryPrompt = `Write a concise summary of this conversation session in at most 200 words, suitable for long-term recall. Preserve: decisions made, user preferences, project facts and gotchas, file paths touched, open tasks. Drop: chit-chat, tool output, repeated code.`

// minSessionMessages is the threshold below which a session is too trivial to
// summarize into long-term memory.
const minSessionMessages = 4

// ChatLLM is the minimal model client the summarizer needs.
type ChatLLM interface {
	ChatStream(ctx context.Context, messages []llm.Message, tools []llm.ToolSchema, onDelta func(string)) (*llm.Response, error)
}

// SummarizeSession condenses a finished session and stores the summary as a
// semantic memory (source "summary"). It is best-effort: errors are returned
// for the caller to surface, but a too-short session is skipped silently.
func SummarizeSession(ctx context.Context, model ChatLLM, st *Store, vs *VectorStore, sessionID string) error {
	if st == nil || vs == nil || sessionID == "" {
		return nil
	}
	history, err := st.History(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for summary: %w", err)
	}
	if len(history) < minSessionMessages {
		return nil // too trivial to remember
	}
	var b strings.Builder
	for _, m := range history {
		if m.Role == "tool" && len(m.Content) > 400 {
			b.WriteString(m.Role + ": [large tool result omitted]\n")
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	prompt := []llm.Message{
		{Role: "system", Content: "You are a session summarizer. " + sessionSummaryPrompt},
		{Role: "user", Content: b.String()},
	}
	resp, err := model.ChatStream(ctx, prompt, nil, func(string) {})
	if err != nil {
		return fmt.Errorf("summarize session: %w", err)
	}
	summary := strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return nil
	}
	if err := vs.Save(ctx, summary, "summary", sessionID); err != nil {
		return fmt.Errorf("store session summary: %w", err)
	}
	return nil
}

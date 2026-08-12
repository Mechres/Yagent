// Package dataset converts verified Yagent sessions into fine-tuning
// trajectories (OpenAI chat or ShareGPT JSONL) so local-first users can train
// a small model on their own tool-calling workflows.
package dataset

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
)

// Format selects the output schema.
type Format string

const (
	FormatOpenAI   Format = "openai"   // {"messages":[{"role","content","tool_calls"}...]}
	FormatShareGPT Format = "sharegpt" // {"conversations":[{"from","value"}...]}
	FormatDPO      Format = "dpo"      // {"prompt","chosen","rejected"} preference pairs
)

// OpenAI message shapes (mirrors llm.Message but flattened to the wire form
// fine-tuners expect).
type openAIMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type shareGPTMessage struct {
	From  string `json:"from"` // system | human | gpt | tool
	Value string `json:"value"`
}

// dpoPair is one preference example: the same user prompt answered two ways.
type dpoPair struct {
	Prompt   string `json:"prompt"`
	Chosen   string `json:"chosen"`
	Rejected string `json:"rejected"`
}

// Options control trajectory extraction.
type Options struct {
	Format      Format
	MinMessages int    // skip sessions with fewer messages than this
	SessionID   string // "" = every session
}

// Export writes fine-tuning trajectories for the sessions in st to w, one JSON
// object per line. Failed turns are dropped: any message whose content carries
// a redaction/home marker, and assistant replies that are empty (cancelled or
// loop-aborted turns). Tool-call arguments referencing secrets are likewise
// skipped. Returns the number of trajectories written.
func Export(ctx context.Context, st *memory.Store, w io.Writer, opt Options) (int, error) {
	if opt.Format == "" {
		opt.Format = FormatOpenAI
	}
	if opt.MinMessages <= 0 {
		opt.MinMessages = 2
	}
	sessions, err := st.ListSessions(ctx)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	enc := json.NewEncoder(bw)

	var written int
	for _, s := range sessions {
		if opt.SessionID != "" && s.ID != opt.SessionID {
			continue
		}
		if s.Messages < opt.MinMessages {
			continue
		}
		msgs, err := st.History(ctx, s.ID)
		if err != nil {
			return written, fmt.Errorf("history %s: %w", s.ID, err)
		}
		clean := cleanTrajectory(msgs)
		if len(clean) < 2 {
			continue
		}
		var line any
		switch opt.Format {
		case FormatShareGPT:
			line = shareGPTLine(clean)
		case FormatDPO:
			pairs := dpoPairs(clean)
			for _, p := range pairs {
				if err := enc.Encode(p); err != nil {
					return written, err
				}
				written++
			}
			continue
		default:
			line = map[string]any{"messages": openAILine(clean)}
		}
		if err := enc.Encode(line); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// isToolError reports whether a tool result carries an execution failure.
func isToolError(content string) bool {
	if strings.HasPrefix(strings.TrimSpace(content), "error:") || strings.HasPrefix(strings.TrimSpace(content), "Error:") {
		return true
	}
	return strings.Contains(content, "[class=") && (strings.Contains(content, "retryable=") || strings.Contains(content, "suggest="))
}

// dpoPairs mines preference pairs from a trajectory: within each user turn, a
// failed tool call (the model's first attempt — the REJECTED response) paired
// with the eventual successful call or answer (the CHOSEN response). Only
// turns that contain both a failure and a later success yield a pair — the
// model's self-correction IS the preference signal. One pair per failed call.
func dpoPairs(msgs []llm.Message) []dpoPair {
	var pairs []dpoPair
	var prompt string
	var turn []llm.Message
	flush := func() {
		if len(turn) == 0 {
			return
		}
		pairs = append(pairs, pairTurn(prompt, turn)...)
		turn = nil
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			flush()
			prompt = m.Content
		case "system":
			continue
		default:
			turn = append(turn, m)
		}
	}
	flush()
	return pairs
}

// pairTurn turns one user turn's messages into (rejected, chosen) pairs.
func pairTurn(prompt string, turn []llm.Message) []dpoPair {
	var out []dpoPair
	var rejected *dpoPair
	for i, m := range turn {
		// A tool result: does it fail?
		if m.Role == "tool" {
			if isToolError(m.Content) {
				// The assistant message before this result emitted the bad call.
				if i > 0 && turn[i-1].Role == "assistant" && len(turn[i-1].ToolCalls) > 0 {
					rejected = &dpoPair{Prompt: prompt, Rejected: renderAssistant(turn[i-1]) + "\n" + m.Content}
				}
				continue
			}
			// A success after a failure closes the pair.
			if rejected != nil {
				rejected.Chosen = m.Content
				if len(strings.TrimSpace(rejected.Chosen)) > 0 && len(strings.TrimSpace(rejected.Rejected)) > 0 {
					out = append(out, *rejected)
				}
				rejected = nil
			}
			continue
		}
		// An assistant answer (no tool call) after a failure also closes it.
		if m.Role == "assistant" && rejected != nil && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != "" {
			rejected.Chosen = m.Content
			if len(strings.TrimSpace(rejected.Chosen)) > 0 && len(strings.TrimSpace(rejected.Rejected)) > 0 {
				out = append(out, *rejected)
			}
			rejected = nil
		}
	}
	return out
}

// renderAssistant renders an assistant message's tool call as text (the
// rejected response is "what the model tried" — the call + its rationale).
func renderAssistant(m llm.Message) string {
	if len(m.ToolCalls) == 0 {
		return m.Content
	}
	var b strings.Builder
	if m.Content != "" {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	for _, tc := range m.ToolCalls {
		fmt.Fprintf(&b, "tool_call %s(%s)", tc.Function.Name, tc.Function.Arguments)
	}
	return b.String()
}

// cleanTrajectory drops messages that would poison a fine-tune: redacted
// content, empty assistant replies, and tool calls with scrubbed arguments.
func cleanTrajectory(msgs []llm.Message) []llm.Message {
	out := msgs[:0:0]
	for _, m := range msgs {
		if m.Role == "system" {
			out = append(out, m)
			continue
		}
		if strings.Contains(m.Content, "[redacted]") || strings.Contains(m.Content, "[home]") {
			continue
		}
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			continue // cancelled / loop-aborted turn
		}
		clean := m
		var calls []llm.ToolCall
		for _, tc := range m.ToolCalls {
			args := string(tc.Function.Arguments)
			if strings.Contains(args, "[redacted]") || strings.Contains(args, "[home]") {
				continue
			}
			calls = append(calls, tc)
		}
		clean.ToolCalls = calls
		out = append(out, clean)
	}
	return out
}

func openAILine(msgs []llm.Message) []openAIMessage {
	var out []openAIMessage
	for _, m := range msgs {
		om := openAIMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolCalls: m.ToolCalls}
		out = append(out, om)
	}
	return out
}

func shareGPTLine(msgs []llm.Message) map[string]any {
	var conv []shareGPTMessage
	for _, m := range msgs {
		switch m.Role {
		case "system":
			conv = append(conv, shareGPTMessage{From: "system", Value: m.Content})
		case "user":
			conv = append(conv, shareGPTMessage{From: "human", Value: m.Content})
		case "tool":
			conv = append(conv, shareGPTMessage{From: "tool", Value: m.Content})
		case "assistant":
			v := m.Content
			if len(m.ToolCalls) > 0 {
				if v != "" {
					v += "\n"
				}
				for _, tc := range m.ToolCalls {
					v += fmt.Sprintf("tool_call %s(%s)", tc.Function.Name, tc.Function.Arguments)
				}
			}
			conv = append(conv, shareGPTMessage{From: "gpt", Value: v})
		}
	}
	return map[string]any{"conversations": conv}
}

package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"yagent/internal/agent"
	"yagent/internal/config"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/tools"
)

// RunChat runs an agent-driven REPL: user lines go through the agent loop,
// model tokens stream as they arrive, tool activity is shown inline, and
// Write/Destructive tool calls prompt for approval (y/n) on the same stdin.
// A new session is created (and persisted) unless continueID is given. On
// exit, the session is summarized into long-term memory (best-effort).
// Slash commands:
//
//	/exit   quit
//	/clear  reset conversation history
//	/help   list commands
func RunChat(ctx context.Context, client *llm.Client, cfg *config.Config, continueID string) error {
	ws, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer st.Close()
	vs, err := memory.OpenVectorStore(cfg.DataDir, cfg.ServerURL, cfg.EmbeddingModel)
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	defer vs.Close()

	sessionID, initialHistory, initialSummary, err := resolveSession(ctx, st, ws, continueID)
	if err != nil {
		return err
	}

	registry := tools.NewRegistry(ws, vs, sessionID)
	reader := bufio.NewReader(os.Stdin)
	w := os.Stdout
	ap := &replApprover{reader: reader, writer: w}

	ag := agent.New(client, registry, ap, agent.Config{
		OnToken: func(delta string) { _, _ = io.WriteString(w, delta) },
		OnTool: func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		},
		Store:          st,
		SessionID:      sessionID,
		Vectors:        vs,
		InitialHistory: initialHistory,
		InitialSummary: initialSummary,
		Window:         cfg.ContextWindow,
	}, ws)

	fmt.Printf("yagent chat — session %s (/exit, /clear, /help)\n", sessionID)
	if initialSummary != "" {
		fmt.Println("(resumed with running summary of the earlier conversation)")
	}
	for {
		fmt.Fprint(w, "> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "/exit":
			goto done
		case "/clear":
			ag.Reset()
			fmt.Fprintln(w, "history cleared")
			continue
		case "/help":
			fmt.Fprintln(w, "commands: /exit /clear /help")
			continue
		}
		if strings.HasPrefix(line, "/") {
			fmt.Fprintln(w, "unknown command:", line)
			continue
		}

		_, err = ag.Run(ctx, line)
		if err != nil {
			if errors.Is(err, agent.ErrMaxIterations) {
				fmt.Fprintf(w, "\n[%v — type /clear to start fresh]\n", err)
			} else {
				fmt.Fprintf(w, "\nerror: %v\n", err)
			}
		}
		fmt.Fprintln(w) // next prompt on a fresh line after the streamed answer
	}
done:
	// Session-end summary job: fold the session into long-term memory.
	if err := memory.SummarizeSession(ctx, client, st, vs, sessionID); err != nil {
		fmt.Fprintf(w, "\nwarning: session summary: %v\n", err)
	}
	return nil
}

// resolveSession returns the session id plus seeded history/summary: a new
// session for a fresh chat, or the resumed state for --continue.
func resolveSession(ctx context.Context, st *memory.Store, ws, continueID string) (string, []llm.Message, string, error) {
	if continueID == "" {
		sess, err := st.NewSession(ctx, ws)
		if err != nil {
			return "", nil, "", fmt.Errorf("create session: %w", err)
		}
		return sess.ID, nil, "", nil
	}
	summary, until, err := st.Summary(ctx, continueID)
	if err != nil {
		return "", nil, "", fmt.Errorf("load summary: %w", err)
	}
	history, err := st.HistoryAfter(ctx, continueID, until)
	if err != nil {
		return "", nil, "", fmt.Errorf("load history: %w", err)
	}
	if len(history) == 0 && summary == "" {
		found := false
		if sessions, err := st.ListSessions(ctx); err == nil {
			for _, s := range sessions {
				if s.ID == continueID {
					found = true
				}
			}
		}
		if !found {
			return "", nil, "", fmt.Errorf("unknown session %q; run 'yagent sessions' to list them", continueID)
		}
	}
	return continueID, history, summary, nil
}

// previewArgs renders tool arguments for the approval prompt, compactly.
func previewArgs(args []byte) string {
	s := strings.TrimSpace(string(args))
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

// replApprover prompts for y/n on stdin; implements agent.Approver.
type replApprover struct {
	reader *bufio.Reader
	writer io.Writer
}

func (a *replApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fmt.Fprintf(a.writer, "\n[%s] %s\n  %s\nAllow? [y/N] ",
		risk, call.Function.Name, previewArgs(call.Function.Arguments))
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

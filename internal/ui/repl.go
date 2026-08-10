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
	"yagent/internal/llm"
	"yagent/internal/tools"
)

// RunChat runs an agent-driven REPL: user lines go through the agent loop,
// model tokens stream as they arrive, tool activity is shown inline, and
// Write/Destructive tool calls prompt for approval (y/n) on the same stdin.
// Slash commands:
//
//	/exit   quit
//	/clear  reset conversation history
//	/help   list commands
func RunChat(ctx context.Context, client *llm.Client) error {
	ws, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	registry := tools.NewRegistry(ws)

	reader := bufio.NewReader(os.Stdin)
	w := os.Stdout
	ap := &replApprover{reader: reader, writer: w}

	ag := agent.New(client, registry, ap, agent.Config{
		OnToken: func(delta string) { _, _ = io.WriteString(w, delta) },
		OnTool: func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		},
	}, ws)

	fmt.Println("yagent chat — /exit to quit, /clear to reset, /help for commands")
	for {
		fmt.Fprint(w, "> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "/exit":
			return nil
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

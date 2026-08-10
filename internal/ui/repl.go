package ui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"yagent/internal/llm"
)

// RunChat runs a stdin/stdout REPL: user lines are sent to the model via
// ChatStream and tokens print as they arrive. Slash commands:
//
//	/exit   quit
//	/clear  reset conversation history
func RunChat(ctx context.Context, client *llm.Client) error {
	sc := bufio.NewScanner(os.Stdin)
	history := []llm.Message{}
	fmt.Println("yagent chat — type /exit to quit, /clear to reset")
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/exit":
			return nil
		case "/clear":
			history = nil
			fmt.Println("history cleared")
			continue
		}
		if strings.HasPrefix(line, "/") {
			fmt.Println("unknown command:", line)
			continue
		}

		history = append(history, llm.Message{Role: "user", Content: line})
		var full string
		err := client.ChatStream(ctx, history, func(delta string) {
			fmt.Print(delta)
			full += delta
		})
		fmt.Println()
		if err != nil {
			fmt.Println("error:", err)
			history = history[:len(history)-1] // drop the failed user turn
			continue
		}
		history = append(history, llm.Message{Role: "assistant", Content: full})
	}
}

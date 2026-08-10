package ui

import (
    "bufio"
    "context"
    "fmt"
    "os"
    "yagent/internal/llm"
)

// RunChat runs a minimal REPL that sends user lines to the llm client.
func RunChat(ctx context.Context, client *llm.Client) error {
    sc := bufio.NewScanner(os.Stdin)
    fmt.Println("yagent chat — type /exit to quit")
    for {
        fmt.Print("> ")
        if !sc.Scan() {
            return sc.Err()
        }
        line := sc.Text()
        if line == "/exit" {
            return nil
        }
        resp, err := client.Chat(ctx, line)
        if err != nil {
            fmt.Println("error:", err)
            continue
        }
        fmt.Println(resp)
    }
}

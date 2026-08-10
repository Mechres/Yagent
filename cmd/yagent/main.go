package main

import (
    "context"
    "flag"
    "fmt"
    "os"
    "yagent/internal/config"
    "yagent/internal/llm"
    "yagent/internal/ui"
)

func main() {
    version := flag.Bool("version", false, "print version")
    cfgPath := flag.String("config", "", "config file")
    flag.Parse()
    if *version {
        fmt.Println("yagent v0.0.0")
        return
    }
    if len(flag.Args()) == 0 || flag.Args()[0] != "chat" {
        fmt.Fprintln(os.Stderr, "usage: yagent chat")
        os.Exit(2)
    }
    cfg, _ := config.LoadConfig(*cfgPath)
    client := llm.NewClient(cfg.ServerURL, cfg.Model)
    if err := ui.RunChat(context.Background(), client); err != nil {
        fmt.Fprintln(os.Stderr, "error:", err)
        os.Exit(1)
    }
}

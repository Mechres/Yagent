package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"yagent/internal/config"
	"yagent/internal/llm"
	"yagent/internal/memory"
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
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}

	cfg, err := config.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	switch args[0] {
	case "chat":
		fs := flag.NewFlagSet("chat", flag.ContinueOnError)
		continueID := fs.String("continue", "", "resume session by id")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		client := llm.NewClient(cfg.ServerURL, cfg.Model)
		if err := ui.RunChat(context.Background(), client, cfg, *continueID); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "sessions":
		if err := runSessions(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: yagent chat [--continue <id>] | yagent sessions | yagent --version")
	os.Exit(2)
}

// runSessions lists persisted sessions, newest first.
func runSessions(cfg *config.Config) error {
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer st.Close()

	sessions, err := st.ListSessions(context.Background())
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions yet")
		return nil
	}
	fmt.Printf("%-40s  %-8s  %s\n", "id", "msgs", "title")
	for _, s := range sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("%-40s  %-8d  %s\n", s.ID, s.Messages, title)
	}
	fmt.Printf("\n%d session(s) (oldest shown last; resume with: yagent chat --continue <id>)\n", len(sessions))
	return nil
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"yagent/internal/config"
	"yagent/internal/doctor"
	"yagent/internal/llm"
	"yagent/internal/logx"
	"yagent/internal/memory"
	"yagent/internal/skills"
	"yagent/internal/ui"
)

// version is overridden at build time via
// -ldflags "-X main.version=<git describe output>" (see Makefile).
var version = "v0.0.0"

func main() {
	versionFlag := flag.Bool("version", false, "print version")
	cfgPath := flag.String("config", "", "config file")
	debug := flag.Bool("debug", false, "debug logging to stderr + log file")
	flag.Parse()
	if *versionFlag {
		fmt.Println(version)
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
	if _, err := logx.Setup(cfg.DataDir, *debug); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	switch args[0] {
	case "chat":
		fs := flag.NewFlagSet("chat", flag.ContinueOnError)
		continueID := fs.String("continue", "", "resume session by id")
		forkID := fs.String("fork", "", "branch a new session from an existing session id")
		goal := fs.String("goal", "", "run the agent autonomously toward this goal (loop mode), then exit")
		rounds := fs.Int("rounds", 0, "max goal-loop rounds (default 8; only with --goal)")
		plain := fs.Bool("plain", false, "force the plain REPL instead of the TUI")
		yolo := fs.Bool("yolo", false, "auto-approve every write/destructive tool and apply skills immediately")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		client := llm.NewClient(cfg.ServerURL, cfg.Model)
		if err := ui.RunChat(context.Background(), client, cfg, *continueID, ui.Options{
			Plain: *plain, YOLO: *yolo, Fork: *forkID, Goal: *goal, Rounds: *rounds,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "sessions":
		if err := runSessions(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "skills":
		fs := flag.NewFlagSet("skills", flag.ContinueOnError)
		scope := fs.String("scope", "global", "store: global or project")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if err := runSkills(cfg, fs.Args(), *scope); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "doctor":
		rep := doctor.Run(cfg)
		fmt.Println("yagent doctor — local-first agent diagnostics")
		if err := rep.Render(os.Stdout); err != nil {
			os.Exit(1)
		}
	case "completion":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: yagent completion bash|zsh")
			os.Exit(2)
		}
		script, err := completionScript(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Print(script)
	default:
		usage()
	}
}

// completionScript returns a shell completion script for bash or zsh.
func completionScript(shell string) (string, error) {
	switch shell {
	case "bash":
		return bashCompletion, nil
	case "zsh":
		return zshCompletion, nil
	}
	return "", fmt.Errorf("unknown shell %q (bash | zsh)", shell)
}

const bashCompletion = `# yagent bash completion — source with: source <(yagent completion bash)
_yagent() {
    local cur
    cur="${COMP_WORDS[COMP_CWORD]}"
    local commands="chat sessions skills doctor completion"
    local chat_flags="--continue --fork --plain --yolo"
    local skills_cmds="list import"
    local scopes="global project"
    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
        return 0
    fi
    case "${COMP_WORDS[1]}" in
        chat)
            COMPREPLY=( $(compgen -W "$chat_flags" -- "$cur") ) ;;
        skills)
            if [ "$COMP_CWORD" -eq 2 ]; then
                COMPREPLY=( $(compgen -W "$skills_cmds" -- "$cur") )
            elif [ "$COMP_CWORD" -eq 3 ] && [ "${COMP_WORDS[2]}" = "import" ]; then
                COMPREPLY=( $(compgen -f -- "$cur") )
            elif [ "$COMP_CWORD" -eq 4 ] && [ "${COMP_WORDS[2]}" = "import" ]; then
                COMPREPLY=( $(compgen -W "$scopes" -- "$cur") )
            fi
            ;;
        completion)
            COMPREPLY=( $(compgen -W "bash zsh" -- "$cur") ) ;;
    esac
    return 0
}
complete -F _yagent yagent
`

const zshCompletion = `#compdef yagent
# yagent zsh completion — add this directory to your fpath and symlink to _yagent
_arguments '1:command:(chat sessions skills doctor completion)' '*: :->args'
case $words[1] in
  chat) _arguments '--continue=[resume session id]:id:' '--fork=[fork from session id]:id:' '--plain[force the plain REPL]' '--yolo[auto-approve writes]' ;;
  skills) _arguments '1:skill command:(list import)' '*: :->file' ;;
  completion) _arguments '1:shell:(bash zsh)' ;;
esac
`

func usage() {
	fmt.Fprintln(os.Stderr, "usage: yagent chat [--continue <id>] [--fork <id>] [--plain] [--yolo] | yagent sessions | yagent skills list|import <file> [--scope global|project] | yagent doctor | yagent --version")
	os.Exit(2)
}

// openSkillsStore resolves the skills store roots from cfg.
func openSkillsStore(cfg *config.Config) (*skills.Store, error) {
	skillsRoot := cfg.Skills.DataDir
	if skillsRoot == "" {
		skillsRoot = cfg.DataDir
	}
	projectDir := cfg.Skills.ProjectDir
	if projectDir == "" {
		ws, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("workspace: %w", err)
		}
		projectDir = filepath.Join(ws, ".yagent", "skills")
	}
	return skills.OpenProject(skillsRoot, projectDir)
}

// runSkills implements `yagent skills list|import`.
func runSkills(cfg *config.Config, args []string, scope string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yagent skills list | import <SKILL.md> [--scope global|project]")
	}
	sk, err := openSkillsStore(cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		metas := sk.List()
		if len(metas) == 0 {
			fmt.Println("no skills yet")
			return nil
		}
		for _, m := range metas {
			root := ""
			if m.Root == skills.RootProject {
				root = ", project"
			}
			fmt.Printf("- %s [%s, %s%s]: %s\n", m.Name, m.Category, m.Source, root, m.Description)
		}
		fmt.Printf("\n%d skill(s)\n", len(metas))
		return nil
	case "import":
		if len(args) < 2 {
			return fmt.Errorf("usage: yagent skills import <SKILL.md> [--scope global|project]")
		}
		warning, err := sk.ImportFile(args[1], scope)
		if err != nil {
			return err
		}
		fmt.Printf("imported %s\n", args[1])
		if warning != "" {
			fmt.Println(warning)
		}
		return nil
	default:
		return fmt.Errorf("unknown skills command %q (list | import)", args[0])
	}
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

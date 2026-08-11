package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/doctor"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/logx"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/ui"
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
		client.BearerToken = cfg.APIKey
		if err := ui.RunChat(context.Background(), client, cfg, *continueID, ui.Options{
			Plain: *plain, YOLO: *yolo, Fork: *forkID, Goal: *goal, Rounds: *rounds,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "sessions":
		if err := runSessionsCmd(cfg, args[1:]); err != nil {
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
	case "init":
		if err := runInit(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ContinueOnError)
		output := fs.String("output", ".", "directory to write the backup into")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if err := runBackup(cfg, *output); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
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
	fmt.Fprintln(os.Stderr, "usage: yagent chat [--continue <id>] [--fork <id>] [--plain] [--yolo] | yagent sessions [search <q>|export <id>] | yagent init | yagent backup [--output dir] | yagent skills list|import <file> [--scope global|project] | yagent doctor | yagent --version")
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
			return fmt.Errorf("usage: yagent skills import <SKILL.md | url> [--scope global|project]")
		}
		content, err := readSkillInput(args[1])
		if err != nil {
			return err
		}
		warning, err := sk.ImportContent(content, scope)
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

// readSkillInput reads SKILL.md content from a local file or an http(s) URL.
func readSkillInput(src string) (string, error) {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		data, err := os.ReadFile(src)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", src, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// runInit writes a starter config file if none exists.
func runInit(cfg *config.Config) error {
	if _, err := os.Stat(cfg.Path); err == nil {
		fmt.Printf("config already exists at %s\n", cfg.Path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return err
	}
	starter := `# Yagent configuration — see config.example.yaml for the full schema.
# Edit with ` + "`yagent chat`" + ` -> /settings, or by hand.
server_url: ` + config.DefaultServerURL + `
model: ` + config.DefaultModel + `
embedding_model: ` + config.DefaultEmbeddingModel + `
context_window: ` + fmt.Sprint(config.DefaultContextWindow) + `
skills:
  write_approval: false
web_search:
  provider: duckduckgo
`
	if err := os.WriteFile(cfg.Path, []byte(starter), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", cfg.Path)
	fmt.Println("next: run 'yagent doctor' to verify your server and model.")
	return nil
}

// runBackup snapshots the whole data dir into a timestamped folder.
func runBackup(cfg *config.Config, output string) error {
	if _, err := os.Stat(cfg.DataDir); err != nil {
		return fmt.Errorf("data dir %s does not exist yet", cfg.DataDir)
	}
	dir := filepath.Join(output, "yagent-backup-"+time.Now().Format("20060102-150405"))
	if err := copyDir(cfg.DataDir, dir); err != nil {
		return err
	}
	fmt.Printf("backed up %s -> %s\n", cfg.DataDir, dir)
	return nil
}

// copyDir recursively copies src into dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// runSessionsCmd dispatches `yagent sessions [search <q> | export <id>]`.
func runSessionsCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return runSessions(cfg)
	}
	switch args[0] {
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("usage: yagent sessions search <query>")
		}
		return runSessionSearch(cfg, args[1])
	case "export":
		if len(args) < 2 {
			return fmt.Errorf("usage: yagent sessions export <id> [--output file.md]")
		}
		output := ""
		if len(args) > 2 {
			if args[2] == "--output" {
				if len(args) < 4 {
					return fmt.Errorf("usage: yagent sessions export <id> [--output file.md]")
				}
				output = args[3]
			} else {
				return fmt.Errorf("unknown option %q (use --output)", args[2])
			}
		}
		return runSessionExport(cfg, args[1], output)
	}
	return fmt.Errorf("unknown sessions command %q (search | export)", args[0])
}

// runSessionSearch full-text searches across all sessions' messages.
func runSessionSearch(cfg *config.Config, query string) error {
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	hits, err := st.SearchMessages(context.Background(), query, 20)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, h := range hits {
		title := h.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("%s  [%s] %s\n  %s\n", h.SessionID[:8], h.Role, title, h.Snippet)
	}
	return nil
}

// runSessionExport renders a session transcript as Markdown.
func runSessionExport(cfg *config.Config, id, output string) error {
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	md, err := st.RenderMarkdown(context.Background(), id)
	if err != nil {
		return err
	}
	if output == "-" {
		fmt.Print(md)
		return nil
	}
	if output == "" {
		output = "session-" + id + ".md"
	}
	noteRedacted(md)
	if err := os.WriteFile(output, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("exported %s -> %s\n", id[:8], output)
	return nil
}

// noteRedacted warns when an export still contains redaction markers, i.e. the
// original session had secrets that were scrubbed from persistent storage.
func noteRedacted(md string) {
	if strings.Contains(md, "[redacted]") || strings.Contains(md, "[home]") {
		fmt.Println("note: this export contains [redacted]/[home] markers — the original session had secrets scrubbed from storage")
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

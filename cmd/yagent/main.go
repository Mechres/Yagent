package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/bench"
	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/dataset"
	"github.com/Mechres/Yagent/internal/doctor"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/logx"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/playbook"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/ui"
)

// stringList is a repeatable flag value ("-x a -x b").
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseSuccessChecks converts the --check flag strings ("main.go contains
// config.New", "pkg/config.go exists", "main.go !contains stress/pkg") into
// agent.SuccessCheck predicates. A malformed entry is a hard error the user
// must fix (better than silently running ungated).
func parseSuccessChecks(flags []string) []agent.SuccessCheck {
	var out []agent.SuccessCheck
	for _, f := range flags {
		switch {
		case strings.Contains(f, " contains "):
			parts := strings.SplitN(f, " contains ", 2)
			out = append(out, agent.SuccessCheck{FileContains: parts[0] + ":" + parts[1]})
		case strings.Contains(f, " !contains "):
			parts := strings.SplitN(f, " !contains ", 2)
			out = append(out, agent.SuccessCheck{FileNotContains: parts[0] + ":" + parts[1]})
		case strings.HasSuffix(f, " exists"):
			out = append(out, agent.SuccessCheck{FileExists: strings.TrimSuffix(f, " exists")})
		default:
			fmt.Fprintf(os.Stderr, "error: malformed --check %q (use \"<file> contains <text>\", \"<file> !contains <text>\", or \"<file> exists\")\n", f)
			os.Exit(2)
		}
	}
	return out
}

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
		research := fs.String("research", "", "run the agent as an autonomous research workflow on this topic (writes a cited report to .yagent/research/), then exit")
		resumeGoal := fs.String("resume-goal", "", "resume an interrupted goal run: restore the goal checkpoint and continue this session")
		playbookName := fs.String("playbook", "", "run a declarative multi-stage workflow (.yagent/playbooks/<name>.yaml), then exit")
		traceFile := fs.String("trace", "", "write a per-context prompt dump (with token estimates) to this file")
		plain := fs.Bool("plain", false, "force the plain REPL instead of the TUI")
		yolo := fs.Bool("yolo", false, "auto-approve every write/destructive tool and apply skills immediately")
		codegen := fs.Bool("codegen", false, "greenfield-code mode: whole-file writes, compile-gated final answers (auto-enabled by --goal)")
		var checkFlags stringList
		fs.Var(&checkFlags, "check", "goal-success predicate (repeatable): \"<file> contains <text>\", \"<file> !contains <text>\", or \"<file> exists\"; the goal DONE verdict is refused until all pass")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		client := llm.NewClient(cfg.ServerURL, cfg.Model)
		client.BearerToken = cfg.APIKey
		client.Sampling = llm.Sampling{
			Temperature:        cfg.Sampling.Temperature,
			TopP:               cfg.Sampling.TopP,
			TopK:               cfg.Sampling.TopK,
			RepetitionPenalty:  cfg.Sampling.RepetitionPenalty,
			MinP:               cfg.Sampling.MinP,
			ReasoningMaxTokens: cfg.Sampling.ReasoningMaxTokens,
		}
		// P2 — cap the context budget at the server's real window so the
		// agent can never push a request past n_ctx (over-length 400s), and
		// raise the inference slot limit to the server's real slot count.
		if p, ok := client.ProbeServerProps(context.Background()); ok {
			if p.NCtx > 0 && cfg.ContextWindow > p.NCtx {
				fmt.Fprintf(os.Stderr, "note: server context window is %d (configured %d); capping the agent budget\n", p.NCtx, cfg.ContextWindow)
				cfg.ContextWindow = p.NCtx
			}
			if p.Slots > 0 {
				llm.SetDefaultSlotLimit(p.Slots)
			}
		}
		var trace io.Writer
		if *traceFile != "" {
			f, err := os.Create(*traceFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: open trace file:", err)
				os.Exit(1)
			}
			defer f.Close()
			trace = f
		}
		if err := ui.RunChat(context.Background(), client, cfg, *continueID, ui.Options{
			Plain: *plain, YOLO: *yolo, Fork: *forkID, Goal: *goal, Rounds: *rounds,
			Research: *research, ResumeGoal: *resumeGoal, Playbook: *playbookName, Trace: trace,
			Codegen: *codegen, Checks: parseSuccessChecks(checkFlags),
		}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "sessions":
		if err := runSessionsCmd(cfg, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "memory":
		if err := runMemoryCmd(cfg, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "export-dataset":
		fs := flag.NewFlagSet("export-dataset", flag.ContinueOnError)
		output := fs.String("output", "", "write the dataset to this file (default: stdout)")
		format := fs.String("format", "openai", "output format: openai | sharegpt | dpo")
		sessionID := fs.String("session", "", "only export this session id (default: all)")
		minMsgs := fs.Int("min-messages", 2, "skip sessions with fewer messages than this")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if err := runExportDataset(cfg, *output, *format, *sessionID, *minMsgs); err != nil {
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
	case "bench":
		fs := flag.NewFlagSet("bench", flag.ContinueOnError)
		jsonOut := fs.Bool("json", false, "machine-readable JSON report")
		repeat := fs.Int("repeat", 1, "run each task N times for a stabler score (default 1)")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if err := runBench(cfg, *jsonOut, *repeat); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "calibrate":
		fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
		write := fs.Bool("write", false, "write the best recipe's sampling block into the config file")
		if err := fs.Parse(args[1:]); err != nil {
			os.Exit(2)
		}
		if err := runCalibrate(cfg, *write); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "playbook":
		if len(args) < 2 || args[1] != "list" {
			fmt.Fprintln(os.Stderr, "usage: yagent playbook list   (also: yagent chat --playbook <name>)")
			os.Exit(2)
		}
		ws, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		names := playbook.List(ws)
		if len(names) == 0 {
			fmt.Println("no playbooks in .yagent/playbooks/")
		}
		for _, n := range names {
			if pb, err := playbook.Load(ws, n); err == nil {
				fmt.Printf("%-24s %s (%d phases)\n", n, pb.Description, len(pb.Phases))
			} else {
				fmt.Printf("%-24s (error: %v)\n", n, err)
			}
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
    local commands="chat sessions skills doctor completion playbook calibrate bench export-dataset init backup memory"
    local chat_flags="--continue --fork --goal --rounds --research --resume-goal --playbook --trace --plain --yolo --codegen --check"
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
_arguments '1:command:(chat sessions skills doctor completion playbook calibrate bench export-dataset init backup memory)' '*: :->args'
case $words[1] in
  chat) _arguments '--continue=[resume session id]:id:' '--fork=[fork from session id]:id:' '--goal=[autonomous goal mode]:goal:' '--rounds=[max goal rounds]:n:' '--research=[autonomous research workflow]:topic:' '--resume-goal=[resume an interrupted goal run]:id:' '--playbook=[run playbook]:name:' '--trace=[prompt dump file]:file:_files' '--plain[force the plain REPL]' '--yolo[auto-approve writes]' '--codegen[greenfield-code mode]' '--check=[goal success predicate (repeatable)]:check:' ;;
  skills) _arguments '1:skill command:(list import)' '*: :->file' ;;
  export-dataset) _arguments '--output=[output file]:file:_files' '--format=[format]:format:(openai sharegpt dpo)' '--session=[session id]:id:' '--min-messages=[min messages]:n:' ;;
  calibrate) _arguments '--write[write sampling to config]' ;;
  bench) _arguments '--json[machine-readable JSON report]' '--repeat=[repeat task count]:n:' ;;
  backup) _arguments '--output=[output directory]:dir:_files -/' ;;
  completion) _arguments '1:shell:(bash zsh)' ;;
  playbook) _arguments '1:command:(list)' ;;
  memory) _arguments '1:command:(list count search delete export)' ;;
esac
`

func usage() {
	fmt.Fprintln(os.Stderr, "usage: yagent chat [--continue <id>] [--fork <id>] [--goal <g>] [--rounds <n>] [--check \"<file> contains <text>\"|!contains|exists]... [--research <topic>] [--resume-goal <id>] [--playbook <name>] [--trace <file>] [--plain] [--yolo] [--codegen] | yagent sessions [search <q>|export <id>] | yagent memory [list|count|search <q>|delete <id|--all>|export <file>] | yagent export-dataset [--output file] [--format openai|sharegpt|dpo] [--session <id>] [--min-messages <n>] | yagent playbook list | yagent calibrate [--write] | yagent bench [--json] [--repeat <n>] | yagent init | yagent backup [--output dir] | yagent skills list|import <file> [--scope global|project] | yagent doctor | yagent --version")
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

// runBench runs the canonical small-model tasks once against the configured
// model and reports per-task pass/fail + timing. --json emits a machine-readable
// report so results can be collected across models (see docs/models-benchmark.md).
func runBench(cfg *config.Config, jsonOut bool, repeat int) error {
	client := llm.NewClient(cfg.ServerURL, cfg.Model)
	client.BearerToken = cfg.APIKey
	client.Sampling = llm.Sampling{
		Temperature:        cfg.Sampling.Temperature,
		TopP:               cfg.Sampling.TopP,
		TopK:               cfg.Sampling.TopK,
		RepetitionPenalty:  cfg.Sampling.RepetitionPenalty,
		MinP:               cfg.Sampling.MinP,
		ReasoningMaxTokens: cfg.Sampling.ReasoningMaxTokens,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := client.ChatStream(ctx, []llm.Message{{Role: "user", Content: "reply with the single word ok"}}, nil, func(string) {}, nil); err != nil {
		return fmt.Errorf("cannot reach the model at %s: %w", cfg.ServerURL, err)
	}
	if repeat < 1 {
		repeat = 1
	}

	tasks := bench.Tasks()
	start := time.Now()
	reports := make([]benchTaskReport, 0, len(tasks))
	for _, tk := range tasks {
		var passed, reason, tokens int
		var wallMS int64
		var detail string
		for i := 0; i < repeat; i++ {
			res := bench.RunTask(client, tk)
			passed += b2i(res.Pass)
			wallMS += res.WallMS
			tokens += res.Tokens
			reason += res.ReasonTokens
			if detail == "" {
				detail = res.Detail
			}
		}
		avg := int64(repeat)
		reports = append(reports, benchTaskReport{
			Name: tk.Name, Passed: passed, Runs: repeat, Detail: detail,
			WallMS: wallMS / avg, TokensPerSec: float64(tokens) / (float64(wallMS) / 1000),
			ReasonTokens: reason / repeat,
		})
	}
	total := time.Since(start)

	// Regression gate (T1-2): record this run as the model's baseline and warn
	// when it's below the model's own best — a model/sampling change that
	// silently degrades the loop should not pass unnoticed. Uses repeat>=2
	// scores (a single flaky run shouldn't overwrite a solid best).
	pass := passedRuns(reports)
	base := bench.LoadBaseline(cfg.DataDir)
	prevBest, improved := base.Record(cfg.DataDir, cfg.Model, pass, len(tasks)*repeat)
	_ = improved
	if repeat >= 2 && prevBest > pass {
		fmt.Fprintf(os.Stderr, "\n⚠ regression: %q fell from %d/%d to %d/%d — re-run `yagent bench --repeat 3` to confirm before trusting this model\n",
			cfg.Model, prevBest, len(tasks)*repeat, pass, len(tasks)*repeat)
	}

	if jsonOut {
		out := map[string]any{
			"model":    cfg.Model,
			"server":   cfg.ServerURL,
			"repeat":   repeat,
			"pass":     pass,
			"total":    len(tasks) * repeat,
			"wall_sec": total.Seconds(),
			"sampling": client.Sampling,
			"tasks":    reports,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Printf("benchmark %q across %d canonical tasks × %d run(s) (sampling: temp %v, top_p %v, reasoning cap %d)\n",
		cfg.Model, len(tasks), repeat, client.Sampling.Temperature, client.Sampling.TopP, client.Sampling.ReasoningMaxTokens)
	for _, r := range reports {
		mark := "ok  "
		if r.Passed < r.Runs {
			mark = fmt.Sprintf("%d/%d", r.Passed, r.Runs)
		}
		think := ""
		if r.ReasonTokens > 0 {
			think = fmt.Sprintf(" · %d think", r.ReasonTokens)
		}
		fmt.Printf("  %-12s %s  %7s  %6.1f t/s%s  %s\n", r.Name, mark,
			(time.Duration(r.WallMS) * time.Millisecond).Round(100*time.Millisecond),
			r.TokensPerSec, think, r.Detail)
	}
	fmt.Printf("\n%d/%d run(s) passed · total %s\n", pass, len(tasks)*repeat, total.Round(time.Second))
	if s := base.ScoreString(cfg.Model); s != "" {
		fmt.Printf("baseline for %q: %s\n", cfg.Model, s)
	}
	return nil
}

// benchTaskReport is one task's aggregate across its runs.
type benchTaskReport struct {
	Name         string  `json:"name"`
	Passed       int     `json:"passed"` // runs that passed
	Runs         int     `json:"runs"`
	Detail       string  `json:"detail,omitempty"`
	WallMS       int64   `json:"wall_ms"` // average per run
	TokensPerSec float64 `json:"tok_s"`
	ReasonTokens int     `json:"reason_tokens"` // avg thinking tokens per run
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// passedRuns sums the passed runs across the per-task reports.
func passedRuns(reports []benchTaskReport) int {
	n := 0
	for _, r := range reports {
		n += r.Passed
	}
	return n
}

// runCalibrate runs the canonical small-model benchmark across the sampling
// recipes against the real local model and reports pass rates (P3). With
// --write it persists the best recipe's sampling into the config file.
func runCalibrate(cfg *config.Config, writeBest bool) error {
	client := llm.NewClient(cfg.ServerURL, cfg.Model)
	client.BearerToken = cfg.APIKey
	client.Sampling = llm.Sampling{
		Temperature:       cfg.Sampling.Temperature,
		TopP:              cfg.Sampling.TopP,
		TopK:              cfg.Sampling.TopK,
		RepetitionPenalty: cfg.Sampling.RepetitionPenalty,
		MinP:              cfg.Sampling.MinP,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := client.ChatStream(ctx, []llm.Message{{Role: "user", Content: "reply with the single word ok"}}, nil, func(string) {}, nil); err != nil {
		return fmt.Errorf("cannot reach the model at %s: %w", cfg.ServerURL, err)
	}

	tasks := bench.Tasks()
	fmt.Printf("calibrating sampling for %q across %d canonical tasks\n", cfg.Model, len(tasks))
	var best *bench.RecipeResult
	for _, r := range bench.RunSweep(client, tasks) {
		if best == nil || r.Pass() > best.Pass() {
			rr := r
			best = &rr
		}
		fmt.Printf("%-12s %d/%d pass\n", r.Recipe.Name, r.Pass(), len(tasks))
		for i, tk := range tasks {
			mark := "ok "
			if !r.Results[i].Pass {
				mark = "FAIL"
			}
			fmt.Printf("  %-12s %s  %s\n", tk.Name, mark, r.Results[i].Detail)
		}
	}
	fmt.Printf("\nbest recipe: %s (%d/%d)\n", best.Recipe.Name, best.Pass(), len(tasks))
	if writeBest && cfg.Path != "" {
		for _, kv := range samplingKeyValues(best.Recipe.Sampling) {
			if err := config.Set(cfg.Path, kv.key, kv.value); err != nil {
				return err
			}
		}
		fmt.Printf("wrote sampling to %s\n", cfg.Path)
		return nil
	}
	fmt.Println("add this to your config to apply it:")
	fmt.Print(bench.RenderRecipe(best.Recipe))
	return nil
}

type samplingKV struct{ key, value string }

func samplingKeyValues(s llm.Sampling) []samplingKV {
	kv := []samplingKV{
		{"sampling.temperature", strconv.FormatFloat(s.Temperature, 'f', -1, 64)},
		{"sampling.top_p", strconv.FormatFloat(s.TopP, 'f', -1, 64)},
	}
	if s.TopK > 0 {
		kv = append(kv, samplingKV{"sampling.top_k", strconv.Itoa(s.TopK)})
	}
	if s.RepetitionPenalty > 0 {
		kv = append(kv, samplingKV{"sampling.repetition_penalty", strconv.FormatFloat(s.RepetitionPenalty, 'f', -1, 64)})
	}
	if s.MinP > 0 {
		kv = append(kv, samplingKV{"sampling.min_p", strconv.FormatFloat(s.MinP, 'f', -1, 64)})
	}
	return kv
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
			return fmt.Errorf("usage: yagent sessions export <id> [--output file] [--format md|html]")
		}
		output := ""
		format := "md"
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--output":
				if i+1 >= len(args) {
					return fmt.Errorf("--output needs a file path")
				}
				i++
				output = args[i]
			case "--format":
				if i+1 >= len(args) {
					return fmt.Errorf("--format needs md or html")
				}
				i++
				format = args[i]
			default:
				return fmt.Errorf("unknown option %q (use --output or --format)", args[i])
			}
		}
		return runSessionExport(cfg, args[1], output, format)
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

// runMemoryCmd inspects, searches, deletes and exports the L3 semantic memory
// store — the human-side counterpart to the model-facing memory_save/search
// tools, so a user can audit and prune what the agent remembers.
func runMemoryCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return runMemoryList(cfg, false, 0)
	}
	switch args[0] {
	case "list":
		limit := 0
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--limit":
				if i+1 >= len(args) {
					return fmt.Errorf("--limit needs a number")
				}
				i++
				n, err := strconv.Atoi(args[i])
				if err != nil {
					return fmt.Errorf("--limit needs a number, got %q", args[i])
				}
				limit = n
			default:
				return fmt.Errorf("unknown option %q (use --limit)", args[i])
			}
		}
		return runMemoryList(cfg, false, limit)
	case "count":
		return runMemoryList(cfg, true, 0)
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("usage: yagent memory search <query> [--scope global|project]")
		}
		scope := "global"
		for i := 2; i < len(args); i++ {
			if args[i] == "--scope" && i+1 < len(args) {
				i++
				scope = args[i]
			}
		}
		return runMemorySearch(cfg, args[1], scope)
	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: yagent memory delete <id|--all>")
		}
		if args[1] == "--all" {
			return runMemoryDelete(cfg, "", true)
		}
		return runMemoryDelete(cfg, args[1], false)
	case "export":
		if len(args) < 2 {
			return fmt.Errorf("usage: yagent memory export <file>")
		}
		return runMemoryExport(cfg, args[1])
	}
	return fmt.Errorf("unknown memory command %q (list | count | search | delete | export)", args[0])
}

// memoryScopeStore opens the global or project vector store. The project store
// lives under <workspace>/.yagent/memory/.
func memoryScopeStore(cfg *config.Config, scope string) (*memory.VectorStore, error) {
	if scope == "project" {
		ws, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		return memory.OpenProjectVectorStore(filepath.Join(ws, ".yagent", "memory"), cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	}
	return memory.OpenVectorStore(cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel)
}

func runMemoryList(cfg *config.Config, countOnly bool, limit int) error {
	vs, err := memory.OpenVectorStore(cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	if err != nil {
		return err
	}
	defer vs.Close()
	if countOnly {
		fmt.Printf("memories: %d\n", vs.Count())
		return nil
	}
	mems, err := vs.List(limit)
	if err != nil {
		return err
	}
	if len(mems) == 0 {
		fmt.Println("no memories stored")
		return nil
	}
	for _, m := range mems {
		src := m.Source
		if src == "" {
			src = "tool"
		}
		fmt.Printf("#%s  [%s] %s\n  %s\n", m.ID, src, shortMemText(m.Text), sourceHint(m.SessionID))
	}
	fmt.Printf("\n%d memories (global store)\n", len(mems))
	return nil
}

func shortMemText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 140 {
		return s[:140] + "…"
	}
	return s
}

func sourceHint(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return fmt.Sprintf("(session %s)", sessionID[:min(len(sessionID), 8)])
}

func runMemorySearch(cfg *config.Config, query, scope string) error {
	vs, err := memoryScopeStore(cfg, scope)
	if err != nil {
		return err
	}
	defer vs.Close()
	mems, err := vs.Search(context.Background(), query, 10)
	if err != nil {
		return err
	}
	if len(mems) == 0 {
		fmt.Println("no matching memories")
		return nil
	}
	for _, m := range mems {
		fmt.Printf("#%s  score=%.2f  %s\n  %s\n", m.ID, m.Score, shortMemText(m.Text), sourceHint(m.SessionID))
	}
	return nil
}

func runMemoryDelete(cfg *config.Config, id string, all bool) error {
	vs, err := memory.OpenVectorStore(cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	if err != nil {
		return err
	}
	defer vs.Close()
	if all {
		if err := vs.DeleteAll(); err != nil {
			return err
		}
		fmt.Println("all memories deleted")
		return nil
	}
	ok, err := vs.Delete(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no memory with id %s", id)
	}
	fmt.Printf("deleted memory #%s\n", id)
	return nil
}

func runMemoryExport(cfg *config.Config, path string) error {
	vs, err := memory.OpenVectorStore(cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	if err != nil {
		return err
	}
	defer vs.Close()
	mems, err := vs.List(0)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Yagent semantic memories (%d)\n\n", len(mems))
	for _, m := range mems {
		fmt.Fprintf(&b, "## #%s [%s]\n%s\n\n", m.ID, m.Source, m.Text)
	}
	if path == "-" {
		fmt.Print(b.String())
		return nil
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("exported %d memories to %s\n", len(mems), path)
	return nil
}

// runSessionExport renders a session transcript as Markdown or HTML.
func runSessionExport(cfg *config.Config, id, output, format string) error {
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	var body string
	switch format {
	case "md":
		body, err = st.RenderMarkdown(context.Background(), id)
	case "html":
		body, err = st.RenderHTML(context.Background(), id)
	default:
		return fmt.Errorf("unknown format %q (md | html)", format)
	}
	if err != nil {
		return err
	}
	if output == "-" {
		fmt.Print(body)
		return nil
	}
	if output == "" {
		output = "session-" + id + "." + format
	}
	noteRedacted(body)
	if err := os.WriteFile(output, []byte(body), 0o644); err != nil {
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

// runExportDataset writes fine-tuning trajectories from sessions to a JSONL
// file (or stdout), skipping failed/redacted turns.
func runExportDataset(cfg *config.Config, output, format, sessionID string, minMsgs int) error {
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer st.Close()

	var w io.Writer = os.Stdout
	var f *os.File
	if output != "" && output != "-" {
		f, err = os.Create(output)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	n, err := dataset.Export(context.Background(), st, w, dataset.Options{
		Format:      dataset.Format(format),
		SessionID:   sessionID,
		MinMessages: minMsgs,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d trajectory(ies) in %s format\n", n, format)
	return nil
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

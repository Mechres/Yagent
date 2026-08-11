package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/checkpoint"
	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/jobs"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
	"github.com/Mechres/Yagent/internal/undo"
	"github.com/Mechres/Yagent/internal/web"
)

// Options tunes the chat UI.
type Options struct {
	// Plain forces the streaming REPL instead of the TUI (useful for pipes).
	Plain bool
	// YOLO auto-approves every write/destructive tool and applies skill
	// writes immediately instead of staging them. Use at your own risk.
	YOLO bool
	// Fork branches a new session from an existing session id (the original
	// history is copied, the original session is untouched).
	Fork string
	// Goal runs the agent autonomously toward a goal (loop mode) instead of
	// interactive chat; Rounds caps the loop (default 8).
	Goal   string
	Rounds int
}

// autoApprover grants every approval without prompting (--yolo).
type autoApprover struct{}

func (autoApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	return agent.Approval{OK: true}, nil
}

// toggleableApprover wraps a prompting approver and can be switched to yolo
// mode at runtime via /yolo.
type toggleableApprover struct {
	inner agent.Approver
	mu    sync.RWMutex
	yolo  bool
}

func newToggleableApprover(inner agent.Approver) *toggleableApprover {
	return &toggleableApprover{inner: inner}
}

func (t *toggleableApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	t.mu.RLock()
	yolo := t.yolo
	t.mu.RUnlock()
	if yolo {
		return agent.Approval{OK: true}, nil
	}
	return t.inner.Approve(ctx, call, risk)
}

func (t *toggleableApprover) SetYOLO(on bool) {
	t.mu.Lock()
	t.yolo = on
	t.mu.Unlock()
}

func (t *toggleableApprover) IsYOLO() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.yolo
}

// RunChat runs an agent-driven REPL: user lines go through the agent loop,
// model tokens stream as they arrive, tool activity is shown inline, and
// Write/Destructive tool calls prompt for approval (y/n) on the same stdin.
// A new session is created (and persisted) unless continueID is given. On
// exit, the session is summarized into long-term memory (best-effort).
// Slash commands:
//
//	/exit                  quit
//	/clear                 reset conversation history
//	/help                  list commands
//	/skills list           list skills
//	/skills pending        staged skill writes
//	/skills diff <id>      full diff of a staged write
//	/skills approve <id|all>
//	/skills reject <id|all>
//	/skills approval on|off
//	/skill-name            load a SKILL.md into context
func RunChat(ctx context.Context, client *llm.Client, cfg *config.Config, continueID string, opts Options) error {
	// Goal mode: autonomous loop toward a goal, then exit.
	if opts.Goal != "" {
		env, err := newChatEnv(ctx, cfg, continueID, opts.Fork)
		if err != nil {
			return err
		}
		defer env.st.Close()
		defer env.vs.Close()
		defer env.projVS.Close()
		defer env.idx.Close()
		return runGoalMode(ctx, client, cfg, env, opts.Goal, opts.Rounds, opts.YOLO)
	}
	// TUI by default on a real terminal; --plain (or piped stdin) falls back
	// to the streaming REPL.
	if !opts.Plain && isTerminal(os.Stdin) {
		return RunTUI(ctx, client, cfg, continueID, opts.YOLO, opts.Fork)
	}
	env, err := newChatEnv(ctx, cfg, continueID, opts.Fork)
	if err != nil {
		return err
	}
	defer env.st.Close()
	defer env.vs.Close()
	defer env.projVS.Close()
	defer env.idx.Close()
	defer env.jobs.StopAll()

	w := os.Stdout
	env.registry.SetIndexProgress(func(line string) {
		fmt.Fprintf(w, "  [index] %s\n", line)
	})
	startBackgroundIndex(ctx, env, func(line string) {
		fmt.Fprintf(w, "  [index] %s\n", line)
	})
	reader := bufio.NewReader(os.Stdin)
	ap := newToggleableApprover(&replApprover{reader: reader, writer: w})
	ap.SetYOLO(opts.YOLO)
	if opts.YOLO {
		env.registry.SetSkillsWriteApproval(false)
	}

	ag := newAgent(client, cfg, env, ap,
		func(delta string) { _, _ = io.WriteString(w, delta) },
		// Thinking is shown dimmed/italic above the answer (display-only),
		// unless ui.show_reasoning is off.
		func(delta string) {
			if !cfg.UI.ShowReasoning {
				return
			}
			_, _ = fmt.Fprintf(w, "\x1b[2m\x1b[3m%s\x1b[0m", delta)
		},
		func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		})

	skillsCmd := &skillsHandler{
		store:       env.sk,
		reg:         env.registry,
		cfg:         cfg,
		w:           w,
		approval:    &cfg.Skills.WriteApproval,
		ctx:         ctx,
		client:      client,
		env:         env,
		yoloToggler: ap,
	}

	fmt.Printf("yagent chat — session %s (/exit, /clear, /help)\n", env.sessionID)
	if env.forkSource != "" {
		fmt.Printf("(forked from %s — the original session is untouched)\n", env.forkSource)
	}
	if env.initialSummary != "" {
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
			fmt.Fprintln(w, "commands: /exit /clear /help /yolo /export [file] /settings /set /goal <what> /undo /skills list|pending|diff|verify|approve|reject|approval /skill-name")
			continue
		}
		if strings.HasPrefix(line, "/") {
			handled, err := skillsCmd.handle(line, ag)
			if err != nil {
				fmt.Fprintf(w, "error: %v\n", err)
			}
			if handled {
				continue
			}
			fmt.Fprintln(w, "unknown command:", line)
			continue
		}

		env.undo.StartTurn()
		_, err = ag.Run(ctx, line)
		env.undo.EndTurn()
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
	// End-of-session skill-creation opportunity, then the session-summary job.
	if err := ag.Finish(ctx); err != nil {
		fmt.Fprintf(w, "\nwarning: skill review: %v\n", err)
	}
	if err := memory.SummarizeSession(ctx, client, env.st, env.vs, env.sessionID); err != nil {
		fmt.Fprintf(w, "\nwarning: session summary: %v\n", err)
	}
	fmt.Fprintf(w, "\nsession: %s (resume with: yagent chat --continue %s)\n", env.sessionID, env.sessionID)
	return nil
}

// chatEnv is the shared runtime state for the REPL and TUI.
type chatEnv struct {
	st             *memory.Store
	vs             *memory.VectorStore
	projVS         *memory.VectorStore
	sk             *skills.Store
	idx            *index.Store
	web            *web.Client
	registry       *tools.Registry
	sessionID      string
	initialHistory []llm.Message
	initialSummary string
	forkSource     string
	undo           *undo.Buffer
	jobs           *jobs.Registry
}

// runGoalMode drives the agent autonomously toward a goal: each round runs the
// full loop, streams its answer, and a DONE/CONTINUE verdict decides whether to
// continue. Ends with the session id for later resume.
func runGoalMode(ctx context.Context, client *llm.Client, cfg *config.Config, env *chatEnv, goal string, rounds int, yolo bool) error {
	w := os.Stdout
	ap := newToggleableApprover(&replApprover{reader: bufio.NewReader(os.Stdin), writer: w})
	ap.SetYOLO(yolo)
	if yolo {
		env.registry.SetSkillsWriteApproval(false)
	}
	ag := newAgent(client, cfg, env, ap,
		func(delta string) { _, _ = io.WriteString(w, delta) },
		func(delta string) {
			_, _ = fmt.Fprintf(w, "\x1b[2m\x1b[3m%s\x1b[0m", delta)
		},
		func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		})

	fmt.Printf("goal mode — working toward: %s\n", goal)
	if env.forkSource != "" {
		fmt.Printf("(forked from %s)\n", env.forkSource)
	}
	if ws, err := os.Getwd(); err == nil {
		if dir, err := checkpoint.Save(ws, checkpoint.GoalName); err != nil {
			fmt.Fprintf(w, "(warning: could not snapshot workspace for rollback: %v)\n", err)
		} else {
			fmt.Fprintf(w, "workspace snapshotted (%s) — revert later with /checkpoint restore goal\n", dir)
		}
	}
	answer, err := ag.RunGoal(ctx, goal, rounds, func(r int, _ string) {
		fmt.Fprintf(w, "\n—— round %d ——\n", r)
	})
	if err != nil {
		fmt.Fprintf(w, "\ngoal loop: %v\n", err)
	}
	if err := ag.Finish(ctx); err != nil {
		fmt.Fprintf(w, "\nwarning: skill review: %v\n", err)
	}
	if err := memory.SummarizeSession(ctx, client, env.st, env.vs, env.sessionID); err != nil {
		fmt.Fprintf(w, "\nwarning: session summary: %v\n", err)
	}
	_ = answer
	fmt.Fprintf(w, "\nsession: %s (resume with: yagent chat --continue %s)\n", env.sessionID, env.sessionID)
	return nil
}

// startBackgroundIndex refreshes the code index at session start when it is
// already built (Count > 0): Index() hash-checks every file and only re-embeds
// the changed ones, so an unchanged repo is near-free. The first build is left
// to the explicit index_repo tool. Runs in a goroutine; never blocks the UI.
func startBackgroundIndex(ctx context.Context, env *chatEnv, sink func(string)) {
	if env == nil || env.idx == nil || env.idx.Count() == 0 {
		return
	}
	env.idx.SetOnProgress(sink)
	go func() {
		if _, err := env.idx.Index(ctx); err != nil {
			slog.Warn("background re-index", "error", err)
		}
	}()
}

// newChatEnv opens all stores and builds the agent runtime shared by the
// plain REPL and the TUI.
func newChatEnv(ctx context.Context, cfg *config.Config, continueID, forkID string) (*chatEnv, error) {
	ws, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	st, err := memory.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	vs, err := memory.OpenVectorStore(cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	vs.SetBearerToken(cfg.APIKey)
	projVS, err := memory.OpenProjectVectorStore(filepath.Join(ws, ".yagent", "memory"), cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	if err != nil {
		return nil, fmt.Errorf("open project memory store: %w", err)
	}
	projVS.SetBearerToken(cfg.APIKey)
	skillsRoot := cfg.Skills.DataDir
	if skillsRoot == "" {
		skillsRoot = cfg.DataDir
	}
	sk, err := skills.OpenProject(skillsRoot, cfg.Skills.ProjectDir)
	if err != nil {
		return nil, fmt.Errorf("open skills store: %w", err)
	}
	idx, err := index.Open(ws, cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel)
	if err != nil {
		return nil, fmt.Errorf("open code index: %w", err)
	}
	idx.SetBearerToken(cfg.APIKey)
	webClient, err := web.New(web.Config{Provider: cfg.Web.Provider, SearxngURL: cfg.Web.SearxngURL})
	if err != nil {
		return nil, fmt.Errorf("web search config: %w", err)
	}
	var consultClient *llm.Client
	if cfg.Consult.Model != "" {
		consultURL := cfg.Consult.ServerURL
		if consultURL == "" {
			consultURL = cfg.ServerURL
		}
		consultClient = llm.NewClient(consultURL, cfg.Consult.Model)
		consultClient.BearerToken = cfg.Consult.APIKey
	}

	sessionID, initialHistory, initialSummary, forkSource, err := resolveSession(ctx, st, ws, continueID, forkID)
	if err != nil {
		return nil, err
	}

	env := &chatEnv{
		st: st, vs: vs, projVS: projVS, sk: sk, idx: idx, web: webClient,
		sessionID: sessionID, initialHistory: initialHistory, initialSummary: initialSummary,
		forkSource: forkSource, undo: undo.New(), jobs: jobs.New(),
	}
	env.registry = tools.NewRegistry(ws, tools.Options{
		Vectors:             vs,
		ProjectVectors:      projVS,
		SessionID:           sessionID,
		Skills:              sk,
		Index:               idx,
		Web:                 webClient,
		Consult:             consultClient,
		ConsultCmd:          cfg.Consult.Cmd,
		Undo:                env.undo,
		Jobs:                env.jobs,
		ShellSandbox:        cfg.Shell.Sandbox,
		SkillsWriteApproval: cfg.Skills.WriteApproval,
	})
	return env, nil
}

// newAgent builds the agent loop over a chatEnv.
func newAgent(client *llm.Client, cfg *config.Config, env *chatEnv, approver agent.Approver, onToken, onReasoning func(string), onTool func(llm.ToolCall)) *agent.Agent {
	ws, _ := os.Getwd()
	// M7 v1: subagents are read-only child agents that return a summary.
	// M7 beyond v2: an optional tools slice scopes each child's registry.
	env.registry.SetSubagent(func(ctx context.Context, task, workspace string, toolset []string) (string, error) {
		reg := tools.NewRegistry(workspace, tools.Options{
			ReadOnly:       true,
			Web:            env.web,
			Index:          env.idx,
			Vectors:        env.vs,
			ProjectVectors: env.projVS,
			Skills:         env.sk,
		})
		if len(toolset) > 0 {
			var err error
			reg, err = reg.Restrict(toolset)
			if err != nil {
				return err.Error(), nil
			}
		}
		answer, tokens, err := agent.RunSubagent(ctx, client, reg, task, workspace)
		if err != nil {
			return "error: subagent failed: " + err.Error(), nil
		}
		return fmt.Sprintf("%s\n\n(subagent used ~%d tokens)", answer, tokens), nil
	})
	return agent.New(client, env.registry, approver, agent.Config{
		OnToken:         onToken,
		OnReasoning:     onReasoning,
		OnTool:          onTool,
		Store:           env.st,
		SessionID:       env.sessionID,
		Vectors:         env.vs,
		ProjectVectors:  env.projVS,
		Skills:          env.sk,
		Index:           env.idx,
		IndexAutoInject: true,
		InitialHistory:  env.initialHistory,
		InitialSummary:  env.initialSummary,
		Window:          cfg.ContextWindow,
	}, ws)
}

// skillsHandler implements the /skills and /skill-name REPL commands.
type skillsHandler struct {
	store       *skills.Store
	reg         *tools.Registry
	cfg         *config.Config
	w           io.Writer
	approval    *bool
	ctx         context.Context
	client      *llm.Client
	env         *chatEnv
	yoloToggler *toggleableApprover
}

// denyWriteApprover allows only read-only tools during skill verification
// (safety: a verification pass must never auto-approve side effects).
type denyWriteApprover struct{}

func (denyWriteApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	return agent.Approval{OK: risk == tools.RiskReadOnly}, nil
}

func (h *skillsHandler) handle(line string, ag *agent.Agent) (bool, error) {
	rest := strings.TrimPrefix(line, "/")
	switch {
	case rest == "skills":
		fmt.Fprintln(h.w, "usage: /skills list | pending | diff <id> | verify <id> | approve <id|all> | reject <id|all> | approval on|off")
		return true, nil
	case strings.HasPrefix(rest, "skills "):
		return h.handleSkills(strings.Fields(rest)[1:])
	case rest == "export" || strings.HasPrefix(rest, "export "):
		return h.exportSession(rest, ag)
	case rest == "yolo" || strings.HasPrefix(rest, "yolo "):
		return h.handleYOLO(rest)
	case rest == "goal" || strings.HasPrefix(rest, "goal "):
		parts := strings.Fields(rest)
		if len(parts) < 2 {
			return true, fmt.Errorf("usage: /goal <what to achieve>")
		}
		return h.runGoal(ag, strings.TrimSpace(strings.TrimPrefix(rest, "goal")))
	case rest == "settings" || strings.HasPrefix(rest, "set "):
		if rest == "settings" {
			return h.showSettings()
		}
		return h.setSetting(rest)
	case rest == "undo":
		return h.undoLastTurn()
	case rest == "checkpoint" || strings.HasPrefix(rest, "checkpoint "):
		return h.handleCheckpoint(rest)
	default:
		// /skill-name: load a SKILL.md into context and continue.
		content, warning, err := h.store.View(rest, "")
		if err != nil {
			return false, nil // not a skill; let the caller say "unknown command"
		}
		ag.InjectSystem("Skill loaded (procedural memory) — follow it when applicable:\n\n" + content)
		fmt.Fprintf(h.w, "loaded skill %s\n", rest)
		if warning != "" {
			fmt.Fprintln(h.w, warning)
		}
		return true, nil
	}
}

func (h *skillsHandler) handleSkills(args []string) (bool, error) {
	if len(args) == 0 {
		fmt.Fprintln(h.w, "usage: /skills list | pending | diff <id> | approve <id|all> | reject <id|all> | approval on|off")
		return true, nil
	}
	switch args[0] {
	case "list":
		metas := h.store.List()
		if len(metas) == 0 {
			fmt.Fprintln(h.w, "no skills yet")
			return true, nil
		}
		for _, m := range metas {
			fmt.Fprintf(h.w, "- %s [%s, %s%s]: %s\n", m.Name, m.Category, m.Source,
				projectSuffix(m.Root), m.Description)
		}
		return true, nil
	case "pending":
		pending, err := h.store.ListPending()
		if err != nil {
			return true, err
		}
		if len(pending) == 0 {
			fmt.Fprintln(h.w, "no pending skill writes (approval gate on: writes are staged here)")
			return true, nil
		}
		for _, p := range pending {
			note := ""
			if p.Failures >= skills.MaxSkillFailures {
				note = fmt.Sprintf("  (failed verification %d×)", p.Failures)
			} else if p.Failures > 0 {
				note = fmt.Sprintf("  (verification FAIL %d×)", p.Failures)
			}
			fmt.Fprintf(h.w, "%s  %-11s %s%s\n", shortID(p.ID), p.Action, p.Name, note)
		}
		return true, nil
	case "diff":
		if len(args) < 2 {
			return true, fmt.Errorf("diff needs an id")
		}
		diff, err := h.store.PendingDiff(args[1])
		if err != nil {
			return true, err
		}
		fmt.Fprintln(h.w, diff)
		return true, nil
	case "verify":
		if len(args) < 2 {
			return true, fmt.Errorf("verify needs an id")
		}
		if err := h.verifyPending(args[1]); err != nil {
			return true, err
		}
		return true, nil
	case "approve":
		if len(args) < 2 {
			return true, fmt.Errorf("approve needs an id or 'all'")
		}
		ids, err := h.pendingIDs(args[1])
		if err != nil {
			return true, err
		}
		for _, id := range ids {
			// warn when the staged write failed verification repeatedly
			pending, _ := h.store.ListPending()
			for _, p := range pending {
				if p.ID == id && p.Failures >= skills.MaxSkillFailures {
					fmt.Fprintf(h.w, "warning: %s failed verification %d× — approving anyway\n", shortID(id), p.Failures)
				}
			}
			if warning, err := h.store.ApprovePending(id); err != nil {
				fmt.Fprintf(h.w, "error: %s: %v\n", id, err)
			} else {
				fmt.Fprintf(h.w, "approved %s\n", id)
				if warning != "" {
					fmt.Fprintln(h.w, warning)
				}
			}
		}
		return true, nil
	case "reject":
		if len(args) < 2 {
			return true, fmt.Errorf("reject needs an id or 'all'")
		}
		ids, err := h.pendingIDs(args[1])
		if err != nil {
			return true, err
		}
		for _, id := range ids {
			if err := h.store.RejectPending(id); err != nil {
				fmt.Fprintf(h.w, "error: %s: %v\n", id, err)
			} else {
				fmt.Fprintf(h.w, "rejected %s\n", id)
			}
		}
		return true, nil
	case "approval":
		if len(args) < 2 || (args[1] != "on" && args[1] != "off") {
			return true, fmt.Errorf("approval needs 'on' or 'off'")
		}
		on := args[1] == "on"
		*h.approval = on
		h.reg.SetSkillsWriteApproval(on)
		if err := config.SetWriteApproval(h.cfg.Path, on); err != nil {
			fmt.Fprintf(h.w, "note: could not persist to config (%v); toggle is session-only\n", err)
		} else {
			fmt.Fprintf(h.w, "skills approval %s (persisted to %s)\n", args[1], h.cfg.Path)
		}
		return true, nil
	}
	return false, nil
}

// exportSession writes the conversation transcript as Markdown (default
// session-<id>.md in the current directory; override with /export <path>).
func (h *skillsHandler) exportSession(rest string, ag *agent.Agent) (bool, error) {
	parts := strings.Fields(rest)
	path := "session-" + h.env.sessionID + ".md"
	if len(parts) > 1 {
		path = parts[1]
	}
	md, err := h.env.st.RenderMarkdown(h.ctx, h.env.sessionID)
	if err != nil {
		return true, fmt.Errorf("render session: %w", err)
	}
	if strings.Contains(md, "[redacted]") || strings.Contains(md, "[home]") {
		fmt.Fprintln(h.w, "note: this export contains [redacted]/[home] markers — the session had secrets scrubbed from storage")
	}
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return true, fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(h.w, "saved session to %s\n", path)
	return true, nil
}

// pendingIDs resolves an id or "all" to the list of staged write ids.
func (h *skillsHandler) pendingIDs(id string) ([]string, error) {
	if id != "all" {
		return []string{id}, nil
	}
	pending, err := h.store.ListPending()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.ID)
	}
	return ids, nil
}

// undoLastTurn reverts the file writes from the most recent completed turn.
func (h *skillsHandler) undoLastTurn() (bool, error) {
	if h.env == nil || h.env.undo == nil || !h.env.undo.CanUndo() {
		fmt.Fprintln(h.w, "nothing to undo")
		return true, nil
	}
	entries, err := h.env.undo.UndoLastTurn()
	if err != nil {
		return true, err
	}
	for _, e := range entries {
		fmt.Fprintf(h.w, "  reverted %s\n", e.Path)
	}
	return true, nil
}

// handleCheckpoint manages workspace snapshots: /checkpoint [list], save,
// restore, delete. Goal mode auto-saves a "goal" snapshot before running.
func (h *skillsHandler) handleCheckpoint(rest string) (bool, error) {
	ws, err := os.Getwd()
	if err != nil {
		return true, fmt.Errorf("workspace: %w", err)
	}
	parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(rest, "checkpoint")))
	usage := "usage: /checkpoint save <name> | restore <name> | delete <name>"
	if len(parts) == 0 {
		names := checkpoint.List(ws)
		if len(names) == 0 {
			fmt.Fprintln(h.w, "no checkpoints yet (goal mode auto-saves a 'goal' snapshot)")
			return true, nil
		}
		for _, n := range names {
			fmt.Fprintf(h.w, "  %s  (%s)\n", n, checkpoint.FormatAge(ws, n))
		}
		fmt.Fprintln(h.w, usage)
		return true, nil
	}
	switch parts[0] {
	case "save":
		if len(parts) < 2 {
			return true, errors.New(usage)
		}
		dir, err := checkpoint.Save(ws, parts[1])
		if err != nil {
			return true, err
		}
		fmt.Fprintf(h.w, "  saved checkpoint %s -> %s\n", parts[1], dir)
	case "restore":
		if len(parts) < 2 {
			return true, errors.New(usage)
		}
		if err := checkpoint.Restore(ws, parts[1]); err != nil {
			return true, err
		}
		fmt.Fprintf(h.w, "  restored workspace from checkpoint %s\n", parts[1])
	case "delete":
		if len(parts) < 2 {
			return true, errors.New(usage)
		}
		if err := checkpoint.Delete(ws, parts[1]); err != nil {
			return true, err
		}
		fmt.Fprintf(h.w, "  deleted checkpoint %s\n", parts[1])
	default:
		return true, fmt.Errorf("unknown checkpoint subcommand %q; %s", parts[0], usage)
	}
	return true, nil
}

// showSettings prints every editable setting and its current value.
func (h *skillsHandler) showSettings() (bool, error) {
	for _, s := range config.Settings() {
		fmt.Fprintf(h.w, "%-24s %s\n", s.Key, h.cfg.Get(s.Key))
	}
	fmt.Fprintf(h.w, "config file: %s\n", h.cfg.Path)
	if h.cfg.ProjectPath != "" {
		fmt.Fprintf(h.w, "project config: %s (overrides global)\n", h.cfg.ProjectPath)
	}
	fmt.Fprintf(h.w, "edit with: /set <key> <value>  (most keys take effect on the next chat session)\n")
	return true, nil
}

// setSetting persists a dotted config key — to the project config when one
// exists, otherwise the global one — and applies it to the running config.
func (h *skillsHandler) setSetting(rest string) (bool, error) {
	parts := strings.SplitN(rest, " ", 3)
	if len(parts) < 3 {
		return true, fmt.Errorf("usage: /set <key> <value>  (see /settings for keys)")
	}
	key, value := parts[1], parts[2]
	target := h.cfg.Path
	if h.cfg.ProjectPath != "" {
		target = h.cfg.ProjectPath
	}
	if err := config.Set(target, key, value); err != nil {
		return true, err
	}
	if err := applySetting(h.cfg, h.reg, key, value); err != nil {
		return true, err
	}
	fmt.Fprintf(h.w, "%s = %s (saved to %s)\n", key, value, target)
	if key != "skills.write_approval" {
		fmt.Fprintf(h.w, "note: takes effect on the next chat session\n")
	}
	return true, nil
}

// applySetting mirrors a persisted value into the running Config and, where
// possible, the live registry.
func applySetting(c *config.Config, reg *tools.Registry, key, value string) error {
	switch key {
	case "server_url":
		c.ServerURL = value
	case "model":
		c.Model = value
	case "embedding_model":
		c.EmbeddingModel = value
	case "embedding_server_url":
		c.EmbeddingServerURL = value
	case "data_dir":
		c.DataDir = value
	case "theme":
		if !slices.Contains(config.ThemeOptions, value) {
			return fmt.Errorf("theme must be one of: %s", strings.Join(config.ThemeOptions, ", "))
		}
		c.Theme = value
	case "sampling.temperature":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		c.Sampling.Temperature = f
	case "sampling.top_p":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		c.Sampling.TopP = f
	case "sampling.top_k":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Sampling.TopK = n
	case "sampling.repetition_penalty":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		c.Sampling.RepetitionPenalty = f
	case "ui.show_reasoning":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.UI.ShowReasoning = b
	case "context_window":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.ContextWindow = n
	case "web_search.provider":
		c.Web.Provider = value
	case "web_search.searxng_url":
		c.Web.SearxngURL = value
	case "skills.write_approval":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.Skills.WriteApproval = b
		if reg != nil {
			reg.SetSkillsWriteApproval(b)
		}
	case "skills.data_dir":
		c.Skills.DataDir = value
	case "skills.project_dir":
		c.Skills.ProjectDir = value
	case "shell.sandbox":
		c.Shell.Sandbox = value
	case "consult.server_url":
		c.Consult.ServerURL = value
	case "consult.model":
		c.Consult.Model = value
	case "consult.api_key":
		c.Consult.APIKey = value
	}
	return nil
}

// runGoal drives the current agent through the autonomous goal loop, printing
// a round marker per round; the answers stream through the UI's OnToken.
func (h *skillsHandler) runGoal(ag *agent.Agent, goal string) (bool, error) {
	fmt.Fprintf(h.w, "goal mode — working toward: %s\n", goal)
	if ws, err := os.Getwd(); err == nil {
		if dir, err := checkpoint.Save(ws, checkpoint.GoalName); err != nil {
			fmt.Fprintf(h.w, "(warning: could not snapshot workspace for rollback: %v)\n", err)
		} else {
			fmt.Fprintf(h.w, "workspace snapshotted (%s) — revert later with /checkpoint restore goal\n", dir)
		}
	}
	_, err := ag.RunGoal(h.ctx, goal, agent.DefaultGoalRounds, func(r int, _ string) {
		fmt.Fprintf(h.w, "\n—— round %d ——\n", r)
	})
	if err != nil {
		fmt.Fprintf(h.w, "\ngoal loop: %v\n", err)
		return true, nil
	}
	fmt.Fprintf(h.w, "\ngoal achieved.\n")
	return true, nil
}

// handleYOLO toggles yolo mode at runtime: /yolo flips it, /yolo on|off sets
// it. Yolo auto-approves every write/destructive tool and applies skill
// writes immediately.
func (h *skillsHandler) handleYOLO(rest string) (bool, error) {
	parts := strings.Fields(rest)
	var on bool
	switch {
	case len(parts) > 1 && parts[1] == "on":
		on = true
	case len(parts) > 1 && parts[1] == "off":
		on = false
	case len(parts) > 1:
		return true, fmt.Errorf("yolo takes on|off, or bare /yolo to toggle")
	default:
		on = !h.yoloToggler.IsYOLO()
	}
	h.yoloToggler.SetYOLO(on)
	h.reg.SetSkillsWriteApproval(!on)
	suffix := ""
	if on {
		suffix = " and skill writes apply immediately"
	}
	fmt.Fprintf(h.w, "yolo mode %s — every write/destructive tool is auto-approved%s\n", boolWord(on), suffix)
	return true, nil
}

func boolWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// verifyPending runs the verification harness on a staged skill write: the
// model executes the skill's "## Verification" section with the workspace
// tools (read-only) and reports PASS/FAIL. A FAIL increments the skill's
// failure counter (staleness); a PASS resets it.
func (h *skillsHandler) verifyPending(id string) error {
	content, err := h.store.PendingSkillContent(id)
	if err != nil {
		return err
	}
	if content == "" {
		fmt.Fprintln(h.w, "nothing to verify (delete/remove_file write)")
		return nil
	}
	fmt.Fprintln(h.w, "verifying staged skill — executing its Verification section (read-only tools)...")
	ws, err := os.Getwd()
	if err != nil {
		return err
	}
	reg := tools.NewRegistry(ws, tools.Options{Index: h.env.idx, Web: h.env.web})
	answer, err := agent.VerifySkill(h.ctx, h.client, reg, denyWriteApprover{}, content, ws)
	if err != nil {
		return err
	}
	// record staleness against the skill and the staged write
	name, _ := h.store.PendingName(id)
	switch agent.ParseVerdict(answer) {
	case "PASS":
		if name != "" {
			_ = h.store.ClearFailures(name)
		}
		_ = h.store.ClearPendingFailures(id)
	case "FAIL":
		if name != "" {
			_ = h.store.RecordFailure(name)
		}
		_ = h.store.RecordPendingFailure(id)
	}
	fmt.Fprintln(h.w, answer)
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func projectSuffix(root string) string {
	if root == skills.RootProject {
		return ", project"
	}
	return ""
}

// resolveSession returns the session id plus seeded history/summary: a new
// session for a fresh chat, the resumed state for --continue, or a full
// independent copy for --fork.
func resolveSession(ctx context.Context, st *memory.Store, ws, continueID, forkID string) (string, []llm.Message, string, string, error) {
	if forkID != "" {
		return forkSession(ctx, st, ws, forkID)
	}
	if continueID == "" {
		sess, err := st.NewSession(ctx, ws)
		if err != nil {
			return "", nil, "", "", fmt.Errorf("create session: %w", err)
		}
		return sess.ID, nil, "", "", nil
	}
	summary, until, err := st.Summary(ctx, continueID)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("load summary: %w", err)
	}
	history, err := st.HistoryAfter(ctx, continueID, until)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("load history: %w", err)
	}
	if len(history) == 0 && summary == "" {
		if !sessionExists(ctx, st, continueID) {
			return "", nil, "", "", fmt.Errorf("unknown session %q; run 'yagent sessions' to list them", continueID)
		}
	}
	return continueID, history, summary, "", nil
}

// forkSession copies the source session's full history into a brand-new
// session and returns it as the running context. The original is untouched.
func forkSession(ctx context.Context, st *memory.Store, ws, forkID string) (string, []llm.Message, string, string, error) {
	if !sessionExists(ctx, st, forkID) {
		return "", nil, "", "", fmt.Errorf("unknown session %q; run 'yagent sessions' to list them", forkID)
	}
	history, err := st.History(ctx, forkID)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("load fork source: %w", err)
	}
	sess, err := st.NewSession(ctx, ws)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("create fork: %w", err)
	}
	for _, m := range history {
		if _, err := st.Append(ctx, sess.ID, m); err != nil {
			return "", nil, "", "", fmt.Errorf("copy fork history: %w", err)
		}
	}
	return sess.ID, history, "", forkID, nil
}

func sessionExists(ctx context.Context, st *memory.Store, id string) bool {
	sessions, err := st.ListSessions(ctx)
	if err != nil {
		return false
	}
	for _, s := range sessions {
		if s.ID == id {
			return true
		}
	}
	return false
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

func (a *replApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	if err := ctx.Err(); err != nil {
		return agent.Approval{}, err
	}
	fmt.Fprintf(a.writer, "\n[%s] %s\n  %s\nAllow? [y/N] ",
		risk, call.Function.Name, previewArgs(call.Function.Arguments))
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return agent.Approval{}, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return agent.Approval{OK: answer == "y" || answer == "yes"}, nil
}

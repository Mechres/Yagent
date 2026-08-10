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
	"yagent/internal/index"
	"yagent/internal/llm"
	"yagent/internal/memory"
	"yagent/internal/skills"
	"yagent/internal/tools"
	"yagent/internal/web"
)

// Options tunes the chat UI.
type Options struct {
	// Plain forces the streaming REPL instead of the TUI (useful for pipes).
	Plain bool
	// YOLO auto-approves every write/destructive tool and applies skill
	// writes immediately instead of staging them. Use at your own risk.
	YOLO bool
}

// autoApprover grants every approval without prompting (--yolo).
type autoApprover struct{}

func (autoApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (bool, error) {
	return true, nil
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
	// TUI by default on a real terminal; --plain (or piped stdin) falls back
	// to the streaming REPL.
	if !opts.Plain && isTerminal(os.Stdin) {
		return RunTUI(ctx, client, cfg, continueID, opts.YOLO)
	}
	env, err := newChatEnv(ctx, cfg, continueID)
	if err != nil {
		return err
	}
	defer env.st.Close()
	defer env.vs.Close()
	defer env.idx.Close()

	w := os.Stdout
	env.registry.SetIndexProgress(func(line string) {
		fmt.Fprintf(w, "  [index] %s\n", line)
	})
	reader := bufio.NewReader(os.Stdin)
	var ap agent.Approver = &replApprover{reader: reader, writer: w}
	if opts.YOLO {
		ap = autoApprover{}
		env.registry.SetSkillsWriteApproval(false)
	}

	ag := newAgent(client, cfg, env, ap,
		func(delta string) { _, _ = io.WriteString(w, delta) },
		func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		})

	skillsCmd := &skillsHandler{
		store: env.sk, reg: env.registry, cfg: cfg, w: w,
		approval: &cfg.Skills.WriteApproval,
	}

	fmt.Printf("yagent chat — session %s (/exit, /clear, /help)\n", env.sessionID)
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
			fmt.Fprintln(w, "commands: /exit /clear /help /skills list|pending|diff|approve|reject|approval /skill-name")
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
	// End-of-session skill-creation opportunity, then the session-summary job.
	if err := ag.Finish(ctx); err != nil {
		fmt.Fprintf(w, "\nwarning: skill review: %v\n", err)
	}
	if err := memory.SummarizeSession(ctx, client, env.st, env.vs, env.sessionID); err != nil {
		fmt.Fprintf(w, "\nwarning: session summary: %v\n", err)
	}
	return nil
}

// chatEnv is the shared runtime state for the REPL and TUI.
type chatEnv struct {
	st             *memory.Store
	vs             *memory.VectorStore
	sk             *skills.Store
	idx            *index.Store
	web            *web.Client
	registry       *tools.Registry
	sessionID      string
	initialHistory []llm.Message
	initialSummary string
}

// newChatEnv opens all stores and builds the agent runtime shared by the
// plain REPL and the TUI.
func newChatEnv(ctx context.Context, cfg *config.Config, continueID string) (*chatEnv, error) {
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
	webClient, err := web.New(web.Config{Provider: cfg.Web.Provider, SearxngURL: cfg.Web.SearxngURL})
	if err != nil {
		return nil, fmt.Errorf("web search config: %w", err)
	}

	sessionID, initialHistory, initialSummary, err := resolveSession(ctx, st, ws, continueID)
	if err != nil {
		return nil, err
	}

	env := &chatEnv{
		st: st, vs: vs, sk: sk, idx: idx, web: webClient,
		sessionID: sessionID, initialHistory: initialHistory, initialSummary: initialSummary,
	}
	env.registry = tools.NewRegistry(ws, tools.Options{
		Vectors:             vs,
		SessionID:           sessionID,
		Skills:              sk,
		Index:               idx,
		Web:                 webClient,
		SkillsWriteApproval: cfg.Skills.WriteApproval,
	})
	return env, nil
}

// newAgent builds the agent loop over a chatEnv.
func newAgent(client *llm.Client, cfg *config.Config, env *chatEnv, approver agent.Approver, onToken func(string), onTool func(llm.ToolCall)) *agent.Agent {
	ws, _ := os.Getwd()
	return agent.New(client, env.registry, approver, agent.Config{
		OnToken:         onToken,
		OnTool:          onTool,
		Store:           env.st,
		SessionID:       env.sessionID,
		Vectors:         env.vs,
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
	store    *skills.Store
	reg      *tools.Registry
	cfg      *config.Config
	w        io.Writer
	approval *bool
}

func (h *skillsHandler) handle(line string, ag *agent.Agent) (bool, error) {
	rest := strings.TrimPrefix(line, "/")
	switch {
	case rest == "skills":
		fmt.Fprintln(h.w, "usage: /skills list | pending | diff <id> | approve <id|all> | reject <id|all> | approval on|off")
		return true, nil
	case strings.HasPrefix(rest, "skills "):
		return h.handleSkills(strings.Fields(rest)[1:])
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
			fmt.Fprintf(h.w, "%s  %-11s %s\n", shortID(p.ID), p.Action, p.Name)
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
	case "approve":
		if len(args) < 2 {
			return true, fmt.Errorf("approve needs an id or 'all'")
		}
		ids, err := h.pendingIDs(args[1])
		if err != nil {
			return true, err
		}
		for _, id := range ids {
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

package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/capsule"
	"github.com/Mechres/Yagent/internal/checkpoint"
	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/gitops"
	"github.com/Mechres/Yagent/internal/index"
	"github.com/Mechres/Yagent/internal/jobs"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/mcp"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/playbook"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
	"github.com/Mechres/Yagent/internal/undo"
	"github.com/Mechres/Yagent/internal/web"
)

// checkpointDefaultKeep caps the number of user-named workspace snapshots kept
// by /checkpoint save (the fixed "goal" snapshot is separate and reused). Old
// named checkpoints are pruned after each save so .yagent/checkpoints doesn't
// grow unbounded.
const checkpointDefaultKeep = 10

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
	// Research runs the agent as an autonomous research workflow on the topic:
	// parallel web searches, fetches, cross-checked findings, and a cited
	// report written to .yagent/research/ (research gate refuses DONE until
	// the deliverable exists). Rounds caps the loop.
	Research string
	// ResumeGoal resumes an interrupted goal run: the goal checkpoint is
	// restored and the given session is continued in goal mode.
	ResumeGoal string
	// Playbook runs a declarative multi-stage workflow
	// (.yagent/playbooks/<name>.yaml), then exits (P8).
	Playbook string
	// Trace, when set, receives a per-section dump of every assembled context
	// with token estimates (B2, `yagent chat --trace <file>`).
	Trace io.Writer
	// Codegen switches the loop to greenfield-code strategy (whole-file writes,
	// compile-gated final answers, plan-narration-as-stall). Also auto-enabled
	// for goal and playbook modes.
	Codegen bool
	// Checks are deterministic goal-success predicates applied to goal mode's
	// DONE gate (repeatable --check, e.g. "main.go contains config.New"). A
	// DONE verdict is refused while any fails — catches "copy instead of move".
	Checks []agent.SuccessCheck
}

// replAskUser prompts on stdin with a numbered choice list (or free text) and
// returns the user's pick — the REPL implementation of the clarify/plan tools.
func replAskUser(reader *bufio.Reader, w io.Writer) func(ctx context.Context, question string, choices []string) (string, error) {
	return func(ctx context.Context, question string, choices []string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fmt.Fprintf(w, "\n[ask] %s\n", question)
		for i, c := range choices {
			fmt.Fprintf(w, "  %d) %s\n", i+1, c)
		}
		fmt.Fprint(w, "answer (number or text): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		ans := strings.TrimSpace(line)
		if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1], nil
		}
		if ans == "" {
			return "(no answer)", nil
		}
		return ans, nil
	}
}

// autoApprover grants every approval without prompting (--yolo).
type autoApprover struct{}

func (autoApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	return agent.Approval{OK: true}, nil
}

// notifyOS fires an OS notification (notify-send on Linux, osascript on macOS)
// best-effort — local model runs are slow and the user may have walked away
// (Hermes P1). Silent when the tools aren't available.
func notifyOS(title, body string) {
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"osascript", "-e", fmt.Sprintf("display notification %q with title %q", body, title)}
	case "linux":
		args = []string{"notify-send", title, body}
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, args[0], args[1:]...).Run()
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

// rememberingApprover wraps a prompting approver and, after a write is
// approved once, remembers its (tool + normalized args) signature so identical
// calls are auto-approved for the rest of the session (Hermes P0: cuts approval
// fatigue on slow single-GPU runs without a blanket /yolo). The remembered set
// is in-memory and session-scoped.
type rememberingApprover struct {
	inner agent.Approver
	mu    sync.Mutex
	seen  map[string]bool
}

func newRememberingApprover(inner agent.Approver) *rememberingApprover {
	return &rememberingApprover{inner: inner, seen: map[string]bool{}}
}

func (r *rememberingApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	key := call.Function.Name + "\x00" + string(call.Function.Arguments)
	r.mu.Lock()
	remembered := r.seen[key]
	r.mu.Unlock()
	if remembered {
		return agent.Approval{OK: true}, nil
	}
	appr, err := r.inner.Approve(ctx, call, risk)
	if err == nil && appr.OK {
		r.mu.Lock()
		r.seen[key] = true
		r.mu.Unlock()
	}
	return appr, err
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
	// Codegen: explicit --codegen flag wins, and autonomous goal/playbook
	// modes always inherit it (they exist to build things, not to chat).
	if opts.Codegen || opts.Goal != "" || opts.ResumeGoal != "" || opts.Playbook != "" {
		cfg.Codegen = true
	}
	// Goal-mode resume: restore the goal checkpoint, continue the session, and
	// pick the goal back up from its last user message (C2).
	if opts.ResumeGoal != "" {
		ws, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		if err := checkpoint.Restore(ws, checkpoint.GoalName); err != nil {
			return fmt.Errorf("resume goal: %w", err)
		}
		env, err := newChatEnv(ctx, cfg, opts.ResumeGoal, opts.Fork)
		if err != nil {
			return err
		}
		defer env.st.Close()
		defer env.vs.Close()
		defer env.projVS.Close()
		defer env.idx.Close()
		defer env.closeMCP()
		goal, err := lastUserMessage(ctx, env)
		if err != nil || goal == "" {
			return fmt.Errorf("resume goal: could not find the goal in session %s", opts.ResumeGoal)
		}
		fmt.Printf("resuming goal from checkpoint — %q\n", goal)
		return runGoalMode(ctx, client, cfg, env, goal, opts.Rounds, opts.YOLO, opts.Trace, opts.Checks)
	}
	// Playbook mode: run a declarative multi-stage workflow, then exit (P8).
	if opts.Playbook != "" {
		env, err := newChatEnv(ctx, cfg, continueID, opts.Fork)
		if err != nil {
			return err
		}
		defer env.st.Close()
		defer env.vs.Close()
		defer env.projVS.Close()
		defer env.idx.Close()
		defer env.closeMCP()
		return runPlaybookMode(ctx, client, cfg, env, opts.Playbook, opts.YOLO, opts.Trace, opts.Checks)
	}
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
		defer env.closeMCP()
		return runGoalMode(ctx, client, cfg, env, opts.Goal, opts.Rounds, opts.YOLO, opts.Trace, opts.Checks)
	}
	// Research mode: autonomous research workflow (parallel searches, fetches,
	// cross-checked findings, cited report) then exit.
	if opts.Research != "" {
		env, err := newChatEnv(ctx, cfg, continueID, opts.Fork)
		if err != nil {
			return err
		}
		defer env.st.Close()
		defer env.vs.Close()
		defer env.projVS.Close()
		defer env.idx.Close()
		defer env.closeMCP()
		return runResearchMode(ctx, client, cfg, env, opts.Research, opts.Rounds, opts.YOLO, opts.Trace)
	}
	// TUI by default on a real terminal; --plain (or piped stdin) falls back
	// to the streaming REPL.
	if !opts.Plain && isTerminal(os.Stdin) {
		return RunTUI(ctx, client, cfg, continueID, opts)
	}
	env, err := newChatEnv(ctx, cfg, continueID, opts.Fork)
	if err != nil {
		return err
	}
	defer env.st.Close()
	defer env.vs.Close()
	defer env.projVS.Close()
	defer env.idx.Close()
	defer env.closeMCP()
	defer env.jobs.StopAll()

	w := os.Stdout
	env.registry.SetIndexProgress(func(line string) {
		fmt.Fprintf(w, "  [index] %s\n", line)
	})
	startBackgroundIndex(ctx, env, func(line string) {
		fmt.Fprintf(w, "  [index] %s\n", line)
	})
	reader := bufio.NewReader(os.Stdin)
	ap := newToggleableApprover(newRememberingApprover(&replApprover{reader: reader, writer: w}))
	ap.SetYOLO(opts.YOLO)
	if opts.YOLO {
		env.registry.SetSkillsWriteApproval(false)
	}
	env.registry.SetAskUser(replAskUser(reader, w))

	var thinkBuf strings.Builder
	loopWarned := false
	lastLine := ""
	ag := newAgent(client, cfg, env, ap,
		func(delta string) { _, _ = io.WriteString(w, delta) },
		// Thinking is shown dimmed/italic above the answer (display-only),
		// unless ui.show_reasoning is off.
		func(delta string) {
			if !cfg.UI.ShowReasoning {
				return
			}
			thinkBuf.WriteString(delta)
			if !loopWarned && agent.RepeatLoop(thinkBuf.String()) {
				loopWarned = true
				fmt.Fprintf(w, "\n[the model appears to be repeating itself — /set sampling.repetition_penalty 1.05 often fixes it, or press Ctrl-C]\n")
			}
			_, _ = fmt.Fprintf(w, "\x1b[2m\x1b[3m%s\x1b[0m", delta)
		},
		func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		},
		opts.Trace)

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
	if env.gitEnabled {
		env.commitDirtyStart()
		fmt.Println("(git auto-commit on — each turn is committed; /undo reverts it)")
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
		case "/compact":
			note, err := ag.Compact(context.Background())
			if err != nil {
				fmt.Fprintf(w, "error: %v\n", err)
			} else {
				fmt.Fprintln(w, note)
			}
			continue
		case "/help":
			fmt.Fprintln(w, "commands: /exit /clear /compact /help /yolo /retry /export [file] /settings /set /model /key /diff /plan /goal <what> /research <topic> /steer <text> /checkpoint /playbook /sessions /undo [list|<N>] /skills list|pending|diff|verify|approve|reject|approval /skill-name")
			continue
		case "/steer":
			text := strings.TrimSpace(strings.TrimPrefix(line, "/steer"))
			ag.Steer(text)
			if text == "" {
				fmt.Fprintln(w, "steer cleared")
			} else {
				fmt.Fprintf(w, "steer set: %s (pinned to the top of TASK STATE for the next requests)\n", text)
			}
			continue
		case "/retry":
			if lastLine == "" {
				fmt.Fprintln(w, "nothing to retry")
				continue
			}
			// A single loop/malformed call is usually sampling instability.
			client.Sampling.Temperature = 0.3
			client.Sampling.RepetitionPenalty = 1.05
			fmt.Fprintln(w, "retrying with a stable sampling profile (temp 0.3, repetition_penalty 1.05)")
			line = lastLine
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

		thinkBuf.Reset()
		loopWarned = false
		lastLine = line
		env.undo.StartTurn()
		_, err = ag.Run(ctx, line)
		env.undo.EndTurn()
		env.turnSeq++
		if ws, werr := os.Getwd(); werr == nil {
			env.commitTurn(ws, fmt.Sprintf("turn %d", env.turnSeq))
		}
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
	if env.maybeDeleteEmptySession(ctx) {
		fmt.Fprintf(w, "\n(no messages — empty session not saved)\n")
		return nil
	}
	fmt.Fprintf(w, "\nsession: %s (resume with: yagent chat --continue %s)\n", env.sessionID, env.sessionID)
	return nil
}

// runPlaybookMode executes a declarative playbook's phases in order, each as an
// autonomous goal run scoped to the phase's tool subset (P8). The workspace is
// snapshotted before every phase so a failed phase can be rolled back.
func runPlaybookMode(ctx context.Context, client *llm.Client, cfg *config.Config, env *chatEnv, name string, yolo bool, trace io.Writer, checks []agent.SuccessCheck) error {
	ws, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	pb, err := playbook.Load(ws, name)
	if err != nil {
		return err
	}
	fmt.Printf("playbook %q — %s (%d phases)\n", pb.Name, pb.Description, len(pb.Phases))

	w := os.Stdout
	reader := bufio.NewReader(os.Stdin)
	ap := newToggleableApprover(&replApprover{reader: reader, writer: w})
	// Autonomous playbook runs are unattended: always auto-approve writes
	// (the per-phase checkpoint is the rollback safety net). No AskUser, so
	// clarify/plan aren't offered mid-run.
	ap.SetYOLO(true)
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
		},
		trace)
	ag.SetSuccessChecks(checks)

	executePlaybook(ctx, client, cfg, env, ag, w, pb)
	if err := ag.Finish(ctx); err != nil {
		fmt.Fprintf(w, "\nwarning: skill review: %v\n", err)
	}
	if err := memory.SummarizeSession(ctx, client, env.st, env.vs, env.sessionID); err != nil {
		fmt.Fprintf(w, "\nwarning: session summary: %v\n", err)
	}
	fmt.Fprintf(w, "\nplaybook %q done — session: %s (resume with: yagent chat --continue %s)\n", pb.Name, env.sessionID, env.sessionID)
	return nil
}

// executePlaybook runs a playbook's phases in order against an existing agent
// (P8): each phase is an autonomous goal run scoped to its tool subset, with a
// workspace snapshot before it so a failed phase can be rolled back. The agent's
// registry is restored after every phase.
func executePlaybook(ctx context.Context, client *llm.Client, cfg *config.Config, env *chatEnv, ag *agent.Agent, w io.Writer, pb *playbook.Playbook) {
	ws, _ := os.Getwd()
	for i, phase := range pb.Phases {
		rounds := phase.Rounds
		if rounds <= 0 {
			rounds = agent.DefaultGoalRounds
		}
		headline := strings.TrimSpace(strings.SplitN(phase.Goal, "\n", 2)[0])
		fmt.Fprintf(w, "\n—— phase %d/%d: %s ——\n", i+1, len(pb.Phases), headline)
		if phase.Success != "" {
			fmt.Fprintf(w, "  done when: %s\n", phase.Success)
		}
		if ws != "" {
			if _, err := checkpoint.Save(ws, checkpoint.GoalName); err != nil {
				fmt.Fprintf(w, "(warning: could not snapshot workspace: %v)\n", err)
			}
		}
		if len(phase.Tools) > 0 {
			reg, err := env.registry.Restrict(phase.Tools)
			if err != nil {
				fmt.Fprintf(w, "\nphase %d failed: %v\n", i+1, err)
				break
			}
			ag.SetRegistry(reg)
		}
		_, err := ag.RunGoal(ctx, phase.Goal, rounds, func(r int, _ string) {
			fmt.Fprintf(w, "\n—— round %d ——\n", r)
		})
		ag.SetRegistry(env.registry)
		if err != nil {
			fmt.Fprintf(w, "\nphase %d failed: %v\n", i+1, err)
			break
		}
		// Deterministic success predicates (Luna #11): the model's DONE is a
		// proposal — when checks are set, the phase isn't complete until they
		// pass. Re-run the phase once to let the agent fix failures.
		if phase.HasChecks() {
			fails := evaluatePhaseChecks(ws, env, phase)
			if len(fails) > 0 {
				fmt.Fprintf(w, "success checks failed:\n")
				for _, f := range fails {
					fmt.Fprintf(w, "  - %s\n", f)
				}
				fmt.Fprintf(w, "re-running the phase once to fix them…\n")
				if _, err := ag.RunGoal(ctx, phase.Goal, rounds, func(r int, _ string) {
					fmt.Fprintf(w, "\n—— round %d ——\n", r)
				}); err == nil {
					fails = evaluatePhaseChecks(ws, env, phase)
				}
				if len(fails) > 0 {
					fmt.Fprintf(w, "checks still failing — aborting playbook\n")
					for _, f := range fails {
						fmt.Fprintf(w, "  - %s\n", f)
					}
					break
				}
			}
		}
	}
}

// evaluatePhaseChecks runs a phase's deterministic success predicates against
// the workspace (including the diagnostics check via the registry) and returns
// the failures.
func evaluatePhaseChecks(ws string, env *chatEnv, phase playbook.Phase) []string {
	var fails []string
	for _, c := range phase.Checks {
		fails = append(fails, c.Evaluate(ws)...)
		if c.DiagnosticsPass {
			res := runDiagnosticsCheck(env)
			// Use the same determination as the goal gate: a checker that
			// outputs an informational banner on success must not fail the
			// phase (agy #3).
			if agent.DiagnosticsFailed(res) {
				fails = append(fails, "workspace_diagnostics reported problems")
			}
		}
		if c.TestsPass != "" {
			if err := runTestsCheck(env, c.TestsPass); err != nil {
				fails = append(fails, err.Error())
			}
		}
	}
	return fails
}

// runTestsCheck runs the project's unit tests (optionally filtered by a test
// name) via test_runner and reports success/failure (agy #2).
func runTestsCheck(env *chatEnv, filter string) error {
	tool, ok := env.registry.Get("test_runner")
	if !ok {
		return fmt.Errorf("tests check requires test_runner (not registered)")
	}
	args := `{"scope":"package"}`
	if filter != "" {
		args = fmt.Sprintf(`{"scope":"symbol","symbol":%q}`, filter)
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		return fmt.Errorf("tests check failed: %v", err)
	}
	// test_runner returns pruned output; failures surface FAIL / error: lines.
	if strings.Contains(res, "FAIL") || strings.HasPrefix(strings.TrimSpace(res), "error:") {
		return fmt.Errorf("tests failed: %s", truncateForCheck(res, 200))
	}
	return nil
}

func truncateForCheck(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runDiagnosticsCheck executes the workspace_diagnostics tool for a success
// predicate (the tool commands are fixed and read-only).
func runDiagnosticsCheck(env *chatEnv) string {
	tool, ok := env.registry.Get("workspace_diagnostics")
	if !ok {
		return ""
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		return "error: " + err.Error()
	}
	return res
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
	summ           *llm.Client    // optional offloaded summarizer (budget + /compact)
	capsules       *capsule.Store // persistent tool-failure memory (.yagent/capsules.json)
	// mcpClients are connected Model Context Protocol servers whose advertised
	// tools are registered into the tool registry (server-prefixed names).
	mcpClients []*mcp.Client
	// gitEnabled is true when git auto-commit is active (git.auto_commit on AND
	// the workspace is a git repo): each turn's writes become a real commit and
	// /undo becomes a git revert (durable, crash-safe). When false, the
	// in-memory undo buffer is the fallback.
	gitEnabled bool
	// isNewSession is true when this is a brand-new session (not --continue or
	// --fork). At teardown, a new session that never received a message is
	// deleted so opening/closing the TUI without chatting leaves nothing behind.
	isNewSession bool
	// gitBaseline is HEAD after the startup dirty-commit snapshot — the point
	// the session started from. /diff shows the agent's cumulative changes
	// against it (the plandex-style review sandbox).
	gitBaseline string
	turnSeq     int
}

// maybeDeleteEmptySession removes a brand-new session that never received a
// message (opened the TUI/REPL and closed it without chatting). Resumed and
// forked sessions are never touched. Returns true when deleted.
func (env *chatEnv) maybeDeleteEmptySession(ctx context.Context) bool {
	if !env.isNewSession {
		return false
	}
	if err := env.st.DeleteIfEmpty(ctx, env.sessionID); err != nil {
		slog.Warn("delete empty session", "error", err)
		return false
	}
	return true
}

// commitDirtyStart commits any pre-existing uncommitted changes up front (aider
// style), so the user's work is never mixed into or overwritten by agent
// commits. Best-effort: logs a warning on failure rather than blocking startup.
func (env *chatEnv) commitDirtyStart() {
	if !env.gitEnabled {
		return
	}
	ws, err := os.Getwd()
	if err != nil {
		return
	}
	if _, err := gitops.CommitDirty(ws, "yagent: snapshot pre-session dirty state"); err != nil {
		slog.Warn("git auto-commit: could not snapshot dirty state", "error", err)
	}
	env.gitBaseline = gitops.Head(ws)
}

// sessionDiff returns the agent's cumulative changes since the session
// baseline (git diff of baseline...HEAD), for the /diff review command.
func (env *chatEnv) sessionDiff() (stat, diff string) {
	if !env.gitEnabled || env.gitBaseline == "" {
		return "", ""
	}
	ws, _ := os.Getwd()
	return gitops.DiffStat(ws, env.gitBaseline), gitops.DiffSince(ws, env.gitBaseline)
}

// commitTurn commits the agent's changes for one completed turn (aider style:
// /undo then reverts it). Best-effort.
func (env *chatEnv) commitTurn(ws, subject string) {
	if !env.gitEnabled {
		return
	}
	if _, err := gitops.AgentCommit(ws, subject); err != nil {
		slog.Warn("git auto-commit: could not commit turn", "error", err)
	}
}

// maybeUndo reverts the last N agent turns via git when enabled, else falls
// back to the in-memory buffer. Returns a human message.
func (env *chatEnv) maybeUndo(ws string, n int) (string, error) {
	if env.gitEnabled {
		reverted, err := gitops.RevertN(ws, n)
		if err != nil {
			return "", err
		}
		if len(reverted) == 0 {
			return "", fmt.Errorf("nothing to undo")
		}
		return fmt.Sprintf("reverted %d commit(s): %s", len(reverted), strings.Join(reverted, "; ")), nil
	}
	if n <= 1 {
		entries, err := env.undo.UndoLastTurn()
		if err != nil {
			return "", err
		}
		var paths []string
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
		return fmt.Sprintf("reverted %d file(s): %s", len(paths), strings.Join(paths, ", ")), nil
	}
	entries, err := env.undo.UndoN(n)
	if err != nil {
		return "", err
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return fmt.Sprintf("reverted %d file(s) across %d turn(s): %s", len(paths), n, strings.Join(paths, ", ")), nil
}

// undoList returns one line per undoable turn (git commits or buffer turns).
func (env *chatEnv) undoList(ws string) []string {
	if env.gitEnabled {
		commits, err := gitops.AgentCommits(ws)
		if err != nil {
			return nil
		}
		if len(commits) == 0 {
			return []string{"no yagent commits to undo"}
		}
		out := make([]string, 0, len(commits))
		for i, c := range commits {
			out = append(out, fmt.Sprintf("commit %d: %s (%s)", i+1, c.Subject, c.Hash))
		}
		return out
	}
	return env.undo.Turns()
}

// runGoalMode drives the agent autonomously toward a goal: each round runs the
// full loop, streams its answer, and a DONE/CONTINUE verdict decides whether to
// continue. The workspace is snapshotted before the run and after every
// completed round (so `--resume-goal` can roll back to the last good state).
// Ends with the session id for later resume.
func runGoalMode(ctx context.Context, client *llm.Client, cfg *config.Config, env *chatEnv, goal string, rounds int, yolo bool, trace io.Writer, checks []agent.SuccessCheck) error {
	w := os.Stdout
	reader := bufio.NewReader(os.Stdin)
	ap := newToggleableApprover(&replApprover{reader: reader, writer: w})
	// Autonomous goal runs are unattended: a write tool mid-round must not
	// block on an interactive y/n (the terminal appears frozen and the run
	// hangs until the user pkill -9s it). Always auto-approve writes — the
	// goal checkpoint and /undo provide the rollback safety net. AskUser is
	// left unset so clarify/plan aren't offered (no interactive handoff in
	// an autonomous run).
	ap.SetYOLO(true)
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
		},
		trace)
	ag.SetSuccessChecks(checks)

	fmt.Printf("goal mode — working toward: %s\n", goal)
	if env.forkSource != "" {
		fmt.Printf("(forked from %s)\n", env.forkSource)
	}
	saveGoalCheckpoint := func(label string) {
		ws, err := os.Getwd()
		if err != nil {
			return
		}
		if dir, err := checkpoint.Save(ws, checkpoint.GoalName); err != nil {
			fmt.Fprintf(w, "(warning: could not %s: %v)\n", label, err)
		} else if label == "snapshot workspace" {
			fmt.Fprintf(w, "workspace snapshotted (%s) — revert later with /checkpoint restore goal\n", dir)
		}
	}
	saveGoalCheckpoint("snapshot workspace")
	answer, err := ag.RunGoal(ctx, goal, rounds, func(r int, _ string) {
		fmt.Fprintf(w, "\n—— round %d ——\n", r)
		saveGoalCheckpoint("save round checkpoint")
	})
	if err != nil {
		fmt.Fprintf(w, "\ngoal loop: %v\n", err)
		notifyOS("yagent — goal finished", "goal loop ended with an error")
	} else {
		notifyOS("yagent — goal done", "the autonomous goal run completed")
	}
	_ = answer
	if err == nil {
		offerDistillation(ctx, ag, env, w)
	}
	if err := ag.Finish(ctx); err != nil {
		fmt.Fprintf(w, "\nwarning: skill review: %v\n", err)
	}
	if err := memory.SummarizeSession(ctx, client, env.st, env.vs, env.sessionID); err != nil {
		fmt.Fprintf(w, "\nwarning: session summary: %v\n", err)
	}
	fmt.Fprintf(w, "\nsession: %s (resume with: yagent chat --continue %s)\n", env.sessionID, env.sessionID)
	return nil
}

// runResearchMode drives the agent as an autonomous research workflow: each
// round searches in parallel, fetches pages, cross-checks claims and records
// findings; the research gate refuses a DONE verdict until the cited report
// exists under .yagent/research/. Writes are auto-approved (the report file is
// the deliverable) and the session id is printed at the end.
func runResearchMode(ctx context.Context, client *llm.Client, cfg *config.Config, env *chatEnv, topic string, rounds int, yolo bool, trace io.Writer) error {
	w := os.Stdout
	ap := newToggleableApprover(&autoApprover{}) // report write is the deliverable
	ap.SetYOLO(true)
	ag := newAgent(client, cfg, env, ap,
		func(delta string) { _, _ = io.WriteString(w, delta) },
		func(delta string) {
			_, _ = fmt.Fprintf(w, "\x1b[2m\x1b[3m%s\x1b[0m", delta)
		},
		func(call llm.ToolCall) {
			fmt.Fprintf(w, "\n→ %s %s\n", call.Function.Name, previewArgs(call.Function.Arguments))
		},
		trace)
	fmt.Printf("research mode — investigating: %s\n", topic)
	answer, err := ag.RunResearch(ctx, topic, rounds, func(r int, _ string) {
		fmt.Fprintf(w, "\n—— round %d ——\n", r)
	})
	if err != nil {
		fmt.Fprintf(w, "\nresearch loop: %v\n", err)
		notifyOS("yagent — research finished", "the research loop ended with an error")
	} else {
		notifyOS("yagent — research done", "the research report is ready")
	}
	_ = answer
	// Report the research deliverables the deterministic gate verified.
	if report := ag.ResearchReport(); report != "" {
		fmt.Fprintf(w, "\nreport: %s\n", report)
		if prov := ag.ResearchProvenance(); prov != "" {
			fmt.Fprintf(w, "provenance: %s\n", prov)
		}
	}
	if srcs := ag.ResearchSources(); len(srcs) > 0 {
		fmt.Fprintf(w, "sources (%d):\n", len(srcs))
		for _, u := range srcs {
			fmt.Fprintf(w, "  - %s\n", u)
		}
	}
	if err := ag.Finish(ctx); err != nil {
		fmt.Fprintf(w, "\nwarning: skill review: %v\n", err)
	}
	fmt.Fprintf(w, "\nsession: %s (resume with: yagent chat --continue %s)\n", env.sessionID, env.sessionID)
	return nil
}

// offerDistillation asks the model, after a successful autonomous goal run, to
// save a reusable declarative playbook capturing the workflow (the model writes
// it with fs_write; it may decline with "no playbook"). Gated on the session
// having done real work (>= 3 tool calls) so trivial goals aren't distilled.
func offerDistillation(ctx context.Context, ag *agent.Agent, env *chatEnv, w io.Writer) {
	if ag == nil || env == nil {
		return
	}
	toolCalls := 0
	for _, m := range ag.History() {
		if m.Role == "tool" {
			toolCalls++
		}
	}
	if toolCalls < 3 {
		return
	}
	prompt := "One-shot distillation opportunity: this autonomous run succeeded. If the work is a repeatable multi-step workflow, write a declarative playbook to .yagent/playbooks/<name>.yaml that would reproduce it: one or more phases, each with a goal, optionally a tools subset and success checks (file_contains / file_exists / diagnostics). Use fs_write to create the file. If it was a one-off task, reply exactly: no playbook"
	fmt.Fprintf(w, "\n[offering to distill this run into a reusable playbook…]\n")
	if _, err := ag.Run(ctx, prompt); err != nil {
		fmt.Fprintf(w, "\n(distillation skipped: %v)\n", err)
	}
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

// closeMCP tears down connected MCP server clients (best-effort).
func (env *chatEnv) closeMCP() {
	for _, c := range env.mcpClients {
		_ = c.Close()
	}
	env.mcpClients = nil
}

// toToolHooks converts config-declared hooks into the tools package's Hook
// shape (config is a leaf package; tools can't import it).
func toToolHooks(hooks []config.Hook) []tools.Hook {
	out := make([]tools.Hook, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, tools.Hook{When: h.When, Tool: h.Tool, Command: h.Command})
	}
	return out
}

// connectMCP connects every enabled MCP server from the config (best-effort:
// a server that fails to connect is logged and skipped, never fatal — the
// agent must keep working without it).
func connectMCP(ctx context.Context, cfg *config.Config) []*mcp.Client {
	var clients []*mcp.Client
	for name, srv := range cfg.MCP {
		if !srv.Enabled {
			continue
		}
		if len(srv.Command) == 0 && srv.URL == "" {
			slog.Warn("mcp server has no command or url", "name", name)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		client, err := mcp.Connect(cctx, mcp.Config{
			Name: name, Command: srv.Command, URL: srv.URL, Headers: srv.Headers,
		})
		cancel()
		if err != nil {
			slog.Warn("mcp server failed to connect", "name", name, "error", err)
			continue
		}
		clients = append(clients, client)
		slog.Info("mcp server connected", "name", name, "tools", len(client.Tools()))
	}
	return clients
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
	webClient, err := web.New(web.Config{Provider: cfg.Web.Provider, SearxngURL: cfg.Web.SearxngURL, MaxFetchBytes: cfg.Web.MaxFetchKib * 1024, LangSearchKey: cfg.Web.LangSearchKey, Papers: cfg.Web.Papers, SemanticScholarKey: cfg.Web.SemanticScholarKey})
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

	// Summarizer: when configured, history condensation (budget + /compact) is
	// offloaded to a second model/server (e.g. a laptop). The main loop never
	// uses it for tools, so a small/slow summarizer is safe.
	var summClient *llm.Client
	if cfg.Summarizer.Model != "" {
		summURL := cfg.Summarizer.ServerURL
		if summURL == "" {
			summURL = cfg.ServerURL
		}
		summClient = llm.NewClient(summURL, cfg.Summarizer.Model)
	}

	sessionID, initialHistory, initialSummary, forkSource, err := resolveSession(ctx, st, ws, continueID, forkID)
	if err != nil {
		return nil, err
	}

	// Failure capsules: project-scoped persistent tool-failure memory under
	// .yagent/capsules.json (gitignored). Best-effort — a broken store just
	// disables the hint.
	var capsulesStore *capsule.Store
	if cps, cerr := capsule.Open(filepath.Join(ws, ".yagent", "capsules.json")); cerr == nil {
		capsulesStore = cps
	}

	env := &chatEnv{
		st: st, vs: vs, projVS: projVS, sk: sk, idx: idx, web: webClient,
		sessionID: sessionID, initialHistory: initialHistory, initialSummary: initialSummary,
		forkSource: forkSource, undo: undo.New(), jobs: jobs.New(),
		summ:       summClient,
		capsules:   capsulesStore,
		mcpClients: connectMCP(ctx, cfg),
	}
	// A fresh session (no --continue / --fork) that never receives a message is
	// deleted at teardown; resumed sessions are never touched.
	env.isNewSession = continueID == "" && forkID == ""
	env.gitEnabled = cfg.AutoCommitGit && gitops.IsRepo(ws)
	env.registry = tools.NewRegistry(ws, tools.Options{
		Vectors:             vs,
		ProjectVectors:      projVS,
		SessionID:           sessionID,
		Skills:              sk,
		Index:               idx,
		Web:                 webClient,
		Papers:              cfg.Web.Papers,
		Consult:             consultClient,
		ConsultCmd:          cfg.Consult.Cmd,
		Undo:                env.undo,
		Jobs:                env.jobs,
		ShellSandbox:        cfg.Shell.Sandbox,
		SkillsWriteApproval: cfg.Skills.WriteApproval,
		MCP:                 env.mcpClients,
		Hooks:               toToolHooks(cfg.Hooks),
	})
	return env, nil
}

// newLLMClient builds an OpenAI-compatible client from the (possibly just
// provider-switched) config, mirroring cmd/yagent's chat flag wiring. Shared by
// startup and the /model runtime swap.
func newLLMClient(cfg *config.Config) *llm.Client {
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
	return client
}

// newAgent builds the agent loop over a chatEnv. trace, when non-nil, receives
// the per-context prompt dump (B2); the client is also wired as the token
// counter so the context gauge uses the server tokenizer when available (C1).
func newAgent(client *llm.Client, cfg *config.Config, env *chatEnv, approver agent.Approver, onToken, onReasoning func(string), onTool func(llm.ToolCall), trace io.Writer) *agent.Agent {
	ws, _ := os.Getwd()
	// M7 v1: subagents are read-only child agents that return a summary.
	// M7 beyond v2: an optional tools slice scopes each child's registry.
	env.registry.SetSubagent(func(ctx context.Context, task, workspace string, toolset []string, role tools.SubagentRole) (string, error) {
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
		childClient := client
		if role.Temperature > 0 {
			childClient = client.Clone() // P2: a role may tune the child's sampling
			childClient.Sampling.Temperature = role.Temperature
		}
		answer, tokens, err := agent.RunSubagent(ctx, childClient, reg, task, workspace, role)
		if err != nil {
			return "error: subagent failed: " + err.Error(), nil
		}
		return fmt.Sprintf("%s\n\n(subagent used ~%d tokens)", answer, tokens), nil
	})
	acfg := agent.Config{
		OnToken:          onToken,
		OnReasoning:      onReasoning,
		OnTool:           onTool,
		Store:            env.st,
		SessionID:        env.sessionID,
		Vectors:          env.vs,
		ProjectVectors:   env.projVS,
		Skills:           env.sk,
		Index:            env.idx,
		IndexAutoInject:  true,
		InitialHistory:   env.initialHistory,
		InitialSummary:   env.initialSummary,
		Window:           cfg.ContextWindow,
		Reserve:          cfg.ContextWindow / 8, // P2: auto-reserve as a % of the window
		Counter:          client,
		Trace:            trace,
		VerifyWrites:     true, // deterministic verify-don't-trust "done" gate
		GoalGate:         true, // refuse DONE while the static check fails
		TestGate:         true, // refuse DONE while the unit tests fail
		GoalMemorize:     true, // persist round facts to L3 memory (multi-turn recall)
		MemorizeResearch: true, // persist research sources/findings to L3 memory
		Codegen:          cfg.Codegen,
		Research:         true, // research_note tool + SOURCES/RESEARCH NOTES ledger
		VramThresholdTPS: cfg.VramThresholdTPS,
		Capsules:         env.capsules,
	}
	if env.summ != nil {
		// Only set the offloaded summarizer when actually configured: passing a
		// typed-nil *llm.Client makes the interface non-nil and defeats the
		// agent's nil-default, panicking the budget summarizer.
		acfg.Summarizer = env.summ
	}
	return agent.New(client, env.registry, approver, acfg, ws)
}

// lastUserMessage returns the goal text of a goal-mode session (goal mode
// re-sends the goal as a user message every round, so the last user message IS
// the goal). Used by `--resume-goal` to recover the goal from a session.
func lastUserMessage(ctx context.Context, env *chatEnv) (string, error) {
	for i := len(env.initialHistory) - 1; i >= 0; i-- {
		if env.initialHistory[i].Role == "user" {
			return env.initialHistory[i].Content, nil
		}
	}
	history, err := env.st.History(ctx, env.sessionID)
	if err != nil {
		return "", err
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i].Content, nil
		}
	}
	return "", nil
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
	case rest == "plan":
		on := !ag.PlanMode()
		ag.SetPlanMode(on)
		if on {
			fmt.Fprintln(h.w, "plan mode ON — read-only tools only; approve a plan (/plan again or plan tool) to edit")
		} else {
			fmt.Fprintln(h.w, "plan mode OFF — editing enabled")
		}
		return true, nil
	case rest == "goal" || strings.HasPrefix(rest, "goal "):
		parts := strings.Fields(rest)
		if len(parts) < 2 {
			return true, fmt.Errorf("usage: /goal <what to achieve>")
		}
		return h.runGoal(ag, strings.TrimSpace(strings.TrimPrefix(rest, "goal")))
	case rest == "research" || strings.HasPrefix(rest, "research "):
		parts := strings.Fields(rest)
		if len(parts) < 2 {
			return true, fmt.Errorf("usage: /research <topic> — runs an autonomous research workflow (parallel searches, fetches, cross-checked findings) and writes a cited report to .yagent/research/")
		}
		return h.runResearch(ag, strings.TrimSpace(strings.TrimPrefix(rest, "research")))
	case rest == "settings" || strings.HasPrefix(rest, "set "):
		if rest == "settings" {
			return h.showSettings()
		}
		return h.setSetting(rest)
	case rest == "model" || strings.HasPrefix(rest, "model "):
		return h.showModels(rest, ag)
	case rest == "key" || strings.HasPrefix(rest, "key "):
		return h.setAPIKey(rest)
	case rest == "diff" || strings.HasPrefix(rest, "diff "):
		return h.showDiff(rest)
	case rest == "undo":
		return h.undoLastTurn()
	case rest == "undo list":
		return h.undoList()
	case strings.HasPrefix(rest, "undo "):
		return h.undoN(strings.TrimSpace(strings.TrimPrefix(rest, "undo")))
	case rest == "checkpoint" || strings.HasPrefix(rest, "checkpoint "):
		return h.handleCheckpoint(rest)
	case rest == "sessions":
		return h.listSessions()
	case rest == "playbook" || strings.HasPrefix(rest, "playbook "):
		parts := strings.Fields(rest)
		if len(parts) < 2 {
			return h.listPlaybooks()
		}
		return h.runPlaybook(ag, parts[1])
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
// With git auto-commit it reverts the last agent commit (durable); otherwise
// it uses the in-memory buffer.
func (h *skillsHandler) undoLastTurn() (bool, error) {
	if h.env == nil {
		fmt.Fprintln(h.w, "undo is not available")
		return true, nil
	}
	ws, err := os.Getwd()
	if err != nil {
		return true, err
	}
	msg, err := h.env.maybeUndo(ws, 1)
	if err != nil {
		fmt.Fprintln(h.w, err.Error())
		return true, nil
	}
	fmt.Fprintln(h.w, msg)
	return true, nil
}

// undoList shows the undo history (/undo list): the agent's git commits when
// git auto-commit is on, else the in-memory turn buffer.
func (h *skillsHandler) undoList() (bool, error) {
	if h.env == nil {
		fmt.Fprintln(h.w, "undo is not available")
		return true, nil
	}
	ws, err := os.Getwd()
	if err != nil {
		return true, err
	}
	turns := h.env.undoList(ws)
	if len(turns) == 0 {
		fmt.Fprintln(h.w, "no turns to undo")
		return true, nil
	}
	label := "turns"
	if h.env.gitEnabled {
		label = "commits"
	}
	fmt.Fprintf(h.w, "%s (most recent first):\n", label)
	for _, t := range turns {
		fmt.Fprintln(h.w, "  "+t)
	}
	fmt.Fprintln(h.w, "use: /undo <N> to revert the N most recent turns, /undo to revert the last turn")
	return true, nil
}

// undoN reverts the N most recent turns (/undo <N>, proposal #6).
func (h *skillsHandler) undoN(nstr string) (bool, error) {
	n, err := strconv.Atoi(nstr)
	if err != nil || n <= 0 {
		return true, fmt.Errorf("usage: /undo <N> where N is a positive number of turns (see /undo list)")
	}
	if h.env == nil {
		fmt.Fprintln(h.w, "undo is not available")
		return true, nil
	}
	ws, err := os.Getwd()
	if err != nil {
		return true, err
	}
	msg, err := h.env.maybeUndo(ws, n)
	if err != nil {
		fmt.Fprintln(h.w, err.Error())
		return true, nil
	}
	fmt.Fprintln(h.w, msg)
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
		// Retention: keep only the most recent 10 named snapshots (the fixed
		// "goal" snapshot is separate and reused). Prevents .yagent/checkpoints
		// from growing unbounded across many /checkpoint save calls.
		if pruned, perr := checkpoint.Prune(ws, checkpointDefaultKeep); perr != nil {
			fmt.Fprintf(h.w, "  (warning: could not prune old checkpoints: %v)\n", perr)
		} else if len(pruned) > 0 {
			fmt.Fprintf(h.w, "  pruned %d old checkpoint(s): %s\n", len(pruned), strings.Join(pruned, ", "))
		}
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

// showDiff renders the agent's cumulative changes since the session baseline
// (the plandex-style review sandbox: see the whole session's work before you
// keep or discard it). /diff = everything since baseline; /diff <N> = the last
// N agent commits; /diff discard = revert all of the session's changes.
func (h *skillsHandler) showDiff(rest string) (bool, error) {
	if h.env == nil {
		fmt.Fprintln(h.w, "diff requires a git-backed session (git_auto_commit on in a git repo)")
		return true, nil
	}
	if !h.env.gitEnabled {
		fmt.Fprintln(h.w, "git auto-commit is off — /diff needs the git layer (set git_auto_commit: true)")
		return true, nil
	}
	parts := strings.Fields(rest)
	if len(parts) >= 2 && parts[1] == "discard" {
		ws, err := os.Getwd()
		if err != nil {
			return true, err
		}
		commits, _ := gitops.AgentCommits(ws)
		n := len(commits)
		if n == 0 {
			fmt.Fprintln(h.w, "nothing to discard")
			return true, nil
		}
		msg, err := h.env.maybeUndo(ws, n)
		if err != nil {
			return true, err
		}
		fmt.Fprintln(h.w, msg)
		return true, nil
	}
	if len(parts) >= 2 {
		n, err := strconv.Atoi(parts[1])
		if err != nil || n <= 0 {
			return true, fmt.Errorf("usage: /diff [<N>|discard] — the cumulative changes since the session started")
		}
		return h.showLastNDiff(n)
	}
	stat, diff := h.env.sessionDiff()
	if stat == "" && diff == "" {
		fmt.Fprintln(h.w, "no changes yet this session")
		return true, nil
	}
	if stat != "" {
		fmt.Fprintln(h.w, stat)
	}
	fmt.Fprintln(h.w)
	if diff == "" {
		fmt.Fprintln(h.w, "(no tracked-file diff — new files only; see git status)")
		return true, nil
	}
	fmt.Fprintln(h.w, renderPlainDiff(diff))
	return true, nil
}

// showLastNDiff shows the diff of the last N agent commits.
func (h *skillsHandler) showLastNDiff(n int) (bool, error) {
	ws, err := os.Getwd()
	if err != nil {
		return true, err
	}
	commits, err := gitops.AgentCommits(ws)
	if err != nil {
		return true, err
	}
	if len(commits) == 0 {
		fmt.Fprintln(h.w, "no yagent commits to diff")
		return true, nil
	}
	if n > len(commits) {
		n = len(commits)
	}
	// the last N agent commits end at the newest agent commit's parent-boundary;
	// diff from the commit just before the run to HEAD.
	oldest := commits[n-1]
	diff := gitops.DiffSince(ws, oldest.Hash+"^")
	if diff == "" {
		fmt.Fprintln(h.w, "(no diff — files added/removed only)")
		return true, nil
	}
	fmt.Fprintln(h.w, renderPlainDiff(diff))
	return true, nil
}

// renderPlainDiff colorizes a unified git diff for terminal output (no ANSI on
// a pipe; keeps the +/- markers visible either way).
func renderPlainDiff(diff string) string {
	if diff == "" {
		return ""
	}
	var lines []string
	for _, ln := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---") ||
			strings.HasPrefix(ln, "diff "):
			lines = append(lines, "\x1b[90m"+ln+"\x1b[0m")
		case strings.HasPrefix(ln, "@@"):
			lines = append(lines, "\x1b[36m"+ln+"\x1b[0m")
		case strings.HasPrefix(ln, "+"):
			lines = append(lines, "\x1b[32m"+ln+"\x1b[0m")
		case strings.HasPrefix(ln, "-"):
			lines = append(lines, "\x1b[31m"+ln+"\x1b[0m")
		default:
			lines = append(lines, ln)
		}
	}
	return strings.Join(lines, "\n")
}

// setAPIKey stores an API key in the config's api_key field (`/key <value>`),// the TUI-free counterpart to the /model selector's inline key entry. `clear`
// empties it (revert to env-var-only keys).
func (h *skillsHandler) setAPIKey(rest string) (bool, error) {
	target := h.cfg.Path
	if h.cfg.ProjectPath != "" {
		target = h.cfg.ProjectPath
	}
	value := strings.TrimSpace(strings.TrimPrefix(rest, "key"))
	if value == "" {
		fmt.Fprintln(h.w, "usage: /key <api-key>  (stored in config api_key; 'clear' to remove)")
		return true, nil
	}
	if value == "clear" {
		if err := config.Set(target, "api_key", ""); err != nil {
			return true, err
		}
		h.cfg.APIKey = ""
		fmt.Fprintln(h.w, "api_key cleared — cloud keys now come from env vars only")
		return true, nil
	}
	if err := config.Set(target, "api_key", value); err != nil {
		return true, err
	}
	h.cfg.APIKey = value
	fmt.Fprintln(h.w, "api_key saved (used on the next chat session; never shown in listings)")
	return true, nil
}

// showModels lists the built-in provider catalog and applies a selection. In
// the plain REPL the agent loop holds the client, so switching provider/model
// takes effect on the next chat session (the TUI /model selector applies live).
func (h *skillsHandler) showModels(rest string, ag *agent.Agent) (bool, error) {
	if rest == "model" {
		fmt.Fprintln(h.w, "providers:")
		for i, p := range config.Providers {
			mark := "  "
			if p.BaseURL == h.cfg.ServerURL {
				mark = "▸ "
			}
			fmt.Fprintf(h.w, "%s[%d] %s  (%s)\n", mark, i, p.Name, p.BaseURL)
		}
		fmt.Fprintln(h.w, "usage: /model <provider-number> [model]  — e.g. /model 2 deepseek-chat")
		fmt.Fprintln(h.w, "provider keys come from the env (OPENCODE_ZEN_API_KEY, DEEPSEEK_API_KEY, OPENROUTER_API_KEY, …) or api_key; never stored in config")
		fmt.Fprintln(h.w, "applies on the next chat session (use the TUI /model selector to switch live)")
		return true, nil
	}
	parts := strings.Fields(rest)
	if len(parts) < 2 {
		return true, fmt.Errorf("usage: /model <provider-number> [model]")
	}
	idx, err := strconv.Atoi(parts[1])
	if err != nil || idx < 0 || idx >= len(config.Providers) {
		return true, fmt.Errorf("unknown provider %q (see /model for the list)", parts[1])
	}
	prov := config.Providers[idx]
	model := ""
	if len(parts) > 2 {
		model = parts[2]
	} else if len(prov.Models) > 0 {
		model = prov.Models[0]
	}
	target := h.cfg.Path
	if h.cfg.ProjectPath != "" {
		target = h.cfg.ProjectPath
	}
	key := h.cfg.KeyFor(prov)
	if err := config.SetProvider(target, prov, model, key); err != nil {
		return true, err
	}
	h.cfg.SelectProvider(prov, model)
	fmt.Fprintf(h.w, "switched to %s / %s (%s) — applies next session\n", prov.Name, model, prov.BaseURL)
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

// listSessions lists existing sessions in the REPL.
func (h *skillsHandler) listSessions() (bool, error) {
	if h.env == nil || h.env.st == nil {
		fmt.Fprintln(h.w, "session store is not available")
		return true, nil
	}
	sessions, err := h.env.st.ListSessions(h.ctx)
	if err != nil {
		return true, fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(h.w, "no sessions yet")
		return true, nil
	}
	fmt.Fprintf(h.w, "%-40s  %-8s  %s\n", "id", "msgs", "title")
	for _, s := range sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(h.w, "%-40s  %-8d  %s\n", s.ID, s.Messages, title)
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
	case "api_key":
		c.APIKey = value
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
	case "sampling.min_p":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		c.Sampling.MinP = f
	case "sampling.reasoning_max_tokens":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Sampling.ReasoningMaxTokens = n
	case "ui.show_reasoning":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.UI.ShowReasoning = b
	case "ui.loop_guard":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.UI.LoopGuard = b
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
	case "web_search.max_fetch_kib":
		n, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		c.Web.MaxFetchKib = n
	case "web_search.langsearch_api_key":
		c.Web.LangSearchKey = value
	case "web_search.papers":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		c.Web.Papers = b
	case "web_search.semanticscholar_api_key":
		c.Web.SemanticScholarKey = value
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
	case "vram_threshold_tps":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		c.VramThresholdTPS = f
	case "consult.server_url":
		c.Consult.ServerURL = value
	case "consult.model":
		c.Consult.Model = value
	case "consult.api_key":
		c.Consult.APIKey = value
	case "consult.cmd":
		c.Consult.Cmd = strings.Fields(value)
	case "summarizer.server_url":
		c.Summarizer.ServerURL = value
	case "summarizer.model":
		c.Summarizer.Model = value
	}
	return nil
}

// runGoal drives the current agent through the autonomous goal loop, printing
// a round marker per round; the answers stream through the UI's OnToken. The
// workspace is snapshotted before the run and after each round (C2), so an
// interrupted run can be resumed with `yagent chat --resume-goal <session>`.
func (h *skillsHandler) runGoal(ag *agent.Agent, goal string) (bool, error) {
	fmt.Fprintf(h.w, "goal mode — working toward: %s\n", goal)
	saveGoalCheckpoint := func() {
		if ws, err := os.Getwd(); err == nil {
			if _, err := checkpoint.Save(ws, checkpoint.GoalName); err != nil {
				fmt.Fprintf(h.w, "(warning: could not snapshot workspace for rollback: %v)\n", err)
			}
		}
	}
	if ws, err := os.Getwd(); err == nil {
		if dir, err := checkpoint.Save(ws, checkpoint.GoalName); err != nil {
			fmt.Fprintf(h.w, "(warning: could not snapshot workspace for rollback: %v)\n", err)
		} else {
			fmt.Fprintf(h.w, "workspace snapshotted (%s) — revert later with /checkpoint restore goal, or resume with --resume-goal %s\n", dir, h.env.sessionID)
		}
	}
	_, err := ag.RunGoal(h.ctx, goal, agent.DefaultGoalRounds, func(r int, _ string) {
		fmt.Fprintf(h.w, "\n—— round %d ——\n", r)
		saveGoalCheckpoint()
	})
	if err != nil {
		fmt.Fprintf(h.w, "\ngoal loop: %v\n", err)
		return true, nil
	}
	fmt.Fprintf(h.w, "\ngoal achieved.\n")
	offerDistillation(h.ctx, ag, h.env, h.w)
	return true, nil
}

// runResearch drives the current agent as an autonomous research workflow on
// the topic: the research gate refuses a DONE verdict until a cited report
// exists under .yagent/research/.
func (h *skillsHandler) runResearch(ag *agent.Agent, topic string) (bool, error) {
	fmt.Fprintf(h.w, "research mode — investigating: %s\n", topic)
	_, err := ag.RunResearch(h.ctx, topic, agent.DefaultGoalRounds, func(r int, _ string) {
		fmt.Fprintf(h.w, "\n—— round %d ——\n", r)
	})
	if err != nil {
		fmt.Fprintf(h.w, "\nresearch loop: %v\n", err)
		return true, nil
	}
	if report := ag.ResearchReport(); report != "" {
		fmt.Fprintf(h.w, "\nresearch complete — report written to %s\n", report)
		if prov := ag.ResearchProvenance(); prov != "" {
			fmt.Fprintf(h.w, "provenance: %s\n", prov)
		}
	} else {
		fmt.Fprintf(h.w, "\nresearch complete.\n")
	}
	return true, nil
}

// listPlaybooks prints the available playbooks in the workspace.
func (h *skillsHandler) listPlaybooks() (bool, error) {
	ws, _ := os.Getwd()
	names := playbook.List(ws)
	if len(names) == 0 {
		fmt.Fprintln(h.w, "no playbooks in .yagent/playbooks/ (create one with: /playbook <name>)")
		return true, nil
	}
	fmt.Fprintln(h.w, "playbooks:")
	for _, n := range names {
		pb, err := playbook.Load(ws, n)
		if err != nil {
			fmt.Fprintf(h.w, "  %s  (error: %v)\n", n, err)
			continue
		}
		fmt.Fprintf(h.w, "  %-24s %s (%d phases)\n", n, pb.Description, len(pb.Phases))
	}
	return true, nil
}

// runPlaybook runs a declarative playbook inline through the current agent
// (each phase as an autonomous goal run). Streams through the UI's OnToken.
func (h *skillsHandler) runPlaybook(ag *agent.Agent, name string) (bool, error) {
	ws, err := os.Getwd()
	if err != nil {
		return true, fmt.Errorf("workspace: %w", err)
	}
	pb, err := playbook.Load(ws, name)
	if err != nil {
		return true, err
	}
	fmt.Fprintf(h.w, "playbook %q — %s (%d phases)\n", pb.Name, pb.Description, len(pb.Phases))
	executePlaybook(h.ctx, h.client, h.cfg, h.env, ag, h.w, pb)
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
	// RESUME anchor (deepseek review #6): prepend a compact structured
	// bootstrap to the running summary so a resumed session starts oriented —
	// "where did we leave off" — instead of relying on the raw tail alone.
	if anchor := resumeAnchor(ctx, st, continueID, summary, history); anchor != "" {
		if summary != "" {
			summary = anchor + "\n\n" + summary
		} else {
			summary = anchor
		}
	}
	return continueID, history, summary, "", nil
}

// resumeAnchor builds a compact [RESUMED SESSION] bootstrap for --continue:
// the session title, how many messages survived summarization, and the last
// assistant answer (trimmed). Structured and cheap — the running summary holds
// the condensed arc; this pinpoints where the work stopped.
func resumeAnchor(ctx context.Context, st *memory.Store, sessionID, summary string, history []llm.Message) string {
	var b strings.Builder
	if title, err := st.SessionTitle(ctx, sessionID); err == nil && title != "" {
		fmt.Fprintf(&b, "RESUMED SESSION: %q", title)
	}
	last := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && strings.TrimSpace(history[i].Content) != "" {
			last = history[i].Content
			break
		}
	}
	if summary != "" && b.Len() > 0 {
		b.WriteString(" (older history is summarized below)")
	}
	if last != "" {
		last = strings.Join(strings.Fields(last), " ")
		if len(last) > 220 {
			last = last[:220] + "…"
		}
		fmt.Fprintf(&b, "\nLast answer: %s", last)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
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
	notifyOS("yagent — approval needed", fmt.Sprintf("%s (%s)", call.Function.Name, risk))
	fmt.Fprintf(a.writer, "\n[%s] %s\n  %s\nAllow? [y/N] ",
		risk, call.Function.Name, previewArgs(call.Function.Arguments))
	line, err := a.reader.ReadString('\n')
	if err != nil {
		return agent.Approval{}, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return agent.Approval{OK: answer == "y" || answer == "yes"}, nil
}

# Research: what to borrow from opencode, aider, plandex

Assessment date: 2026-08-15. Sources: opencode.ai/docs (providers, lsp, mcp-servers,
zen, go), aider.chat/docs (git, repomap, edit-formats), plandex README + docs.
Filter applied: *does it make a 9B–14B local model more reliable?* — features that
assume a frontier model are listed as Skip.

Legend: P0 = build now · P1 = build next · P2/P3 = nice-to-have · Skip = not a fit.

---

## A. From opencode

| Idea | Our state | Fit | Effort | Priority |
|---|---|---|---|---|
| MCP support (local + remote servers) | none | High — users add tools (context7-style doc search, git-host) without forking; our `Registry` already has the tool shape MCP tools slot into | Med-High (JSON-RPC client + config + tool adapter) | **P1** |
| Skills hierarchy (global/per-agent, tool scoping) | procedural skills + subagent roles | Med — theirs is more polished; ours covers the primitive | Low | P2 |
| LSP integration | `workspace_diagnostics` (CLI lint/typecheck) | **Low — opencode's own docs say LSP is "not always a net positive"; prefer CLI checkers fed into the loop. We already do that.** | High | Skip |
| Model provider catalog via models.dev | own catalog (v0.1.67-69) | Could sync from models.dev at runtime instead of hardcoding | Low | P3 |
| Cloud auth via `/connect` | `/key` + `/model` inline entry (v0.1.69) | Done in spirit | — | Done |

## B. From aider

| Idea | Our state | Fit | Effort | Priority |
|---|---|---|---|---|
| **Auto-commit + git-based undo per change** | in-memory `undo.Buffer` (crash-unsafe, deferred 4×) | **Very high — closes the oldest robustness gap.** Commit dirty files *before* editing (never lose user work), commit after each edit with attribution, `/undo` = revert. Reuses git instead of a journal. | Med | **P0 — build** |
| Repo-map (page-rank symbol map) | tree-sitter chunking + `code_topology`/`code_outline` + per-turn injection | We already do equivalent-or-better; our budget covers their 1k map | — | Covered |
| `whole` edit format | codegen mode uses exactly this | Aider calls it slow/costly but it is the *reliable* format for small models — our call was correct | — | Keep |
| `/diff` in-chat | `fs_patch` approval diff + `/undo list` | Show changes since last message | Low | P3 |
| Model alias / warnings | `/model` selector | Low value | Low | P3 |

## C. From plandex

| Idea | Our state | Fit | Effort | Priority |
|---|---|---|---|---|
| **Cumulative diff sandbox** (stage changes; review + apply) | writes go straight to disk (undo records bytes); checkpoints only in goal mode | **High — pairs with git auto-commit (B): stage → review diff → apply/rollback. `fs_patch` per-hunk approval is the seed.** | Med-High | **P1** (after B) |
| Command approvals / controlled execution | `shell_exec` approval-gated + bwrap sandbox | Already done, arguably better | — | Covered |
| Plan version control / branches | `plan` tool (approve/reject) + playbooks | Overkill for a 9B | High | Skip |
| Model packs (curated provider+model combos) | provider catalog | Nice: presets per task type | Low | P3 |
| Automated command debugging loop | `workspace_diagnostics` + test/smoke gates + error envelopes | Already done more deterministically | — | Covered |

---

## Build order

1. **P0 — Git auto-commit/undo** (aider): replaces crash-unsafe in-memory undo,
   makes `/undo` and resume trivial, reuses git. Commits dirty user files before
   editing so user work is never lost. **→ DONE in v0.1.71 (`internal/gitops`,
   `git_auto_commit`, `/undo` routes through git).**
2. **P1 — Cumulative diff sandbox** (plandex): stage changes → review diff →
   apply/rollback, layered on the git commits. **→ DONE in v0.1.72 (`/diff`:
   stat + colorized diff vs the session baseline; `/diff <N>`, `/diff discard`;
   scrollable TUI modal).**
3. **P1 — MCP support** (opencode): the highest-leverage extension surface; our
   tool registry already has the shape MCP tools need. **→ DONE in v0.1.73
   (`internal/mcp` stdio+HTTP client, `mcp:` config, `<server>_<tool>` adapter,
   always offered; a failed server never blocks).**

All three research items are implemented. The remaining lower-priority
candidates were assessed in v0.1.74:

| Idea | Verdict |
|------|---------|
| Skills hierarchy polish (P2) | **Skipped** — we already have global/project skills + subagent tool subsets; the marginal polish isn't worth the surface |
| models.dev runtime sync (P3) | **Done in v0.1.74** (`config.FetchModelsDev`, `/model` shows "live from models.dev") |
| In-chat per-message `/diff` (P3) | **Skipped** — covered by `/diff <N>` (v0.1.72) |
| Model packs / curated combos (P3) | **Skipped** — marginal; the catalog + live sync covers it |
| Model aliases / warnings (P3) | **Warnings done in v0.1.74** (`config.ModelWarning` in the confirm step); aliases skipped (low value) |

---

## D. Broader landscape — Claude Code, OpenAI Codex CLI, Cline, Gemini CLI, Goose, Aider, OpenCode

Assessment date: 2026-08-15 (follow-up). Sources: claude.com/docs (subagents,
hooks, permissions), openai.com/codex changelog + blakecrosley guide (sandbox,
steer, hooks, subagents), Cline/Roo docs (modes, checkpoints, YOLO),
gemini-cli docs (plan mode, 4-tier memory, extensions), goose-docs.ai (MCP
recipes), aider.chat (repo-map, edit modes, git), awesome-opencode (plugins,
worktrees, LSP/AST). Filter: same local-first / 9B–14B constraint as above.
Gaps below were verified against the source tree (`internal/...`): Yagent has
**no hook bus** (0 matches for PreToolUse/PostToolUse/RunHooks), **no git
worktree** isolation, **no enforced read-only plan mode** (only a `plan`
approval-gate tool + `consult`), and **no OS notifications / cron**.

### D.0 Capability matrix (Yagent vs the field)

| Capability | Yagent | Claude Code | Codex CLI | Cline | Gemini CLI | Goose | Aider | OpenCode |
|---|---|---|---|---|---|---|---|---|
| Read-only plan/act mode | ⚠️ `plan` gate only | ✅ | ✅ steer | ✅ | ✅ | ❌ | ⚠️ architect | ✅ |
| Pre/Post-tool hooks | ❌ | ✅ | ✅ | ❌ | ⚠️ policy | ❌ | ❌ | ✅ plugins |
| Subagents (isolated, roles) | ✅ | ✅ | ✅ (≤6) | ❌ | ⚠️ plan-mode | ❌ | ❌ | ✅ |
| Git worktree isolation | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ |
| Git auto-commit / undo / checkpoints | ✅ (v0.1.71/72) | ✅ | ✅ | ✅ shadow | ❌ | ❌ | ✅ | ✅ |
| Hybrid memory (vector+FTS) | ✅ strong | ✅ | ✅ | ❌ | ✅ 4-tier | ⚠️ | ❌ | ✅ |
| Skills / procedural memory | ✅ (scanner+verify) | ✅ | ✅ | ❌ | ✅ | ⚠️ | ❌ | ✅ |
| MCP client | ✅ (v0.1.73) | ✅ | ✅ | ✅ | ✅ (+resources) | ✅ 70+ | ❌ | ✅ |
| Sandbox (bwrap, net-off) | ✅ | ✅ | ✅ (default net-off) | ✅ YOLO | ⚠️ | ⚠️ | ❌ | ✅ |
| Approval tiers / allow-remember | ⚠️ risk+`/yolo` | ✅ deny-first | ✅ 3-level+profiles | ✅ categories | ✅ policy | ❌ | ❌ | ✅ |
| Deterministic eval / gates | ✅ very strong | ❌ | ❌ | ❌ | ❌ | ❌ | ⚠️ lint/test | ❌ |
| Notifications (idle/long-run) | ❌ | ⚠️ | ⚠️ | ✅ OS | ❌ | ❌ | ❌ | ✅ plugin |
| Voice input | ❌ | ❌ | ✅ | ❌ | ✅ | ❌ | ✅ Whisper | ❌ |
| Repo-map / code topology | ✅ superset | ✅ | ✅ | ✅ | ✅ | ⚠️ | ✅ PageRank | ✅ AST |
| Headless / CLI / CI | ✅ | ✅ SDK | ✅ exec+SDK | ✅ | ✅ | ✅ API | ✅ | ✅ |
| Browser automation | ❌ | ✅ | ⚠️ | ✅ | ❌ | ✅ | ❌ | ⚠️ |

Legend: ✅ full · ⚠️ partial/adjacent · ❌ absent.

### D.1 What they have that Yagent lacks — applicable verdict

**🟢 P0 — high local-AI relevance, build next**

- **Read-only Plan mode (explore-then-edit).** CC/Cline/Gemini/Codex all split a
  safe read-only exploration phase from an edit phase. Highest-leverage gap for
  a 7B–14B model: a small model burns turns half-editing while it "thinks," and
  without an enforced read-only phase it mutates files it should only inspect.
  Yagent's `plan` tool is only an approval gate for *its own* plan. **Applicable:**
  add a mode flag (or `/plan` toggle) that restricts the loop to read-only tools
  + `consult`, builds a structured plan, then requires approval to flip to edit
  mode. Reuses existing `ReadOnly` scoping + `Approver`. Low model burden.
- **Pre/Post-tool hooks (lifecycle events).** Codex's hooks engine + CC hooks run
  deterministic code at `PreToolUse`/`PostToolUse`/`SessionStart`/`Stop`. Yagent
  has the *seed* (verify-don't-trust auto-diagnostics, `/diff`) but hardcoded,
  not a general mechanism. **Applicable:** generalize into a small hook bus —
  config-declared hooks firing on tool events (e.g. "after fs_write, run
  diagnostics," "before `rm` shell_exec, escalate"). Pushes *policy* into
  deterministic Go — exactly the local-model discipline. Medium effort; fits
  `internal/tools` dispatch.
- **Finer approval tiers + "allow & remember."** Codex/Cline let users
  pre-authorize categories and *remember* a decision. Yagent only has 3 risk
  levels + `/yolo`. **Applicable:** a `permissions` config (deny-first rules,
  per-tool allowlists, "allow and remember for this session") cuts approval
  fatigue on slow single-GPU runs. Medium effort.

**🟡 P1 — worthwhile, some tradeoff**

- **Git worktree isolation for subagents/experiments.** OpenCode + CC isolate
  parallel work in worktrees. **Applicable but lower priority:** single local
  GPU serializes inference, so worktrees mainly buy *cleanliness* (try a refactor
  in a branch, discard if bad). Existing `/diff` + git auto-commit already give
  rollback. Worth it if/when subagent fan-out becomes common, not a bottleneck
  today. Medium effort (native `git worktree`).
- **OS notifications for long runs.** Cline notifies on approval-needed or
  30s-over auto-approved commands; OpenCode has notify plugins. **Applicable:**
  local models are *slow*; a user walks away during a 25-iter goal run. A
  lightweight `notify-send`/`osascript` on idle/approval-needed/goal-done is a
  real local win, trivial (one `internal/ui` hook). Low effort, high UX payoff.
- **MCP *resources* (not just tools).** Gemini v0.42 added MCP resource tools;
  Yagent's MCP is client-only and tool-shaped. **Applicable:** read-only context
  from an MCP server (doc index, schema) fits the retrieval pipeline — feed MCP
  resources into the same index/recall path. Low priority (servers mostly expose
  tools), cheap to add to `internal/mcp`.
- **Steer / mid-run course correction.** Codex "steer mode" redirects an
  in-flight run; Yagent has `Esc` (cancel turn) + `clarify`. **Applicable:** a
  `/steer <instruction>` injecting a user override into the next loop iteration
  (without losing history) is a small, model-friendly addition. Low-medium effort.

**⚪ Not applicable / explicitly a poor fit (reject)**

- **Voice input** (Aider Whisper, Gemini audio) — gimmick for a local terminal
  agent; adds STT dependency + model burden. Skip.
- **Browser automation** (Cline/Roo/Goose) — headless-browser dep + heavy
  surface; `web_fetch` already covers research. Skip unless concrete local case.
- **Plugin marketplace / extension catalog** (Codex/OpenCode/Goose) — network
  registries + trust infra conflict with local-first single-binary stance.
  `skills import` + `mcp:` config already cover the extension surface. Skip.
- **LSP live integration** — Yagent's own docs already decided CLI checkers beat
  LSP for small models; confirmed correct, keep.
- **Frontier-only multi-model voting / speculative parallel inference** — already
  rejected in `ideas/luna.md` #12; single-slot GPU worsens latency, no quality gain.

### D.2 What Yagent already does *better* than most (don't re-propose)

- **Deterministic eval/gates** (`GoalGate`, `TestGate`, `SuccessChecks`,
  `export-dataset`, `bench`, golden evals) — CC/Codex/Cline/Gemini have *none*.
  The verify-don't-trust barrier is ahead of the field for small-model reliability.
- **Hybrid memory** (vector + FTS5 + importance + recency) + `goal-memorize` +
  accurate server-tokenizer counting — competitive with Codex's memories.
- **Skills with dangerous-pattern scanner + `/skills verify`** — more guarded than
  CC/Codex skills for weak models.
- **Git auto-commit/undo + `/diff` cumulative sandbox** (v0.1.71/72) — covers
  Aider/Cline's safety net.
- **bwrap sandbox defaulting net-off** — matches Codex's safest default.
- **Repair + fuzzy-aliasing tool-call JSON** — purpose-built for small-model
  fallibility; the field generally lacks this.

### D.3 Prioritized "applicable to our agent" shortlist

| # | Item | Why it fits local-first | Effort | Priority |
|---|---|---|---|---|
| 1 | **Plan/read-only mode** | Stops 7B models mutating while exploring; reuses `ReadOnly`+`Approver` | Med | **P0 → DONE v0.1.75** |
| 2 | **Hook bus** (Pre/Post-tool) | Pushes policy into deterministic Go; reduces model burden | Med | **P0 → DONE v0.1.75** |
| 3 | **Approval tiers + allow-remember** | Cuts approval fatigue on slow single-GPU runs | Med | **P0 → DONE v0.1.75 (allow-remember)** |
| 4 | **Idle/long-run OS notifications** | Local runs are slow; user walks away | Low | **P1 → DONE v0.1.75** |
| 5 | **`/steer` mid-run redirect** | Cheap course-correction without history loss | Low-Med | **P1 → DONE v0.1.79** (pins `USER STEER` at the top of TASK STATE; the plan-step tracker rides along as `ACTIVE PLAN`) |
| 6 | **Git worktree isolation** | Cleanliness for subagent experiments (not a bottleneck yet) | Med | **P2 — deferred** |
| 7 | **MCP resources** | Read-only context into recall pipeline | Low | **P2 — deferred** |
| — | Voice / browser / marketplace / LSP | — | — | **Skip** |

All three original items (opencode MCP, aider git-undo, plandex diff sandbox) are
shipped (v0.1.71–73). The D-section items above are the next frontier and extend
the existing `Registry`/`Approver`/`Options` seams in `internal/tools/tools.go`
and `internal/agent/agent.go`.

---

## E. Next directions (assessed 2026-08-15, not yet started)

After ~18 versions of deterministic-gate work, the harness has diminishing
returns. The original bottleneck is unchanged: a 9B-14B local model can't
reliably *finish* long multi-file work — the gates make it fail honestly, not
succeed. Candidate next moves, in assessed order:

1. **Re-measure `TestLiveGoalStress` with the current gates** — codegen mode,
   TestGate, success checks, impact hints and plan mode all landed since the
   last measurement (1/5 → 3/3 → copy-not-move). A fresh run settles whether
   C3 (structured subagent returns) is still needed or the single loop now
   clears the bar. 30 minutes, evidence-driven.
2. **Fine-tune a small model on our own trajectories** — `export-dataset`
   (OpenAI/ShareGPT/DPO JSONL) exists for this. A LoRA/QLoRA on
   Qwen3VL-8B-Instruct using verified successful tool-call trajectories is the
   one move that *raises the model ceiling* (the exact weakness every bench
   shows) instead of adding scaffolding.
3. **Real-world soak** — run Yagent on real tasks against real projects for a
   few days, then fix what it hits. Surfaces gaps synthetic evals can't.
4. **Adversarial-QA pass on the new surface** — gitops, MCP, hooks, plan mode,
   allow-remember (the v0.1.35 pass found 9 real bugs on the old surface).
5. **Polish backlog** — git worktree, MCP resources (deferred, low-value).
   (`/steer` shipped in v0.1.79.)

Recommendation recorded 2026-08-15: #1 first (settles C3 with data), then #2
(the genuine ceiling-raiser).

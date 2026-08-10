# Skills — procedural memory

Modeled on the **Hermes Agent** skill system (NousResearch, MIT, [`github.com/nousresearch/hermes-agent`](https://github.com/nousresearch/hermes-agent)) and the open **Agent Skills** standard ([agentskills.io](https://agentskills.io/specification)). Hermes: *"skills are the agent's procedural memory — when it figures out a non-trivial workflow, it saves the approach as a skill for future reuse."* Memory (L1–L3, `memory.md`) stores small durable facts that stay in context; skills store longer procedures that load **only when relevant**.

Ships as milestone **M3.5** (after the M2 tool loop, which it depends on).

## Scope for v1 — adopt vs defer

Adopt (small-model, local-first subset):

- `SKILL.md` format — agentskills.io-compatible subset of the Hermes frontmatter
- Filesystem store: global `<data>/skills/` **plus project-scoped `<workspace>/.yagent/skills/`** (both read roots)
- Progressive disclosure: `skills_list` → `skill_view(name)` → `skill_view(name, path)`
- `skill_manage` tool (create/patch/edit/delete/write_file/remove_file)
- Autonomous creation triggers + slash-command invocation (`/skill-name`)
- Write-approval gate, **default ON** (deliberate difference from Hermes, which defaults off — see below)
- **Anti-hoarding guard** (dedup, per-session cap, authoring rules) — see "Autonomous creation"
- **Dangerous-pattern scanner** on agent skill writes + load warning — see "Safety"
- **Lifecycle metadata** (`source`, `created_at`, `last_used`) powering L0 eviction — see "SKILL.md format"

Defer (note why):

- Skills Hub / taps / marketplaces, `hermes skills install/search` — needs network registries + trust infra; not local-first
- Bundles, `platforms`, fallback/conditional activation, `config` settings, `required_environment_variables` — context and complexity a 7B–14B model doesn't need day one
- Follow-ups with their own spec below: staleness/retirement, verification harness, `yagent skills` CLI, background self-improvement review, `/learn` from large corpora

## Storage

Two read roots; both are plain directories walked for `skills_list`. Category dirs optional but encouraged; a skill without a category goes straight under `skills/<name>/`.

```
<data>/skills/                     # global store (source of truth, like Hermes ~/.hermes/skills/)
├── code-review/
│   └── rust-unsafe-audit/
│       ├── SKILL.md               # required
│       └── references/            # optional supporting docs
└── deploy/
    └── ollama-rocmsetup/          # agent-created
        └── SKILL.md

<workspace>/.yagent/skills/        # project store (ships in git, like AGENTS.md)
└── deploy/
    └── yagent-release-flow/
        └── SKILL.md
```

- Writes go to the **global** store by default; `skill_manage` accepts `scope: global|project` (default `global`) to target the project store
- On name collision the project store shadows the global one in `skills_list`/`skill_view`
- Skill names must match `^[a-z][a-z0-9_-]*$` (same rule as Hermes/agentskills slugs)
- Filesystem is the store — no SQLite needed; the only index is the directory walk for `skills_list`

## SKILL.md format

agentskills.io-compatible frontmatter subset (parsed with the already-approved `gopkg.in/yaml.v3`; no new dependency). Model-facing fields:

```markdown
---
name: rust-unsafe-audit
description: Audit Rust unsafe blocks for soundness (≤60 chars, one line)
version: 1.0.0
tags: [rust, security]
category: code-review
---
# Rust unsafe audit
## When to Use
Trigger conditions.
## Procedure
1. Step
## Pitfalls
- Known failure modes and fixes
## Verification
How to confirm it worked.
```

Store-managed fields (set by the store, never by the model):

```
source: agent        # agent | user | bundled
created_at: 1730000000    # unix; set on first write
last_used: 1730000000     # unix; updated on every skill_view
```

Validation on write (feeds errors back to the model for retry, per small-model discipline):

- `name` matches the slug regex; unique in the store
- `description` required, ≤ 60 chars, single line, and should state the trigger condition ("when X…", per agentskills.io description best practices)
- `version` optional, semver
- body must contain `## When to Use` and `## Procedure` sections
- `SKILL.md` content cap 8 KiB; reference files cap 16 KiB each

## Progressive disclosure

```
L0 skills_list()          → [{name, description, category, source}, ...]   always in system prompt, budgeted
L1 skill_view(name)       → full SKILL.md                                  on activation
L2 skill_view(name, path) → specific references/ file                      on demand
```

Only the L0 list lives in the system prompt, hard-capped at ~3k tokens / 40 skills; beyond that, **evict by `last_used` desc, then `created_at` desc** (this is why the store records lifecycle metadata). Full content loads as a tool result on activation and counts against the L1 context budget from `agent-loop.md`.

## Tools

Three tools, two read-only and one gated:

| Tool | Mode | Purpose |
|------|------|---------|
| `skills_list` | read | name+description+category+source of all skills (also serves L0) |
| `skill_view` | read | full SKILL.md or a reference file by `(name, path)`; bumps `last_used` |
| `skill_manage` | **write, gated** | `create` / `patch` (preferred, token-efficient) / `edit` / `delete` / `write_file` / `remove_file` |

`schema` for `skill_manage` (typed struct, compact):

```
action: create|patch|edit|delete|write_file|remove_file   (required)
name: string                                              (required, slug)
content: string                                           (create/edit: full SKILL.md)
old_string, new_string: string                            (patch: exact, single-occurrence)
file_path, file_content: string                           (write_file/remove_file: under skill dir)
category: string                                          (create: optional)
scope: global|project                                     (default global)
```

Path hardening: `file_path` is resolved relative to the skill dir and must stay inside it (reject `..`, absolute paths) — same rule as `fs_*` tools. `patch` requires the old_string to match exactly once; ambiguity is an error fed back to the model.

## Autonomous creation — when the agent writes skills

Hermes's triggers, adopted verbatim: the agent creates a skill **after** a turn when any of:

1. It completed a complex task (5+ tool calls) successfully
2. It hit errors/dead ends and found the working path
3. The user corrected its approach
4. It discovered a non-trivial workflow

Mechanically: at end of turn (and at session end), the loop appends a one-shot *skill-creation opportunity* prompt — *"Did this turn meet a trigger above? If so propose a skill with `skill_manage create`; otherwise reply with nothing."* The prompt offers **only the skills tools** (`skills_list`, `skill_view`, `skill_manage`), because the authoring rules require the model to consult `skills_list` before proposing. The model's proposal goes through the normal `skill_manage` path and the approval gate. Trigger count: the agent offers the opportunity when a turn used 5+ tool calls (the `totalToolCalls` counter also drives the session-end pass). The exchange is persisted only when the model actually proposes a write, so quiet turns leave no trace in the session store.

**Authoring rules** are embedded in that prompt (agentskills.io best practices; small models need explicit standards): description ≤60 chars and states the trigger; standard section order (When to Use → Procedure → Pitfalls → Verification); concrete steps with real paths/commands from this session; never invent tools or commands the skill can't actually run.

**Anti-hoarding guard** (Hermes has the triggers but no cap; small models over-create):

- **Dedup on create**: the creation prompt requires the model to consult `skills_list` first; if an existing skill covers the procedure, it must propose `patch`/merge instead of `create`. A `create` that duplicates an existing name or near-identical description (same category + high token overlap) is rejected with a validation error suggesting `patch`.
- **Per-session cap**: at most 2 staged skill writes per session; after the cap the end-of-turn prompt stops offering the opportunity.

Self-improvement during use = the `patch` action: when the agent notices a procedure in an existing skill is wrong/incomplete, it patches it in place (same gate, dedup checks skipped).

## Safety — dangerous-pattern scanner

A skill is text loaded into context; a bad one (hallucinated, or written under a hijacked turn) can steer the agent. Hermes ships this as `skills.guard_agent_created`; we adopt a lighter version at **write time**, applied to every agent `skill_manage` write before staging:

- **block** verdicts (cannot be staged; error fed back): destructive commands (`rm -rf`, `dd`, `mkfs`, `chmod -R 777` on roots), obvious exfiltration (`curl|nc|scp|rsync` to a remote + `base64`/`.git/config` payloads)
- **flag** verdicts (staged normally, but `skill_view` prepends a one-line warning): prompt-injection markers ("ignore previous instructions", "disregard the system prompt", "say 'I confirm'"), shell pipes with `eval`, overly broad `find -exec`

Heuristics live in one table in `internal/skills` (regex + token checks), testable in isolation. Not a security boundary — it's a guard against accidents and prompt-injection attempts, same trust model as Hermes's scanner. User-authored and bundled skills are exempt (the user wrote them).

## Approval gate — default ON

Hermes defaults `skills.write_approval: false` (write freely). **Yagent defaults to `true`** because our models are smaller and misjudge what they learned more often, and AGENTS.md constraint #2 (writes require approval) is stricter by design.

- `write_approval: false` → writes apply immediately
- `write_approval: true` (default) → every `skill_manage` write is **staged** under `<data>/pending/skills/<id>/` (survives restarts; full unified diff) instead of applied

`skill_manage` is **self-gated** (never a generic y/n prompt): the gate *is* the staging step. Gate on → the call returns "staged as `<id>`" and the user reviews via the slash commands below; gate off → applies immediately. The cap counter and staging live in the store, so a staged write is not visible to the agent until approved.

REPL review surface (extends the M1 slash-command set):

```
/skills list                # name + description of all skills
/skills pending             # staged writes + one-line gist each
/skills diff <id>           # full unified diff
/skills approve <id|all>    # apply
/skills reject <id|all>     # drop
/skills approval on|off     # toggle gate, persisted to config
/skill-name [args]          # invoke a skill by name
```

Approval uses the same y/n prompt machinery as dangerous `fs`/`shell` writes. A staged write shows the diff, not a one-line summary — a SKILL.md is too large to review inline.

## Context budget integration

- L0 list: always present, hard-capped (~3k tokens); evicts by `last_used` desc, then `created_at` desc
- Activation: `skill_view` result is a tool result; if injecting it would blow the window, it is truncated and the running summary notes "skill X loaded partially" (`memory.md` L1 rules apply)
- Skills never load automatically by keyword — only by explicit model tool call or `/skill-name`
- Because the llama.cpp template in use accepts only ONE system message per request, the L0 index (plus running summary, recall and any `/skill-name` injection) is merged into the single leading system message in `assembleContext` — see [`docs/models.md`](../models.md). Keep it that way; don't reintroduce separate system messages.

## Implementation mapping (M3.5)

- `internal/skills`: store (`Open(dataDir, workspace)` / `OpenProject(dataDir, projectDir)` → `List`, `View(name, path)`, `Apply(op)`, `Stage(op)`, `ApprovePending(id)`, `RejectPending(id)`, `ListPending`, `PendingDiff`, `StagedCount`), lifecycle metadata updates on view/write, frontmatter parse/validate (`yaml.v3`), path hardening, dangerous-pattern scanner table, dedup helper (description overlap). Mutations are unified in one `Op` struct (`create`/`patch`/`edit`/`delete`/`write_file`/`remove_file`, `scope: global|project`); `Stage` validates + writes a review diff to `<data>/pending/skills/<id>/` and bumps the session cap, `Apply` validates + mutates immediately.
- `internal/tools`: `skills_list`, `skill_view` (read, bumps `last_used`), `skill_manage` (write, **self-gated**: gate on → `Stage`, gate off → `Apply`; enforces per-session cap counter). Registered only when a skills store is configured.
- `internal/agent`: L0 index injected each request (merged into the single system message); end-of-turn creation-trigger pass offering only the skills tools (with authoring rules + cap); `InjectSystem` for `/skill-name`; `Finish` for the session-end pass.
- `internal/ui`: `/skills` + `/skill-name` commands; approve/reject flow; `/skills approval on|off` persists to the config file via `config.SetWriteApproval`.
- config: `skills.write_approval` (default true), `skills.data_dir` (default data dir), `skills.project_dir` (default `<workspace>/.yagent/skills`)

Tests (no network — fake LLM server pattern from M2):

- scripted turn with 5+ tool calls → model emits `skill_manage create` → staged → approve → appears in next session's `skills_list`
- `write_approval: false` applies immediately
- `patch` single-occurrence enforcement; `delete`; path-traversal attempt rejected
- frontmatter validation failures return actionable errors (retry path)
- **dedup**: create with a near-identical description → rejected, error suggests `patch`
- **cap**: 3rd staged write in one session → rejected
- **scanner**: `rm -rf` pattern blocked before staging; "ignore previous instructions" flagged and warned on `skill_view`
- **project store**: project skill listed and viewable; `scope: project` write lands under `.yagent/skills/`
- `/skill-name` activation injects SKILL.md; following it changes behavior (fake-server assertion)

## Acceptance (M3.5)

- [x] "Remember how I fixed the Ollama ROCm env issue" → skill created (staged), approved, recalled via `skills_list` in a later session
- [x] scripted 60-tool-call session creates ≤ 3 skills, all gated, none applied without approval; a duplicate proposal is merged into the existing skill instead
- [x] a wrong procedure in an existing skill is patched via `skill_manage patch` after user correction
- [x] a skill containing `rm -rf /` is blocked at write time; one with "ignore previous instructions" loads with a visible warning
- [x] a project-scoped skill committed to `.yagent/skills/` is available in any session on that repo
- [x] 100 skills installed → `skills_list` output stays under the L0 budget cap, keeping the 40 most recently used

## Follow-ups (deferred, same staging/approval machinery)

- **`yagent skills` CLI** — **shipped (M6.5)**: `yagent skills list` and `yagent skills import <SKILL.md> [--scope global|project]`. Imports are user-authored: `source: user`, exempt from the dangerous-pattern scanner (the user wrote them), and edits preserve the original source.
- **Staleness/retirement**: `skill_view` records a failure flag when a skill's procedure errors during use; after N failures (or when superseded) surface "looks stale" at L0 or stage a deprecation
- **Verification harness**: run a new skill's own `## Verification` section once via existing tools before approval, so garbage skills are caught pre-landing
- **Background self-improvement review**: after each session, review suggests staged skill changes (Hermes does this post-turn)
- **`/learn` from large corpora**: knowledge-base skills with a `references/` index (Hermes `/learn`)

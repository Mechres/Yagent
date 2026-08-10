# Skills — procedural memory

Modeled on the **Hermes Agent** skill system (NousResearch, MIT, [`github.com/nousresearch/hermes-agent`](https://github.com/nousresearch/hermes-agent)) and the open **Agent Skills** standard ([agentskills.io](https://agentskills.io/specification)). Hermes: *"skills are the agent's procedural memory — when it figures out a non-trivial workflow, it saves the approach as a skill for future reuse."* Memory (L1–L3, `memory.md`) stores small durable facts that stay in context; skills store longer procedures that load **only when relevant**.

Ships as milestone **M3.5** (after the M2 tool loop, which it depends on).

## Scope for v1 — adopt vs defer

Adopt (small-model, local-first subset):

- `SKILL.md` format — agentskills.io-compatible subset of the Hermes frontmatter
- Filesystem store under the data dir: `<data>/skills/<category>/<name>/SKILL.md` (+ `references/`)
- Progressive disclosure: `skills_list` → `skill_view(name)` → `skill_view(name, path)`
- `skill_manage` tool (create/patch/edit/delete/write_file/remove_file)
- Autonomous creation triggers + slash-command invocation (`/skill-name`)
- Write-approval gate, **default ON** (deliberate difference from Hermes, which defaults off — see below)

Defer (note why):

- Skills Hub / taps / marketplaces, `hermes skills install/search` — needs network registries + trust infra; not local-first
- Bundles, `platforms`, fallback/conditional activation, `config` settings, `required_environment_variables` — context and complexity a 7B–14B model doesn't need day one
- Background self-improvement review after every session — follow-up to M3.5 core, same staging machinery
- `/learn` from large corpora (knowledge-base skills with `references/` index) — follow-up; the minimal `/learn`-style path (turn a just-finished conversation into a skill) is covered by the creation triggers

## Storage

Single source of truth: `<data>/skills/` (same tree as sessions/embeddings; `data dir` per `memory.md`). Category dirs are optional but encouraged; an agent-created skill without a category goes straight under `skills/<name>/`.

```
<data>/skills/
├── code-review/            # category
│   └── rust-unsafe-audit/
│       ├── SKILL.md        # required
│       └── references/     # optional supporting docs
└── deploy/
    └── ollama-rocmsetup/   # agent-created
        └── SKILL.md
```

Skill names must match `^[a-z][a-z0-9_-]*$` (same rule as Hermes/agentskills slugs). Filesystem is the store — no SQLite needed; the only index is the directory walk for `skills_list`.

## SKILL.md format

agentskills.io-compatible frontmatter subset (parsed with the already-approved `gopkg.in/yaml.v3`; no new dependency):

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

Validation on write (feeds errors back to the model for retry, per small-model discipline):

- `name` matches the slug regex; unique in the store
- `description` required, ≤ 60 chars, single line
- `version` optional, semver
- body must contain `## When to Use` and `## Procedure` sections
- `SKILL.md` content cap 8 KiB; reference files cap 16 KiB each

## Progressive disclosure

```
L0 skills_list()          → [{name, description, category}, ...]   always in system prompt, budgeted
L1 skill_view(name)       → full SKILL.md                          on activation
L2 skill_view(name, path) → specific references/ file              on demand
```

Only the L0 list lives in the system prompt (capped at ~3k tokens / 40 skills; beyond that, truncate — the model asks `skills_list` for more). Full content loads as a tool result on activation and counts against the L1 context budget from `agent-loop.md`.

## Tools

Three tools, two read-only and one gated:

| Tool | Mode | Purpose |
|------|------|---------|
| `skills_list` | read | name+description+category of all skills (also serves L0) |
| `skill_view` | read | full SKILL.md or a reference file by `(name, path)` |
| `skill_manage` | **write, gated** | `create` / `patch` (preferred, token-efficient) / `edit` / `delete` / `write_file` / `remove_file` |

`schema` for `skill_manage` (typed struct, compact):

```
action: create|patch|edit|delete|write_file|remove_file   (required)
name: string                                              (required, slug)
content: string                                           (create/edit: full SKILL.md)
old_string, new_string: string                            (patch: exact, single-occurrence)
file_path, file_content: string                           (write_file/remove_file: under skill dir)
category: string                                          (create: optional)
```

Path hardening: `file_path` is resolved relative to the skill dir and must stay inside it (reject `..`, absolute paths) — same rule as `fs_*` tools. `patch` requires the old_string to match exactly once; ambiguity is an error fed back to the model.

## Autonomous creation — when the agent writes skills

Hermes's triggers, adopted verbatim: the agent creates a skill **after** a turn when any of:

1. It completed a complex task (5+ tool calls) successfully
2. It hit errors/dead ends and found the working path
3. The user corrected its approach
4. It discovered a non-trivial workflow

Mechanically: at end of turn (and at session end), the loop appends a one-shot *skill-creation opportunity* prompt (no tools) — *"Did this turn meet a trigger above? If so propose a skill with `skill_manage create`; otherwise reply with nothing."* The model's proposal goes through the normal `skill_manage` path and the approval gate. Self-improvement during use = the `patch` action: when the agent notices a procedure in an existing skill is wrong/incomplete, it patches it in place (same gate).

## Approval gate — default ON

Hermes defaults `skills.write_approval: false` (write freely). **Yagent defaults to `true`** because our models are smaller and misjudge what they learned more often, and AGENTS.md constraint #2 (writes require approval) is stricter by design.

- `write_approval: false` → writes apply immediately
- `write_approval: true` (default) → every `skill_manage` write is **staged** under `<data>/pending/skills/<id>/` (survives restarts; full unified diff) instead of applied
- REPL review surface (extends the M1 slash-command set):

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

- L0 list: always present, hard-capped (~3k tokens); evicts oldest/least-relevant first if the store grows past the cap
- Activation: `skill_view` result is a tool result; if injecting it would blow the window, it is truncated and the running summary notes "skill X loaded partially" (`memory.md` L1 rules apply)
- Skills never load automatically by keyword — only by explicit model tool call or `/skill-name`

## Implementation mapping (M3.5)

- `internal/skills`: store (`Open(dataDir)` → `List`, `View(name, path)`, `Write(skill)`, `Patch`, `Delete`, `RemoveFile`), frontmatter parse/validate (`yaml.v3`), path hardening
- `internal/tools`: `skills_list`, `skill_view` (read), `skill_manage` (write; stages instead of applying when gate on)
- `internal/agent`: end-of-turn creation-trigger prompt; gate wiring; budget for L0 + activations
- `internal/ui`: `/skills` + `/skill-name` commands; approve/reject flow reusing the approval prompt
- config: `skills.write_approval` (default true)

Tests (no network — fake LLM server pattern from M2):

- scripted turn with 5+ tool calls → model emits `skill_manage create` → staged → approve → appears in next session's `skills_list`
- `write_approval: false` applies immediately
- `patch` single-occurrence enforcement; `delete`; path-traversal attempt rejected
- frontmatter validation failures return actionable errors (retry path)
- `/skill-name` activation injects SKILL.md; following it changes behavior (fake-server assertion)

## Acceptance (M3.5)

- [ ] "Remember how I fixed the Ollama ROCm env issue" → skill created (staged), approved, recalled via `skills_list` in a later session
- [ ] scripted 60-tool-call session creates ≤ 3 skills, all gated, none applied without approval
- [ ] a wrong procedure in an existing skill is patched via `skill_manage patch` after user correction
- [ ] 100 skills installed → `skills_list` output stays under the L0 budget cap

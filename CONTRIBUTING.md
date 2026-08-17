# Contributing

Thanks for considering a contribution to Yagent. This is a local-first Go
project with strict rules — please read `AGENTS.md` (contributor guide) and
`docs/PLAN.md` (milestones) first.

## Getting started

- Go 1.22+; **a C toolchain is required** (tree-sitter chunking uses cgo).
- `make build` — build with `git describe` versioning.
- `make test` / `make vet` / `make race` — the gates every change must pass.
- `go test ./...` must be green, `go vet ./...` clean, `gofmt` clean.

## Rules

- **Local-first by default**: LLM and embedding requests use the configured
  server, which defaults to local Ollama/llama.cpp. The main loop and
  `consult` can use an OpenAI-compatible cloud endpoint only after the user
  explicitly configures an `api_key`; do not add provider SDKs.
- **No new dependencies** without explaining why in the commit (see the
  approved list in `AGENTS.md`).
- **No git mutations from the agent's tools**, and no pushes, rebases, or
  resets without an explicit ask. `git_auto_commit: true` is the documented
  opt-in for local, per-turn safety commits in Git repositories.
- Add tests with every change (table-driven where sensible; fake the LLM via
  `httptest` — no network in unit tests).

## What's useful

- Pick an item from `improvement.md` or a milestone from `docs/PLAN.md`.
- Reproduce a bug with an eval/test first, then fix it.
- Keep `AGENTS.md`, `docs/PLAN.md` and the design docs in sync with what you
  build.

## Submitting

Open a PR with a clear description. The CI workflow runs `test`, `vet`, `race`
and `gofmt` — make sure they pass locally first.

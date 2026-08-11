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

- **Local-first**: LLM/embedding requests go only to the configured server.
  The one deliberately opt-in cloud path is `consult` with an explicit
  `api_key`; everything else stays on your machine.
- **No new dependencies** without explaining why in the commit (see the
  approved list in `AGENTS.md`).
- **No git mutations from the agent's tools**, and no commits without an
  explicit ask.
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

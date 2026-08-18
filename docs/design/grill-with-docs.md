# Grill With Docs

`/grill-with-docs <topic>` is a user-invoked clarification workflow for
ambiguous repository changes. It is intentionally one normal agent turn, not a
second orchestration loop.

The workflow injects a compact mode prompt, then uses the existing `clarify`,
`fs_read`, and approval-gated filesystem tools. It asks one question at a time,
with a maximum of eight clarification questions per invocation. The model must
read the repository before asking questions that code can answer.

The only durable artifacts are:

- `CONTEXT.md` at the repository root: project vocabulary only.
- `docs/adr/NNNN-<short-name>.md`: decisions that are hard to reverse,
  surprising without context, and involve a real trade-off.

The workflow must re-read changed artifacts before handing off to `/plan` or
`/goal`. It must not edit source, tests, configuration, or generated files.
Writes use the normal approval and local git-turn-commit behavior.

Use `/handoff` after the interview to inject the settled discussion into a
fresh planning turn. Handoff enables read-only plan mode first; the existing
`plan` approval is required before implementation can begin.

## Local-model context compression

High-volume shell, git, and similar results are saved in full under
`.yagent/scratch/` before a preview is returned. When profitable, the preview
is content-aware:

- JSON whitespace is compacted without changing its data.
- Logs keep headers, diagnostics, and the tail while marking routine lines as
  concealed.
- Diffs keep file headers, hunks, and changed lines while dropping context.

Compression is applied only when it materially reduces size. The tool result
always includes the recovery path, so the model can use `fs_read` for exact
content. Code source is not lossy-compressed by this mechanism.

`tools.CompressionStats()` exposes aggregate offload/compression byte counts for
benchmarks and UI instrumentation. Search results and source reads remain
exact by default; fetched web pages and MCP results use the same recoverable
offload path because their size and semantics are externally controlled.

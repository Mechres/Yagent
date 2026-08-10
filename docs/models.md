# Model notes (quirks discovered in acceptance runs)

Track model-specific behavior here as you find it (PLAN working agreements).
Last updated: M2 acceptance, real hardware.

## Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf (llama.cpp, port 8089)

The current dev model, served by `llama-server` (Vulkan backend) on
`localhost:8089`. OpenAI-compatible endpoint: `http://localhost:8089/v1`.

Observed behavior (M1/M2 acceptance, 2025-06):

| Quirk | Observed | Handling |
|---|---|---|
| Context window | `n_ctx` = 125696 (VRAM-limited; not the advertised 1M) | fine for current milestones |
| `reasoning_content` | Emits a reasoning block before the answer (Claude-style template); server exposes it as `reasoning_content`, which the SSE parser ignores | nothing to do; reasoning never enters history/context |
| Narrates tool calls | Sometimes says "I need to use fs_edit…" and ends the turn **without** emitting a `tool_calls`; the loop then treats the narration as the final answer | fixed by an explicit system-prompt rule: *"To use a tool, emit the tool call now. Never just describe a tool call you intend to make…"* |
| Path guessing | Called `fs_read {"path":"README"}` (dropped `.md`) and gave up on the error | prompts with exact filenames work; model reads tool errors correctly and adapts |
| Tool-call format | Uses the standard OpenAI `tools` API correctly when it does call | — |
| Verbose persona | Self-identifies as "Qwythos…" and is chatty despite "be concise" | keep the concise bias in the system prompt |
| llama.cpp schema strictness | Server returns 400 `"type must be array, but is null"` when a function schema has `"required": null` (Go `nil` slice) | `fnSchema` normalizes `properties`/`required` to `{}`/`[]` |

Tool-calling reliability is moderate; the reference model for tool-heavy work
remains `qwen2.5-coder:14b` (see README). If you switch models, append a row
above with the new model and re-run the M1/M2 acceptance flows.

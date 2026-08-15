# Model notes (quirks discovered in acceptance runs)

Track model-specific behavior here as you find it (PLAN working agreements).
Last updated: 2026-08-12 — measured with `yagent bench --repeat 3` on the
reference hardware (RX 6700 XT 12 GB, llama.cpp `--jinja`). Full numbers and
recommended settings in [`models-benchmark.md`](models-benchmark.md).

## Compatibility matrix (contribute a row!)

If you run Yagent against a model not listed here, add a row — it is the
kind of data that makes a local-first agent trustworthy. Run `yagent bench
--repeat 3` (or the eval harness + a scripted goal) and record what you
observe. `tool-call` is the reliability of the model actually emitting
`tool_calls` instead of narrating them; `sysmsg` is whether it accepts one
leading system message only.

| Model | Server | tool-call | bench (6 tasks ×3) | notes |
|---|---|---|---|---|
| `Qwen3VL-8B-Instruct-Q4_K_M` | llama.cpp :8089 | excellent | **18/18** | **default model**; best measured; fast; no mandatory thinking; the vision encoder is unused by Yagent but the Qwen3 text/tool base is excellent. A `models:` profile with `reasoning_max_tokens` (e.g. 1024) caps the thinking span on long autonomous/research runs — the biggest 12 GB speed lever |
| `Qwopus3.5-9B-coder-Exp-Q4_K_M` | llama.cpp | good | 12/18 (temp 1.0) | agentic coder (Apache-2.0); card's ToolCall claims don't transfer to llama.cpp OpenAI format; grep-find/code-locate weak; needs temp 1.0 |
| `Qwen3-8B-Q4_K_M` (non-VL) | llama.cpp | excellent | 14/18 | same base but **thinks by default on llama.cpp** → ~4× slower than the VL variant; cap `reasoning_max_tokens` if you use it |
| `LFM2.5-8B-A1B-UD-Q4_K_M` | llama.cpp | good | 13/18 (recipe 0.2/80/1.05) | MoE; recipe helps (10→13); code-locate 0/3 both recipes |
| `LFM2.5-2.6B-Q4_K_M` | llama.cpp | good | 14/18 (default) | capable for 1.7 GB; loops on edit-verify/multi-turn; its recipe (0.1/50/1.1) is within noise |
| `gemma-4-12B-it-heretic-Q4_K_M` | llama.cpp | excellent | 18/18 (single run) | strongest quality; slow (heavy reasoning) |
| `fable-qwen2.5-3b-agentic-...-Q4_K_M` | llama.cpp | good | 18/18 (single run) | tiny + fast; good for simple tasks |
| `gpt-oss-20b-Q4_K_M` | llama.cpp | good | 14/18 | MoE, tight VRAM fit; weaker on recall/glob |
| `Qwythos-9B-...Q4_K_M` (Qwen3.5-9B base) | llama.cpp :8089 | moderate | 11/18 | dev model, see quirks below; flaky recall |
| `qwen2.5-coder-7b-instruct-q4_k_m` | llama.cpp | **poor** | 1/6 | answers without emitting tool calls on llama.cpp — use Ollama's native tools or an agent-tuned variant |
| `qwen2.5-coder:14b` | Ollama | good | — | the classic reference on Ollama (native tool handling) |

`sysmsg` note: all llama.cpp models above work with Yagent's single leading
system message; `assembleContext` merges everything into one either way.

## Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf (llama.cpp, port 8089)

The current dev model, served by `llama-server` (Vulkan backend) on
`localhost:8089`. OpenAI-compatible endpoint: `http://localhost:8089/v1`.

Observed behavior (M1–M3.5 acceptance, 2025-06):

| Quirk | Observed | Handling |
|---|---|---|
| **Sampling recipe** | Qwen3.5 reasoning template degenerates into repetition loops at greedy/low temperature; the card recommends `temperature=0.6, top_p=0.95, top_k=20, repetition_penalty=1.05` | Yagent forwards `sampling.temperature`/`top_p` (default 0.6/0.95) and optional `top_k`/`repetition_penalty` on every chat request (see `config.example.yaml`) |
| Context window | `n_ctx` = 125696 (VRAM-limited; not the advertised 1M) | fine for current milestones |
| `reasoning_content` | Emits a reasoning block before the answer (Claude-style template); server exposes it as `reasoning_content` | surfaced dimmed/italic as a "thinking" block in the TUI/REPL (`agent.OnReasoning`); it never enters history or context |
| Narrates tool calls | Sometimes says "I need to use fs_edit…" and ends the turn **without** emitting a `tool_calls`; the loop then treats the narration as the final answer | fixed by an explicit system-prompt rule: *"To use a tool, emit the tool call now. Never just describe a tool call you intend to make…"* |
| Path guessing | Called `fs_read {"path":"README"}` (dropped `.md`) and gave up on the error | prompts with exact filenames work; model reads tool errors correctly and adapts |
| Tool-call format | Uses the standard OpenAI `tools` API correctly when it does call | — |
| Verbose persona | Self-identifies as "Qwythos…" and is chatty despite "be concise" | keep the concise bias in the system prompt |
| llama.cpp schema strictness | Server returns 400 `"type must be array, but is null"` when a function schema has `"required": null` (Go `nil` slice) | `fnSchema` normalizes `properties`/`required` to `{}`/`[]` |
| **Single system message** | The chat template raises 400 `"System message must be at the beginning"` when a request contains **more than one** system message — even two contiguous leading ones | `assembleContext` merges the system prompt, L0 skills index, running summary, recall and injected skill content into **one** leading system message (`internal/agent/agent.go`) |
| **Tool-only message lists** | The template also 400s (`"No user query found in messages."` / `"System message must be at the beginning."`) if a request has **no plain user message** — this happened when the L1 budget summarized the oldest half of history *mid tool-loop* and swallowed the current user turn | `budget` never summarizes the current user turn (or anything after it): it only condenses messages that precede the last `user` message (`internal/agent/budget`) |
| Section-heading strictness | A skill body with `# When to Use` (H1) was rejected; corrected to `## When to Use` after the validation error | validation error feedback works; the model fixes and retries |
| Embeddings endpoint | `/v1/embeddings` needs the server started with `--embeddings` **and** a non-`none` pooling type. Without `--pooling mean` (or `cls`/`last`) llama.cpp replies 400 `"Pooling type 'none' is not OAI compatible"` | dev server now runs `--embeddings --pooling mean`, so L3 recall works using Qwythos as the embedder (chat and embedding share one server). Set `--pooling mean` in the llama-server command |

Tool-calling reliability is **model-dependent**: `Qwen3VL-8B` and
`gemma-4-12B` are the strongest measured on llama.cpp; `qwen2.5-coder-7b-
instruct` does not emit tool calls on llama.cpp at all (fine on Ollama). If you
switch models, append a row above and re-run `yagent bench --repeat 3`.

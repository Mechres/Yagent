# Model benchmark & guidance

What to expect from different local models running Yagent's agent loop, measured
on the reference hardware. Re-run on your own machine with:

```bash
yagent bench                        # per-task pass/fail + timing + t/s for the configured model
yagent bench --repeat 3             # run each task 3× for a stabler score (flaky tasks vary)
yagent bench --json                 # machine-readable (collect across models)
yagent calibrate                    # tune sampling recipes for the configured model
```

Each task reports its **pass rate**, **average wall time**, **content t/s** and
**reasoning tokens**. Reading the t/s column: for a reasoning model the content
t/s looks low because most of the wall time is thinking — the "think" count
shows that overhead. `--repeat N` is worth using before judging a model: the
`multi-turn` and `code-locate` tasks are the flakiest and swing run-to-run.

## How the benchmark works

`yagent bench` runs six canonical agent-loop tasks against the model and checks
the **observable outcome**, not just that it streamed text:

1. **tool-json** — must read a file via a real tool call and reproduce the fact.
2. **multi-turn** — must recall a code word given two turns earlier.
3. **edit-verify** — must run `workspace_diagnostics` and surface a compile error.
4. **fuzzy-path** — must resolve `README` → `README.md` (dropped extension).
5. **code-locate** — must locate a declaration via a code tool.
6. **grep-find** — must identify which file holds a value.

A task only passes if a tool actually produced the result — the model can't
cheat by guessing.

## Reference hardware

- GPU: **AMD RX 6700 XT (12 GB)**
- Server: llama.cpp `llama-server` with `--jinja`, one slot
- Sampling: default recipe (temperature 0.6, top_p 0.95), **no reasoning cap**
- Models: Q4_K_M GGUF

## Results (single run each — small sample, expect run-to-run variance)

| Model | Size | Pass | Wall time | Notes |
|---|---|---|---|---|
| **fable-qwen2.5-3b-agentic** | 3B | **6/6** | 12.5 s | Fast and agentic-tuned; great for simple/weak hardware. Tiny model — long or deep tasks will still strain it. |
| **gemma-4-12B** | 12B | **6/6** | 63.6 s | Strongest quality; slowest (large + heavy reasoning). Best "capable" pick if speed doesn't matter. |
| **Qwen3.6-14B-A3B** | 14B MoE | 4/6 | 43.4 s | Good and reasonably fast (MoE); flaky on fuzzy-path and code-locate. |
| **Qwythos-9B** (dev default) | 9B | 4/6 | 53.4 s | Good; flaky on multi-turn recall (model variance, not a bug). |
| **qwen2.5-coder-7b-instruct** | 7B | **1/6** | 13.2 s | Answers directly instead of emitting tool calls **on this llama.cpp setup**. Use Ollama's native tool handling, or an agent-tuned variant. |

## What to expect in practice

- **Capable models pass 4–6/6** on the canonical tasks. The two recurring weak
  spots across all models are `multi-turn` (recall after a gap) and
  `code-locate` (turning a question into a precise tool call) — both vary
  run-to-run, so treat a single run as a range, not a verdict.
- **Small agent-tuned models (3B) can genuinely work** for routine tasks —
  fable-qwen2.5-3b scored 6/6 faster than every larger model. They fall apart
  on long, multi-file reasoning, not on basic tool use.
- **Non-agent-tuned coding models (qwen2.5-coder-instruct) may not emit
  `tool_calls`** under llama.cpp's OpenAI-compatible endpoint and will just
  answer in prose — the agent nudges them, but the ergonomics are poor.
- **Reasoning models are slow**: Qwythos / Qwen3.6 / gemma-4 all burn seconds
  of thinking per round-trip. Yagent's loop guard stops them if they repeat,
  but for real work set a cap:

```yaml
sampling:
  reasoning_max_tokens: 1024   # ~25s → ~5s per round-trip on a 12 GB card
```

## Recommended settings by model

| Model | Sampling | Reasoning cap |
|---|---|---|
| fable-qwen2.5-3b | default (0.6 / 0.95) | — |
| gemma-4-12B | default | 1024 (or `yagent calibrate`) |
| Qwen3.6-14B-A3B | default | 1024 |
| Qwythos-9B | default (0.6 / 0.95); rep_penalty 1.05 if it loops | 1024 |
| qwen2.5-coder-7b-instruct | — (prefer Ollama, or an agent-tuned variant) | — |

Use `yagent calibrate` on your own model + hardware to tune the recipe — the
sweep compares four sampling profiles over the same six tasks.

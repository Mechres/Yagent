# Agent Loop & Tool Calling

The agent loop is the heart of Yagent. It is deliberately simple: local 7B–14B models cope poorly with cleverness, so the loop is linear, explicit and defensive.

## Core loop

```go
func (a *Agent) Run(ctx context.Context, input string) error {
    a.history = append(a.history, user(input))
    for i := 0; i < a.cfg.MaxIterations; i++ {
        msgs := a.assembleContext()          // see below
        resp, err := a.llm.ChatStream(ctx, msgs, a.tools.Schemas(), a.ui.TokenWriter())
        if err != nil { return err }          // llm pkg already retried transient errors

        a.history = append(a.history, resp.Message)

        if len(resp.ToolCalls) == 0 {
            return nil                        // final answer, done
        }
        for _, call := range resp.ToolCalls {
            result := a.dispatch(ctx, call)   // validate → approve → execute → truncate
            a.history = append(a.history, toolResult(call.ID, result))
        }
        if err := a.budget(ctx); err != nil { // summarize/shrink if over window
            return err
        }
    }
    return ErrMaxIterations                   // report gracefully to the user
}
```

## Termination conditions

| Condition | Behavior |
|---|---|
| Response with no tool calls | Return when no write-verification, goal, test, or success-check gate needs another pass |
| `max_iterations` (default 25) hit | Stop and return a clear incomplete-turn error |
| `ctx.Done()` (Esc/Ctrl-C) | Abort promptly; the session remains usable |
| Validation errors 3× on the same call | Give up on that call, tell the model it's failing |
| Tool failure | Return structured error text **to the model** as a tool result; never crash the loop |
| Repetition, stalled reads, or truncated stream | Send a bounded recovery nudge; cancel only when recovery cannot converge |

## Context assembly (order matters)

```
[system prompt]                    identity, rules, tool usage guidance   ~800 tok
[workspace profile]                project markers + local prerequisites   ~100 tok
[tool schemas]                     sent as API tools field, not in prompt ~varies
[running summary]                  of old history, if any                  ≤ 600 tok
[long-term memory retrieval]       top-k relevant memories                 ≤ 1000 tok
[repo/index retrieval]             top-k code chunks for this query        ≤ 2000 tok
[recent history]                   verbatim messages, newest-first fill    remainder
[current user message]
```

Defaults: `context_window: 16384`, with a response reserve of 2048 tokens (or
one eighth of the configured window in the UI). Token counting uses the
**server tokenizer** (`llama.cpp /tokenize` / Ollama `/api/tokenize`, probed
once at startup) via `llm.Client.CountTokens`, with a `len/4` fallback when the
server has no tokenizer. Every request's tool-schema cost is accounted too
(MCP servers included), so the context gauge and budget reflect the real prompt.

When recent history doesn't fit, old tool results are first concealed behind
one-line markers. If that is not enough, the **oldest half** is summarized by
the main model or an explicitly configured `summarizer:` model. See
`memory.md`.

### Workspace capability profile

Before the first request, the agent inspects supported project manifests and
the locally available toolchain. A directory with no manifest is reported as a
**greenfield** workspace, not as an unsupported project: the model keeps its
core file, shell, Git, planning, and research tools, but project verification
schemas are withheld until they can be useful. The profile explicitly asks the
model to clarify the language/framework or offer a small scaffold. After a
successful filesystem/refactor/shell mutation, it is refreshed; creating
`go.mod`, `package.json`, `Cargo.toml`, or a Python/C/C++ marker therefore
enables the matching diagnostics, tests, and smoke checks on the next request.

For a recognized project with a missing local prerequisite, the profile says
so directly and likewise suppresses those verification schemas. The registry
still resolves an explicitly emitted tool call; schema filtering reduces noise
for small models rather than becoming a hard capability block.

## Tool-call protocol

Use the OpenAI tools API (`tools` + `tool_choice: "auto"`). Both Ollama and llama-server support it.

1. **Schemas stay small.** Name + one-line description + few args. Enums over free strings where possible.
2. **Validate before executing.** Parse args into a typed struct; on failure do NOT execute — append a tool result like `error: argument "path" is required` and let the model retry. Cap at 3 retries per call.
3. **Parallel execution** is allowed only for read-only tools. Anything with side effects runs sequentially, after approval.
4. **Truncation.** Every tool result passes through a per-tool cap (default: 2000 lines / 32 KiB, whichever first) with a `... truncated (N bytes omitted)` marker. This is what keeps the context alive.
5. **Unknown tool name** → return `error: unknown tool "x", available: ...` as the tool result; the model usually recovers.

## Error taxonomy

| Layer | Examples | Handling |
|---|---|---|
| Transport/API | connection refused, 5xx, malformed SSE | `llm`: exponential backoff, 3 retries; then fail the turn with a clear message ("is the server running?") |
| Model behavior | bad tool args, unknown tool, JSON garbage | feed error back as tool result, retry in-loop |
| Tool execution | file not found, command exit≠0, timeout | return stderr/error text as tool result (truncated) |
| Budget | history overflow | summarize, never hard-fail |
| Fatal | config invalid, DB corrupt | return error to ui, exit non-zero from CLI |

## Key interfaces (sketch)

```go
// internal/llm
type Client interface {
    ChatStream(ctx context.Context, msgs []Message, tools []ToolSchema, w TokenWriter) (*Response, error)
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// internal/tools
type Tool interface {
    Schema() llm.ToolSchema            // name, description, JSON-schema args
    Risk() RiskLevel                   // ReadOnly | Write | Destructive
    Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// internal/agent
type Approver interface {              // implemented by ui
    Approve(ctx context.Context, call ToolCall, risk RiskLevel) (bool, error)
}
```

## Runtime and configuration knobs

```yaml
context_window: 16384       # capped at the server's real n_ctx when lower
sampling:
  reasoning_max_tokens: 0  # opt-in cap on thinking
vram_threshold_tps: 5      # force-prune tool output after a slow stream; 0 disables
# summarizer:               # optional second OpenAI-compatible model for condensation
#   server_url: http://laptop:11434
#   model: qwen3:4b
```

`max_iterations`, reserve size, recall budget, and completion gates are agent
runtime settings rather than public YAML keys. The UI wires the production
defaults; goal mode also supplies its own round limit through `--rounds`.

## What the loop must never do

- Call tools concurrently when any of them is Write/Destructive risk
- Retry a failed tool call silently more than 3 times
- Grow history without a budget check each iteration
- Hide tool errors from the model (they're how small models self-correct)
- Expose raw tool schemas inside the prompt text (use the API's tools field)

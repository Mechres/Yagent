package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config contains runtime configuration for the agent.
type Config struct {
	ServerURL string `yaml:"server_url"`
	Model     string `yaml:"model"`
	// APIKey, when set, is sent as `Authorization: Bearer <api_key>` on every
	// LLM/embedding request. This is the deliberately opt-in cloud path: point
	// server_url at any OpenAI-compatible endpoint (OpenRouter, Groq, Together,
	// Gemini) to run the whole loop in the cloud. Local-first remains the
	// default (empty = no auth header).
	APIKey         string `yaml:"api_key"`
	EmbeddingModel string `yaml:"embedding_model"`
	// EmbeddingServerURL is where /v1/embeddings is served; defaults to
	// ServerURL. Set it to a dedicated embedding server (e.g. a second
	// llama-server running bge-m3) for better recall.
	EmbeddingServerURL string `yaml:"embedding_server_url"`
	DataDir            string `yaml:"data_dir"`
	ContextWindow      int    `yaml:"context_window"`
	// Theme is the TUI color palette name (tokyo, catppuccin, nord).
	Theme  string       `yaml:"theme"`
	Skills SkillsConfig `yaml:"skills"`
	Web    WebConfig    `yaml:"web_search"`
	Shell  ShellConfig  `yaml:"shell"`
	// UI holds display preferences.
	UI UIConfig `yaml:"ui"`
	// Sampling is forwarded on every chat request (zero values are omitted, so
	// servers/cloud endpoints that don't understand a field only get what was
	// set). Defaults follow the Qwythos recipe for temperature/top_p; top_k and
	// repetition_penalty are opt-in (some OpenAI-compatible endpoints reject
	// them).
	Sampling SamplingConfig `yaml:"sampling"`
	// Models is an ordered list of per-model sampling profiles: the first whose
	// Match is a substring of the resolved model name wins, overriding the
	// base sampling defaults (P1 — turns docs/models.md data into code).
	Models []ModelProfile `yaml:"models"`
	// Consult points the `consult` tool at a second local model ("advisor")
	// the agent can ask for guidance. Empty = disabled.
	Consult ConsultConfig `yaml:"consult"`
	// Summarizer overrides the history-condensing model (budget + /compact).
	// Empty = the main model summarizes.
	Summarizer SummarizerConfig `yaml:"summarizer"`
	// MCP lists Model Context Protocol servers (external tools) to attach:
	// name -> {command: [...] (stdio) or url + headers (HTTP), enabled}.
	MCP map[string]MCPServer `yaml:"mcp"`
	// Hooks are deterministic lifecycle hooks (Hermes P0): a command run before
	// ("pre") or after ("post") a matching tool executes. Policy as code — e.g.
	// run diagnostics after fs_write, escalate before a destructive shell.
	Hooks []Hook `yaml:"hooks"`
	// VramThresholdTPS flags context pressure when a stream's average
	// generation speed drops below this many tokens/second (0 = off). A slow
	// stream on a 12 GB card usually means the KV cache spilled out of VRAM;
	// the agent then force-prunes old tool output to pull context back.
	VramThresholdTPS float64 `yaml:"vram_threshold_tps"`
	// Codegen switches the agent loop to a greenfield-code strategy tuned for
	// small local models: whole-file fs_write over incremental fs_edit,
	// compile-driven fixes (refuse a final answer while the static check
	// fails), and plan-narration-as-stall (a final answer that lists "next
	// steps" is fed back until the work is done).
	Codegen bool `yaml:"codegen"`
	// AutoCommitGit, when true (default), makes each turn's file changes a
	// real git commit (aider-style) so /undo is a revert and a crash can't
	// lose a session's work. Only active when the workspace is a git repo;
	// pre-existing dirty files are committed before the agent edits.
	AutoCommitGit bool `yaml:"git_auto_commit"`
	// Path is the config file this was loaded from ("" when none existed);
	// used to persist runtime toggles like skills.write_approval.
	Path string `yaml:"-"`
	// ProjectPath is a per-repo config (<workspace>/.yagent/config.yaml) that
	// overlays the global config, when one exists.
	ProjectPath string `yaml:"-"`
}

// Provider is one entry in the built-in provider catalog (`/model` selector):
// a named OpenAI-compatible endpoint plus the models it serves. Selecting a
// provider persists server_url/model/api_key and rebuilds the client — the
// opt-in cloud path, with the local server always the default.
type Provider struct {
	// Name is the display label, e.g. "DeepSeek" or "OpenRouter".
	Name string
	// BaseURL is the OpenAI-compatible endpoint root (no /v1 suffix).
	BaseURL string
	// KeyEnv is the environment variable holding the API key (empty = no key).
	KeyEnv string
	// Models are the selectable model names for this provider.
	Models []string
	// Dynamic, when true, means the model list is fetched live from the
	// server's /v1/models at selector-open time (local llama.cpp/Ollama). The
	// static Models list is the fallback when the server is unreachable.
	Dynamic bool
	// ModelsDev is the models.dev provider key (e.g. "deepseek", "openrouter",
	// "togetherai") when the model list can be refreshed live from models.dev —
	// the same index opencode uses — so cloud models never go stale. Empty =
	// no live sync (the static Models list is used).
	ModelsDev string
}

// Providers is the built-in catalog for the `/model` selector. Local is always
// first and default. Local providers are Dynamic (models fetched live from
// /v1/models); cloud providers list the current recommended models statically.
// Cloud keys are read from the env var when the user selects a cloud provider,
// or from the configured api_key if already set — never stored in the config.
var Providers = []Provider{
	{
		Name:    "Local (llama.cpp :8089)",
		BaseURL: "http://localhost:8089",
		Dynamic: true,
		Models:  []string{"Qwen3VL-8B-Instruct-Q4_K_M.gguf"},
	},
	{
		Name:    "Local (Ollama :11434)",
		BaseURL: "http://localhost:11434",
		Dynamic: true,
		Models:  []string{"qwen3:8b", "qwen2.5-coder:7b"},
	},
	{
		Name:    "OpenCode Zen",
		BaseURL: "https://opencode.ai/zen",
		KeyEnv:  "OPENCODE_ZEN_API_KEY",
		Models: []string{
			"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-flash-free",
			"qwen3.7-max", "qwen3.7-plus",
			"kimi-k2.7-code", "kimi-k3",
			"glm-5.2", "minimax-m3",
		},
	},
	{
		Name:    "OpenCode Go",
		BaseURL: "https://opencode.ai/zen/go",
		KeyEnv:  "OPENCODE_ZEN_API_KEY",
		Models: []string{
			"deepseek-v4-pro", "deepseek-v4-flash",
			"glm-5.3", "glm-5.2", "glm-5.1",
			"kimi-k2.7-code", "kimi-k3", "kimi-k2.6",
			"qwen3.8-max", "qwen3.7-max", "qwen3.7-plus",
			"mimo-v2.5", "mimo-v2.5-pro", "minimax-m3", "hy3",
		},
	},
	{
		Name:      "DeepSeek",
		BaseURL:   "https://api.deepseek.com",
		KeyEnv:    "DEEPSEEK_API_KEY",
		Models:    []string{"deepseek-v4-pro", "deepseek-v4-flash"},
		ModelsDev: "deepseek",
	},
	{
		Name:      "OpenRouter",
		BaseURL:   "https://openrouter.ai/api",
		KeyEnv:    "OPENROUTER_API_KEY",
		ModelsDev: "openrouter",
		Models: []string{
			"deepseek/deepseek-v4-pro",
			"deepseek/deepseek-v4-flash",
			"anthropic/claude-sonnet-4.5",
			"openai/gpt-5",
			"google/gemini-2.5-pro",
			"qwen/qwen3-coder",
			"z-ai/glm-5.2",
			"moonshotai/kimi-k2",
		},
	},
	{
		Name:      "Groq",
		BaseURL:   "https://api.groq.com/openai",
		KeyEnv:    "GROQ_API_KEY",
		ModelsDev: "groq",
		Models:    []string{"openai/gpt-oss-120b", "openai/gpt-oss-20b", "qwen/qwen3.6-27b", "groq/compound"},
	},
	{
		Name:      "Together",
		BaseURL:   "https://api.together.xyz/v1",
		KeyEnv:    "TOGETHER_API_KEY",
		ModelsDev: "togetherai",
		Models:    []string{"deepseek-ai/DeepSeek-V4-Pro", "moonshotai/Kimi-K2.7-Code", "MiniMaxAI/MiniMax-M3", "openai/gpt-oss-120b"},
	},
	{
		Name:      "Mistral",
		BaseURL:   "https://api.mistral.ai/v1",
		KeyEnv:    "MISTRAL_API_KEY",
		ModelsDev: "mistral",
		Models:    []string{"devstral-2512", "mistral-large-2512", "mistral-medium-2604"},
	},
	{
		Name:      "NVIDIA NIM",
		BaseURL:   "https://integrate.api.nvidia.com/v1",
		KeyEnv:    "NVIDIA_API_KEY",
		ModelsDev: "nvidia",
		Models: []string{
			"nvidia/nemotron-3-super-120b-a12b",
			"nvidia/nemotron-3-ultra-550b-a55b",
			"nvidia/nemotron-3-nano-30b-a3b",
			"deepseek-ai/deepseek-v4-flash",
			"qwen/qwen3-coder-480b-a35b-instruct",
			"openai/gpt-oss-120b",
		},
	},
}

// ProviderNames returns the display labels in catalog order.
func ProviderNames() []string {
	out := make([]string, len(Providers))
	for i, p := range Providers {
		out[i] = p.Name
	}
	return out
}

// ProviderByName finds a catalog entry by display name ("" when unknown).
func ProviderByName(name string) (Provider, bool) {
	for _, p := range Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// KeyFor returns the API key to use for a provider: the already-configured
// api_key first, else the provider's environment variable, else "".
func (c *Config) KeyFor(p Provider) string {
	if c.APIKey != "" {
		return c.APIKey
	}
	if p.KeyEnv != "" {
		return os.Getenv(p.KeyEnv)
	}
	return ""
}

// FetchModels queries a provider's /v1/models endpoint and returns the model
// ids it reports — the local auto-detection path for Dynamic providers
// (llama.cpp and Ollama both serve the OpenAI-shaped endpoint). It handles both
// the OpenAI `data[].id` and Ollama `models[].name` shapes. Returns ok=false
// when the server is unreachable or returns no models.
func FetchModels(ctx context.Context, baseURL string) ([]string, bool) {
	client := &http.Client{Timeout: 4 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false
	}
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, m := range list.Data {
		add(m.ID)
	}
	for _, m := range list.Models {
		add(m.Name)
	}
	return out, len(out) > 0
}

// FetchModelsDev fetches the live model list for a cloud provider from
// models.dev (the same index opencode uses), so cloud models never go stale.
// It returns the provider's model IDs filtered to coding-relevant ones and
// capped, or ok=false when the fetch fails. This is the cloud counterpart of
// FetchModels (which reads a local /v1/models endpoint).
//
// currentModel, when non-empty, is guaranteed to appear in the result even if
// it would sort past the cap — the user's active model must stay selectable.
// Models whose id contains the provider's own name (e.g. "nvidia/…" on NVIDIA)
// are prioritized so a provider's native models don't get alphabetically
// squeezed out by third-party listings.
func FetchModelsDev(ctx context.Context, providerKey, currentModel string) ([]string, bool) {
	if providerKey == "" {
		return nil, false
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var index map[string]struct {
		Models map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, false
	}
	prov, ok := index[providerKey]
	if !ok {
		return nil, false
	}
	// Order: the ACTIVE/configured model first (it must always be selectable
	// regardless of the cap), then the provider's own models (id starts with
	// "<provider>/" or contains the provider name), then coding-relevant, then
	// the rest. Keeps the cap from hiding the user's current model or a
	// provider's native models (NVIDIA's nemotron-* sort behind mistralai/*
	// alphabetically, and behind dozens of other nvidia/* entries).
	own := providerKey
	var ownNames, current, coding, other []string
	for id := range prov.Models {
		low := strings.ToLower(id)
		isOwn := own != "" && strings.HasPrefix(low, own+"/")
		if currentModel != "" && id == currentModel {
			current = append(current, id)
			continue
		}
		if isOwn {
			ownNames = append(ownNames, id)
			continue
		}
		if strings.Contains(low, "coder") || strings.Contains(low, "code") ||
			strings.Contains(low, "instruct") || strings.Contains(low, "deepseek") ||
			strings.Contains(low, "qwen3") || strings.Contains(low, "devstral") ||
			strings.Contains(low, "gpt-oss") || strings.Contains(low, "nemotron") {
			coding = append(coding, id)
		} else {
			other = append(other, id)
		}
	}
	sort.Strings(ownNames)
	sort.Strings(coding)
	sort.Strings(other)
	out := append(current, ownNames...)
	out = append(out, coding...)
	out = append(out, other...)
	const max = 20
	if len(out) > max {
		out = out[:max]
	}
	return out, len(out) > 0
}

// ModelWarning returns a caution for a model our own bench data shows is weak
// at tool calling, or "" when it's fine/unknown. Surfaced in the /model
// selector's confirm step so a user doesn't unknowingly pick a model that
// can't drive tools.
func ModelWarning(model string) string {
	m := strings.ToLower(model)
	for _, weak := range []string{
		"mini", "nano", "1b", "2b", "3b", "qwen2.5-coder-7b", "gpt-3.5",
	} {
		if strings.Contains(m, weak) {
			return "caution: this model may be weak at tool calling on this stack (a 7B-9B model needs a good function-calling recipe) — run `yagent bench` to confirm it can drive tools"
		}
	}
	return ""
}

// modelsDevURL is the models.dev index endpoint (overridable in tests).
var modelsDevURL = "https://models.dev/api.json"

// ContextLength returns a cloud model's real context window (in tokens) from
// the models.dev index, or 0 when unknown/unreachable. Cloud APIs are not
// GPU-bound, so a model like DeepSeek V4 Flash legitimately runs a 1M-token
// window — the local `context_window` (a GPU-VRAM-bound value) should not cap
// it. The models.dev entry carries the provider's documented `limit.context`.
func ContextLength(ctx context.Context, providerKey, model string) int {
	if providerKey == "" || model == "" {
		return 0
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var index map[string]struct {
		Models map[string]struct {
			Limit *struct {
				Context int `json:"context"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return 0
	}
	prov, ok := index[providerKey]
	if !ok {
		return 0
	}
	m, ok := prov.Models[model]
	if !ok {
		return 0
	}
	if m.Limit == nil {
		return 0
	}
	return m.Limit.Context
}

// maxCloudContextWindow bounds the auto-raised window for a cloud model, so the
// agent's token math (budget, reserve, gauge) never runs into integer/large
// overflow or absurd reserve sizes. 1M tokens is beyond any current real need
// and far past any practical summarization budget.
const maxCloudContextWindow = 1 << 20

// SkillsConfig configures procedural memory (M3.5).
type SkillsConfig struct {
	// WriteApproval gates skill writes: when true every skill_manage write is
	// staged for review; when false (default) skill writes apply immediately.
	// The safety scanner still blocks dangerous content either way.
	WriteApproval bool `yaml:"write_approval"`
	// DataDir overrides where the global skills store lives (default: data_dir).
	DataDir string `yaml:"data_dir"`
	// ProjectDir overrides the project store (default: <workspace>/.yagent/skills).
	ProjectDir string `yaml:"project_dir"`
}

// WebConfig configures the M5 web tools.
type WebConfig struct {
	// Provider is the web_search backend: "duckduckgo" (default, no key or
	// server needed) or "searxng" (self-hosted JSON, requires SearxngURL).
	Provider string `yaml:"provider"`
	// SearxngURL is the base URL of a SearXNG instance with format=json enabled.
	SearxngURL string `yaml:"searxng_url"`
	// MaxFetchKib caps web_fetch's extracted-text output in KiB (0 = default
	// 32 KiB). Raise for research-heavy sessions where pages must be read whole.
	MaxFetchKib int `yaml:"max_fetch_kib"`
	// LangSearchKey enables the hosted LangSearch web-search provider (free
	// API, requires a dashboard key at langsearch.com). When set it joins the
	// provider fallback chain.
	LangSearchKey string `yaml:"langsearch_api_key"`
	// Papers enables the paper_search tool (arXiv + PubMed; Semantic Scholar
	// when SemanticScholarKey is set).
	Papers bool `yaml:"papers"`
	// SemanticScholarKey enables the Semantic Scholar paper index (keyless use
	// is rate-limited).
	SemanticScholarKey string `yaml:"semanticscholar_api_key"`
	// AllowLocalFetch opts web_fetch's SSRF guard OUT of the internal-host
	// (loopback/private/link-local) check. OFF by default. Set it only when the
	// agent must read a local dev server (e.g. one started via shell_bg on
	// 127.0.0.1). Enabling it lets a model fetch internal hosts, so leave it
	// disabled unless you trust the workspace.
	AllowLocalFetch bool `yaml:"allow_local_fetch"`
}

// ShellConfig configures shell_exec.
type ShellConfig struct {
	// Sandbox wraps shell commands in bubblewrap when set to "bwrap"
	// (workspace writable, system read-only, no network, private /tmp). It
	// fails loudly when bubblewrap is not installed — it never silently runs
	// unsandboxed. Empty disables the sandbox.
	Sandbox string `yaml:"sandbox"`
}

// ConsultConfig configures the `consult` tool (an "advisor" model). Two
// backends: a remote OpenAI-compatible server (ServerURL/Model/APIKey) or an
// installed terminal AI app run as a subprocess (Cmd, e.g. ["claude", "-p"],
// with the prompt appended as the final argument).
type ConsultConfig struct {
	// ServerURL of the advisor model (defaults to server_url when Model is set).
	ServerURL string `yaml:"server_url"`
	// Model is the advisor model name (HTTP backend).
	Model string `yaml:"model"`
	// APIKey enables cloud OpenAI-compatible endpoints (Authorization: Bearer).
	APIKey string `yaml:"api_key"`
	// Cmd is a terminal AI app used as the advisor, e.g. ["claude", "-p"].
	Cmd []string `yaml:"cmd"`
}

// SummarizerConfig overrides which model condenses old history (the budget
// summarizer and /compact). Defaults to the main model when unset — use this to
// offload summarization to a second machine/model. The main loop never uses it
// for tool calls, so a small/slow summarizer only costs on budget/compact.
type SummarizerConfig struct {
	// ServerURL of the summarizer (defaults to server_url when Model is set).
	ServerURL string `yaml:"server_url"`
	// Model is the summarizer model name.
	Model string `yaml:"model"`
}

// MCPServer is one Model Context Protocol server to attach. Exactly one of
// Command (stdio transport: spawn a subprocess) or URL (HTTP transport) is
// used. Headers are sent on HTTP requests (auth tokens etc.).
type MCPServer struct {
	// Command spawns a local MCP server, e.g. ["npx","-y","@modelcontextprotocol/server-everything"].
	Command []string `yaml:"command"`
	// URL of a remote MCP server, e.g. "https://mcp.context7.com/mcp".
	URL string `yaml:"url"`
	// Headers to send on HTTP requests.
	Headers map[string]string `yaml:"headers"`
	// Enabled gates the server; false servers are skipped at startup.
	Enabled bool `yaml:"enabled"`
}

// Hook is one deterministic lifecycle hook: a command run before ("pre") or
// after ("post") a matching tool executes. Tool "*" matches every tool. The
// hook receives the tool name via YAGENT_TOOL and the raw JSON args via
// YAGENT_ARGS. A pre-hook with a non-zero exit can veto the call.
type Hook struct {
	// When is "pre" or "post".
	When string `yaml:"when"`
	// Tool is the tool name to match, or "*" for all.
	Tool string `yaml:"tool"`
	// Command is the argv to run (e.g. ["notify-send", "yagent"]).
	Command []string `yaml:"command"`
}

// SamplingConfig is the subset of generation parameters forwarded on chat
// requests. Zero values are omitted (top_k/repetition_penalty/min_p default
// off so OpenAI-compatible cloud endpoints that reject them aren't broken by
// default).
type SamplingConfig struct {
	Temperature       float64 `yaml:"temperature"`
	TopP              float64 `yaml:"top_p"`
	TopK              int     `yaml:"top_k"`
	RepetitionPenalty float64 `yaml:"repetition_penalty"`
	// MinP is the nucleus lower-bound filter (0 = off; llama.cpp/Ollama only —
	// often tightens up small local models).
	MinP float64 `yaml:"min_p"`
	// ReasoningMaxTokens caps the model's thinking span per request (0 = off).
	// On a 12 GB card this is the single biggest speed lever for reasoning
	// models — each round-trip stops thinking sooner and answers.
	ReasoningMaxTokens int `yaml:"reasoning_max_tokens"`
}

// ModelProfile overrides the base sampling for a model whose name contains
// Match (substring). Pointer fields mean "only set when present" — unset
// fields inherit the base SamplingConfig.
type ModelProfile struct {
	Match             string   `yaml:"match"`
	Temperature       *float64 `yaml:"temperature"`
	TopP              *float64 `yaml:"top_p"`
	TopK              *int     `yaml:"top_k"`
	RepetitionPenalty *float64 `yaml:"repetition_penalty"`
	MinP              *float64 `yaml:"min_p"`
	ReasoningMax      *int     `yaml:"reasoning_max_tokens"`
}

// applyModels applies the first matching per-model profile to the base sampling
// (P1). Match is a substring of the model name; the first hit wins.
func (c *Config) applyModels() {
	for _, p := range c.Models {
		if p.Match == "" || !strings.Contains(c.Model, p.Match) {
			continue
		}
		if p.Temperature != nil {
			c.Sampling.Temperature = *p.Temperature
		}
		if p.TopP != nil {
			c.Sampling.TopP = *p.TopP
		}
		if p.TopK != nil {
			c.Sampling.TopK = *p.TopK
		}
		if p.RepetitionPenalty != nil {
			c.Sampling.RepetitionPenalty = *p.RepetitionPenalty
		}
		if p.MinP != nil {
			c.Sampling.MinP = *p.MinP
		}
		if p.ReasoningMax != nil {
			c.Sampling.ReasoningMaxTokens = *p.ReasoningMax
		}
		return
	}
}

// UIConfig holds display preferences.
type UIConfig struct {
	// ShowReasoning toggles the dimmed "thinking" block in the TUI/REPL
	// (reasoning_content). Reasoning never enters history either way.
	ShowReasoning bool `yaml:"show_reasoning"`
	// LoopGuard auto-cancels a running turn when the model visibly repeats
	// itself (a stuck generation loop). Default on.
	LoopGuard bool `yaml:"loop_guard"`
	// Accessibility controls terminal presentation: standard, high-contrast,
	// or ascii (emoji-free labels for limited terminal fonts).
	Accessibility string `yaml:"accessibility"`
	// ReducedMotion stops the spinner animation (a static indicator instead),
	// for users with vestibular sensitivity or in log-scroll contexts.
	ReducedMotion bool `yaml:"reduced_motion"`
}

// Defaults applied when no config file and no env override is present.
const (
	DefaultServerURL      = "http://localhost:11434"
	DefaultModel          = "Qwen3VL-8B-Instruct-Q4_K_M.gguf"
	DefaultEmbeddingModel = "nomic-embed-text"
	DefaultContextWindow  = 16384
	DefaultTheme          = "tokyo"
	// DefaultVramThresholdTPS flags context pressure when streaming drops below
	// this t/s (0 = off). A 12 GB card normally streams 30–50 t/s; a collapse
	// to 1–2 t/s means the KV cache spilled into system RAM.
	DefaultVramThresholdTPS = 5.0
	// maxVramThresholdTPS is the sane finite upper bound for the threshold.
	// A value above this would mark every normal stream as "pressure" and
	// force needless context pruning; NaN would disable detection, +Inf would
	// trip it on every qualifying stream (codex audit IT10, 2026-08-16).
	maxVramThresholdTPS = 1000.0
	// DefaultTemperature / DefaultTopP follow the Qwythos-9B recipe (0.6 /
	// 0.95). TopK (20) and RepetitionPenalty (1.05) are documented but not
	// defaulted, since some OpenAI-compatible endpoints reject them.
	DefaultTemperature = 0.6
	DefaultTopP        = 0.95
)

// validateVramThreshold rejects NaN / Inf / negative / absurdly large values.
// A finite, non-negative threshold is required or detection misbehaves.
func validateVramThreshold(n float64) error {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("vram_threshold_tps must be a finite number (NaN/Inf not allowed)")
	}
	if n < 0 {
		return fmt.Errorf("vram_threshold_tps must be >= 0")
	}
	if n > maxVramThresholdTPS {
		return fmt.Errorf("vram_threshold_tps must be <= %g", maxVramThresholdTPS)
	}
	return nil
}

// ThemeOptions are the selectable TUI palette names.
var ThemeOptions = []string{"tokyo", "catppuccin", "nord"}

// EnvVarServerURL / EnvVarModel / EnvVarEmbeddingModel / EnvVarDataDir are the
// environment variable overrides, applied on top of whatever the config file
// (or defaults) resolved to.
const (
	EnvVarServerURL       = "YAGENT_SERVER_URL"
	EnvVarModel           = "YAGENT_MODEL"
	EnvVarAPIKey          = "YAGENT_API_KEY"
	EnvVarEmbeddingModel  = "YAGENT_EMBEDDING_MODEL"
	EnvVarEmbeddingServer = "YAGENT_EMBEDDING_SERVER_URL"
	EnvVarDataDir         = "YAGENT_DATA_DIR"
	EnvVarContextWindow   = "YAGENT_CONTEXT_WINDOW"
	EnvVarTheme           = "YAGENT_THEME"
	EnvVarWebProvider     = "YAGENT_WEB_SEARCH_PROVIDER"
	EnvVarSearxngURL      = "YAGENT_SEARXNG_URL"
	EnvVarConsultServer   = "YAGENT_CONSULT_SERVER_URL"
	EnvVarConsultModel    = "YAGENT_CONSULT_MODEL"
	EnvVarConsultAPIKey   = "YAGENT_CONSULT_API_KEY"
	EnvVarShellSandbox    = "YAGENT_SHELL_SANDBOX"
	EnvVarVramThreshold   = "YAGENT_VRAM_THRESHOLD_TPS"
)

// DefaultPath is the config file used when no explicit path is given.
// Config file names and precedence: explicit path (must exist) >
// env overrides > default path (if it exists) > built-in defaults.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(dir, "yagent", "config.yaml"), nil
}

// DefaultDataDir is where sessions, vector memory and skills live:
// $XDG_DATA_HOME/yagent, falling back to ~/.local/share/yagent.
func DefaultDataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "yagent"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "yagent"), nil
}

// LoadConfig loads configuration from path, or the default path when path
// is empty. An explicit path that does not exist is an error; a missing
// default path silently falls back to built-in defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		ServerURL:      DefaultServerURL,
		Model:          DefaultModel,
		EmbeddingModel: DefaultEmbeddingModel,
		ContextWindow:  DefaultContextWindow,
		Theme:          DefaultTheme,
		UI:             UIConfig{ShowReasoning: true, LoopGuard: true, Accessibility: "standard"}, Sampling: SamplingConfig{Temperature: DefaultTemperature, TopP: DefaultTopP},
		Skills:           SkillsConfig{WriteApproval: false},
		VramThresholdTPS: DefaultVramThresholdTPS,
		AutoCommitGit:    true,
	}
	dataDir, err := DefaultDataDir()
	if err != nil {
		return nil, err
	}
	cfg.DataDir = dataDir

	usePath := path
	if usePath == "" {
		def, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		usePath = def
	}
	cfg.Path = usePath

	data, err := os.ReadFile(usePath)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", usePath, err)
		}
	case os.IsNotExist(err):
		if path != "" {
			return nil, fmt.Errorf("config file %s: %w", path, err)
		}
		// no default config file: keep defaults
	default:
		return nil, fmt.Errorf("read config %s: %w", usePath, err)
	}

	// Per-repo config: <workspace>/.yagent/config.yaml overlays the global one
	// (only keys present in it are overridden), so a team can pin the model,
	// server, sandbox, etc. for a repo and commit it.
	if proj := ProjectConfigPath(); proj != "" {
		if data, err := os.ReadFile(proj); err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse project config %s: %w", proj, err)
			}
			cfg.ProjectPath = proj
		}
	}

	// Env overrides beat the file (and defaults).
	if v := os.Getenv(EnvVarServerURL); v != "" {
		cfg.ServerURL = v
	}
	if v := os.Getenv(EnvVarModel); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv(EnvVarAPIKey); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv(EnvVarEmbeddingModel); v != "" {
		cfg.EmbeddingModel = v
	}
	if v := os.Getenv(EnvVarEmbeddingServer); v != "" {
		cfg.EmbeddingServerURL = v
	}
	if v := os.Getenv(EnvVarDataDir); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv(EnvVarTheme); v != "" {
		cfg.Theme = v
	}
	if v := os.Getenv(EnvVarContextWindow); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 {
			return nil, fmt.Errorf("%s must be an integer >= 100, got %q", EnvVarContextWindow, v)
		}
		cfg.ContextWindow = n
	}
	if v := os.Getenv(EnvVarWebProvider); v != "" {
		cfg.Web.Provider = v
	}
	if v := os.Getenv(EnvVarSearxngURL); v != "" {
		cfg.Web.SearxngURL = v
	}
	if v := os.Getenv(EnvVarConsultServer); v != "" {
		cfg.Consult.ServerURL = v
	}
	if v := os.Getenv(EnvVarConsultModel); v != "" {
		cfg.Consult.Model = v
	}
	if v := os.Getenv(EnvVarConsultAPIKey); v != "" {
		cfg.Consult.APIKey = v
	}
	if v := os.Getenv(EnvVarShellSandbox); v != "" {
		cfg.Shell.Sandbox = v
	}
	if v := os.Getenv(EnvVarVramThreshold); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a number >= 0, got %q", EnvVarVramThreshold, v)
		}
		if err := validateVramThreshold(n); err != nil {
			return nil, fmt.Errorf("%s: %w", EnvVarVramThreshold, err)
		}
		cfg.VramThresholdTPS = n
	}

	if cfg.ServerURL == "" {
		cfg.ServerURL = DefaultServerURL
	}
	if cfg.EmbeddingServerURL == "" {
		cfg.EmbeddingServerURL = cfg.ServerURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.EmbeddingModel == "" {
		cfg.EmbeddingModel = DefaultEmbeddingModel
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = DefaultContextWindow
	}
	if cfg.Web.Provider == "" {
		cfg.Web.Provider = "duckduckgo"
	}
	if cfg.DataDir == "" {
		dataDir, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		cfg.DataDir = dataDir
	}
	// Per-model sampling profiles override the base recipe for the resolved
	// model (P1) — last, so they apply on top of file + env sampling.
	cfg.applyModels()
	if err := validateVramThreshold(cfg.VramThresholdTPS); err != nil {
		return nil, fmt.Errorf("config vram_threshold_tps invalid: %w", err)
	}
	return cfg, nil
}

// ProjectConfigPath returns <workspace>/.yagent/config.yaml when it exists.
func ProjectConfigPath() string {
	ws, err := os.Getwd()
	if err != nil {
		return ""
	}
	proj := filepath.Join(ws, ".yagent", "config.yaml")
	if _, err := os.Stat(proj); err == nil {
		return proj
	}
	return ""
}

// SetWriteApproval persists skills.write_approval to the config file at path.
func SetWriteApproval(path string, on bool) error {
	return Set(path, "skills.write_approval", strconv.FormatBool(on))
}

// SetProvider persists a provider selection (server_url + model + api_key) to
// the config file at path, atomically in catalog order. api_key may be empty
// (local provider, or a cloud key read from the environment rather than
// stored). Applying three Set calls is fine: each is independent and validated.
// SetProvider persists a provider selection and, for cloud providers, raises
// the context_window config to the model's real (models.dev-documented) context
// length — cloud APIs are not GPU-bound, so a leftover local VRAM value should
// not cap the budget. Returns the window that was raised to (0 = unchanged).
func SetProvider(path string, p Provider, model, apiKey string) (int, error) {
	if err := Set(path, "server_url", p.BaseURL); err != nil {
		return 0, err
	}
	if model != "" {
		if err := Set(path, "model", model); err != nil {
			return 0, err
		}
	}
	// Cloud APIs are not GPU-bound: a model's real context window (from the
	// models.dev index) can be far larger than the local context_window config
	// (a VRAM-bound value). Auto-raise the window to the model's documented
	// context so the agent's budget uses the actual capacity, not a leftover
	// GPU value. Local providers have no ModelsDev key -> no-op. Runs before
	// the api_key write (the key path returns early below).
	var raised int
	if p.ModelsDev != "" && model != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if n := ContextLength(ctx, p.ModelsDev, model); n > 0 {
			n = min(n, maxCloudContextWindow)
			if err := Set(path, "context_window", strconv.Itoa(n)); err == nil {
				raised = n
			}
		}
	}
	if apiKey != "" {
		if err := Set(path, "api_key", apiKey); err != nil {
			return 0, err
		}
	}
	return raised, nil
}

// SelectProvider applies a catalog selection to the config in memory and
// returns the api key to use (from config or the provider's env var). It does
// not persist — callers persist via SetProvider or the generic Set calls.
func (c *Config) SelectProvider(p Provider, model string) string {
	c.ServerURL = p.BaseURL
	if model != "" {
		c.Model = model
	}
	c.APIKey = c.KeyFor(p)
	return c.APIKey
}

// SettingKey is a dotted config key; Settings lists them for the /settings UI.
// Options, when non-empty, makes the field a chooser (select from the list)
// instead of free text.
type SettingKey struct {
	Key     string
	Label   string
	Options []string
}

// Settings is the ordered catalog of editable settings.
func Settings() []SettingKey {
	return []SettingKey{
		{Key: "server_url", Label: "Server URL"},
		{Key: "model", Label: "Model"},
		{Key: "api_key", Label: "API key (Bearer auth, cloud)"},
		{Key: "embedding_model", Label: "Embedding model"},
		{Key: "embedding_server_url", Label: "Embedding server URL"},
		{Key: "context_window", Label: "Context window (tokens)"},
		{Key: "data_dir", Label: "Data dir"},
		{Key: "theme", Label: "TUI theme", Options: ThemeOptions},
		{Key: "sampling.temperature", Label: "Sampling temperature"},
		{Key: "sampling.top_p", Label: "Sampling top_p"},
		{Key: "sampling.top_k", Label: "Sampling top_k (0 = off)"},
		{Key: "sampling.repetition_penalty", Label: "Sampling repetition penalty (0 = off)"},
		{Key: "sampling.min_p", Label: "Sampling min_p (0 = off; llama.cpp/Ollama)"},
		{Key: "sampling.reasoning_max_tokens", Label: "Reasoning cap per request (0 = off; speeds up reasoning models)"},
		{Key: "ui.show_reasoning", Label: "Show thinking block", Options: []string{"true", "false"}},
		{Key: "ui.loop_guard", Label: "Stop repeating-generation loops", Options: []string{"true", "false"}},
		{Key: "ui.accessibility", Label: "TUI accessibility", Options: []string{"standard", "high-contrast", "ascii"}},
		{Key: "ui.reduced_motion", Label: "Reduce animation (static spinner)", Options: []string{"false", "true"}},
		{Key: "web_search.provider", Label: "Web search provider", Options: []string{"duckduckgo", "mojeek", "searxng", "langsearch"}},
		{Key: "web_search.searxng_url", Label: "SearXNG URL"},
		{Key: "web_search.max_fetch_kib", Label: "web_fetch text cap (KiB, 0 = 32)"},
		{Key: "web_search.langsearch_api_key", Label: "LangSearch API key (free web-search API)"},
		{Key: "web_search.papers", Label: "paper_search tool (arXiv + PubMed + Semantic Scholar)", Options: []string{"false", "true"}},
		{Key: "web_search.semanticscholar_api_key", Label: "Semantic Scholar API key (paper search)"},
		{Key: "skills.write_approval", Label: "Skills write approval", Options: []string{"false", "true"}},
		{Key: "skills.data_dir", Label: "Skills data dir"},
		{Key: "skills.project_dir", Label: "Skills project dir"},
		{Key: "shell.sandbox", Label: "Shell sandbox", Options: []string{"", "bwrap", "unsafe"}},
		{Key: "vram_threshold_tps", Label: "VRAM pressure t/s threshold (0 = off; auto-prunes context when streaming slows)"},
		{Key: "codegen", Label: "Codegen mode (whole-file writes + compile-gated final answers)", Options: []string{"false", "true"}},
		{Key: "git_auto_commit", Label: "Auto-commit each turn to git (/undo = revert; needs a git repo)", Options: []string{"true", "false"}},
		{Key: "consult.server_url", Label: "Consult server URL"},
		{Key: "consult.model", Label: "Consult model"},
		{Key: "consult.api_key", Label: "Consult API key"},
		{Key: "consult.cmd", Label: "Consult CLI app (space-separated argv)"},
		{Key: "summarizer.server_url", Label: "Summarizer server URL (empty = main server)"},
		{Key: "summarizer.model", Label: "Summarizer model (empty = main model)"},
	}
}

// Get returns the current value of a dotted setting key.
func (c *Config) Get(key string) string {
	switch key {
	case "server_url":
		return c.ServerURL
	case "model":
		return c.Model
	case "api_key":
		return c.APIKey
	case "embedding_model":
		return c.EmbeddingModel
	case "embedding_server_url":
		return c.EmbeddingServerURL
	case "context_window":
		return strconv.Itoa(c.ContextWindow)
	case "data_dir":
		return c.DataDir
	case "theme":
		return c.Theme
	case "sampling.temperature":
		return strconv.FormatFloat(c.Sampling.Temperature, 'f', -1, 64)
	case "sampling.top_p":
		return strconv.FormatFloat(c.Sampling.TopP, 'f', -1, 64)
	case "sampling.top_k":
		return strconv.Itoa(c.Sampling.TopK)
	case "sampling.repetition_penalty":
		return strconv.FormatFloat(c.Sampling.RepetitionPenalty, 'f', -1, 64)
	case "sampling.min_p":
		return strconv.FormatFloat(c.Sampling.MinP, 'f', -1, 64)
	case "sampling.reasoning_max_tokens":
		return strconv.Itoa(c.Sampling.ReasoningMaxTokens)
	case "ui.show_reasoning":
		return strconv.FormatBool(c.UI.ShowReasoning)
	case "ui.loop_guard":
		return strconv.FormatBool(c.UI.LoopGuard)
	case "ui.accessibility":
		return c.UI.Accessibility
	case "ui.reduced_motion":
		return strconv.FormatBool(c.UI.ReducedMotion)
	case "web_search.provider":
		return c.Web.Provider
	case "web_search.searxng_url":
		return c.Web.SearxngURL
	case "web_search.max_fetch_kib":
		return strconv.Itoa(c.Web.MaxFetchKib)
	case "web_search.langsearch_api_key":
		return c.Web.LangSearchKey
	case "web_search.papers":
		return strconv.FormatBool(c.Web.Papers)
	case "web_search.semanticscholar_api_key":
		return c.Web.SemanticScholarKey
	case "skills.write_approval":
		return strconv.FormatBool(c.Skills.WriteApproval)
	case "skills.data_dir":
		return c.Skills.DataDir
	case "skills.project_dir":
		return c.Skills.ProjectDir
	case "shell.sandbox":
		return c.Shell.Sandbox
	case "vram_threshold_tps":
		return strconv.FormatFloat(c.VramThresholdTPS, 'f', -1, 64)
	case "codegen":
		return strconv.FormatBool(c.Codegen)
	case "git_auto_commit":
		return strconv.FormatBool(c.AutoCommitGit)
	case "consult.server_url":
		return c.Consult.ServerURL
	case "consult.model":
		return c.Consult.Model
	case "consult.api_key":
		return c.Consult.APIKey
	case "consult.cmd":
		return strings.Join(c.Consult.Cmd, " ")
	case "summarizer.server_url":
		return c.Summarizer.ServerURL
	case "summarizer.model":
		return c.Summarizer.Model
	}
	return ""
}

// Set persists a dotted config key (e.g. "web_search.provider",
// "consult.model") to the config file at path, validating known keys and
// values. Returns a ValidationError for unknown keys or bad values.
func Set(path, key, value string) error {
	if path == "" {
		return fmt.Errorf("no config file path is known")
	}
	parts := strings.Split(key, ".")
	if err := validateKey(parts, value); err != nil {
		return err
	}
	doc, m, err := loadConfigNode(path)
	if err != nil {
		return err
	}
	node := m
	for _, part := range parts[:len(parts)-1] {
		child := mappingValue(node, part)
		if child == nil || child.Kind != yaml.MappingNode {
			child = &yaml.Node{Kind: yaml.MappingNode}
			setMappingKey(node, part, child)
		}
		node = child
	}
	setMappingKey(node, parts[len(parts)-1], typedScalar(parts[len(parts)-1], value))
	// Cross-field validation: a provider/key combination that would make the
	// next `yagent chat` fail to start must be rejected here, not written to
	// disk (newChatEnv fails hard on searxng-without-url / langsearch-without-
	// key / unknown provider). Parse the updated tree and check.
	if err := validateCrossField(doc); err != nil {
		return err
	}
	return saveConfigNode(doc, path)
}

// validateCrossField rejects config files whose web_search provider/key
// combination would brick `yagent chat` at startup (newChatEnv fails hard on
// these, so /set must not persist them).
func validateCrossField(doc *yaml.Node) error {
	var c Config
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil // can't render; let the caller see the real error
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil
	}
	switch c.Web.Provider {
	case "searxng":
		if c.Web.SearxngURL == "" {
			return &ValidationError{msg: "web_search.provider searxng requires web_search.searxng_url (or `yagent chat` will fail to start)"}
		}
	case "langsearch":
		if c.Web.LangSearchKey == "" {
			return &ValidationError{msg: "web_search.provider langsearch requires web_search.langsearch_api_key (or `yagent chat` will fail to start)"}
		}
	case "", "duckduckgo", "mojeek":
	default:
		return &ValidationError{msg: fmt.Sprintf("unknown web_search.provider %q (duckduckgo | mojeek | searxng | langsearch)", c.Web.Provider)}
	}
	return nil
}

// loadConfigNode reads the config file into yaml nodes (creating an empty one
// when the file does not exist) and returns the document and root mapping.
func loadConfigNode(path string) (*yaml.Node, *yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, doc); err != nil {
			return nil, nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("config %s is not a mapping", path)
	}
	return doc, m, nil
}

func saveConfigNode(doc *yaml.Node, path string) error {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// validateKey rejects unknown keys and invalid values.
func validateKey(parts []string, value string) error {
	known := map[string]bool{
		"server_url": true, "model": true, "api_key": true, "embedding_model": true,
		"embedding_server_url": true, "context_window": true, "data_dir": true,
		"theme":                true,
		"sampling.temperature": true, "sampling.top_p": true,
		"sampling.top_k": true, "sampling.repetition_penalty": true,
		"sampling.min_p": true, "sampling.reasoning_max_tokens": true,
		"ui.show_reasoning":   true,
		"ui.loop_guard":       true,
		"ui.accessibility":    true,
		"ui.reduced_motion":   true,
		"web_search.provider": true, "web_search.searxng_url": true,
		"web_search.max_fetch_kib":      true,
		"web_search.langsearch_api_key": true, "web_search.papers": true,
		"web_search.semanticscholar_api_key": true,
		"skills.write_approval":              true, "skills.data_dir": true, "skills.project_dir": true,
		"shell.sandbox":      true,
		"vram_threshold_tps": true,
		"codegen":            true,
		"git_auto_commit":    true,
		"consult.server_url": true, "consult.model": true, "consult.api_key": true,
		"consult.cmd":           true,
		"summarizer.server_url": true, "summarizer.model": true,
	}
	key := strings.Join(parts, ".")
	if !known[key] {
		return &ValidationError{msg: fmt.Sprintf("unknown setting %q (see /settings)", key)}
	}
	switch key {
	case "context_window":
		n, err := strconv.Atoi(value)
		if err != nil || n < 100 {
			return &ValidationError{msg: "context_window must be an integer >= 100"}
		}
	case "skills.write_approval":
		if value != "true" && value != "false" {
			return &ValidationError{msg: "skills.write_approval must be true or false"}
		}
	case "web_search.provider":
		switch value {
		case "duckduckgo", "mojeek", "searxng", "langsearch":
		default:
			return &ValidationError{msg: "web_search.provider must be duckduckgo, mojeek, searxng or langsearch"}
		}
	case "web_search.papers":
		if value != "true" && value != "false" {
			return &ValidationError{msg: "web_search.papers must be true or false"}
		}
	case "web_search.langsearch_api_key", "web_search.semanticscholar_api_key":
		// empty value clears the key (allowed)
	case "web_search.max_fetch_kib":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 512 {
			return &ValidationError{msg: "web_search.max_fetch_kib must be an integer between 0 and 512"}
		}
	case "theme":
		if !slices.Contains(ThemeOptions, value) {
			return &ValidationError{msg: "theme must be one of: " + strings.Join(ThemeOptions, ", ")}
		}
	case "sampling.temperature", "sampling.top_p", "sampling.repetition_penalty":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 2 {
			return &ValidationError{msg: key + " must be a number between 0 and 2"}
		}
	case "vram_threshold_tps":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || validateVramThreshold(f) != nil {
			return &ValidationError{msg: "vram_threshold_tps must be a finite number >= 0 (0 = off)"}
		}
	case "sampling.top_k":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return &ValidationError{msg: "sampling.top_k must be a non-negative integer (0 = off)"}
		}
	case "sampling.min_p":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 1 {
			return &ValidationError{msg: "sampling.min_p must be a number between 0 and 1 (0 = off)"}
		}
	case "sampling.reasoning_max_tokens":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return &ValidationError{msg: "sampling.reasoning_max_tokens must be a non-negative integer (0 = off)"}
		}
	case "ui.show_reasoning", "ui.loop_guard", "codegen", "git_auto_commit":
		if value != "true" && value != "false" {
			return &ValidationError{msg: key + " must be true or false"}
		}
	case "ui.accessibility":
		if !slices.Contains([]string{"standard", "high-contrast", "ascii"}, value) {
			return &ValidationError{msg: "ui.accessibility must be standard, high-contrast or ascii"}
		}
	case "ui.reduced_motion":
		if value != "true" && value != "false" {
			return &ValidationError{msg: "ui.reduced_motion must be true or false"}
		}
	case "shell.sandbox":
		if value != "" && value != "bwrap" && value != "unsafe" {
			return &ValidationError{msg: "shell.sandbox must be empty, bwrap, or unsafe"}
		}
	case "api_key":
		// empty value means "clear the stored key" (revert to env-var-only).
	default:
		if value == "" {
			return &ValidationError{msg: key + " cannot be empty"}
		}
	}
	return nil
}

// typedScalar renders a leaf value as the right scalar tag. Set() passes the
// last dotted segment (e.g. "top_k" for "sampling.top_k"), matching the
// existing write_approval/context_window convention.
func typedScalar(key, value string) *yaml.Node {
	switch key {
	case "write_approval", "show_reasoning", "loop_guard", "papers", "codegen", "git_auto_commit", "reduced_motion":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value}
	case "context_window", "top_k", "reasoning_max_tokens", "max_fetch_kib":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value}
	case "temperature", "top_p", "repetition_penalty", "min_p", "vram_threshold_tps":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: value}
	case "cmd":
		// consult.cmd is stored as a YAML sequence: "/set consult.cmd claude -p"
		// round-trips to `cmd: [claude, -p]` (ConsultConfig.Cmd is []string).
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: func() []*yaml.Node {
			var items []*yaml.Node
			for _, part := range strings.Fields(value) {
				items = append(items, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part})
			}
			return items
		}()}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// ValidationError marks a bad setting key or value (feeds back to the UI).
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

// mappingValue returns the value node for key in a mapping, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setMappingKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// empty scalar value removes the key entirely (e.g. clearing api_key);
			// sequence/mapping values (consult.cmd, nested blocks) are never
			// empty scalars so they are untouched.
			if value.Kind == yaml.ScalarNode && value.Value == "" {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
				return
			}
			m.Content[i+1] = value
			return
		}
	}
	if value.Kind == yaml.ScalarNode && value.Value == "" {
		return // nothing to add
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value)
}

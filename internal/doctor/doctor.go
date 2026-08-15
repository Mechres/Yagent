// Package doctor implements `yagent doctor`: a local-first diagnostic that
// checks config, server reachability, the configured model, the embeddings
// endpoint and the data dir, and reports PASS/WARN/FAIL so failures exit
// non-zero.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mechres/Yagent/internal/bench"
	"github.com/Mechres/Yagent/internal/config"
	"github.com/Mechres/Yagent/internal/memory"
)

// Status of one check.
type Status int

const (
	StatusPass Status = iota
	StatusWarn
	StatusInfo
	StatusFail
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "PASS"
	case StatusWarn:
		return "WARN"
	case StatusInfo:
		return "INFO"
	default:
		return "FAIL"
	}
}

// Check is one diagnostic line.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// Report is the full diagnostic result.
type Report struct {
	Checks   []Check
	Failures int
}

func (r *Report) add(name string, st Status, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: st, Detail: detail})
	if st == StatusFail {
		r.Failures++
	}
}

// Render prints the report; returns an error to exit non-zero when any check
// failed.
func (r *Report) Render(w io.Writer) error {
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-6s %-18s %s\n", c.Status, c.Name+":", c.Detail)
	}
	if r.Failures > 0 {
		fmt.Fprintf(w, "\n%d check(s) failed\n", r.Failures)
		return fmt.Errorf("%d check(s) failed", r.Failures)
	}
	fmt.Fprintf(w, "\nall checks passed\n")
	return nil
}

const timeout = 25 * time.Second

// addProjectToolchain inspects the current working directory's project marker
// files and verifies the matching toolchain binary is on PATH, so
// workspace_diagnostics / test_runner won't hit a missing tool (agy #5).
func (r *Report) addProjectToolchain() {
	ws, err := os.Getwd()
	if err != nil {
		return
	}
	has := func(rel string) bool {
		_, err := os.Stat(filepath.Join(ws, rel))
		return err == nil
	}
	check := func(marker string, toolNames []string) {
		if !has(marker) {
			return
		}
		for _, bin := range toolNames {
			if _, err := exec.LookPath(bin); err == nil {
				r.add("toolchain", StatusPass, bin+" available for "+marker)
				return
			}
		}
		r.add("toolchain", StatusFail, "project uses "+marker+" but none of "+strings.Join(toolNames, ", ")+" is on PATH — workspace_diagnostics/test_runner will error")
	}
	if has("go.mod") {
		check("go.mod", []string{"go"})
	}
	if has("Cargo.toml") {
		check("Cargo.toml", []string{"cargo", "rustc"})
	}
	if has("package.json") {
		check("package.json", []string{"npx", "node"})
	}
	if has("pyproject.toml") {
		check("pyproject.toml", []string{"python3", "python"})
	} else if has("requirements.txt") {
		check("requirements.txt", []string{"python3", "python"})
	} else if has("setup.py") {
		check("setup.py", []string{"python3", "python"})
	}
	if has("Makefile") || has("makefile") {
		check("Makefile", []string{"make"})
	}
	if has("CMakeLists.txt") {
		check("CMakeLists.txt", []string{"cmake"})
	}
}

// addStorage audits what the agent remembers (L3 semantic memories, sessions,
// checkpoints) so `yagent doctor` doubles as a storage health report and the
// human can prune growth (companion to `yagent memory`).
func (r *Report) addStorage(cfg *config.Config) {
	memCount := "n/a"
	if vs, err := memory.OpenVectorStore(cfg.DataDir, cfg.EmbeddingServerURL, cfg.EmbeddingModel); err == nil {
		memCount = fmt.Sprint(vs.Count())
		vs.Close()
	}
	parts := []string{"memories " + memCount}
	if st, err := memory.Open(cfg.DataDir); err == nil {
		n, _ := st.CountSessions()
		parts = append(parts, fmt.Sprintf("sessions %d", n))
		st.Close()
	}
	if n, sz := countCheckpoints(cfg.DataDir); n > 0 {
		parts = append(parts, fmt.Sprintf("checkpoints %d (%.1f MB)", n, sz))
	} else {
		parts = append(parts, "checkpoints 0")
	}
	if sz, err := dirSizeMB(cfg.DataDir); err == nil {
		parts = append(parts, fmt.Sprintf("data dir %.1f MB", sz))
	}
	rep := strings.Join(parts, ", ")
	if memCount != "0" {
		r.add("storage", StatusInfo, rep+" — audit with `yagent memory list`")
		return
	}
	r.add("storage", StatusInfo, rep)
}

// countCheckpoints counts .yagent/checkpoints dirs and their total size (the
// per-workspace checkpoint layout).
func countCheckpoints(root string) (int, float64) {
	var n int
	var sz int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "checkpoints" && strings.Contains(path, string(filepath.Separator)+".yagent"+string(filepath.Separator)) {
			_ = filepath.WalkDir(path, func(p string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() {
					if fi, err := e.Info(); err == nil {
						sz += fi.Size()
					}
				}
				return nil
			})
			n++
		}
		return nil
	})
	return n, float64(sz) / (1 << 20)
}

func dirSizeMB(dir string) (float64, error) {
	var sz int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			sz += fi.Size()
		}
		return nil
	})
	return float64(sz) / (1 << 20), err
}

// Run executes the diagnostics against cfg.
func Run(cfg *config.Config) Report {
	var rep Report

	// --- config ---
	u, err := url.Parse(cfg.ServerURL)
	switch {
	case cfg.ServerURL == "":
		rep.add("config", StatusFail, "server_url is empty")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https"):
		rep.add("config", StatusFail, fmt.Sprintf("server_url %q is not a valid http(s) URL", cfg.ServerURL))
	default:
		rep.add("config", StatusPass, fmt.Sprintf("server_url %s, model %q, data dir %s, context window %d", cfg.ServerURL, cfg.Model, cfg.DataDir, cfg.ContextWindow))
	}
	if n, ok := serverNCtx(cfg.ServerURL); ok {
		if cfg.ContextWindow > n {
			rep.add("context window", StatusWarn, fmt.Sprintf("configured %d exceeds the server's n_ctx %d — the agent caps its budget at startup", cfg.ContextWindow, n))
		} else {
			rep.add("context window", StatusPass, fmt.Sprintf("server n_ctx %d (agent budget %d)", n, cfg.ContextWindow))
		}
	}
	rep.addServerPerf(cfg)
	if cfg.APIKey != "" {
		rep.add("config", StatusInfo, "api_key set — requests are sent to a cloud OpenAI-compatible endpoint (opt-in)")
	}
	if cfg.ProjectPath != "" {
		rep.add("project config", StatusInfo, fmt.Sprintf("%s overlays the global config", cfg.ProjectPath))
	}
	if cfg.Model == "" {
		rep.add("model", StatusFail, "model is empty (set YAGENT_MODEL / model)")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		rep.add("data dir", StatusFail, fmt.Sprintf("cannot create %s: %v", cfg.DataDir, err))
	} else if probe := filepath.Join(cfg.DataDir, ".doctor-write-test"); os.WriteFile(probe, []byte("x"), 0o644) == nil {
		os.Remove(probe)
		rep.add("data dir", StatusPass, fmt.Sprintf("%s is writable", cfg.DataDir))
	} else {
		rep.add("data dir", StatusFail, fmt.Sprintf("%s is not writable", cfg.DataDir))
	}

	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		// can't reach anything; stop here
		return rep
	}

	// --- local tooling ---
	if cfg.Shell.Sandbox == "bwrap" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			rep.add("sandbox", StatusFail, "shell.sandbox is bwrap but bubblewrap is not installed")
		} else {
			rep.add("sandbox", StatusPass, "bubblewrap found for shell.sandbox")
		}
	} else {
		rep.add("sandbox", StatusInfo, "shell sandbox disabled (set shell.sandbox: bwrap to enable)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		rep.add("git", StatusWarn, "git not on PATH; the git tools will error")
	} else {
		rep.add("git", StatusPass, "git available")
	}
	// Project toolchain (agy #5): detect the cwd's project type and verify the
	// diagnostic/test tool is on PATH, so the agent never hits a missing
	// binary (cargo/tsc/ruff/go) when it calls workspace_diagnostics.
	rep.addProjectToolchain()
	if cfg.Consult.Model != "" {
		backend := cfg.Consult.ServerURL
		if backend == "" {
			backend = cfg.ServerURL
		}
		note := ""
		if cfg.Consult.APIKey != "" {
			note = " (cloud endpoint, opt-in)"
		}
		rep.add("consult", StatusInfo, "advisor: "+cfg.Consult.Model+" @ "+backend+note)
	} else if len(cfg.Consult.Cmd) > 0 {
		rep.add("consult", StatusInfo, "advisor: terminal app "+cfg.Consult.Cmd[0])
	} else {
		rep.add("consult", StatusInfo, "advisor disabled (set consult.* or consult.cmd)")
	}

	// --- web_search config (a misconfigured provider bricks `yagent chat`
	// at startup — newChatEnv fails hard, so doctor must catch it here) ---
	switch cfg.Web.Provider {
	case "", "duckduckgo", "mojeek":
		rep.add("web_search", StatusPass, "provider "+orDefault(cfg.Web.Provider, "duckduckgo"))
	case "searxng":
		if cfg.Web.SearxngURL == "" {
			rep.add("web_search", StatusFail, "provider searxng requires web_search.searxng_url — `yagent chat` will not start until it is set")
		} else {
			rep.add("web_search", StatusPass, "provider searxng @ "+cfg.Web.SearxngURL)
		}
	case "langsearch":
		if cfg.Web.LangSearchKey == "" {
			rep.add("web_search", StatusFail, "provider langsearch requires web_search.langsearch_api_key — `yagent chat` will not start until it is set")
		} else {
			rep.add("web_search", StatusPass, "provider langsearch (key set)")
		}
	default:
		rep.add("web_search", StatusFail, fmt.Sprintf("unknown provider %q (duckduckgo | mojeek | searxng | langsearch) — `yagent chat` will not start", cfg.Web.Provider))
	}
	if cfg.Web.Papers && cfg.Web.SemanticScholarKey == "" {
		rep.add("web_search", StatusInfo, "papers enabled without a semantic scholar key — the Semantic Scholar index is rate-limited to ~1 req/s without one")
	}

	// --- bench regression gate (T1-2) ---
	if cfg.Model != "" {
		base := bench.LoadBaseline(cfg.DataDir)
		if s := base.ScoreString(cfg.Model); s != "" {
			if reg := base.Regression(cfg.Model); reg != "" {
				rep.add("bench", StatusWarn, reg)
			} else {
				rep.add("bench", StatusPass, "recorded baseline "+s)
			}
		} else {
			rep.add("bench", StatusInfo, "no recorded baseline (run `yagent bench --repeat 3`)")
		}
	}

	// --- storage usage (yagent memory audit companion) ---
	rep.addStorage(cfg)

	client := &http.Client{Timeout: timeout}

	// --- server reachable + model list ---
	models, errText := fetchModels(client, cfg.ServerURL, cfg.APIKey)
	if errText != "" {
		rep.add("server", StatusFail, errText)
		return rep
	}
	rep.add("server", StatusPass, fmt.Sprintf("%s reachable", serverName(client, cfg.ServerURL)))

	modelFound := false
	for _, m := range models {
		// Substring match: llama.cpp lists the full model path
		// (/home/.../Qwen3VL-8B-Instruct-Q4_K_M.gguf) and Ollama lists
		// "name:tag" — an exact match would false-warn on both.
		if strings.Contains(m, cfg.Model) || strings.Contains(cfg.Model, m) {
			modelFound = true
			break
		}
	}
	switch {
	case len(models) == 0:
		rep.add("model", StatusWarn, "server returned no models; cannot verify "+cfg.Model)
	case modelFound:
		rep.add("model", StatusPass, cfg.Model)
	default:
		rep.add("model", StatusWarn, fmt.Sprintf("%q not in server model list (%s); check YAGENT_MODEL", cfg.Model, strings.Join(models, ", ")))
	}

	// --- embeddings endpoint ---
	// Probe the embedding server (a dedicated embedding_server_url when set,
	// else the main server) — this is the endpoint L3 memory/index actually
	// uses, so a green check here means memory really works.
	embedBase := cfg.EmbeddingServerURL
	if embedBase == "" {
		embedBase = cfg.ServerURL
	}
	switch dim, err := probeEmbeddings(client, embedBase, cfg.EmbeddingModel, cfg.APIKey); {
	case err == nil:
		rep.add("embeddings", StatusPass, fmt.Sprintf("/v1/embeddings OK (%d-dim) @ %s", dim, embedBase))
	case strings.Contains(err.Error(), "501") || strings.Contains(err.Error(), "not support embeddings"):
		rep.add("embeddings", StatusWarn, "server does not serve embeddings (start llama-server with --embeddings --pooling mean, or use Ollama nomic-embed-text)")
	default:
		rep.add("embeddings", StatusWarn, fmt.Sprintf("/v1/embeddings: %v", err))
	}

	// --- chat sanity ---
	if err := probeChat(client, cfg.ServerURL, cfg.Model, cfg.APIKey); err != nil {
		rep.add("chat", StatusFail, fmt.Sprintf("model did not answer: %v", err))
	} else {
		rep.add("chat", StatusPass, "model answered a ping")
	}

	// --- backend / GPU (best-effort; not every server exposes it) ---
	rep.Checks = append(rep.Checks, serverBackend(client, cfg.ServerURL))

	return rep
}

// baseURL normalizes a configured server URL for the /v1/* suffixes: it strips
// a trailing "/v1" so /v1/models, /v1/embeddings and /v1/chat/completions
// resolve to <base>/v1/<endpoint>. Without this, a provider whose documented
// base already ends in /v1 (NVIDIA NIM, Together, Mistral) produces
// /v1/v1/... -> 404 — the same bug llm.baseURL fixed for the agent loop.
func baseURL(serverURL string) string {
	u := strings.TrimRight(serverURL, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	return u
}

// orDefault returns value, or def when value is empty.
func orDefault(value, def string) string {
	if value == "" {
		return def
	}
	return value
}

func fetchModels(client *http.Client, base, apiKey string) ([]string, string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		baseURL(base)+"/v1/models", nil)
	if err != nil {
		return nil, err.Error()
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Sprintf("cannot reach %s: %v", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Sprintf("GET /v1/models returned %s", resp.Status)
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
		return nil, ""
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
	return out, ""
}

func probeEmbeddings(client *http.Client, base, model, apiKey string) (int, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": "doctor probe"})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL(base)+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	if len(out.Data) != 1 {
		return 0, fmt.Errorf("expected 1 embedding, got %d", len(out.Data))
	}
	return len(out.Data[0].Embedding), nil
}

func probeChat(client *http.Client, base, model, apiKey string) error {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
		"stream":   false,
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL(base)+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return fmt.Errorf("empty answer")
	}
	return nil
}

// addServerPerf reports inference-engine performance flags when the server
// exposes them, and flags large-context risk on ≤12 GB GPUs (agy #5). KV cache
// quantization is exposed by some llama.cpp builds; flash-attn is a launch
// flag not in /props, so that part is best-effort guidance only.
func (r *Report) addServerPerf(cfg *config.Config) {
	props, ok := probeServerProps(cfg.ServerURL)
	if !ok {
		return // Ollama or non-llama.cpp; nothing to audit
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("n_ctx %d", props.NCtx))
	if props.CacheK != "" || props.CacheV != "" {
		parts = append(parts, fmt.Sprintf("kv cache %s/%s", props.CacheK, props.CacheV))
	}
	detail := "server: " + strings.Join(parts, ", ")
	// Only flag when the server explicitly reports f16 KV; an absent field
	// means the build doesn't expose it, so we can't claim it's unquantized
	// (that would nag users on every run).
	if cfg.ContextWindow > 16384 && props.CacheK == "f16" {
		detail += " — large context with unquantized KV cache may spill to RAM on ≤12 GB GPUs; consider launching llama-server with --cache-type-k q8_0 --cache-type-v q8_0 (and --flash-attn when supported)"
		r.add("server perf", StatusWarn, detail)
		return
	}
	r.add("server perf", StatusInfo, detail)
}

// probeServerProps fetches llama.cpp /props including KV cache types.
func probeServerProps(base string) (struct {
	NCtx   int
	CacheK string
	CacheV string
}, bool) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimRight(base, "/") + "/props")
	if err != nil {
		return struct {
			NCtx   int
			CacheK string
			CacheV string
		}{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return struct {
			NCtx   int
			CacheK string
			CacheV string
		}{}, false
	}
	var props struct {
		DefaultGenerationSettings struct {
			NCtx       int     `json:"n_ctx"`
			CacheTypeK *string `json:"cache_type_k,omitempty"`
			CacheTypeV *string `json:"cache_type_v,omitempty"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return struct {
			NCtx   int
			CacheK string
			CacheV string
		}{}, false
	}
	out := struct {
		NCtx   int
		CacheK string
		CacheV string
	}{NCtx: props.DefaultGenerationSettings.NCtx}
	if props.DefaultGenerationSettings.CacheTypeK != nil {
		out.CacheK = *props.DefaultGenerationSettings.CacheTypeK
	}
	if props.DefaultGenerationSettings.CacheTypeV != nil {
		out.CacheV = *props.DefaultGenerationSettings.CacheTypeV
	}
	return out, out.NCtx > 0
}

// serverNCtx reads the server's real context window from llama.cpp /props
// (P2). ok=false when the server doesn't expose it (e.g. Ollama).
func serverNCtx(base string) (int, bool) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimRight(base, "/") + "/props")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var props struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil || props.DefaultGenerationSettings.NCtx <= 0 {
		return 0, false
	}
	return props.DefaultGenerationSettings.NCtx, true
}

// serverName distinguishes llama.cpp from Ollama for the reachability line.
func serverName(client *http.Client, base string) string {
	resp, err := client.Get(strings.TrimRight(base, "/") + "/props")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return "llama.cpp server"
		}
	}
	return "inference server"
}

// serverBackend reports a best-effort backend/device line.
func serverBackend(client *http.Client, base string) Check {
	baseURL := strings.TrimRight(base, "/")
	// llama.cpp exposes device info under /props in builds that report it.
	if resp, err := client.Get(baseURL + "/props"); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var props map[string]any
			if json.NewDecoder(resp.Body).Decode(&props) == nil {
				for k := range props {
					if strings.HasPrefix(strings.ToLower(k), "device") {
						return Check{Name: "backend", Status: StatusInfo,
							Detail: fmt.Sprintf("server reports %s: %v", k, props[k])}
					}
				}
			}
		}
	}
	// Ollama exposes GPU info under /api/ps.
	if resp, err := client.Get(baseURL + "/api/ps"); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var ps struct {
				Models []struct {
					Details struct {
						Family string `json:"family"`
					} `json:"details"`
				} `json:"models"`
			}
			if json.NewDecoder(resp.Body).Decode(&ps) == nil && len(ps.Models) > 0 {
				return Check{Name: "backend", Status: StatusInfo,
					Detail: "Ollama with loaded models (GPU details not exposed here)"}
			}
		}
	}
	return Check{Name: "backend", Status: StatusInfo,
		Detail: "server does not report device/backend info"}
}

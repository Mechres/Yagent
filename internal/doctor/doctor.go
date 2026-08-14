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

const timeout = 5 * time.Second

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
	switch {
	case has("go.mod"):
		check("go.mod", []string{"go"})
	case has("Cargo.toml"):
		check("Cargo.toml", []string{"cargo", "rustc"})
	case has("package.json"):
		check("package.json", []string{"npx", "node"})
	case has("pyproject.toml"):
		check("pyproject.toml", []string{"python3", "python"})
	case has("requirements.txt"):
		check("requirements.txt", []string{"python3", "python"})
	case has("setup.py"):
		check("setup.py", []string{"python3", "python"})
	}
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

	client := &http.Client{Timeout: timeout}

	// --- server reachable + model list ---
	models, errText := fetchModels(client, cfg.ServerURL)
	if errText != "" {
		rep.add("server", StatusFail, errText)
		return rep
	}
	rep.add("server", StatusPass, fmt.Sprintf("%s reachable", serverName(client, cfg.ServerURL)))

	modelFound := false
	for _, m := range models {
		if m == cfg.Model {
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
	switch dim, err := probeEmbeddings(client, cfg.ServerURL, cfg.EmbeddingModel); {
	case err == nil:
		rep.add("embeddings", StatusPass, fmt.Sprintf("/v1/embeddings OK (%d-dim)", dim))
	case strings.Contains(err.Error(), "501") || strings.Contains(err.Error(), "not support embeddings"):
		rep.add("embeddings", StatusWarn, "server does not serve embeddings (start llama-server with --embeddings --pooling mean, or use Ollama nomic-embed-text)")
	default:
		rep.add("embeddings", StatusWarn, fmt.Sprintf("/v1/embeddings: %v", err))
	}

	// --- chat sanity ---
	if err := probeChat(client, cfg.ServerURL, cfg.Model); err != nil {
		rep.add("chat", StatusFail, fmt.Sprintf("model did not answer: %v", err))
	} else {
		rep.add("chat", StatusPass, "model answered a ping")
	}

	// --- backend / GPU (best-effort; not every server exposes it) ---
	rep.Checks = append(rep.Checks, serverBackend(client, cfg.ServerURL))

	return rep
}

func fetchModels(client *http.Client, base string) ([]string, string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err.Error()
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

func probeEmbeddings(client *http.Client, base, model string) (int, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": "doctor probe"})
	resp, err := client.Post(strings.TrimRight(base, "/")+"/v1/embeddings",
		"application/json", bytes.NewReader(body))
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

func probeChat(client *http.Client, base, model string) error {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
		"stream":   false,
	})
	resp, err := client.Post(strings.TrimRight(base, "/")+"/v1/chat/completions",
		"application/json", bytes.NewReader(body))
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

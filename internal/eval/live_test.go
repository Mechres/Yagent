package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/bench"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/tools"
)

// liveClient returns a client for a real, reachable local model, or skips the
// test. This is the real-hardware evidence harness (like TestShellExecSandboxLive):
// it needs no network for the unit suite (the probe skips when no server is up),
// but a running local server makes it a genuine end-to-end measurement.
func liveClient(t *testing.T) *llm.Client {
	t.Helper()
	url := os.Getenv("YAGENT_SERVER_URL")
	if url == "" {
		url = "http://localhost:8089"
	}
	model := os.Getenv("YAGENT_MODEL")
	if model == "" {
		model = "Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf"
	}
	client := llm.NewClient(url, model)
	client.Sampling = llm.Sampling{Temperature: 0.6, TopP: 0.95} // Qwythos recipe
	// The dev llama-server has a single slot; a stuck generation can queue a
	// probe behind it, so retry a few times before skipping.
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, err := client.ChatStream(ctx, []llm.Message{{Role: "user", Content: "reply with the single word ok"}}, nil, func(string) {}, nil)
		cancel()
		if err == nil {
			return client
		}
		t.Logf("live probe attempt %d: %v", attempt+1, err)
		time.Sleep(5 * time.Second)
	}
	t.Skipf("no live model at %s (3 probes failed)", url)
	return nil
}

// childBaseTask has a subagent enumerate exactly 18 fact values from a
// workspace (six files, three facts each, buried in decoy prose); the child's
// final message is the "summary" under test.
const childBaseTask = `Inspect every file in this workspace. There are exactly 6 files named fact-0.txt through fact-5.txt, and each contains exactly 3 fact values on lines marked "fact value:". Read every one of them. Report EVERY fact value you find, exactly as written, with the file it came from. Do not stop until you have listed all 18 values.`

// childConciseTask mirrors the real subagent system prompt ("finish with a
// concise summary") — the condition where free-text compression is most likely
// to drop facts.
const childConciseTask = `Inspect every file in this workspace. There are exactly 6 files named fact-0.txt through fact-5.txt, each containing 3 fact values on lines marked "fact value:". Read every one of them, then summarize your findings concisely: the facts you found and where. Keep the summary short.`

// recallValues feeds the child's report to a fresh comprehension pass and counts
// how many of the (high-entropy, uninventable) fact values survive. reported is
// how many facts the child's own summary surfaced; recalled is how many the
// parent reproduced. recall/reported is the summary-fidelity metric.
func recallValues(client *llm.Client, summary string, facts []string) (recalled, reported int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	resp, err := client.ChatStream(ctx, []llm.Message{
		{Role: "system", Content: "A subagent investigated a workspace and reported this:\n\n" + summary},
		{Role: "user", Content: "List every fact value the subagent reported. One per line, exact values only. Do not invent any value."},
	}, nil, func(string) {}, nil)
	if err != nil {
		return 0, 0, err
	}
	answer := resp.Message.Content
	for _, f := range facts {
		if strings.Contains(summary, f) {
			reported++
		}
		if strings.Contains(answer, f) {
			recalled++
		}
	}
	return recalled, reported, nil
}

// TestLiveTurnCancellation proves the Esc / loop-guard cancel actually stops a
// real generation mid-stream: the turn's context is cancelled after the first
// token and Run must return promptly (this is what keeps the TUI session alive
// when the user hits Esc or the loop guard fires).
func TestLiveTurnCancellation(t *testing.T) {
	if os.Getenv("YAGENT_LIVE_EVAL") == "" {
		t.Skip("set YAGENT_LIVE_EVAL=1 to run the real-hardware cancellation eval")
	}
	client := liveClient(t)
	ws := t.TempDir()
	reg := tools.NewRegistry(ws, tools.Options{ReadOnly: true})

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	a := agent.New(client, reg, nil, agent.Config{
		MaxIterations: 1,
		Window:        16384,
		OnToken: func(string) {
			select {
			case <-first:
			default:
				close(first)
			}
		},
	}, ws)

	done := make(chan struct{})
	var answer string
	var err error
	go func() {
		answer, err = a.Run(ctx, "Explain the history of the Roman Empire in at least ten long paragraphs.")
		close(done)
	}()

	select {
	case <-first:
	case <-time.After(90 * time.Second):
		t.Fatal("no token streamed within 90s")
	}
	cancel()
	select {
	case <-done:
		t.Logf("cancelled cleanly: err=%v", err)
		if err == nil && answer == "" {
			t.Error("Run returned with no error and no answer after a cancel")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return within 30s of cancellation")
	}
}

// TestLiveSmallModelBenchmark runs the canonical tasks under the default
// sampling and reports pass/fail — the baseline for tuning a local model.
// The tasks themselves live in internal/bench so `yagent calibrate` reuses them.
func TestLiveSmallModelBenchmark(t *testing.T) {
	if os.Getenv("YAGENT_LIVE_EVAL") == "" {
		t.Skip("set YAGENT_LIVE_EVAL=1 to run the real-hardware small-model benchmark")
	}
	client := liveClient(t)
	tasks := bench.Tasks()
	t.Logf("%-14s %-8s %s", "task", "result", "detail")
	for _, tk := range tasks {
		res := bench.RunTask(client, tk)
		t.Logf("%-14s %-8s %s", tk.Name, passWord(res.Pass), res.Detail)
	}
}

// TestLiveSamplingSweep runs the same benchmark across a few sampling recipes
// so the model's generation config is tuned on evidence, not folklore. Opt-in
// (YAGENT_LIVE_SWEEP=1) because it is 4× the benchmark cost.
func TestLiveSamplingSweep(t *testing.T) {
	if os.Getenv("YAGENT_LIVE_EVAL") == "" {
		t.Skip("set YAGENT_LIVE_EVAL=1 to run the real-hardware sampling sweep")
	}
	if os.Getenv("YAGENT_LIVE_SWEEP") == "" {
		t.Skip("set YAGENT_LIVE_SWEEP=1 to run the full sampling sweep (4x benchmark cost)")
	}
	client := liveClient(t)
	tasks := bench.Tasks()
	t.Logf("sampling-recipe sweep over %d tasks", len(tasks))
	for _, r := range bench.RunSweep(client, tasks) {
		t.Logf("%-12s %d/%d pass", r.Recipe.Name, r.Pass(), len(tasks))
	}
}

func passWord(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// jsonFindingsNote validates a JSON-array findings report, returning a short
// verdict note (or "" when not requested).
func jsonFindingsNote(summary string) string {
	start := strings.Index(summary, "[")
	end := strings.LastIndex(summary, "]")
	if start < 0 || end <= start {
		return "NO JSON ARRAY EMITTED"
	}
	var arr []struct {
		Value      string  `json:"value"`
		Path       string  `json:"path"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(summary[start:end+1]), &arr); err != nil {
		return "MALFORMED JSON: " + err.Error()
	}
	return fmt.Sprintf("VALID JSON, %d entries", len(arr))
}

// TestLiveSubagentInfoLoss measures whether the subagent's free-text summary
// loses facts the parent needs, versus a structured findings listing and strict
// JSON. It is the evidence for C3 (structured subagent returns): run it against
// a real local model with YAGENT_LIVE_EVAL=1 and read the RESULT table. It is
// opt-in so `go test ./...` stays fast even when a dev server is running.
func TestLiveSubagentInfoLoss(t *testing.T) {
	if os.Getenv("YAGENT_LIVE_EVAL") == "" {
		t.Skip("set YAGENT_LIVE_EVAL=1 to run the real-hardware subagent fidelity eval")
	}
	client := liveClient(t)
	ws := t.TempDir()
	// 18 high-entropy fact values (3 per file, buried in decoy prose) — the
	// "concise summary" condition forces the child to compress, which is where
	// free-text summaries are expected to drop facts.
	facts := []string{
		"x7zq2", "k4lm9", "p8rta", "q1wv6", "m3nbc", "t6hdp",
		"j9fkd", "b2wse", "v5qcz", "n8gtm", "r3xyl", "c7dhn",
		"h4pjb", "z6mkr", "l9scw", "f2tqy", "g8vnw", "d5jub",
	}
	for i := 0; i < 6; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "intro line for file %d — not a fact\n\n", i)
		for k := 0; k < 3; k++ {
			fmt.Fprintf(&b, "fact value: %s\n", facts[i*3+k])
			b.WriteString("context line about this value, unrelated to any other fact\n")
		}
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("fact-%d.txt", i)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := tools.NewRegistry(ws, tools.Options{ReadOnly: true})

	type variant struct {
		name      string
		task      string
		checkJSON bool
	}
	variants := []variant{
		{"free-text · exhaustive", childBaseTask, false},
		{"free-text · concise", childConciseTask, false},
		{"findings · concise", childConciseTask + "\n\nEnd with a findings list, one line per finding:\nvalue = <exact value>  |  file = <path>", false},
		{"strict JSON · concise", childConciseTask + "\n\nEnd with a JSON array of findings, then nothing else:\n[{\"value\":\"<exact value>\",\"path\":\"<file>\",\"confidence\":0.0}]", true},
	}
	// YAGENT_LIVE_VARIANT, when set, runs only that variant (e.g. to re-run
	// a single timed-out cell without spending the full cycle).
	if only := os.Getenv("YAGENT_LIVE_VARIANT"); only != "" {
		var filtered []variant
		for _, v := range variants {
			if v.name == only {
				filtered = append(filtered, v)
			}
		}
		if len(filtered) == 0 {
			t.Fatalf("unknown YAGENT_LIVE_VARIANT %q", only)
		}
		variants = filtered
	}

	type result struct {
		reported int
		recalled int
		note     string
		err      error
	}
	results := make(map[string]result, len(variants))

	for _, v := range variants {
		t.Logf("\n===== variant: %s =====", v.name)
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		summary, tokens, err := agent.RunSubagent(ctx, client, reg, v.task, ws, tools.SubagentRole{})
		cancel()
		r := result{err: err}
		if err != nil {
			t.Logf("child subagent failed: %v", err)
			results[v.name] = r
			continue
		}
		t.Logf("--- child report (subagent ~%d tokens) ---\n%s", tokens, summary)
		if v.checkJSON {
			r.note = jsonFindingsNote(summary)
			t.Logf("--- %s ---", r.note)
		}
		r.recalled, r.reported, err = recallValues(client, summary, facts)
		if err != nil {
			r.err = err
			t.Logf("recall pass failed: %v", err)
		} else {
			t.Logf("--- recall: child reported %d/%d facts, parent recalled %d/%d ---",
				r.reported, len(facts), r.recalled, len(facts))
		}
		results[v.name] = r
	}

	t.Logf("\n========== RESULT: subagent summary fidelity ==========")
	t.Logf("reported = facts surfaced by the child summary · recalled = reproduced by the parent")
	t.Logf("%-24s %-8s %-8s %-10s %s", "variant", "reported", "recalled", "surfaced%", "note")
	for _, v := range variants {
		r := results[v.name]
		pct := "-"
		if r.reported > 0 {
			pct = fmt.Sprintf("%d%%", r.recalled*100/r.reported)
		}
		errTxt := ""
		if r.err != nil {
			errTxt = "error: " + r.err.Error()
		}
		t.Logf("%-24s %-8d %-8d %-10s %s%s", v.name, r.reported, r.recalled, pct, r.note, errTxt)
	}
}

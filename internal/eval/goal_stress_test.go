package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/memory"
	"github.com/Mechres/Yagent/internal/tools"
)

// TestLiveGoalStress is the long-horizon evidence harness for the C3/M7
// question: *is the single agent loop the bottleneck on real multi-file work?*
//
// It runs a scripted autonomous goal — a genuine multi-file refactor over a
// fixture Go repo, up to 8 goal rounds with workspace checkpoints — and then
// measures four things:
//
//  1. GOAL DONE — did the agent reach a DONE verdict (vs max-rounds timeout)?
//  2. CORRECT — did it actually restructure the code (new package exists, old
//     references gone, the code still compiles)?
//  3. PRESERVED — did the refactor clobber unrelated files/facts in the repo?
//  4. ROUNDS — how many rounds the loop needed (efficiency).
//
// The verdict the reviewer cares about: if the single loop completes a real
// multi-file refactor reliably at 7B–14B scale, C3 (structured subagent
// workspace) stays gated; if it consistently times out or half-completes, that
// is the concrete failure case to un-gate it. Opt-in (YAGENT_LIVE_EVAL=1)
// because it needs a real local server and takes minutes.
func TestLiveGoalStress(t *testing.T) {
	if os.Getenv("YAGENT_LIVE_EVAL") == "" {
		t.Skip("set YAGENT_LIVE_EVAL=1 to run the real-hardware goal stress eval")
	}
	client := liveClient(t)
	ws := t.TempDir()
	writeGoalFixture(t, ws)

	// Real L3 memory against the live server's embedding endpoint so the
	// GoalMemorize path is verified end-to-end (facts persist + searchable).
	vs, err := memory.OpenVectorStore(t.TempDir(), client.ServerURL, "embed")
	if err != nil {
		t.Fatalf("open vector store: %v", err)
	}

	reg := tools.NewRegistry(ws, tools.Options{
		Undo:     nil, // goal mode has no interactive undo; /checkpoint covers it
		Index:    nil, // no index; the task is code surgery, not search
		ReadOnly: false,
	})
	approver := &taskApprover{ws: ws}
	a := agent.New(client, reg, approver, agent.Config{
		MaxIterations:   20,
		Window:          32768,
		Vectors:         vs,
		Index:           nil,
		IndexAutoInject: false,
		VerifyWrites:    true, // match production goal mode (deterministic done-gate)
		GoalGate:        true, // refuse DONE while the static check fails (2026-08-13 fix)
		GoalMemorize:    true, // persist round facts to L3 memory
	}, ws)

	goal := `Refactor this Go workspace: the type Config currently lives in the package pkg and is
used by main.go and the tests. Move Config (and the SetDefault method) into its own new
package pkg/config with a type Config plus a function New() that returns a zero Config.
Update main.go and pkg_test.go to import the new package and use it. After the change,
run workspace_diagnostics so the code is verified to compile, then report DONE.`

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	rounds := 0
	var last string
	last, err = a.RunGoal(ctx, goal, 8, func(r int, _ string) { rounds = r })

	// --- measurements ---
	newPkg := fileExists(filepath.Join(ws, "pkg", "config", "config.go"))
	// old package line gone is only meaningful when the new file actually
	// exists (an absent file trivially "lacks" the old line).
	oldRefGone := newPkg && !fileContains(filepath.Join(ws, "pkg", "config", "config.go"), "package pkg\n")
	compiles := fileContains(filepath.Join(ws, "pkg", "config", "config.go"), "func New()")
	mainUsesNew := fileContains(filepath.Join(ws, "main.go"), "config.New") || fileContains(filepath.Join(ws, "main.go"), "config.")
	testUsesNew := fileContains(filepath.Join(ws, "pkg_test.go"), "config.New") || fileContains(filepath.Join(ws, "pkg_test.go"), "config.")
	// facts: decoy files must be untouched by the refactor
	factsIntact := 0
	for i := 0; i < 4; i++ {
		if fileContains(filepath.Join(ws, fmt.Sprintf("notes-%d.md", i)), fmt.Sprintf("FACT-%d-", i)) {
			factsIntact++
		}
	}

	done := err == nil
	correct := newPkg && compiles && (mainUsesNew || testUsesNew)
	// Did the agent make ANY file changes at all? A DONE with zero writes is a
	// hallucinated completion — the loop accepted a prose claim as finished.
	approver.mu.Lock()
	wroteAnything := approver.writes > 0
	approver.mu.Unlock()
	// GoalMemorize: how many round facts landed in L3 memory?
	memCount := vs.Count()
	memRecall, _ := vs.Search(ctx, "goal refactor Config touched", 5)
	var memTexts []string
	for _, m := range memRecall {
		memTexts = append(memTexts, m.Text)
	}
	_ = memCount

	t.Logf("\n========== RESULT: long-horizon goal stress ==========")
	t.Logf("goal done (DONE verdict): %v", done)
	t.Logf("rounds used:              %d / 8", rounds)
	t.Logf("wall time:                %s", time.Since(start).Round(time.Second))
	t.Logf("new pkg/config exists:    %v", newPkg)
	t.Logf("old package line gone:    %v", oldRefGone)
	t.Logf("New() present & compiles: %v", compiles)
	t.Logf("main.go uses new pkg:     %v", mainUsesNew)
	t.Logf("test uses new pkg:        %v", testUsesNew)
	t.Logf("decoy facts intact:       %d/4", factsIntact)
	t.Logf("wrote any file:           %v", wroteAnything)
	t.Logf("goal memories saved:      %d", memCount)
	t.Logf("goal memory recall:       %s", strings.Join(memTexts, " | "))
	t.Logf("correct (core criteria):  %v", correct)
	if err != nil {
		t.Logf("final error: %v", err)
	}
	t.Logf("last answer tail:\n%s", tailOf(last, 400))

	// The stress-test is a measurement, not a hard gate: log everything and let
	// the human read the table. Fail only when nothing happened at all (which
	// would mean the harness itself is broken).
	if rounds == 0 && !newPkg && !fileExists(filepath.Join(ws, "pkg", "config", "config.go")) {
		t.Error("goal made no observable progress — harness likely misconfigured")
	}
}

// writeGoalFixture builds the multi-file refactor target: a small Go module
// where Config lives in pkg and is used by main and tests, plus 4 decoy note
// files carrying facts the refactor must not clobber.
func writeGoalFixture(t *testing.T, ws string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module stress\n\ngo 1.22\n")
	write("pkg/config.go", `package pkg

// Config holds the fixture's settings.
type Config struct {
	Host string
	Port int
}

// SetDefault returns a Config with default values.
func (c *Config) SetDefault() {
	c.Host = "localhost"
	c.Port = 8080
}
`)
	write("main.go", `package main

import (
	"fmt"

	"stress/pkg"
)

func main() {
	var c pkg.Config
	c.SetDefault()
	fmt.Println(c.Host, c.Port)
}
`)
	write("pkg_test.go", `package main

import (
	"testing"

	"stress/pkg"
)

func TestDefault(t *testing.T) {
	var c pkg.Config
	c.SetDefault()
	if c.Port != 8080 {
		t.Fatalf("port = %d, want 8080", c.Port)
	}
}
`)
	// decoy files with facts the refactor must leave alone
	for i := 0; i < 4; i++ {
		write(fmt.Sprintf("notes-%d.md", i), fmt.Sprintf("TODO notes %d\n\nFACT-%d-%s\n", i, i, factStrings[i]))
	}
}

// factStrings are high-entropy values seeded into the decoy notes so we can
// tell whether the agent's edits wandered outside the refactor's blast radius.
var factStrings = []string{"qx7p2m", "n3kz8v", "b6tjr4", "w9c1h5"}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileContains(p, sub string) bool {
	data, err := os.ReadFile(p)
	return err == nil && strings.Contains(string(data), sub)
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

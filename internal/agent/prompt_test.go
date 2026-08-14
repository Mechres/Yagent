package agent

import (
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/llm"
)

func TestCompactPromptSmaller(t *testing.T) {
	full := buildSystemPrompt("/tmp/x")
	compact := buildCompactSystemPrompt("/tmp/x")
	if len(compact) >= len(full) {
		t.Errorf("compact prompt not smaller: %d >= %d", len(compact), len(full))
	}
	// The compact variant must keep the operational core (tool JSON rule)
	// while dropping the worked examples.
	if len(full) < 1000 || len(compact) < 200 {
		t.Errorf("suspicious prompt sizes: full=%d compact=%d", len(full), len(compact))
	}
}

func TestCompactPromptSelectedUnderPressure(t *testing.T) {
	// assembleContext must swap to the compact prompt when usage > 70%.
	// Build an agent with a tiny window and lots of history so the estimate
	// crosses the threshold.
	ws := t.TempDir()
	a := &Agent{
		systemPrompt:  buildSystemPrompt(ws),
		compactPrompt: buildCompactSystemPrompt(ws),
		cfg:           Config{Window: 800},
	}
	// seed history large enough to blow past 70% of 800
	for i := 0; i < 20; i++ {
		a.history = append(a.history, historyEntry{
			msg:    llm.Message{Role: "user", Content: "a reasonably long message to inflate the token estimate"},
			tokens: 60,
		})
	}
	a.sysTokens = len(a.systemPrompt) / 4
	a.summaryTokens = 50
	a.runningSummary = "summary"
	msgs := a.assembleContext("", "")
	compactUsed := false
	for _, m := range msgs {
		// The compact prompt drops the worked-examples section; the full one
		// has it. Presence/absence of that marker proves which variant ran.
		if m.Role == "system" && !strings.Contains(m.Content, "Worked examples") {
			compactUsed = true
		}
	}
	if !compactUsed {
		t.Errorf("compact system prompt not used under pressure; first system=%q...", msgs[0].Content[:min(80, len(msgs[0].Content))])
	}
}

func TestDebugUsage(t *testing.T) {
	ws := t.TempDir()
	a := &Agent{
		systemPrompt:  buildSystemPrompt(ws),
		compactPrompt: buildCompactSystemPrompt(ws),
		cfg:           Config{Window: 800},
	}
	for i := 0; i < 20; i++ {
		a.history = append(a.history, historyEntry{msg: llm.Message{Role: "user", Content: "x"}, tokens: 60})
	}
	a.sysTokens = len(a.systemPrompt) / 4
	a.summaryTokens = 50
	a.runningSummary = "s"
	t.Logf("usage=%d compact=%d full=%d", a.estTokensLocked(), len(a.compactPrompt), len(a.systemPrompt))
}

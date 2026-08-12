package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/agent"
	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/skills"
	"github.com/Mechres/Yagent/internal/tools"
	"github.com/Mechres/Yagent/internal/undo"
)

// fixedAnswerLLM returns a fixed assistant answer — enough to drive VerifySkill
// (which runs one plain turn when the verification needs no tools).
type fixedAnswerLLM struct{ answer string }

func (f fixedAnswerLLM) ChatStream(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema, onDelta, onReasoning func(string)) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: f.answer}}, nil
}

// TestEvalSkillsVerifyFlow closes the B1 gap: a staged skill write that fails
// verification accumulates failures on the pending write AND the skill; a PASS
// clears them. It mirrors the real `/skills verify` handler, which parses the
// model's verdict after VerifySkill and records it on the store.
func TestEvalSkillsVerifyFlow(t *testing.T) {
	dataDir := t.TempDir()
	ws := t.TempDir()
	sk, err := skills.Open(dataDir, ws)
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(ws, tools.Options{Skills: sk, SkillsWriteApproval: true})

	// an existing skill with a staged patch (the realistic FAIL target)
	content := "---\nname: verify-me\ndescription: verify flow\n---\n## When to Use\nwhen asked\n## Procedure\n1. do it\n## Verification\ncheck output\n"
	if _, err := sk.Apply(skills.Op{Action: skills.ActionCreate, Name: "verify-me", Content: content}); err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("skill_manage")
	if !ok {
		t.Fatal("skill_manage not registered")
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(
		`{"action":"patch","name":"verify-me","old_string":"1. do it","new_string":"1. do it twice"}`))
	if err != nil || !strings.Contains(res, "staged") {
		t.Fatalf("stage: %q / %v", res, err)
	}
	pending, _ := sk.ListPending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	id := pending[0].ID

	// run the verification and record the verdict like the /skills verify
	// handler does
	verify := func(verdict string) {
		answer, err := agent.VerifySkill(context.Background(), fixedAnswerLLM{answer: verdict}, reg, denyWriteApprover{}, content, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got := agent.ParseVerdict(answer); got != agent.ParseVerdict(verdict) {
			t.Fatalf("verdict = %q, want %q", got, agent.ParseVerdict(verdict))
		}
		name, _ := sk.PendingName(id)
		if agent.ParseVerdict(verdict) == "FAIL" {
			_ = sk.RecordFailure(name)
			_ = sk.RecordPendingFailure(id)
		} else {
			_ = sk.ClearFailures(name)
			_ = sk.ClearPendingFailures(id)
		}
	}

	// FAIL -> failure recorded on both the pending write and the skill
	verify("FAIL the verification step could not be run")
	pending, _ = sk.ListPending()
	if len(pending) != 1 || pending[0].Failures != 1 {
		t.Errorf("pending failures = %+v, want 1", pending)
	}
	if metas := sk.List(); len(metas) != 1 || metas[0].Failures != 1 {
		t.Errorf("skill failures = %+v, want 1", metas)
	}

	// PASS -> clears both counters
	verify("PASS verified successfully")
	pending, _ = sk.ListPending()
	if pending[0].Failures != 0 {
		t.Errorf("pending failures not cleared: %+v", pending[0])
	}
	if metas := sk.List(); metas[0].Failures != 0 {
		t.Errorf("skill failures not cleared: %+v", metas[0])
	}
}

// TestEvalUndoRevertsAgentWrite closes the B1 gap: a turn whose fs_write is
// recorded by the undo buffer can be reverted end-to-end (the /undo path).
func TestEvalUndoRevertsAgentWrite(t *testing.T) {
	ws := t.TempDir()
	ub := undo.New()
	reg := tools.NewRegistry(ws, tools.Options{Undo: ub, SkillsWriteApproval: true})

	llmServer, _ := scriptedServer(t, []Step{
		{ToolCall: &ToolCallStep{Name: "fs_write", Args: `{"path":"new.txt","content":"created"}`}},
		{Answer: strPtr("wrote the file")},
	})
	defer llmServer.Close()
	client := llm.NewClient(llmServer.URL, "test-model")

	a := agent.New(client, reg, newTaskApprover(Task{}, ws), agent.Config{MaxIterations: 5}, ws)
	ub.StartTurn()
	answer, err := a.Run(context.Background(), "create new.txt")
	ub.EndTurn()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "wrote the file") {
		t.Fatalf("answer = %q", answer)
	}
	target := filepath.Join(ws, "new.txt")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("fs_write did not create the file: %v", err)
	}
	if !ub.CanUndo() {
		t.Fatal("undo buffer has nothing to undo")
	}
	entries, err := ub.UndoLastTurn()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != target {
		t.Fatalf("undo entries = %+v", entries)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("file still exists after undo")
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func strPtr(s string) *string { return &s }

// denyWriteApprover lets a verification pass only read-only tools (safety:
// a verification must never auto-approve side effects).
type denyWriteApprover struct{}

func (denyWriteApprover) Approve(ctx context.Context, call llm.ToolCall, risk tools.RiskLevel) (agent.Approval, error) {
	return agent.Approval{OK: risk == tools.RiskReadOnly}, nil
}

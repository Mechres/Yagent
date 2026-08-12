package tools

import (
	"context"
	"strings"
	"testing"
)

func TestClarifyTool(t *testing.T) {
	var gotQ, gotChoices string
	reg := NewRegistry(t.TempDir(), Options{AskUser: func(ctx context.Context, q string, choices []string) (string, error) {
		gotQ, gotChoices = q, strings.Join(choices, "|")
		return "option two", nil
	}})
	res := execTool(t, reg, "clarify", map[string]any{
		"question": "which one?", "choices": []string{"a", "b", "c"},
	})
	if !strings.Contains(res, "user answered: option two") {
		t.Errorf("clarify result = %q", res)
	}
	if gotQ != "which one?" || gotChoices != "a|b|c" {
		t.Errorf("ask got %q / %q", gotQ, gotChoices)
	}
	// empty question is a validation error
	if got := execTool(t, reg, "clarify", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("empty question = %q", got)
	}
	// too many choices
	if got := execTool(t, reg, "clarify", map[string]any{"question": "q", "choices": []string{"1", "2", "3", "4", "5", "6", "7"}}); !strings.Contains(got, "validation-error") {
		t.Errorf("too many choices = %q", got)
	}
	// no AskUser wired -> the clarify tool is simply not offered
	reg2 := NewRegistry(t.TempDir(), Options{})
	if _, ok := reg2.Get("clarify"); ok {
		t.Error("clarify should not be registered without an AskUser callback")
	}
	if _, ok := reg2.Get("plan"); ok {
		t.Error("plan should not be registered without an AskUser callback")
	}
}

func TestPlanTool(t *testing.T) {
	approved := NewRegistry(t.TempDir(), Options{AskUser: func(ctx context.Context, q string, choices []string) (string, error) {
		return "Approve plan", nil
	}})
	res := execTool(t, approved, "plan", map[string]any{"steps": []string{"read", "edit", "verify"}})
	if !strings.Contains(res, "plan approved") {
		t.Errorf("approved plan = %q", res)
	}

	revised := NewRegistry(t.TempDir(), Options{AskUser: func(ctx context.Context, q string, choices []string) (string, error) {
		return "Revise — start with the tests", nil
	}})
	res2 := execTool(t, revised, "plan", map[string]any{"steps": []string{"edit first"}})
	if !strings.Contains(res2, "plan rejected") || !strings.Contains(res2, "start with the tests") {
		t.Errorf("revised plan = %q", res2)
	}

	// empty steps is a validation error
	if got := execTool(t, approved, "plan", map[string]any{}); !strings.Contains(got, "validation-error") {
		t.Errorf("empty steps = %q", got)
	}
}

// Package grill contains the small, stateful planning prompt used by the
// /grill-with-docs workflow. The workflow deliberately uses Yagent's existing
// clarify and filesystem tools instead of introducing a second agent loop.
package grill

import (
	"fmt"
	"path"
	"strings"
)

// MaxQuestions is the deterministic clarification budget for one grill run.
const MaxQuestions = 8

// Prompt returns the workflow instructions injected for a single grilling
// session. The topic is user input and is kept separate from the instructions
// so it cannot redefine the workflow rules.
func Prompt(topic string) string {
	return fmt.Sprintf(`GRILL-WITH-DOCS MODE

You are interviewing the user before implementation. Topic: %q

Goal: establish shared understanding of the repository change and leave a
small, durable paper trail. Do not implement source-code changes in this mode.

Rules:
- Read the repository and relevant project instructions first. Do not ask the
  user questions that the codebase can answer.
- Ask one focused question at a time with the clarify tool. Prefer concrete
  choices when there are meaningful alternatives. Do not dump a questionnaire.
- Stop when the scope, vocabulary, constraints, and important trade-offs are
  settled, or when the user says to stop. Keep the interview to at most %d
  clarification questions unless the user explicitly asks for more.
- Update CONTEXT.md at the repository root when a project term is resolved.
  Keep it a glossary: term, precise meaning, and a short usage note. Do not
  put implementation plans in the glossary.
- Write a decision to docs/adr/NNNN-<short-name>.md only when it is hard to
  reverse, surprising without context, and has a real trade-off. Include
  Status, Context, Decision, and Consequences. Do not create ADRs for routine
  choices.
- Use fs_read before editing either artifact and fs_write/fs_edit only for
  CONTEXT.md or docs/adr/*.md. Never edit source, tests, configuration, or
  generated files. These artifact writes still require normal approval.
- Before finishing, re-read every artifact you changed and report its paths.
- End with a concise handoff: settled vocabulary, decisions, unresolved
  questions, and the recommended next command (/plan or /goal).

 This is an interview and documentation pass, not an implementation pass.`, topic, MaxQuestions)
}

// Opening is the user turn that starts the workflow after the prompt is
// injected. Keeping it short leaves the model's attention on the rules above.
func Opening(topic string) string {
	return fmt.Sprintf("Start the grill-with-docs interview for %q. Inspect the repository first, then ask the first highest-value question.", topic)
}

// IsArtifactPath identifies the only files grill mode is allowed to write.
func IsArtifactPath(name string) bool {
	p := path.Clean(strings.TrimSpace(strings.ReplaceAll(name, "\\", "/")))
	return p == "CONTEXT.md" || (strings.HasPrefix(p, "docs/adr/") && strings.HasSuffix(p, ".md"))
}

// ValidateArtifact checks the durable documentation formats written by grill.
// It is deliberately structural rather than a Markdown linter.
func ValidateArtifact(name, content string) error {
	if !IsArtifactPath(name) {
		return nil
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if len(content) > 64<<10 {
		return fmt.Errorf("%s exceeds the 64 KiB documentation limit", name)
	}
	if name == "CONTEXT.md" {
		return validateContext(content)
	}
	return validateADR(content)
}

func validateContext(content string) error {
	terms := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "## ") {
			terms++
		}
	}
	if terms == 0 {
		return fmt.Errorf("CONTEXT.md needs at least one glossary term under a ## heading")
	}
	lower := strings.ToLower(content)
	if strings.Contains(lower, "## procedure") || strings.Contains(lower, "## implementation") {
		return fmt.Errorf("CONTEXT.md is a glossary; move procedures or implementation details to a skill or ADR")
	}
	return nil
}

func validateADR(content string) error {
	lower := strings.ToLower(content)
	for _, heading := range []string{"status", "context", "decision", "consequences"} {
		if !strings.Contains(lower, "## "+heading) {
			return fmt.Errorf("ADR requires a ## %s section", heading)
		}
	}
	return nil
}

// HandoffPrompt asks the next turn to turn the settled interview into an
// approved implementation plan without carrying grill mode into later turns.
func HandoffPrompt() string {
	return `The clarification interview is complete. Summarize the settled vocabulary, decisions, constraints, and unresolved questions from this session. Then call the plan tool with a concise implementation plan. Do not edit files until the user approves the plan.`
}

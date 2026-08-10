package skills

import "regexp"

// Verdict is the result of the dangerous-pattern scanner. It is a guard
// against accidents and prompt-injection attempts, not a security boundary
// (see docs/design/skills.md — same trust model as Hermes's scanner).
type Verdict struct {
	Blocked bool
	Flagged bool
	// Reasons are human-readable verdict lines, e.g. "BLOCK: ...".
	Reasons []string
}

type pattern struct {
	re     *regexp.Regexp
	reason string
}

// blockPatterns cannot be staged or applied.
var blockPatterns = []pattern{
	{regexp.MustCompile(`(?m)\b(?:sudo\s+)?rm\s+-rf\s+(/(?:\s|$)|/\*(?:\s|$)|\$HOME|~(?:/|\s|$))`),
		"destructive recursive delete on a root path (rm -rf /, $HOME, ~)"},
	{regexp.MustCompile(`(?m)\bdd\b[^\n]*\bof=/dev/`), "dd writing to a block device"},
	{regexp.MustCompile(`(?m)\bmkfs\b`), "filesystem formatting (mkfs)"},
	{regexp.MustCompile(`(?m)\bchmod\s+-R\s+777\s+(/(?:\s|$)|/\*|\$HOME|~)`),
		"recursive world-writable permissions on a root path"},
	{regexp.MustCompile(`(?m)\bfind\s+/(?:\s+[^\n]*)?\s+-exec\s+rm`), "find over / deleting files"},
	{regexp.MustCompile(`(?m)\{:\(\)\s*\{\s*:\|:&\s*\};`), "fork bomb"},
}

// flagPatterns are staged normally, but skill_view prepends a warning.
var flagPatterns = []pattern{
	{regexp.MustCompile(`(?i)ignore (all |the |any )?(previous|prior) (instructions|system prompt)`),
		"prompt-injection marker ('ignore previous instructions')"},
	{regexp.MustCompile(`(?i)disregard (the )?(system prompt|previous|prior) instructions`),
		"prompt-injection marker ('disregard the system prompt')"},
	{regexp.MustCompile(`(?i)override (the )?system prompt`), "prompt-injection marker ('override the system prompt')"},
	{regexp.MustCompile(`(?i)say (the |exact )?(words |phrase )?["']?i confirm`),
		"prompt-injection marker ('say \"I confirm\"')"},
	{regexp.MustCompile(`(?i)\byou are now\b`), "role-redefinition marker ('you are now ...')"},
	{regexp.MustCompile(`(?i)\beval\b`), "shell eval (arbitrary code execution)"},
	{regexp.MustCompile(`(?m)\bfind\s+/(?:\s+[^\n]*)?\s+-exec`), "overly broad find -exec"},
}

// Exfiltration is a combination check: a remote-sending tool plus an encoded
// or credential-bearing payload.
var (
	remoteSendRE  = regexp.MustCompile(`(?i)\b(curl|wget|scp|rsync|nc)\b`)
	payloadDecode = regexp.MustCompile(`(?i)\bbase64\s+(-d|--decode)\b`)
	gitConfigRE   = regexp.MustCompile(`\.git/config`)
)

// Scan inspects skill content for dangerous patterns.
func Scan(content string) Verdict {
	var v Verdict
	for _, p := range blockPatterns {
		if p.re.MatchString(content) {
			v.Blocked = true
			v.Reasons = append(v.Reasons, "BLOCK: "+p.reason)
		}
	}
	if remoteSendRE.MatchString(content) &&
		(payloadDecode.MatchString(content) || gitConfigRE.MatchString(content)) {
		v.Blocked = true
		v.Reasons = append(v.Reasons, "BLOCK: apparent exfiltration (remote send + encoded/credential payload)")
	}
	for _, p := range flagPatterns {
		if p.re.MatchString(content) {
			v.Flagged = true
			v.Reasons = append(v.Reasons, "FLAG: "+p.reason)
		}
	}
	return v
}

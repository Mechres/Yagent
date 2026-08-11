// Package scrub redacts likely secrets and home paths from text before it is
// persisted (SQLite messages/summaries/memories) or logged. It is a heuristic
// guard, not a security boundary.
package scrub

import "regexp"

var (
	// key-value secrets: "api_key = abcdef1234567890" / "Authorization: Bearer ..."
	keyValue = regexp.MustCompile(`(?i)(api[_-]?key|secret|passwd|password|token|authorization)\s*[:=]\s*["']?[A-Za-z0-9_\-./+]{8,}`)
	bearer   = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	// whitespace-separated: "token abcdef1234567890" (no '='); value must be
	// 12+ chars to avoid false positives like "the token count is 5"
	sepValue = regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password)\s+[A-Za-z0-9._~+/=-]{12,}`)
	// private-ish local paths.
	homePath = regexp.MustCompile(`(?:/home/[a-zA-Z0-9_.-]+|/Users/[a-zA-Z0-9_.-]+|C:\\Users\\[a-zA-Z0-9_.-]+)`)
)

// Text returns s with likely secrets and home paths replaced by markers.
func Text(s string) string {
	s = keyValue.ReplaceAllString(s, "${1}=[redacted]")
	s = bearer.ReplaceAllString(s, "bearer [redacted]")
	s = sepValue.ReplaceAllString(s, "${1} [redacted]")
	s = homePath.ReplaceAllString(s, "[home]")
	return s
}

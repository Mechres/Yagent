// Package scrub redacts likely secrets and home paths from text before it is
// persisted (SQLite messages/summaries/memories) or logged. It is a heuristic
// guard, not a security boundary.
package scrub

import (
	"regexp"
	"strings"
)

var (
	// key-value secrets: "api_key = abcdef1234567890" / "Authorization: Bearer ..."
	keyValue = regexp.MustCompile(`(?i)(api[_-]?key|secret|passwd|password|token|authorization)\s*[:=]\s*["']?[A-Za-z0-9_\-./+]{8,}`)
	bearer   = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	// whitespace-separated: "token abcdef1234567890" (no '='); value must be
	// 12+ chars to avoid false positives like "the token count is 5"
	sepValue = regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password)\s+[A-Za-z0-9._~+/=-]{12,}`)
	// private-ish local paths.
	homePath = regexp.MustCompile(`(?:/home/[a-zA-Z0-9_.-]+|/Users/[a-zA-Z0-9_.-]+|C:\\Users\\[a-zA-Z0-9_.-]+)`)
	// credential-bearing URLs: scheme://user:pass@host
	credURL = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s:@]+@`)
	// PEM private keys.
	privateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	// secret-looking names, both suffixed (API_TOKEN) and conventional exact
	// names (GH_PAT).
	secretSuffixes = []string{"_TOKEN", "_KEY", "_SECRET", "_PASSWORD", "_PASSWD", "_PASS", "_AUTH", "_CREDENTIAL", "_CREDENTIALS", "_PRIVATE_KEY"}
	secretNames    = []string{
		"GH_PAT", "GITLAB_TOKEN", "MYSQL_PWD", "PGPASSWORD", "PGPASS",
		"DOCKER_AUTH_CONFIG", "KUBE_CONFIG", "AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
	}
)

// Text returns s with likely secrets and home paths replaced by markers.
func Text(s string) string {
	s = keyValue.ReplaceAllString(s, "${1}=[redacted]")
	s = bearer.ReplaceAllString(s, "bearer [redacted]")
	s = sepValue.ReplaceAllString(s, "${1} [redacted]")
	s = homePath.ReplaceAllString(s, "[home]")
	return s
}

// SecretEnv reports whether an environment variable (name and value) should be
// withheld from child processes the agent spawns. Name checks are
// suffix/exact-match based; value checks catch unconventional names that still
// carry SSH keys, bearer tokens or credential-bearing URLs (e.g. a DATABASE_URL
// with embedded credentials).
func SecretEnv(key, value string) bool {
	up := strings.ToUpper(key)
	for _, s := range secretSuffixes {
		if strings.HasSuffix(up, s) {
			return true
		}
	}
	for _, n := range secretNames {
		if up == n {
			return true
		}
	}
	return secretValue(value)
}

// secretValue reports whether a value looks like a secret regardless of the
// variable name.
func secretValue(v string) bool {
	if v == "" {
		return false
	}
	if bearer.MatchString(v) {
		return true
	}
	if credURL.MatchString(v) {
		return true
	}
	if privateKey.MatchString(v) {
		return true
	}
	// "KEY=value" or "token value" style content inside the value itself
	return keyValue.MatchString(v) || sepValue.MatchString(v)
}

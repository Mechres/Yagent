package scrub

import (
	"strings"
	"testing"
)

func TestScrubText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`api_key=abcdef1234567890`, `api_key=[redacted]`},
		{`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload`, `Authorization: bearer [redacted]`},
		{`token = 0xdeadbeefcafe1234`, `token=[redacted]`},
		{`the path is /home/mechres/projects and /Users/bob/x`, `the path is [home]/projects and [home]/x`},
		{`plain text with no secrets`, `plain text with no secrets`},
	}
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// short values must not be redacted (avoid false positives)
	if got := Text("token = ok"); strings.Contains(got, "[redacted]") {
		t.Errorf("short value wrongly redacted: %q", got)
	}
}

func TestSecretEnv(t *testing.T) {
	secret := [][2]string{
		{"API_TOKEN", "abc"},
		{"MY_TOKEN", "abc"},
		{"OPENAI_API_KEY", "sk-abc"},
		{"AWS_SECRET_ACCESS_KEY", "zzz"},
		{"DB_PASSWORD", "hunter2"},
		{"MYSQL_PWD", "root"},
		{"GH_PAT", "ghp_abc"},
		{"DATABASE_URL", "postgres://u:p@host/db"},
		{"APP_CREDENTIALS", "path/to/creds"},
		{"SSH_KEY", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"},
		{"WEIRD_NAME", "https://user:pass@example.com/x"},
		{"WEIRD_NAME", "Authorization: Bearer abcdefghijklmnop"},
	}
	for _, c := range secret {
		if !SecretEnv(c[0], c[1]) {
			t.Errorf("SecretEnv(%q) = false, want true", c[0])
		}
	}
	keep := [][2]string{
		{"PATH", "/usr/bin"},
		{"HOME", "/home/user"},
		{"EDITOR", "vim"},
		{"YAGENT_SERVER_URL", "http://localhost:8089"},
		{"LANG", "en_US.UTF-8"},
		{"GOPATH", "/home/user/go"},
		{"DATABASE_URL", "postgres://db.example.com/app"}, // no embedded creds
	}
	for _, c := range keep {
		if SecretEnv(c[0], c[1]) {
			t.Errorf("SecretEnv(%q) = true, want false", c[0])
		}
	}
}

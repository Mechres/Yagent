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

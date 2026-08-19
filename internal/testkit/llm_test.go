package testkit

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestServerScriptsEventsAndCapturesRequests(t *testing.T) {
	s := NewServer(t, Step{Events: []string{TextEvent("hello")}})
	resp, err := http.Post(s.HTTP.URL, "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "[DONE]") || !strings.Contains(string(body), "hello") {
		t.Fatalf("response = %q", body)
	}
	if got := s.Requests(); len(got) != 1 || string(got[0]) != `{"messages":[]}` {
		t.Fatalf("requests = %q", got)
	}
}

func TestServerCanEmitTruncatedResponse(t *testing.T) {
	s := NewServer(t, Step{Events: []string{TextEvent("partial")}, Truncated: true})
	resp, err := http.Get(s.HTTP.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "[DONE]") {
		t.Fatalf("truncated response unexpectedly ended: %q", body)
	}
}

package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mechres/Yagent/internal/llm"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	sess, err := st.NewSession(ctx, "/tmp/repo")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("empty session id")
	}

	msgs := []Message{
		{Role: "user", Content: "remember that I prefer tabs"},
		{Role: "assistant", Content: "ok", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "fs_read", Arguments: []byte(`{"path":"a"}`)}}}},
		{Role: "tool", Content: "result", ToolCallID: "c1"},
	}
	for _, m := range msgs {
		if _, err := st.Append(ctx, sess.ID, m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := st.History(ctx, sess.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("history len = %d, want 3", len(got))
	}
	if got[0].Content != "remember that I prefer tabs" {
		t.Errorf("msg0 = %q", got[0].Content)
	}
	// tool_calls round-trip
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Function.Name != "fs_read" {
		t.Errorf("msg1 tool_calls = %+v", got[1].ToolCalls)
	}
	if got[2].ToolCallID != "c1" {
		t.Errorf("msg2 tool_call_id = %q", got[2].ToolCallID)
	}

	sessions, err := st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Messages != 3 {
		t.Errorf("sessions = %+v", sessions)
	}
	if sessions[0].Title != "remember that I prefer tabs" {
		t.Errorf("auto title = %q", sessions[0].Title)
	}
}

func TestStoreSummary(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/repo")
	for i := 0; i < 5; i++ {
		if _, err := st.Append(ctx, sess.ID, Message{Role: "user", Content: "msg"}); err != nil {
			t.Fatal(err)
		}
	}

	summary, until, err := st.Summary(ctx, sess.ID)
	if err != nil || summary != "" || until != 0 {
		t.Errorf("empty summary = %q/%d/%v", summary, until, err)
	}

	if err := st.SetSummary(ctx, sess.ID, "decided X", 3); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	summary, until, err = st.Summary(ctx, sess.ID)
	if err != nil || summary != "decided X" || until != 3 {
		t.Errorf("summary = %q/%d/%v", summary, until, err)
	}

	// HistoryAfter skips covered messages (ids ≤ 3); ids 4,5 remain
	after, err := st.HistoryAfter(ctx, sess.ID, 3)
	if err != nil || len(after) != 2 {
		t.Errorf("HistoryAfter = %d msgs, err %v", len(after), err)
	}
}

func TestStoreCleanSlate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "yagent-data")
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/repo")
	if _, err := st.Append(ctx, sess.ID, Message{Role: "user", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// deleting the data dir = complete "forget everything"
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("dir still exists")
	}

	st2, err := Open(dir) // recreates cleanly
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	defer st2.Close()
	sessions, err := st2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions after clean slate: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected empty store, got %+v", sessions)
	}
}

func TestVectorStoreSaveSearch(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()

	dir := t.TempDir()
	vs, err := OpenVectorStore(dir, ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("OpenVectorStore: %v", err)
	}
	defer vs.Close()

	ctx := context.Background()
	if err := vs.Save(ctx, "user prefers tabs over spaces", "tool", "s1", 0.5); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := vs.Save(ctx, "the deploy script lives in scripts/deploy.sh", "tool", "s1", 0.5); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := vs.Search(ctx, "what about tabs?", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d memories, want 1 (only the tabs one is above threshold)", len(got))
	}
	if !strings.Contains(got[0].Text, "tabs") {
		t.Errorf("recalled = %q", got[0].Text)
	}
	if got[0].Source != "tool" || got[0].SessionID != "s1" {
		t.Errorf("metadata = %+v", got[0])
	}
}

func TestVectorStoreCleanSlate(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()

	dir := filepath.Join(t.TempDir(), "vdata")
	vs, err := OpenVectorStore(dir, ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("OpenVectorStore: %v", err)
	}
	if err := vs.Save(context.Background(), "fact one", "tool", "s1", 0.5); err != nil {
		t.Fatal(err)
	}
	vs.Close()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	vs2, err := OpenVectorStore(dir, ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	defer vs2.Close()
	if vs2.Count() != 0 {
		t.Errorf("count = %d, want 0 after clean slate", vs2.Count())
	}
}

func TestVectorSearchEmptyStoreSkipsEmbed(t *testing.T) {
	// agy #1: an empty memory store must skip the embedding request entirely —
	// no /v1/embeddings HTTP call, no SlotLock acquisition, on every turn.
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
	}))
	defer ts.Close()

	vs, err := OpenVectorStore(filepath.Join(t.TempDir(), "mem"), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	defer vs.Close()
	if vs.Count() != 0 {
		t.Fatalf("precondition: store should be empty, count=%d", vs.Count())
	}
	if _, err := vs.Search(context.Background(), "anything", 5); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Errorf("embed server received %d request(s) on an empty store, want 0", requests)
	}
}

// ---------- L3 hybrid retrieval (Mnemosyne-style) ----------

// The fake embedder maps "tab" → (0,1), everything else → (1,0), so cosine is
// a poor signal. Hybrid keyword search must rescue memories that share query
// words but not embedding direction.

func TestHybridRescuesByKeyword(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := OpenVectorStore(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("OpenVectorStore: %v", err)
	}
	defer vs.Close()
	ctx := context.Background()
	// both embed to (0,1); the query "vault secret" embeds to (1,0), so both
	// cosines are 0 and pure-vector search would return nothing.
	if err := vs.Save(ctx, "vault has the tab key", "tool", "s1", 0.5); err != nil {
		t.Fatal(err)
	}
	if err := vs.Save(ctx, "the tab container", "tool", "s1", 0.5); err != nil {
		t.Fatal(err)
	}

	got, err := vs.Search(ctx, "vault secret", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Text, "vault") {
		t.Fatalf("got %+v, want the 'vault' memory rescued by keyword overlap", got)
	}
	// without any keyword overlap nothing surfaces
	if got, err := vs.Search(ctx, "quantum teleportation", 5); err != nil || len(got) != 0 {
		t.Fatalf("no-overlap query = %+v / %v, want empty", got, err)
	}
}

func TestHybridImportanceRanking(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := OpenVectorStore(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("OpenVectorStore: %v", err)
	}
	defer vs.Close()
	ctx := context.Background()
	if err := vs.Save(ctx, "the tab bar config", "tool", "s1", 1.0); err != nil {
		t.Fatal(err)
	}
	if err := vs.Save(ctx, "the tab size cache", "tool", "s1", 0.1); err != nil {
		t.Fatal(err)
	}

	got, err := vs.Search(ctx, "tab", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 || got[0].Text != "the tab bar config" {
		t.Fatalf("got %+v, want the high-importance memory first", got)
	}
}

func TestHybridRecencyRanking(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := OpenVectorStore(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatalf("OpenVectorStore: %v", err)
	}
	defer vs.Close()
	ctx := context.Background()
	if err := vs.Save(ctx, "the tab layout guide", "tool", "s1", 0.5); err != nil {
		t.Fatal(err)
	}
	if err := vs.Save(ctx, "the tab style guide", "tool", "s1", 0.5); err != nil {
		t.Fatal(err)
	}
	// age the second memory by 90 days so recency decides the tie
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	if _, err := vs.db.Exec(`UPDATE memories SET created_at = ? WHERE text = ?`, old, "the tab style guide"); err != nil {
		t.Fatal(err)
	}

	got, err := vs.Search(ctx, "tab", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 || got[0].Text != "the tab layout guide" {
		t.Fatalf("got %+v, want the recent memory first", got)
	}
}

func TestStoreScrubsSecrets(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/repo")

	secret := "my api_key=supersecretvalue123456 lives at /home/alice/projects"
	if _, err := st.Append(ctx, sess.ID, Message{Role: "user", Content: secret}); err != nil {
		t.Fatal(err)
	}
	hist, _ := st.History(ctx, sess.ID)
	if len(hist) != 1 {
		t.Fatalf("history = %d", len(hist))
	}
	if strings.Contains(hist[0].Content, "supersecretvalue123456") || strings.Contains(hist[0].Content, "/home/alice") {
		t.Errorf("secret persisted unredacted: %q", hist[0].Content)
	}
	if !strings.Contains(hist[0].Content, "[redacted]") || !strings.Contains(hist[0].Content, "[home]") {
		t.Errorf("not redacted: %q", hist[0].Content)
	}

	// summaries are scrubbed too
	if err := st.SetSummary(ctx, sess.ID, "user mentioned token abcdefghijklmnopqrst", 1); err != nil {
		t.Fatal(err)
	}
	sum, _, _ := st.Summary(ctx, sess.ID)
	if strings.Contains(sum, "abcdefghijklmnopqrst") {
		t.Errorf("summary leaked a secret: %q", sum)
	}
}

func TestSearchMessagesAndRender(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/repo")
	if _, err := st.Append(ctx, sess.ID, Message{Role: "user", Content: "hello world tabs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, sess.ID, Message{Role: "assistant", Content: "nothing else"}); err != nil {
		t.Fatal(err)
	}

	hits, err := st.SearchMessages(ctx, "tabs", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Snippet, "tabs") {
		t.Errorf("hits = %+v", hits)
	}
	// no match
	if hits, _ := st.SearchMessages(ctx, "zzzqqq", 10); len(hits) != 0 {
		t.Errorf("unexpected hits = %+v", hits)
	}

	md, err := st.RenderMarkdown(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "hello world tabs") || !strings.Contains(md, "nothing else") {
		t.Errorf("markdown = %q", md)
	}
}

func TestRenderHTML(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/repo")
	if _, err := st.Append(ctx, sess.ID, Message{Role: "user", Content: "hello <world> & bye"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, sess.ID, Message{Role: "assistant", Content: "hi there"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, sess.ID, Message{Role: "tool", Content: "result payload"}); err != nil {
		t.Fatal(err)
	}

	page, err := st.RenderHTML(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<html", "<h1>", `class="msg user"`, `class="msg assistant"`, `class="msg tool"`} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	// content is escaped (no raw markup injection)
	if !strings.Contains(page, "hello &lt;world&gt; &amp; bye") {
		t.Errorf("user content not escaped: %q", page)
	}
	if strings.Contains(page, "<world>") {
		t.Error("raw markup leaked into HTML")
	}
}

func TestDeleteSession(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sess, _ := st.NewSession(ctx, "/tmp/repo")
	if _, err := st.Append(ctx, sess.ID, Message{Role: "user", Content: "hello world"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if hits, _ := st.SearchMessages(ctx, "hello", 5); len(hits) != 0 {
		t.Errorf("search still finds deleted messages: %+v", hits)
	}
	if sessions, _ := st.ListSessions(ctx); len(sessions) != 0 {
		t.Errorf("sessions after delete = %+v", sessions)
	}
}

func TestDeleteIfEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// an empty session is deleted
	empty, err := st.NewSession(ctx, "/tmp/repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteIfEmpty(ctx, empty.ID); err != nil {
		t.Fatal(err)
	}
	sessions, _ := st.ListSessions(ctx)
	if len(sessions) != 0 {
		t.Fatalf("empty session not deleted: %+v", sessions)
	}

	// a session with messages is kept
	used, err := st.NewSession(ctx, "/tmp/repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, used.ID, Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteIfEmpty(ctx, used.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.History(ctx, used.ID); err != nil {
		t.Fatalf("session with messages was deleted: %v", err)
	}
}

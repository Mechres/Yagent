package index

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingEmbedServer returns a deterministic /v1/embeddings fake: every text
// gets the same vector, so the vector axis is neutral and FTS/keyword decides.
// It counts embedding requests to prove incremental re-indexing.
func countingEmbedServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var inputs []string
		if err := json.Unmarshal(req.Input, &inputs); err != nil {
			http.Error(w, "expected array", http.StatusBadRequest)
			return
		}
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, 0, len(inputs))
		for i := range inputs {
			data = append(data, item{Object: "embedding", Index: i, Embedding: []float32{1, 0, 0, 0}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(ts.Close)
	return ts, &requests
}

// fixtureRepo lays out a small source tree and returns its path.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.25\n")
	write("pkg/a.go", `package pkg

// validateToolInput checks the tool arguments before dispatch.
func validateToolInput(name string, args string) error {
	if name == "" {
		return errors.New("empty tool name")
	}
	return nil
}
`)
	write("pkg/b.go", `package pkg

// renderPage draws the dashboard.
func renderPage(title string) string {
	return "<h1>" + title + "</h1>"
}
`)
	write(".gitignore", "ignored/\n*.secret\n")
	write("ignored/skip.go", "package ignored\n")
	write("data.secret", "hidden\n")
	return ws
}

func openIndex(t *testing.T, ws, dataDir string, embedURL string) *Store {
	t.Helper()
	s, err := Open(ws, dataDir, embedURL, "test-embed")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIndexEndToEnd(t *testing.T) {
	ts, requests := countingEmbedServer(t)
	ws := fixtureRepo(t)
	s := openIndex(t, ws, t.TempDir(), ts.URL)

	sum, err := s.Index(context.Background())
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if sum.Files != 3 || sum.Chunks == 0 {
		t.Fatalf("summary = %+v, want 3 files indexed with chunks", sum)
	}
	if s.Count() == 0 {
		t.Fatal("no chunks stored")
	}
	firstEmbeds := *requests

	// gitignore: ignored/ and *.secret must not be indexed
	rows, err := s.db.Query(`SELECT path FROM index_files`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		rows.Scan(&p)
		if strings.Contains(p, "ignored/") || strings.Contains(p, ".secret") {
			t.Errorf("ignored file %q was indexed", p)
		}
	}

	// second pass: everything unchanged → no re-embedding
	sum2, err := s.Index(context.Background())
	if err != nil {
		t.Fatalf("Index 2: %v", err)
	}
	if sum2.Files != 0 || sum2.Skipped != 3 {
		t.Errorf("second pass = %+v, want 0 files re-indexed, 3 skipped", sum2)
	}
	if *requests != firstEmbeds {
		t.Errorf("embed requests grew from %d to %d on an unchanged tree", firstEmbeds, *requests)
	}
}

func TestIndexIncrementalRebuild(t *testing.T) {
	ts, requests := countingEmbedServer(t)
	ws := fixtureRepo(t)
	s := openIndex(t, ws, t.TempDir(), ts.URL)

	if _, err := s.Index(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := s.Count()
	embedsAfterFirst := *requests

	// change one file → only it is re-embedded
	path := filepath.Join(ws, "pkg", "a.go")
	orig, _ := os.ReadFile(path)
	content := string(orig) + "\n// added\nfunc extra() {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 1 {
		t.Errorf("re-index = %+v, want exactly 1 file re-indexed", sum)
	}
	if *requests <= embedsAfterFirst {
		t.Error("no new embedding happened for the changed file")
	}
	if s.Count() <= before {
		t.Error("chunk count did not grow with the new function")
	}

	// deleting a file prunes it
	if err := os.Remove(filepath.Join(ws, "pkg", "b.go")); err != nil {
		t.Fatal(err)
	}
	sum3, err := s.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum3.StaleFiles != 1 {
		t.Errorf("prune = %+v, want 1 stale file removed", sum3)
	}
	res, err := s.Search(context.Background(), "renderPage", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Path == "pkg/b.go" || strings.Contains(r.Content, "renderPage") {
			t.Errorf("deleted file still searchable: %+v", res)
		}
	}
}

func TestIndexSearchRelevance(t *testing.T) {
	ts, _ := countingEmbedServer(t)
	ws := fixtureRepo(t)
	s := openIndex(t, ws, t.TempDir(), ts.URL)
	if _, err := s.Index(context.Background()); err != nil {
		t.Fatal(err)
	}

	res, err := s.Search(context.Background(), "tool validation", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("no results for 'tool validation'")
	}
	if res[0].Path != "pkg/a.go" || !strings.Contains(res[0].Content, "validateToolInput") {
		t.Errorf("top result = %s:%d-%d %q, want pkg/a.go validateToolInput", res[0].Path, res[0].StartLine, res[0].EndLine, res[0].Content)
	}

	// keyword-disjoint query falls back to the vector axis (neutral here) and
	// still returns something rather than erroring
	if res, err := s.Search(context.Background(), "banana", 3); err != nil || len(res) == 0 {
		t.Errorf("no-vector-signal query = %+v / %v, want non-empty", res, err)
	}
}

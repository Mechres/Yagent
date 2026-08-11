package index

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkChunkerGo(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("package demo\n\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "// helper function %d\nfunc helper%d(x int) int {\n    _ = x\n    return x + %d\n}\n\n", i, i, i)
	}
	content := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunkSource("big.go", content)
	}
}

func BenchmarkSymbolsGo(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("package demo\n\n")
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "func helper%d(x int) int { return x + %d }\n\ntype T%d struct{}\n\n", i, i, i)
	}
	content := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		symbolsFor("big.go", content)
	}
}

func BenchmarkHybridSearch1000(b *testing.B) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var inputs []string
		_ = json.Unmarshal(req.Input, &inputs)
		type item struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, len(inputs))
		for i := range inputs {
			vec := make([]float32, 8)
			for j := range vec {
				vec[j] = rand.Float32()
			}
			data[i] = item{Object: "embedding", Index: i, Embedding: vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer ts.Close()

	s, err := Open(b.TempDir(), b.TempDir(), ts.URL, "e")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 1000; i++ {
		path := fmt.Sprintf("pkg/file%d.go", i%20)
		content := fmt.Sprintf("package pkg\nfunc fn%d(a int) int {\n    return a + %d\n}\n", i, i)
		vec := make([]float32, 8)
		for j := range vec {
			vec[j] = rand.Float32()
		}
		res, err := s.db.Exec(`INSERT INTO index_chunks (path, start_line, end_line, content, vector) VALUES (?, 1, 4, ?, ?)`,
			path, content, f32ToBytes(vec))
		if err != nil {
			b.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := s.db.Exec(`INSERT INTO index_chunks_fts (rowid, content) VALUES (?, ?)`, id, content); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search(context.Background(), "where is fn", 5); err != nil {
			b.Fatal(err)
		}
	}
}

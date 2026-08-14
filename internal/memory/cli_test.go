package memory

import (
	"context"
	"strings"
	"testing"
)

func TestMemoryListDelete(t *testing.T) {
	ts := newEmbedServer(t)
	defer ts.Close()
	vs, err := OpenVectorStore(t.TempDir(), ts.URL, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	defer vs.Close()
	if err := vs.Save(context.Background(), "user prefers tabs", "tool", "s1", 0.9); err != nil {
		t.Fatal(err)
	}
	if err := vs.Save(context.Background(), "kv is q8_0", "tool", "s2", 0.7); err != nil {
		t.Fatal(err)
	}

	mems, err := vs.List(10)
	if err != nil || len(mems) != 2 {
		t.Fatalf("List: %v len=%d", err, len(mems))
	}
	if mems[0].Text != "kv is q8_0" { // newest first
		t.Errorf("newest-first order broken: %q", mems[0].Text)
	}
	if !strings.HasPrefix(mems[0].ID, "2") {
		t.Errorf("id = %q", mems[0].ID)
	}
	ok, err := vs.Delete(mems[0].ID)
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	mems, _ = vs.List(10)
	if len(mems) != 1 {
		t.Errorf("after delete len=%d, want 1", len(mems))
	}
	// deleting an unknown id reports false
	ok, _ = vs.Delete("99999")
	if ok {
		t.Error("delete of unknown id should return false")
	}
	_ = vs.DeleteAll()
	mems, _ = vs.List(10)
	if len(mems) != 0 {
		t.Errorf("after DeleteAll len=%d, want 0", len(mems))
	}
}

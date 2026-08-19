package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksPreAndPost(t *testing.T) {
	dir := t.TempDir()
	pre := filepath.Join(dir, "pre")
	post := filepath.Join(dir, "post")
	writeFile(t, dir, "a.txt", "x")

	reg := NewRegistry(dir, Options{
		Hooks: []Hook{
			{When: "pre", Tool: "fs_read", Command: []string{"sh", "-c", "echo pre >> " + pre}},
			{When: "post", Tool: "fs_read", Command: []string{"sh", "-c", "echo post >> " + post}},
		},
	})
	res, _ := reg.ExecuteWithHooks(ctx(), "fs_read", argsJSON(t, map[string]any{"path": "a.txt"}))
	if !strings.Contains(res, "x") {
		t.Errorf("tool result = %q", res)
	}
	preData, _ := os.ReadFile(pre)
	if !strings.Contains(string(preData), "pre") {
		t.Errorf("pre-hook not run: %q", preData)
	}
	postData, _ := os.ReadFile(post)
	if !strings.Contains(string(postData), "post") {
		t.Errorf("post-hook not run: %q", postData)
	}
}

func TestHookVetoesPre(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "keep me")
	reg := NewRegistry(dir, Options{
		Hooks: []Hook{{When: "pre", Tool: "fs_write", Command: []string{"sh", "-c", "exit 1"}}},
	})
	res, _ := reg.ExecuteWithHooks(ctx(), "fs_write", argsJSON(t, map[string]any{"path": "a.txt", "content": "replaced"}))
	if !strings.Contains(res, "vetoed") {
		t.Errorf("pre-hook veto not surfaced: %q", res)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "keep me" {
		t.Errorf("file was modified despite pre-hook veto: %q", data)
	}
}

func TestHooksOnlyMatchTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "x")
	reg := NewRegistry(dir, Options{
		Hooks: []Hook{{When: "pre", Tool: "fs_write", Command: []string{"sh", "-c", "echo only-fs-write"}}},
	})
	// a different tool must not run the fs_write-only pre-hook (no veto, no panic)
	res, err := reg.ExecuteWithHooks(ctx(), "fs_read", argsJSON(t, map[string]any{"path": "a.txt"}))
	if err != nil || !strings.Contains(res, "x") {
		t.Errorf("fs_read with fs_write hook: %q, %v", res, err)
	}
}

func TestMonotonicGuardRunsBeforeHooks(t *testing.T) {
	dir := t.TempDir()
	hookMarker := filepath.Join(dir, "hook-ran")
	reg := NewRegistry(dir, Options{
		Guards: []Guard{func(name string, args json.RawMessage) error {
			if name == "fs_write" {
				return fmt.Errorf("policy denies writes")
			}
			return nil
		}},
		Hooks: []Hook{{When: "pre", Tool: "fs_write", Command: []string{"sh", "-c", "echo ran > " + hookMarker}}},
	})
	res, _ := reg.ExecuteWithHooks(ctx(), "fs_write", argsJSON(t, map[string]any{"path": "x", "content": "no"}))
	if !strings.Contains(res, "tool guard denied") {
		t.Fatalf("guard result = %q", res)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("hook ran after guard denial: %v", err)
	}
}

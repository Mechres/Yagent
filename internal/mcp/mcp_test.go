package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMCPStdioServer is a minimal MCP server speaking newline-delimited JSON-RPC
// on stdin/stdout. It implements initialize, tools/list and tools/call.
func fakeMCPStdioServer(t *testing.T) string {
	t.Helper()
	script := `#!/usr/bin/env python3
import json, sys

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if "method" not in msg:
        continue
    method = msg["method"]
    if method == "initialize":
        send({"jsonrpc":"2.0","id":msg["id"],"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fake-stdio"}}})
    elif method == "notifications/initialized":
        pass
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":msg["id"],"result":{"tools":[
            {"name":"echo","description":"echo text back","inputSchema":{"type":"object","properties":{"text":{"type":"string","description":"the text"}},"required":["text"]}}
        ]}})
    elif method == "tools/call":
        name = msg["params"]["name"]
        args = msg["params"].get("arguments", {})
        text = args.get("text", "")
        if name == "echo":
            send({"jsonrpc":"2.0","id":msg["id"],"result":{"content":[{"type":"text","text":"echo: " + text}]}})
        else:
            send({"jsonrpc":"2.0","id":msg["id"],"error":{"code":-32601,"message":"unknown tool"}})
`
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_server.py")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStdioConnectListAndCall(t *testing.T) {
	server := fakeMCPStdioServer(t)
	client, err := Connect(context.Background(), Config{
		Name:    "fake",
		Command: []string{"python3", server},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	tools := client.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := client.Call(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo: hello") {
		t.Errorf("call output = %q", out)
	}
	// an unknown tool surfaces as an error
	if _, err := client.Call(context.Background(), "nope", nil); err == nil {
		t.Error("unknown tool should error")
	}
}

func TestConnectSkipsBadCommand(t *testing.T) {
	// command that doesn't exist -> error
	if _, err := Connect(context.Background(), Config{Name: "bad", Command: []string{"/no/such/binary"}}); err == nil {
		t.Error("expected error for a bad command")
	}
}

func TestHTTPConnectListAndCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "fake-http"},
				},
			})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "sum", "description": "sum two ints",
							"inputSchema": map[string]any{"type": "object",
								"properties": map[string]any{"a": map[string]any{"type": "integer"}, "b": map[string]any{"type": "integer"}},
								"required":   []string{"a", "b"}}},
					},
				},
			})
		case "tools/call":
			params := req.Params
			name, _ := params["name"].(string)
			args, _ := params["arguments"].(map[string]any)
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			if name == "sum" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("sum: %v", a+b)}}},
				})
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	defer ts.Close()

	client, err := Connect(context.Background(), Config{Name: "fakehttp", URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools := client.Tools()
	if len(tools) != 1 || tools[0].Name != "sum" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := client.Call(context.Background(), "sum", map[string]any{"a": 2.0, "b": 3.0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "5") {
		t.Errorf("call output = %q", out)
	}
}

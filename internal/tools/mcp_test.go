package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mechres/Yagent/internal/mcp"
)

// newFakeMCPHTTPServer returns an httptest server speaking JSON-RPC with one
// tool: "search" (args: query string). The endpoint is the same one used by the
// mcp package's own tests, duplicated here to avoid an import cycle concern in
// the tools adapter test.
func newFakeMCPHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "docs"},
				},
			})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "search", "description": "search documentation",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"query": map[string]any{"type": "string", "description": "the query"},
								},
								"required": []string{"query"},
							}},
					},
				},
			})
		case "tools/call":
			params := req.Params
			args, _ := params["arguments"].(map[string]any)
			q, _ := args["query"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "results for " + q}},
				},
			})
		}
	}))
}

func TestMCPToolAdapterSchemaAndExecute(t *testing.T) {
	ts := newFakeMCPHTTPServer(t)
	defer ts.Close()
	client, err := mcp.Connect(context.Background(), mcp.Config{Name: "docs", URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	reg := NewRegistry(t.TempDir(), Options{MCP: []*mcp.Client{client}})
	names := reg.MCPToolNames()
	if len(names) != 1 || names[0] != "docs_search" {
		t.Fatalf("MCPToolNames = %v", names)
	}
	schemas := reg.SchemasFor(names)
	if len(schemas) != 1 || schemas[0].Function.Name != "docs_search" {
		t.Fatalf("schemas = %+v", schemas)
	}
	props, _ := schemas[0].Function.Parameters["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Errorf("properties missing query: %v", schemas[0].Function.Parameters)
	}

	out := execTool(t, reg, "docs_search", map[string]any{"query": "context7"})
	if !strings.Contains(out, "results for context7") {
		t.Errorf("execute output = %q", out)
	}
}

func TestMCPToolNamesForSignal(t *testing.T) {
	// GPT sol #7: MCP schemas are offered selectively — only when the input
	// signals the server or the model already used the tool this turn. A big
	// MCP server must not re-flood every request.
	ts := newFakeMCPHTTPServer(t)
	defer ts.Close()
	client, err := mcp.Connect(context.Background(), mcp.Config{Name: "docs", URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	reg := NewRegistry(t.TempDir(), Options{MCP: []*mcp.Client{client}})

	// no signal -> no MCP tools offered
	if got := reg.MCPToolNamesForSignal("fix the bug", nil); len(got) != 0 {
		t.Errorf("no-signal offered MCP tools: %v", got)
	}
	// input mentions the server name -> its tools offered
	if got := reg.MCPToolNamesForSignal("use docs to search", nil); len(got) != 1 || got[0] != "docs_search" {
		t.Errorf("server-signal offered = %v, want [docs_search]", got)
	}
	// used this turn -> offered regardless of input
	if got := reg.MCPToolNamesForSignal("unrelated", map[string]bool{"docs_search": true}); len(got) != 1 || got[0] != "docs_search" {
		t.Errorf("used-this-turn offered = %v, want [docs_search]", got)
	}
	// all tools still resolvable at dispatch even when not offered
	if _, ok := reg.Get("docs_search"); !ok {
		t.Error("docs_search not in the registry (dispatch would fail)")
	}
}

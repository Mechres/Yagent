package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mechres/Yagent/internal/llm"
	"github.com/Mechres/Yagent/internal/mcp"
)

// mcpTool adapts one tool advertised by an MCP server to the registry's Tool
// interface: its schema is the server's inputSchema converted to our shape,
// and Execute forwards arguments to the server's tools/call.
type mcpTool struct {
	client *mcp.Client
	tool   mcp.Tool
}

func (t *mcpTool) Schema() llm.ToolSchema {
	s := llm.ToolSchema{Type: "function"}
	s.Function.Name = mcpToolName(t.client.Name(), t.tool.Name)
	s.Function.Description = t.tool.Description
	props, required := convertSchema(t.tool.InputSchema)
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	s.Function.Parameters = map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	return s
}

func (t *mcpTool) Risk() RiskLevel { return RiskReadOnly }

func (t *mcpTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", validationErrorf("invalid arguments: %v", err)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	out, err := t.client.Call(ctx, t.tool.Name, args)
	if err != nil {
		return fmt.Sprintf("error: %s [class=mcp_error retryable=false]", err), nil
	}
	return capResult(out, maxResultBytes), nil
}

// convertSchema translates an MCP JSON Schema object into our {properties,
// required} shape. Unsupported/unknown schema keywords are dropped rather than
// forwarded (llama.cpp tool-grammar builds reject them).
func convertSchema(schema map[string]any) (map[string]any, []string) {
	if schema == nil {
		return nil, nil
	}
	props := map[string]any{}
	if rawProps, ok := schema["properties"].(map[string]any); ok {
		for name, p := range rawProps {
			if pm, ok := p.(map[string]any); ok {
				props[name] = sanitizeProp(pm)
			}
		}
	}
	var required []string
	if rawReq, ok := schema["required"].([]any); ok {
		for _, r := range rawReq {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	return props, required
}

// sanitizeProp keeps only the schema keywords our tools field understands.
func sanitizeProp(p map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"type", "description", "enum", "items"} {
		if v, ok := p[k]; ok {
			out[k] = v
		}
	}
	// keep "any" nullable types as plain strings for the grammar builder
	if t, ok := p["type"]; ok {
		switch tv := t.(type) {
		case string:
			if tv == "" {
				out["type"] = "string"
			}
		case []any:
			// union types ("string"|"null"): use the first non-null member
			for _, member := range tv {
				if s, ok := member.(string); ok && s != "null" {
					out["type"] = s
					break
				}
			}
			if _, ok := out["type"]; !ok {
				out["type"] = "string"
			}
		}
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "string"
	}
	return out
}

// normalize server names: spaces and dots become underscores so the tool name
// stays GBNF-safe.
func mcpToolName(server, tool string) string {
	replacer := strings.NewReplacer(" ", "_", "-", "_", ".", "_")
	return replacer.Replace(server) + "_" + replacer.Replace(tool)
}

// Package mcp implements a minimal Model Context Protocol (MCP) client so
// yagent can attach external tool servers (documentation search, git hosts,
// CI dashboards, …) without forking the tool registry. It speaks JSON-RPC 2.0
// over stdio (spawn a local command) or HTTP POST (remote server), performs the
// initialize handshake, lists tools, and forwards calls.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP protocol version this client speaks.
const ProtocolVersion = "2025-06-18"

// Tool is one tool advertised by an MCP server (tools/list).
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema object describing the tool's arguments.
	InputSchema map[string]any
}

// Client is a connection to one MCP server.
type Client struct {
	name    string
	command []string // stdio transport: spawn this process
	url     string   // HTTP transport: POST JSON-RPC here
	headers map[string]string

	mu    sync.Mutex
	cmd   *exec.Cmd      // stdio child process
	stdin io.WriteCloser // stdio request writer
	scan  *bufio.Scanner // stdio response reader
	next  int64
	tools []Tool
}

// Config configures one MCP server.
type Config struct {
	Name    string            `yaml:"name"`
	Command []string          `yaml:"command"` // stdio: ["npx","-y","server"]
	URL     string            `yaml:"url"`     // HTTP: "https://mcp.example.com/mcp"
	Headers map[string]string `yaml:"headers"` // HTTP: extra headers (auth etc.)
	// Enabled gates the server; disabled servers are skipped at startup.
	Enabled bool `yaml:"enabled"`
}

// Connect opens a connection to an MCP server and runs the initialize
// handshake + tools/list. The transport is chosen from the config: a command
// spawns a stdio subprocess; otherwise the URL is used over HTTP POST.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	c := &Client{
		name:    cfg.Name,
		command: cfg.Command,
		url:     cfg.URL,
		headers: cfg.Headers,
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	if err := c.listTools(ctx); err != nil {
		// tolerate a server with no tool support; the client stays usable for
		// resources/prompts, but tools are what we advertise.
		return c, nil
	}
	return c, nil
}

// initialize performs the JSON-RPC initialize request and the initialized
// notification. On the stdio transport it starts the child process first.
func (c *Client) initialize(ctx context.Context) error {
	if len(c.command) > 0 {
		if err := c.startStdio(); err != nil {
			return fmt.Errorf("mcp %s: %w", c.name, err)
		}
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    map[string]any
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if _, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "yagent", "version": "v0.1"},
	}, &result); err != nil {
		return fmt.Errorf("mcp %s: initialize: %w", c.name, err)
	}
	// Notify the server the client is initialized (no id).
	if len(c.command) > 0 {
		c.notify(ctx, "notifications/initialized", map[string]any{})
	}
	return nil
}

// listTools fetches the server's tool list.
func (c *Client) listTools(ctx context.Context) error {
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if _, err := c.request(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return fmt.Errorf("mcp %s: tools/list: %w", c.name, err)
	}
	c.mu.Lock()
	c.tools = nil
	for _, t := range result.Tools {
		if t.InputSchema == nil {
			t.InputSchema = map[string]any{"type": "object"}
		}
		c.tools = append(c.tools, Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	c.mu.Unlock()
	return nil
}

// Tools returns the tools advertised by the server (from the last listTools).
func (c *Client) Tools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// Call invokes a tool and returns the combined text of the result content.
func (c *Client) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if _, err := c.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, &result); err != nil {
		return "", fmt.Errorf("mcp %s: %w", c.name, err)
	}
	var b strings.Builder
	for _, blk := range result.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
			b.WriteString("\n")
		}
	}
	if result.IsError {
		return strings.TrimSpace(b.String()), fmt.Errorf("tool %s returned an error: %s", name, strings.TrimSpace(b.String()))
	}
	return strings.TrimSpace(b.String()), nil
}

// Close tears down the transport (kills a stdio child).
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_, _ = c.cmd.Process.Wait()
		c.cmd = nil
	}
	return nil
}

// startStdio spawns the MCP server as a subprocess speaking newline-delimited
// JSON-RPC on stdin/stdout. The child is deliberately NOT bound to the caller's
// context (which may be a short-lived connect timeout): it lives until Close().
func (c *Client) startStdio() error {
	c.cmd = exec.Command(c.command[0], c.command[1:]...)
	c.cmd.Env = os.Environ()
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	c.cmd.Stderr = os.Stderr
	if err := c.cmd.Start(); err != nil {
		return err
	}
	c.stdin = stdin
	c.scan = bufio.NewScanner(stdout)
	c.scan.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return nil
}

// request sends a JSON-RPC request (method+params) and decodes the result into
// out. It dispatches to stdio or HTTP by transport.
func (c *Client) request(ctx context.Context, method string, params map[string]any, out any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()

	if len(c.command) > 0 {
		return c.stdioRequest(ctx, id, method, params, out)
	}
	return c.httpRequest(ctx, id, method, params, out)
}

// notify sends a JSON-RPC notification (no id). Only meaningful on stdio.
func (c *Client) notify(ctx context.Context, method string, params map[string]any) {
	if len(c.command) == 0 {
		return
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	raw, _ := json.Marshal(msg)
	_, _ = c.stdin.Write(append(raw, '\n'))
}

// stdioRequest writes a request line and reads responses until the matching id.
func (c *Client) stdioRequest(ctx context.Context, id int64, method string, params map[string]any, out any) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	for c.scan.Scan() {
		line := bytes.TrimSpace(c.scan.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // not a JSON-RPC response (e.g. a log line)
		}
		if resp.ID != id {
			continue // stale/other response
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return resp.Result, err
			}
		}
		return resp.Result, nil
	}
	if err := c.scan.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

// httpRequest POSTs the JSON-RPC request to the server URL and decodes the
// single JSON response (non-streaming transport).
func (c *Client) httpRequest(ctx context.Context, id int64, method string, params map[string]any, out any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Some servers stream; read the body (bounded) and find the first JSON
	// object. A single-response server returns the object directly.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", c.name, resp.StatusCode, truncate(string(data), 200))
	}
	var parsed struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// Try the whole body first; fall back to scanning lines for `data:` frames.
	body = data
	if err := json.Unmarshal(body, &parsed); err != nil {
		if frame := firstJSONFrame(data); frame != nil {
			if err := json.Unmarshal(frame, &parsed); err != nil {
				return nil, fmt.Errorf("mcp %s: bad JSON-RPC response: %v", c.name, err)
			}
		} else {
			return nil, fmt.Errorf("mcp %s: bad JSON-RPC response: %v", c.name, err)
		}
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s: %s", c.name, method, parsed.Error.Message)
	}
	if out != nil && len(parsed.Result) > 0 {
		if err := json.Unmarshal(parsed.Result, out); err != nil {
			return parsed.Result, err
		}
	}
	return parsed.Result, nil
}

// firstJSONFrame extracts a JSON object from an SSE `data:` frame if the body
// is not itself JSON.
func firstJSONFrame(data []byte) []byte {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if json.Valid([]byte(payload)) {
				return []byte(payload)
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

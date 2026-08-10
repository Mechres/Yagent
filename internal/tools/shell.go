package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"yagent/internal/llm"
)

// ---------- shell_exec ----------

type shellExecTool struct{ ws string }

type shellExecArgs struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

const (
	shellDefaultTimeout = 30 * time.Second
	shellMaxTimeout     = 300 * time.Second
	shellMaxOutput      = 32 << 10
)

var shellExecSchema = fnSchema("shell_exec", "run a shell command via sh -c; destructive, requires approval; output capped at 32 KiB",
	map[string]any{
		"command":     strProp("shell command to run"),
		"timeout_sec": intProp("timeout in seconds, default 30, max 300 (optional)"),
	},
	[]string{"command"})

func (t *shellExecTool) Schema() llm.ToolSchema { return shellExecSchema }
func (t *shellExecTool) Risk() RiskLevel        { return RiskDestructive }

func (t *shellExecTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a shellExecArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Command == "" {
		return "", validationErrorf(`argument "command" is required`)
	}
	timeout := shellDefaultTimeout
	if a.TimeoutSec > 0 {
		timeout = time.Duration(a.TimeoutSec) * time.Second
		if timeout > shellMaxTimeout {
			return "", validationErrorf("timeout_sec %d exceeds max %d", a.TimeoutSec, int(shellMaxTimeout.Seconds()))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
	cmd.Dir = t.ws
	cmd.Env = scrubEnv(os.Environ())
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("error: command timed out after %s:\n%s%s", timeout, out.String(), errBuf.String()), nil
	}
	var b strings.Builder
	b.WriteString(out.String())
	if errBuf.Len() > 0 {
		fmt.Fprintf(&b, "stderr:\n%s", errBuf.String())
	}
	if err != nil {
		fmt.Fprintf(&b, "exit status: %v", err)
	}
	if b.Len() == 0 {
		return "(no output)", nil
	}
	return capResult(b.String(), shellMaxOutput), nil
}

// scrubEnv drops secret-looking variables (e.g. API_TOKEN, OPENAI_API_KEY,
// AWS_SECRET_ACCESS_KEY) from the child environment.
func scrubEnv(env []string) []string {
	kept := env[:0]
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToUpper(kv[:eq])
		if strings.HasSuffix(key, "_TOKEN") || strings.HasSuffix(key, "_KEY") || strings.HasSuffix(key, "_SECRET") {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

package llm

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// ErrStreamTruncated reports an SSE stream that ended without a "[DONE]" marker
// — the connection dropped mid-response, so the reply is incomplete. Local
// models hit this when a generation runs long, the server OOMs, or the client
// times out. Callers that also saw a terminal finish_reason (stop/length/
// tool_calls) may treat the missing marker as benign.
var ErrStreamTruncated = errors.New("SSE stream ended without [DONE] marker")

// ParseSSE reads an SSE stream from r and calls handle for each data event.
// It treats a data value of "[DONE]" as end-of-stream and returns nil. Reaching
// EOF without "[DONE]" returns ErrStreamTruncated (the reply is incomplete).
func ParseSSE(r io.Reader, handle func(data string) error) error {
	br := bufio.NewReader(r)
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if sb.Len() > 0 {
				data := sb.String()
				if data == "[DONE]" {
					return nil
				}
				if err := handle(data); err != nil {
					return err
				}
				sb.Reset()
			}
			if err == io.EOF {
				return ErrStreamTruncated
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(part)
		}
		if err == io.EOF {
			if sb.Len() > 0 {
				data := sb.String()
				if data == "[DONE]" {
					return nil
				}
				if err := handle(data); err != nil {
					return err
				}
			}
			return ErrStreamTruncated
		}
	}
}

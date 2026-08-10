package llm

import (
	"bufio"
	"io"
	"strings"
)

// ParseSSE reads an SSE stream from r and calls handle for each data event.
// It treats a data value of "[DONE]" as end-of-stream and returns nil.
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
				return nil
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
			return nil
		}
	}
}

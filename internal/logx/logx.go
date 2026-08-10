// Package logx configures the process slog logger: a persistent log file
// under the data dir plus an optional debug mirror to stderr.
package logx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Setup returns the configured logger and installs it as the process default:
// Info+ goes to <dataDir>/yagent.log; with debug, Debug+ is also mirrored to
// stderr (and the file is opened at Debug level too).
func Setup(dataDir string, debug bool) (*slog.Logger, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "yagent.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	level := slog.LevelInfo
	var handlers []slog.Handler
	handlers = append(handlers, slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))
	if debug {
		level = slog.LevelDebug
		handlers = append(handlers, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	logger := slog.New(&multiHandler{handlers: handlers})
	slog.SetDefault(logger)
	return logger, nil
}

// multiHandler fans out to several handlers (file + optional stderr).
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}

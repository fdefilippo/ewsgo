// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
)

// ParseLogLevel implements cumulative thresholds. None suppresses every event.
func ParseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return slog.Level(127), nil
	case "err", "error":
		return slog.LevelError, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	default:
		return 0, errors.New("invalid loglevel: use none, error, warn, info or debug")
	}
}
func NewLogger(out io.Writer, level string) (*slog.Logger, error) {
	threshold, err := ParseLogLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: threshold})), nil
}

// Respect both the configured threshold and an embedding caller's handler.
type thresholdHandler struct {
	slog.Handler
	minimum slog.Level
}

func (h thresholdHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.minimum && h.Handler.Enabled(ctx, level)
}
func (h thresholdHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return thresholdHandler{h.Handler.WithAttrs(attrs), h.minimum}
}
func (h thresholdHandler) WithGroup(name string) slog.Handler {
	return thresholdHandler{h.Handler.WithGroup(name), h.minimum}
}

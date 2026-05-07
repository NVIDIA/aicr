// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ANSI color codes
const (
	colorGreen = "\033[32m"
	colorReset = "\033[0m"
	colorRed   = "\033[31m"
)

// logPrefixEnvVar is the environment variable name for customizing the log prefix.
const logPrefixEnvVar = "AICR_LOG_PREFIX"

// getLogPrefix returns the log prefix from env var or default "cli".
func getLogPrefix() string {
	if prefix := os.Getenv(logPrefixEnvVar); prefix != "" {
		return prefix
	}
	return "cli"
}

// CLIHandler is a custom slog.Handler for CLI output.
// It formats log messages in a user-friendly way:
// - Non-error messages: just the message text
// - Error messages: message text in red
type CLIHandler struct {
	writer io.Writer
	level  slog.Level
	color  bool
	attrs  []slog.Attr
	groups []string
}

// newCLIHandler creates a new CLI handler that writes to the given writer.
// Color output is enabled when the writer is a terminal and NO_COLOR is unset
// (https://no-color.org/).
func newCLIHandler(w io.Writer, level slog.Level) *CLIHandler {
	return &CLIHandler{
		writer: w,
		level:  level,
		color:  shouldUseColor(w),
	}
}

// shouldUseColor returns true when the writer is a terminal and NO_COLOR is
// not set. Honors the de-facto NO_COLOR convention regardless of TTY status.
func shouldUseColor(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// Enabled returns true if the handler handles records at the given level.
func (h *CLIHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle formats and writes the log record with attributes.
func (h *CLIHandler) Handle(_ context.Context, r slog.Record) error {
	msg := "[" + getLogPrefix() + "] " + r.Message

	// Collect attributes: handler-bound attrs first, then record attrs.
	var attrs []string
	groupPrefix := strings.Join(h.groups, ".")
	for _, a := range h.attrs {
		attrs = append(attrs, formatAttr(groupPrefix, a))
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, formatAttr(groupPrefix, a))
		return true
	})
	if len(attrs) > 0 {
		msg = msg + ": " + strings.Join(attrs, " ")
	}

	// Add color for error messages and success messages when supported.
	if h.color {
		if r.Level >= slog.LevelError {
			msg = colorRed + msg + colorReset
		} else {
			msg = colorGreen + msg + colorReset
		}
	}

	if _, err := fmt.Fprintln(h.writer, msg); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write log output", err)
	}
	return nil
}

// formatAttr renders a slog.Attr as "key=value", prefixing key with the
// group path when present.
func formatAttr(groupPrefix string, a slog.Attr) string {
	key := a.Key
	if groupPrefix != "" {
		key = groupPrefix + "." + key
	}
	return fmt.Sprintf("%s=%v", key, a.Value)
}

// WithAttrs returns a new handler with the given attributes appended.
func (h *CLIHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	clone := *h
	clone.attrs = merged
	return &clone
}

// WithGroup returns a new handler that prefixes subsequent attribute keys
// with the given group name (joined with ".").
func (h *CLIHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	clone := *h
	clone.groups = groups
	return &clone
}

func newCLILogger(level string) *slog.Logger {
	lev := ParseLogLevel(level)
	handler := newCLIHandler(os.Stderr, lev)
	return slog.New(handler)
}

// SetDefaultCLILogger initializes the CLI logger with the appropriate log level
// and sets it as the default logger.
// Parameters:
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
func SetDefaultCLILogger(level string) {
	slog.SetDefault(newCLILogger(level))
}

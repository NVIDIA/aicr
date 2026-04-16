// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
}

// newCLIHandler creates a new CLI handler that writes to the given writer.
func newCLIHandler(w io.Writer, level slog.Level) *CLIHandler {
	return &CLIHandler{
		writer: w,
		level:  level,
	}
}

// Enabled returns true if the handler handles records at the given level.
func (h *CLIHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle formats and writes the log record with attributes.
func (h *CLIHandler) Handle(_ context.Context, r slog.Record) error {
	msg := "[" + getLogPrefix() + "] " + r.Message

	// Append attributes as key=value pairs
	if r.NumAttrs() > 0 {
		var attrs []string
		r.Attrs(func(a slog.Attr) bool {
			attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value))
			return true
		})
		if len(attrs) > 0 {
			msg = msg + ": " + strings.Join(attrs, " ")
		}
	}

	// Add color for error messages and success messages
	if r.Level >= slog.LevelError {
		msg = colorRed + msg + colorReset
	} else {
		msg = colorGreen + msg + colorReset
	}

	if _, err := fmt.Fprintln(h.writer, msg); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write log output", err)
	}
	return nil
}

// WithAttrs returns a new handler with the given attributes.
// For CLI handler, we ignore attributes for simplicity.
func (h *CLIHandler) WithAttrs(_ []slog.Attr) slog.Handler {
	return h
}

// WithGroup returns a new handler with the given group.
// For CLI handler, we ignore groups for simplicity.
func (h *CLIHandler) WithGroup(_ string) slog.Handler {
	return h
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

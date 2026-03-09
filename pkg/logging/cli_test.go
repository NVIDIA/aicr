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
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func Test_newCLILogger(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{"debug level", "debug"},
		{"info level", "info"},
		{"warn level", "warn"},
		{"error level", "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := newCLILogger(tt.level)
			if logger == nil {
				t.Fatal("newCLILogger returned nil")
			}

			// Verify logger is usable
			logger.Info("test message")
		})
	}
}

func TestCLIHandler_InfoMessage(t *testing.T) {
	var buf bytes.Buffer
	handler := newCLIHandler(&buf, slog.LevelInfo)
	logger := slog.New(handler)

	logger.Info("test info message")

	output := buf.String()

	// Should contain just the message, no color codes
	if !strings.Contains(output, "test info message") {
		t.Errorf("output should contain message, got: %q", output)
	}

	// Should not contain color codes for info messages
	if strings.Contains(output, colorRed) {
		t.Errorf("info message should not be colored, got: %q", output)
	}
}

func TestCLIHandler_ErrorMessage(t *testing.T) {
	var buf bytes.Buffer
	handler := newCLIHandler(&buf, slog.LevelInfo)
	logger := slog.New(handler)

	logger.Error("test error message")

	output := buf.String()

	// Should contain the message
	if !strings.Contains(output, "test error message") {
		t.Errorf("output should contain message, got: %q", output)
	}

	// Should contain red color code for error messages
	if !strings.Contains(output, colorRed) {
		t.Errorf("error message should be colored red, got: %q", output)
	}

	// Should contain reset code
	if !strings.Contains(output, colorReset) {
		t.Errorf("error message should reset color, got: %q", output)
	}
}

func TestCLIHandler_LevelFiltering(t *testing.T) {
	tests := []struct {
		name         string
		handlerLevel slog.Level
		logLevel     slog.Level
		logFunc      func(*slog.Logger)
		shouldLog    bool
	}{
		{
			name:         "info handler logs info",
			handlerLevel: slog.LevelInfo,
			logLevel:     slog.LevelInfo,
			logFunc:      func(l *slog.Logger) { l.Info("test") },
			shouldLog:    true,
		},
		{
			name:         "info handler filters debug",
			handlerLevel: slog.LevelInfo,
			logLevel:     slog.LevelDebug,
			logFunc:      func(l *slog.Logger) { l.Debug("test") },
			shouldLog:    false,
		},
		{
			name:         "debug handler logs debug",
			handlerLevel: slog.LevelDebug,
			logLevel:     slog.LevelDebug,
			logFunc:      func(l *slog.Logger) { l.Debug("test") },
			shouldLog:    true,
		},
		{
			name:         "error handler logs error",
			handlerLevel: slog.LevelError,
			logLevel:     slog.LevelError,
			logFunc:      func(l *slog.Logger) { l.Error("test") },
			shouldLog:    true,
		},
		{
			name:         "error handler filters info",
			handlerLevel: slog.LevelError,
			logLevel:     slog.LevelInfo,
			logFunc:      func(l *slog.Logger) { l.Info("test") },
			shouldLog:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := newCLIHandler(&buf, tt.handlerLevel)
			logger := slog.New(handler)

			tt.logFunc(logger)

			output := buf.String()
			hasOutput := len(output) > 0

			if hasOutput != tt.shouldLog {
				t.Errorf("shouldLog=%v but hasOutput=%v, output: %q", tt.shouldLog, hasOutput, output)
			}
		})
	}
}

func TestCLIHandler_IncludesAttributes(t *testing.T) {
	var buf bytes.Buffer
	handler := newCLIHandler(&buf, slog.LevelInfo)
	logger := slog.New(handler)

	// Log with attributes
	logger.Info("test message", "key1", "value1", "key2", "value2")

	output := buf.String()

	// Should contain the message
	if !strings.Contains(output, "test message") {
		t.Errorf("output should contain message, got: %q", output)
	}

	// Should contain attributes as key=value pairs
	if !strings.Contains(output, "key1=value1") {
		t.Errorf("output should contain key1=value1, got: %q", output)
	}
	if !strings.Contains(output, "key2=value2") {
		t.Errorf("output should contain key2=value2, got: %q", output)
	}
}

func TestGetLogPrefix(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(logPrefixEnvVar, "")
		if got := getLogPrefix(); got != "cli" {
			t.Errorf("getLogPrefix() = %q, want cli", got)
		}
	})

	t.Run("custom", func(t *testing.T) {
		t.Setenv(logPrefixEnvVar, "test-prefix")
		if got := getLogPrefix(); got != "test-prefix" {
			t.Errorf("getLogPrefix() = %q, want test-prefix", got)
		}
	})
}

func TestCLIHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	handler := newCLIHandler(&buf, slog.LevelInfo)

	// WithAttrs should return the same handler (no-op implementation)
	result := handler.WithAttrs([]slog.Attr{slog.String("key", "value")})
	if result != handler {
		t.Error("WithAttrs should return the same handler")
	}

	// nil attrs
	result = handler.WithAttrs(nil)
	if result != handler {
		t.Error("WithAttrs(nil) should return the same handler")
	}
}

func TestCLIHandler_WithGroup(t *testing.T) {
	var buf bytes.Buffer
	handler := newCLIHandler(&buf, slog.LevelInfo)

	// WithGroup should return the same handler (no-op implementation)
	result := handler.WithGroup("test-group")
	if result != handler {
		t.Error("WithGroup should return the same handler")
	}

	// empty group
	result = handler.WithGroup("")
	if result != handler {
		t.Error("WithGroup(\"\") should return the same handler")
	}
}

func TestSetDefaultCLILogger(t *testing.T) {
	// Save original default logger
	originalLogger := slog.Default()
	defer slog.SetDefault(originalLogger)

	tests := []struct {
		name  string
		level string
	}{
		{"set with debug level", "debug"},
		{"set with info level", "info"},
		{"set with error level", "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetDefaultCLILogger(tt.level)

			// Verify we can use the default logger
			defaultLogger := slog.Default()
			if defaultLogger == nil {
				t.Fatal("Default logger is nil after SetDefaultCLILogger")
			}

			// Verify the logger is usable
			defaultLogger.Info("test message from default CLI logger")
		})
	}
}

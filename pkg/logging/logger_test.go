// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"log/slog"
	"os"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected slog.Level
	}{
		{
			name:     "debug lowercase",
			input:    "debug",
			expected: slog.LevelDebug,
		},
		{
			name:     "debug uppercase",
			input:    "DEBUG",
			expected: slog.LevelDebug,
		},
		{
			name:     "debug with whitespace",
			input:    "  debug  ",
			expected: slog.LevelDebug,
		},
		{
			name:     "info lowercase",
			input:    "info",
			expected: slog.LevelInfo,
		},
		{
			name:     "info uppercase",
			input:    "INFO",
			expected: slog.LevelInfo,
		},
		{
			name:     "warn lowercase",
			input:    "warn",
			expected: slog.LevelWarn,
		},
		{
			name:     "warning lowercase",
			input:    "warning",
			expected: slog.LevelWarn,
		},
		{
			name:     "warn uppercase",
			input:    "WARN",
			expected: slog.LevelWarn,
		},
		{
			name:     "warning uppercase",
			input:    "WARNING",
			expected: slog.LevelWarn,
		},
		{
			name:     "error lowercase",
			input:    "error",
			expected: slog.LevelError,
		},
		{
			name:     "error uppercase",
			input:    "ERROR",
			expected: slog.LevelError,
		},
		{
			name:     "empty string defaults to info",
			input:    "",
			expected: slog.LevelInfo,
		},
		{
			name:     "unknown level defaults to info",
			input:    "unknown",
			expected: slog.LevelInfo,
		},
		{
			name:     "invalid level defaults to info",
			input:    "invalid123",
			expected: slog.LevelInfo,
		},
		{
			name:     "whitespace only defaults to info",
			input:    "   ",
			expected: slog.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseLogLevel(tt.input)
			if result != tt.expected {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func Test_newStructuredLogger(t *testing.T) {
	tests := []struct {
		name    string
		module  string
		version string
		level   string
	}{
		{
			name:    "create logger with debug level",
			module:  "test-module",
			version: "v1.0.0",
			level:   "debug",
		},
		{
			name:    "create logger with info level",
			module:  "another-module",
			version: "v2.5.1",
			level:   "info",
		},
		{
			name:    "create logger with warn level",
			module:  "warn-module",
			version: "v0.1.0",
			level:   "warn",
		},
		{
			name:    "create logger with error level",
			module:  "error-module",
			version: "v3.0.0",
			level:   "error",
		},
		{
			name:    "create logger with empty module and version",
			module:  "",
			version: "",
			level:   "info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := newStructuredLogger(tt.module, tt.version, tt.level)

			if logger == nil {
				t.Fatal("newStructuredLogger returned nil")
			}

			// Verify that the logger is usable by calling a method
			// This ensures the logger was properly initialized
			logger.Info("test message")
		})
	}
}

func TestSetDefaultStructuredLogger(t *testing.T) {
	// Save original default logger to restore after test
	originalLogger := slog.Default()
	defer slog.SetDefault(originalLogger)

	tests := []struct {
		name    string
		module  string
		version string
		envVar  string
	}{
		{
			name:    "set default logger with debug from env",
			module:  "test-module",
			version: "v1.0.0",
			envVar:  "debug",
		},
		{
			name:    "set default logger with info from env",
			module:  "info-module",
			version: "v2.0.0",
			envVar:  "info",
		},
		{
			name:    "set default logger with empty env (defaults to info)",
			module:  "default-module",
			version: "v3.0.0",
			envVar:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up any existing environment variable first
			os.Unsetenv(EnvVarLogLevel)

			// Set environment variable
			if tt.envVar != "" {
				os.Setenv(EnvVarLogLevel, tt.envVar)
			}
			defer os.Unsetenv(EnvVarLogLevel)

			// Set the default logger
			SetDefaultStructuredLogger(tt.module, tt.version)

			// Verify we can use the default logger
			defaultLogger := slog.Default()
			if defaultLogger == nil {
				t.Fatal("Default logger is nil after SetDefaultStructuredLogger")
			}

			// Verify the logger is usable
			defaultLogger.Info("test message from default logger")
		})
	}
}

func TestSetDefaultStructuredLoggerWithLevel(t *testing.T) {
	// Save original default logger to restore after test
	originalLogger := slog.Default()
	defer slog.SetDefault(originalLogger)

	tests := []struct {
		name    string
		module  string
		version string
		level   string
	}{
		{
			name:    "set default logger with explicit debug level",
			module:  "debug-module",
			version: "v1.0.0",
			level:   "debug",
		},
		{
			name:    "set default logger with explicit warn level",
			module:  "warn-module",
			version: "v2.0.0",
			level:   "warn",
		},
		{
			name:    "set default logger with explicit error level",
			module:  "error-module",
			version: "v3.0.0",
			level:   "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the default logger with explicit level
			SetDefaultStructuredLoggerWithLevel(tt.module, tt.version, tt.level)

			// Verify we can use the default logger
			defaultLogger := slog.Default()
			if defaultLogger == nil {
				t.Fatal("Default logger is nil after SetDefaultStructuredLoggerWithLevel")
			}

			// Verify the logger is usable
			defaultLogger.Info("test message from default logger with explicit level")
		})
	}
}

func TestEnvVarLogLevel(t *testing.T) {
	if EnvVarLogLevel != "LOG_LEVEL" {
		t.Errorf("EnvVarLogLevel = %q, want %q", EnvVarLogLevel, "LOG_LEVEL")
	}
}

func Test_newTextLogger(t *testing.T) {
	tests := []struct {
		name    string
		module  string
		version string
		level   string
	}{
		{
			name:    "create text logger with debug level",
			module:  "test-module",
			version: "v1.0.0",
			level:   "debug",
		},
		{
			name:    "create text logger with info level",
			module:  "another-module",
			version: "v2.5.1",
			level:   "info",
		},
		{
			name:    "create text logger with warn level",
			module:  "warn-module",
			version: "v0.1.0",
			level:   "warn",
		},
		{
			name:    "create text logger with error level",
			module:  "error-module",
			version: "v3.0.0",
			level:   "error",
		},
		{
			name:    "create text logger with empty module and version",
			module:  "",
			version: "",
			level:   "info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := newTextLogger(tt.module, tt.version, tt.level)

			if logger == nil {
				t.Fatal("newTextLogger returned nil")
			}

			// Verify that the logger is usable by calling a method
			// This ensures the logger was properly initialized
			logger.Info("test message")
		})
	}
}

func TestSetDefaultLogger(t *testing.T) {
	// Save original default logger to restore after test
	originalLogger := slog.Default()
	defer slog.SetDefault(originalLogger)

	tests := []struct {
		name    string
		module  string
		version string
		envVar  string
	}{
		{
			name:    "set default text logger with debug from env",
			module:  "test-module",
			version: "v1.0.0",
			envVar:  "debug",
		},
		{
			name:    "set default text logger with info from env",
			module:  "info-module",
			version: "v2.0.0",
			envVar:  "info",
		},
		{
			name:    "set default text logger with empty env (defaults to info)",
			module:  "default-module",
			version: "v3.0.0",
			envVar:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up any existing environment variable first
			os.Unsetenv(EnvVarLogLevel)

			// Set environment variable
			if tt.envVar != "" {
				os.Setenv(EnvVarLogLevel, tt.envVar)
			}
			defer os.Unsetenv(EnvVarLogLevel)

			// Set the default logger
			SetDefaultLogger(tt.module, tt.version)

			// Verify we can use the default logger
			defaultLogger := slog.Default()
			if defaultLogger == nil {
				t.Fatal("Default logger is nil after SetDefaultLogger")
			}

			// Verify the logger is usable
			defaultLogger.Info("test message from default text logger")
		})
	}
}

func TestSetDefaultLoggerWithLevel(t *testing.T) {
	// Save original default logger to restore after test
	originalLogger := slog.Default()
	defer slog.SetDefault(originalLogger)

	tests := []struct {
		name    string
		module  string
		version string
		level   string
	}{
		{
			name:    "set default text logger with explicit debug level",
			module:  "debug-module",
			version: "v1.0.0",
			level:   "debug",
		},
		{
			name:    "set default text logger with explicit warn level",
			module:  "warn-module",
			version: "v2.0.0",
			level:   "warn",
		},
		{
			name:    "set default text logger with explicit error level",
			module:  "error-module",
			version: "v3.0.0",
			level:   "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the default logger with explicit level
			SetDefaultLoggerWithLevel(tt.module, tt.version, tt.level)

			// Verify we can use the default logger
			defaultLogger := slog.Default()
			if defaultLogger == nil {
				t.Fatal("Default logger is nil after SetDefaultLoggerWithLevel")
			}

			// Verify the logger is usable
			defaultLogger.Info("test message from default text logger with explicit level")
		})
	}
}

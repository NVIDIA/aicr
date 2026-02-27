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
	"strings"
)

const (
	// EnvVarLogLevel is the environment variable name for setting the log level.
	EnvVarLogLevel = "LOG_LEVEL"
)

func newStructuredLogger(module, version, level string) *slog.Logger {
	lev := ParseLogLevel(level)
	addSource := lev <= slog.LevelDebug

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     lev,
		AddSource: addSource,
	})).With("module", module, "version", version)
}

func newTextLogger(module, version, level string) *slog.Logger {
	lev := ParseLogLevel(level)
	addSource := lev <= slog.LevelDebug

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     lev,
		AddSource: addSource,
	})).With("module", module, "version", version)
}

// SetDefaultStructuredLogger initializes the structured logger with the
// appropriate log level and sets it as the default logger.
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//
// Derives log level from the LOG_LEVEL environment variable.
func SetDefaultStructuredLogger(module, version string) {
	SetDefaultStructuredLoggerWithLevel(module, version, os.Getenv(EnvVarLogLevel))
}

// SetDefaultStructuredLoggerWithLevel initializes the structured logger with the specified log level
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
func SetDefaultStructuredLoggerWithLevel(module, version, level string) {
	slog.SetDefault(newStructuredLogger(module, version, level))
}

// SetDefaultLogger initializes the text logger with the
// appropriate log level and sets it as the default logger.
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//
// Derives log level from the LOG_LEVEL environment variable.
func SetDefaultLogger(module, version string) {
	SetDefaultLoggerWithLevel(module, version, os.Getenv(EnvVarLogLevel))
}

// SetDefaultLoggerWithLevel initializes the text logger with the specified log level
// and sets it as the default logger.
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
func SetDefaultLoggerWithLevel(module, version, level string) {
	slog.SetDefault(newTextLogger(module, version, level))
}

// ParseLogLevel converts a string representation of a log level into a slog.Level.
// Parameters:
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
//
// Returns:
//   - slog.Level corresponding to the input string. Defaults to slog.LevelInfo for unrecognized strings.
func ParseLogLevel(level string) slog.Level {
	var lev slog.Level

	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lev = slog.LevelDebug
	case "warn", "warning":
		lev = slog.LevelWarn
	case "error":
		lev = slog.LevelError
	default:
		lev = slog.LevelInfo
	}

	return lev
}

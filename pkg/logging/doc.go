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

// Package logging provides structured logging utilities for Eidos components.
//
// # Overview
//
// This package wraps the standard library slog package with Eidos-specific defaults
// and conventions for consistent logging across all components. It supports three
// logging modes: structured JSON (machine-readable), text with metadata (debugging),
// and CLI (user-friendly minimal output).
//
// # Features
//
//   - Three logging modes: JSON, Text, CLI
//   - CLI mode: Minimal output with ANSI color support (red for errors)
//   - Structured JSON logging to stderr
//   - Text logging with full metadata for debugging
//   - Environment-based log level configuration (LOG_LEVEL)
//   - Automatic module and version context
//   - Source location tracking for debug logs
//   - Flexible log level parsing
//   - Integration with standard library log package
//
// # Log Levels
//
// Supported log levels (case-insensitive):
//   - DEBUG: Detailed diagnostic information with source location
//   - INFO: General informational messages (default)
//   - WARN/WARNING: Warning messages for potentially problematic situations
//   - ERROR: Error messages for failures requiring attention
//
// # Usage
//
// # Logging Modes
//
// The package provides three logging modes:
//
// 1. **CLI Mode (default for CLI applications)**: Minimal user-friendly output
//
//	logging.SetDefaultCLILogger(slog.LevelInfo)
//	slog.Info("Snapshot captured successfully")  // Output: Snapshot captured successfully
//	slog.Error("Failed to connect")              // Output: Failed to connect (in red)
//
// 2. **Text Mode (--debug flag)**: Human-readable with metadata
//
//	logging.SetDefaultLoggerWithLevel("eidos", "v1.0.0", "debug")
//	// Output: time=2025-01-06T10:30:00.123Z level=INFO module=eidos version=v1.0.0 msg="server started"
//
// 3. **JSON Mode (--log-json flag)**: Machine-readable structured logs
//
//	logging.SetDefaultStructuredLogger("eidos", "v1.0.0")
//	// Output: {"time":"2025-01-06T10:30:00.123Z","level":"INFO","module":"eidos","version":"v1.0.0","msg":"server started"}
//
// Setting the default logger (CLI mode for user-facing tools):
//
//	func main() {
//	    logging.SetDefaultCLILogger(slog.LevelInfo)
//	    slog.Info("application started")
//
//	    // Errors display in red
//	    if err != nil {
//	        slog.Error("operation failed")
//	    }
//	}
//
// Creating a custom structured logger for API servers:
//
//	logger := logging.NewStructuredLogger("api-server", "v2.0.0", "debug")
//	logger.Debug("server starting", "port", 8080)
//
// Setting explicit log level:
//
//	logging.SetDefaultStructuredLoggerWithLevel("cli", "v1.0.0", "warn")
//
// Converting standard library logger:
//
//	stdLogger := logging.NewLogLogger(slog.LevelInfo, false)
//	stdLogger.Println("legacy log message")
//
// # Environment Configuration
//
// The LOG_LEVEL environment variable controls logging verbosity:
//
//	LOG_LEVEL=debug eidos snapshot
//	LOG_LEVEL=error eidosd
//
// If LOG_LEVEL is not set, defaults to INFO level.
//
// # Output Format
//
// Output format depends on the logging mode:
//
// **CLI Mode (default)**: Minimal output, just message text
//
//	Snapshot captured successfully
//	Failed to connect  (in red ANSI color)
//
// **Text Mode (--debug)**: Key=value format with metadata
//
//	time=2025-01-06T10:30:00.123Z level=INFO module=eidos version=v1.0.0 msg="server started" port=8080
//
// **JSON Mode (--log-json)**: Structured JSON to stderr
//
//	{
//	    "time": "2025-01-06T10:30:00.123Z",
//	    "level": "INFO",
//	    "msg": "server started",
//	    "module": "api-server",
//	    "version": "v1.0.0",
//	    "port": 8080
//	}
//
// Debug logs in JSON mode include source location:
//
//	{
//	    "time": "2025-01-06T10:30:00.123Z",
//	    "level": "DEBUG",
//	    "source": {
//	        "function": "main.processRequest",
//	        "file": "server.go",
//	        "line": 45
//	    },
//	    "msg": "processing request",
//	    "module": "api-server",
//	    "version": "v1.0.0"
//	}
//
// # Best Practices
//
// 1. Set default logger early in main() based on application type:
//
//	// CLI applications: Use CLI logger for user-friendly output
//	func main() {
//	    logging.SetDefaultCLILogger(slog.LevelInfo)
//	    slog.Info("application started")
//	    // ...
//	}
//
//	// API servers: Use structured JSON logger
//	func main() {
//	    logging.SetDefaultStructuredLogger("myapp", version)
//	    slog.Info("application started")
//	    // ...
//	}
//
// 2. Include context in log messages:
//
//	slog.Debug("request processed",
//	    "method", "GET",
//	    "path", "/api/v1/resources",
//	    "duration_ms", 125,
//	)
//
// 3. Use appropriate log levels:
//
//	slog.Debug("cache hit", "key", key)  // Development/troubleshooting
//	slog.Debug("server started")          // Normal operations
//	slog.Warn("retry attempt 3")         // Potential issues
//	slog.Error("db connection failed")   // Errors requiring action
//
// 4. Log errors with context:
//
//	slog.Error("failed to process request",
//	    "error", err,
//	    "request_id", requestID,
//	    "retry_count", retries,
//	)
//
// # Integration
//
// This package is used by:
//   - pkg/api - API server logging
//   - pkg/cli - CLI command logging
//   - pkg/collector - Data collection logging
//   - pkg/snapshotter - Snapshot operation logging
//   - pkg/recipe - Recipe generation logging
//
// All components share consistent logging format and configuration.
package logging

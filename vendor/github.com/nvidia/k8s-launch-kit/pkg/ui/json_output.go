// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// LogEntry represents a single message collected during JSON output mode.
type LogEntry struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

// JSONResult is the structured output emitted to stdout in JSON mode.
type JSONResult struct {
	Success        bool              `json:"success"`
	Phase          string            `json:"phase"`
	Profile        map[string]string `json:"profile,omitempty"`
	GeneratedFiles []string          `json:"generatedFiles,omitempty"`
	Deployed       bool              `json:"deployed"`
	DryRun         bool              `json:"dryRun,omitempty"`
	Error          json.RawMessage   `json:"error,omitempty"`
	Messages       []LogEntry        `json:"messages"`
}

// JSONOutput implements Output for structured JSON mode.
// Human-readable messages are written to stderr; a single JSON result
// is written to stdout via Finalize().
type JSONOutput struct {
	stdout   io.Writer
	stderr   io.Writer
	mu       sync.Mutex
	messages []LogEntry
}

// NewJSON creates a JSON output handler.
// stdout receives the final JSON result; stderr receives human-readable messages.
func NewJSON(stdout, stderr io.Writer) *JSONOutput {
	return &JSONOutput{
		stdout:   stdout,
		stderr:   stderr,
		messages: []LogEntry{},
	}
}

func (o *JSONOutput) appendMessage(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	entry := LogEntry{
		Level:     level,
		Message:   msg,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	o.mu.Lock()
	o.messages = append(o.messages, entry)
	o.mu.Unlock()
	// Also write to stderr so humans can follow along
	_, _ = fmt.Fprintf(o.stderr, "[%s] %s\n", level, msg)
}

func (o *JSONOutput) Info(format string, args ...interface{}) {
	o.appendMessage("info", format, args...)
}

func (o *JSONOutput) Success(format string, args ...interface{}) {
	o.appendMessage("success", format, args...)
}

func (o *JSONOutput) Warning(format string, args ...interface{}) {
	o.appendMessage("warning", format, args...)
}

func (o *JSONOutput) Error(format string, args ...interface{}) {
	o.appendMessage("error", format, args...)
}

func (o *JSONOutput) StartProgress(message string) Progress {
	o.appendMessage("info", "%s", message)
	return &noopProgress{output: o}
}

func (o *JSONOutput) Header(_ string) {}

func (o *JSONOutput) Section(text string) {
	o.appendMessage("info", "%s", text)
}

// Confirm always returns true in JSON mode — agents cannot answer prompts.
func (o *JSONOutput) Confirm(_ string) (bool, error) {
	return true, nil
}

// IsTTY is always false in JSON mode — there is no interactive
// terminal to drive a spinner against.
func (o *JSONOutput) IsTTY() bool { return false }

// Messages returns a copy of collected messages.
func (o *JSONOutput) Messages() []LogEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := make([]LogEntry, len(o.messages))
	copy(cp, o.messages)
	return cp
}

// Finalize writes the JSON result to stdout.
func (o *JSONOutput) Finalize(result *JSONResult) error {
	result.Messages = o.Messages()
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON result: %w", err)
	}
	_, err = fmt.Fprintf(o.stdout, "%s\n", data)
	return err
}

// noopProgress implements Progress as a no-op for JSON mode.
type noopProgress struct {
	output *JSONOutput
}

func (p *noopProgress) Update(message string) {
	p.output.appendMessage("info", "%s", message)
}

func (p *noopProgress) Success(message string) {
	p.output.appendMessage("success", "%s", message)
}

func (p *noopProgress) Fail(message string) {
	p.output.appendMessage("error", "%s", message)
}

// NewOutputForFormat creates the appropriate Output implementation based on format.
// "json" returns a JSONOutput; anything else returns the standard text output.
func NewOutputForFormat(format string, autoConfirm bool) (Output, *JSONOutput) {
	if format == "json" {
		jo := NewJSON(os.Stdout, os.Stderr)
		return jo, jo
	}
	if autoConfirm {
		return NewWithOptions(OutputOptions{AutoConfirm: true}), nil
	}
	return New(), nil
}

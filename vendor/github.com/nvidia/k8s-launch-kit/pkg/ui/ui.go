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
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Output represents the user interface for CLI messages
type Output interface {
	// Info displays an informational message
	Info(format string, args ...interface{})
	// Success displays a success message
	Success(format string, args ...interface{})
	// Warning displays a warning message
	Warning(format string, args ...interface{})
	// Error displays an error message
	Error(format string, args ...interface{})
	// StartProgress starts a progress indicator for a long-running operation
	StartProgress(message string) Progress
	// Header displays a header banner
	Header(text string)
	// Section displays a section header
	Section(text string)
	// Confirm displays a yes/no prompt and returns the user's choice.
	// Returns true if the user confirms, false otherwise.
	// In non-interactive contexts (e.g., silent output), returns false.
	Confirm(prompt string) (bool, error)
	// IsTTY reports whether the underlying writer is an interactive
	// terminal. Callers that want to drive a spinner *and* log
	// history use this to skip the spinner-label update in non-TTY
	// mode (where the spinner's Update would otherwise print a
	// duplicate line on top of the caller's own Info() output).
	IsTTY() bool
}

// Progress represents a long-running operation with progress updates
type Progress interface {
	// Update changes the progress message
	Update(message string)
	// Success marks the progress as successful
	Success(message string)
	// Fail marks the progress as failed
	Fail(message string)
}

// StandardOutput implements Output for standard terminal output.
//
// `mu` is shared between regular log writers (Info/Success/Warning/Error/
// Section/Header) and any active spinner goroutine — without that lock
// the spinner's `\r\033[K<spinner> <message>` writes interleave with
// log lines, producing the broken "spinner-and-log-glued-together"
// output we saw on long NicClusterPolicy waits. `activeProgress`
// tracks the currently animating spinner so log writers can clear its
// line before printing; the spinner repaints itself on its next tick.
type StandardOutput struct {
	writer       io.Writer
	isTTY        bool
	colorEnabled bool
	autoConfirm  bool

	mu             sync.Mutex
	activeProgress *standardProgress
}

// OutputOptions configures StandardOutput behavior.
type OutputOptions struct {
	Writer      io.Writer
	AutoConfirm bool
}

// New creates a standard output handler writing to stdout
func New() Output {
	return NewWithWriter(os.Stdout)
}

// NewWithWriter creates a standard output handler with a custom writer
func NewWithWriter(w io.Writer) Output {
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}

	return &StandardOutput{
		writer:       w,
		isTTY:        isTTY,
		colorEnabled: isTTY, // Enable colors only for TTY
	}
}

// NewWithOptions creates a standard output handler with the given options.
func NewWithOptions(opts OutputOptions) Output {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}
	isTTY := false
	if f, ok := w.(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}
	return &StandardOutput{
		writer:       w,
		isTTY:        isTTY,
		colorEnabled: isTTY,
		autoConfirm:  opts.AutoConfirm,
	}
}

// NewSilent creates a silent output handler that discards all output
func NewSilent() Output {
	return NewWithWriter(io.Discard)
}

// writeLine emits a complete log line, coordinating with any active
// spinner so the two don't interleave on the same row. When a spinner
// is running in TTY mode the line is prefixed with `\r\033[K` to erase
// the spinner's current paint; the spinner's next tick repaints itself
// on the new line below.
//
// `prefix` (e.g. "✓ ", "⚠ ", "✗ ") is rendered without color when
// colorEnabled is false; callers wanting color provide an already-ANSI-
// wrapped prefix.
func (o *StandardOutput) writeLine(prefix, format string, args ...interface{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.activeProgress != nil && o.isTTY {
		_, _ = fmt.Fprint(o.writer, "\r\033[K")
	}
	if prefix != "" {
		_, _ = fmt.Fprint(o.writer, prefix)
	}
	_, _ = fmt.Fprintf(o.writer, format+"\n", args...)
}

// Info displays an informational message
func (o *StandardOutput) Info(format string, args ...interface{}) {
	o.writeLine("", format, args...)
}

// Success displays a success message
func (o *StandardOutput) Success(format string, args ...interface{}) {
	prefix := "✓ "
	if o.colorEnabled {
		prefix = "\033[32m✓\033[0m "
	}
	o.writeLine(prefix, format, args...)
}

// Warning displays a warning message
func (o *StandardOutput) Warning(format string, args ...interface{}) {
	prefix := "⚠ "
	if o.colorEnabled {
		prefix = "\033[33m⚠\033[0m "
	}
	o.writeLine(prefix, format, args...)
}

// Error displays an error message
func (o *StandardOutput) Error(format string, args ...interface{}) {
	prefix := "✗ "
	if o.colorEnabled {
		prefix = "\033[31m✗\033[0m "
	}
	o.writeLine(prefix, format, args...)
}

// StartProgress starts a progress indicator. Only one spinner runs at a
// time per output; a new StartProgress preempts the previous one
// silently (the previous Progress's Success/Fail still work, they just
// won't drive the spinner anymore).
func (o *StandardOutput) StartProgress(message string) Progress {
	return newProgress(o, message)
}

// IsTTY returns true when the underlying writer is an interactive
// terminal capable of redrawing the spinner in place.
func (o *StandardOutput) IsTTY() bool {
	return o.isTTY
}

// terminalWidth reports the current terminal width in columns, or 0
// when it can't be determined (e.g. non-TTY writer, or os.File doesn't
// back the writer). The spinner uses this to truncate long progress
// messages before painting, so a wide reason like
// "ready: 11/12; pending: state-OFED" never wraps onto a second line
// and accumulates fragments on screen.
func (o *StandardOutput) terminalWidth() int {
	if !o.isTTY {
		return 0
	}
	f, ok := o.writer.(*os.File)
	if !ok {
		return 0
	}
	w, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	return w
}

// Header displays a header banner
func (o *StandardOutput) Header(text string) {
	width := 60
	if !o.isTTY {
		width = len(text) + 4
	}

	border := strings.Repeat("═", width)
	padding := (width - len(text)) / 2

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.activeProgress != nil && o.isTTY {
		_, _ = fmt.Fprint(o.writer, "\r\033[K")
	}
	_, _ = fmt.Fprintf(o.writer, "\n%s\n", border)
	_, _ = fmt.Fprintf(o.writer, "%s%s\n", strings.Repeat(" ", padding), text)
	_, _ = fmt.Fprintf(o.writer, "%s\n\n", border)
}

// Confirm displays a yes/no prompt and waits for user input.
// Returns true immediately when AutoConfirm is set (--yes flag or JSON mode).
func (o *StandardOutput) Confirm(prompt string) (bool, error) {
	if o.autoConfirm {
		return true, nil
	}
	if !o.isTTY {
		return false, nil
	}
	_, _ = fmt.Fprintf(o.writer, "%s [y/N]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("failed to read user input: %w", err)
	}
	input = strings.TrimSpace(strings.ToLower(input))
	return input == "y" || input == "yes", nil
}

// Section displays a section header
func (o *StandardOutput) Section(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.activeProgress != nil && o.isTTY {
		_, _ = fmt.Fprint(o.writer, "\r\033[K")
	}
	if o.colorEnabled {
		// Bold text
		_, _ = fmt.Fprintf(o.writer, "\n\033[1m%s\033[0m\n", text)
	} else {
		_, _ = fmt.Fprintf(o.writer, "\n%s\n", text)
	}

	// Underline with dashes
	_, _ = fmt.Fprintf(o.writer, "%s\n\n", strings.Repeat("─", len(text)))
}

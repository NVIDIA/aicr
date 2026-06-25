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
	"fmt"
	"sync"
	"time"
)

// spinnerChars are the characters used for the spinner animation
var spinnerChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// standardProgress implements Progress for terminal progress indicators.
//
// The spinner's animation goroutine writes to the same stream the
// owning StandardOutput's Info/Success/Warning/Error/Header/Section
// methods write to. To avoid the two writers interleaving on the same
// row (and producing the "spinner-glued-to-log-line" artifact that
// long NCP waits used to exhibit), both sides serialize through the
// StandardOutput's `mu`. Log writers also clear the spinner line with
// `\r\033[K` before printing — the spinner repaints itself on the
// next tick on a fresh line below.
type standardProgress struct {
	output    *StandardOutput
	stateMu   sync.Mutex
	message   string
	done      chan bool
	spinIndex int
	startTime time.Time
	stopped   bool
}

// newProgress creates a new progress indicator. It also registers
// itself as `output.activeProgress` so concurrent log writers can
// coordinate with the animation.
func newProgress(output *StandardOutput, message string) Progress {
	p := &standardProgress{
		output:    output,
		message:   message,
		done:      make(chan bool),
		startTime: time.Now(),
		stopped:   false,
	}

	output.mu.Lock()
	output.activeProgress = p
	output.mu.Unlock()

	// Start the spinner in a goroutine
	go p.spin()

	return p
}

// spin runs the spinner animation. It takes the shared output mutex
// before every write so log writers can complete cleanly without
// being torn by a mid-print spinner tick.
func (p *standardProgress) spin() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.stateMu.Lock()
			if p.stopped {
				p.stateMu.Unlock()
				return
			}
			msg := p.message
			frame := spinnerChars[p.spinIndex]
			p.spinIndex = (p.spinIndex + 1) % len(spinnerChars)
			elapsed := time.Since(p.startTime)
			p.stateMu.Unlock()

			var timeStr string
			if elapsed > 30*time.Second {
				timeStr = fmt.Sprintf(" (%s)", formatDuration(elapsed))
			}

			if p.output.isTTY {
				p.output.mu.Lock()
				// Only paint if we're still the active progress —
				// guards against StartProgress preempting us mid-tick.
				if p.output.activeProgress == p {
					line := truncateForWidth(fmt.Sprintf("%s %s%s", frame, msg, timeStr), p.output.terminalWidth())
					_, _ = fmt.Fprintf(p.output.writer, "\r\033[K%s", line)
				}
				p.output.mu.Unlock()
			}
		}
	}
}

// Update changes the progress message.
func (p *standardProgress) Update(message string) {
	p.stateMu.Lock()
	stopped := p.stopped
	if !stopped {
		p.message = message
	}
	p.stateMu.Unlock()

	if stopped {
		return
	}

	// For non-TTY, print the update as a discrete line under the
	// shared output mutex so it doesn't interleave with anything.
	if !p.output.isTTY {
		p.output.mu.Lock()
		_, _ = fmt.Fprintf(p.output.writer, "  %s\n", message)
		p.output.mu.Unlock()
	}
}

// Success marks the progress as successful.
func (p *standardProgress) Success(message string) {
	p.stop()

	p.output.mu.Lock()
	defer p.output.mu.Unlock()
	if p.output.isTTY {
		_, _ = fmt.Fprint(p.output.writer, "\r\033[K")
	}
	if p.output.colorEnabled {
		_, _ = fmt.Fprintf(p.output.writer, "\033[32m✓\033[0m %s\n", message)
	} else {
		_, _ = fmt.Fprintf(p.output.writer, "✓ %s\n", message)
	}
}

// Fail marks the progress as failed.
func (p *standardProgress) Fail(message string) {
	p.stop()

	p.output.mu.Lock()
	defer p.output.mu.Unlock()
	if p.output.isTTY {
		_, _ = fmt.Fprint(p.output.writer, "\r\033[K")
	}
	if p.output.colorEnabled {
		_, _ = fmt.Fprintf(p.output.writer, "\033[31m✗\033[0m %s\n", message)
	} else {
		_, _ = fmt.Fprintf(p.output.writer, "✗ %s\n", message)
	}
}

// stop stops the spinner goroutine and unregisters this progress from
// the output. Callers (Success/Fail) are responsible for writing the
// terminal line after this returns.
func (p *standardProgress) stop() {
	p.stateMu.Lock()
	if p.stopped {
		p.stateMu.Unlock()
		return
	}
	p.stopped = true
	close(p.done)
	p.stateMu.Unlock()

	p.output.mu.Lock()
	if p.output.activeProgress == p {
		p.output.activeProgress = nil
	}
	p.output.mu.Unlock()
}

// truncateForWidth truncates s so its rendered width fits in `width`
// columns, appending an ellipsis when truncated. width <= 0 disables
// truncation (returns s as-is). Rune-aware so we don't slice
// mid-multi-byte (e.g. the spinner glyphs are 3-byte UTF-8).
func truncateForWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	s := d / time.Second
	m := s / 60
	s = s % 60

	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

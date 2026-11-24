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

package validators

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"aicr.run/pkg/defaults"
	aicrerrors "aicr.run/pkg/errors"
	"aicr.run/pkg/validator/ctrf"
)

// outputGuard writes text to w in whole lines, with a space inside every
// occurrence of ctrf.ReservedLinePrefix ("##AICR-" becomes "# #AICR-"), so
// output a check relays from another process never reaches the orchestrator as
// a sentinel line. Each write that Write and flush make to w is one or more
// complete lines with no ReservedLinePrefix in it, so no line another writer
// puts between two writes can complete one. A partial line is held until its
// newline, or until it reaches defaults.ValidatorOutputLineLimit, when it is
// written with a newline added. Sentinel lines themselves go through
// writeSentinel. It is safe for concurrent use.
type outputGuard struct {
	mu sync.Mutex
	w  io.Writer
	// held is the partial line written since the last newline.
	held []byte
}

// Write always consumes all of p, so a failing destination never blocks the
// pipe feeding it. Write errors to w are ignored, as callers of fmt.Print
// usually ignore them.
func (g *outputGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = append(g.held, p...)
	if i := bytes.LastIndexByte(g.held, '\n'); i >= 0 {
		g.emit(g.held[:i+1])
		g.held = append(g.held[:0], g.held[i+1:]...)
	}
	if len(g.held) >= defaults.ValidatorOutputLineLimit {
		g.endHeld()
	}
	return len(p), nil
}

// writeSentinel writes line verbatim at the start of a line, ending any
// partial line first so the sentinel is not glued to it.
func (g *outputGuard) writeSentinel(line []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.endHeld()
	return g.w.Write(line)
}

// flush writes any partial line, with a newline added.
func (g *outputGuard) flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.endHeld()
}

// endHeld writes the partial line, if any, with a newline added. The caller
// holds mu.
func (g *outputGuard) endHeld() {
	if len(g.held) == 0 {
		return
	}
	g.emit(append(g.held, '\n'))
	g.held = g.held[:0]
}

// emit writes lines, which end in a newline, as one Write with every
// ctrf.ReservedLinePrefix broken. The caller holds mu.
func (g *outputGuard) emit(lines []byte) {
	_, _ = g.w.Write(bytes.ReplaceAll(lines, []byte(ctrf.ReservedLinePrefix), []byte("# #AICR-")))
}

// sentinelWriter writes each Write as one sentinel line through g.
type sentinelWriter struct{ g *outputGuard }

func (s sentinelWriter) Write(p []byte) (int, error) { return s.g.writeSentinel(p) }

// guardOutput routes os.Stdout and os.Stderr through an outputGuard each and
// sends sentinel lines straight to the original stdout. The returned stop
// closes the pipes, waits up to drainTimeout for everything written to them
// to be copied, then restores both streams and clears extrasOut. The wait is
// bounded because a child process that inherited a pipe keeps it open past
// the check. When it times out, any partial line already read is written
// with a newline added, waiting up to drainTimeout more, since a stalled
// destination can block that write too.
//
// Errors:
//   - ErrCodeInternal when a pipe cannot be created.
func guardOutput(drainTimeout time.Duration) (stop func(), err error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create stdout pipe", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create stderr pipe", err)
	}

	stdout, stderr := os.Stdout, os.Stderr
	outGuard, errGuard := &outputGuard{w: stdout}, &outputGuard{w: stderr}
	drained := make(chan struct{})
	var wg sync.WaitGroup
	relay := func(g *outputGuard, r *os.File) {
		defer wg.Done()
		_, _ = io.Copy(g, r)
		g.flush()
	}
	wg.Add(2)
	go relay(outGuard, outR)
	go relay(errGuard, errR)
	go func() {
		wg.Wait()
		close(drained)
	}()

	os.Stdout, os.Stderr = outW, errW
	extrasOut = sentinelWriter{outGuard}
	return func() {
		_ = outW.Close()
		_ = errW.Close()
		timer := time.NewTimer(drainTimeout)
		defer timer.Stop()
		select {
		case <-drained:
			_ = outR.Close()
			_ = errR.Close()
		case <-timer.C:
			// Logged from a goroutine, since the stalled destination may be
			// the stderr the default slog handler writes to.
			go slog.Warn("check output still open after the check returned; not waiting for it",
				"timeout", drainTimeout)
			var flushes sync.WaitGroup
			for _, g := range []*outputGuard{outGuard, errGuard} {
				flushes.Go(g.flush)
			}
			flushed := make(chan struct{})
			go func() {
				flushes.Wait()
				close(flushed)
			}()
			timer.Reset(drainTimeout)
			select {
			case <-flushed:
			case <-timer.C:
			}
		}
		os.Stdout, os.Stderr = stdout, stderr
		extrasOut = nil
	}, nil
}

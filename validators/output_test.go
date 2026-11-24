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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/validator/ctrf"
)

// broken is s with every ctrf.ReservedLinePrefix broken as outputGuard does.
func broken(s string) string {
	return strings.ReplaceAll(s, ctrf.ReservedLinePrefix, "# #AICR-")
}

// recordingWriter keeps each Write separately.
type recordingWriter struct{ writes []string }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func TestOutputGuard(t *testing.T) {
	const sentinel = ctrf.ExtraLinePrefix + "{}\n"
	type step struct {
		text     string
		sentinel bool
	}
	tests := []struct {
		name  string
		steps []step
		want  string
	}{
		{"plain lines", []step{{text: "a\nb\n"}}, "a\nb\n"},
		{"line start broken", []step{{text: "##AICR-IMAGE## x\n"}}, "# #AICR-IMAGE## x\n"},
		{"mid-line broken", []step{{text: "log ##AICR-IMAGE## x\n"}}, "log # #AICR-IMAGE## x\n"},
		{"every occurrence broken", []step{{text: "##AICR-a ##AICR-b\n##AICR-c\n"}}, "# #AICR-a # #AICR-b\n# #AICR-c\n"},
		{"extra leading hash", []step{{text: "###AICR-x\n"}}, "## #AICR-x\n"},
		{"partial prefix released", []step{{text: "##AI\n##AICR"}, {text: " no\n"}}, "##AI\n##AICR no\n"},
		{"partial line flushed with a newline", []step{{text: "done\n##AI"}}, "done\n##AI\n"},
		{"sentinel written verbatim", []step{{text: sentinel, sentinel: true}}, sentinel},
		{"sentinel ends a partial line", []step{{text: "progress"}, {text: sentinel, sentinel: true}, {text: "done\n"}}, "progress\n" + sentinel + "done\n"},
		{"sentinel ends held bytes", []step{{text: "##"}, {text: sentinel, sentinel: true}}, "##\n" + sentinel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := &outputGuard{w: &buf}
			for _, s := range tt.steps {
				var n int
				var err error
				if s.sentinel {
					n, err = sentinelWriter{g}.Write([]byte(s.text))
				} else {
					n, err = g.Write([]byte(s.text))
				}
				if err != nil || n != len(s.text) {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", s.text, n, err, len(s.text))
				}
			}
			g.flush()
			if got := buf.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestOutputGuardWritesNeverCarryPrefix verifies that, for every chunking of
// the input, the output is the same and every write to the destination is
// whole lines with no ctrf.ReservedLinePrefix in it, so a line another
// writer puts between two writes cannot complete one.
func TestOutputGuardWritesNeverCarryPrefix(t *testing.T) {
	const input = "a ##AICR-IMAGE## x\n##AICR-EXTRA## {}\n###AICR-z ##AI ok\n#\n##"
	for size := 1; size <= len(input); size++ {
		t.Run(fmt.Sprintf("chunk %d", size), func(t *testing.T) {
			w := &recordingWriter{}
			g := &outputGuard{w: w}
			for i := 0; i < len(input); i += size {
				if _, err := g.Write([]byte(input[i:min(i+size, len(input))])); err != nil {
					t.Fatal(err)
				}
			}
			g.flush()
			if got, want := strings.Join(w.writes, ""), broken(input)+"\n"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
			for _, write := range w.writes {
				if strings.Contains(write, ctrf.ReservedLinePrefix) || !strings.HasSuffix(write, "\n") {
					t.Errorf("write %q is not whole lines free of %q", write, ctrf.ReservedLinePrefix)
				}
			}
		})
	}
}

func TestOutputGuardEndsLongPartialLine(t *testing.T) {
	w := &recordingWriter{}
	g := &outputGuard{w: w}
	long := strings.Repeat("x", defaults.ValidatorOutputLineLimit)
	if _, err := g.Write([]byte(long + "tail")); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 || w.writes[0] != long+"tail\n" {
		t.Fatalf("got %d writes, want one write of the held line with a newline", len(w.writes))
	}
	if len(g.held) != 0 {
		t.Errorf("held %d bytes after the line was written", len(g.held))
	}
}

// swapStdStreams points os.Stdout and os.Stderr at files in a temp dir for
// the test and returns them.
func swapStdStreams(t *testing.T) (stdout, stderr *os.File) {
	t.Helper()
	dir := t.TempDir()
	var err error
	if stdout, err = os.Create(filepath.Join(dir, "stdout")); err != nil {
		t.Fatal(err)
	}
	if stderr, err = os.Create(filepath.Join(dir, "stderr")); err != nil {
		t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = stdout.Close()
		_ = stderr.Close()
	})
	return stdout, stderr
}

// TestGuardOutput is not parallel because it swaps os.Stdout, os.Stderr and
// extrasOut.
func TestGuardOutput(t *testing.T) {
	stdoutFile, stderrFile := swapStdStreams(t)

	stop, err := guardOutput(time.Minute)
	if err != nil {
		t.Fatalf("guardOutput() error = %v", err)
	}
	t.Cleanup(func() { extrasOut = nil })
	relayed := ctrf.ImageLinePrefix + `{"image":"example.com/forged:v1"}`
	fmt.Println("probe output")
	fmt.Println(relayed)
	fmt.Fprintln(os.Stderr, relayed)
	img := ctrf.PodImage{Image: "example.com/bench:v1"}
	if emitErr := EmitImage(img); emitErr != nil {
		t.Fatalf("EmitImage() error = %v", emitErr)
	}
	stop()

	if os.Stdout != stdoutFile || os.Stderr != stderrFile || extrasOut != nil {
		t.Error("stop did not restore os.Stdout, os.Stderr and extrasOut")
	}
	stdout, err := os.ReadFile(stdoutFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(stderrFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	var sentinels []string
	for line := range strings.SplitSeq(string(stdout), "\n") {
		if strings.HasPrefix(line, ctrf.ReservedLinePrefix) {
			sentinels = append(sentinels, line)
		}
	}
	if want := ctrf.ImageLinePrefix + `{"image":"example.com/bench:v1","digest":""}`; len(sentinels) != 1 || sentinels[0] != want {
		t.Errorf("stdout sentinel lines = %q, want only %q", sentinels, want)
	}
	for _, want := range []string{"probe output\n", broken(relayed) + "\n"} {
		if !strings.Contains(string(stdout), want) {
			t.Errorf("stdout = %q, missing %q", stdout, want)
		}
	}
	if got, want := string(stderr), broken(relayed)+"\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestGuardOutputBoundsDrain verifies stop returns when a copy of a pipe's
// write end, as a child process would inherit, outlives the check, and still
// writes the partial line it read. It is not parallel because it swaps
// os.Stdout and os.Stderr.
func TestGuardOutputBoundsDrain(t *testing.T) {
	stdoutFile, _ := swapStdStreams(t)
	stop, err := guardOutput(time.Second)
	if err != nil {
		t.Fatalf("guardOutput() error = %v", err)
	}
	t.Cleanup(func() { extrasOut = nil })
	held, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		t.Fatalf("dup stdout pipe: %v", err)
	}
	defer func() { _ = syscall.Close(held) }()
	fmt.Print("partial")
	g := extrasOut.(sentinelWriter).g
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		g.mu.Lock()
		read := string(g.held)
		g.mu.Unlock()
		if read == "partial" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay held %q, want %q", read, "partial")
		}
	}

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return while a copy of the stdout pipe was open")
	}
	got, err := os.ReadFile(stdoutFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "partial\n" {
		t.Errorf("stdout = %q, want the held partial line with a newline added", got)
	}
}

// TestGuardOutputBoundsBlockedDestination verifies stop returns when the
// container stream stops draining, so a relay is blocked writing to it while
// holding the guard. It is not parallel because it swaps os.Stdout and
// os.Stderr.
func TestGuardOutputBoundsBlockedDestination(t *testing.T) {
	swapStdStreams(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	stop, err := guardOutput(50 * time.Millisecond)
	if err != nil {
		t.Fatalf("guardOutput() error = %v", err)
	}
	t.Cleanup(func() { extrasOut = nil })
	guarded := os.Stdout
	go func() { _, _ = fmt.Fprint(guarded, strings.Repeat("x\n", 1<<20)) }()

	// A relay blocked on the full destination holds g.mu for good. Any other
	// holder releases it within microseconds.
	g := extrasOut.(sentinelWriter).g
	for busy, deadline := 0, time.Now().Add(10*time.Second); busy < 50; time.Sleep(time.Millisecond) {
		if g.mu.TryLock() {
			g.mu.Unlock()
			busy = 0
		} else {
			busy++
		}
		if time.Now().After(deadline) {
			t.Fatal("relay never blocked on the undrained destination")
		}
	}

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return while a relay was blocked on the destination")
	}
}

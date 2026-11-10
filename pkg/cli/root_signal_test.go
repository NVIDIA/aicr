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

//go:build unix

package cli

import (
	"bufio"
	"context"
	stderrors "errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestCommandSignalContextNormalRelease(t *testing.T) {
	for range 10 {
		ctx, release := commandSignalContext(t.Context())
		release()
		release()
		if !stderrors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("context not released: %v", ctx.Err())
		}
	}
}

func TestCommandSignalContextSecondSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandSignalHelperProcess$") //nolint:gosec // Re-exec the current test binary only.
			cmd.Env = append(os.Environ(), "AICR_TEST_SIGNAL_HELPER=1")
			cmd.Stderr = os.Stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close() }()
			if startErr := cmd.Start(); startErr != nil {
				t.Fatal(startErr)
			}
			// CommandContext kills the subprocess on the test deadline; every path
			// reaps it, even a failed synchronization or signal delivery.
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			reader := bufio.NewReader(stdout)
			readLine := func(want string) {
				t.Helper()
				got, readErr := reader.ReadString('\n')
				if readErr != nil || got != want+"\n" {
					t.Fatalf("child output=%q err=%v want %q", got, readErr, want)
				}
			}
			readLine("READY")
			if signalErr := cmd.Process.Signal(sig); signalErr != nil {
				t.Fatal(signalErr)
			}
			readLine("CLEANUP")
			if signalErr := cmd.Process.Signal(sig); signalErr != nil {
				t.Fatal(signalErr)
			}
			err = cmd.Wait()
			if ctx.Err() != nil {
				t.Fatalf("second signal did not terminate child: %v", ctx.Err())
			}
			var exitErr *exec.ExitError
			if !stderrors.As(err, &exitErr) {
				t.Fatalf("Wait()=%v want signal exit", err)
			}
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != sig {
				t.Fatalf("child exit=%v want %v", exitErr, sig)
			}
		})
	}
}

func TestCommandSignalHelperProcess(t *testing.T) {
	if os.Getenv("AICR_TEST_SIGNAL_HELPER") != "1" {
		return
	}
	ctx, release := commandSignalContext(context.Background())
	if _, err := os.Stdout.WriteString("READY\n"); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	release() // Wait until NotifyContext has restored the prior disposition.
	if _, err := os.Stdout.WriteString("CLEANUP\n"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin) // Parent holds stdin open through the second signal.
}

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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"aicr.run/pkg/defaults"
	aicrerrors "aicr.run/pkg/errors"
	"aicr.run/pkg/validator/ctrf"
	v1 "aicr.run/pkg/validator/v1"
)

// terminationLogPath is the standard K8s termination message path.
// Declared as a var (not const) only so unit tests can redirect writes
// to a temp file; production code must not reassign it.
var terminationLogPath = "/dev/termination-log"

// exitCodeSkip is the exit code for a skipped check (not applicable).
const exitCodeSkip = 2

// exitCodeTerminated is the exit code for a check interrupted by SIGTERM. It
// is deliberately neither 0, 1 nor 2: the orchestrator maps any other code to
// CTRF status "other" (indeterminate), because the check never reached a
// verdict. It still blocks, like "failed".
const exitCodeTerminated = 3

// exitCodePanicked is the exit code for a check that panicked. It maps to
// CTRF status "other" because the check reached no verdict. The Go runtime's
// own exit code for an unrecovered panic is 2, which reads as a skip, and a
// panic in a goroutine the check starts still exits that way.
const exitCodePanicked = 4

// errSkip is the sentinel error for skipping a check.
var errSkip = errors.New("skip")

// Skip returns a skip sentinel error with the given reason.
// When returned from a CheckFunc, the runner exits with code 2 (skip).
func Skip(reason string) error {
	return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, reason, errSkip)
}

// IsSkip reports whether err (or anything in its Unwrap chain) came from
// Skip. errSkip is unexported, so this is the supported way for callers and
// tests outside this package to classify a skip without matching on
// rendered error text.
func IsSkip(err error) bool {
	return errors.Is(err, errSkip)
}

// CheckFunc is the signature for a v2 validator check function.
// Return nil for pass, non-nil error for fail, Skip() for skip.
// Evidence goes to stdout, debug logs to stderr.
type CheckFunc func(ctx *Context) error

// Run is the main entry point for v2 validator containers.
// It loads the context, dispatches to the named check, and handles
// exit codes and termination log writing. What the check writes to
// os.Stdout and os.Stderr passes through a filter that breaks every
// occurrence of ctrf.ReservedLinePrefix. This package's sentinel writers
// bypass it, and so does the default slog handler, whose text output quotes
// any value that contains a newline. Writes straight to file descriptors 1
// and 2 are not filtered. Once the check returns and the runner stops the
// filter, writes to os.Stdout and os.Stderr are dropped. A process that exits
// before the check returns, such as on a panic in a goroutine the check
// started, loses the output the filter still holds.
//
// Usage in main.go:
//
//	func main() {
//	    validators.Run(map[string]validators.CheckFunc{
//	        "operator-health":    checkOperatorHealth,
//	        "expected-resources": checkExpectedResources,
//	    })
//	}
func Run(checks map[string]CheckFunc) {
	// Debug logs go to stderr
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) < 2 {
		writeTerminationLog("usage: %s <check-name>", os.Args[0])
		os.Exit(1) //nolint:gocritic // Intentional exit before any deferred resources
	}

	checkName := os.Args[1]
	checkFn, ok := checks[checkName]
	if !ok {
		writeTerminationLog("unknown check: %s", checkName)
		os.Exit(1) //nolint:gocritic // Intentional exit before any deferred resources
	}

	slog.Info("starting check", "name", checkName)
	os.Exit(runGuarded(checkFn, LoadContext))
}

// runGuarded runs checkFn with a Context from load, with the check's output
// filtered by guardOutput, and returns the exit code.
func runGuarded(checkFn CheckFunc, load func() (*Context, error)) int {
	stop, err := guardOutput(defaults.ValidatorOutputDrainTimeout)
	if err != nil {
		writeTerminationLog("failed to guard check output: %v", err)
		return 1
	}
	defer stop()
	return runCheck(checkFn, load)
}

// runCheck publishes the result-contract and platform stamps, loads the
// Context with load, runs the check, and returns the exit code. Separated
// from Run so defer works correctly before os.Exit.
//
// The stamps go out before anything else in runCheck can fail or exit, so
// every result carries them unless a stamp cannot be written, which fails
// the check.
func runCheck(checkFn CheckFunc, load func() (*Context, error)) int {
	if err := publishStamps(); err != nil {
		writeTerminationLog("failed to publish result stamps: %v", err)
		return 1
	}
	ctx, err := load()
	if err != nil {
		writeTerminationLog("failed to load context: %v", err)
		return 1
	}
	defer ctx.Cancel()
	return runCheckWithContext(ctx, checkFn)
}

// runCheckWithContext runs checkFn and maps the outcome to an exit code. A
// panic in checkFn returns exitCodePanicked. Otherwise a SIGTERM that
// arrived before checkFn returned always wins over its result. The verdict
// is incomplete, so a nil return (a check that swallowed the cancellation)
// must not be reported as a pass.
func runCheckWithContext(ctx *Context, checkFn CheckFunc) int {
	recovered, stack, err := callCheck(ctx, checkFn)
	ctx.Cancel()
	if recovered != nil {
		slog.Error("check panicked", "panic", fmt.Sprint(recovered), "stack", string(stack))
		writeTerminationLog("check panicked: %v", recovered)
		return exitCodePanicked
	}
	cause := context.Cause(ctx.Ctx)
	if checkCtx, ok := ctx.Ctx.(*checkContext); ok {
		cause = context.Cause(checkCtx.signalCtx)
	}
	if errors.Is(cause, errTerminated) {
		return handleTerminated(err)
	}
	return handleCheckResult(err)
}

// callCheck runs checkFn and returns what it panicked with, and the stack at
// the panic, or its result when it returned.
func callCheck(ctx *Context, checkFn CheckFunc) (recovered any, stack []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			recovered, stack = r, debug.Stack()
		}
	}()
	return nil, nil, checkFn(ctx)
}

// handleTerminated records that the check was interrupted by SIGTERM and
// returns exitCodeTerminated. checkErr is what the check returned after its
// context was canceled; it is appended to the message when present.
func handleTerminated(checkErr error) int {
	const msg = "terminated by SIGTERM before the check completed (Job deadline, eviction, or node drain)"
	if checkErr == nil {
		writeTerminationLog("%s", msg)
	} else {
		writeTerminationLog("%s: %v", msg, checkErr)
	}
	return exitCodeTerminated
}

// publishStamps stamps ExtraKeyResultSchema on the result, with the
// v1.ResultNonceEnv value as ctrf.ExtraKeyResultNonce when it is set, and
// writes the binary's platform as one ctrf.PlatformLinePrefix line. The
// validator Dockerfiles copy the binary built for the image's architecture,
// so the binary's platform is the image's. It unsets v1.ResultNonceEnv, so
// processes the check starts do not inherit the nonce.
//
// Errors:
//   - ErrCodeInternal when v1.ResultNonceEnv cannot be unset or a sentinel
//     line could not be written to stdout.
func publishStamps() error {
	stamp := ResultSchemaExtra()
	if nonce := os.Getenv(v1.ResultNonceEnv); nonce != "" {
		stamp[ctrf.ExtraKeyResultNonce] = nonce
	}
	if err := os.Unsetenv(v1.ResultNonceEnv); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to unset the result nonce", err)
	}
	if err := EmitExtra(stamp); err != nil {
		return err
	}
	return emitSentinel(ctrf.PlatformLinePrefix, runtime.GOOS+"/"+runtime.GOARCH, false, "platform")
}

// handleCheckResult maps a CheckFunc return value to an exit code and
// surfaces the reason (skip or failure) via slog and the termination log.
// Writing the skip reason to the termination log is required for the
// orchestrator's ValidatorResult.TerminationMsg to flow into the CTRF
// report's TestResult.Message; without it skipped checks appear with a
// null message in the signed evidence bundle.
func handleCheckResult(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errSkip) {
		slog.Info("SKIP", "reason", err.Error())
		writeTerminationFile(err.Error())
		return exitCodeSkip
	}
	writeTerminationLog("%v", err)
	return 1
}

// writeTerminationFile writes msg to the termination log path, truncated
// to TerminationLogMaxSize. Does not emit any slog entry for the message
// itself; callers log separately so the slog level can reflect skip vs.
// failure. A failed write is logged at WARN — silent loss here is the
// exact class of bug this code path exists to prevent.
func writeTerminationFile(msg string) {
	if len(msg) > defaults.TerminationLogMaxSize {
		msg = msg[:defaults.TerminationLogMaxSize]
	}
	if err := os.WriteFile(filepath.Clean(terminationLogPath), []byte(msg), 0o600); err != nil { //nolint:gosec // Fixed path, not user-controlled
		slog.Warn("failed to write termination log", "path", terminationLogPath, "error", err)
	}
}

func writeTerminationLog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	slog.Error("FAIL", "message", msg)
	writeTerminationFile(msg)
}

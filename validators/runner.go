// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// terminationLogPath is the standard K8s termination message path.
// Kept as a constant (not configurable) since it matches the Job spec.
const terminationLogPath = "/dev/termination-log"

// exitCodeSkip is the exit code for a skipped check (not applicable).
const exitCodeSkip = 2

// errSkip is the sentinel error for skipping a check.
var errSkip = errors.New("skip")

// Skip returns a skip sentinel error with the given reason.
// When returned from a CheckFunc, the runner exits with code 2 (skip).
func Skip(reason string) error {
	return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, reason, errSkip)
}

// CheckFunc is the signature for a v2 validator check function.
// Return nil for pass, non-nil error for fail, Skip() for skip.
// Evidence goes to stdout, debug logs to stderr.
type CheckFunc func(ctx *Context) error

// Run is the main entry point for v2 validator containers.
// It loads the context, dispatches to the named check, and handles
// exit codes and termination log writing.
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

	exitCode := runCheck(checkFn)
	os.Exit(exitCode)
}

// runCheck loads context, runs the check, and returns the exit code.
// Separated from Run so defer works correctly before os.Exit.
func runCheck(checkFn CheckFunc) int {
	ctx, err := LoadContext()
	if err != nil {
		writeTerminationLog("failed to load context: %v", err)
		return 1
	}
	defer ctx.Cancel()

	if err := checkFn(ctx); err != nil {
		if errors.Is(err, errSkip) {
			slog.Info("SKIP", "reason", err.Error())
			return exitCodeSkip
		}
		writeTerminationLog("%v", err)
		return 1
	}

	return 0
}

func writeTerminationLog(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	slog.Error("FAIL", "message", msg)
	if len(msg) > defaults.TerminationLogMaxSize {
		msg = msg[:defaults.TerminationLogMaxSize]
	}
	_ = os.WriteFile(filepath.Clean(terminationLogPath), []byte(msg), 0o600) //nolint:gosec // Fixed path, not user-controlled
}

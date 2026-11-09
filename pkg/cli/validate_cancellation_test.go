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

package cli

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestFinishValidationPreservesPartialReport(t *testing.T) {
	for _, tt := range []struct {
		name           string
		outputFails    bool
		serializeFails bool
		rbacFails      bool
	}{
		{"canceled report written", false, false, false},
		{"canceled output fails", true, false, false},
		{"canceled serialization fails", false, true, false},
		{"RBAC failure report written", false, false, true},
		{"RBAC failure output fails", true, false, true},
		{"RBAC failure serialization fails", false, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, rec := phaseLogRecipe(t)
			builder := ctrf.NewBuilder("aicr", "test", "deployment")
			builder.AddResult(&ctrf.ValidatorResult{Name: "completed", Phase: "deployment", ExitCode: 0})
			builder.AddResult(&ctrf.ValidatorResult{Name: "interrupted", Phase: "deployment", ExitCode: -1, TerminationMsg: "canceled: validation run was interrupted"})
			builder.AddSkipped("not-started", "deployment", "not run: validation canceled")
			results := []*aicr.PhaseResult{{Phase: aicr.PhaseDeployment, Status: ctrf.StatusOther, Report: builder.Build()}}
			dir := t.TempDir()
			output := filepath.Join(dir, "partial.json")
			if tt.outputFails {
				output = dir
			}
			if tt.serializeFails {
				output = "cm://fake/partial"
			}
			evidenceDir := filepath.Join(dir, "must-not-exist")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cancellation := observeValidationCancellation(ctx, time.Now)
			defer cancellation.release()
			var cause error = errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "validation interrupted")
			code, exit := errors.ErrCodeCanceled, errors.ExitCanceled
			revokeErr := stderrors.New("privileged revocation failed")
			if tt.rbacFails {
				cause = errors.Wrap(errors.ErrCodeInternal, "cluster-admin binding may be orphaned", stderrors.Join(cause, revokeErr))
				code, exit = errors.ErrCodeInternal, errors.ExitInternal
			}
			err := finishValidation(ctx, cancellation, client, rec, phaseLogSnapshot(), validationConfig{output: output, outFormat: serializer.FormatJSON, kubeconfig: filepath.Join(dir, "missing-kubeconfig"), cleanup: true, failOnError: false, evidenceDir: evidenceDir, evidence: &recipeEvidenceConfig{OutDir: evidenceDir, Push: "://must-not-push"}}, results, cause)
			errorstest.WantReportedCode(t, err, code)
			if !stderrors.Is(err, context.Canceled) || (tt.rbacFails && !stderrors.Is(err, revokeErr)) {
				t.Errorf("lost validation cause: %v", err)
			}
			if errors.ExitCodeFromError(err) != exit {
				t.Errorf("exit code=%d", errors.ExitCodeFromError(err))
			}
			if !tt.outputFails && !tt.serializeFails {
				data, readErr := os.ReadFile(filepath.Clean(output))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var report ctrf.Report
				if err := json.Unmarshal(data, &report); err != nil {
					t.Fatal(err)
				}
				if got := report.Results.Summary; got.Tests != 3 || got.Passed != 1 || got.Other != 1 || got.Skipped != 1 {
					t.Errorf("summary=%+v", got)
				}
			}
			if _, statErr := os.Stat(evidenceDir); !os.IsNotExist(statErr) {
				t.Errorf("canceled run rendered evidence: %v", statErr)
			}
		})
	}
}

func TestFinishValidationWithoutResults(t *testing.T) {
	err := finishValidation(t.Context(), nil, nil, nil, nil, validationConfig{}, nil, errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "canceled"))
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

// TestValidationCancellationBudgetCoversPodGrace guards that the CLI's
// aggregate deadline covers every interrupted-run window: the shared phase
// cleanup budget, the termination wait that keeps per-run RBAC alive through
// the validator pod's SIGTERM grace, and the separate RBAC/data teardown.
func TestValidationCancellationBudgetCoversPodGrace(t *testing.T) {
	want := 2*defaults.K8sCleanupTimeout + defaults.ValidatorTerminationGracePeriod + defaults.ValidatorTerminationWaitMargin
	if validationCancellationBudget != want {
		t.Errorf("validationCancellationBudget=%v want %v", validationCancellationBudget, want)
	}
}

func TestValidationCancellationOutputDeadline(t *testing.T) {
	for _, tt := range []struct {
		name    string
		elapsed time.Duration
		expired bool
	}{
		{"remaining shared deadline", validationCancellationBudget - 15*time.Second, false},
		{"already expired", validationCancellationBudget + time.Second, true},
		{"smaller output cap", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			observed := time.Now().Add(-tt.elapsed)
			cancellation := observeValidationCancellation(ctx, func() time.Time { return observed })
			defer cancellation.release()
			cancel()
			var first time.Time
			for range 2 {
				outputCtx, release := cancellation.outputContext(ctx)
				defer release()
				deadline, ok := outputCtx.Deadline()
				if !ok || deadline.After(time.Now().Add(defaults.K8sCleanupTimeout)) {
					t.Fatalf("output exceeded I/O cap: %v", deadline)
				}
				if tt.elapsed != 0 {
					want := observed.Add(validationCancellationBudget)
					if !deadline.Equal(want) || (!first.IsZero() && !deadline.Equal(first)) {
						t.Errorf("output renewed aggregate deadline: got %v want %v", deadline, want)
					}
				}
				if (outputCtx.Err() != nil) != tt.expired {
					t.Errorf("output context error=%v expired=%v", outputCtx.Err(), tt.expired)
				}
				first = deadline
			}
		})
	}
}

func TestValidationCancellationNormalRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	cancellation := observeValidationCancellation(ctx, func() time.Time {
		calls.Add(1)
		return time.Now()
	})
	if calls.Load() != 0 {
		t.Fatal("budget started before cancellation")
	}
	cancellation.release()
	cancellation.release()
	cancel()
	<-cancellation.done
	if calls.Load() != 0 || !cancellation.deadline.IsZero() {
		t.Fatal("normal release left a cancellation callback alive")
	}
}

func TestValidationCancellationJoinsStartingCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, proceed := make(chan struct{}), make(chan struct{})
	observed := time.Now().Add(15*time.Second - validationCancellationBudget)
	cancellation := observeValidationCancellation(ctx, func() time.Time {
		close(entered)
		<-proceed
		return observed
	})
	cancel()
	<-entered
	output := make(chan context.Context, 1)
	released := make(chan struct{})
	go func() {
		outputCtx, release := cancellation.outputContext(ctx)
		defer release()
		output <- outputCtx
	}()
	go func() {
		cancellation.release()
		close(released)
	}()
	close(proceed)
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer waitCancel()
	select {
	case outputCtx := <-output:
		deadline, ok := outputCtx.Deadline()
		if !ok || !deadline.Equal(observed.Add(validationCancellationBudget)) {
			t.Fatalf("callback startup renewed/lost deadline: %v", deadline)
		}
	case <-waitCtx.Done():
		t.Fatal("output did not join the cancellation callback")
	}
	select {
	case <-released:
	case <-waitCtx.Done():
		t.Fatal("release did not join the cancellation callback")
	}
	cancellation.release()
}

func TestFinishValidationExpiredOutputBudget(t *testing.T) {
	client, rec := phaseLogRecipe(t)
	builder := ctrf.NewBuilder("aicr", "test", "deployment")
	builder.AddSkipped("not-started", "deployment", "not run: validation canceled")
	results := []*aicr.PhaseResult{{Phase: aicr.PhaseDeployment, Status: ctrf.StatusOther, Report: builder.Build()}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancellation := observeValidationCancellation(ctx, func() time.Time {
		return time.Now().Add(-validationCancellationBudget - time.Second)
	})
	defer cancellation.release()
	cancel()
	output := filepath.Join(t.TempDir(), "must-not-be-created.json")
	cause := errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "validation interrupted")
	err := finishValidation(ctx, cancellation, client, rec, nil, validationConfig{output: output, outFormat: serializer.FormatJSON}, results, cause)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Errorf("expired budget attempted file creation: %v", statErr)
	}
	if len(results) != 1 || results[0].Report.Results.Summary.Skipped != 1 {
		t.Fatal("in-memory partial report changed")
	}
}

// TestFinishValidationWritesAfterWriterCreation guards that the partial-output
// budget is checked only before the writer exists. Creating a file output
// truncates it, so a check after creation would abandon an empty file.
func TestFinishValidationWritesAfterWriterCreation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "partial.json")
		if err := os.WriteFile(output, []byte("previous report"), 0o600); err != nil {
			t.Fatal(err)
		}
		builder := ctrf.NewBuilder("aicr", "test", "deployment")
		builder.AddSkipped("not-started", "deployment", "not run: validation canceled")
		results := []*aicr.PhaseResult{{Phase: aicr.PhaseDeployment, Status: ctrf.StatusOther, Report: builder.Build()}}
		const remaining = time.Second
		ctx, cancel := context.WithCancel(t.Context())
		cancellation := observeValidationCancellation(ctx, func() time.Time {
			return time.Now().Add(remaining - validationCancellationBudget)
		})
		defer cancellation.release()
		cancel()
		create := newOutputWriter
		t.Cleanup(func() { newOutputWriter = create })
		newOutputWriter = func(format serializer.Format, path, kubeconfig string) (serializer.Serializer, error) {
			ser, err := create(format, path, kubeconfig)
			time.Sleep(2 * remaining) // The output budget expires after truncation.
			return ser, err
		}
		cause := errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "validation interrupted")
		err := finishValidation(ctx, cancellation, nil, nil, nil, validationConfig{output: output, outFormat: serializer.FormatJSON}, results, cause)
		errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
		data, readErr := os.ReadFile(filepath.Clean(output))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var report ctrf.Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatalf("truncated output abandoned (%q): %v", data, err)
		}
		if report.Results.Summary.Skipped != 1 {
			t.Errorf("summary=%+v", report.Results.Summary)
		}
	})
}

// TestFinishValidationPartialOutputOnlyOnInterruption guards that partial CTRF
// output is written only for operator interruptions (including one whose
// privileged revocation also failed). Any other ValidateState error keeps the
// prior behavior: no output file, and the error is returned unchanged.
func TestFinishValidationPartialOutputOnlyOnInterruption(t *testing.T) {
	canceled := errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "validation interrupted")
	revokeErr := errors.New(errors.ErrCodeInternal, "failed to revoke privileged RBAC")
	for _, tt := range []struct {
		name    string
		cause   error
		code    errors.ErrorCode
		written bool
	}{
		{"operator cancellation", canceled, errors.ErrCodeCanceled, true},
		{"cancellation and RBAC revocation failure", errors.Wrap(errors.ErrCodeInternal, "cluster-admin binding may be orphaned", stderrors.Join(canceled, revokeErr)), errors.ErrCodeInternal, true},
		{"pre-flight failure after earlier phases", errors.New(errors.ErrCodeInvalidRequest, "dependencyAffinity pre-flight failed"), errors.ErrCodeInvalidRequest, false},
		{"RBAC revocation failure after success", revokeErr, errors.ErrCodeInternal, false},
		{"deadline", errors.WrapCtxErr(context.DeadlineExceeded, errors.ErrCodeTimeout, "validation interrupted"), errors.ErrCodeTimeout, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			builder := ctrf.NewBuilder("aicr", "test", "deployment")
			builder.AddResult(&ctrf.ValidatorResult{Name: "completed", Phase: "deployment", ExitCode: 0})
			results := []*aicr.PhaseResult{{Phase: aicr.PhaseDeployment, Status: ctrf.StatusPassed, Report: builder.Build()}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cancellation := observeValidationCancellation(ctx, time.Now)
			defer cancellation.release()
			if tt.written {
				cancel()
			}
			output := filepath.Join(t.TempDir(), "partial.json")
			err := finishValidation(ctx, cancellation, nil, nil, nil, validationConfig{output: output, outFormat: serializer.FormatJSON}, results, tt.cause)
			errorstest.WantReportedCode(t, err, tt.code)
			if !stderrors.Is(err, tt.cause) {
				t.Errorf("cause replaced: %v", err)
			}
			_, statErr := os.Stat(output)
			if written := statErr == nil; written != tt.written {
				t.Errorf("output written=%v want %v (%v)", written, tt.written, statErr)
			}
		})
	}
}

func TestFinishValidationNoResultsWording(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cause error
		want  string
	}{
		{"unstructured cancellation", context.Canceled, "validation interrupted"},
		{"unstructured failure", stderrors.New("setup failed"), "validation failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := finishValidation(t.Context(), nil, nil, nil, nil, validationConfig{}, nil, tt.cause)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if !strings.Contains(err.Error(), tt.want) || !stderrors.Is(err, tt.cause) {
				t.Errorf("err=%v want wording %q", err, tt.want)
			}
		})
	}
}

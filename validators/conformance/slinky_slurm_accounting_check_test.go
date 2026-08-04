// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
)

func TestCheckSlinkySlurmHealthSkipsAccountingWhenNotEnabled(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
	}{
		{name: "legacy recipe without accounting override"},
		{
			name: "accounting explicitly disabled",
			overrides: map[string]any{
				"accounting": map[string]any{"enabled": false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := slurmReadyTestContext(t, false)
			ctx.ValidationInput.ComponentRefs[0].Overrides = tt.overrides
			var commands []string
			restore := replaceSlinkyExecForTest(func(
				_ context.Context,
				_ *validators.Context,
				_, _ string,
				command []string,
				_ podExecOptions,
			) (podExecResult, error) {

				commands = append(commands, command[0])
				if command[0] == "sbatch" || command[0] == "sacct" {
					t.Fatalf("unexpected accounting command %v", command)
				}
				return podExecResult{Stdout: "ok\n"}, nil
			})
			defer restore()

			if err := CheckSlinkySlurmHealth(ctx); err != nil {
				t.Fatalf("CheckSlinkySlurmHealth() error = %v", err)
			}
			if len(commands) != len(slinkySlurmHealthCommands) {
				t.Fatalf("commands = %v, want only %d base health commands",
					commands, len(slinkySlurmHealthCommands))
			}
		})
	}
}

func TestCheckSlinkySlurmHealthRejectsMalformedAccountingGate(t *testing.T) {
	ctx := slurmReadyTestContext(t, false)
	ctx.ValidationInput.ComponentRefs[0].Overrides = map[string]any{
		"accounting": map[string]any{"enabled": "true"},
	}
	err := CheckSlinkySlurmHealth(ctx)
	if err == nil || !strings.Contains(err.Error(), "must be boolean") {
		t.Fatalf("error = %v, want malformed accounting.enabled failure", err)
	}
}

func TestSlurmAccountingEnabledFromResolvedRecipes(t *testing.T) {
	tests := []struct {
		mode recipe.AccountingMode
		want bool
	}{
		{mode: recipe.AccountingModeDisabled, want: false},
		{mode: recipe.AccountingModeCustomerManaged, want: true},
		{mode: recipe.AccountingModeAICRProvided, want: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			result, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &recipe.Criteria{
				Service:     recipe.CriteriaServiceEKS,
				Accelerator: recipe.CriteriaAcceleratorH100,
				Intent:      recipe.CriteriaIntentTraining,
				OS:          recipe.CriteriaOSUbuntu,
				Platform:    recipe.CriteriaPlatformSlurm,
			}, recipe.WithAccountingMode(tt.mode))
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}
			enabled, err := slurmAccountingEnabled(v1.ToValidationInput(result))
			if err != nil {
				t.Fatalf("slurmAccountingEnabled() error = %v", err)
			}
			if enabled != tt.want {
				t.Fatalf("slurmAccountingEnabled() = %t, want %t", enabled, tt.want)
			}
		})
	}
}

func TestCheckSlinkySlurmHealthPersistsCompletedJobWhenAccountingEnabled(t *testing.T) {
	ctx := slurmAccountingTestContext(t)
	var commands []string
	var jobName string
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		command []string,
		opts podExecOptions,
	) (podExecResult, error) {

		commands = append(commands, strings.Join(command, " "))
		if opts != slinkyLoginPodExecOptions {
			t.Fatalf("pod exec options = %+v, want %+v", opts, slinkyLoginPodExecOptions)
		}
		switch command[0] {
		case "sbatch":
			for _, arg := range command {
				if parsed, found := strings.CutPrefix(arg, "--job-name="); found {
					jobName = parsed
					break
				}
			}
			return podExecResult{Stdout: "12345;cluster\n"}, nil
		case "sacct":
			return podExecResult{
				Stdout: fmt.Sprintf("12345|%s|COMPLETED|0:0\n", jobName),
			}, nil
		default:
			return podExecResult{Stdout: "ok\n"}, nil
		}
	})
	defer restore()

	var err error
	out := captureStdout(t, func() {
		err = CheckSlinkySlurmHealth(ctx)
	})
	if err != nil {
		t.Fatalf("CheckSlinkySlurmHealth() error = %v", err)
	}
	if len(commands) != len(slinkySlurmHealthCommands)+2 {
		t.Fatalf("commands = %v, want base health commands then sbatch and sacct", commands)
	}
	sbatchCommand := commands[len(commands)-2]
	sacctCommand := commands[len(commands)-1]
	if strings.Contains(sbatchCommand, " --wait") ||
		!strings.Contains(sbatchCommand, "sbatch --parsable") ||
		!strings.Contains(sbatchCommand,
			"--time="+slurmAccountingJobTimeLimit(defaults.SlurmAccountingRecordTimeout)) ||
		!strings.Contains(sacctCommand, "sacct --allocations --noheader --parsable2 --jobs=12345") {

		t.Fatalf("commands = %v, want bounded sbatch and sacct probes", commands)
	}
	for _, want := range []string{
		"--- Slinky Slurm accounting record ---",
		"JobID: 12345",
		"JobName: " + slurmAccountingProbeJobName + "-",
		"State: COMPLETED",
		"ExitCode: 0:0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want containing %q", out, want)
		}
	}
}

func TestSlurmAccountingJobTimeLimit(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{name: "minimum", want: "00:00:01"},
		{name: "rounds up subsecond", timeout: 1500 * time.Millisecond, want: "00:00:02"},
		{name: "configured timeout", timeout: defaults.SlurmAccountingRecordTimeout, want: "00:02:00"},
		{name: "hours", timeout: 2*time.Hour + 3*time.Minute + 4*time.Second, want: "02:03:04"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slurmAccountingJobTimeLimit(tt.timeout); got != tt.want {
				t.Errorf("slurmAccountingJobTimeLimit(%s) = %q, want %q", tt.timeout, got, tt.want)
			}
		})
	}
}

func TestSubmitSlurmAccountingProbeFailures(t *testing.T) {
	tests := []struct {
		name   string
		result podExecResult
		err    error
		want   string
	}{
		{
			name: "exec failure",
			err:  errors.New(errors.ErrCodeInternal, "exec failed"),
			want: "exec failed",
		},
		{
			name:   "nonzero exit",
			result: podExecResult{ExitCode: 1, Stderr: "submission failed"},
			want:   "exit code 1",
		},
		{
			name:   "missing job ID",
			result: podExecResult{},
			want:   "no Slurm job ID",
		},
		{
			name:   "malformed job ID",
			result: podExecResult{Stdout: "not-a-job\n"},
			want:   "invalid Slurm job ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := replaceSlinkyExecForTest(func(
				context.Context,
				*validators.Context,
				string,
				string,
				[]string,
				podExecOptions,
			) (podExecResult, error) {

				return tt.result, tt.err
			})
			defer restore()

			_, err := submitSlurmAccountingProbe(
				&validators.Context{Ctx: context.Background()},
				"slurm", "login", slurmAccountingProbeJobName+"-test")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestWaitForSlurmAccountingRecordRetriesUntilVisible(t *testing.T) {
	var calls int
	restore := replaceSlinkyExecForTest(func(
		context.Context,
		*validators.Context,
		string,
		string,
		[]string,
		podExecOptions,
	) (podExecResult, error) {

		calls++
		if calls == 1 {
			return podExecResult{}, nil
		}
		return podExecResult{Stdout: "42|probe|COMPLETED|0:0\n"}, nil
	})
	defer restore()

	record, err := waitForSlurmAccountingRecord(
		&validators.Context{Ctx: context.Background()},
		"slurm",
		"login",
		"42",
		"probe",
		100*time.Millisecond,
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("waitForSlurmAccountingRecord() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("sacct calls = %d, want 2", calls)
	}
	if record.State != "COMPLETED" || record.ExitCode != "0:0" {
		t.Fatalf("record = %+v, want completed 0:0", record)
	}
}

func TestWaitForSlurmAccountingRecordFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		result  podExecResult
		timeout time.Duration
		want    string
	}{
		{
			name:    "terminal failure",
			result:  podExecResult{Stdout: "42|probe|FAILED|1:0\n"},
			timeout: time.Second,
			want:    "terminal but unsuccessful",
		},
		{
			name:    "record never visible",
			result:  podExecResult{},
			timeout: 20 * time.Millisecond,
			want:    "did not become complete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := replaceSlinkyExecForTest(func(
				context.Context,
				*validators.Context,
				string,
				string,
				[]string,
				podExecOptions,
			) (podExecResult, error) {

				return tt.result, nil
			})
			defer restore()

			_, err := waitForSlurmAccountingRecord(
				&validators.Context{Ctx: context.Background()},
				"slurm",
				"login",
				"42",
				"probe",
				tt.timeout,
				time.Millisecond,
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseSlurmAccountingRecord(t *testing.T) {
	tests := []struct {
		name       string
		accounting string
		want       slurmAccountingRecord
		wantFound  bool
	}{
		{
			name:       "exact allocation match",
			accounting: "42.batch|probe|COMPLETED|0:0\n42|probe|COMPLETED|0:0\n",
			want: slurmAccountingRecord{
				JobID: "42", JobName: "probe", State: "COMPLETED", ExitCode: "0:0",
			},
			wantFound: true,
		},
		{
			name:       "step record rejected",
			accounting: "42.batch|probe|COMPLETED|0:0\n",
		},
		{
			name:       "stale job name rejected",
			accounting: "42|stale-probe|COMPLETED|0:0\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record, found := parseSlurmAccountingRecord(tt.accounting, "42", "probe")
			if found != tt.wantFound {
				t.Fatalf("parseSlurmAccountingRecord() found = %v, want %v",
					found, tt.wantFound)
			}
			if found && (record == nil || *record != tt.want) {
				t.Errorf("parseSlurmAccountingRecord() record = %+v, want %+v",
					record, tt.want)
			}
			if !found && record != nil {
				t.Errorf("parseSlurmAccountingRecord() record = %+v, want nil", record)
			}
		})
	}
}

func TestIsTerminalSlurmJobState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{state: "COMPLETED", want: true},
		{state: "CANCELLED+", want: true},
		{state: "CANCELLED by 1000", want: true},
		{state: "CANCELLED+ by 1000", want: true},
		{state: "RUNNING"},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := isTerminalSlurmJobState(tt.state); got != tt.want {
				t.Errorf("isTerminalSlurmJobState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestRunSlinkySlurmAccountingProbeCancelsFailedJob(t *testing.T) {
	var submittedJobName string
	var canceledJobID string
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		command []string,
		_ podExecOptions,
	) (podExecResult, error) {

		switch command[0] {
		case "sbatch":
			for _, arg := range command {
				if parsed, found := strings.CutPrefix(arg, "--job-name="); found {
					submittedJobName = parsed
					break
				}
			}
			return podExecResult{Stdout: "42\n"}, nil
		case "sacct":
			return podExecResult{
				Stdout: fmt.Sprintf("42|%s|FAILED|1:0\n", submittedJobName),
			}, nil
		case "scancel":
			canceledJobID = command[1]
			return podExecResult{}, nil
		default:
			t.Fatalf("unexpected command %v", command)
			return podExecResult{}, nil
		}
	})
	defer restore()

	err := runSlinkySlurmAccountingProbe(
		&validators.Context{Ctx: t.Context()}, "slurm", "login")
	if err == nil || !strings.Contains(err.Error(), "terminal but unsuccessful") {
		t.Fatalf("runSlinkySlurmAccountingProbe() error = %v, want terminal failure", err)
	}
	if canceledJobID != "42" {
		t.Fatalf("canceled job ID = %q, want 42", canceledJobID)
	}
}

func slurmAccountingTestContext(t *testing.T) *validators.Context {
	t.Helper()
	ctx := slurmReadyTestContext(t, false)
	ctx.ValidationInput.ComponentRefs[0].Overrides = map[string]any{
		"accounting": map[string]any{"enabled": true},
	}
	return ctx
}

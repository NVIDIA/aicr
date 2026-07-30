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
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
)

const slurmAccountingProbeJobName = "aicr-sacct-conformance"

type slurmAccountingRecord struct {
	JobID    string
	JobName  string
	State    string
	ExitCode string
}

// runSlinkySlurmAccountingProbe proves that a completed batch job is
// persisted through SlurmDBD into the configured database.
func runSlinkySlurmAccountingProbe(
	ctx *validators.Context,
	namespace string,
	loginPodName string,
) error {

	jobName := slurmAccountingProbeJobName + "-" + uuid.NewString()
	jobID, err := submitSlurmAccountingProbe(ctx, namespace, loginPodName, jobName)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			cancelSlurmAccountingProbe(ctx, namespace, loginPodName, jobID)
		}
	}()
	record, err := waitForSlurmAccountingRecord(
		ctx,
		namespace,
		loginPodName,
		jobID,
		jobName,
		defaults.SlurmAccountingRecordTimeout,
		defaults.SlurmAccountingRecordPollInterval,
	)
	if err != nil {
		return err
	}

	recordRawTextArtifact(ctx, "Slinky Slurm accounting record",
		fmt.Sprintf("sacct --allocations --jobs=%s", jobID),
		fmt.Sprintf("JobID: %s\nJobName: %s\nState: %s\nExitCode: %s",
			record.JobID, record.JobName, record.State, record.ExitCode))
	completed = true
	return nil
}

func slurmAccountingEnabled(input *v1.ValidationInput) (bool, error) {
	for i := range input.ComponentRefs {
		ref := &input.ComponentRefs[i]
		if ref.Name != slinkySlurmComponent || !ref.IsEnabled() {
			continue
		}
		value, found := component.GetValueByPath(ref.Overrides, "accounting.enabled")
		if !found {
			// Recipes created before typed accounting was introduced do not
			// carry this override. Preserve their original health behavior.
			return false, nil
		}
		enabled, ok := value.(bool)
		if !ok {
			return false, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("slinky-slurm accounting.enabled must be boolean (got %T)", value))
		}
		return enabled, nil
	}
	return false, nil
}

func submitSlurmAccountingProbe(
	ctx *validators.Context,
	namespace string,
	podName string,
	jobName string,
) (string, error) {

	result, err := slinkyExecCommand(
		ctx.Ctx,
		ctx,
		namespace,
		podName,
		[]string{
			"sbatch",
			"--parsable",
			"--job-name=" + jobName,
			"--time=" + slurmAccountingJobTimeLimit(defaults.SlurmAccountingRecordTimeout),
			"--output=/dev/null",
			"--wrap=true",
		},
		slinkyLoginPodExecOptions,
	)
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to submit Slurm accounting conformance job")
	}
	if result.ExitCode != 0 {
		return "", errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"Slurm accounting conformance job submission failed with exit code %d: %s",
			result.ExitCode, strings.TrimSpace(result.Stderr)))
	}
	jobID, err := parseSlurmJobID(result.Stdout)
	if err != nil {
		return "", err
	}
	return jobID, nil
}

func cancelSlurmAccountingProbe(
	ctx *validators.Context,
	namespace string,
	podName string,
	jobID string,
) {

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()

	result, err := slinkyExecCommand(
		cleanupCtx,
		ctx,
		namespace,
		podName,
		[]string{"scancel", jobID},
		slinkyLoginPodExecOptions,
	)
	if err != nil {
		slog.Warn("failed to cancel Slurm accounting conformance job",
			"jobID", jobID, "error", err)
		return
	}
	if result.ExitCode != 0 {
		slog.Warn("Slurm accounting conformance job cancellation failed",
			"jobID", jobID,
			"exitCode", result.ExitCode,
			"stderr", strings.TrimSpace(result.Stderr))
	}
}

func parseSlurmJobID(stdout string) (string, error) {
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return "", errors.New(errors.ErrCodeInternal,
			"sbatch returned no Slurm job ID")
	}
	jobID, _, _ := strings.Cut(fields[0], ";")
	parsed, err := strconv.ParseUint(jobID, 10, 64)
	if err != nil || parsed == 0 {
		return "", errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("sbatch returned invalid Slurm job ID %q", fields[0]))
	}
	return jobID, nil
}

func slurmAccountingJobTimeLimit(timeout time.Duration) string {
	seconds := int64(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, (seconds%3600)/60, seconds%60)
}

func waitForSlurmAccountingRecord(
	ctx *validators.Context,
	namespace string,
	podName string,
	jobID string,
	jobName string,
	timeout time.Duration,
	interval time.Duration,
) (*slurmAccountingRecord, error) {

	waitCtx, cancel := context.WithTimeout(ctx.Ctx, timeout)
	defer cancel()

	var lastObservation string
	var record *slurmAccountingRecord
	err := wait.PollUntilContextCancel(waitCtx, interval, true,
		func(pollCtx context.Context) (bool, error) {
			result, execErr := slinkyExecCommand(
				pollCtx,
				ctx,
				namespace,
				podName,
				[]string{
					"sacct",
					"--allocations",
					"--noheader",
					"--parsable2",
					"--jobs=" + jobID,
					"--starttime=now-1hour",
					"--format=JobIDRaw,JobName%128,State,ExitCode",
				},
				slinkyLoginPodExecOptions,
			)
			if execErr != nil {
				if pollCtx.Err() == nil {
					lastObservation = execErr.Error()
				}
				return false, nil
			}
			if result.ExitCode != 0 {
				lastObservation = fmt.Sprintf("sacct exit code %d: %s",
					result.ExitCode, strings.TrimSpace(result.Stderr))
				return false, nil
			}

			observed, found := parseSlurmAccountingRecord(result.Stdout, jobID, jobName)
			if !found {
				lastObservation = "job record not found"
				return false, nil
			}
			record = observed
			lastObservation = fmt.Sprintf("state=%s exitCode=%s", record.State, record.ExitCode)
			if record.State == "COMPLETED" && record.ExitCode == "0:0" {
				return true, nil
			}
			if isTerminalSlurmJobState(record.State) {
				return false, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"Slurm accounting record for job %s is terminal but unsuccessful: %s",
					jobID, lastObservation))
			}
			return false, nil
		},
	)
	if err == nil {
		return record, nil
	}
	if ctx.Ctx.Err() != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"waiting for Slurm accounting record canceled", ctx.Ctx.Err())
	}
	if waitCtx.Err() != nil {
		return nil, errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
			"Slurm accounting record for job %s did not become complete within %s: %s",
			jobID, timeout, lastObservation))
	}
	return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
		"failed while waiting for Slurm accounting record")
}

func parseSlurmAccountingRecord(stdout, jobID, jobName string) (*slurmAccountingRecord, bool) {
	for line := range strings.Lines(stdout) {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) < 4 ||
			strings.TrimSpace(fields[0]) != jobID ||
			strings.TrimSpace(fields[1]) != jobName {

			continue
		}
		return &slurmAccountingRecord{
			JobID:    jobID,
			JobName:  jobName,
			State:    strings.TrimSpace(fields[2]),
			ExitCode: strings.TrimSpace(fields[3]),
		}, true
	}
	return nil, false
}

func isTerminalSlurmJobState(state string) bool {
	stateToken, _, _ := strings.Cut(strings.TrimSpace(state), " ")
	stateToken = strings.TrimSuffix(stateToken, "+")
	switch stateToken {
	case "BOOT_FAIL", "CANCELLED", "COMPLETED", "DEADLINE", "FAILED",
		"NODE_FAIL", "OUT_OF_MEMORY", "PREEMPTED", "REVOKED", "TIMEOUT":
		return true
	default:
		return false
	}
}

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

package main

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

// Commands the Slurm GPU probe runs through the login pod. Named because the
// health and IMEX checks already use both literals (goconst counts three).
const (
	slurmSrunCommand = "srun"
	posixShell       = "/bin/sh"
)

// CheckSlinkySlurmGPUAccess verifies on Slinky Slurm recipes the two properties
// secure-accelerator-access verifies for Kubernetes pods and skips here: a
// Slurm job allocated one GPU can use exactly that GPU, and a job with no GPU
// allocation on the same Slurm node cannot open any GPU device.
func CheckSlinkySlurmGPUAccess(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if ctx.RESTConfig == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}
	if ctx.ValidationInput == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "validation is not available")
	}
	if !recipeHasComponent(ctx, slinkySlurmComponent) {
		return validators.Skip("slinky-slurm component not present in recipe")
	}
	criteria := ctx.ValidationInput.Criteria
	if criteria.Service == recipe.CriteriaServiceKind {
		return validators.Skip("recipe service is kind: Kind Slinky leaves run without task/cgroup, so there is no Slurm GPU device confinement to verify")
	}

	namespace := resolveSlinkySlurmNamespace(ctx)
	if err := discoverSlinkySetAPIs(ctx); err != nil {
		return err
	}
	nodeSetPods, err := runnableSlinkyNodeSetPods(ctx, namespace)
	if err != nil {
		return err
	}
	if !nodeSetPodsRequestNVIDIAGPUs(nodeSetPods) {
		concreteService := criteria.Service != "" && criteria.Service != recipe.CriteriaServiceAny
		concreteAccelerator := criteria.Accelerator != "" && criteria.Accelerator != recipe.CriteriaAcceleratorAny
		if concreteService && concreteAccelerator {
			return errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
				"recipe criteria require service=%s accelerator=%s, but no NodeSet pod has a positive nvidia.com/gpu request or limit",
				criteria.Service, criteria.Accelerator))
		}
		return validators.Skip("no NodeSet pod has a positive nvidia.com/gpu request or limit and the recipe criteria are incomplete: no GPU-backed Slurm node to verify")
	}

	loginPod, err := findReadySlinkyLoginPod(ctx, namespace)
	if err != nil {
		return err
	}

	allocated, err := runSlinkySlurmGPUProbe(ctx, namespace, loginPod.Name,
		"GPU access (allocated job)", slinkySlurmGPUProbeCommand("--gpus=1"))
	if err != nil {
		return err
	}
	if evalErr := evaluateAllocatedGPUProbe(allocated); evalErr != nil {
		recordSlinkySlurmGPUAccessSummary(ctx, &allocated, nil, "not run", evalErr)
		return evalErr
	}

	select {
	case <-ctx.Ctx.Done():
		canceledErr := errors.Wrap(errors.ErrCodeTimeout, "canceled before the unallocated Slurm GPU isolation job", ctx.Ctx.Err())
		recordSlinkySlurmGPUAccessSummary(ctx, &allocated, nil, "not run (canceled)", canceledErr)
		return canceledErr
	default:
	}

	unallocated, err := runSlinkySlurmGPUProbe(ctx, namespace, loginPod.Name,
		"GPU isolation (unallocated job)", slinkySlurmGPUProbeCommand("--nodelist="+allocated.node))
	if err != nil {
		recordSlinkySlurmGPUAccessSummary(ctx, &allocated, nil,
			"ran but returned no usable result (see the GPU isolation (unallocated job) result)", err)
		return err
	}
	evalErr := evaluateUnallocatedGPUProbe(unallocated, allocated.node)
	recordSlinkySlurmGPUAccessSummary(ctx, &allocated, &unallocated, "", evalErr)
	return evalErr
}

// slinkySlurmGPUProbeCommand bounds the probe job like the other Slinky
// checks; placement is "--gpus=1" for the allocated job and
// "--nodelist=<node>" for the unallocated one. No container image: the
// cgroup Slurm sets up is the same with or without Pyxis.
func slinkySlurmGPUProbeCommand(placement string) []string {
	return []string{
		slurmSrunCommand, "--immediate=30", "--time=1:00", "--nodes=1", "--ntasks=1", "--cpus-per-task=1", "--mem=128M",
		placement, posixShell, "-c", slinkySlurmGPUProbeShell,
	}
}

func runSlinkySlurmGPUProbe(
	ctx *validators.Context,
	namespace, loginPodName, label string,
	command []string,
) (slurmGPUProbe, error) {

	result, execErr := slinkyExecCommand(ctx.Ctx, ctx, namespace, loginPodName, command, slinkyLoginPodExecOptions)
	recordSlinkyExecResult(ctx, namespace, loginPodName,
		slinkySlurmHealthCommand{label: label, command: command}, result, execErr)
	if execErr != nil {
		return slurmGPUProbe{}, errors.Wrap(errors.ErrCodeInternal, label+": exec failed", execErr)
	}
	if result.ExitCode != 0 {
		detail := lastNonEmptyLine(result.Stderr)
		if parsed, parseErr := parseSlurmGPUProbe(result.Stdout); parseErr == nil && parsed.probeError != "" {
			detail = "PROBE_ERROR=" + parsed.probeError
		}
		return slurmGPUProbe{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s: exit code %d: %s", label, result.ExitCode, valueOrUnknown(detail)))
	}
	probe, err := parseSlurmGPUProbe(result.Stdout)
	if err != nil {
		return slurmGPUProbe{}, errors.Wrap(errors.ErrCodeInternal, label+": unreadable probe output", err)
	}
	return probe, nil
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// recordSlinkySlurmGPUAccessSummary prints unallocatedStatus in place of the
// unallocated probe when that probe is nil: never run, or run without a
// parseable result.
func recordSlinkySlurmGPUAccessSummary(
	ctx *validators.Context,
	allocated, unallocated *slurmGPUProbe,
	unallocatedStatus string,
	verdict error,
) {

	var body strings.Builder
	fmt.Fprintf(&body, "Node:             %s\n", valueOrUnknown(allocated.node))
	fmt.Fprintf(&body, "Allocated job:    %s\n", formatSlurmGPUProbe(*allocated))
	if unallocated != nil {
		fmt.Fprintf(&body, "Unallocated job:  %s\n", formatSlurmGPUProbe(*unallocated))
	} else {
		fmt.Fprintf(&body, "Unallocated job:  %s\n", unallocatedStatus)
	}
	if verdict == nil {
		body.WriteString("Verdict:          PASS\n")
	} else {
		fmt.Fprintf(&body, "Verdict:          FAIL: %v\n", verdict)
	}
	recordRawTextArtifact(ctx, "Slinky Slurm GPU access and isolation", "", body.String())
}

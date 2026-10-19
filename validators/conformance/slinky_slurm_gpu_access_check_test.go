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
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// gpuSlurmTestContext is slurmReadyTestContext on an EKS/H100 recipe whose
// NodeSet pod holds gpus GPUs ("" leaves the pod without a GPU limit).
func gpuSlurmTestContext(t *testing.T, kwok bool, gpus string) *validators.Context {
	t.Helper()
	ctx := slurmReadyTestContext(t, kwok)
	ctx.ValidationInput.Criteria = recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
	}
	if gpus == "" {
		return ctx
	}
	pod, err := ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Get(ctx.Ctx, "slinky-nodeset-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeSet pod: %v", err)
	}
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse(gpus),
	}
	if _, err = ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Update(ctx.Ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update NodeSet pod: %v", err)
	}
	return ctx
}

// slurmGPUExecFake answers the allocated job (the command carrying --gpus=1)
// and the unallocated job, and records every command it was asked to run.
type slurmGPUExecFake struct {
	allocated      podExecResult
	allocatedErr   error
	unallocated    podExecResult
	unallocatedErr error
	afterAllocated func()
	commands       [][]string
	options        []podExecOptions
}

func (f *slurmGPUExecFake) exec(
	_ context.Context, _ *validators.Context, _, _ string, command []string, opts podExecOptions,
) (podExecResult, error) {

	f.commands = append(f.commands, slices.Clone(command))
	f.options = append(f.options, opts)
	if slices.Contains(command, "--gpus=1") {
		if f.afterAllocated != nil {
			f.afterAllocated()
		}
		return f.allocated, f.allocatedErr
	}
	return f.unallocated, f.unallocatedErr
}

var (
	allocatedProbeOK   = podExecResult{Stdout: slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, epermExcept(3))}
	unallocatedProbeOK = podExecResult{Stdout: slurmGPUProbeStdout("slinky-0", 6, []string{"No devices were found"}, epermExcept(-1))}
)

func TestCheckSlinkySlurmGPUAccessRequiresContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  *validators.Context
	}{
		{name: "no clientset", ctx: &validators.Context{Ctx: context.Background(),
			RESTConfig: &rest.Config{}, ValidationInput: &v1.ValidationInput{}}},
		{name: "no RESTConfig", ctx: &validators.Context{Ctx: context.Background(),
			Clientset: k8sfake.NewSimpleClientset(), ValidationInput: &v1.ValidationInput{}}},
		{name: "no validation input", ctx: &validators.Context{Ctx: context.Background(),
			Clientset: k8sfake.NewSimpleClientset(), RESTConfig: &rest.Config{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSlinkySlurmGPUAccess(tt.ctx)
			if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
				t.Fatalf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestCheckSlinkySlurmGPUAccessSkipsWithoutSlinkyComponent(t *testing.T) {
	ctx := &validators.Context{
		Ctx:        context.Background(),
		Clientset:  k8sfake.NewSimpleClientset(),
		RESTConfig: &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
		},
	}
	err := CheckSlinkySlurmGPUAccess(ctx)
	if !validators.IsSkip(err) || !strings.Contains(err.Error(), "slinky-slurm component not present in recipe") {
		t.Fatalf("error = %v, want skip naming the missing slinky-slurm component", err)
	}
}

func TestCheckSlinkySlurmGPUAccessGates(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func(t *testing.T) *validators.Context
		wantSkip bool
		wantCode errors.ErrorCode
		wantText string
	}{
		{
			name: "kind recipes have no task/cgroup to verify",
			ctx: func(t *testing.T) *validators.Context {
				ctx := gpuSlurmTestContext(t, false, "8")
				ctx.ValidationInput.Criteria.Service = recipe.CriteriaServiceKind
				return ctx
			},
			wantSkip: true,
			wantText: "recipe service is kind",
		},
		{
			name:     "NodeSet pods on KWOK nodes",
			ctx:      func(t *testing.T) *validators.Context { return gpuSlurmTestContext(t, true, "8") },
			wantSkip: true,
			wantText: "KWOK",
		},
		{
			name:     "GPU recipe whose NodeSet holds no GPU fails closed",
			ctx:      func(t *testing.T) *validators.Context { return gpuSlurmTestContext(t, false, "") },
			wantCode: errors.ErrCodeUnavailable,
			wantText: "recipe criteria require service=eks accelerator=h100, but no NodeSet pod has a positive nvidia.com/gpu request or limit",
		},
		{
			name: "incomplete criteria without a GPU NodeSet skips",
			ctx: func(t *testing.T) *validators.Context {
				ctx := gpuSlurmTestContext(t, false, "")
				ctx.ValidationInput.Criteria = recipe.Criteria{}
				return ctx
			},
			wantSkip: true,
			wantText: "criteria are incomplete",
		},
		{
			name: "concrete service with any accelerator without a GPU NodeSet skips",
			ctx: func(t *testing.T) *validators.Context {
				ctx := gpuSlurmTestContext(t, false, "")
				ctx.ValidationInput.Criteria = recipe.Criteria{
					Service:     recipe.CriteriaServiceEKS,
					Accelerator: recipe.CriteriaAcceleratorAny,
				}
				return ctx
			},
			wantSkip: true,
			wantText: "criteria are incomplete",
		},
		{
			name: "any service with a concrete accelerator without a GPU NodeSet skips",
			ctx: func(t *testing.T) *validators.Context {
				ctx := gpuSlurmTestContext(t, false, "")
				ctx.ValidationInput.Criteria = recipe.Criteria{
					Service:     recipe.CriteriaServiceAny,
					Accelerator: recipe.CriteriaAcceleratorH100,
				}
				return ctx
			},
			wantSkip: true,
			wantText: "criteria are incomplete",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &slurmGPUExecFake{allocated: allocatedProbeOK, unallocated: unallocatedProbeOK}
			defer replaceSlinkyExecForTest(fake.exec)()

			err := CheckSlinkySlurmGPUAccess(tt.ctx(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantText)
			}
			if tt.wantSkip != validators.IsSkip(err) {
				t.Fatalf("IsSkip(%v) = %t, want %t", err, validators.IsSkip(err), tt.wantSkip)
			}
			if !tt.wantSkip && errorstest.ReportedCode(err) != tt.wantCode {
				t.Fatalf("error = %v, want code %s", err, tt.wantCode)
			}
			if len(fake.commands) != 0 {
				t.Fatalf("ran %d srun commands, want 0 when the check does not apply", len(fake.commands))
			}
		})
	}
}

func TestCheckSlinkySlurmGPUAccessPassesAndPinsIsolationJobToAllocatedNode(t *testing.T) {
	fake := &slurmGPUExecFake{allocated: allocatedProbeOK, unallocated: unallocatedProbeOK}
	defer replaceSlinkyExecForTest(fake.exec)()
	ctx := gpuSlurmTestContext(t, false, "8")

	var err error
	out := captureStdout(t, func() { err = CheckSlinkySlurmGPUAccess(ctx) })
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	want := [][]string{
		{"srun", "--immediate=30", "--time=1:00", "--nodes=1", "--ntasks=1", "--cpus-per-task=1", "--mem=128M",
			"--gpus=1", "/bin/sh", "-c", slinkySlurmGPUProbeShell},
		{"srun", "--immediate=30", "--time=1:00", "--nodes=1", "--ntasks=1", "--cpus-per-task=1", "--mem=128M",
			"--nodelist=slinky-0", "/bin/sh", "-c", slinkySlurmGPUProbeShell},
	}
	if len(fake.commands) != len(want) {
		t.Fatalf("ran %d commands, want %d", len(fake.commands), len(want))
	}
	for i := range want {
		if !slices.Equal(fake.commands[i], want[i]) {
			t.Fatalf("command %d = %q, want %q", i, fake.commands[i], want[i])
		}
	}
	for _, opts := range fake.options {
		if opts != slinkyLoginPodExecOptions {
			t.Fatalf("exec options = %+v, want the Slinky login pod options", opts)
		}
	}
	if !strings.Contains(out, "--- Slinky Slurm GPU access and isolation ---") || !strings.Contains(out, "Verdict:          PASS") {
		t.Fatalf("output = %q, want a PASS summary artifact", out)
	}
}

func TestCheckSlinkySlurmGPUAccessDoesNotRunIsolationJobWhenAllocatedJobFails(t *testing.T) {
	tests := []struct {
		name       string
		allocated  podExecResult
		execErr    error
		wantErr    string
		wantOutput []string
	}{
		{
			name:       "allocated job sees two GPUs",
			allocated:  podExecResult{Stdout: slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine, "GPU-ffffffff-0000-1111-2222-333333333333, Disabled"}, epermExcept(0))},
			wantErr:    "nvidia-smi listed 2 GPUs, want exactly 1",
			wantOutput: []string{"Verdict:          FAIL", "Unallocated job:  not run"},
		},
		{
			name:    "exec to the login pod fails",
			execErr: errors.New(errors.ErrCodeInternal, "stream closed"),
			wantErr: "GPU access (allocated job): exec failed",
		},
		{
			name: "allocated job cannot be scheduled",
			allocated: podExecResult{ExitCode: 1,
				Stderr: "srun: error: Unable to allocate resources: Requested node configuration is not available\n"},
			wantErr: "GPU access (allocated job): exit code 1: srun: error: Unable to allocate resources: Requested node configuration is not available",
		},
		{
			name:      "perl missing from the slurmd image",
			allocated: podExecResult{ExitCode: 1, Stdout: "NODE=slinky-0\nNVSMI_RC=0\nPROBE_ERROR=perl not found\n"},
			wantErr:   "GPU access (allocated job): exit code 1: PROBE_ERROR=perl not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &slurmGPUExecFake{allocated: tt.allocated, allocatedErr: tt.execErr, unallocated: unallocatedProbeOK}
			defer replaceSlinkyExecForTest(fake.exec)()
			ctx := gpuSlurmTestContext(t, false, "8")

			var err error
			out := captureStdout(t, func() { err = CheckSlinkySlurmGPUAccess(ctx) })
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if len(fake.commands) != 1 {
				t.Fatalf("ran %d commands, want only the allocated job", len(fake.commands))
			}
			for _, want := range tt.wantOutput {
				if !strings.Contains(out, want) {
					t.Fatalf("output = %q, want containing %q", out, want)
				}
			}
		})
	}
}

func TestCheckSlinkySlurmGPUAccessFailsWhenUnallocatedJobOpensGPU(t *testing.T) {
	fake := &slurmGPUExecFake{
		allocated:   allocatedProbeOK,
		unallocated: podExecResult{Stdout: slurmGPUProbeStdout("slinky-0", 6, nil, epermExcept(2))},
	}
	defer replaceSlinkyExecForTest(fake.exec)()
	ctx := gpuSlurmTestContext(t, false, "8")

	var err error
	out := captureStdout(t, func() { err = CheckSlinkySlurmGPUAccess(ctx) })
	if err == nil || !strings.Contains(err.Error(), "opened 1 GPU device node(s) without a GPU allocation: Slurm GPU isolation is broken") {
		t.Fatalf("error = %v, want broken-isolation failure", err)
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
		t.Fatalf("error = %v, want ErrCodeInternal", err)
	}
	if !strings.Contains(out, "Verdict:          FAIL") {
		t.Fatalf("output = %q, want a FAIL summary artifact", out)
	}
}

func TestCheckSlinkySlurmGPUAccessSummarizesUnallocatedExecFailure(t *testing.T) {
	fake := &slurmGPUExecFake{
		allocated:      allocatedProbeOK,
		unallocatedErr: errors.New(errors.ErrCodeInternal, "stream closed"),
	}
	defer replaceSlinkyExecForTest(fake.exec)()
	ctx := gpuSlurmTestContext(t, false, "8")

	var err error
	out := captureStdout(t, func() { err = CheckSlinkySlurmGPUAccess(ctx) })
	if err == nil || !strings.Contains(err.Error(), "GPU isolation (unallocated job): exec failed") {
		t.Fatalf("error = %v, want the unallocated job's exec failure", err)
	}
	for _, want := range []string{"Unallocated job:  ran but returned no usable result", "Verdict:          FAIL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want containing %q", out, want)
		}
	}
}

func TestCheckSlinkySlurmGPUAccessStopsWhenCanceledBetweenJobs(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &slurmGPUExecFake{allocated: allocatedProbeOK, unallocated: unallocatedProbeOK, afterAllocated: cancel}
	defer replaceSlinkyExecForTest(fake.exec)()
	ctx := gpuSlurmTestContext(t, false, "8")
	ctx.Ctx = base

	var err error
	out := captureStdout(t, func() { err = CheckSlinkySlurmGPUAccess(ctx) })
	if errorstest.ReportedCode(err) != errors.ErrCodeTimeout {
		t.Fatalf("error = %v, want ErrCodeTimeout", err)
	}
	if len(fake.commands) != 1 {
		t.Fatalf("ran %d commands, want only the allocated job", len(fake.commands))
	}
	for _, want := range []string{
		"Node:             slinky-0\n",
		"Unallocated job:  not run (canceled)\n",
		"Verdict:          FAIL: " + err.Error() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want the summary artifact containing %q", out, want)
		}
	}
}

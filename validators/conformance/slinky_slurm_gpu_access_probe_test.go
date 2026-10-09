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
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const testGPUUUID = "GPU-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"

// testGPULine is one nvidia-smi --query-gpu=uuid,mig.mode.current line.
const testGPULine = testGPUUUID + ", Disabled"

// slurmGPUProbeStdout renders output in the shape slinkySlurmGPUProbeShell
// prints. errnos[i] is the open errno reported for /dev/nvidia<i>.
func slurmGPUProbeStdout(node string, nvsmiExit int, nvsmiLines []string, errnos []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "NODE=%s\nCUDA_VISIBLE_DEVICES=0\nNVSMI_RC=%d\n", node, nvsmiExit)
	for _, line := range nvsmiLines {
		fmt.Fprintf(&b, "NVSMI_LINE=%s\n", line)
	}
	fmt.Fprintf(&b, "DEVICES_LISTED=%d\n", len(errnos))
	for i, errno := range errnos {
		fmt.Fprintf(&b, "DEV=/dev/nvidia%d %d\n", i, errno)
	}
	b.WriteString("DONE=1\n")
	return b.String()
}

// epermExcept returns the open errnos of an 8-GPU node: all EPERM (1) except
// index openIdx, which is 0 (opened). openIdx -1 leaves every device denied.
func epermExcept(openIdx int) []int {
	errnos := make([]int, 8)
	for i := range errnos {
		errnos[i] = 1
	}
	if openIdx >= 0 {
		errnos[openIdx] = 0
	}
	return errnos
}

func TestParseSlurmGPUProbe(t *testing.T) {
	twoDevices := []slurmGPUDeviceOpen{{path: "/dev/nvidia0", errno: 0}, {path: "/dev/nvidia1", errno: 1}}
	tests := []struct {
		name    string
		stdout  string
		want    slurmGPUProbe
		wantErr string
	}{
		{
			name:   "allocated job on a two-GPU node",
			stdout: slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 1}),
			want: slurmGPUProbe{node: "slinky-0", cudaVisibleDevices: "0", nvsmiSeen: true,
				gpuUUIDs: []string{testGPUUUID}, devicesListed: 2, listedSeen: true, devices: twoDevices, done: true},
		},
		{
			name:   "wc pads the device count",
			stdout: strings.Replace(slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 1}), "DEVICES_LISTED=2", "DEVICES_LISTED=       2", 1),
			want: slurmGPUProbe{node: "slinky-0", cudaVisibleDevices: "0", nvsmiSeen: true,
				gpuUUIDs: []string{testGPUUUID}, devicesListed: 2, listedSeen: true, devices: twoDevices, done: true},
		},
		{
			name: "slurm and prolog noise is ignored",
			stdout: "srun: job 42 queued and waiting for resources\nexport FOO=bar\n" +
				slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 1}),
			want: slurmGPUProbe{node: "slinky-0", cudaVisibleDevices: "0", nvsmiSeen: true,
				gpuUUIDs: []string{testGPUUUID}, devicesListed: 2, listedSeen: true, devices: twoDevices, done: true},
		},
		{
			name: "nvidia-smi warning line is not a GPU",
			stdout: slurmGPUProbeStdout("slinky-0", 0,
				[]string{"WARNING: infoROM is corrupted at gpu 0000:3B:00.0", testGPULine}, []int{0, 1}),
			want: slurmGPUProbe{node: "slinky-0", cudaVisibleDevices: "0", nvsmiSeen: true,
				gpuUUIDs: []string{testGPUUUID}, devicesListed: 2, listedSeen: true, devices: twoDevices, done: true},
		},
		{
			name:   "MIG mode enabled is counted",
			stdout: slurmGPUProbeStdout("slinky-0", 0, []string{testGPUUUID + ", Enabled"}, []int{0, 1}),
			want: slurmGPUProbe{node: "slinky-0", cudaVisibleDevices: "0", nvsmiSeen: true,
				gpuUUIDs: []string{testGPUUUID}, migEnabledGPUs: 1, devicesListed: 2, listedSeen: true, devices: twoDevices, done: true},
		},
		{
			name:   "CRLF line endings",
			stdout: strings.ReplaceAll(slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 1}), "\n", "\r\n"),
			want: slurmGPUProbe{node: "slinky-0", cudaVisibleDevices: "0", nvsmiSeen: true,
				gpuUUIDs: []string{testGPUUUID}, devicesListed: 2, listedSeen: true, devices: twoDevices, done: true},
		},
		{
			name:    "repeated NODE means more than one task ran",
			stdout:  "NODE=slinky-0\nNODE=slinky-1\n",
			wantErr: "GPU probe output repeats NODE",
		},
		{
			name:    "non-integer NVSMI_RC",
			stdout:  "NVSMI_RC=x\n",
			wantErr: `GPU probe NVSMI_RC "x" is not an integer`,
		},
		{
			name:    "GPU line without mig.mode.current",
			stdout:  "NVSMI_LINE=GPU-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0\n",
			wantErr: `nvidia-smi line "GPU-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0" is not "<uuid>, <mig.mode.current>"`,
		},
		{
			name:    "non-integer device count",
			stdout:  "DEVICES_LISTED=two\n",
			wantErr: `GPU probe DEVICES_LISTED "two" is not a non-negative integer`,
		},
		{
			name:    "DEV line without errno",
			stdout:  "DEV=/dev/nvidia0\n",
			wantErr: `malformed GPU probe line "DEV=/dev/nvidia0"`,
		},
		{
			name:    "negative errno",
			stdout:  "DEV=/dev/nvidia0 -1\n",
			wantErr: `malformed GPU probe line "DEV=/dev/nvidia0 -1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSlurmGPUProbe(tt.stdout)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
					t.Fatalf("error = %v, want ErrCodeInternal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parsed = %+v\nwant     %+v", got, tt.want)
			}
		})
	}
}

func TestEvaluateAllocatedGPUProbe(t *testing.T) {
	ok := slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, epermExcept(0))
	tests := []struct {
		name    string
		stdout  string
		wantErr string
	}{
		{name: "one GPU listed and exactly one device opens", stdout: ok},
		{
			name: "nvidia-smi warning plus one GPU",
			stdout: slurmGPUProbeStdout("slinky-0", 0,
				[]string{"WARNING: infoROM is corrupted at gpu 0000:3B:00.0", testGPULine}, epermExcept(0)),
		},
		{
			name:    "probe did not finish",
			stdout:  strings.Replace(ok, "DONE=1\n", "", 1),
			wantErr: "allocated job (--gpus=1): GPU probe output has no DONE=1 marker, so the probe did not finish",
		},
		{
			name:    "no NVSMI_RC line",
			stdout:  strings.Replace(ok, "NVSMI_RC=0\n", "", 1),
			wantErr: "allocated job (--gpus=1): GPU probe output has no NVSMI_RC line",
		},
		{
			name:    "no DEVICES_LISTED line",
			stdout:  strings.Replace(ok, "DEVICES_LISTED=8\n", "", 1),
			wantErr: "allocated job (--gpus=1): GPU probe output has no DEVICES_LISTED line",
		},
		{
			name:    "DONE is not 1",
			stdout:  strings.Replace(ok, "DONE=1\n", "DONE=0\n", 1),
			wantErr: "allocated job (--gpus=1): GPU probe output has no DONE=1 marker",
		},
		{
			name:    "probe aborted",
			stdout:  ok + "PROBE_ERROR=perl not found\n",
			wantErr: "allocated job (--gpus=1): GPU probe aborted: perl not found",
		},
		{
			name:    "SLURMD_NODENAME unset",
			stdout:  slurmGPUProbeStdout("", 0, []string{testGPULine}, epermExcept(0)),
			wantErr: `allocated job (--gpus=1): GPU probe reported Slurm node "", want a single node name`,
		},
		{
			name:    "hostlist expression as node",
			stdout:  slurmGPUProbeStdout("slinky-[0-1]", 0, []string{testGPULine}, epermExcept(0)),
			wantErr: `allocated job (--gpus=1): GPU probe reported Slurm node "slinky-[0-1]", want a single node name`,
		},
		{
			name:    "perl dropped a device",
			stdout:  strings.Replace(ok, "DEV=/dev/nvidia7 1\n", "", 1),
			wantErr: "allocated job (--gpus=1): GPU probe reported 7 of 8 listed GPU device nodes",
		},
		{
			name:    "nvidia-smi not found",
			stdout:  slurmGPUProbeStdout("slinky-0", 127, []string{"/bin/sh: 5: nvidia-smi: not found"}, epermExcept(0)),
			wantErr: "allocated job (--gpus=1): nvidia-smi exited 127, so the job cannot use its allocated GPU",
		},
		{
			name:    "no GPU listed",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, nil, epermExcept(0)),
			wantErr: "allocated job (--gpus=1): nvidia-smi listed 0 GPUs, want exactly 1",
		},
		{
			name:    "two GPUs listed",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine, "GPU-ffffffff-0000-1111-2222-333333333333, Disabled"}, epermExcept(0)),
			wantErr: "allocated job (--gpus=1): nvidia-smi listed 2 GPUs, want exactly 1",
		},
		{
			name:    "MIG device listed",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, []string{testGPUUUID + ", Enabled"}, epermExcept(0)),
			wantErr: "allocated job (--gpus=1): nvidia-smi reports MIG mode enabled on 1 GPU(s); this check verifies whole-GPU allocation only",
		},
		{
			name:    "no confinement: every device opens",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 0, 0, 0, 0, 0, 0, 0}),
			wantErr: "allocated job (--gpus=1): opened 8 of 8 GPU device nodes, want exactly 1",
		},
		{
			name:    "allocation reached no device",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, epermExcept(-1)),
			wantErr: "allocated job (--gpus=1): opened 0 of 8 GPU device nodes, want exactly 1",
		},
		{
			name:    "ENXIO on another minor",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 6, 1, 1, 1, 1, 1, 1}),
			wantErr: "allocated job (--gpus=1): unexpected errno opening GPU device nodes (/dev/nvidia1 errno 6); only EPERM counts as denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe, err := parseSlurmGPUProbe(tt.stdout)
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			err = evaluateAllocatedGPUProbe(probe)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEvaluateUnallocatedGPUProbe(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		wantErr string
	}{
		{
			name:   "every device listed and refused with EPERM",
			stdout: slurmGPUProbeStdout("slinky-0", 6, []string{"No devices were found"}, epermExcept(-1)),
		},
		{
			name:   "no device nodes listed at all",
			stdout: slurmGPUProbeStdout("slinky-0", 127, []string{"/bin/sh: 5: nvidia-smi: not found"}, nil),
		},
		{
			name:    "ran on another node",
			stdout:  slurmGPUProbeStdout("slinky-1", 6, nil, epermExcept(-1)),
			wantErr: `unallocated job (no GPU request) ran on Slurm node "slinky-1", want "slinky-0" (the allocated job's node)`,
		},
		{
			name:    "isolation broken: a device opens",
			stdout:  slurmGPUProbeStdout("slinky-0", 6, nil, epermExcept(2)),
			wantErr: "unallocated job (no GPU request) opened 1 GPU device node(s) without a GPU allocation: Slurm GPU isolation is broken",
		},
		{
			name:    "ENXIO is inconclusive",
			stdout:  slurmGPUProbeStdout("slinky-0", 6, nil, []int{1, 6, 1, 1}),
			wantErr: "unallocated job (no GPU request): unexpected errno opening GPU device nodes (/dev/nvidia1 errno 6); only EPERM counts as denied, so isolation is unproven",
		},
		{
			name:    "EACCES is inconclusive until a live run says otherwise",
			stdout:  slurmGPUProbeStdout("slinky-0", 6, nil, []int{13, 13}),
			wantErr: "(/dev/nvidia0 errno 13, /dev/nvidia1 errno 13)",
		},
		{
			name:    "NVML lists a GPU without an allocation",
			stdout:  slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, epermExcept(-1)),
			wantErr: "unallocated job (no GPU request): nvidia-smi listed 1 GPU(s) without a GPU allocation: Slurm GPU isolation is broken",
		},
		{
			name:    "NVML lists a MIG device without an allocation",
			stdout:  slurmGPUProbeStdout("slinky-0", 6, []string{testGPUUUID + ", Enabled"}, epermExcept(-1)),
			wantErr: "unallocated job (no GPU request): nvidia-smi listed 1 GPU(s) without a GPU allocation: Slurm GPU isolation is broken",
		},
		{
			name:    "probe did not finish",
			stdout:  strings.Replace(slurmGPUProbeStdout("slinky-0", 6, nil, epermExcept(-1)), "DONE=1\n", "", 1),
			wantErr: "unallocated job (no GPU request): GPU probe output has no DONE=1 marker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe, err := parseSlurmGPUProbe(tt.stdout)
			if err != nil {
				t.Fatalf("parse error = %v", err)
			}
			err = evaluateUnallocatedGPUProbe(probe, "slinky-0")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestFormatSlurmGPUProbe(t *testing.T) {
	probe, err := parseSlurmGPUProbe(slurmGPUProbeStdout("slinky-0", 0, []string{testGPULine}, []int{0, 1, 6}))
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	tests := []struct {
		name  string
		probe slurmGPUProbe
		want  string
	}{
		{
			name:  "allocated job with one open, one denied and one inconclusive minor",
			probe: probe,
			want:  "node=slinky-0 nvidia-smi exit=0 GPUs=[GPU-0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0] MIG-enabled=0 CUDA_VISIBLE_DEVICES=0 device nodes listed=3 opened=1 denied(EPERM)=1 other=[/dev/nvidia2 errno 6]",
		},
		{
			name:  "empty probe reports unknown node and devices",
			probe: slurmGPUProbe{},
			want:  "node=unknown nvidia-smi exit=0 GPUs=[] MIG-enabled=0 CUDA_VISIBLE_DEVICES=unknown device nodes listed=0 opened=0 denied(EPERM)=0 other=[]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSlurmGPUProbe(tt.probe); got != tt.want {
				t.Fatalf("formatSlurmGPUProbe = %q\nwant                 %q", got, tt.want)
			}
		})
	}
}

// The shipped probe must be byte-identical to the copy the PR's offline
// Docker smoke runs (Task 6); editing one without the other fails here.
func TestSlinkySlurmGPUProbeShellMatchesOfflineVerifiedCopy(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "slinky_slurm_gpu_probe.sh"))
	if err != nil {
		t.Fatalf("read testdata copy: %v", err)
	}
	if string(want) != slinkySlurmGPUProbeShell {
		t.Fatalf("slinkySlurmGPUProbeShell differs from testdata/slinky_slurm_gpu_probe.sh;\nconst:\n%s\ntestdata:\n%s",
			slinkySlurmGPUProbeShell, want)
	}
}

// writeProbeShim writes an executable sh script named name into a fresh
// directory and returns the directory, for prepending to PATH.
func writeProbeShim(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write %s shim: %v", name, err)
	}
	return dir
}

// TestSlinkySlurmGPUProbeShellRuns executes the shipped probe under /bin/sh
// with PATH shims for nvidia-smi and find, so a guard lost from the shell
// itself fails here, without Docker or a GPU.
func TestSlinkySlurmGPUProbeShellRuns(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skipf("perl is not on PATH (%v); the probe shell needs perl, which the slurmd runtime image provides but this host does not", err)
	}
	tests := []struct {
		name        string
		failingFind bool
		wantExit    int
	}{
		{name: "reports node, GPUs and every device node it lists", wantExit: 0},
		{name: "a failing find aborts the probe instead of reporting no devices", failingFind: true, wantExit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pathDirs := []string{writeProbeShim(t, "nvidia-smi", "printf '%s\\n' '"+testGPULine+"'")}
			if tt.failingFind {
				pathDirs = append([]string{writeProbeShim(t, "find", "exit 1")}, pathDirs...)
			}
			pathDirs = append(pathDirs, os.Getenv("PATH"))

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", slinkySlurmGPUProbeShell)
			cmd.Env = append(os.Environ(), "SLURMD_NODENAME=slinky-0",
				"PATH="+strings.Join(pathDirs, string(os.PathListSeparator)))
			out, runErr := cmd.Output()
			stdout := string(out)
			exitCode := 0
			if runErr != nil {
				var exitErr *exec.ExitError
				if !stderrors.As(runErr, &exitErr) {
					t.Fatalf("run probe: %v", runErr)
				}
				exitCode = exitErr.ExitCode()
			}
			if exitCode != tt.wantExit {
				t.Fatalf("probe exit code = %d, want %d\nstdout:\n%s", exitCode, tt.wantExit, stdout)
			}
			probe, err := parseSlurmGPUProbe(stdout)
			if err != nil {
				t.Fatalf("parse error = %v\nstdout:\n%s", err, stdout)
			}

			if tt.failingFind {
				if !strings.Contains(stdout, "PROBE_ERROR=find failed\n") || strings.Contains(stdout, "DONE=1") {
					t.Fatalf("stdout = %q, want PROBE_ERROR=find failed and no DONE=1", stdout)
				}
				err = evaluateUnallocatedGPUProbe(probe, "slinky-0")
				want := "unallocated job (no GPU request): GPU probe aborted: find failed"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("evaluateUnallocatedGPUProbe error = %v, want containing %q", err, want)
				}
				return
			}

			if probe.node != "slinky-0" {
				t.Fatalf("node = %q, want %q", probe.node, "slinky-0")
			}
			if !probe.nvsmiSeen || probe.nvsmiExit != 0 {
				t.Fatalf("nvsmiSeen = %t nvsmiExit = %d, want true and 0", probe.nvsmiSeen, probe.nvsmiExit)
			}
			if !reflect.DeepEqual(probe.gpuUUIDs, []string{testGPUUUID}) {
				t.Fatalf("gpuUUIDs = %q, want [%q]", probe.gpuUUIDs, testGPUUUID)
			}
			if probe.migEnabledGPUs != 0 {
				t.Fatalf("migEnabledGPUs = %d, want 0", probe.migEnabledGPUs)
			}
			// GPU CI hosts list /dev/nvidiaN, so the count is not pinned to 0.
			if !probe.listedSeen || len(probe.devices) != probe.devicesListed {
				t.Fatalf("listedSeen = %t, %d DEV lines for DEVICES_LISTED=%d, want every listed node reported",
					probe.listedSeen, len(probe.devices), probe.devicesListed)
			}
			if !probe.done || probe.probeError != "" {
				t.Fatalf("done = %t probeError = %q, want DONE=1 and no PROBE_ERROR", probe.done, probe.probeError)
			}
		})
	}
}

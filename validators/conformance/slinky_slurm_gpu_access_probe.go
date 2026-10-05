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
	"regexp"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// slinkySlurmGPUProbeShell runs as a Slurm job step on a slurmd pod and prints
// what the step can reach as KEY=value lines: the Slurm node name, the GPUs
// NVML enumerates with each one's mig.mode.current, and the errno of a
// read-only open of every GPU minor device node (0 when the open succeeded).
// It decides nothing; the evaluate functions do. Slurm confines devices with
// the cgroup device controller, which leaves /dev/nvidiaN listed and refuses
// open() with EPERM, so a listed node is not evidence of access. The name filter selects GPU minors only (nvidiactl,
// nvidia-uvm and nvidia-modeset do not match), and the script needs perl,
// which the pinned slurmd-pyxis image ships. Commands whose failure would
// erase evidence (find, the nvidia-smi line split) run outside pipelines and
// under a guard, so their failure aborts the probe.
const slinkySlurmGPUProbeShell = nvidiaUserlandPathPrologue + `
printf 'NODE=%s\n' "${SLURMD_NODENAME:-}"
printf 'CUDA_VISIBLE_DEVICES=%s\n' "${CUDA_VISIBLE_DEVICES-<unset>}"
if out="$(nvidia-smi --query-gpu=uuid,mig.mode.current --format=csv,noheader 2>&1)"; then rc=0; else rc=$?; fi
printf 'NVSMI_RC=%s\n' "$rc"
while IFS= read -r line; do printf 'NVSMI_LINE=%s\n' "$line"; done <<NVSMI_OUT || { printf 'PROBE_ERROR=nvidia-smi line split failed\n'; exit 1; }
$out
NVSMI_OUT
command -v perl >/dev/null 2>&1 || { printf 'PROBE_ERROR=perl not found\n'; exit 1; }
devs="$(find /dev -maxdepth 1 -type c -name 'nvidia[0-9]*')" || { printf 'PROBE_ERROR=find failed\n'; exit 1; }
if [ -n "$devs" ]; then n="$(printf '%s\n' "$devs" | wc -l)"; else n=0; fi
printf 'DEVICES_LISTED=%s\n' "$n"
if [ -n "$devs" ]; then
  printf '%s\n' "$devs" | perl -ne 'chomp; if (sysopen(my $fh, $_, 0)) { close($fh); print "DEV=$_ 0\n" } else { print "DEV=$_ ", $!+0, "\n" }' || { printf 'PROBE_ERROR=perl failed\n'; exit 1; }
fi
printf 'DONE=1\n'
`

// Errno values the probe reports for a GPU device open.
const (
	slurmGPUOpenSucceeded = 0
	slurmGPUOpenEPERM     = 1
)

// Job labels used in every rule message.
const (
	slurmGPUAllocatedJob   = "allocated job (--gpus=1)"
	slurmGPUUnallocatedJob = "unallocated job (no GPU request)"
)

// slurmNodeNamePattern admits one Slurm node name and rejects hostlist
// expressions such as "slinky-[0-1]", which --nodelist would expand.
var slurmNodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// slurmGPUDeviceOpen is one DEV line: a GPU minor device node and the errno of
// opening it read-only.
type slurmGPUDeviceOpen struct {
	path  string
	errno int
}

// slurmGPUProbe is the parsed output of one slinkySlurmGPUProbeShell run.
type slurmGPUProbe struct {
	node               string
	cudaVisibleDevices string
	nvsmiExit          int
	nvsmiSeen          bool
	gpuUUIDs           []string
	migEnabledGPUs     int
	devicesListed      int
	listedSeen         bool
	devices            []slurmGPUDeviceOpen
	probeError         string
	done               bool
}

// parseSlurmGPUProbe parses probe stdout. Lines without "=" and unknown keys
// are ignored (srun and prolog output share the stream); a repeated
// single-valued key means more than one task ran the probe and is an error.
func parseSlurmGPUProbe(stdout string) (slurmGPUProbe, error) {
	var p slurmGPUProbe
	seen := map[string]bool{}
	for raw := range strings.SplitSeq(stdout, "\n") {
		line := strings.TrimRight(raw, "\r")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "NODE", "CUDA_VISIBLE_DEVICES", "NVSMI_RC", "DEVICES_LISTED", "PROBE_ERROR", "DONE":
			if seen[key] {
				return slurmGPUProbe{}, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("GPU probe output repeats %s; more than one task ran the probe", key))
			}
			seen[key] = true
		}
		switch key {
		case "NODE":
			p.node = strings.TrimSpace(value)
		case "CUDA_VISIBLE_DEVICES":
			p.cudaVisibleDevices = value
		case "NVSMI_RC":
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return slurmGPUProbe{}, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("GPU probe NVSMI_RC %q is not an integer", value), err)
			}
			p.nvsmiExit = n
			p.nvsmiSeen = true
		case "NVSMI_LINE":
			entry := strings.TrimSpace(value)
			if !strings.HasPrefix(entry, "GPU-") {
				continue
			}
			fields := strings.Split(entry, ",")
			if len(fields) != 2 {
				return slurmGPUProbe{}, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf(`nvidia-smi line %q is not "<uuid>, <mig.mode.current>"`, entry))
			}
			p.gpuUUIDs = append(p.gpuUUIDs, strings.TrimSpace(fields[0]))
			if strings.TrimSpace(fields[1]) == "Enabled" {
				p.migEnabledGPUs++
			}
		case "DEVICES_LISTED":
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return slurmGPUProbe{}, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("GPU probe DEVICES_LISTED %q is not a non-negative integer", value))
			}
			p.devicesListed = n
			p.listedSeen = true
		case "DEV":
			path, errnoText, ok := strings.Cut(strings.TrimSpace(value), " ")
			errno, err := strconv.Atoi(strings.TrimSpace(errnoText))
			if !ok || path == "" || err != nil || errno < 0 {
				return slurmGPUProbe{}, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("malformed GPU probe line %q", line))
			}
			p.devices = append(p.devices, slurmGPUDeviceOpen{path: path, errno: errno})
		case "PROBE_ERROR":
			p.probeError = strings.TrimSpace(value)
		case "DONE":
			p.done = strings.TrimSpace(value) == "1"
		}
	}
	return p, nil
}

func (p slurmGPUProbe) checkComplete(job string) error {
	switch {
	case p.probeError != "":
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("%s: GPU probe aborted: %s", job, p.probeError))
	case !p.done:
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s: GPU probe output has no DONE=1 marker, so the probe did not finish", job))
	case !slurmNodeNamePattern.MatchString(p.node):
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s: GPU probe reported Slurm node %q, want a single node name", job, p.node))
	case !p.nvsmiSeen:
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("%s: GPU probe output has no NVSMI_RC line", job))
	case !p.listedSeen:
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("%s: GPU probe output has no DEVICES_LISTED line", job))
	case len(p.devices) != p.devicesListed:
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s: GPU probe reported %d of %d listed GPU device nodes", job, len(p.devices), p.devicesListed))
	}
	return nil
}

// openCounts splits device opens into succeeded, refused with EPERM, and any
// other errno, which is never counted as denied.
func (p slurmGPUProbe) openCounts() (int, int, []string) {
	var opened, denied int
	var unexpected []string
	for _, d := range p.devices {
		switch d.errno {
		case slurmGPUOpenSucceeded:
			opened++
		case slurmGPUOpenEPERM:
			denied++
		default:
			unexpected = append(unexpected, fmt.Sprintf("%s errno %d", d.path, d.errno))
		}
	}
	return opened, denied, unexpected
}

// evaluateAllocatedGPUProbe passes only when the one-GPU job lists exactly one
// GPU through NVML and can open exactly one GPU minor, every other minor
// refused with EPERM.
func evaluateAllocatedGPUProbe(p slurmGPUProbe) error {
	if err := p.checkComplete(slurmGPUAllocatedJob); err != nil {
		return err
	}
	if p.nvsmiExit != 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: nvidia-smi exited %d, so the job cannot use its allocated GPU", slurmGPUAllocatedJob, p.nvsmiExit))
	}
	if p.migEnabledGPUs > 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: nvidia-smi reports MIG mode enabled on %d GPU(s); this check verifies whole-GPU allocation only",
			slurmGPUAllocatedJob, p.migEnabledGPUs))
	}
	if len(p.gpuUUIDs) != 1 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: nvidia-smi listed %d GPUs, want exactly 1", slurmGPUAllocatedJob, len(p.gpuUUIDs)))
	}
	opened, _, unexpected := p.openCounts()
	if len(unexpected) > 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: unexpected errno opening GPU device nodes (%s); only EPERM counts as denied",
			slurmGPUAllocatedJob, strings.Join(unexpected, ", ")))
	}
	if opened != 1 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: opened %d of %d GPU device nodes, want exactly 1 (0: the allocation reached no device; more than 1: Slurm is not confining the job)",
			slurmGPUAllocatedJob, opened, len(p.devices)))
	}
	return nil
}

// evaluateUnallocatedGPUProbe passes only when the job ran on wantNode, opened
// no GPU minor, saw every refusal as EPERM, and NVML listed no GPU. A node with
// no GPU minors listed passes: wantNode already proved a GPU opens there when
// allocated.
func evaluateUnallocatedGPUProbe(p slurmGPUProbe, wantNode string) error {
	if err := p.checkComplete(slurmGPUUnallocatedJob); err != nil {
		return err
	}
	if p.node != wantNode {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s ran on Slurm node %q, want %q (the allocated job's node)", slurmGPUUnallocatedJob, p.node, wantNode))
	}
	opened, _, unexpected := p.openCounts()
	if opened > 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s opened %d GPU device node(s) without a GPU allocation: Slurm GPU isolation is broken",
			slurmGPUUnallocatedJob, opened))
	}
	if len(unexpected) > 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: unexpected errno opening GPU device nodes (%s); only EPERM counts as denied, so isolation is unproven",
			slurmGPUUnallocatedJob, strings.Join(unexpected, ", ")))
	}
	if listed := len(p.gpuUUIDs); listed > 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s: nvidia-smi listed %d GPU(s) without a GPU allocation: Slurm GPU isolation is broken",
			slurmGPUUnallocatedJob, listed))
	}
	return nil
}

// formatSlurmGPUProbe renders one probe for the evidence summary.
func formatSlurmGPUProbe(p slurmGPUProbe) string {
	opened, denied, unexpected := p.openCounts()
	return fmt.Sprintf(
		"node=%s nvidia-smi exit=%d GPUs=%v MIG-enabled=%d CUDA_VISIBLE_DEVICES=%s device nodes listed=%d opened=%d denied(EPERM)=%d other=%v",
		valueOrUnknown(p.node), p.nvsmiExit, p.gpuUUIDs, p.migEnabledGPUs, valueOrUnknown(p.cudaVisibleDevices),
		p.devicesListed, opened, denied, unexpected)
}

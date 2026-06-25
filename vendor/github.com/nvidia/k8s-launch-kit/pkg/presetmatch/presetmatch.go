// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

// Package presetmatch is the single entry point for comparing a
// discovered cluster group against the topology-preset catalog.
//
// Both `l8k discover` (at discovery time, when preset data is also
// applied into the group via presets.ApplyPreset) and `l8k validate`
// (at validation time, to confirm that the cluster's recorded hardware
// still matches the certified preset) call MatchGroup / MatchAll
// here. Keeping the lookup logic in one place means new lookup
// behaviour (fuzzy matching, preset-version awareness, …) lands in
// one file and both call sites pick it up.
//
// MatchGroup does not mutate the input group. Callers that want to
// also enrich the group's transient topology fields call
// presets.ApplyPreset themselves (typically only the discovery path
// does that).
package presetmatch

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
)

// Status enumerates the outcomes of a per-group preset lookup.
type Status string

const (
	// StatusMatch — preset found and every comparison field
	// matches the discovered hardware exactly.
	StatusMatch Status = "match"
	// StatusDeviation — preset found but the hardware drifts on at
	// least one field (PF count, PCI addresses, device IDs).
	// Deviations are informational: the deployment can still run
	// correctly against drifted hardware, just not against the
	// certified preset.
	StatusDeviation Status = "deviation"
	// StatusNotFound — no preset matches the (machineType, gpuType)
	// pair. Most common reason: the pair hasn't been catalogued
	// yet, or discovery didn't populate machineType / gpuType
	// (e.g. running on hardware the GPU operator labels don't
	// cover).
	StatusNotFound Status = "not-found"
	// StatusSkipped — the lookup couldn't even be attempted (e.g.
	// machineType or gpuType empty on the group). Distinct from
	// StatusNotFound so callers can render "discovery is
	// incomplete" rather than "no preset for this hardware."
	StatusSkipped Status = "skipped"
)

// Result describes one group's preset-lookup outcome. Surfaced
// verbatim by validate's text/JSON output and the HTML report. The
// Deviations slice mirrors config.PresetDeviationEntry so callers
// that already render the existing presetDeviation field keep
// working without translation.
type Result struct {
	Group       string
	MachineType string
	GPUType     string
	// Manufacturer is propagated from the matched preset's
	// topology.yaml when a preset was found (StatusMatch /
	// StatusDeviation). Empty otherwise. Surfaced in the user-
	// facing "server type" label as the leading segment of
	// <manufacturer>-<machineType>-<gpuType>.
	Manufacturer string
	Status       Status
	// PresetName is the catalog directory name (what `l8k preset
	// list` prints) when a preset was found. Empty otherwise.
	PresetName string
	// Reason carries a short human-readable explanation when
	// Status is StatusNotFound or StatusSkipped, and a one-line
	// summary when StatusDeviation ("3 deviation(s) — pfCount,
	// pciAddress, deviceID"). Empty when StatusMatch.
	Reason     string
	Deviations []config.PresetDeviationEntry
	// Preset is the loaded topology that produced the match. Used
	// downstream to enrich "missing PCI" rows in the validation
	// report with the expected deviceID / rail / netdev when the
	// cluster doesn't have a device the certified topology
	// expects. nil when Status is NotFound or Skipped.
	Preset *presets.Topology
}

// MatchGroup runs the preset lookup + comparison for one group.
// Does not mutate the group; callers that also want to enrich
// rail/NUMA topology fields invoke presets.ApplyPreset separately.
//
// Lookup is exact-match on (machineType, gpuType). Empty fields
// short-circuit to StatusSkipped — without those values the preset
// catalog can't be queried, and we don't fall back to fuzzy matching
// because picking the wrong preset would silently rewrite the
// deployment to target the wrong hardware shape.
func MatchGroup(group config.ClusterConfig) Result {
	res := Result{
		Group:       group.Identifier,
		MachineType: group.MachineType,
		GPUType:     group.GPUType,
	}
	if group.MachineType == "" || group.GPUType == "" {
		res.Status = StatusSkipped
		switch {
		case group.MachineType == "" && group.GPUType == "":
			res.Reason = "machineType and gpuType not discovered on group — `l8k discover` did not populate them"
		case group.MachineType == "":
			res.Reason = "machineType not discovered on group"
		default:
			res.Reason = "gpuType not discovered on group"
		}
		return res
	}
	preset, err := presets.LoadPreset(group.MachineType, group.GPUType)
	if err != nil {
		res.Status = StatusNotFound
		res.Reason = fmt.Sprintf("preset lookup failed: %v", err)
		return res
	}
	if preset == nil {
		res.Status = StatusNotFound
		res.Reason = fmt.Sprintf("no preset matches (%s, %s) in the local presets directory", group.MachineType, group.GPUType)
		return res
	}
	deviations := presets.ValidatePreset(preset, group.PFs)
	res.PresetName = preset.MachineType + "/" + preset.GPUType
	res.Manufacturer = preset.Manufacturer
	res.Deviations = deviations
	res.Preset = preset
	if len(deviations) == 0 {
		res.Status = StatusMatch
		return res
	}
	res.Status = StatusDeviation
	res.Reason = fmt.Sprintf("%d deviation(s) from matched preset", len(deviations))
	return res
}

// MatchAll runs MatchGroup over every entry in cfg.ClusterConfig and
// returns the results in the same order. cfg is never mutated.
func MatchAll(cfg *config.LaunchKitConfig) []Result {
	if cfg == nil {
		return nil
	}
	out := make([]Result, 0, len(cfg.ClusterConfig))
	for _, g := range cfg.ClusterConfig {
		out = append(out, MatchGroup(g))
	}
	return out
}

// AnyMatched reports whether any of the results found AND fully
// matched a preset. Used by the validate CLI to decide whether the
// "preset" check is worth surfacing at all (vs. hidden when no
// presets exist for the cluster's hardware).
func AnyMatched(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusMatch || r.Status == StatusDeviation {
			return true
		}
	}
	return false
}

// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package presets

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// embeddedPresets is the in-binary copy of the preset tree, rooted at "data/".
// Library callers (and the CLI without a checked-out repo) get a working
// preset catalog without any filesystem layout on the host. A disk override
// is honored when present — see resolvePresetsFS.
//
//go:embed all:data
var embeddedPresets embed.FS

// Topology represents a predefined cluster topology preset loaded from a
// topology.yaml file. It is intentionally separate from config.ClusterConfig
// to carry additional topology metadata (NUMA, GPU affinity) without
// polluting the core config.
//
// Both MachineType and GPUType are required. A topology.yaml that omits either
// is rejected at load time so downstream code can rely on them as matching keys.
type Topology struct {
	MachineType     string                      `yaml:"machineType"`
	Manufacturer    string                      `yaml:"manufacturer,omitempty"`
	GPUType         string                      `yaml:"gpuType"`
	NicModel        string                      `yaml:"nicModel,omitempty"`
	GPUInterconnect string                      `yaml:"gpuInterconnect,omitempty"`
	NumaNodes       int                         `yaml:"numaNodes,omitempty"`
	Capabilities    *config.ClusterCapabilities `yaml:"capabilities,omitempty"`
	PFs             []PresetPF                  `yaml:"pfs"`
}

// PresetPF describes a single physical function in a topology preset.
// It is a superset of config.PFConfig with additional topology fields.
type PresetPF struct {
	DeviceID         string `yaml:"deviceID"`
	PciAddress       string `yaml:"pciAddress"`
	RdmaDevice       string `yaml:"rdmaDevice"`
	NetworkInterface string `yaml:"networkInterface"`
	Traffic          string `yaml:"traffic"`
	Rail             *int   `yaml:"rail,omitempty"`
	NumaNode         *int   `yaml:"numaNode,omitempty"`
	ConnectedGPU     string `yaml:"connectedGPU,omitempty"`
	GPUProximity     string `yaml:"gpuProximity,omitempty"`
	PSID             string `yaml:"psid,omitempty"`
	PartNumber       string `yaml:"partNumber,omitempty"`
}

// presetEntry pairs a parsed topology with the directory name that produced it.
// The directory name is the user-visible identifier (used by --for and
// `l8k preset list`); the topology fields are the matching keys used by
// LoadPreset.
type presetEntry struct {
	DirName  string
	Topology Topology
}

// GetPresetsDir resolves an on-disk presets directory using the lookup chain:
// 1. ./presets (CWD — container/repo root)
// 2. /usr/local/share/l8k/presets (default install)
// 3. <binary-dir>/../share/l8k/presets (custom prefix install)
//
// Returns ("", nil) if no on-disk presets directory is found. Callers do NOT
// need an on-disk directory to use this package — see resolvePresetsFS for
// the embedded-FS fallback that powers library callers.
func GetPresetsDir() (string, error) {
	candidates := []string{
		"presets",
		"/usr/local/share/l8k/presets",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "share", "l8k", "presets"))
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return p, nil
		}
	}
	return "", nil
}

// presetsSource describes the FS-rooted preset tree loadAllPresets walks. It
// carries enough metadata for diagnostics to point operators at the right
// place ("./presets" vs "embedded") when a preset is rejected.
type presetsSource struct {
	fs.FS
	// label is how this source is referenced in user-facing log lines —
	// either a filesystem path ("./presets", "/usr/local/share/l8k/presets")
	// or the sentinel "embedded".
	label string
	// embedded is true when this source is the in-binary copy; only used to
	// gate path-shaped diagnostics (skipped entries record a relative path
	// rather than an absolute one).
	embedded bool
}

// resolvePresetsFS chooses the preset tree to load against. Disk wins over
// embedded — an on-disk presets directory found via GetPresetsDir is the
// user's override, kept ahead of the binary's baked-in copy so `l8k preset
// update` and curated per-machine trees keep working. With no on-disk dir,
// the embedded copy is used.
func resolvePresetsFS() (*presetsSource, error) {
	dir, err := GetPresetsDir()
	if err != nil {
		return nil, err
	}
	if dir != "" {
		return &presetsSource{FS: os.DirFS(dir), label: dir}, nil
	}
	sub, err := fs.Sub(embeddedPresets, "data")
	if err != nil {
		return nil, fmt.Errorf("failed to open embedded presets FS: %w", err)
	}
	return &presetsSource{FS: sub, label: "embedded", embedded: true}, nil
}

// SkippedPreset describes a preset directory that loadAllPresets rejected.
// It is surfaced by l8k preset list so the user can see WHY a preset they
// expect to find isn't appearing — silent skipping would otherwise look
// indistinguishable from "no presets installed".
type SkippedPreset struct {
	DirName string
	Path    string
	Reason  string
}

// loadAllPresets returns every valid preset under the resolved preset source
// (disk if present, embedded otherwise), sorted by directory name, plus a
// list of skipped entries with reasons. Invalid entries (parse error,
// missing machineType, missing gpuType) are skipped rather than failing the
// whole load — keeps the lookup robust to stale or partial preset
// directories. Returns (nil, nil, nil) when neither a disk presets dir nor
// the embedded FS yields any entries (only happens in a misconfigured build).
func loadAllPresets() ([]presetEntry, []SkippedPreset, error) {
	src, err := resolvePresetsFS()
	if err != nil {
		return nil, nil, err
	}
	if src == nil {
		return nil, nil, nil
	}

	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read presets source %s: %w", src.label, err)
	}

	var out []presetEntry
	var skipped []SkippedPreset
	skip := func(name, path, reason string) {
		log.Log.V(1).Info("Skipping preset", "preset", name, "path", path, "reason", reason)
		skipped = append(skipped, SkippedPreset{DirName: name, Path: path, Reason: reason})
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// path.Join (not filepath.Join) because fs.FS contract requires
		// forward-slash separators regardless of host OS. Harmless on
		// Linux where filepath.Separator == '/', but the right helper.
		topoPath := path.Join(e.Name(), "topology.yaml")
		data, err := fs.ReadFile(src, topoPath)
		if err != nil {
			// errors.Is(err, fs.ErrNotExist) is the fs-style predicate
			// that mirrors how we read (fs.ReadFile). Equivalent to
			// os.IsNotExist on disk-backed sources, but stylistically
			// consistent with the embedded-FS path.
			if errors.Is(err, fs.ErrNotExist) {
				// No topology.yaml — silent skip; the directory just
				// isn't a preset and that's expected (e.g. README).
				continue
			}
			skip(e.Name(), src.diagPath(topoPath), fmt.Sprintf("read failed: %v", err))
			continue
		}

		var t Topology
		if err := yaml.Unmarshal(data, &t); err != nil {
			skip(e.Name(), src.diagPath(topoPath), fmt.Sprintf("parse failed: %v", err))
			continue
		}
		if t.MachineType == "" {
			skip(e.Name(), src.diagPath(topoPath), "missing required field 'machineType'")
			continue
		}
		if t.GPUType == "" {
			// The most common reason today: the YAML still has the old
			// 'productType:' key from before the rename. Mention it
			// explicitly so users know how to fix without reading code.
			skip(e.Name(), src.diagPath(topoPath),
				"missing required field 'gpuType' (old 'productType' key was renamed to 'gpuType' — reinstall presets or rename the key)")
			continue
		}

		out = append(out, presetEntry{DirName: e.Name(), Topology: t})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].DirName < out[j].DirName })
	return out, skipped, nil
}

// diagPath returns a user-facing path for an entry inside this source. For
// disk sources it joins the source dir with the relative path; for the
// embedded source it prefixes "embedded:" so log readers can tell which copy
// produced a skip reason.
func (s *presetsSource) diagPath(rel string) string {
	if s.embedded {
		return "embedded:" + rel
	}
	return filepath.Join(s.label, rel)
}

// LoadPreset returns the topology preset whose YAML declares both machineType
// and gpuType equal to the arguments. Lookup is exact-match — there is no
// "any-GPU" fallback. Returns (nil, nil) when no preset matches or no presets
// directory exists.
func LoadPreset(machineType, gpuType string) (*Topology, error) {
	all, _, err := loadAllPresets()
	if err != nil {
		return nil, err
	}

	var matches []presetEntry
	for _, p := range all {
		if p.Topology.MachineType == machineType && p.Topology.GPUType == gpuType {
			matches = append(matches, p)
		}
	}

	if len(matches) == 0 {
		log.Log.V(1).Info("Preset lookup miss",
			"machineType", machineType, "gpuType", gpuType, "candidatesScanned", len(all))
		return nil, nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.DirName)
		}
		log.Log.V(1).Info("Multiple presets match (machineType, gpuType); picking first by directory name",
			"machineType", machineType, "gpuType", gpuType, "candidates", names, "picked", matches[0].DirName)
	} else {
		log.Log.V(1).Info("Preset lookup hit",
			"machineType", machineType, "gpuType", gpuType, "preset", matches[0].DirName)
	}
	t := matches[0].Topology
	return &t, nil
}

// LoadPresetByDir returns the topology preset stored at <presets-dir>/<dirName>.
// This is the lookup used by `--for`: the user passes a directory name (which
// `l8k preset list` shows) and we hand back the parsed topology. Returns
// (nil, error) with a typed error listing available presets when the directory
// is missing or the preset is invalid.
func LoadPresetByDir(dirName string) (*Topology, error) {
	all, skipped, err := loadAllPresets()
	if err != nil {
		return nil, err
	}

	for _, p := range all {
		if p.DirName == dirName {
			t := p.Topology
			return &t, nil
		}
	}

	// Did the requested name match a preset that loadAllPresets rejected?
	// If so, surface the rejection reason so the user knows it's not just
	// a typo.
	for _, s := range skipped {
		if s.DirName == dirName {
			return nil, fmt.Errorf("preset %q exists but was rejected at load time: %s", dirName, s.Reason)
		}
	}

	available := make([]string, 0, len(all))
	for _, p := range all {
		available = append(available, p.DirName)
	}
	if len(available) == 0 {
		if len(skipped) > 0 {
			return nil, fmt.Errorf("unknown preset %q: %d preset(s) on disk but all were rejected at load time (see warnings); run 'l8k preset list' to inspect",
				dirName, len(skipped))
		}
		return nil, fmt.Errorf("unknown preset %q: no presets are installed (run 'l8k preset update')", dirName)
	}
	return nil, fmt.Errorf("unknown preset %q; available: %v", dirName, available)
}

// ListPresets returns the directory names of all valid presets, sorted.
// Returns nil if no presets directory exists. Invalid presets (missing
// machineType / gpuType, parse failures) are filtered out with a warning.
func ListPresets() ([]string, error) {
	all, _, err := loadAllPresets()
	if err != nil {
		return nil, err
	}
	if all == nil {
		return nil, nil
	}
	names := make([]string, 0, len(all))
	for _, p := range all {
		names = append(names, p.DirName)
	}
	return names, nil
}

// ListPresetSummaries returns a summary row per valid preset: directory name
// plus the (machineType, gpuType) pair declared in its YAML. Used by
// `l8k preset list`.
type PresetSummary struct {
	DirName     string
	MachineType string
	GPUType     string
}

// ListPresetSummaries returns one summary per valid preset (sorted by
// directory name) plus the list of skipped/invalid presets so callers can
// surface rejection reasons.
func ListPresetSummaries() ([]PresetSummary, []SkippedPreset, error) {
	all, skipped, err := loadAllPresets()
	if err != nil {
		return nil, nil, err
	}
	if all == nil && skipped == nil {
		return nil, nil, nil
	}
	out := make([]PresetSummary, 0, len(all))
	for _, p := range all {
		out = append(out, PresetSummary{
			DirName:     p.DirName,
			MachineType: p.Topology.MachineType,
			GPUType:     p.Topology.GPUType,
		})
	}
	return out, skipped, nil
}

// ApplyPreset enriches a discovered ClusterConfig group with data from a
// validated preset. It matches PFs by PCI address and overrides topology-
// derived fields (traffic, rail, NUMA, GPU affinity) while preserving
// live-state fields (RDMA device, network interface, PSID, part number).
// It returns true when the preset was applied.
//
// ApplyPreset is ALL-OR-NOTHING: it applies only when the group and the preset
// describe the same set of PCI addresses (equal PF count and every group PF
// present in the preset). On any partial match it makes NO changes and returns
// false. A partial application is incoherent: a preset's rail numbers are
// authored as one self-consistent set for the whole board, so overlaying them
// onto a subset — or mixing preset-numbered PFs with PFs still carrying their
// live, sequentially-assigned rails — produces gaps or duplicate rail indices,
// and clobbers the correct live traffic classification for whichever PCI
// addresses happen to overlap. The discovery path already gates on
// presets.HasTopologyDeviation, but ApplyPreset enforces the invariant itself
// so it is safe to call from any context.
//
// On a full match the preset's authoritative rail values are copied VERBATIM,
// never renumbered: presets may legitimately number rails out of PCI-address
// order (e.g. multi-PCI-domain layouts where rail 0 lives on domain 0001), and
// that mapping is the certified intent.
//
// GPUType is no longer filled in from the preset: by the time ApplyPreset
// runs, the discovered group's GPUType is what was passed to LoadPreset, so
// it already matches the preset's GPUType exactly.
func ApplyPreset(preset *Topology, group *config.ClusterConfig) bool {
	presetMap := make(map[string]*PresetPF, len(preset.PFs))
	for i := range preset.PFs {
		presetMap[preset.PFs[i].PciAddress] = &preset.PFs[i]
	}

	// Precondition: full PCI bijection between group and preset. Anything
	// short of that can't be applied coherently (see doc comment), so make
	// no changes and report that nothing was applied.
	if len(group.PFs) != len(preset.PFs) {
		log.Log.V(1).Info("Preset not applied — PF count differs from group",
			"groupPFs", len(group.PFs), "presetPFs", len(preset.PFs))
		return false
	}
	for i := range group.PFs {
		if _, ok := presetMap[group.PFs[i].PciAddress]; !ok {
			log.Log.V(1).Info("Preset not applied — group PF absent from preset",
				"pciAddress", group.PFs[i].PciAddress)
			return false
		}
	}

	for i := range group.PFs {
		pf := &group.PFs[i]
		pp := presetMap[pf.PciAddress] // guaranteed present by the check above

		pf.Traffic = pp.Traffic
		pf.Rail = pp.Rail
		pf.NumaNode = pp.NumaNode
		pf.ConnectedGPU = pp.ConnectedGPU
		pf.GPUProximity = pp.GPUProximity

		if pp.PartNumber != "" && pf.PartNumber != "" && pp.PartNumber != pf.PartNumber {
			log.Log.V(1).Info("Preset part number differs from discovered — using discovered value",
				"pciAddress", pf.PciAddress, "preset", pp.PartNumber, "discovered", pf.PartNumber)
		}
		if pp.PSID != "" && pf.PSID != "" && pp.PSID != pf.PSID {
			log.Log.V(1).Info("Preset PSID differs from discovered — using discovered value",
				"pciAddress", pf.PciAddress, "preset", pp.PSID, "discovered", pf.PSID)
		}
	}

	group.PresetApplied = true
	return true
}

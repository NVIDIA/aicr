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
	"bufio"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
)

const dcgmExporterURL = "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"

// acceleratorDeviceLabelKeys are the label keys that identify one accelerator
// in a per-device sample, as kubernetes-sigs/ai-conformance
// TestAcceleratorPerformanceMetrics lists them. DCGM carries gpu and UUID.
var acceleratorDeviceLabelKeys = []string{
	"gpu",          // NVIDIA DCGM integer device index
	"UUID",         // NVIDIA DCGM GPU UUID
	"uuid",         // lowercase UUID variant
	"device_id",    // generic device identifier
	"GPU_I_ID",     // NVIDIA DCGM GPU instance ID (MIG)
	"minor_number", // Linux device minor number
	"pci_bus_id",   // PCI bus identifier
}

// acceleratorMetricFamily is a family of metrics matched by case-insensitive
// name substrings; required families fail the check when absent, optional
// (SHOULD) families are recorded either way.
type acceleratorMetricFamily struct {
	kind     string
	patterns []string
	required bool
}

// acceleratorMetricFamilies are the families the requirement names: per-device
// utilization and memory are the MUST (DCGM_FI_DEV_GPU_UTIL and
// DCGM_FI_DEV_FB_USED); temperature, power and interconnect bandwidth are
// SHOULD observations a hardware or virtualization layer may omit.
var acceleratorMetricFamilies = []acceleratorMetricFamily{
	{kind: "utilization", patterns: []string{"gpu_util"}, required: true},
	{kind: "memory", patterns: []string{"fb_used", "mem_used", "memory_used", "vram_used", "hbm_used"}, required: true},
	{kind: "temperature", patterns: []string{"temp", "temperature"}},
	{kind: "power", patterns: []string{"power", "watt", "energy"}},
	{kind: "interconnect bandwidth", patterns: []string{"nvlink", "pcie_tx", "pcie_rx", "bandwidth", "xgmi"}},
}

// CheckAcceleratorMetrics validates CNCF requirement accelerator_metrics: the
// accelerator metrics exporter's /metrics endpoint serves per-device
// utilization and memory samples in Prometheus exposition format. It scrapes
// the DCGM exporter Service directly over in-cluster DNS.
func CheckAcceleratorMetrics(ctx *validators.Context) error {
	return checkAcceleratorMetricsWithURL(ctx, dcgmExporterURL)
}

// checkAcceleratorMetricsWithURL is the testable implementation that accepts a
// configurable URL. It fails when the scrape returns no samples or when a
// required family has no sample carrying a device label; optional families
// are recorded as PASS (SHOULD) or SKIP (SHOULD), never failed.
func checkAcceleratorMetricsWithURL(ctx *validators.Context, url string) error {
	body, err := httpGet(ctx.Ctx, url)
	if err != nil {
		// Preserve a deterministic code (e.g. oversize response → InvalidRequest)
		// instead of relabeling every failure as a reachability problem.
		return errors.PropagateOrWrap(err, errors.ErrCodeUnavailable,
			"DCGM exporter metrics endpoint unreachable")
	}

	metricsText := string(body)
	// A small raw sample keeps the artifact well under the pod log line
	// limit once it is base64-encoded.
	recordRawTextArtifact(ctx, "DCGM Exporter Metrics (sample)",
		fmt.Sprintf("curl -s %s | head -8", url), truncateLines(metricsText, 8))

	samples := parsePrometheusText(metricsText)
	if len(samples) == 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("metrics endpoint %s returned no samples", url))
	}

	report, missing := summarizeAcceleratorFamilies(samples)
	recordArtifact(ctx, "Per-device accelerator metric families", report)

	if len(missing) > 0 {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no per-device %s metric served by the accelerator exporter (a sample must carry one of the device labels %v)",
				strings.Join(missing, " or "), acceleratorDeviceLabelKeys))
	}
	return nil
}

// summarizeAcceleratorFamilies classifies the scraped samples by family and
// returns the evidence report plus the required families with no per-device
// sample. Optional families render PASS (SHOULD) or SKIP (SHOULD).
func summarizeAcceleratorFamilies(samples []promSample) (string, []string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Samples scraped: %d\nDevice label keys accepted: %s\n\n",
		len(samples), strings.Join(acceleratorDeviceLabelKeys, ", "))
	var missing []string
	for _, family := range acceleratorMetricFamilies {
		found := findPerDeviceSamples(samples, family.patterns)
		level := "SHOULD"
		if family.required {
			level = "MUST"
		}
		switch {
		case len(found) == 0 && family.required:
			missing = append(missing, family.kind)
			fmt.Fprintf(&sb, "  %-24s FAIL (%s): no metric matching %v carries a device label\n",
				family.kind, level, family.patterns)
		case len(found) == 0:
			fmt.Fprintf(&sb, "  %-24s SKIP (%s): no per-device metric matching %v — hardware or virtualization layer may not expose it\n",
				family.kind, level, family.patterns)
		default:
			fmt.Fprintf(&sb, "  %-24s PASS (%s): %s (%d samples, device labels %s)\n",
				family.kind, level, strings.Join(sampleNames(found), ", "), len(found),
				strings.Join(sampleDeviceLabelKeys(found), ", "))
		}
	}
	return sb.String(), missing
}

// promSample is one sample line of Prometheus text exposition.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parsePrometheusText parses exposition text, skipping comments, blank lines
// and samples whose value is not a number.
func parsePrometheusText(body string) []promSample {
	var samples []promSample
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if s, ok := parsePrometheusLine(line); ok {
			samples = append(samples, s)
		}
	}
	return samples
}

func parsePrometheusLine(line string) (promSample, bool) {
	braceOpen := strings.IndexByte(line, '{')
	spaceIdx := strings.IndexByte(line, ' ')

	var name, labelsStr, rest string
	switch {
	case braceOpen != -1 && (spaceIdx == -1 || braceOpen < spaceIdx):
		name = line[:braceOpen]
		braceClose := strings.LastIndexByte(line, '}')
		if braceClose <= braceOpen {
			return promSample{}, false
		}
		labelsStr = line[braceOpen+1 : braceClose]
		rest = strings.TrimSpace(line[braceClose+1:])
	case spaceIdx != -1:
		name = line[:spaceIdx]
		rest = strings.TrimSpace(line[spaceIdx+1:])
	default:
		return promSample{}, false
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return promSample{}, false
	}
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return promSample{}, false
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || math.IsNaN(v) {
		return promSample{}, false
	}
	return promSample{name: name, labels: parsePrometheusLabels(labelsStr), value: v}, true
}

func parsePrometheusLabels(s string) map[string]string {
	out := make(map[string]string)
	if s == "" {
		return out
	}
	for _, pair := range splitPrometheusLabelPairs(s) {
		eqIdx := strings.IndexByte(pair, '=')
		if eqIdx <= 0 {
			continue
		}
		key := strings.TrimSpace(pair[:eqIdx])
		val := strings.TrimSpace(pair[eqIdx+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		// An empty label value is the same as an absent label in Prometheus,
		// so it does not identify a device.
		if key != "" && strings.TrimSpace(val) != "" {
			out[key] = val
		}
	}
	return out
}

// splitPrometheusLabelPairs splits on commas outside quoted values, honoring
// backslash escapes inside them.
func splitPrometheusLabelPairs(s string) []string {
	var result []string
	var cur strings.Builder
	inQuote, escaped := false, false
	for _, ch := range s {
		switch {
		case escaped:
			cur.WriteRune(ch)
			escaped = false
		case ch == '\\' && inQuote:
			cur.WriteRune(ch)
			escaped = true
		case ch == '"':
			inQuote = !inQuote
			cur.WriteRune(ch)
		case ch == ',' && !inQuote:
			result = append(result, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(ch)
		}
	}
	if cur.Len() > 0 {
		result = append(result, cur.String())
	}
	return result
}

func sampleHasDeviceLabel(s promSample) bool {
	for _, key := range acceleratorDeviceLabelKeys {
		if _, ok := s.labels[key]; ok {
			return true
		}
	}
	return false
}

// findPerDeviceSamples returns the samples whose name contains one of the
// patterns (case-insensitive) and that carry a device label.
func findPerDeviceSamples(samples []promSample, patterns []string) []promSample {
	var found []promSample
	for _, s := range samples {
		if !sampleHasDeviceLabel(s) {
			continue
		}
		lower := strings.ToLower(s.name)
		for _, p := range patterns {
			if strings.Contains(lower, p) {
				found = append(found, s)
				break
			}
		}
	}
	return found
}

func sampleNames(samples []promSample) []string {
	seen := map[string]bool{}
	for _, s := range samples {
		seen[s.name] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// sampleDeviceLabelKeys lists which accepted device label keys the samples
// carry, in acceptance order.
func sampleDeviceLabelKeys(samples []promSample) []string {
	var keys []string
	for _, key := range acceleratorDeviceLabelKeys {
		for _, s := range samples {
			if _, ok := s.labels[key]; ok {
				keys = append(keys, key)
				break
			}
		}
	}
	return keys
}

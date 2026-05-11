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

package fingerprint

import (
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe/oskind"
)

// Subtype names referenced from collector outputs. Kept as local
// constants because the collector packages keep them unexported and we
// don't want to import them just for the names.
const (
	subtypeK8sServer        = "server"
	subtypeK8sNode          = "node"
	subtypeGPUSMI           = "smi"
	subtypeOSRelease        = "release"
	subtypeTopologySummary  = "summary"
	subtypeTopologyLabel    = "label"
	keyK8sNodeProvider      = "provider"
	keyGPUSMIModel          = "gpu.model"
	keyOSReleaseID          = "ID"
	keyOSReleaseVersionID   = "VERSION_ID"
	keyTopologyNodeCount    = "node-count"
	labelKeyRegion          = "topology.kubernetes.io/region"
	sourceServiceProvider   = "k8s.node.provider"
	sourceAcceleratorSMI    = "gpu.smi.gpu.model"
	sourceOSRelease         = "os.release"
	sourceK8sServerVersion  = "k8s.server.version"
	sourceTopologyNodeCount = "nodeTopology.summary.node-count"
	sourceTopologyRegion    = "nodeTopology.label." + labelKeyRegion
	sourceTopologyGPU       = "nodeTopology.label." + labelKeyGPUProduct
	labelKeyGPUProduct      = "nvidia.com/gpu.product"
	noteMultiRegion         = "multi-region"
	noteMultiGPU            = "multi-gpu"
	noteUnknownSKU          = "unknown-sku"
)

// FromMeasurements builds a Fingerprint from a snapshot's measurement
// slice. Dimensions whose source signal is missing keep their zero
// value (empty string for Dimension/OSDimension, 0 for IntDimension);
// callers compare those against criteria using Match, which treats
// missing fingerprint values as "unknown" rather than "mismatched."
func FromMeasurements(measurements []*measurement.Measurement) *Fingerprint {
	fp := &Fingerprint{}
	var topo *measurement.Measurement
	for _, m := range measurements {
		if m == nil {
			continue
		}
		switch m.Type {
		case measurement.TypeK8s:
			populateFromK8s(fp, m)
		case measurement.TypeGPU:
			populateFromGPU(fp, m)
		case measurement.TypeOS:
			populateFromOS(fp, m)
		case measurement.TypeNodeTopology:
			populateFromTopology(fp, m)
			topo = m
		case measurement.TypeSystemD:
			// systemd measurements do not contribute to the cluster
			// fingerprint; intentionally skipped.
		}
	}
	if topo != nil {
		reconcileAccelerator(fp, topo)
	}
	return fp
}

// reconcileAccelerator cross-references the per-node smi reading with
// cluster-wide nvidia.com/gpu.product labels (when the GPU operator
// labels nodes). The smi collector only inspects a single node's
// nvidia-smi output, so a heterogeneous cluster (e.g. half H100, half
// L40) would otherwise be claimed as homogeneous in whichever SKU the
// snapshotter happened to land on. The topology label data lets us
// detect disagreement and surface it as multi-gpu rather than lie.
//
// Resolution order:
//   - Topology shows multiple GPU SKUs (disambiguated keys) → record
//     multi-gpu note, clear Value.
//   - Topology shows one GPU SKU and smi was empty → backfill from
//     topology so non-GPU snapshotter nodes still surface accelerator.
//   - Topology label present but unrecognized AND smi did not already
//     mark unknown-sku → record unknown-sku via the topology source.
//   - Otherwise → keep smi result.
func reconcileAccelerator(fp *Fingerprint, topo *measurement.Measurement) {
	st := topo.GetSubtype(subtypeTopologyLabel)
	if st == nil {
		return
	}

	if hasMultiValueKeys(st, labelKeyGPUProduct) {
		fp.Accelerator = Dimension{Source: sourceTopologyGPU, Note: noteMultiGPU}
		return
	}
	if fp.Accelerator.Value != "" {
		return
	}
	raw, err := st.GetString(labelKeyGPUProduct)
	if err != nil || raw == "" {
		return
	}
	product, _ := parseLabelEncoding(raw)
	if sku := ParseGPUSKU(product); sku != "" {
		fp.Accelerator = Dimension{Value: sku, Source: sourceTopologyGPU}
		return
	}
	// Topology label present but unrecognized — mark unknown-sku via
	// the topology source unless smi already marked it (no point
	// overwriting an identical signal from a less-specific source).
	if fp.Accelerator.Note == "" {
		fp.Accelerator = Dimension{Source: sourceTopologyGPU, Note: noteUnknownSKU}
	}
}

// parseLabelEncoding splits the topology collector's label value
// encoding ("<value>|<node1,node2,...>") into its two halves. Returns
// the entire input as value and an empty node list when no separator
// is present.
func parseLabelEncoding(raw string) (value, nodes string) {
	if i := strings.Index(raw, "|"); i >= 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

// hasMultiValueKeys reports whether the label subtype contains
// disambiguated keys (`<label>.<value>`) for the given label name,
// which the topology collector emits when nodes carry the label with
// differing values.
func hasMultiValueKeys(st *measurement.Subtype, label string) bool {
	prefix := label + "."
	count := 0
	for k := range st.Data {
		if strings.HasPrefix(k, prefix) {
			count++
			if count > 1 {
				return true
			}
		}
	}
	return false
}

func populateFromK8s(fp *Fingerprint, m *measurement.Measurement) {
	if st := m.GetSubtype(subtypeK8sServer); st != nil {
		if v, err := st.GetString(measurement.KeyVersion); err == nil && v != "" {
			fp.K8sVersion = Dimension{
				Value:  strings.TrimPrefix(v, "v"),
				Source: sourceK8sServerVersion,
			}
		}
	}
	if st := m.GetSubtype(subtypeK8sNode); st != nil {
		if v, err := st.GetString(keyK8sNodeProvider); err == nil && v != "" {
			fp.Service = Dimension{Value: v, Source: sourceServiceProvider}
		}
	}
}

func populateFromGPU(fp *Fingerprint, m *measurement.Measurement) {
	st := m.GetSubtype(subtypeGPUSMI)
	if st == nil {
		return
	}
	model, err := st.GetString(keyGPUSMIModel)
	if err != nil || model == "" {
		return
	}
	if sku := ParseGPUSKU(model); sku != "" {
		fp.Accelerator = Dimension{Value: sku, Source: sourceAcceleratorSMI}
		return
	}
	// nvidia-smi reported a product string we don't recognize. Surface
	// the staleness via unknown-sku so a maintainer sees the registry
	// gap rather than the snapshot reading as "no GPU." The raw model
	// stays in the underlying measurement for forensics.
	fp.Accelerator = Dimension{Source: sourceAcceleratorSMI, Note: noteUnknownSKU}
}

func populateFromOS(fp *Fingerprint, m *measurement.Measurement) {
	st := m.GetSubtype(subtypeOSRelease)
	if st == nil {
		return
	}
	id, _ := st.GetString(keyOSReleaseID)
	kind := normalizeOSID(id)
	if kind == "" {
		// Avoid emitting a Version with no recognized Value — auditors
		// reading "version: 9.4" with no kind have no actionable
		// signal, and verifier Markdown would render confusingly.
		return
	}
	version, _ := st.GetString(keyOSReleaseVersionID)
	fp.OS = OSDimension{
		Value:   kind,
		Version: version,
		Source:  sourceOSRelease,
	}
}

func populateFromTopology(fp *Fingerprint, m *measurement.Measurement) {
	if st := m.GetSubtype(subtypeTopologySummary); st != nil {
		if count, err := st.GetInt64(keyTopologyNodeCount); err == nil {
			fp.NodeCount = IntDimension{
				Value:  int(count),
				Source: sourceTopologyNodeCount,
			}
		}
	}
	if region, multi := extractRegion(m); region != "" {
		fp.Region = Dimension{Value: region, Source: sourceTopologyRegion}
	} else if multi {
		fp.Region = Dimension{Source: sourceTopologyRegion, Note: noteMultiRegion}
	}
	if st := m.GetSubtype(subtypeTopologyLabel); st != nil {
		fp.GPUNodeCount = IntDimension{
			Value:  countGPUNodes(st),
			Source: sourceTopologyGPU,
		}
	}
}

// countGPUNodes returns the number of distinct nodes carrying the
// nvidia.com/gpu.product label (either as a single aggregated key or
// as disambiguated `.<value>` keys for heterogeneous clusters). The
// label value is encoded as "<value>|<node1,node2,...>" so the node
// list is parsed out and unioned across all matching keys.
func countGPUNodes(st *measurement.Subtype) int {
	nodes := make(map[string]struct{})
	prefix := labelKeyGPUProduct + "."
	for k, v := range st.Data {
		if k != labelKeyGPUProduct && !strings.HasPrefix(k, prefix) {
			continue
		}
		_, nodeList := parseLabelEncoding(v.String())
		if nodeList == "" {
			continue
		}
		for _, n := range strings.Split(nodeList, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				nodes[n] = struct{}{}
			}
		}
	}
	return len(nodes)
}

// extractRegion reads the topology.kubernetes.io/region label value
// from the topology measurement's "label" subtype. The topology
// collector encodes single-valued labels under the plain key with
// value "<region>|<node-list>"; when the cluster spans multiple
// regions the collector disambiguates by appending ".<value>" to the
// key, in which case extractRegion returns ("", true) so the caller
// can record the multi-region note without picking arbitrarily.
func extractRegion(m *measurement.Measurement) (region string, multi bool) {
	st := m.GetSubtype(subtypeTopologyLabel)
	if st == nil {
		return "", false
	}
	if hasMultiValueKeys(st, labelKeyRegion) {
		return "", true
	}
	raw, err := st.GetString(labelKeyRegion)
	if err != nil || raw == "" {
		return "", false
	}
	value, _ := parseLabelEncoding(raw)
	return value, false
}

// normalizeOSID maps an /etc/os-release ID value to the
// recipe.CriteriaOSType enum. Returns "" for IDs that do not match a
// supported OS kind so callers treat them as "fingerprint did not
// detect this dimension" rather than fabricating a match.
func normalizeOSID(id string) string {
	v := strings.ToLower(strings.TrimSpace(id))
	switch v {
	case oskind.Ubuntu:
		return oskind.Ubuntu
	case oskind.RHEL, "redhatenterpriselinux", "redhat":
		return oskind.RHEL
	case oskind.COS:
		return oskind.COS
	case oskind.AmazonLinux, "amzn", "amazon", "al2", "al2023":
		return oskind.AmazonLinux
	case oskind.Talos:
		return oskind.Talos
	default:
		return ""
	}
}

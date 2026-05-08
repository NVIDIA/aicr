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
	keyK8sNodeProvider      = "provider"
	keyGPUSMIModel          = "gpu.model"
	keyOSReleaseID          = "ID"
	keyOSReleaseVersionID   = "VERSION_ID"
	keyTopologyNodeCount    = "node-count"
	sourceServiceProvider   = "k8s.node.provider"
	sourceAcceleratorSMI    = "gpu.smi.gpu.model"
	sourceOSRelease         = "os.release"
	sourceK8sServerVersion  = "k8s.server.version"
	sourceTopologyNodeCount = "nodeTopology.summary.node-count"
)

// FromMeasurements builds a Fingerprint from a snapshot's measurement
// slice. Dimensions whose source signal is missing keep their zero
// value (empty string for Dimension/OSDimension, 0 for IntDimension);
// callers compare those against criteria using Match, which treats
// missing fingerprint values as "unknown" rather than "mismatched."
func FromMeasurements(measurements []*measurement.Measurement) *Fingerprint {
	fp := &Fingerprint{}
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
		case measurement.TypeSystemD:
			// systemd measurements do not contribute to the cluster
			// fingerprint; intentionally skipped.
		}
	}
	return fp
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
	}
}

func populateFromOS(fp *Fingerprint, m *measurement.Measurement) {
	st := m.GetSubtype(subtypeOSRelease)
	if st == nil {
		return
	}
	id, _ := st.GetString(keyOSReleaseID)
	version, _ := st.GetString(keyOSReleaseVersionID)
	kind := normalizeOSID(id)
	if kind == "" && version == "" {
		return
	}
	fp.OS = OSDimension{
		Value:   kind,
		Version: version,
		Source:  sourceOSRelease,
	}
}

func populateFromTopology(fp *Fingerprint, m *measurement.Measurement) {
	st := m.GetSubtype(subtypeTopologySummary)
	if st == nil {
		return
	}
	count, err := st.GetInt64(keyTopologyNodeCount)
	if err != nil {
		return
	}
	fp.NodeCount = IntDimension{
		Value:  int(count),
		Source: sourceTopologyNodeCount,
	}
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

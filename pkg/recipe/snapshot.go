// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package recipe

import (
	"strings"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
)

// ExtractCriteriaFromSnapshot extracts criteria from a snapshot.
// This maps snapshot measurements to criteria fields.
func ExtractCriteriaFromSnapshot(snap *snapshotter.Snapshot) *Criteria {
	criteria := NewCriteria()

	if snap == nil {
		return criteria
	}

	for _, m := range snap.Measurements {
		if m == nil {
			continue
		}

		switch m.Type {
		case measurement.TypeK8s:
			for _, st := range m.Subtypes {
				if st.Name == "server" {
					if svcType, ok := st.Data["service"]; ok {
						if parsed, err := ParseCriteriaServiceType(svcType.String()); err == nil {
							criteria.Service = parsed
						}
					}

					if version, ok := st.Data["version"]; ok {
						versionStr := version.String()
						switch {
						case strings.Contains(versionStr, "-eks-"):
							criteria.Service = CriteriaServiceEKS
						case strings.Contains(versionStr, "-gke"):
							criteria.Service = CriteriaServiceGKE
						case strings.Contains(versionStr, "-aks"):
							criteria.Service = CriteriaServiceAKS
						}
					}
				}
			}

		case measurement.TypeGPU:
			for _, st := range m.Subtypes {
				if st.Name == "smi" || st.Name == "device" {
					if model, ok := st.Data["gpu.model"]; ok {
						if acc := matchAccelerator(model.String()); acc != "" {
							criteria.Accelerator = acc
						}
					}
					if criteria.Accelerator == CriteriaAcceleratorAny {
						if model, ok := st.Data["model"]; ok {
							if acc := matchAccelerator(model.String()); acc != "" {
								criteria.Accelerator = acc
							}
						}
					}
				}
			}

		case measurement.TypeOS:
			for _, st := range m.Subtypes {
				if st.Name == "release" {
					if osID, ok := st.Data["ID"]; ok {
						if parsed, err := ParseCriteriaOSType(osID.String()); err == nil {
							criteria.OS = parsed
						}
					}
				}
			}

		case measurement.TypeSystemD:
			continue
		}
	}

	return criteria
}

func matchAccelerator(model string) CriteriaAcceleratorType {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "gb200"):
		return CriteriaAcceleratorGB200
	case strings.Contains(lower, "h100"):
		return CriteriaAcceleratorH100
	case strings.Contains(lower, "a100"):
		return CriteriaAcceleratorA100
	case strings.Contains(lower, "l40"):
		return CriteriaAcceleratorL40
	default:
		return ""
	}
}

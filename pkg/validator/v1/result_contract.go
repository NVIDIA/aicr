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

package v1

// ClusterAutoscalingCheckName is the catalog name of the conformance check
// that records the cluster's autoscaler arrangement. Exported so the catalog
// package can assert that an embedded entry with this exact name exists, as it
// does for the other named checks.
const ClusterAutoscalingCheckName = "cluster-autoscaling"

// observationOnlyChecks are the checks whose every passing result is an
// observation (validators.ObservationOnlyExtra), never a demonstration. It is
// the one list both ends of the result contract consult: the conformance
// image's tests assert each name is a registered check that emits the marker,
// and pkg/evidence/cncf reads every unmarked pass from one of them as an
// observation regardless of the ctrf.ExtraKeyResultSchema stamp; the stamp
// only decides whether the reason says the image predates the contract or
// the marker went missing.
var observationOnlyChecks = map[string]bool{
	ClusterAutoscalingCheckName: true,
}

// IsObservationOnlyCheck reports whether every pass of the named check is an
// observation rather than a demonstration.
func IsObservationOnlyCheck(name string) bool {
	return observationOnlyChecks[name]
}

// ObservationOnlyChecks returns the names of the observation-only checks, in
// no particular order, for tests that pin the set to the registered checks.
func ObservationOnlyChecks() []string {
	out := make([]string, 0, len(observationOnlyChecks))
	for name := range observationOnlyChecks {
		out = append(out, name)
	}
	return out
}

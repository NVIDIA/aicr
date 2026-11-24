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

import (
	"crypto/sha256"
	"encoding/hex"
)

// Workload namespace names shared by the performance checks that create them
// and the orchestrator that deletes them after abandoning a check. Each run
// gets its own namespace, suffixed with WorkloadNamespaceSuffix, so concurrent
// validate invocations never share one and fixed resource names inside it
// never collide; NCCL folds its variant into the name so its checks stay apart
// too.
const (
	InferencePerfNamespacePrefix = "aicr-inference-perf"
	NCCLPerfNamespacePrefix      = "aicr-nccl-perf"
)

// ncclNamespaceVariants are the variant segments NCCLPerfNamespace is called
// with, one per NCCL catalog check (default, -net, -nvls).
var ncclNamespaceVariants = []string{"default", "net", "nvls"}

// WorkloadNamespaceSuffix returns the 8-hex-character suffix of runID's
// workload namespaces: the first 4 bytes of its SHA-256. Short enough that
// names built from it (namespaces, Job names, Grove's "<namespace>-<name>"
// PodClique labels) stay within the 63-character DNS-label limit, and
// deterministic, so a retry under the same run ID reclaims its own namespace.
func WorkloadNamespaceSuffix(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return hex.EncodeToString(sum[:4])
}

// InferencePerfNamespace returns the inference-perf workload namespace for
// suffix (WorkloadNamespaceSuffix of the run ID, or a standalone run's random
// suffix).
func InferencePerfNamespace(suffix string) string {
	return InferencePerfNamespacePrefix + "-" + suffix
}

// NCCLPerfNamespace returns the workload namespace of the NCCL check for
// variant ("default", "net" or "nvls") and suffix.
func NCCLPerfNamespace(variant, suffix string) string {
	return NCCLPerfNamespacePrefix + "-" + variant + "-" + suffix
}

// IsRunWorkloadNamespace reports whether name is one a performance check
// derives for runID. A label alone proves nothing, so the orchestrator
// deletes a run-labeled namespace only when its name also reconstructs from
// the run ID.
func IsRunWorkloadNamespace(name, runID string) bool {
	if runID == "" {
		return false
	}
	suffix := WorkloadNamespaceSuffix(runID)
	if name == InferencePerfNamespace(suffix) {
		return true
	}
	for _, variant := range ncclNamespaceVariants {
		if name == NCCLPerfNamespace(variant, suffix) {
			return true
		}
	}
	return false
}

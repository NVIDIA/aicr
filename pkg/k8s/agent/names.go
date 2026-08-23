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

package agent

import (
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

// defaultNameBase is the prefix used for generated resource names when the
// caller does not set Config.NameBase.
const defaultNameBase = "aicr"

// staticClusterRoleName is the un-scoped ClusterRole/ClusterRoleBinding
// name used only as the prefix input to nameWithRunID.
const staticClusterRoleName = "aicr-node-reader"

// staticStagingConfigMapName is the un-scoped staging ConfigMap name used
// only as the prefix input to nameWithRunID.
//
// It is deliberately NOT "aicr-snapshot": pkg/validator names its own
// snapshot data ConfigMap "aicr-snapshot-<runID>" (see EnsureDataConfigMaps
// and cleanupDataConfigMaps in pkg/validator/validator.go, and the volume in
// pkg/validator/v1/job_plan_internal.go). `aicr validate` hands ONE run ID to
// both the snapshot agent and the validator Jobs and points both at the same
// namespace, so a shared prefix would put two owners on one object: the
// validator would adopt and overwrite the agent's staging ConfigMap, and its
// (UID-unpinned) cleanup would delete it. Distinct prefixes keep the two
// namespaces of generated names disjoint by construction.
const staticStagingConfigMapName = "aicr-agent-snapshot"

// nameWithRunID joins prefix and runID, truncating prefix so the result fits
// within the Kubernetes name ceiling. An empty prefix yields the bare runID.
// An empty runID yields the prefix with any trailing "-" trimmed rather than
// appending one: a trailing separator would leave a Kubernetes object name
// that fails validation (names must end in an alphanumeric character).
//
// Every in-tree caller supplies a RunID — pkg/snapshotter defaults and
// rejects an all-whitespace one before building an agent Config — so the
// empty-runID fallback is reachable only by an SDK caller constructing a
// Config directly, which then gets unscoped, collision-prone names.
func nameWithRunID(prefix, runID string) string {
	if prefix == "" {
		return runID
	}
	if runID == "" {
		return strings.TrimRight(prefix, "-")
	}
	budget := defaults.MaxK8sNameLength - len(runID) - 1
	if budget < 0 {
		budget = 0
	}
	if len(prefix) > budget {
		prefix = prefix[:budget]
	}
	prefix = strings.TrimRight(prefix, "-")
	if prefix == "" {
		return runID
	}
	return prefix + "-" + runID
}

// base returns the configured name base, defaulting to "aicr" when unset.
func (d *Deployer) base() string {
	if d.config.NameBase != "" {
		return d.config.NameBase
	}
	return defaultNameBase
}

// jobName returns the run-scoped name for the agent Job.
func (d *Deployer) jobName() string {
	prefix := d.config.JobName
	if prefix == "" {
		prefix = d.base()
	}
	return nameWithRunID(prefix, d.config.RunID)
}

// saName returns the run-scoped name for the agent ServiceAccount.
func (d *Deployer) saName() string {
	prefix := d.config.ServiceAccountName
	if prefix == "" {
		prefix = d.base()
	}
	return nameWithRunID(prefix, d.config.RunID)
}

// roleName returns the run-scoped name for the agent Role and RoleBinding.
// It shares the ServiceAccount's name, matching the existing convention
// where the Role/RoleBinding are named after the ServiceAccount they bind.
func (d *Deployer) roleName() string {
	return d.saName()
}

// clusterRoleName returns the run-scoped name for the agent ClusterRole and
// ClusterRoleBinding.
func (d *Deployer) clusterRoleName() string {
	return nameWithRunID(staticClusterRoleName, d.config.RunID)
}

// StagingConfigMapName returns the run-scoped name of the internal staging
// ConfigMap the agent Job writes its snapshot result to for the given run ID.
// It is exported so the one caller that builds the Job's `cm://` output URI
// (pkg/snapshotter's agentConfigMapTarget) derives that name from the same
// place Cleanup deletes it, instead of repeating the format string.
func StagingConfigMapName(runID string) string {
	return nameWithRunID(staticStagingConfigMapName, runID)
}

// stagingConfigMapName returns the run-scoped name for the staging
// ConfigMap the agent writes its snapshot result to.
func (d *Deployer) stagingConfigMapName() string {
	return StagingConfigMapName(d.config.RunID)
}

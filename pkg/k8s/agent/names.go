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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/apimachinery/pkg/util/validation"
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
// The empty-runID fallback is not reachable through Deploy, which rejects
// an empty or malformed RunID up front (see validateRunID). It survives for
// the accessors a caller can reach without deploying — JobName, Cleanup on a
// Deployer that never ran — where returning a name that fails Kubernetes
// validation would be strictly worse than returning the bare prefix.
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

// validateRunID rejects a Config.RunID that cannot be folded into a valid
// Kubernetes object name. Deploy calls it before any object is created:
// every run-owned name is "<prefix>-<RunID>", so a bad run ID would
// otherwise surface as an opaque apiserver "Invalid value: metadata.name"
// from deep inside the ensure* chain — after some objects already exist.
//
// The constraint is exactly DNS-1123 label: lowercase alphanumerics and
// "-", starting and ending alphanumeric, at most 63 characters. The length
// bound is what keeps the generated name inside defaults.MaxK8sNameLength:
// nameWithRunID truncates the prefix to fit, so an over-long run ID is the
// one input it cannot compensate for (its budget floors at zero and the
// bare run ID is returned).
//
// pkg/snapshotter defaults and whitespace-checks RunID before building an
// agent Config, but that guard does not cover callers who construct a
// pkg/k8s/agent Config directly, which is the public SDK surface.
// Error-context keys shared by the name-validation failures below, so a
// caller parsing a structured error keys off one spelling.
const (
	ctxKeyField        = "field"
	ctxKeyValue        = "value"
	ctxKeyResolvedName = "resolvedName"
)

func (d *Deployer) validateRunID() error {
	runID := d.config.RunID
	if runID == "" {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"Config.RunID is required: every object this Deployer creates is named \"<prefix>-<RunID>\"; generate one with runid.Generate()",
			map[string]any{ctxKeyField: "Config.RunID", ctxKeyValue: runID})
	}
	if problems := validation.IsDNS1123Label(runID); len(problems) > 0 {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Config.RunID %q is not a valid Kubernetes name segment: %s",
				runID, strings.Join(problems, "; ")),
			map[string]any{ctxKeyField: "Config.RunID", ctxKeyValue: runID})
	}
	return nil
}

// resolvedName is one caller-influenced object name this Deployer would
// create, carried together with the Config field that supplied its prefix.
// Validation reports the field rather than only the derived string, because
// the field is what a caller can actually change.
type resolvedName struct {
	field  string // Config field the prefix came from
	prefix string // the prefix value that field held
	value  string // the resolved object name, "<prefix>-<RunID>"
	// objects names what value is the name of, so a rejection states the
	// blast radius: saName() also names the Role and the RoleBinding.
	objects string
}

// resolvedNames returns every object name this Deployer builds from a
// caller-supplied prefix, paired with its source field.
//
// The ClusterRole/ClusterRoleBinding and staging ConfigMap names are
// deliberately absent: their prefixes are package constants
// (staticClusterRoleName, staticStagingConfigMapName), so the only
// caller-supplied input they carry is the run ID, which validateRunID
// already covers.
func (d *Deployer) resolvedNames() []resolvedName {
	jobField, jobPrefix := "Config.NameBase", d.base()
	if d.config.JobName != "" {
		jobField, jobPrefix = "Config.JobName", d.config.JobName
	}
	saField, saPrefix := "Config.NameBase", d.base()
	if d.config.ServiceAccountName != "" {
		saField, saPrefix = "Config.ServiceAccountName", d.config.ServiceAccountName
	}
	return []resolvedName{
		{field: jobField, prefix: jobPrefix, value: d.jobName(), objects: "Job"},
		{field: saField, prefix: saPrefix, value: d.saName(), objects: "ServiceAccount, Role and RoleBinding"},
	}
}

// validateResolvedNames rejects a generated object name Kubernetes would
// refuse. validateRunID covers one half of every run-owned name; this covers
// the other. NameBase, JobName and ServiceAccountName are caller-supplied
// too, and a prefix such as "agent_" yields "agent_-<RunID>" — which the
// apiserver rejects with an opaque "Invalid value: metadata.name" from
// partway through Deploy's ensure* chain, after some objects already exist.
//
// The constraint is DNS-1123 subdomain, which is what the apiserver enforces
// on Job and ServiceAccount names, plus the narrower defaults.MaxK8sNameLength
// ceiling this package budgets against (a Job name also becomes the
// batch.kubernetes.io/job-name label value on every Pod the Job creates, and
// label values share that ceiling). nameWithRunID truncates the prefix to
// that budget, so the length branch is reachable only for a run ID that is
// itself over-long — which validateRunID rejects first when both run under
// validateNames, but not when this method is called on its own.
func (d *Deployer) validateResolvedNames() error {
	for _, n := range d.resolvedNames() {
		problems := validation.IsDNS1123Subdomain(n.value)
		if len(problems) == 0 && len(n.value) > defaults.MaxK8sNameLength {
			problems = []string{fmt.Sprintf("must be no more than %d characters", defaults.MaxK8sNameLength)}
		}
		if len(problems) == 0 {
			continue
		}
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s %q yields the %s name %q, which is not a valid Kubernetes object name: %s",
				n.field, n.prefix, n.objects, n.value, strings.Join(problems, "; ")),
			map[string]any{ctxKeyField: n.field, ctxKeyValue: n.prefix, ctxKeyResolvedName: n.value})
	}
	return nil
}

// validateNames is Deploy's naming pre-flight: it rejects both halves of a
// run-owned name — the run ID and the resolved object names built from the
// caller's prefixes — before any cluster call is made, so an invalid value
// can never leave a partially-created deployment behind.
func (d *Deployer) validateNames() error {
	if err := d.validateRunID(); err != nil {
		return err
	}
	return d.validateResolvedNames()
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

// podServiceAccountName returns the ServiceAccount the agent pod actually
// runs as: the operator's already-existing ServiceAccount when Deploy
// resolved Config.ServiceAccountName to an exact match, otherwise this run's
// own run-scoped one.
//
// It is deliberately separate from saName(), which stays the run-scoped
// name unconditionally. saName() also names the Role and RoleBinding, and
// those exist only in prefix mode — folding the exact name into it would
// make roleName() silently return an operator-owned ServiceAccount's name.
func (d *Deployer) podServiceAccountName() string {
	if name := d.existingServiceAccount(); name != "" {
		return name
	}
	return d.saName()
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

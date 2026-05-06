// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package config

import (
	"maps"
	"slices"
)

// Accessors here are nil-receiver tolerant so callers can write
// `cfg.RecipeCriteria()` without nil-checking every intermediate pointer.
// Slice and map returns are defensive copies (slices.Clone / maps.Clone)
// so callers cannot mutate the loaded config; nil inputs preserve nil
// outputs while explicitly-empty collections (e.g. `set: []` in YAML)
// round-trip as non-nil empty values, preserving the user's intent to
// clear an inherited default.

// === Top-level ===

// Recipe returns the recipe section, or nil if cfg or the section is unset.
func (c *AICRConfig) Recipe() *RecipeSpec {
	if c == nil {
		return nil
	}
	return c.Spec.Recipe
}

// Bundle returns the bundle section, or nil if cfg or the section is unset.
func (c *AICRConfig) Bundle() *BundleSpec {
	if c == nil {
		return nil
	}
	return c.Spec.Bundle
}

// === RecipeSpec accessors ===

// CriteriaFields returns the criteria spec or nil.
func (r *RecipeSpec) CriteriaFields() *CriteriaSpec {
	if r == nil {
		return nil
	}
	return r.Criteria
}

// SnapshotPath returns spec.recipe.input.snapshot, or "" when unset.
func (r *RecipeSpec) SnapshotPath() string {
	if r == nil || r.Input == nil {
		return ""
	}
	return r.Input.Snapshot
}

// OutputPath returns spec.recipe.output.path, or "" when unset.
func (r *RecipeSpec) OutputPath() string {
	if r == nil || r.Output == nil {
		return ""
	}
	return r.Output.Path
}

// OutputFormat returns spec.recipe.output.format, or "" when unset.
func (r *RecipeSpec) OutputFormat() string {
	if r == nil || r.Output == nil {
		return ""
	}
	return r.Output.Format
}

// DataDir returns spec.recipe.data, or "" when unset.
func (r *RecipeSpec) DataDir() string {
	if r == nil {
		return ""
	}
	return r.Data
}

// === BundleSpec accessors ===

// RecipeInput returns spec.bundle.input.recipe, or "" when unset.
func (b *BundleSpec) RecipeInput() string {
	if b == nil || b.Input == nil {
		return ""
	}
	return b.Input.Recipe
}

// OutputTarget returns spec.bundle.output.target, or "" when unset.
func (b *BundleSpec) OutputTarget() string {
	if b == nil || b.Output == nil {
		return ""
	}
	return b.Output.Target
}

// OutputImageRefs returns spec.bundle.output.imageRefs, or "" when unset.
func (b *BundleSpec) OutputImageRefs() string {
	if b == nil || b.Output == nil {
		return ""
	}
	return b.Output.ImageRefs
}

// DeploymentDeployer returns spec.bundle.deployment.deployer, or "" when unset.
func (b *BundleSpec) DeploymentDeployer() string {
	if b == nil || b.Deployment == nil {
		return ""
	}
	return b.Deployment.Deployer
}

// DeploymentRepo returns spec.bundle.deployment.repo, or "" when unset.
func (b *BundleSpec) DeploymentRepo() string {
	if b == nil || b.Deployment == nil {
		return ""
	}
	return b.Deployment.Repo
}

// DeploymentSet returns a defensive copy of spec.bundle.deployment.set.
func (b *BundleSpec) DeploymentSet() []string {
	if b == nil || b.Deployment == nil {
		return nil
	}
	return slices.Clone(b.Deployment.Set)
}

// DeploymentDynamic returns a defensive copy of spec.bundle.deployment.dynamic.
func (b *BundleSpec) DeploymentDynamic() []string {
	if b == nil || b.Deployment == nil {
		return nil
	}
	return slices.Clone(b.Deployment.Dynamic)
}

// SystemNodeSelector returns a defensive copy of the system selector.
func (b *BundleSpec) SystemNodeSelector() map[string]string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return maps.Clone(b.Scheduling.SystemNodeSelector)
}

// SystemNodeTolerations returns a defensive copy of the system tolerations.
func (b *BundleSpec) SystemNodeTolerations() []string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return slices.Clone(b.Scheduling.SystemNodeTolerations)
}

// AcceleratedNodeSelector returns a defensive copy of the accelerated selector.
func (b *BundleSpec) AcceleratedNodeSelector() map[string]string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return maps.Clone(b.Scheduling.AcceleratedNodeSelector)
}

// AcceleratedNodeTolerations returns a defensive copy of accelerated tolerations.
func (b *BundleSpec) AcceleratedNodeTolerations() []string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return slices.Clone(b.Scheduling.AcceleratedNodeTolerations)
}

// WorkloadGate returns spec.bundle.scheduling.workloadGate, or "" when unset.
func (b *BundleSpec) WorkloadGate() string {
	if b == nil || b.Scheduling == nil {
		return ""
	}
	return b.Scheduling.WorkloadGate
}

// WorkloadSelector returns a defensive copy of the workload selector.
func (b *BundleSpec) WorkloadSelector() map[string]string {
	if b == nil || b.Scheduling == nil {
		return nil
	}
	return maps.Clone(b.Scheduling.WorkloadSelector)
}

// SchedulingNodes returns spec.bundle.scheduling.nodes, or 0 when unset.
func (b *BundleSpec) SchedulingNodes() int {
	if b == nil || b.Scheduling == nil {
		return 0
	}
	return b.Scheduling.Nodes
}

// SchedulingStorageClass returns spec.bundle.scheduling.storageClass, or "" when unset.
func (b *BundleSpec) SchedulingStorageClass() string {
	if b == nil || b.Scheduling == nil {
		return ""
	}
	return b.Scheduling.StorageClass
}

// AttestEnabled returns spec.bundle.attestation.enabled, or false when unset.
func (b *BundleSpec) AttestEnabled() bool {
	if b == nil || b.Attestation == nil {
		return false
	}
	return b.Attestation.Enabled
}

// CertIDRegexp returns the certificateIdentityRegexp, or "" when unset.
func (b *BundleSpec) CertIDRegexp() string {
	if b == nil || b.Attestation == nil {
		return ""
	}
	return b.Attestation.CertificateIdentityRegexp
}

// OIDCDeviceFlow returns spec.bundle.attestation.oidcDeviceFlow.
func (b *BundleSpec) OIDCDeviceFlow() bool {
	if b == nil || b.Attestation == nil {
		return false
	}
	return b.Attestation.OIDCDeviceFlow
}

// RegistryInsecureTLS returns spec.bundle.registry.insecureTLS.
func (b *BundleSpec) RegistryInsecureTLS() bool {
	if b == nil || b.Registry == nil {
		return false
	}
	return b.Registry.InsecureTLS
}

// RegistryPlainHTTP returns spec.bundle.registry.plainHTTP.
func (b *BundleSpec) RegistryPlainHTTP() bool {
	if b == nil || b.Registry == nil {
		return false
	}
	return b.Registry.PlainHTTP
}

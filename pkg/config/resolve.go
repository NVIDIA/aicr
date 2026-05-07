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
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"

	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// BundleResolved is the typed-domain projection of BundleSpec produced by
// (*BundleSpec).Resolve. Every field is converted from its wire form
// exactly once at the conversion boundary; CLI and API consumers layer
// flag overrides on top of these values rather than re-parsing strings.
//
// Zero values mean "config did not set this field." Maps and slices
// preserve the nil-vs-explicitly-empty distinction from the wire spec —
// callers can therefore detect whether a user wrote `selector: {}` to
// clear an inherited default vs. omitted the key entirely.
type BundleResolved struct {
	// RecipeInput is spec.bundle.input.recipe.
	RecipeInput string

	// OutputTarget is the parsed spec.bundle.output.target. Nil when
	// config did not set a target. OutputTargetRaw preserves the original
	// string for log/error messages.
	OutputTarget    *oci.Reference
	OutputTargetRaw string

	// ImageRefs is spec.bundle.output.imageRefs.
	ImageRefs string

	// Deployer is the parsed spec.bundle.deployment.deployer. Empty
	// (zero) when config did not set a deployer.
	Deployer bundlercfg.DeployerType

	// Repo is spec.bundle.deployment.repo.
	Repo string

	// ValueOverrides is spec.bundle.deployment.set, parsed.
	// Nil if config did not set the field; non-nil (possibly empty) if
	// config provided an explicit list (including `set: []`).
	ValueOverrides []bundlercfg.ComponentPath

	// DynamicValues is spec.bundle.deployment.dynamic, parsed.
	// Same nil-vs-empty semantics as ValueOverrides.
	DynamicValues []bundlercfg.ComponentPath

	// SystemNodeSelector is spec.bundle.scheduling.systemNodeSelector.
	// Nil if config did not set it; non-nil empty if `{}` was explicit.
	SystemNodeSelector map[string]string

	// SystemNodeTolerations is spec.bundle.scheduling.systemNodeTolerations,
	// parsed. Nil if config did not set the field.
	SystemNodeTolerations []corev1.Toleration

	// AcceleratedNodeSelector is spec.bundle.scheduling.acceleratedNodeSelector.
	AcceleratedNodeSelector map[string]string

	// AcceleratedNodeTolerations is the parsed slice.
	AcceleratedNodeTolerations []corev1.Toleration

	// WorkloadGate is the parsed spec.bundle.scheduling.workloadGate taint.
	// Nil when config did not set it.
	WorkloadGate *corev1.Taint

	// WorkloadSelector is spec.bundle.scheduling.workloadSelector.
	WorkloadSelector map[string]string

	// Nodes is spec.bundle.scheduling.nodes; 0 when unset.
	Nodes int

	// StorageClass is spec.bundle.scheduling.storageClass.
	StorageClass string

	// Attest is spec.bundle.attestation.enabled.
	Attest bool

	// CertIDRegexp is spec.bundle.attestation.certificateIdentityRegexp.
	CertIDRegexp string

	// OIDCDeviceFlow is spec.bundle.attestation.oidcDeviceFlow.
	OIDCDeviceFlow bool

	// InsecureTLS is spec.bundle.registry.insecureTLS.
	InsecureTLS bool

	// PlainHTTP is spec.bundle.registry.plainHTTP.
	PlainHTTP bool
}

// Resolve converts a BundleSpec from the wire-string form to a typed
// BundleResolved. It is nil-receiver tolerant and never returns a nil
// pointer — callers reach into fields, so nil would just relocate the
// nil-pointer dereference.
//
// Errors are attributed to their source spec path (for example,
// "spec.bundle.deployment.set") so callers can surface the location of
// invalid input without reconstructing the path themselves.
func (b *BundleSpec) Resolve() (*BundleResolved, error) {
	out := &BundleResolved{}
	if b == nil {
		return out, nil
	}

	if b.Input != nil {
		out.RecipeInput = b.Input.Recipe
	}

	if b.Output != nil {
		out.OutputTargetRaw = b.Output.Target
		out.ImageRefs = b.Output.ImageRefs
		if b.Output.Target != "" {
			ref, err := oci.ParseOutputTarget(b.Output.Target)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.output.target", err)
			}
			out.OutputTarget = ref
		}
	}

	if b.Deployment != nil {
		out.Repo = b.Deployment.Repo
		if b.Deployment.Deployer != "" {
			d, err := bundlercfg.ParseDeployerType(b.Deployment.Deployer)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.deployment.deployer", err)
			}
			out.Deployer = d
		}
		if b.Deployment.Set != nil {
			paths, err := bundlercfg.ParseValueOverrides(b.Deployment.Set)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.deployment.set", err)
			}
			out.ValueOverrides = paths
		}
		if b.Deployment.Dynamic != nil {
			paths, err := bundlercfg.ParseDynamicValues(b.Deployment.Dynamic)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.deployment.dynamic", err)
			}
			out.DynamicValues = paths
		}
	}

	if b.Scheduling != nil {
		if b.Scheduling.Nodes < 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("spec.bundle.scheduling.nodes must be >= 0, got %d", b.Scheduling.Nodes))
		}
		out.Nodes = b.Scheduling.Nodes
		out.StorageClass = b.Scheduling.StorageClass

		// maps.Clone preserves nil-vs-explicitly-empty: clone(nil) is nil,
		// clone({}) is non-nil empty.
		out.SystemNodeSelector = maps.Clone(b.Scheduling.SystemNodeSelector)
		out.AcceleratedNodeSelector = maps.Clone(b.Scheduling.AcceleratedNodeSelector)
		out.WorkloadSelector = maps.Clone(b.Scheduling.WorkloadSelector)

		if b.Scheduling.SystemNodeTolerations != nil {
			tols, err := snapshotter.ParseTolerations(b.Scheduling.SystemNodeTolerations)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.scheduling.systemNodeTolerations", err)
			}
			out.SystemNodeTolerations = tols
		}
		if b.Scheduling.AcceleratedNodeTolerations != nil {
			tols, err := snapshotter.ParseTolerations(b.Scheduling.AcceleratedNodeTolerations)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.scheduling.acceleratedNodeTolerations", err)
			}
			out.AcceleratedNodeTolerations = tols
		}
		if b.Scheduling.WorkloadGate != "" {
			t, err := snapshotter.ParseTaint(b.Scheduling.WorkloadGate)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					"invalid spec.bundle.scheduling.workloadGate", err)
			}
			out.WorkloadGate = t
		}
	}

	if b.Attestation != nil {
		out.Attest = b.Attestation.Enabled
		out.CertIDRegexp = b.Attestation.CertificateIdentityRegexp
		out.OIDCDeviceFlow = b.Attestation.OIDCDeviceFlow
	}

	if b.Registry != nil {
		out.InsecureTLS = b.Registry.InsecureTLS
		out.PlainHTTP = b.Registry.PlainHTTP
	}

	return out, nil
}

// ResolveCriteria converts the recipe criteria spec from wire-string form
// to a typed *recipe.Criteria. Nil-receiver tolerant; never returns a nil
// pointer.
//
// Unlike recipe.NewCriteria, the returned Criteria does NOT default
// unset fields to "any": empty enum fields signal "config did not set
// this slot" so callers can detect what to copy onto a target Criteria.
// Nodes < 0 is rejected; Nodes == 0 means unset.
func (r *RecipeSpec) ResolveCriteria() (*recipe.Criteria, error) {
	out := &recipe.Criteria{}
	if r == nil || r.Criteria == nil {
		return out, nil
	}
	c := r.Criteria
	if c.Service != "" {
		v, err := recipe.ParseCriteriaServiceType(c.Service)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"invalid spec.recipe.criteria.service", err)
		}
		out.Service = v
	}
	if c.Accelerator != "" {
		v, err := recipe.ParseCriteriaAcceleratorType(c.Accelerator)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"invalid spec.recipe.criteria.accelerator", err)
		}
		out.Accelerator = v
	}
	if c.Intent != "" {
		v, err := recipe.ParseCriteriaIntentType(c.Intent)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"invalid spec.recipe.criteria.intent", err)
		}
		out.Intent = v
	}
	if c.OS != "" {
		v, err := recipe.ParseCriteriaOSType(c.OS)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"invalid spec.recipe.criteria.os", err)
		}
		out.OS = v
	}
	if c.Platform != "" {
		v, err := recipe.ParseCriteriaPlatformType(c.Platform)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"invalid spec.recipe.criteria.platform", err)
		}
		out.Platform = v
	}
	if c.Nodes < 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("spec.recipe.criteria.nodes must be >= 0, got %d", c.Nodes))
	}
	out.Nodes = c.Nodes
	return out, nil
}

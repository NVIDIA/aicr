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
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// DependencyRequirement is the strength of a dependency affinity.
type DependencyRequirement string

const (
	// DependencyRequirementPreferred renders as preferredDuringSchedulingIgnoredDuringExecution
	// with a high weight; missing components are tolerated with a warning.
	DependencyRequirementPreferred DependencyRequirement = "preferred"

	// DependencyRequirementRequired renders as requiredDuringSchedulingIgnoredDuringExecution
	// and causes pre-flight failure when the referenced component is absent from the recipe.
	DependencyRequirementRequired DependencyRequirement = "required"

	// defaultTopologyKey is used when DependencyAffinity.TopologyKey is empty.
	// kubernetes.io/hostname pins the orchestrator to the same node as the
	// dependency pod, which is the right granularity for the multi-SG/network-locality
	// case that motivates #933. Zone-level locality requires an explicit override.
	defaultTopologyKey = "kubernetes.io/hostname"
)

// DependencyAffinity declares a co-location preference for a validator's
// orchestrator pod with another component's pod.
type DependencyAffinity struct {
	// ComponentRef is the name of a recipe component whose pod the orchestrator
	// should co-locate with. The deployer resolves this to a namespace at spawn
	// time using the resolved recipe's componentRefs.
	ComponentRef string `json:"componentRef" yaml:"componentRef"`

	// PodLabelSelector matches the dependency pod's labels (e.g.,
	// {"app.kubernetes.io/name": "prometheus"}). All key/value pairs must match.
	PodLabelSelector map[string]string `json:"podLabelSelector" yaml:"podLabelSelector"`

	// Requirement controls strength. "required" hard-fails when the dependency
	// is unschedulable; "preferred" (default) is a high-weight scheduling hint.
	Requirement DependencyRequirement `json:"requirement,omitempty" yaml:"requirement,omitempty"`

	// TopologyKey is the node label whose value defines co-location.
	// Defaults to kubernetes.io/hostname (same node) when empty.
	TopologyKey string `json:"topologyKey,omitempty" yaml:"topologyKey,omitempty"`
}

// Validate checks that ComponentRef and PodLabelSelector are non-empty and
// that Requirement is either empty (defaults to preferred), "preferred", or
// "required".
func (d DependencyAffinity) Validate() error {
	if d.ComponentRef == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"dependencyAffinity: componentRef is required")
	}
	if len(d.PodLabelSelector) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("dependencyAffinity %q: podLabelSelector is required", d.ComponentRef))
	}
	switch d.Requirement {
	case "", DependencyRequirementPreferred, DependencyRequirementRequired:
		return nil
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("dependencyAffinity %q: invalid requirement %q (must be \"preferred\" or \"required\")",
				d.ComponentRef, d.Requirement))
	}
}

// RequirementOrDefault returns the requirement strength, defaulting to
// "preferred" when unset.
func (d DependencyAffinity) RequirementOrDefault() DependencyRequirement {
	if d.Requirement == "" {
		return DependencyRequirementPreferred
	}
	return d.Requirement
}

// TopologyKeyOrDefault returns the topology key, defaulting to
// kubernetes.io/hostname when unset.
func (d DependencyAffinity) TopologyKeyOrDefault() string {
	if d.TopologyKey == "" {
		return defaultTopologyKey
	}
	return d.TopologyKey
}

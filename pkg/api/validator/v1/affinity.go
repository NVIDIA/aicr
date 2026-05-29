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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// preferredAffinityWeight is the scheduler weight for preferred dependency
// affinity terms. 100 (max) so the term dominates image-locality scoring on
// the first scheduling decision; once the validator image lands on the
// dependency node, image-locality reinforces affinity on subsequent runs.
const preferredAffinityWeight = 100

// BuildOrchestratorAffinity composes the orchestrator pod's full affinity from
// the validator's declared dependencies and the resolved recipe's component
// refs. The result always includes the default prefer-CPU NodeAffinity; each
// resolvable dependency adds one PodAffinity term.
//
// Resolution rules (per https://github.com/NVIDIA/aicr/issues/933):
//   - A "required" dependency whose componentRef is missing from componentRefs
//     returns ErrCodeInvalidRequest. The caller should treat this as a recipe
//     misconfiguration and fail the run before deploying any Job.
//   - A "preferred" dependency whose componentRef is missing is logged at
//     slog.Warn and produces no PodAffinity term. The orchestrator schedules
//     wherever the scheduler picks; this preserves backward-compatible behavior
//     on flat networks where the dependency may not be present.
//   - Components whose Namespace is empty after recipe resolution are treated
//     as missing (a dependency without a known namespace cannot produce a
//     well-formed PodAffinityTerm).
//
// Note: required only verifies the component is present in the resolved recipe
// (with IsEnabled() and a non-empty namespace). It does NOT check runtime
// readiness — if the dependency pods have not yet started or are crashlooping,
// the orchestrator pod will stay Pending until the Job's activeDeadlineSeconds
// fires. Operators triaging a hung run should inspect both the Job's pod
// PodScheduled condition and the dependency component's replica status.
//
// For pre-flight gates that only need to check resolvability, use
// ValidateDependencyAffinity to avoid allocating the full affinity tree.
func BuildOrchestratorAffinity(
	deps []DependencyAffinity,
	componentRefs []recipe.ComponentRef,
) (*corev1.Affinity, error) {

	affinity := preferCPUNodeAffinity()

	if len(deps) == 0 {
		return affinity, nil
	}

	required, preferred, err := resolveDeps(deps, componentRefs, true)
	if err != nil {
		return nil, err
	}

	if len(required) == 0 && len(preferred) == 0 {
		return affinity, nil
	}

	affinity.PodAffinity = &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution:  required,
		PreferredDuringSchedulingIgnoredDuringExecution: preferred,
	}
	return affinity, nil
}

// ValidateDependencyAffinity verifies that all dependencies resolve against
// componentRefs without constructing the affinity tree. Returns the same
// error class as BuildOrchestratorAffinity (ErrCodeInvalidRequest on any
// malformed entry or any missing required component); suppresses the
// slog.Warn that BuildOrchestratorAffinity emits for missing preferred
// dependencies so pre-flight gates don't duplicate the build-time warning.
//
// Note: only checks recipe membership (componentRef present, enabled, with a
// resolved namespace). Does NOT verify the dependency's pods are actually
// running — see the runtime-readiness note on BuildOrchestratorAffinity.
func ValidateDependencyAffinity(
	deps []DependencyAffinity,
	componentRefs []recipe.ComponentRef,
) error {

	if len(deps) == 0 {
		return nil
	}
	_, _, err := resolveDeps(deps, componentRefs, false)
	return err
}

// resolveDeps walks deps and componentRefs, returning the required and
// preferred PodAffinityTerm slices. emitWarnings controls whether
// missing-preferred-component cases log slog.Warn (true for the build path,
// false for the pre-flight validation path so warnings don't double-fire).
func resolveDeps(
	deps []DependencyAffinity,
	componentRefs []recipe.ComponentRef,
	emitWarnings bool,
) ([]corev1.PodAffinityTerm, []corev1.WeightedPodAffinityTerm, error) {

	refByName := make(map[string]recipe.ComponentRef, len(componentRefs))
	for _, ref := range componentRefs {
		refByName[ref.Name] = ref
	}

	var required []corev1.PodAffinityTerm
	var preferred []corev1.WeightedPodAffinityTerm

	for _, dep := range deps {
		if err := dep.Validate(); err != nil {
			return nil, nil, errors.PropagateOrWrap(err,
				errors.ErrCodeInvalidRequest, "invalid dependencyAffinity")
		}

		ref, found := refByName[dep.ComponentRef]
		// Skip if the component is in the recipe but disabled or unresolved
		// (no namespace). Without this check, "required" would block the
		// orchestrator forever waiting for a pod that never deploys.
		resolved := found && ref.IsEnabled() && ref.Namespace != ""
		req := dep.RequirementOrDefault()

		if !resolved {
			if req == DependencyRequirementRequired {
				var reason string
				switch {
				case !found:
					reason = "is not in the recipe's componentRefs"
				case !ref.IsEnabled():
					reason = "is disabled (overrides.enabled=false)"
				case ref.Namespace == "":
					reason = "has no resolved namespace"
				default:
					reason = "is unresolved"
				}
				return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("required dependencyAffinity component %q %s; either fix the recipe or remove this validator from the validation phase",
						dep.ComponentRef, reason))
			}
			if emitWarnings {
				slog.Warn("preferred dependencyAffinity component not present in recipe; skipping affinity term",
					"componentRef", dep.ComponentRef)
			}
			continue
		}

		term := corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchLabels: dep.PodLabelSelector},
			Namespaces:    []string{ref.Namespace},
			TopologyKey:   dep.TopologyKeyOrDefault(),
		}

		if req == DependencyRequirementRequired {
			required = append(required, term)
		} else {
			preferred = append(preferred, corev1.WeightedPodAffinityTerm{
				Weight:          preferredAffinityWeight,
				PodAffinityTerm: term,
			})
		}
	}

	return required, preferred, nil
}

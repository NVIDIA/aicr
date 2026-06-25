// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Registry maps a GroupVersionKind to its registered Validator. A GVK
// without a registered Validator falls back to existence-only checking
// (see DefaultExistenceValidator).
type Registry struct {
	validators map[schema.GroupVersionKind]Validator
}

// NewRegistry returns an empty registry. Callers must register validators
// manually. Prefer NewDefault for the standard built-in set.
func NewRegistry() *Registry {
	return &Registry{validators: make(map[schema.GroupVersionKind]Validator)}
}

// NewDefault returns a Registry pre-populated with all built-in validators.
func NewDefault() *Registry {
	r := NewRegistry()
	registerStatusStringValidators(r)
	registerSriovValidators(r)
	registerNicConfigValidators(r)
	registerSpectrumXValidators(r)
	return r
}

// Register associates a Validator with a GVK. Re-registering overwrites
// the previous entry; this is intentional so tests can swap validators.
func (r *Registry) Register(gvk schema.GroupVersionKind, v Validator) {
	r.validators[gvk] = v
}

// Validate classifies obj's current cluster state. If no validator is
// registered for obj's GVK, DefaultExistenceValidator runs instead.
func (r *Registry) Validate(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
	if obj == nil {
		return Result{}, fmt.Errorf("crstate.Validate: nil object")
	}
	gvk := obj.GroupVersionKind()
	if v, ok := r.validators[gvk]; ok {
		return v(ctx, c, obj)
	}
	return DefaultExistenceValidator(ctx, c, obj)
}

// NeedsObservationGate reports whether a CR's authoritative status
// lives on the object itself (vs on a companion CR). The deploy
// state machine uses this to decide whether the post-apply
// resourceVersion bump is a meaningful "controller has reacted"
// signal:
//
//   - true  — validator reads `obj.status.*` directly (NCP/NNP/the
//     three Mellanox Network Kinds, SpectrumXRailPoolConfig
//     v1alpha2). Until the controller writes status back the RV
//     stays at the apply-time value, and the validator would return
//     stale data from the previous reconcile. Gate the verdict
//     until live RV moves past apply-time RV.
//
//   - false — validator reads companion CRs whose lifecycle is
//     independent of the apply (SriovNetworkNodePolicy →
//     SriovNetworkNodeState per node; NicInterfaceNameTemplate /
//     NicConfigurationTemplate → NicDevice per device). The
//     companion's RV evolves on its own schedule and the
//     SriovNetworkNodePolicy itself never gets a status write, so
//     gating on its RV would block forever. Validators in this
//     bucket get to give an immediate verdict — staleness is their
//     own problem to detect (and the SR-IOV validator does, by
//     bucketing the "Succeeded but numVfs not at target" case as
//     in-progress per the SR-IOV soft-progress rule).
//
// IPPool / CIDRPool / SriovNetwork / SriovIBNetwork / OVSNetwork
// fall through to the default existence-only validator and also
// don't need the gate — there's no status to read at all.
func NeedsObservationGate(gvk schema.GroupVersionKind) bool {
	if gvk.Group == "mellanox.com" && gvk.Version == "v1alpha1" {
		switch gvk.Kind {
		case "NicClusterPolicy", "NicNodePolicy",
			"HostDeviceNetwork", "IPoIBNetwork", "MacvlanNetwork":
			return true
		}
	}
	if gvk.Group == spcxGroup && gvk.Version == spcxVersionAlpha2 && gvk.Kind == spcxKindRailPoolConfig {
		return true
	}
	return false
}

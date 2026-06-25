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

// Package crstate provides a per-Kind validator registry that classifies
// a Kubernetes manifest's current cluster state into one of four states:
// not-deployed, error, in-progress, success.
//
// The registry is consumed by both `l8k deploy` (state machine: apply,
// wait until success or error) and `l8k validate` (one-shot classification).
package crstate

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CRState is the four-state classification produced by Validator
// implementations.
type CRState string

const (
	// StateNotDeployed means the object is absent from the cluster.
	StateNotDeployed CRState = "not-deployed"
	// StateError means the object is in a terminal failure state.
	StateError CRState = "error"
	// StateInProgress means the object is reconciling and has not yet
	// reached a terminal state.
	StateInProgress CRState = "in-progress"
	// StateSuccess means the object is reconciled and healthy.
	StateSuccess CRState = "success"
)

// Result describes the outcome of a Validator call. Reason is a
// short human-readable summary; Details carries structured per-companion
// information (e.g. per-node syncStatus for SR-IOV) for richer reports.
// Source identifies the object that produced the result (Kind/Name) for
// log breadcrumbs.
type Result struct {
	State   CRState
	Reason  string
	Details map[string]string
	Source  string
}

// Validator inspects a manifest object plus whatever companion CRs it
// needs to classify the deployed state.
//
// A nil error signals an authoritative result (whatever State carries).
// A non-nil error signals a transport-layer failure (network, RBAC,
// API server unreachable) — callers should retry rather than treat the
// returned Result as authoritative.
type Validator func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error)

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

// Package validator provides a container-per-validator execution engine
// for AICR cluster validation. Each validator is an OCI container image
// run as a Kubernetes Job, communicating results via exit codes and
// termination messages.
package validator

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// Validator orchestrates validation runs using containerized validators.
type Validator struct {
	// Version is the validator version (typically the CLI version).
	Version string

	// Namespace is the Kubernetes namespace for validation Jobs.
	Namespace string

	// RunID is a unique identifier for this validation run.
	RunID string

	// Cleanup controls whether to delete Jobs, ConfigMaps, and RBAC after validation.
	Cleanup bool

	// ImagePullSecrets are secret names for pulling validator images.
	ImagePullSecrets []string

	// NoCluster controls whether to skip cluster operations (dry-run mode).
	NoCluster bool

	// Tolerations are applied to validator Jobs for scheduling.
	Tolerations []corev1.Toleration

	// NodeSelector constrains validator Jobs to nodes matching these labels.
	NodeSelector map[string]string
}

// PhaseResult is the outcome of running all validators in a single phase.
type PhaseResult struct {
	// Phase is the phase that was executed.
	Phase Phase

	// Status is the overall phase status derived from the CTRF summary.
	Status string

	// Report is the CTRF report for this phase.
	Report *ctrf.Report

	// Duration is the wall-clock time for the entire phase.
	Duration time.Duration
}

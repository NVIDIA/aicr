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

package validator

import corev1 "k8s.io/api/core/v1"

// Option is a functional option for configuring Validator instances.
type Option func(*Validator)

// WithVersion sets the validator version string (typically the CLI version).
func WithVersion(version string) Option {
	return func(v *Validator) {
		v.Version = version
	}
}

// WithNamespace sets the Kubernetes namespace for validation Jobs.
// Default: "aicr-validation".
func WithNamespace(namespace string) Option {
	return func(v *Validator) {
		v.Namespace = namespace
	}
}

// WithRunID sets the RunID for this validation run.
// Used when resuming a previous run.
func WithRunID(runID string) Option {
	return func(v *Validator) {
		v.RunID = runID
	}
}

// WithCleanup controls whether to delete Jobs, ConfigMaps, and RBAC after validation.
// Default: true.
func WithCleanup(cleanup bool) Option {
	return func(v *Validator) {
		v.Cleanup = cleanup
	}
}

// WithImagePullSecrets sets image pull secrets for validator Jobs.
func WithImagePullSecrets(secrets []string) Option {
	return func(v *Validator) {
		v.ImagePullSecrets = secrets
	}
}

// WithNoCluster controls cluster access. When true, all validators are reported
// as skipped and no K8s API calls are made. Default: false.
func WithNoCluster(noCluster bool) Option {
	return func(v *Validator) {
		v.NoCluster = noCluster
	}
}

// WithTolerations sets tolerations for validator Jobs.
// Default: tolerate-all.
func WithTolerations(tolerations []corev1.Toleration) Option {
	return func(v *Validator) {
		v.Tolerations = tolerations
	}
}

// WithNodeSelector sets node selector labels for validator Jobs.
// When set, validator Jobs will only be scheduled on nodes matching these labels.
func WithNodeSelector(nodeSelector map[string]string) Option {
	return func(v *Validator) {
		v.NodeSelector = nodeSelector
	}
}

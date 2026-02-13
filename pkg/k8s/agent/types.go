// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// clusterRoleName is the name used for the ClusterRole and ClusterRoleBinding.
const clusterRoleName = "eidos-node-reader"

// Config holds the configuration for deploying the agent.
type Config struct {
	Namespace          string
	ServiceAccountName string
	JobName            string
	Image              string
	ImagePullSecrets   []string
	NodeSelector       map[string]string
	Tolerations        []corev1.Toleration
	Output             string
	Debug              bool
	Privileged         bool // If true, run with privileged security context (required for GPU/SystemD collectors)
	RequireGPU         bool // If true, request nvidia.com/gpu resource (required for CDI environments)
}

// Deployer manages the deployment and lifecycle of the agent Job.
type Deployer struct {
	clientset kubernetes.Interface
	config    Config
}

// NewDeployer creates a new agent Deployer with the given configuration.
func NewDeployer(clientset kubernetes.Interface, config Config) *Deployer {
	return &Deployer{
		clientset: clientset,
		config:    config,
	}
}

// CleanupOptions controls what resources to remove during cleanup.
type CleanupOptions struct {
	Enabled bool // If true, removes Job and all RBAC resources
}

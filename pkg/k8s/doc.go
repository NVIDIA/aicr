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

// Package k8s provides Kubernetes integration for Cloud Native Stack.
//
// This package contains sub-packages for Kubernetes cluster interaction:
//
// # Sub-packages
//
// client: Singleton Kubernetes client with automatic authentication
//
//	clientset, config, err := client.GetKubeClient()
//	if err != nil {
//	    return err
//	}
//	// Use clientset for API operations
//
// agent: Kubernetes Job deployment for automated snapshot capture
//
//	deployer := agent.NewDeployer(clientset, agentConfig)
//	if err := deployer.Deploy(ctx); err != nil {
//	    return err
//	}
//
// # Architecture
//
// The k8s package follows these design principles:
//
//   - Singleton Pattern: The client package uses sync.Once to ensure a single
//     Kubernetes client instance is shared across the application, preventing
//     connection exhaustion and reducing API server load.
//
//   - Automatic Authentication: The client automatically detects whether it's
//     running in-cluster (using service account) or out-of-cluster (using
//     kubeconfig file).
//
//   - Job-based Agent: The agent package deploys snapshot capture as a
//     Kubernetes Job, enabling GPU node targeting and ConfigMap-based output
//     storage.
//
// # Usage Patterns
//
// For most use cases, import and use the client sub-package:
//
//	import "github.com/NVIDIA/eidos/pkg/k8s/client"
//
//	// Get shared client instance
//	clientset, _, err := client.GetKubeClient()
//
// For agent deployment, import the agent sub-package:
//
//	import "github.com/NVIDIA/eidos/pkg/k8s/agent"
//
//	// Deploy snapshot agent
//	config := agent.Config{
//	    Namespace: "gpu-operator",
//	    Image:     "ghcr.io/nvidia/eidos-validator:latest",
//	}
//	deployer := agent.NewDeployer(clientset, config)
//
// # Thread Safety
//
// Both sub-packages are designed for concurrent use:
//   - client: Uses sync.Once for thread-safe initialization
//   - agent: Each Deployer instance is independent
package k8s

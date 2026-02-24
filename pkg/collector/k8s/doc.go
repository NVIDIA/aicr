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

// Package k8s collects Kubernetes cluster configuration data.
//
// This collector gathers comprehensive cluster information including node
// details, server version, deployed container images, and GPU Operator
// ClusterPolicy configuration.
//
// # Collected Data
//
// The collector returns a measurement with 6 subtypes:
//
// 1. node - Node information:
//   - provider: Cloud provider (EKS, GKE, AKS, etc.) detected from node labels
//   - kernelVersion: Linux kernel version
//   - osImage: Operating system description
//   - containerRuntime: Runtime and version (containerd, cri-o, docker)
//   - architecture: CPU architecture (amd64, arm64)
//   - hostname: Node name
//
// 2. server - Kubernetes server information:
//   - version: Kubernetes version with vendor suffix (e.g., v1.33.5-eks-3025e55)
//   - goVersion: Go version used to build Kubernetes
//   - platform: OS/Architecture (linux/amd64)
//
// 3. image - Deployed container images:
//   - Kubernetes core images (kube-apiserver, kube-controller-manager, etc.)
//   - GPU Operator images (nvidia-driver, device-plugin, dcgm-exporter, etc.)
//   - Network Operator images (ofed-driver, rdma-cni, etc.)
//   - Application images from running pods
//
// 4. policy - GPU Operator ClusterPolicy:
//   - Complete ClusterPolicy spec if GPU Operator is installed
//   - Driver configuration (version, repository, image pull policy)
//   - Toolkit configuration (version, repository)
//   - Device plugin settings (arguments, resources)
//   - DCGM exporter configuration
//   - MIG manager settings (mode, strategy)
//   - Node feature discovery configuration
//
// 5. helm - Helm releases installed via helm install/upgrade:
//   - Release metadata (name, namespace, revision, status, chart, version)
//   - User-supplied values flattened to dot-notation (e.g., driver.version)
//   - Deduplicated to latest revision per release name+namespace
//
// 6. argocd - ArgoCD Application CRDs (argoproj.io/v1alpha1):
//   - Application metadata (name, namespace, project)
//   - Destination (target namespace, server URL)
//   - Source configuration (repoURL, chart, path, targetRevision)
//   - Multi-source apps indexed by position (sources.0, sources.1, etc.)
//   - Helm parameters and values from Application spec
//   - Sync status (Synced/OutOfSync) and health status (Healthy/Degraded/Progressing)
//   - Gracefully skipped if ArgoCD is not installed
//
// # Usage
//
// Create and use the collector:
//
//	collector := k8s.NewCollector()
//	measurements, err := collector.Collect(ctx)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	for _, m := range measurements {
//	    for _, subtype := range m.Subtypes {
//	        fmt.Printf("%s: %d items\n", subtype.Name, len(subtype.Data))
//	    }
//	}
//
// # Kubernetes Client
//
// The collector uses the centralized Kubernetes client from pkg/k8s/client:
//
//	import "github.com/NVIDIA/aicr/pkg/k8s/client"
//
//	// Automatically uses in-cluster config when running as pod, or kubeconfig
//	clientset, config, err := client.GetKubeClient()
//
// The client is cached using sync.Once for efficient reuse across multiple
// collector calls. The k8s/client package handles both in-cluster (service account)
// and out-of-cluster (kubeconfig) authentication automatically.
//
// # Provider Detection
//
// Cloud provider is detected from node labels:
//   - EKS: eks.amazonaws.com/nodegroup
//   - GKE: cloud.google.com/gke-nodepool
//   - AKS: kubernetes.azure.com/cluster
//   - OKE: node.info.ds.oke
//   - Self-managed: No provider-specific labels
//
// # Context Support
//
// The collector respects context cancellation and timeouts:
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//
//	measurements, err := collector.Collect(ctx)
//
// # Error Handling
//
// The collector continues on non-critical errors:
//   - No ClusterPolicy found: Omits policy subtype
//   - No nodes found: Returns error
//   - API server unreachable: Returns error
//
// Partial data is returned when possible.
//
// # In-Cluster vs Out-of-Cluster
//
// The collector works in both modes:
//
// In-cluster (running as Kubernetes Job/Pod):
//   - Uses service account credentials
//   - Reads from /var/run/secrets/kubernetes.io/serviceaccount
//   - Requires appropriate RBAC permissions
//
// Out-of-cluster (running on workstation):
//   - Uses kubeconfig from ~/.kube/config or KUBECONFIG env var
//   - Requires cluster access and proper authentication
//
// # RBAC Requirements
//
// The collector requires these permissions:
//
//	apiVersion: rbac.authorization.k8s.io/v1
//	kind: ClusterRole
//	metadata:
//	  name: aicr-collector
//	rules:
//	- apiGroups: [""]
//	  resources: ["nodes", "pods", "secrets"]
//	  verbs: ["get", "list"]
//	- apiGroups: ["nvidia.com"]
//	  resources: ["clusterpolicies"]
//	  verbs: ["get", "list"]
//	- apiGroups: ["argoproj.io"]
//	  resources: ["applications"]
//	  verbs: ["get", "list"]
//
// # Use in Recipes
//
// Recipe generation uses Kubernetes data for:
//   - Provider-specific optimizations (EKS, GKE, AKS)
//   - Kubernetes version compatibility checks
//   - GPU Operator configuration recommendations
//   - Container image version management
//   - Runtime-specific tuning (containerd vs cri-o)
package k8s

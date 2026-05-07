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

// Package client provides a singleton Kubernetes client for efficient cluster interactions.
//
// This package centralizes Kubernetes API access using a singleton pattern with sync.Once
// to prevent connection exhaustion and reduce load on the Kubernetes API server.
// The client is shared across all components that need Kubernetes access, including
// collectors and ConfigMap serializers.
//
// # Singleton Pattern
//
// The client is initialized once on first use and cached for subsequent calls:
//
//	import "github.com/NVIDIA/aicr/pkg/k8s/client"
//
//	clientset, config, err := client.GetKubeClient()
//	if err != nil {
//	    return fmt.Errorf("failed to get kubernetes client: %w", err)
//	}
//
//	// Use clientset for API operations
//	pods, err := clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
//
// # Custom Kubeconfig Path
//
// For cases where you need to specify a custom kubeconfig file instead of using
// the singleton with automatic discovery, use BuildKubeClient directly:
//
//	clientset, config, err := client.BuildKubeClient("/path/to/custom/kubeconfig")
//	if err != nil {
//	    return fmt.Errorf("failed to build kubernetes client: %w", err)
//	}
//
// Note: This creates a new client instance and bypasses the singleton cache.
// Use GetKubeClient for most cases; only use BuildKubeClient when you need
// explicit control over the kubeconfig source.
//
// # Authentication Modes
//
// The client automatically handles both in-cluster and out-of-cluster authentication:
//
// In-cluster (running as Kubernetes Pod/Job):
//   - Uses service account credentials from /var/run/secrets/kubernetes.io/serviceaccount/
//   - Automatically configured when running inside a Kubernetes cluster
//   - No additional configuration required
//
// Out-of-cluster (running locally or on non-K8s host):
//   - Checks KUBECONFIG environment variable first
//   - Falls back to ~/.kube/config if KUBECONFIG not set
//   - Returns error if no valid kubeconfig found
//
// # Usage Patterns
//
// Collector usage:
//
//	type Collector struct {
//	    ClientSet  kubernetes.Interface
//	    RestConfig *rest.Config
//	}
//
//	func (c *Collector) Collect(ctx context.Context) error {
//	    if c.ClientSet == nil {
//	        var err error
//	        c.ClientSet, c.RestConfig, err = client.GetKubeClient()
//	        if err != nil {
//	            return fmt.Errorf("failed to get kubernetes client: %w", err)
//	        }
//	    }
//	    // Use c.ClientSet for API operations
//	}
//
// ConfigMap serializer usage:
//
//	func WriteToConfigMap(ctx context.Context, data []byte) error {
//	    clientset, _, err := client.GetKubeClient()
//	    if err != nil {
//	        return fmt.Errorf("failed to get kubernetes client: %w", err)
//	    }
//
//	    cm := &corev1.ConfigMap{...}
//	    _, err = clientset.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
//	    return err
//	}
//
// # Benefits
//
// Connection Reuse:
//   - Single client instance prevents connection exhaustion
//   - Reduces load on Kubernetes API server
//   - Improves performance for repeated API calls
//
// Thread Safety:
//   - sync.Once ensures exactly one initialization
//   - Safe for concurrent use across goroutines
//   - No mutex locks required for read operations
//
// Automatic Configuration:
//   - Works in both in-cluster and out-of-cluster contexts
//   - No manual configuration needed for common scenarios
//   - Follows standard Kubernetes client-go patterns
//
// # Error Handling
//
// The client returns errors in these scenarios:
//   - No valid kubeconfig found (out-of-cluster)
//   - Service account not mounted (in-cluster)
//   - Invalid kubeconfig format
//   - Network connectivity issues
//
// Example error handling:
//
//	clientset, _, err := client.GetKubeClient()
//	if err != nil {
//	    log.Error("failed to get kubernetes client",
//	        "error", err,
//	        "kubeconfig", os.Getenv("KUBECONFIG"))
//	    return err
//	}
//
// # Testing
//
// For testing, use kubernetes client-go fake clients:
//
//	import (
//	    "k8s.io/client-go/kubernetes/fake"
//	)
//
//	func TestCollector(t *testing.T) {
//	    fakeClient := fake.NewSimpleClientset()
//	    collector := &Collector{
//	        ClientSet: fakeClient,
//	    }
//	    // Test collector without real Kubernetes API
//	}
//
// # Package Location
//
// This package was refactored from pkg/collector/k8s to provide centralized
// Kubernetes client access for all components. All imports should now use:
//
//	import "github.com/NVIDIA/aicr/pkg/k8s/client"
//
// See also:
//   - pkg/collector/k8s - K8s collector using this client
//   - pkg/serializer/configmap.go - ConfigMap serializer using this client
//   - pkg/serializer/reader.go - ConfigMap reader using this client
package client

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

package client

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Interface is an alias for kubernetes.Interface to allow easier mocking in tests.
// This enables using fake.NewSimpleClientset() which returns kubernetes.Interface.
type Interface = kubernetes.Interface

var (
	clientOnce   sync.Once
	cachedClient *kubernetes.Clientset
	cachedConfig *rest.Config
	clientErr    error
)

// GetKubeClient returns a singleton Kubernetes client, creating it on first call.
// Subsequent calls return the cached client for connection reuse and reduced overhead.
// This prevents connection exhaustion and reduces load on the Kubernetes API server.
//
// The client automatically discovers configuration from:
//   - KUBECONFIG environment variable
//   - ~/.kube/config (default location)
//   - In-cluster service account (when running as Kubernetes Pod)
//
// For custom kubeconfig paths, use GetKubeClientWithConfig.
func GetKubeClient() (Interface, *rest.Config, error) {
	clientOnce.Do(func() {
		cachedClient, cachedConfig, clientErr = BuildKubeClient("")
	})
	return cachedClient, cachedConfig, clientErr
}

// BuildKubeClient creates a Kubernetes client from the given kubeconfig file.
//
// This function is exported to allow direct client creation with a specific
// kubeconfig path, bypassing the singleton cache. Use GetKubeClient for most
// cases; only use BuildKubeClient when you need explicit control over the
// kubeconfig source (e.g., multi-cluster operations, testing with different configs).
//
// Parameters:
//   - kubeconfig: Path to kubeconfig file. If empty, uses automatic discovery:
//     1. KUBECONFIG environment variable
//     2. ~/.kube/config (if it exists)
//     3. In-cluster configuration (service account)
//
// Returns:
//   - *kubernetes.Clientset: The Kubernetes client
//   - *rest.Config: The rest configuration used to create the client
//   - error: Any error encountered during client creation
//
// Example with custom kubeconfig:
//
//	clientset, config, err := client.BuildKubeClient("/path/to/custom/kubeconfig")
//	if err != nil {
//	    return fmt.Errorf("failed to build client: %w", err)
//	}
func BuildKubeClient(kubeconfig string) (*kubernetes.Clientset, *rest.Config, error) {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")

		if kubeconfig == "" {
			kubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
			if _, err = os.Stat(kubeconfig); os.IsNotExist(err) {
				kubeconfig = ""
			}
		}
	}

	// Use InClusterConfig directly when no kubeconfig is available
	// This avoids the warning: "Neither --kubeconfig nor --master was specified"
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to get in-cluster config", err)
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, nil, errors.WrapWithContext(errors.ErrCodeInternal, "failed to build kube config", err, map[string]interface{}{
				"kubeconfig": kubeconfig,
			})
		}
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
	}

	return client, config, nil
}

// GetKubeClientWithConfig is a convenience wrapper around BuildKubeClient
// that returns the Interface type for compatibility with agent.Deployer.
// This is the recommended function for CLI commands that need custom kubeconfig paths.
//
// Parameters:
//   - kubeconfig: Path to kubeconfig file
//
// Returns:
//   - Interface: The Kubernetes client interface
//   - *rest.Config: The rest configuration
//   - error: Any error encountered
func GetKubeClientWithConfig(kubeconfig string) (Interface, *rest.Config, error) {
	return BuildKubeClient(kubeconfig)
}

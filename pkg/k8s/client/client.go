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

package client

import (
	"os"
	"path/filepath"
	"strings"
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

	// Per-kubeconfig-path cache used by GetKubeClientWithConfig so a single
	// CLI invocation (e.g., validate: recipe read + snapshot read + ConfigMap
	// write) builds at most one client per distinct kubeconfig path instead
	// of N fresh TLS handshakes.
	//
	// Sized for CLI lifetime (a handful of entries, process exits in seconds).
	// Do NOT reach for this from long-lived daemon paths (`aicrd` server) without
	// revisiting bounds (max entries / TTL / eviction); the map is unbounded by
	// design here.
	pathClientMu    sync.Mutex
	pathClientCache = map[string]*cachedPathClient{}
)

// cachedPathClient holds a successfully-built client for a kubeconfig path.
// Errors are deliberately NOT cached: a transient EAGAIN, brief filesystem
// hiccup, or first-call token-rotation race must not pin the failure for the
// entire process lifetime. The cache is an optimization, not a circuit breaker.
type cachedPathClient struct {
	client Interface
	config *rest.Config
}

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

	// Treat whitespace-only paths as unset so a stray space in a CLI flag
	// or env var doesn't bypass the default discovery chain into a guaranteed
	// "stat   : no such file" error from clientcmd.
	kubeconfig = strings.TrimSpace(kubeconfig)

	if kubeconfig == "" {
		kubeconfig = strings.TrimSpace(os.Getenv("KUBECONFIG"))

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

// GetKubeClientWithConfig returns a Kubernetes client for the given kubeconfig
// path, caching successful results per distinct path so repeated calls within a
// single process (e.g., one CLI run that reads recipe + snapshot and writes a
// ConfigMap against the same kubeconfig) share one client and TLS handshake.
//
// Empty or whitespace-only paths delegate to GetKubeClient (the default-discovery
// singleton).
//
// Errors are NOT cached: a transient kubeconfig read failure or token-rotation
// race on first call must not pin the failure for the entire process lifetime.
// Callers retry naturally on the next invocation.
//
// Parameters:
//   - kubeconfig: Path to kubeconfig file. Empty or whitespace-only falls back to
//     default discovery via the singleton.
//
// Returns:
//   - Interface: The Kubernetes client interface
//   - *rest.Config: The rest configuration
//   - error: Any error encountered (recomputed every call until the first success)
func GetKubeClientWithConfig(kubeconfig string) (Interface, *rest.Config, error) {
	key := strings.TrimSpace(kubeconfig)
	if key == "" {
		return GetKubeClient()
	}

	pathClientMu.Lock()
	defer pathClientMu.Unlock()
	if entry, ok := pathClientCache[key]; ok {
		return entry.client, entry.config, nil
	}
	client, config, err := BuildKubeClient(key)
	if err != nil {
		return nil, nil, err
	}
	pathClientCache[key] = &cachedPathClient{client: client, config: config}
	return client, config, nil
}

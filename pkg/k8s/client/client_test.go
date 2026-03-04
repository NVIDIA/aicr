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
	"strings"
	"sync"
	"testing"
)

// TestBuildKubeClient_PathResolution tests the kubeconfig path resolution logic
// without attempting to connect to a cluster.
func TestBuildKubeClient_PathResolution(t *testing.T) {
	// Save original env and restore after test
	originalKubeconfig := os.Getenv("KUBECONFIG")
	defer func() {
		if originalKubeconfig != "" {
			os.Setenv("KUBECONFIG", originalKubeconfig)
		} else {
			os.Unsetenv("KUBECONFIG")
		}
	}()

	tests := []struct {
		name          string
		kubeconfigArg string
		kubeconfigEnv string
		wantErr       bool
		errorContains string
	}{
		{
			name:          "explicit invalid path",
			kubeconfigArg: "/nonexistent/path/to/kubeconfig",
			wantErr:       true,
			errorContains: "failed to build kube config",
		},
		{
			name:          "env var with invalid path",
			kubeconfigArg: "",
			kubeconfigEnv: "/nonexistent/env/kubeconfig",
			wantErr:       true,
			errorContains: "failed to build kube config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up environment
			if tt.kubeconfigEnv != "" {
				os.Setenv("KUBECONFIG", tt.kubeconfigEnv)
			} else {
				os.Unsetenv("KUBECONFIG")
			}

			_, _, err := BuildKubeClient(tt.kubeconfigArg)

			if (err != nil) != tt.wantErr {
				t.Errorf("BuildKubeClient() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil && tt.errorContains != "" {
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("BuildKubeClient() error = %v, want error containing %q", err, tt.errorContains)
				}
			}
		})
	}
}

// TestBuildKubeClient_AutoDiscovery tests auto-discovery behavior with empty path.
// This test doesn't assert success/failure since it depends on the environment
// (presence of ~/.kube/config, in-cluster config, etc.)
func TestBuildKubeClient_AutoDiscovery(t *testing.T) {
	// Save and restore KUBECONFIG env
	originalKubeconfig := os.Getenv("KUBECONFIG")
	defer func() {
		if originalKubeconfig != "" {
			os.Setenv("KUBECONFIG", originalKubeconfig)
		} else {
			os.Unsetenv("KUBECONFIG")
		}
	}()

	// Unset KUBECONFIG to test auto-discovery
	os.Unsetenv("KUBECONFIG")

	_, _, err := BuildKubeClient("")

	// Don't assert success or failure - just verify it completes without panic
	// and returns a consistent result
	if err != nil {
		t.Logf("BuildKubeClient() auto-discovery failed (no valid config found): %v", err)
	} else {
		t.Log("BuildKubeClient() auto-discovery succeeded (valid config found in ~/.kube/config or in-cluster)")
	}
}

// TestBuildKubeClient_ExplicitPath tests BuildKubeClient with an explicit kubeconfig path.
func TestBuildKubeClient_ExplicitPath(t *testing.T) {
	// Create a temporary invalid kubeconfig file to test error handling
	tmpDir := t.TempDir()
	invalidConfig := filepath.Join(tmpDir, "invalid-kubeconfig")

	if err := os.WriteFile(invalidConfig, []byte("invalid yaml content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, _, err := BuildKubeClient(invalidConfig)
	if err == nil {
		t.Error("BuildKubeClient() with invalid config should return error")
	}

	if !strings.Contains(err.Error(), "failed to build kube config") {
		t.Errorf("BuildKubeClient() error = %v, want error containing 'failed to build kube config'", err)
	}
}

// TestGetKubeClient_Singleton tests that GetKubeClient returns the same instance.
func TestGetKubeClient_Singleton(t *testing.T) {
	// Note: This test may fail in test environments without valid kubeconfig.
	// The important behavior is that it only attempts initialization once.

	// Reset the singleton BEFORE this test (normally you wouldn't do this,
	// but it's necessary for isolated testing)
	// WARNING: This is not thread-safe and should only be done in isolated tests
	clientOnce = sync.Once{}
	cachedClient = nil
	cachedConfig = nil
	clientErr = nil

	defer func() {
		// Reset singleton state after test
		clientOnce = sync.Once{}
		cachedClient = nil
		cachedConfig = nil
		clientErr = nil
	}()

	// First call
	client1, config1, err1 := GetKubeClient()

	// Second call
	client2, config2, err2 := GetKubeClient()

	// The key requirement: both calls should return the EXACT SAME results (singleton behavior)
	// This is true regardless of whether initialization succeeded or failed

	// Both calls should return the same error state
	if (err1 != nil) != (err2 != nil) {
		t.Errorf("GetKubeClient() error consistency: first call err=%v, second call err=%v", err1, err2)
	}

	// Both calls should return the same error value
	// nolint:errorlint // intentionally checking pointer equality (singleton pattern)
	if err1 != err2 {
		t.Errorf("GetKubeClient() should return same error instance: first=%v, second=%v", err1, err2)
	}

	// Both calls should return the same client instance (could be nil or non-nil)
	if client1 != client2 {
		t.Error("GetKubeClient() should return the same client instance")
	}

	// Both calls should return the same config instance (could be nil or non-nil)
	if config1 != config2 {
		t.Error("GetKubeClient() should return the same config instance")
	}
}

// TestGetKubeClient_CallsOnce tests that GetKubeClient only initializes once
// even when called multiple times concurrently.
func TestGetKubeClient_CallsOnce(t *testing.T) {
	// Reset singleton state
	defer func() {
		clientOnce = sync.Once{}
		cachedClient = nil
		cachedConfig = nil
		clientErr = nil
	}()

	// Call GetKubeClient multiple times concurrently
	const numGoroutines = 10
	results := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			client, _, _ := GetKubeClient()
			// Record whether client is non-nil (success) or nil (failure)
			results <- (client != nil)
		}()
	}

	// Collect results
	successCount := 0
	failCount := 0
	for i := 0; i < numGoroutines; i++ {
		if <-results {
			successCount++
		} else {
			failCount++
		}
	}

	// All goroutines should get the same result (all success or all failure)
	if successCount > 0 && failCount > 0 {
		t.Errorf("GetKubeClient() returned inconsistent results: %d successes, %d failures", successCount, failCount)
	}
}

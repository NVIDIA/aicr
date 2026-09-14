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
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func assertKubeconfigErrorContext(t *testing.T, err error, wantKubeconfig string) {
	t.Helper()

	var structuredErr *errors.StructuredError
	if !stderrors.As(err, &structuredErr) {
		t.Fatalf("error = %v, want *errors.StructuredError", err)
	}
	gotKubeconfig, ok := structuredErr.Context["kubeconfig"].(string)
	if !ok {
		t.Fatalf("error context kubeconfig = %v, want string", structuredErr.Context["kubeconfig"])
	}
	if gotKubeconfig != wantKubeconfig {
		t.Errorf("error context kubeconfig = %q, want %q", gotKubeconfig, wantKubeconfig)
	}
}

// TestBuildKubeClient_PathResolution tests the kubeconfig path resolution logic
// without attempting to connect to a cluster.
func TestBuildKubeClient_PathResolution(t *testing.T) {
	// t.Setenv automatically restores prior value via t.Cleanup; safer than
	// a manual save/restore that leaks env state if a subtest panics.
	t.Setenv("KUBECONFIG", os.Getenv("KUBECONFIG"))

	tests := []struct {
		name           string
		kubeconfigArg  string
		kubeconfigEnv  string
		wantErr        bool
		errorContains  string
		wantCode       errors.ErrorCode
		wantKubeconfig string
	}{
		{
			name:           "explicit invalid path",
			kubeconfigArg:  "  /nonexistent/path/to/kubeconfig  ",
			wantErr:        true,
			errorContains:  "failed to build kube config",
			wantCode:       errors.ErrCodeInvalidRequest,
			wantKubeconfig: "/nonexistent/path/to/kubeconfig",
		},
		{
			name:           "env var with invalid path",
			kubeconfigArg:  "",
			kubeconfigEnv:  "/nonexistent/env/kubeconfig",
			wantErr:        true,
			errorContains:  "failed to build kube config",
			wantCode:       errors.ErrCodeInvalidRequest,
			wantKubeconfig: "/nonexistent/env/kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv handles unset by passing empty string and restores
			// the value via t.Cleanup at subtest end.
			t.Setenv("KUBECONFIG", tt.kubeconfigEnv)
			if tt.kubeconfigEnv == "" {
				_ = os.Unsetenv("KUBECONFIG")
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
			if err != nil && tt.wantCode != "" && !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("BuildKubeClient() error = %v, want code %s", err, tt.wantCode)
			}
			if err != nil && tt.wantKubeconfig != "" {
				assertKubeconfigErrorContext(t, err, tt.wantKubeconfig)
			}
		})
	}
}

// TestBuildKubeClient_AutoDiscovery tests auto-discovery behavior with empty path.
// This test doesn't assert success/failure since it depends on the environment
// (presence of ~/.kube/config, in-cluster config, etc.)
func TestBuildKubeClient_AutoDiscovery(t *testing.T) {
	// t.Setenv restores prior value via t.Cleanup. Setting to empty does
	// not unset, so explicitly unset for the auto-discovery scenario; the
	// prior value is captured for restoration via t.Setenv.
	t.Setenv("KUBECONFIG", os.Getenv("KUBECONFIG"))
	if err := os.Unsetenv("KUBECONFIG"); err != nil {
		t.Fatalf("unset KUBECONFIG: %v", err)
	}

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
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("BuildKubeClient() error = %v, want ErrCodeInvalidRequest", err)
	}
	assertKubeconfigErrorContext(t, err, invalidConfig)
}

// TestBuildKubeClient_InvalidClientConfigReturnsInvalidRequest verifies that a
// kubeconfig which parses successfully but cannot initialize a Kubernetes
// client is still classified as caller input rather than an internal failure.
func TestBuildKubeClient_InvalidClientConfigReturnsInvalidRequest(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "invalid-client-config")
	content := `apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1
      certificate-authority-data: bm90IGEgcGVtIGNlcnRpZmljYXRl
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user:
      token: test
`
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write test kubeconfig: %v", err)
	}

	_, _, err := BuildKubeClient(kubeconfig)
	if err == nil {
		t.Fatal("BuildKubeClient() error = nil, want invalid request")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("BuildKubeClient() error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "failed to create kubernetes client from kubeconfig") {
		t.Errorf("BuildKubeClient() error = %v, want client construction failure", err)
	}
	assertKubeconfigErrorContext(t, err, kubeconfig)
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

// TestBuildKubeClient_WhitespaceTreatedAsEmpty verifies a stray space in a
// kubeconfig flag/env doesn't bypass the default-discovery chain into a
// guaranteed "stat   : no such file" error from clientcmd.
func TestBuildKubeClient_WhitespaceTreatedAsEmpty(t *testing.T) {
	t.Setenv("KUBECONFIG", "   ")
	// Pin HOME so a malformed real ~/.kube/config on the dev box can't trip
	// the "failed to build kube config" assertion below as a false positive.
	// USERPROFILE covers the Windows equivalent that homedir.HomeDir consults.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Whitespace-only KUBECONFIG must be treated like unset and fall through
	// to ~/.kube/config / in-cluster discovery. The clientcmd-specific error
	// "failed to build kube config" would mean we passed whitespace straight
	// through, which is exactly the regression we are guarding.
	_, _, err := BuildKubeClient("   ")
	if err != nil && strings.Contains(err.Error(), "failed to build kube config") {
		t.Errorf("whitespace kubeconfig was not normalized to empty: %v", err)
	}
}

// TestGetKubeClientWithConfig_ErrorsNotCached verifies that an invalid kubeconfig
// is retried on every call rather than memoized for the process lifetime — a
// transient first-call failure (EAGAIN, token-rotation race) must not become
// permanent. We assert the cache stays empty after error returns; the alternative
// (success caching) is verified end-to-end in the kind-cluster manual tests.
func TestGetKubeClientWithConfig_ErrorsNotCached(t *testing.T) {
	tmpDir := t.TempDir()
	invalidConfig := filepath.Join(tmpDir, "invalid-kubeconfig")
	if err := os.WriteFile(invalidConfig, []byte("invalid yaml content"), 0644); err != nil {
		t.Fatalf("failed to write test kubeconfig: %v", err)
	}

	t.Cleanup(func() {
		pathClientMu.Lock()
		delete(pathClientCache, invalidConfig)
		pathClientMu.Unlock()
	})

	client1, cfg1, err1 := GetKubeClientWithConfig(invalidConfig)
	if err1 == nil {
		t.Fatal("expected error from invalid kubeconfig, got nil")
	}
	if client1 != nil || cfg1 != nil {
		t.Errorf("expected nil client and config on error; got client=%v cfg=%v", client1, cfg1)
	}

	pathClientMu.Lock()
	_, cached := pathClientCache[invalidConfig]
	pathClientMu.Unlock()
	if cached {
		t.Error("error path populated the cache; transient failures must not be memoized")
	}

	// Second call must re-attempt (and re-fail the same way) — not short-circuit
	// on a stale cached error.
	_, _, err2 := GetKubeClientWithConfig(invalidConfig)
	if err2 == nil {
		t.Fatal("expected error from invalid kubeconfig on retry, got nil")
	}
}

// TestGetKubeClientWithConfig_EmptyDelegatesToSingleton verifies the empty
// (and whitespace-only) path takes the GetKubeClient branch rather than
// populating the per-path cache.
func TestGetKubeClientWithConfig_EmptyDelegatesToSingleton(t *testing.T) {
	t.Cleanup(func() {
		pathClientMu.Lock()
		pathClientCache = map[string]*cachedPathClient{}
		pathClientMu.Unlock()
	})

	// Discard the client/config; both inputs go through GetKubeClient whose
	// environment-dependent outcome we explicitly do not assert on (see
	// TestBuildKubeClient_AutoDiscovery). The assertion below is purely
	// about cache-key behavior.
	for _, kubeconfig := range []string{"", "   "} {
		if client, _, err := GetKubeClientWithConfig(kubeconfig); err == nil && client == nil {
			t.Errorf("GetKubeClientWithConfig(%q) succeeded with nil client", kubeconfig)
		}
	}

	pathClientMu.Lock()
	defer pathClientMu.Unlock()
	if len(pathClientCache) != 0 {
		t.Errorf("empty/whitespace kubeconfig polluted per-path cache: %d entries", len(pathClientCache))
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

	for range numGoroutines {
		go func() {
			client, _, _ := GetKubeClient()
			// Record whether client is non-nil (success) or nil (failure)
			results <- (client != nil)
		}()
	}

	// Collect results
	successCount := 0
	failCount := 0
	for range numGoroutines {
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

// writeKubeconfig writes a syntactically valid single-context kubeconfig whose
// cluster/user names are caller-chosen, so a merge across two files can be
// observed by which context wins.
func writeKubeconfig(t *testing.T, dir, file, name, server string) string {
	t.Helper()
	path := filepath.Join(dir, file)
	content := `apiVersion: v1
kind: Config
clusters:
  - name: ` + name + `
    cluster:
      server: ` + server + `
contexts:
  - name: ` + name + `
    context:
      cluster: ` + name + `
      user: ` + name + `
current-context: ` + name + `
users:
  - name: ` + name + `
    user:
      token: ` + name + `-token
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig %s: %v", path, err)
	}
	return path
}

// TestBuildKubeClient_MultiFileKUBECONFIG covers the local-execution regression
// behind the `gate` CLI's documented KUBECONFIG invocation. ResolveKubeconfigPath
// deliberately reports no path for a ':'-separated KUBECONFIG (it is a clientcmd
// loading-rules value, not a path), and BuildKubeClient then fell through to
// rest.InClusterConfig() — so any local run with KUBECONFIG=/a:/b failed with
// "unable to load in-cluster configuration". The chainsaw binary the gate
// replaced used clientcmd's merging loading rules, so this was a regression.
func TestBuildKubeClient_MultiFileKUBECONFIG(t *testing.T) {
	dir := t.TempDir()
	// The fixture is deliberately split so NEITHER file can satisfy the load
	// alone: the first names the current context but never defines it, and the
	// second defines that context plus the cluster/user it points at. A result
	// is only reachable if both files were read and merged. Two complete
	// kubeconfigs would not prove that — the first alone would produce the
	// same answer.
	first := filepath.Join(dir, "first")
	if err := os.WriteFile(first, []byte(`apiVersion: v1
kind: Config
current-context: merged-ctx
clusters:
  - name: decoy
    cluster:
      server: https://decoy.example:6443
`), 0o600); err != nil {
		t.Fatalf("write first kubeconfig: %v", err)
	}
	second := writeKubeconfig(t, dir, "second", "merged-ctx", "https://merged.example:6443")

	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)

	// Precondition: the resolver still reports no single path for this value.
	// If that ever changes, the branch under test becomes unreachable and this
	// test should be revisited rather than silently passing for a new reason.
	if got := ResolveKubeconfigPath(""); got != "" {
		t.Fatalf("ResolveKubeconfigPath() = %q, want %q for a multi-file KUBECONFIG", got, "")
	}

	_, config, err := BuildKubeClient("")
	if err != nil {
		t.Fatalf("BuildKubeClient() with a multi-file KUBECONFIG: %v", err)
	}
	// current-context came from the first file; the context, cluster, and user
	// it resolves to came from the second. Both had to load.
	if config.Host != "https://merged.example:6443" {
		t.Errorf("config.Host = %q, want the second file's server — the merge did not cover both files", config.Host)
	}
	if config.BearerToken != "merged-ctx-token" {
		t.Errorf("config.BearerToken = %q, want %q", config.BearerToken, "merged-ctx-token")
	}
}

// TestBuildKubeClient_MultiFileKUBECONFIGWhitespace guards the divergence
// between this package's classification and clientcmd's own loading rules.
// multiPathKubeconfigEnv trims the value (matching ResolveKubeconfigPath), but
// NewDefaultClientConfigLoadingRules splits os.Getenv("KUBECONFIG") verbatim —
// so with outer whitespace the raw split yields a filename that does not exist,
// Load() skips missing files non-fatally, and the merge silently drops it.
//
// The leading-space case is the dangerous one: before the Precedence override
// it resolved to the SECOND file's context, so a run targeted a different
// cluster than the operator named, with no error.
func TestBuildKubeClient_MultiFileKUBECONFIGWhitespace(t *testing.T) {
	dir := t.TempDir()
	first := writeKubeconfig(t, dir, "first", "first", "https://first.example:6443")
	second := writeKubeconfig(t, dir, "second", "second", "https://second.example:6443")
	joined := first + string(os.PathListSeparator) + second

	tests := []struct {
		name string
		env  string
	}{
		{"no whitespace", joined},
		{"leading whitespace", " " + joined},
		{"trailing whitespace", joined + " "},
		{"whitespace on both ends", "  " + joined + "  "},
		{"whitespace around each entry", " " + first + " " + string(os.PathListSeparator) + " " + second + " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tt.env)

			_, config, err := BuildKubeClient("")
			if err != nil {
				t.Fatalf("BuildKubeClient() with KUBECONFIG=%q: %v", tt.env, err)
			}
			// The first file's current-context wins in every case; anything
			// else means an entry was mangled and silently dropped.
			if config.Host != "https://first.example:6443" {
				t.Errorf("config.Host = %q, want the first file's server — an entry was dropped from the merge", config.Host)
			}
		})
	}
}

// TestKubeconfigPrecedence pins the normalization the loading rules are
// overridden with.
func TestKubeconfigPrecedence(t *testing.T) {
	sep := string(os.PathListSeparator)
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{"two paths", "/a" + sep + "/b", []string{"/a", "/b"}},
		{"outer whitespace", " /a" + sep + "/b ", []string{"/a", "/b"}},
		{"whitespace around entries", " /a " + sep + " /b ", []string{"/a", "/b"}},
		{"trailing separator drops the empty", "/a" + sep, []string{"/a"}},
		{"separators only", sep, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kubeconfigPrecedence(tt.value)
			if len(got) != len(tt.want) {
				t.Fatalf("kubeconfigPrecedence(%q) = %v, want %v", tt.value, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("kubeconfigPrecedence(%q)[%d] = %q, want %q", tt.value, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestBuildKubeClient_MultiFileKUBECONFIGErrorsAreCallerInput pins the error
// classification for the merged path. A file-derived config is caller input, so
// its failures are deterministic and non-retryable (ErrCodeInvalidRequest);
// only the in-cluster path is ErrCodeInternal. Keying on the resolved path
// alone would misclassify this branch, for which the resolver reports "".
func TestBuildKubeClient_MultiFileKUBECONFIGErrorsAreCallerInput(t *testing.T) {
	dir := t.TempDir()
	// Both files parse but neither names a context, so the merged result has
	// nothing to connect with and clientcmd reports an empty configuration.
	// (An unparsable file would not do: clientcmd's loading rules skip those
	// and merge on, which is its documented tolerance, not an error.)
	empty := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	t.Setenv("KUBECONFIG", empty("first")+string(os.PathListSeparator)+empty("second"))

	_, _, err := BuildKubeClient("")
	if err == nil {
		t.Fatal("BuildKubeClient() error = nil, want a rejection for an unparsable kubeconfig in the merge")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("BuildKubeClient() error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "multi-file KUBECONFIG") {
		t.Errorf("BuildKubeClient() error = %v, want it to name the multi-file source", err)
	}
}

// TestIsMultiPathKubeconfig locks the classification that routes between the
// three BuildKubeClient branches.
func TestIsMultiPathKubeconfig(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"single path", "/home/u/.kube/config", false},
		{"empty", "", false},
		{"unix separated", "/a:/b", true},
		// No ':' in the value, so this proves ';' alone is recognized.
		{"windows separated", `\a;\b`, true},
		{"trailing separator still names a list", "/a:", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMultiPathKubeconfig(tt.value); got != tt.want {
				t.Errorf("isMultiPathKubeconfig(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

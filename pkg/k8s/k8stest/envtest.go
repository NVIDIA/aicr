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

// Package k8stest provides test helpers that run against an envtest API
// server.
package k8stest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Both starters skip envtest-backed tests when the envtest binaries are
// unavailable. When the CI environment variable is set they fail instead,
// because CI installs the binaries, and a skip there would pass with nothing
// exercised.

// Start starts an envtest API server and stops it when t finishes.
func Start(t testing.TB) (*envtest.Environment, *rest.Config) {
	t.Helper()
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		if requiredInCI() {
			t.Fatalf("envtest is required in CI: %v", err)
		}
		t.Skipf("envtest not available (set KUBEBUILDER_ASSETS): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return env, cfg
}

// StartPackage starts one envtest API server for a package's TestMain and
// returns its config and a stop func to call after m.Run. When envtest is
// unavailable it returns a nil config, which tests read as a reason to skip,
// or exits the process when the CI environment variable is set.
func StartPackage() (*rest.Config, func()) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		if requiredInCI() {
			fmt.Fprintf(os.Stderr, "envtest is required in CI: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "envtest not available, integration tests will be skipped: %v\n", err)
		return nil, func() {}
	}
	return cfg, func() { _ = env.Stop() }
}

func requiredInCI() bool {
	return os.Getenv("CI") != ""
}

// ShellEnv returns environment entries that point a shell at env as user. They
// set KUBECONFIG to a kubeconfig for user and put envtest's own kubectl first
// on PATH. Append them to os.Environ() so they take precedence.
func ShellEnv(t testing.TB, env *envtest.Environment, cfg *rest.Config, user envtest.User) []string {
	t.Helper()
	added, err := env.AddUser(user, cfg)
	if err != nil {
		t.Fatalf("add envtest user %s: %v", user.Name, err)
	}
	kubeconfig, err := added.KubeConfig()
	if err != nil {
		t.Fatalf("kubeconfig for %s: %v", user.Name, err)
	}
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err = os.WriteFile(kubeconfigPath, kubeconfig, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	kubectl := env.ControlPlane.KubectlPath
	if kubectl == "" {
		t.Fatal("envtest did not resolve a kubectl binary")
	}
	bin := filepath.Join(dir, "bin")
	if err = os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err = os.Symlink(kubectl, filepath.Join(bin, "kubectl")); err != nil {
		t.Fatalf("link kubectl: %v", err)
	}
	return []string{
		"KUBECONFIG=" + kubeconfigPath,
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

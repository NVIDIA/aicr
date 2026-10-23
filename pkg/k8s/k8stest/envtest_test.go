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

//go:build unix

package k8stest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestShellEnvAuthenticatesAsUser runs kubectl the way a script would, so the
// shell resolves it from ShellEnv's PATH rather than the test's own.
func TestShellEnvAuthenticatesAsUser(t *testing.T) {
	env, cfg := Start(t)
	const user = "system:serviceaccount:k8stest:probe"
	shell := ShellEnv(t, env, cfg, envtest.User{Name: user})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c",
		`command -v kubectl && kubectl auth whoami -o jsonpath='{.status.userInfo.username}'`)
	cmd.Env = append(os.Environ(), shell...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl auth whoami: %v\n%s", err, out)
	}
	resolved, username, _ := strings.Cut(string(out), "\n")
	if want := filepath.Join(filepath.Dir(shellValue(t, shell, "KUBECONFIG")), "bin", "kubectl"); resolved != want {
		t.Errorf("shell resolved kubectl to %q, want envtest's link %q", resolved, want)
	}
	if username != user {
		t.Errorf("kubectl authenticated as %q, want %q", username, user)
	}
}

// TestShellEnvLinksRelativeKubectl covers a relative KUBEBUILDER_ASSETS, which
// envtest keeps relative in KubectlPath.
func TestShellEnvLinksRelativeKubectl(t *testing.T) {
	env, cfg := Start(t)
	abs, err := filepath.Abs(env.ControlPlane.KubectlPath)
	if err != nil {
		t.Fatalf("absolute kubectl path: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		t.Fatalf("relative kubectl path: %v", err)
	}
	env.ControlPlane.KubectlPath = rel

	shell := ShellEnv(t, env, cfg, envtest.User{Name: "relative"})
	link := filepath.Join(filepath.Dir(shellValue(t, shell, "KUBECONFIG")), "bin", "kubectl")
	if _, err = os.Stat(link); err != nil {
		t.Errorf("kubectl link does not resolve: %v", err)
	}
}

func TestStartPackageServesAPI(t *testing.T) {
	cfg, stop := StartPackage()
	if cfg == nil {
		t.Skip("envtest not available (set KUBEBUILDER_ASSETS)")
	}
	defer stop()
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = client.Discovery().ServerVersionWithContext(ctx); err != nil {
		t.Fatalf("server version: %v", err)
	}
}

func TestStartersSkipWithoutEnvtestOutsideCI(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("KUBEBUILDER_ASSETS", t.TempDir())

	cfg, stop := StartPackage()
	stop()
	if cfg != nil {
		t.Error("StartPackage returned a config without envtest binaries")
	}

	returned := false
	t.Run("Start", func(t *testing.T) {
		Start(t)
		returned = true
	})
	if returned {
		t.Error("Start returned without envtest binaries instead of skipping")
	}
}

func shellValue(t *testing.T, shell []string, name string) string {
	t.Helper()
	for _, entry := range shell {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	t.Fatalf("ShellEnv set no %s: %v", name, shell)
	return ""
}

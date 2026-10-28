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

package main

import (
	"context"
	stderrors "errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

type helmCall struct {
	timeout time.Duration
	args    string
}

// recordingRun returns run, which answers every call with out and err, and
// calls, which records each call to run.
func recordingRun(out string, err error) (run runFunc, calls *[]helmCall) {
	var recorded []helmCall
	return func(_ context.Context, timeout time.Duration, _ int64, args ...string) ([]byte, error) {
		recorded = append(recorded, helmCall{timeout: timeout, args: strings.Join(args, " ")})
		return []byte(out), err
	}, &recorded
}

func TestHelmCLIList(t *testing.T) {
	run, calls := recordingRun(`[{"name":"gpu-operator","namespace":"gpu-operator","status":"deployed"},{"name":"nodewright","namespace":"nodewright"}]`, nil)

	got, err := helmCLI{run: run}.list(context.Background())
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	want := []helmRelease{{Name: "gpu-operator", Namespace: "gpu-operator", Status: "deployed"}, {Name: "nodewright", Namespace: "nodewright"}}
	if !slices.Equal(got, want) {
		t.Errorf("list() = %v, want %v", got, want)
	}
	wantCalls := []helmCall{{
		defaults.K8sCleanupTimeout,
		"list --all-namespaces --output json --deployed --failed --pending --superseded --uninstalling --uninstalled --max 0",
	}}
	if !slices.Equal(*calls, wantCalls) {
		t.Errorf("helm calls = %v, want %v", *calls, wantCalls)
	}
}

func TestHelmCLIValues(t *testing.T) {
	run, calls := recordingRun(`{"driver":{"enabled":true}}`, nil)

	got, err := helmCLI{run: run}.values(context.Background(), helmRelease{Name: "gpu-operator", Namespace: "privileged-gpu-operator"})
	if err != nil {
		t.Fatalf("values() error = %v", err)
	}
	driver, _ := got["driver"].(map[string]any)
	if enabled, _ := driver["enabled"].(bool); !enabled {
		t.Errorf("values() = %v, want driver.enabled=true", got)
	}
	want := []helmCall{{defaults.K8sCleanupTimeout, "get values gpu-operator --namespace privileged-gpu-operator --all --output json"}}
	if !slices.Equal(*calls, want) {
		t.Errorf("helm calls = %v, want %v", *calls, want)
	}
}

func TestHelmCLIUninstall(t *testing.T) {
	run, calls := recordingRun("", nil)

	if err := (helmCLI{run: run}).uninstall(context.Background(), helmRelease{Name: "gpu-operator", Namespace: "gpu-operator"}); err != nil {
		t.Fatalf("uninstall() error = %v", err)
	}
	want := []helmCall{{
		defaults.ClusterCleanupHelmUninstallTimeout + defaults.K8sCleanupTimeout,
		"uninstall gpu-operator --namespace gpu-operator --wait --timeout " + defaults.ClusterCleanupHelmUninstallTimeout.String(),
	}}
	if !slices.Equal(*calls, want) {
		t.Errorf("helm calls = %v, want %v", *calls, want)
	}
}

func TestHelmCLIFailures(t *testing.T) {
	runErr := errors.New(errors.ErrCodeTimeout, "helm list did not finish")
	tests := []struct {
		name     string
		out      string
		err      error
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{"command error is returned as is", "", runErr, errors.ErrCodeTimeout, "helm list did not finish"},
		{"malformed output", "not json", nil, errors.ErrCodeInternal, "decode helm list output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, _ := recordingRun(tt.out, tt.err)

			_, err := helmCLI{run: run}.list(context.Background())
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("list() error = %v, want it to contain %q", err, tt.wantMsg)
			}
			if tt.err != nil && !stderrors.Is(err, tt.err) {
				t.Errorf("list() error = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestHelmCLIPinsKubeTarget(t *testing.T) {
	run, calls := recordingRun(`[]`, nil)
	h := helmCLI{run: run, kubeArgs: helmKubeArgs("/tmp/kubeconfig", "staging")}
	rel := helmRelease{Name: "gpu-operator", Namespace: "gpu-operator"}

	if _, err := h.list(context.Background()); err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if err := h.uninstall(context.Background(), rel); err != nil {
		t.Fatalf("uninstall() error = %v", err)
	}
	if _, err := h.values(context.Background(), rel); err == nil {
		t.Fatal("values() decoded a list as values")
	}
	const pinned = " --kubeconfig /tmp/kubeconfig --kube-context staging"
	if len(*calls) != 3 {
		t.Fatalf("helm calls = %v, want 3", *calls)
	}
	for _, call := range *calls {
		if !strings.HasSuffix(call.args, pinned) {
			t.Errorf("helm %s is not pinned with %q", call.args, pinned)
		}
	}
}

func TestHelmKubeArgs(t *testing.T) {
	tests := []struct {
		name        string
		kubeconfig  string
		kubeContext string
		want        []string
	}{
		{"file and context", "/tmp/kubeconfig", "staging", []string{"--kubeconfig", "/tmp/kubeconfig", "--kube-context", "staging"}},
		{"merged KUBECONFIG", "", "staging", []string{"--kube-context", "staging"}},
		{"in-cluster", "", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := helmKubeArgs(tt.kubeconfig, tt.kubeContext); !slices.Equal(got, tt.want) {
				t.Errorf("helmKubeArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func testRegistry(t *testing.T) *recipe.ComponentRegistry {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	return registry
}

func TestOwnedReleaseNames(t *testing.T) {
	registry := testRegistry(t)
	got := ownedReleaseNames(registry)
	for _, name := range []string{"gpu-operator", "gpu-operator-pre", "gpu-operator-post", "gpu-operator-readiness", "skyhook-operator", "dynamo-crds"} {
		if !got[name] {
			t.Errorf("ownedReleaseNames() is missing %q", name)
		}
	}
	if got["team-app"] {
		t.Error(`ownedReleaseNames() contains "team-app", which no component installs`)
	}

	for name, reason := range legacyComponents {
		if reason == "" {
			t.Errorf("legacyComponents[%q] has no reason", name)
		}
		if registry.Get(name) != nil {
			t.Errorf("legacyComponents lists %q, which is still a registry component", name)
		}
	}
}

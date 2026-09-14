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

package gatemanifest

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
)

const validReadinessTestYAML = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`

func TestRender(t *testing.T) {
	manifest, err := Render("gpu-operator", "ghcr.io/nvidia/aicr-gate:v1.2.3", []byte(validReadinessTestYAML), config.DeployerArgoCD)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(manifest)

	for _, want := range []string{
		"kind: ServiceAccount",
		"kind: ClusterRole",
		"kind: Job",
		"argocd.argoproj.io/sync-options: Replace=true,Force=true",
		"backoffLimit: 6",
		"customresourcedefinitions",
		`resources: ["*"]`,
		`  - apiGroups: ["operators.coreos.com"]
    resources: ["clusterserviceversions"]
    verbs: ["get", "list", "watch"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	if strings.Contains(got, "secrets") {
		t.Error("manifest must not grant secrets read")
	}
	// The mellanox.com rule is component-specific — it must NOT leak
	// into gpu-operator's gate SA (PR #2337 review). It appears only
	// for the network-operator component; see TestRender_NetworkOperator.
	if strings.Contains(got, "mellanox.com") {
		t.Errorf("gpu-operator manifest must not grant mellanox.com read; got:\n%s", got)
	}
	for _, want := range []string{
		"--timeout=" + defaults.ReadinessGateExecTimeout.String(),
		"--max-wait=" + defaults.ReadinessGateMaxWait.String(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing gate arg %q", want)
		}
	}
}

func TestRender_ClusterScopedNamesAreNamespaceQualified(t *testing.T) {
	got, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), config.DeployerArgoCD)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)

	// Cluster-scoped ClusterRole/ClusterRoleBinding/roleRef must carry the
	// namespace token suffix so same-component bundles in different namespaces
	// do not collide (#1011). The namespace token is left for bundle-time
	// resolution by manifest.Render.
	const qualified = "gpu-operator-readiness-gate-{{ .Release.Namespace }}"
	if want := strings.Count(s, "name: "+qualified); want != 3 {
		t.Errorf("expected 3 namespace-qualified cluster-scoped names (ClusterRole, ClusterRoleBinding, roleRef), got %d", want)
	}

	// The namespaced ServiceAccount subject and Job stay on the bare name —
	// identical names in distinct namespaces never collide. Match the exact
	// line so the suffixed form is not counted.
	const bare = "  name: gpu-operator-readiness-gate\n"
	if want := strings.Count(s, bare); want != 3 {
		t.Errorf("expected 3 bare namespaced names (ServiceAccount, subject, Job), got %d", want)
	}
}

func TestRender_HelmHooks(t *testing.T) {
	got, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), config.DeployerHelm)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		"helm.sh/hook: post-install,post-upgrade",
		"helm.sh/hook-delete-policy: before-hook-creation",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("helm manifest missing %q", want)
		}
	}
}

func TestRender_ArgoCDSyncOptions(t *testing.T) {
	// Replace=true alone maps to `kubectl replace`, which the API server
	// rejects on any upgrade that changes the Job spec because
	// spec.selector/spec.template.metadata.labels are immutable, leaving the
	// Application permanently OutOfSync (#2367). Force=true makes ArgoCD
	// delete-and-recreate on replace failure instead. Both ArgoCD deployer
	// branches (native and Helm-rendered) must emit the same annotation.
	// This Job-level annotation is only half the fix: see
	// TestBuildApplicationData_ApplyOutOfSyncOnly and
	// TestGenerate_ApplyOutOfSyncOnlySyncOptions in
	// pkg/bundler/deployer/argocd for the Application-level
	// ApplyOutOfSyncOnly=true entry that stops Force=true from
	// delete-and-recreating the Job on every no-op resync.
	tests := []struct {
		name     string
		deployer config.DeployerType
	}{
		{"argocd", config.DeployerArgoCD},
		{"argocd-helm", config.DeployerArgoCDHelm},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), tt.deployer)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			s := string(got)
			const want = "argocd.argoproj.io/sync-options: Replace=true,Force=true"
			if !strings.Contains(s, want) {
				t.Errorf("manifest for deployer %v missing %q", tt.deployer, want)
			}
			// Must not use a Helm-style sync hook: hook-annotated resources
			// are excluded from ArgoCD's normal drift detection (see
			// pkg/bundler/deployer/localformat/hooks.go's stripHelmHooks
			// doc), which could let an image-tag-only bump go undetected.
			if strings.Contains(s, "helm.sh/hook") {
				t.Errorf("ArgoCD deployer %v manifest must not use a Helm sync hook", tt.deployer)
			}
		})
	}
}

func TestRender_EmptyComponentName(t *testing.T) {
	if _, err := Render("", "img:tag", []byte("x"), config.DeployerHelm); err == nil {
		t.Fatal("expected error for empty component name")
	}
}

// TestRender_NetworkOperator pins the component-specific mellanox.com rule
// that componentClusterRoleRules injects only for Network Operator gates.
func TestRender_NetworkOperator(t *testing.T) {
	const mellanoxRule = `  - apiGroups: ["mellanox.com"]
    resources: ["nicclusterpolicies"]
    verbs: ["get", "list", "watch"]`
	for _, componentName := range []string{"network-operator", "network-operator-ocp"} {
		t.Run(componentName, func(t *testing.T) {
			got, err := Render(componentName, "img:tag", []byte(validReadinessTestYAML), config.DeployerArgoCD)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			s := string(got)
			if !strings.Contains(s, mellanoxRule) {
				t.Errorf("%s manifest missing mellanox.com rule:\n%s", componentName, s)
			}
			if strings.Count(s, `apiGroups: ["mellanox.com"]`) != 1 {
				t.Errorf("mellanox.com rule must appear exactly once in the ClusterRole; got:\n%s", s)
			}
		})
	}
}

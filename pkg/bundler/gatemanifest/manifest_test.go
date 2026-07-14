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
		"argocd.argoproj.io/sync-options: Replace=true",
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

func TestRender_EmptyComponentName(t *testing.T) {
	if _, err := Render("", "img:tag", []byte("x"), config.DeployerHelm); err == nil {
		t.Fatal("expected error for empty component name")
	}
}

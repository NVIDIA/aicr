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
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const validReadinessTestYAML = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`

func TestRender(t *testing.T) {
	manifest, err := Render("gpu-operator", "ghcr.io/nvidia/aicr-gate:v1.2.3", []byte(validReadinessTestYAML), config.DeployerArgoCD, Placement{})
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
	got, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), config.DeployerArgoCD, Placement{})
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
	got, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), config.DeployerHelm, Placement{})
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
			got, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), tt.deployer, Placement{})
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
	if _, err := Render("", "img:tag", []byte("x"), config.DeployerHelm, Placement{}); err == nil {
		t.Fatal("expected error for empty component name")
	}
}

// renderedGateJob builds the placement for sched, renders the gate manifest
// with it, and decodes the Job, substituting the namespace template token so
// the document parses as YAML.
func renderedGateJob(t *testing.T, sched Scheduling) (batchv1.Job, string) {
	t.Helper()

	placement, err := NewPlacement(sched)
	if err != nil {
		t.Fatalf("NewPlacement: %v", err)
	}
	manifest, err := Render("gpu-operator", "img:tag", []byte(validReadinessTestYAML), config.DeployerHelm, placement)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	resolved := strings.ReplaceAll(string(manifest), "{{ .Release.Namespace }}", "gpu-operator")
	for _, doc := range strings.Split(resolved, "\n---\n") {
		if !strings.Contains(doc, "\nkind: Job\n") {
			continue
		}
		var job batchv1.Job
		if err := yaml.UnmarshalStrict([]byte(doc), &job); err != nil {
			t.Fatalf("decode Job: %v\n%s", err, doc)
		}
		return job, string(manifest)
	}
	t.Fatalf("no Job document in manifest:\n%s", manifest)
	return batchv1.Job{}, ""
}

// TestNewPlacement_SystemScheduling pins #2590: the gate Job carries the
// bundle's system node selector and keyed tolerations. A keyless toleration
// matches every taint, including not-ready, unreachable and unschedulable,
// so it is dropped rather than letting the gate bind to an unhealthy node.
func TestNewPlacement_SystemScheduling(t *testing.T) {
	seconds := int64(300)
	keyed := []corev1.Toleration{
		{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "system-workload", Effect: corev1.TaintEffectNoSchedule},
		{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
	}
	keyless := []corev1.Toleration{
		{Operator: corev1.TolerationOpExists},
		{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	withKeyless := []corev1.Toleration{keyless[0], keyed[0], keyed[1], keyless[1]}
	selector := map[string]string{"node.dgxc.nvidia.com/dedicated": "system-workload", "kubernetes.io/os": "linux"}

	tests := []struct {
		name            string
		sched           Scheduling
		wantTolerations []corev1.Toleration
	}{
		{"none", Scheduling{}, nil},
		{"selector only", Scheduling{NodeSelector: selector}, nil},
		{"keyed tolerations", Scheduling{Tolerations: keyed}, keyed},
		{"keyless tolerations dropped", Scheduling{Tolerations: withKeyless}, keyed},
		{"only keyless tolerations", Scheduling{Tolerations: keyless}, nil},
		{"selector and tolerations", Scheduling{NodeSelector: selector, Tolerations: withKeyless}, keyed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, raw := renderedGateJob(t, tt.sched)
			podSpec := job.Spec.Template.Spec
			if !reflect.DeepEqual(podSpec.NodeSelector, tt.sched.NodeSelector) {
				t.Errorf("nodeSelector = %v, want %v", podSpec.NodeSelector, tt.sched.NodeSelector)
			}
			if !reflect.DeepEqual(podSpec.Tolerations, tt.wantTolerations) {
				t.Errorf("tolerations = %+v, want %+v", podSpec.Tolerations, tt.wantTolerations)
			}
			if podSpec.ServiceAccountName != "gpu-operator-readiness-gate" || len(podSpec.Containers) != 1 {
				t.Errorf("scheduling fields displaced the pod spec: %+v", podSpec)
			}
			if len(tt.sched.NodeSelector) == 0 && strings.Contains(raw, "nodeSelector") {
				t.Errorf("unset selector must not render a nodeSelector key:\n%s", raw)
			}
			if len(tt.wantTolerations) == 0 && strings.Contains(raw, "tolerations") {
				t.Errorf("no keyed tolerations must not render a tolerations key:\n%s", raw)
			}
		})
	}
}

// TestRender_ActiveDeadline pins that the gate Job bounds itself: a pod that
// never schedules or never finishes fails the Job instead of holding a Helm
// hook or Argo CD sync open. The deadline must fall inside Helm's timeout so
// the Job, not Helm, reports the failure.
func TestRender_ActiveDeadline(t *testing.T) {
	job, _ := renderedGateJob(t, Scheduling{})
	want := int64((defaults.ReadinessGateMaxWait + defaults.ReadinessGateActiveDeadlineBuffer).Seconds())
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != want {
		t.Errorf("activeDeadlineSeconds = %v, want %d", job.Spec.ActiveDeadlineSeconds, want)
	}
	if defaults.ReadinessGateActiveDeadlineBuffer >= defaults.ReadinessGateHelmTimeoutBuffer {
		t.Errorf("ReadinessGateActiveDeadlineBuffer (%s) must be below ReadinessGateHelmTimeoutBuffer (%s)",
			defaults.ReadinessGateActiveDeadlineBuffer, defaults.ReadinessGateHelmTimeoutBuffer)
	}
}

// TestNewPlacement_RejectsInvalidScheduling pins that scheduling values are
// validated before being spliced into the manifest, which is later rendered
// as a Go template: a value carrying template syntax must fail, not execute.
// It also pins the API server's toleration rules, so a gate the server would
// reject fails at bundle time instead. Lt and Gt are rejected because they
// need a feature gate the bundle cannot see, and nothing in the CLI, config
// file or API produces them.
func TestNewPlacement_RejectsInvalidScheduling(t *testing.T) {
	seconds := int64(30)
	tests := []struct {
		name  string
		sched Scheduling
	}{
		{"selector key", Scheduling{NodeSelector: map[string]string{"bad key": "v"}}},
		{"selector value template", Scheduling{NodeSelector: map[string]string{"k": "{{ .Values.x }}"}}},
		{"toleration key", Scheduling{Tolerations: []corev1.Toleration{{Key: "{{ .Values.x }}", Operator: corev1.TolerationOpExists}}}},
		{"toleration value", Scheduling{Tolerations: []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpEqual, Value: "{{ x }}"}}}},
		{"toleration operator", Scheduling{Tolerations: []corev1.Toleration{{Key: "k", Operator: "{{ .Values.op }}"}}}},
		{"toleration effect", Scheduling{Tolerations: []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpExists, Effect: "Sometimes"}}}},
		{"empty key without Exists", Scheduling{Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpEqual, Value: "v", Effect: corev1.TaintEffectNoSchedule}}}},
		{"empty key and operator", Scheduling{Tolerations: []corev1.Toleration{{Effect: corev1.TaintEffectNoSchedule}}}},
		{"Exists with a value", Scheduling{Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, Value: "system"}}}},
		{"tolerationSeconds without NoExecute", Scheduling{Tolerations: []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule, TolerationSeconds: &seconds}}}},
		{"numeric operator Gt", Scheduling{Tolerations: []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpGt, Value: "3"}}}},
		{"numeric operator Lt", Scheduling{Tolerations: []corev1.Toleration{{Key: "k", Operator: corev1.TolerationOpLt, Value: "-5"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPlacement(tt.sched)
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("NewPlacement error = %v, want ErrCodeInvalidRequest", err)
			}
		})
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
			got, err := Render(componentName, "img:tag", []byte(validReadinessTestYAML), config.DeployerArgoCD, Placement{})
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

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

package chart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
)

// The chart renders exactly the AICR versions the operator supports, and
// its range is the one .settings.yaml pins.
func TestAICRVersionRange(t *testing.T) {
	ctx := ctxFor(t)
	raw, err := os.ReadFile(filepath.Join(chartDir, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	chartRange := regexp.MustCompile(`define "aicr-operator.aicrRange" -}}([^{]+){{`).FindSubmatch(raw)
	settings := struct {
		Operator struct {
			Range string `yaml:"aicr_version_range"`
		} `yaml:"operator"`
	}{}
	if raw, err = os.ReadFile(filepath.Join("..", "..", "..", ".settings.yaml")); err == nil {
		err = yaml.Unmarshal(raw, &settings)
	}
	if err != nil {
		t.Fatal(err)
	}
	if chartRange == nil || string(chartRange[1]) != settings.Operator.Range {
		t.Errorf("chart range %q, .settings.yaml operator.aicr_version_range %q", chartRange, settings.Operator.Range)
	}

	for _, v := range []string{"v0.9.9", "v1.0.0", "v1.0.1", "v1.1.0", "v1.42.7", "v2.0.0", "v2.1.0", "v1.1.0-rc.1", "1.0.0"} {
		_, out, renderErr := render(ctx, "aicr.version="+v)
		supported := aicrjob.CheckSupportedVersion(v) == nil
		switch {
		case supported && renderErr != nil:
			t.Errorf("%s: the operator supports it, the chart fails: %s", v, out)
		case !supported && renderErr == nil:
			t.Errorf("%s: the operator rejects it, the chart renders", v)
		case !supported && !strings.Contains(out, "outside the supported range") && !strings.Contains(out, "/aicr/version"):
			t.Errorf("%s: failure does not name aicr.version: %s", v, out)
		}
	}
}

// Layouts the chart cannot install fail at render time.
func TestLayoutChecks(t *testing.T) {
	ctx := ctxFor(t)
	cases := []struct {
		name, ns string
		set      []string
		want     string
	}{
		{"release namespace is a created namespace", jobsNS, nil, "the chart cannot also create it"},
		{"duplicate namespaces", operatorNS, []string{"namespaces.discovery=" + jobsNS}, "must be distinct"},
		{"component reuses the Job namespace", operatorNS, []string{"namespaces.components[0].name=" + jobsNS}, "must be distinct"},
		{"static Secret with aws", operatorNS, []string{"registry.auth.staticSecretRef=x"}, "static provider only"},
		{"unpinned AICR image", operatorNS, []string{"aicr.image=ghcr.io/nvidia/aicr:v1.0.0"}, "/aicr/image"},
		{"registry without oci", operatorNS, []string{"registry.repository=https://example.com/aicr"}, "/registry/repository"},
		{"unknown engine", operatorNS, []string{"engine.type=Flux"}, "/engine/type"},
		{"unknown value", operatorNS, []string{"aicr.gateImage=x"}, "gateImage"},
		{"validation without validator images", operatorNS, []string{"aicr.validatorImages=null"}, "needs aicr.validatorImages"},
		{"mutable validator image", operatorNS, []string{"aicr.validatorImages[0]=ghcr.io/nvidia/aicr-validators/deployment:latest"}, "/aicr/validatorImages/0"},
		{"signing key without validation", operatorNS, []string{"aicr.version=v1.1.0", "validation.enabled=false", "validation.signingKey=" + signingKey}, "needs validation.enabled"},
		{"signing key not a KMS key", operatorNS, []string{"aicr.version=v1.1.0", "validation.signingKey=file:///cosign.key"}, "key URI"},
		{"signing key on AICR v1.0.x", operatorNS, []string{"validation.signingKey=" + signingKey}, "needs AICR v1.1.0"},
	}
	for _, tc := range cases {
		if _, out, err := renderIn(ctx, tc.ns, tc.set...); err == nil || !strings.Contains(out, tc.want) {
			t.Errorf("%s: err %v, output %q; want a failure naming %q", tc.name, err, out, tc.want)
		}
	}
}

// The chart writes nothing for argocd-cm, whose keys every Argo CD user
// shares, and the operator gets no access to it; the deprecated
// engine.manageCustomizations is still accepted and changes nothing.
func TestNoArgoCDSettings(t *testing.T) {
	ctx := ctxFor(t)
	for _, set := range [][]string{nil, {"engine.manageCustomizations=false"}} {
		objs, out, err := render(ctx, set...)
		if err != nil {
			t.Fatalf("%v: %s", set, out)
		}
		for _, o := range objs {
			if o.GetKind() == "ConfigMap" {
				t.Errorf("%v: ConfigMap %s rendered", set, o.GetName())
			}
			if o.GetKind() != "Role" && o.GetKind() != "ClusterRole" {
				continue
			}
			rules, _, _ := unstructured.NestedSlice(o.Object, "rules")
			for _, r := range rules {
				if names, _, _ := unstructured.NestedStringSlice(r.(map[string]any), "resourceNames"); slices.Contains(names, "argocd-cm") {
					t.Errorf("%v: %s %s grants access to argocd-cm", set, o.GetKind(), o.GetName())
				}
			}
		}
		if args := deploymentArgs(t, objs); slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "customizations") }) {
			t.Errorf("%v: operator args %v", set, args)
		}
	}
}

// aicr.jobs places every AICR Job; unset, the operator places none.
func TestJobPlacementValues(t *testing.T) {
	ctx := ctxFor(t)
	objs, out, err := render(ctx)
	if err != nil {
		t.Fatal(out)
	}
	if args := deploymentArgs(t, objs); !slices.Contains(args, "--job-node-selector={}") || !slices.Contains(args, "--job-tolerations=[]") {
		t.Errorf("default args %v", args)
	}
	objs, out, err = render(ctx, "aicr.jobs.nodeSelector.node-role=system",
		"aicr.jobs.tolerations[0].key=CriticalAddonsOnly", "aicr.jobs.tolerations[0].operator=Exists")
	if err != nil {
		t.Fatal(out)
	}
	args := deploymentArgs(t, objs)
	if !slices.Contains(args, `--job-node-selector={"node-role":"system"}`) ||
		!slices.Contains(args, `--job-tolerations=[{"key":"CriticalAddonsOnly","operator":"Exists"}]`) {

		t.Errorf("placed args %v", args)
	}
}

// Validation identities follow the AICR version: v1.0.x binds the
// launcher to cluster-admin; from v1.1.0 only the validator ServiceAccount
// is, and the launcher gets namespaced rights and SubjectAccessReviews.
func TestValidationIdentities(t *testing.T) {
	ctx := ctxFor(t)
	clusterAdmins := func(objs []unstructured.Unstructured) []string {
		var subjects []string
		for _, o := range objs {
			if o.GetKind() != "ClusterRoleBinding" && o.GetKind() != "RoleBinding" {
				continue
			}
			if name, _, _ := unstructured.NestedString(o.Object, "roleRef", "name"); name != "cluster-admin" {
				continue
			}
			list, _, _ := unstructured.NestedSlice(o.Object, "subjects")
			for _, s := range list {
				subjects = append(subjects, s.(map[string]any)["name"].(string))
			}
		}
		sort.Strings(subjects)
		return subjects
	}
	cases := []struct {
		set  []string
		want []string
	}{
		{[]string{"aicr.version=v1.0.0"}, []string{"aicr-validation"}},
		{[]string{"aicr.version=v1.1.0"}, []string{"aicr-validator"}},
		{[]string{"aicr.version=v1.1.0", "validation.enabled=false"}, nil},
	}
	for _, tc := range cases {
		objs, out, err := render(ctx, tc.set...)
		if err != nil {
			t.Fatal(out)
		}
		if got := clusterAdmins(objs); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: cluster-admin subjects %v, want %v", tc.set, got, tc.want)
		}
	}
}

// The snapshot launcher holds cluster-scoped namespace create only for AICR
// v1.0.x; from v1.1.0 exact-ServiceAccount mode sends no namespace request
// (NVIDIA/aicr#3255).
func TestSnapshotNamespaceGrant(t *testing.T) {
	ctx := ctxFor(t)
	for version, want := range map[string]bool{"v1.0.0": true, "v1.0.9": true, "v1.1.0": false, "v1.7.2": false} {
		objs, out, err := render(ctx, "aicr.version="+version)
		if err != nil {
			t.Fatal(out)
		}
		var namespaceRules []string
		for _, o := range objs {
			if o.GetKind() != "ClusterRole" {
				continue
			}
			rules, _, _ := unstructured.NestedSlice(o.Object, "rules")
			for _, r := range rules {
				if slices.Contains(r.(map[string]any)["resources"].([]any), any("namespaces")) {
					namespaceRules = append(namespaceRules, o.GetName())
				}
			}
		}
		if got := len(namespaceRules) > 0; got != want {
			t.Errorf("%s: ClusterRoles with namespace rules %v, want present %t", version, namespaceRules, want)
		}
	}
}

// A binding keeps its roleRef across the supported AICR versions: the API
// server refuses a roleRef change, so an AICR upgrade by values could not
// apply the chart. Charts before v0.1.0-rc.10 bound the validation launcher
// as aicr-validation to either role, so no binding reuses that name.
func TestBindingsKeepTheirRoleRef(t *testing.T) {
	ctx := ctxFor(t)
	roleRefs := map[string]map[string]any{"ClusterRoleBinding//aicr-validation": nil}
	for _, version := range []string{"v1.0.0", "v1.1.0"} {
		objs, out, err := render(ctx, "aicr.version="+version)
		if err != nil {
			t.Fatal(out)
		}
		for _, o := range objs {
			if o.GetKind() != "ClusterRoleBinding" && o.GetKind() != "RoleBinding" {
				continue
			}
			key := o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
			ref, _, _ := unstructured.NestedMap(o.Object, "roleRef")
			if prev, ok := roleRefs[key]; ok && !reflect.DeepEqual(prev, ref) {
				t.Errorf("%s: roleRef %v with %s, %v before", key, ref, version, prev)
			}
			roleRefs[key] = ref
		}
	}
}

// No chart role aggregates into the built-in admin, edit or view roles, so
// tenants never inherit operator kinds; and the operator's roles carry no
// wildcard or escalation verb.
func TestRolesStayExplicit(t *testing.T) {
	ctx := ctxFor(t)
	objs, out, err := render(ctx, "aicr.version=v1.1.0")
	if err != nil {
		t.Fatal(out)
	}
	for _, o := range objs {
		if o.GetKind() != "ClusterRole" && o.GetKind() != "Role" {
			continue
		}
		for k := range o.GetLabels() {
			if strings.HasPrefix(k, "rbac.authorization.k8s.io/aggregate-to-") {
				t.Errorf("%s %s aggregates through %s", o.GetKind(), o.GetName(), k)
			}
		}
		rules, _, _ := unstructured.NestedSlice(o.Object, "rules")
		for _, r := range rules {
			rule := r.(map[string]any)
			for _, field := range []string{"apiGroups", "resources", "verbs"} {
				for _, v := range rule[field].([]any) {
					s := v.(string)
					if s == "*" {
						t.Errorf("%s %s has a wildcard in %s", o.GetKind(), o.GetName(), field)
					}
					if field == "verbs" && slices.Contains([]string{"escalate", "bind", "impersonate"}, s) {
						t.Errorf("%s %s grants %s", o.GetKind(), o.GetName(), s)
					}
				}
			}
		}
	}
}

// Exactly the documented resources survive helm uninstall.
func TestKeptResources(t *testing.T) {
	ctx := ctxFor(t)
	objs, out, err := render(ctx)
	if err != nil {
		t.Fatal(out)
	}
	var kept []string
	for _, o := range objs {
		if o.GetAnnotations()["helm.sh/resource-policy"] == "keep" {
			kept = append(kept, o.GetKind()+"/"+o.GetName())
		}
	}
	sort.Strings(kept)
	want := []string{
		"AppProject/aicr",
		"CustomResourceDefinition/clusterruntimes.operator.aicr.run",
		"CustomResourceDefinition/runtimerollouts.operator.aicr.run",
		"Namespace/" + discoveryNS, "Namespace/" + jobsNS, "Namespace/" + validationNS,
		"Namespace/cert-manager", "Namespace/gpu-operator",
		"Secret/aicr-registry",
		"ValidatingAdmissionPolicy/aicr-operator-approval",
		"ValidatingAdmissionPolicyBinding/aicr-operator-approval",
		"ValidatingAdmissionPolicy/aicr-operator-state",
		"ValidatingAdmissionPolicyBinding/aicr-operator-state",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("kept %v\nwant %v", kept, want)
	}
}

// Every installation value reaches the operator as a flag it parses.
func TestDeploymentFlags(t *testing.T) {
	ctx := ctxFor(t)
	objs, out, err := render(ctx, "development.bypassApprovals=true", "discovery.agentRequests=cpu=1")
	if err != nil {
		t.Fatal(out)
	}
	args := deploymentArgs(t, objs)
	for _, want := range []string{
		"--aicr-version=v1.0.0", "--aicr-image=" + aicrImage, "--operator-namespace=" + operatorNS,
		"--jobs-namespace=" + jobsNS, "--discovery-namespace=" + discoveryNS, "--validation-namespace=" + validationNS,
		"--engine=ArgoCD", "--engine-namespace=" + engineNS, "--engine-project=aicr",
		"--registry-repository=" + registry, "--registry-auth-provider=aws", "--engine-repo-creds-secret=aicr-registry",
		"--development-bypass-approvals=true", "--snapshot-agent-requests=cpu=1",
		"--component-namespaces=gpu-operator,cert-manager,kube-system", "--validation-enabled=true",
		"--validator-images=ghcr.io/nvidia/aicr-validators/deployment:v1.0.0@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790",
		"--validation-signing-key=", "--validation-transparency-log=true",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	objs, out, err = render(ctx, "aicr.version=v1.1.0", "validation.signingKey="+signingKey, "validation.transparencyLog=false")
	if err != nil {
		t.Fatal(out)
	}
	args = deploymentArgs(t, objs)
	for _, want := range []string{"--validation-signing-key=" + signingKey, "--validation-transparency-log=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
}

// The operator pod outlives the manager's graceful shutdown, and the Go
// runtime's soft memory limit is the container's memory limit, in bytes.
func TestOperatorPodRuntime(t *testing.T) {
	ctx := ctxFor(t)
	objs, out, err := render(ctx)
	if err != nil {
		t.Fatal(out)
	}
	raw, err := find(t, objs, "Deployment", "aicr-operator").MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	d := &appsv1.Deployment{}
	if err = json.Unmarshal(raw, d); err != nil {
		t.Fatal(err)
	}
	pod := d.Spec.Template.Spec
	if g := pod.TerminationGracePeriodSeconds; g == nil || time.Duration(*g)*time.Second <= defaults.GracefulShutdownTimeout {
		t.Errorf("terminationGracePeriodSeconds %v is not above the %s graceful shutdown", g, defaults.GracefulShutdownTimeout)
	}
	manager := pod.Containers[0]
	var limit *corev1.ResourceFieldSelector
	for _, e := range manager.Env {
		if e.Name == "GOMEMLIMIT" && e.ValueFrom != nil {
			limit = e.ValueFrom.ResourceFieldRef
		}
	}
	if limit == nil || limit.ContainerName != manager.Name || limit.Resource != "limits.memory" ||
		(!limit.Divisor.IsZero() && limit.Divisor.Cmp(resource.MustParse("1")) != 0) {

		t.Errorf("GOMEMLIMIT is %+v, want the %s container's memory limit in bytes", limit, manager.Name)
	}
}

// signingKey is a KMS key URI that signs evidence.
const signingKey = "awskms:///alias/aicr-evidence"

func deploymentArgs(t *testing.T, objs []unstructured.Unstructured) []string {
	t.Helper()
	d := find(t, objs, "Deployment", "aicr-operator")
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	raw := containers[0].(map[string]any)["args"].([]any)
	args := make([]string, 0, len(raw))
	for _, a := range raw {
		args = append(args, a.(string))
	}
	return args
}

// The chart README's values table covers every key in values.yaml, and names
// no key the chart does not have; a row covers the keys nested under it.
func TestValuesReferenceIsComplete(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err = yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if child, ok := v.(map[string]any); ok && len(child) > 0 {
				walk(prefix+k+".", child)
				continue
			}
			keys = append(keys, prefix+k)
		}
	}
	walk("", values)

	readme, err := os.ReadFile(filepath.Join(chartDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, found := strings.Cut(string(readme), "\n## Values\n")
	if !found {
		t.Fatal("charts/aicr-operator/README.md has no ## Values section")
	}
	table, _, _ = strings.Cut(table, "\n## ")
	row := regexp.MustCompile("(?m)^\\| `([a-zA-Z0-9.]+)` \\|")
	matches := row.FindAllStringSubmatch(table, -1)
	rows := make([]string, 0, len(matches))
	for _, m := range matches {
		rows = append(rows, m[1])
	}
	for _, k := range keys {
		if !slices.ContainsFunc(rows, func(r string) bool { return k == r || strings.HasPrefix(k, r+".") }) {
			t.Errorf("values.yaml key %s has no row in the chart README", k)
		}
	}
	for _, r := range rows {
		if !slices.ContainsFunc(keys, func(k string) bool { return k == r || strings.HasPrefix(k, r+".") }) {
			t.Errorf("the chart README describes %s, which values.yaml does not have", r)
		}
	}
}

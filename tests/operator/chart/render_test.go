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
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/manager"
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

	versions := []string{"v0.9.9", "v1.0.0", "v1.0.1", "v1.1.0", "v1.42.7", "v2.0.0", "v2.1.0", "1.0.0", "v1.1",
		"v1.1.0-rc.1", "v1.3.0-rc1", "v1.3.0-next", "v1.2.0-12-gabcdef0", "v0.9.9-rc.1", "v2.0.0-rc.1", "v1.1.0+build.1", "v1.3.0-rc.01", "v1.3.0-01"}
	for _, allow := range []bool{false, true} {
		for _, v := range versions {
			_, out, renderErr := render(ctx, "aicr.version="+v, fmt.Sprintf("aicr.allowPrerelease=%t", allow))
			supported := aicrjob.CheckSupportedVersion(v) == nil && aicrjob.CheckPrereleaseAllowed(v, allow) == nil
			switch {
			case supported && renderErr != nil:
				t.Errorf("%s, allowPrerelease %t: the operator supports it, the chart fails: %s", v, allow, out)
			case !supported && renderErr == nil:
				t.Errorf("%s, allowPrerelease %t: the operator rejects it, the chart renders", v, allow)
			case !supported && !strings.Contains(out, "outside the supported range") &&
				!strings.Contains(out, "aicr.allowPrerelease") && !strings.Contains(out, "/aicr/version"):

				t.Errorf("%s, allowPrerelease %t: failure does not name aicr.version: %s", v, allow, out)
			}
		}
	}

	// The operator receives the opt-in.
	for _, allow := range []bool{false, true} {
		objs, out, err := render(ctx, "aicr.version=v1.3.0-rc.1", fmt.Sprintf("aicr.allowPrerelease=%t", allow))
		if !allow {
			if err == nil || !strings.Contains(out, "set aicr.allowPrerelease: true") {
				t.Errorf("a prerelease without the opt-in: %v, %s", err, out)
			}
			continue
		}
		if err != nil {
			t.Fatal(out)
		}
		if args := deploymentArgs(t, objs); !slices.Contains(args, "--allow-prerelease-aicr=true") ||
			!slices.Contains(args, "--aicr-version=v1.3.0-rc.1") {

			t.Errorf("operator args %v do not carry the prerelease and its opt-in", args)
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
		{"unknown registry mode", operatorNS, []string{"registry.mode=hosted"}, "/registry/mode"},
		{"external registry without a repository", operatorNS, []string{"registry.repository="}, "needs registry.repository"},
		{"external registry values under the in-cluster default", operatorNS,
			[]string{"registry.mode=inCluster", "registry.inCluster.image=x"}, "set registry.mode: external"},
		{"in-cluster registry without an image", operatorNS,
			[]string{"registry.mode=inCluster", "registry.repository="}, "needs registry.inCluster.image"},
		{"in-cluster registry with a static Secret", operatorNS,
			[]string{"registry.mode=inCluster", "registry.repository=", "registry.inCluster.image=x", "registry.auth.staticSecretRef=x"},
			"registry.mode: external only"},
		{"unpinned AICR image", operatorNS, []string{"aicr.image=ghcr.io/nvidia/aicr:v1.0.0"}, "/aicr/image"},
		{"registry without oci", operatorNS, []string{"registry.repository=https://example.com/aicr"}, "/registry/repository"},
		{"unknown engine", operatorNS, []string{"engine.type=Flux"}, "/engine/type"},
		{"unknown value", operatorNS, []string{"aicr.gateImage=x"}, "gateImage"},
		{"validation without validator images", operatorNS, []string{"aicr.validatorImages=null"}, "needs aicr.validatorImages"},
		{"mutable validator image", operatorNS, []string{"aicr.validatorImages[0]=ghcr.io/nvidia/aicr-validators/deployment:latest"}, "/aicr/validatorImages/0"},
		{"validator repository pinned twice", operatorNS, []string{"aicr.validatorImages[0]=" + validatorImage, "aicr.validatorImages[1]=" + otherValidatorTag},
			"pins the validator repository ghcr.io/nvidia/aicr-validators/deployment twice"},
		{"aws provider outside ECR", operatorNS, []string{"registry.repository=oci://ghcr.io/nvidia/aicr"}, "a private Amazon ECR registry"},
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
		}
		for _, r := range decodeKinds[rbacv1.ClusterRole](t, objs, "Role", "ClusterRole") {
			for _, rule := range r.Rules {
				if slices.Contains(rule.ResourceNames, "argocd-cm") {
					t.Errorf("%v: %s %s grants access to argocd-cm", set, r.Kind, r.Name)
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
		for _, b := range decodeKinds[rbacv1.ClusterRoleBinding](t, objs, "RoleBinding", "ClusterRoleBinding") {
			if b.RoleRef.Name != "cluster-admin" {
				continue
			}
			for _, s := range b.Subjects {
				subjects = append(subjects, s.Name)
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
		{[]string{"aicr.version=v1.0.9-rc.1", "aicr.allowPrerelease=true"}, []string{"aicr-validation"}},
		{[]string{"aicr.version=v1.1.0-rc.1", "aicr.allowPrerelease=true"}, []string{"aicr-validator"}},
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
	for version, want := range map[string]bool{"v1.0.0": true, "v1.0.9": true, "v1.1.0": false, "v1.7.2": false,
		"v1.0.9-rc.1": true, "v1.1.0-rc.1": false} {
		objs, out, err := render(ctx, "aicr.version="+version, "aicr.allowPrerelease=true")
		if err != nil {
			t.Fatal(out)
		}
		var namespaceRules []string
		for _, r := range decodeKinds[rbacv1.ClusterRole](t, objs, "ClusterRole") {
			for _, rule := range r.Rules {
				if slices.Contains(rule.Resources, "namespaces") {
					namespaceRules = append(namespaceRules, r.Name)
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
	roleRefs := map[string]*rbacv1.RoleRef{"ClusterRoleBinding//aicr-validation": nil}
	for _, version := range []string{"v1.0.0", "v1.1.0"} {
		objs, out, err := render(ctx, "aicr.version="+version)
		if err != nil {
			t.Fatal(out)
		}
		for _, b := range decodeKinds[rbacv1.ClusterRoleBinding](t, objs, "RoleBinding", "ClusterRoleBinding") {
			key := b.Kind + "/" + b.Namespace + "/" + b.Name
			if prev, ok := roleRefs[key]; ok && (prev == nil || *prev != b.RoleRef) {
				t.Errorf("%s: roleRef %v with %s, %v before", key, b.RoleRef, version, prev)
			}
			roleRefs[key] = &b.RoleRef
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
	for _, r := range decodeKinds[rbacv1.ClusterRole](t, objs, "Role", "ClusterRole") {
		for k := range r.Labels {
			if strings.HasPrefix(k, "rbac.authorization.k8s.io/aggregate-to-") {
				t.Errorf("%s %s aggregates through %s", r.Kind, r.Name, k)
			}
		}
		for _, rule := range r.Rules {
			for field, values := range map[string][]string{"apiGroups": rule.APIGroups, "resources": rule.Resources, "verbs": rule.Verbs} {
				if slices.Contains(values, "*") {
					t.Errorf("%s %s has a wildcard in %s", r.Kind, r.Name, field)
				}
			}
			for _, verb := range rule.Verbs {
				if slices.Contains([]string{"escalate", "bind", "impersonate"}, verb) {
					t.Errorf("%s %s grants %s", r.Kind, r.Name, verb)
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
		"--aicr-version=v1.0.0", "--allow-prerelease-aicr=false", "--aicr-image=" + aicrImage, "--operator-namespace=" + operatorNS,
		"--jobs-namespace=" + jobsNS, "--discovery-namespace=" + discoveryNS, "--validation-namespace=" + validationNS,
		"--engine=ArgoCD", "--engine-namespace=" + engineNS, "--engine-project=aicr",
		"--registry-repository=" + registry, "--registry-auth-provider=aws", "--engine-repo-creds-secret=aicr-registry",
		"--development-bypass-approvals=true", "--snapshot-agent-requests=cpu=1",
		"--component-namespaces=gpu-operator,cert-manager,kube-system", "--validation-enabled=true",
		"--validator-images=" + validatorImage,
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
	d := &appsv1.Deployment{}
	decode(t, find(t, objs, "Deployment", release), d)
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

// otherValidatorTag pins validatorImage's repository at a second tag, and
// untaggedValidator is validatorImage without its tag.
var (
	otherValidatorTag = strings.Replace(validatorImage, "@", ".1@", 1)
	untaggedValidator = regexp.MustCompile(`:[^:/@]+@`).ReplaceAllString(validatorImage, "@")
)

// deploymentArgs returns the operator container's arguments.
func deploymentArgs(t *testing.T, objs []unstructured.Unstructured) []string {
	t.Helper()
	d := find(t, objs, "Deployment", release)
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	for _, c := range containers {
		if c, ok := c.(map[string]any); ok && c["name"] == "manager" {
			args, _, _ := unstructured.NestedStringSlice(c, "args")
			return args
		}
	}
	t.Fatal("the operator Deployment has no manager container")
	return nil
}

// parseOperatorFlags parses the operator Deployment's arguments and
// validates the result, as the operator does at startup.
func parseOperatorFlags(t *testing.T, objs []unstructured.Unstructured) (manager.Flags, error) {
	t.Helper()
	fs := flag.NewFlagSet(release, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f, err := manager.ParseFlags(fs, deploymentArgs(t, objs))
	if err != nil {
		return f, err
	}
	if err = f.Options.Validate(); err != nil {
		return f, err
	}
	return f, f.Config.Validate(false)
}

// The chart renders only configurations the operator starts with. Each case
// probes one of the operator's startup checks.
func TestRenderedConfigurationsStart(t *testing.T) {
	ctx := ctxFor(t)
	cases := []struct {
		name  string
		files []string
		set   []string
	}{
		{"defaults", nil, nil},
		{"in-cluster registry", []string{inClusterValuesFile}, nil},
		{"validation disabled", nil, []string{"validation.enabled=false"}},
		{"validator image without a tag", nil, []string{"aicr.validatorImages[0]=" + untaggedValidator}},
		{"validator repository pinned twice", nil, []string{"aicr.validatorImages[0]=" + validatorImage, "aicr.validatorImages[1]=" + otherValidatorTag}},
		{"signing key", nil, []string{"aicr.version=v1.1.0", "validation.signingKey=" + signingKey}},
		{"no AICR version", nil, []string{"aicr.version="}},
		{"aws provider with a registry outside ECR", nil, []string{"registry.repository=oci://ghcr.io/nvidia/aicr"}},
		{"aws provider without a repo-creds Secret", nil, []string{"registry.auth.repoCredsSecret="}},
		{"static provider", nil, []string{"registry.auth.provider=static", "registry.auth.staticSecretRef=x"}},
		{"anonymous static provider", nil, []string{"registry.auth.provider=static"}},
		{"unknown provider", nil, []string{"registry.auth.provider=gcp"}},
		{"no jobs namespace", nil, []string{"namespaces.jobs="}},
		{"no discovery namespace", nil, []string{"namespaces.discovery="}},
		{"no validation namespace", nil, []string{"namespaces.validation="}},
		{"no engine namespace", nil, []string{"engine.namespace="}},
		{"no engine project", nil, []string{"engine.project="}},
	}
	rendered := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs, _, err := renderFiles(ctx, chartDir, operatorNS, append([]string{valuesFile}, tc.files...), tc.set...)
			if err != nil {
				return
			}
			rendered++
			if _, err = parseOperatorFlags(t, objs); err != nil {
				t.Errorf("the chart renders, the operator refuses to start: %v", err)
			}
		})
	}
	if rendered == 0 {
		t.Error("no case rendered")
	}
}

// helm upgrade --reuse-values renders the new chart with the previous
// release's chart defaults in place of its own, so a key added since is
// missing from both those defaults and the release's values. With any one
// key missing, the chart fails naming it, or configures the operator as the
// key's default does.
func TestValuesFromAnEarlierChart(t *testing.T) {
	ctx := ctxFor(t)
	chartValues := filepath.Join(chartDir, "values.yaml")
	for _, key := range valueKeys(readValues(t, chartValues)) {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			earlier := filepath.Join(dir, release)
			if err := os.CopyFS(earlier, os.DirFS(chartDir)); err != nil {
				t.Fatal(err)
			}
			copyValuesWithout(t, chartValues, filepath.Join(earlier, "values.yaml"), key)
			user := filepath.Join(dir, "user.yaml")
			copyValuesWithout(t, valuesFile, user, key)

			objs, out, err := renderFiles(ctx, earlier, operatorNS, []string{user})
			if err != nil {
				if leaf := key[strings.LastIndex(key, ".")+1:]; !strings.Contains(out, leaf) {
					t.Errorf("the render fails without naming %s: %s", key, out)
				}
				return
			}
			got, err := parseOperatorFlags(t, objs)
			if err != nil {
				t.Fatalf("the operator rejects its arguments: %v", err)
			}
			if objs, out, err = renderFiles(ctx, chartDir, operatorNS, []string{user}); err != nil {
				t.Fatalf("renders without %s, fails with its default: %s", key, out)
			}
			want, err := parseOperatorFlags(t, objs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("operator configuration without %s:\n%+v\nwith its default:\n%+v", key, got, want)
			}
		})
	}
}

// readValues parses the values file at path.
func readValues(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{}
	if err = yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	return values
}

// copyValuesWithout writes the values file src to dst with key, a dotted
// path, removed.
func copyValuesWithout(t *testing.T, src, dst, key string) {
	t.Helper()
	values := readValues(t, src)
	unstructured.RemoveNestedField(values, strings.Split(key, ".")...)
	raw, err := yaml.Marshal(values)
	if err == nil {
		err = os.WriteFile(dst, raw, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// valueKeys returns the dotted path of every leaf in values, sorted. An empty
// map is a leaf.
func valueKeys(values map[string]any) []string {
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
	sort.Strings(keys)
	return keys
}

// The chart README's values table covers every key in values.yaml, and names
// no key the chart does not have. A row covers the keys nested under it.
func TestValuesReferenceIsComplete(t *testing.T) {
	keys := valueKeys(readValues(t, filepath.Join(chartDir, "values.yaml")))

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

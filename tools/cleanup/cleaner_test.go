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
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	k8slabels "github.com/NVIDIA/aicr/pkg/k8s/labels"
	vlabels "github.com/NVIDIA/aicr/pkg/validator/labels"
)

var (
	testNamespaces   = []string{"gang-scheduling-test", "gpu-operator", "nodewright", "skyhook"}
	testReleaseNames = map[string]bool{"gpu-operator": true, "nodewright": true, "unrelated": true}
)

type fakeHelm struct {
	releases    []helmRelease
	listErr     error
	vals        map[string]any
	valsErr     error
	uninstalled []string
}

func (f *fakeHelm) list(context.Context) ([]helmRelease, error) { return f.releases, f.listErr }

func (f *fakeHelm) uninstall(_ context.Context, rel helmRelease) error {
	f.uninstalled = append(f.uninstalled, rel.Namespace+"/"+rel.Name)
	return nil
}

func (f *fakeHelm) values(context.Context, helmRelease) (map[string]any, error) {
	return f.vals, f.valsErr
}

type fixture struct {
	kube *kubefake.Clientset
	dyn  *dynamicfake.FakeDynamicClient
	helm *fakeHelm
	out  bytes.Buffer
}

func newFixture(kubeObjs []runtime.Object, dynObjs ...runtime.Object) *fixture {
	f := &fixture{kube: kubefake.NewClientset(kubeObjs...), helm: &fakeHelm{}}
	f.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			crdGVR:                "CustomResourceDefinitionList",
			configMapGVR:          "ConfigMapList",
			clusterRoleBindingGVR: "ClusterRoleBindingList",
			clusterRoleGVR:        "ClusterRoleList",
			roleBindingGVR:        "RoleBindingList",
			roleGVR:               "RoleList",
			jobGVR:                "JobList",
			serviceAccountGVR:     "ServiceAccountList",
		}, dynObjs...)
	f.kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "configmaps", Namespaced: true, Verbs: []string{"list", "patch", "delete"}},
		},
	}}
	return f
}

func (f *fixture) cleaner(opts options) *cleaner {
	dropTime := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	log := slog.New(slog.NewTextHandler(&f.out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropTime}))
	if opts.dryRun {
		log = log.With("dryRun", true)
	}
	return &cleaner{
		opts:          opts,
		log:           log,
		kubeContext:   "test-context",
		server:        "https://test",
		kube:          f.kube,
		disc:          f.kube.Discovery(),
		dyn:           f.dyn,
		helm:          f.helm,
		namespaces:    testNamespaces,
		owners:        ownership{names: testReleaseNames},
		namespaceWait: time.Millisecond,
	}
}

func (f *fixture) namespaceExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := f.kube.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get namespace %s: %v", name, err)
	}
	return err == nil
}

func (f *fixture) crdNames(t *testing.T) []string {
	t.Helper()
	list, err := f.dyn.Resource(crdGVR).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list CRDs: %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	slices.Sort(names)
	return names
}

func (f *fixture) wantOutput(t *testing.T, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(f.out.String(), line) {
			t.Errorf("output is missing %q\noutput:\n%s", line, f.out.String())
		}
	}
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func terminatingNamespace(name string) *corev1.Namespace {
	ns := namespace(name)
	ns.Status.Phase = corev1.NamespaceTerminating
	return ns
}

func crd(name, group string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"group": group},
	}}
}

func configMap(ns, name string, finalizers ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
	u.SetFinalizers(finalizers)
	return u
}

func clusterObjects() []runtime.Object {
	return []runtime.Object{
		namespace("skyhook"),
		namespace("gpu-operator"),
		namespace("nodewright"),
		namespace("gang-scheduling-test"),
		namespace("gang-scheduling-test-abc12"),
		namespace("default"),
		namespace(defaults.ValidationNamespace),
	}
}

func clusterCRDs() []runtime.Object {
	return []runtime.Object{
		crd("clusterpolicies.nvidia.com", "nvidia.com"),
		crd("skyhooks.skyhook.nvidia.com", "skyhook.nvidia.com"),
		crd("excaliburs.excalibur.nvidia.com", "excalibur.nvidia.com"),
		crd("trainjobs.kubeflow.org", "kubeflow.org"),
		crd("jobsets.jobset.x-k8s.io", "jobset.x-k8s.io"),
		crd("ippools.nv-ipam.nvidia.com", "nv-ipam.nvidia.com"),
	}
}

func clusterReleases() []helmRelease {
	return []helmRelease{
		{Name: "nodewright", Namespace: "skyhook"},
		{Name: "gpu-operator", Namespace: "gpu-operator"},
		{Name: "team-app", Namespace: "nodewright"},
		{Name: "unrelated", Namespace: "default"},
	}
}

func confirmed(context.Context, string) bool { return true }

func mutations(actions []k8stesting.Action) []string {
	var out []string
	for _, a := range actions {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete", "delete-collection":
			out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return out
}

func TestRunFencesExcludedInstalls(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)
	f.helm.releases = clusterReleases()
	opts := options{assumeYes: true, excludeNS: []string{"skyhook"}, excludeCRD: []string{"skyhook.nvidia.com"}}

	if err := f.cleaner(opts).run(context.Background(), confirmed); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if want := []string{"gpu-operator/gpu-operator"}; !slices.Equal(f.helm.uninstalled, want) {
		t.Errorf("uninstalled = %q, want %q", f.helm.uninstalled, want)
	}
	for _, ns := range []string{"skyhook", "nodewright", "default"} {
		if !f.namespaceExists(t, ns) {
			t.Errorf("namespace %s was deleted", ns)
		}
	}
	for _, ns := range []string{"gpu-operator", "gang-scheduling-test", "gang-scheduling-test-abc12", defaults.ValidationNamespace} {
		if f.namespaceExists(t, ns) {
			t.Errorf("namespace %s was not deleted", ns)
		}
	}
	wantCRDs := []string{"excaliburs.excalibur.nvidia.com", "skyhooks.skyhook.nvidia.com", "trainjobs.kubeflow.org"}
	if got := f.crdNames(t); !slices.Equal(got, wantCRDs) {
		t.Errorf("remaining CRDs = %q, want %q", got, wantCRDs)
	}
	f.wantOutput(t,
		`msg="skipping helm release in excluded namespace" release=nodewright namespace=skyhook`,
		`msg="skipping helm release AICR does not own" release=team-app namespace=nodewright`,
		`msg="skipping excluded CRD" name=skyhooks.skyhook.nvidia.com`,
		`msg="skipping excluded namespace" namespace=skyhook`,
		`msg="keeping namespace that holds helm releases AICR does not own" namespace=nodewright releases=[team-app]`,
		`msg="skipping namespace AICR does not own" namespace=kube-system`,
		`msg="AICR cleanup complete"`,
	)
}

func TestRunDryRunMutatesNothing(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)
	f.helm.releases = clusterReleases()

	if err := f.cleaner(options{dryRun: true}).run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if got := mutations(f.kube.Actions()); len(got) > 0 {
		t.Errorf("dry run issued Kubernetes mutations: %q", got)
	}
	if got := mutations(f.dyn.Actions()); len(got) > 0 {
		t.Errorf("dry run issued dynamic mutations: %q", got)
	}
	if len(f.helm.uninstalled) > 0 {
		t.Errorf("dry run uninstalled %q", f.helm.uninstalled)
	}
	f.wantOutput(t,
		`msg="uninstalling helm release" dryRun=true release=gpu-operator namespace=gpu-operator`,
		`msg=deleting dryRun=true resource=customresourcedefinitions name=jobsets.jobset.x-k8s.io`,
		`msg=deleting dryRun=true resource=namespaces name=gang-scheduling-test-abc12`,
		`msg="AICR cleanup complete" dryRun=true`,
	)
}

func TestRunDeclinedMutatesNothing(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)
	f.helm.releases = clusterReleases()

	declined := func(context.Context, string) bool { return false }
	if err := f.cleaner(options{}).run(context.Background(), declined); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if got := slices.Concat(mutations(f.kube.Actions()), mutations(f.dyn.Actions()), f.helm.uninstalled); len(got) > 0 {
		t.Errorf("declined run mutated the cluster: %q", got)
	}
	f.wantOutput(t, `msg="cleanup aborted"`)
}

func TestRunCanceledAtPromptMutatesNothing(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)
	f.helm.releases = clusterReleases()
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := func(context.Context, string) bool {
		cancel()
		return false
	}

	err := f.cleaner(options{}).run(ctx, interrupted)

	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if got := slices.Concat(mutations(f.kube.Actions()), mutations(f.dyn.Actions()), f.helm.uninstalled); len(got) > 0 {
		t.Errorf("interrupted run mutated the cluster: %q", got)
	}
}

func TestRunKeepFlags(t *testing.T) {
	f := newFixture(clusterObjects(), clusterCRDs()...)

	if err := f.cleaner(options{assumeYes: true, keepCRDs: true, keepNamespaces: true}).run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if got := f.crdNames(t); len(got) != len(clusterCRDs()) {
		t.Errorf("--keep-crds left %d CRDs, want %d", len(got), len(clusterCRDs()))
	}
	if !f.namespaceExists(t, "gpu-operator") {
		t.Error("--keep-namespaces deleted gpu-operator")
	}
	f.wantOutput(t, `msg="skipping CRD deletion" flag=--keep-crds`, `msg="skipping namespace deletion" flag=--keep-namespaces`)
}

func TestRunWithoutHelm(t *testing.T) {
	f := newFixture(clusterObjects())
	c := f.cleaner(options{assumeYes: true})
	c.helm = nil

	if err := c.run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !f.namespaceExists(t, "gpu-operator") {
		t.Error("namespace deletion ran without helm")
	}
	f.wantOutput(t, `level=WARN msg="helm is not installed, skipping helm uninstall"`, skipNamespacesNoReleases)
}

const skipNamespacesNoReleases = `level=WARN msg="skipping namespace deletion, since deleting a namespace with no helm release list could orphan releases in it"`

func TestRunHelmListFailure(t *testing.T) {
	f := newFixture(clusterObjects())
	f.helm.listErr = errors.New(errors.ErrCodeInternal, "helm list failed")

	if err := f.cleaner(options{assumeYes: true}).run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !f.namespaceExists(t, "gpu-operator") {
		t.Error("namespace deletion ran after a helm list failure")
	}
	f.wantOutput(t, `level=WARN msg="could not list helm releases, skipping helm uninstall"`, skipNamespacesNoReleases)
}

func TestRunCanceled(t *testing.T) {
	f := newFixture(clusterObjects())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := f.cleaner(options{assumeYes: true}).run(ctx, nil)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

func TestReportExclusions(t *testing.T) {
	const nsOnly, crdOnly = "--exclude-ns given without --exclude-crd", "--exclude-crd given without --exclude-ns"
	tests := []struct {
		name       string
		opts       options
		want       []string
		wantNoneOf []string
	}{
		{"none", options{}, nil, []string{nsOnly, crdOnly, "excluding"}},
		{"namespace only", options{excludeNS: []string{"skyhook"}},
			[]string{`msg="excluding namespaces from helm uninstall and namespace deletion" namespaces=[skyhook]`, nsOnly}, []string{crdOnly}},
		{"crd only", options{excludeCRD: []string{"skyhook.nvidia.com"}},
			[]string{`msg="excluding CRDs" matching=[skyhook.nvidia.com]`, crdOnly}, []string{nsOnly}},
		{"both", options{excludeNS: []string{"skyhook"}, excludeCRD: []string{"skyhook.nvidia.com"}},
			nil, []string{nsOnly, crdOnly}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(nil)
			f.cleaner(tt.opts).reportExclusions()
			f.wantOutput(t, tt.want...)
			for _, line := range tt.wantNoneOf {
				if strings.Contains(f.out.String(), line) {
					t.Errorf("output unexpectedly contains %q\noutput:\n%s", line, f.out.String())
				}
			}
		})
	}
}

func TestDriverManaged(t *testing.T) {
	daemonSet := func(ns, name string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	gpuOperator := []helmRelease{{Name: gpuOperatorRelease, Namespace: "gpu-operator"}}
	tests := []struct {
		name     string
		objs     []runtime.Object
		releases []helmRelease
		vals     map[string]any
		valsErr  error
		noHelm   bool
		want     bool
	}{
		{"driver daemonset in a moved namespace", []runtime.Object{daemonSet("privileged-gpu-operator", driverDaemonSetName)},
			nil, nil, nil, true, true},
		{"other daemonset only", []runtime.Object{daemonSet("gpu-operator", "nvidia-device-plugin-daemonset")},
			gpuOperator, map[string]any{"driver": map[string]any{"enabled": false}}, nil, false, false},
		{"values report driver enabled", nil, gpuOperator,
			map[string]any{"driver": map[string]any{"enabled": true}}, nil, false, true},
		{"nested enabled key is not the driver", nil, gpuOperator,
			map[string]any{"driver": map[string]any{"enabled": false, "rdma": map[string]any{"enabled": true}}}, nil, false, false},
		{"values read fails open", nil, gpuOperator, nil, errors.New(errors.ErrCodeInternal, "boom"), false, false},
		{"no gpu-operator release", nil, []helmRelease{{Name: "other", Namespace: "gpu-operator"}},
			map[string]any{"driver": map[string]any{"enabled": true}}, nil, false, false},
		{"no helm", nil, nil, nil, nil, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(tt.objs)
			f.helm.vals, f.helm.valsErr = tt.vals, tt.valsErr
			c := f.cleaner(options{})
			if tt.noHelm {
				c.helm = nil
			}
			if got := c.driverManaged(context.Background(), tt.releases); got != tt.want {
				t.Errorf("driverManaged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunPrintsDriverGuidance(t *testing.T) {
	f := newFixture([]runtime.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-operator", Name: driverDaemonSetName}},
	})
	if err := f.cleaner(options{dryRun: true}).run(context.Background(), nil); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	const guidance = "gpu-operator manages the NVIDIA kernel driver here, so reboot the GPU nodes"
	if got := strings.Count(f.out.String(), guidance); got != 2 {
		t.Errorf("driver guidance logged %d times, want 2 (before the prompt and after cleanup)\noutput:\n%s", got, f.out.String())
	}
}

func TestCheckNamespacesListFailureIsReported(t *testing.T) {
	f := newFixture([]runtime.Object{namespace("gpu-operator")})
	f.kube.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	})

	f.cleaner(options{assumeYes: true}).deleteNamespaces(context.Background(), nil)

	f.wantOutput(t, "could not list namespaces, so per-run check namespaces were not swept", "prefix=gang-scheduling-test-")
	if f.namespaceExists(t, "gpu-operator") {
		t.Error("owned namespaces were not deleted after the check-namespace list failed")
	}
}

func TestDeleteNamespacesSharedNamespaces(t *testing.T) {
	const shared = "openshift-nfd"
	tests := []struct {
		name       string
		releases   []helmRelease
		wantKept   bool
		wantOutput string
	}{
		{"aicr release found", []helmRelease{{Name: "nfd-ocp-olm", Namespace: shared, Status: "deployed"}}, false,
			`msg=deleting resource=namespaces name=` + shared},
		{"aicr release uninstalled with kept history", []helmRelease{{Name: "nfd-ocp-olm", Namespace: shared, Status: helmStatusUninstalled}}, true,
			`msg="keeping shared namespace with no AICR helm release found" namespace=` + shared},
		{"no release found", nil, true,
			`msg="keeping shared namespace with no AICR helm release found" namespace=` + shared},
		{"aicr release elsewhere", []helmRelease{{Name: "nfd-ocp-olm", Namespace: "gpu-operator"}}, true,
			`msg="keeping shared namespace with no AICR helm release found" namespace=` + shared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture([]runtime.Object{namespace(shared), namespace("gpu-operator")})
			c := f.cleaner(options{assumeYes: true})
			c.namespaces = []string{"gpu-operator", shared}
			c.owners = ownership{names: map[string]bool{"nfd-ocp-olm": true}}

			c.deleteNamespaces(context.Background(), tt.releases)

			if got := f.namespaceExists(t, shared); got != tt.wantKept {
				t.Errorf("namespace %s kept = %v, want %v", shared, got, tt.wantKept)
			}
			if f.namespaceExists(t, "gpu-operator") {
				t.Error("namespace gpu-operator was not deleted")
			}
			f.wantOutput(t, tt.wantOutput)
		})
	}
}

func TestFinalizerRescue(t *testing.T) {
	f := newFixture(
		[]runtime.Object{terminatingNamespace("nodewright"), terminatingNamespace("skyhook")},
		configMap("nodewright", "held", "example.com/hold"),
		configMap("nodewright", "free"),
		configMap("skyhook", "fenced", "example.com/hold"),
	)
	f.kube.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	f.cleaner(options{assumeYes: true, excludeNS: []string{"skyhook"}}).deleteNamespaces(context.Background(), nil)

	finalizers := func(ns, name string) []string {
		obj, err := f.dyn.Resource(configMapGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get configmap %s/%s: %v", ns, name, err)
		}
		return obj.GetFinalizers()
	}
	if got := finalizers("nodewright", "held"); len(got) != 0 {
		t.Errorf("finalizers on nodewright/held = %q, want none", got)
	}
	if got := finalizers("skyhook", "fenced"); len(got) != 1 {
		t.Errorf("finalizers on excluded skyhook/fenced = %q, want them untouched", got)
	}
	var patched []string
	for _, a := range f.dyn.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			patched = append(patched, p.GetNamespace()+"/"+p.GetName())
		}
	}
	if want := []string{"nodewright/held"}; !slices.Equal(patched, want) {
		t.Errorf("patched = %q, want %q", patched, want)
	}
	f.wantOutput(t,
		`msg="namespace stuck terminating, clearing finalizers" namespace=nodewright`,
		`msg="cleared finalizers" resource=configmaps name=held namespace=nodewright`,
	)
}

func TestNamespacedResources(t *testing.T) {
	f := newFixture(nil)
	f.kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Namespaced: true, Verbs: []string{"list", "patch"}},
			{Name: "configmaps/status", Namespaced: true, Verbs: []string{"list", "patch"}},
			{Name: "pods", Namespaced: true, Verbs: []string{"list", "patch"}},
			{Name: "nodes", Namespaced: false, Verbs: []string{"list", "patch"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Namespaced: true, Verbs: []string{"list", "patch"}},
		}},
		{GroupVersion: "apps/v1beta1", APIResources: []metav1.APIResource{
			{Name: "deployments", Namespaced: true, Verbs: []string{"list", "patch"}},
		}},
		{GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
			{Name: "jobs", Namespaced: true, Verbs: []string{"list"}},
		}},
	}

	got := f.cleaner(options{}).namespacedResources()
	want := []schema.GroupVersionResource{
		{Version: "v1", Resource: "configmaps"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("namespacedResources() = %v, want %v", got, want)
	}
}

func TestDeleteObjectReportsFailures(t *testing.T) {
	f := newFixture(nil, crd("jobsets.jobset.x-k8s.io", "jobset.x-k8s.io"), crd("ippools.nv-ipam.nvidia.com", "nv-ipam.nvidia.com"))
	f.dyn.PrependReactor("delete", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == "jobsets.jobset.x-k8s.io" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, "jobsets", nil)
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "customresourcedefinitions"}, "ippools")
	})

	f.cleaner(options{assumeYes: true}).deleteCRDs(context.Background())

	f.wantOutput(t, `level=WARN msg="delete failed" resource=customresourcedefinitions name=jobsets.jobset.x-k8s.io`)
	if strings.Contains(f.out.String(), `msg="delete failed" resource=customresourcedefinitions name=ippools`) {
		t.Errorf("NotFound was reported as a failure\noutput:\n%s", f.out.String())
	}
}

func TestRemoveValidatorArtifacts(t *testing.T) {
	const agentNS = "gpu-operator"
	object := func(apiVersion, kind, ns, name string, labels map[string]string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(kind)
		u.SetNamespace(ns)
		u.SetName(name)
		u.SetLabels(labels)
		return u
	}
	const rbac = "rbac.authorization.k8s.io/v1"
	managed := map[string]string{k8slabels.ManagedBy: k8slabels.ValueAICR}
	operatorRBAC := map[string]string{k8slabels.ManagedBy: k8slabels.ValueAICR, k8slabels.Component: k8slabels.ValueAgentRBAC}
	validator := map[string]string{k8slabels.Name: vlabels.ValueValidator}
	agent := map[string]string{k8slabels.Name: k8slabels.ValueAICR, k8slabels.Component: k8slabels.ValueSnapshotAgent}

	f := newFixture([]runtime.Object{namespace(defaults.ValidationNamespace)},
		object(rbac, "ClusterRole", "", "aicr-validator-run1", nil),
		object(rbac, "ClusterRole", "", "labeled-validator", validator),
		object(rbac, "ClusterRole", "", "aicr-node-reader", nil),
		object(rbac, "ClusterRole", "", "aicr", nil),
		object(rbac, "ClusterRole", "", "cluster-admin", nil),
		object(rbac, "ClusterRole", "", "aicr-operator-sa", operatorRBAC),
		object(rbac, "ClusterRoleBinding", "", "aicr-node-reader-run1", managed),
		object(rbac, "ClusterRoleBinding", "", "aicr-operator-sa", operatorRBAC),
		object("batch/v1", "Job", agentNS, "aicr-run1", agent),
		object("batch/v1", "Job", agentNS, "aicr", nil),
		object("batch/v1", "Job", agentNS, "gpu-operator-upgrade", nil),
		object("batch/v1", "Job", "default", "aicr", nil),
		object("v1", "ServiceAccount", agentNS, "aicr-run1", agent),
		object(rbac, "Role", agentNS, "aicr", nil),
		object(rbac, "RoleBinding", agentNS, "aicr", map[string]string{k8slabels.Component: k8slabels.ValueAgentRBAC}),
	)

	f.cleaner(options{assumeYes: true}).removeValidatorArtifacts(context.Background())

	var deleted []string
	for _, a := range slices.Concat(f.dyn.Actions(), f.kube.Actions()) {
		if d, ok := a.(k8stesting.DeleteAction); ok {
			name := d.GetResource().Resource + "/" + d.GetName()
			if ns := d.GetNamespace(); ns != "" {
				name = ns + "/" + name
			}
			deleted = append(deleted, name)
		}
	}
	slices.Sort(deleted)
	want := []string{
		"clusterrolebindings/aicr-node-reader-run1",
		"clusterroles/aicr",
		"clusterroles/aicr-node-reader",
		"clusterroles/aicr-validator-run1",
		"clusterroles/labeled-validator",
		"gpu-operator/jobs/aicr",
		"gpu-operator/jobs/aicr-run1",
		"gpu-operator/roles/aicr",
		"gpu-operator/serviceaccounts/aicr-run1",
		"namespaces/" + defaults.ValidationNamespace,
	}
	if !slices.Equal(deleted, want) {
		t.Errorf("deleted =\n%q\nwant\n%q", deleted, want)
	}
}

func TestRemoveValidatorArtifactsKeepNamespacesSweepsValidationRuns(t *testing.T) {
	const ns = defaults.ValidationNamespace
	const rbac = "rbac.authorization.k8s.io/v1"
	run := map[string]string{k8slabels.Name: k8slabels.ValueAICR, k8slabels.Component: vlabels.ValueValidation}
	agent := map[string]string{k8slabels.Name: k8slabels.ValueAICR, k8slabels.Component: k8slabels.ValueSnapshotAgent}
	operatorRBAC := map[string]string{k8slabels.Name: k8slabels.ValueAICR, k8slabels.Component: k8slabels.ValueAgentRBAC}
	object := func(apiVersion, kind, name string, labels map[string]string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(kind)
		u.SetNamespace(ns)
		u.SetName(name)
		u.SetLabels(labels)
		return u
	}
	runConfigMap := configMap(ns, "aicr-ctrf-run1")
	runConfigMap.SetLabels(run)
	f := newFixture([]runtime.Object{namespace(ns)},
		object("batch/v1", "Job", "aicr-run1", run),
		object("batch/v1", "Job", "team-job", nil),
		runConfigMap,
		configMap(ns, "team-config"),
		object("batch/v1", "Job", "aicr-validate-run1", agent),
		object("v1", "ServiceAccount", "aicr-validate-run1", agent),
		object(rbac, "Role", "aicr-validate-run1", agent),
		object(rbac, "RoleBinding", "aicr-validate-run1", agent),
		object("v1", "ServiceAccount", defaults.AgentName, nil),
		object(rbac, "RoleBinding", "aicr-operator-sa", operatorRBAC),
	)

	f.cleaner(options{assumeYes: true, keepNamespaces: true}).removeValidatorArtifacts(context.Background())

	var deleted []string
	for _, a := range slices.Concat(f.dyn.Actions(), f.kube.Actions()) {
		if d, ok := a.(k8stesting.DeleteAction); ok && d.GetNamespace() == ns {
			deleted = append(deleted, d.GetResource().Resource+"/"+d.GetName())
		}
	}
	slices.Sort(deleted)
	want := []string{
		"configmaps/aicr-ctrf-run1",
		"jobs/aicr-run1",
		"jobs/aicr-validate-run1",
		"rolebindings/aicr-validate-run1",
		"roles/aicr-validate-run1",
		"serviceaccounts/aicr-validate-run1",
	}
	if !slices.Equal(deleted, want) {
		t.Errorf("deleted in %s = %q, want %q", ns, deleted, want)
	}
	if !f.namespaceExists(t, ns) {
		t.Errorf("namespace %s was deleted", ns)
	}
}

func TestRemoveValidatorArtifactsProtectsValidationNamespace(t *testing.T) {
	tests := []struct {
		name string
		opts options
		want string
	}{
		{"excluded", options{excludeNS: []string{defaults.ValidationNamespace}},
			`msg="skipping excluded namespace" namespace=` + defaults.ValidationNamespace},
		{"keep namespaces", options{keepNamespaces: true},
			`msg="skipping namespace deletion, removing its aicr validate objects" namespace=` + defaults.ValidationNamespace + ` flag=--keep-namespaces`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture([]runtime.Object{namespace(defaults.ValidationNamespace)})
			tt.opts.assumeYes = true

			f.cleaner(tt.opts).removeValidatorArtifacts(context.Background())

			if !f.namespaceExists(t, defaults.ValidationNamespace) {
				t.Errorf("namespace %s was deleted", defaults.ValidationNamespace)
			}
			f.wantOutput(t, tt.want)
		})
	}
}

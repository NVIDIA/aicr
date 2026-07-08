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
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// isSkipError reports whether err came from validators.Skip. Skip wraps the
// unexported "skip" sentinel, so the rendered error always ends with ": skip".
func isSkipError(err error) bool {
	return err != nil && strings.HasSuffix(err.Error(), ": skip")
}

// healthyDRADriverObjects returns the typed objects representing a healthy
// NVIDIA DRA driver installation in the default driver namespace.
func healthyDRADriverObjects() []runtime.Object {
	return healthyDRADriverObjectsIn(draDriverNamespace)
}

// healthyDRADriverObjectsIn returns the typed objects representing a healthy
// NVIDIA DRA driver installation in the given namespace: a driver pod, an
// available controller Deployment, and a ready kubelet plugin DaemonSet.
func healthyDRADriverObjectsIn(namespace string) []runtime.Object {
	replicas := int32(1)
	return []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      draKubeletPluginDaemonSet + "-abc12",
				Namespace: namespace,
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      draControllerDeployment,
				Namespace: namespace,
			},
			Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
			Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      draKubeletPluginDaemonSet,
				Namespace: namespace,
			},
			Status: appsv1.DaemonSetStatus{NumberReady: 1, DesiredNumberScheduled: 1},
		},
	}
}

// validationInputWithRefs builds a ValidationInput carrying the given
// componentRefs, simulating a resolved recipe.
func validationInputWithRefs(refs ...recipe.ComponentRef) *v1.ValidationInput {
	return &v1.ValidationInput{ComponentRefs: refs}
}

func enabledRef(name string) recipe.ComponentRef {
	return recipe.ComponentRef{Name: name}
}

func disabledRef(name string) recipe.ComponentRef {
	return recipe.ComponentRef{Name: name, Overrides: map[string]any{"enabled": false}}
}

func withDRAAPIDiscovery(t *testing.T, clientset *k8sfake.Clientset) {
	t.Helper()
	withDRAAPIDiscoveryAt(t, clientset, draAPIGroupVersion)
}

// withDRAAPIDiscoveryAt registers the DRA API resources under an explicit
// group-version (e.g. resource.k8s.io/v1beta2 for beta-only cluster tests).
func withDRAAPIDiscoveryAt(t *testing.T, clientset *k8sfake.Clientset, groupVersion string) {
	t.Helper()
	fakeDisc, ok := clientset.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("discovery client = %T, want *fakediscovery.FakeDiscovery", clientset.Discovery())
	}
	fakeDisc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: groupVersion,
			APIResources: []metav1.APIResource{
				{Name: "resourceclaims", Kind: "ResourceClaim", Namespaced: true},
				{Name: "resourceslices", Kind: "ResourceSlice", Namespaced: false},
				{Name: "deviceclasses", Kind: "DeviceClass", Namespaced: false},
			},
		},
	}
}

func TestCheckDRASupport_SkipsWhenDriverNotInstalled(t *testing.T) {
	// Backward-compat fallback: no recipe componentRefs at all (nil
	// ValidationInput), no driver pods, no controller Deployment, no
	// kubelet-plugin DaemonSet, no DRA API. Live-driver detection applies
	// and the check skips.
	client := k8sfake.NewClientset()
	fakeDisc := client.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDisc.Resources = []*metav1.APIResourceList{}

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected skip when NVIDIA DRA driver is not installed")
	}
	if !isSkipError(err) {
		t.Fatalf("error = %v, want a skip", err)
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("expected skip message about driver not installed, got: %v", err)
	}
}

func TestCheckDRASupport_RecipeScoping(t *testing.T) {
	tests := []struct {
		name string
		// cluster state: healthy driver objects installed?
		driverInstalled bool
		validation      *v1.ValidationInput
		wantSkip        bool
		wantMsg         string
	}{
		{
			name:            "recipe disables driver: skip even when driver is installed",
			driverInstalled: true,
			validation:      validationInputWithRefs(disabledRef(draDriverComponentName)),
			wantSkip:        true,
			wantMsg:         "out of scope",
		},
		{
			name:            "recipe omits driver component: skip",
			driverInstalled: false,
			validation:      validationInputWithRefs(enabledRef("gpu-operator")),
			wantSkip:        true,
			wantMsg:         "out of scope",
		},
		{
			name:            "recipe enables driver but nothing installed: FAIL, not skip",
			driverInstalled: false,
			validation: validationInputWithRefs(
				enabledRef("gpu-operator"), enabledRef(draDriverComponentName)),
			wantSkip: false,
			// The helper's structured error propagates as-is (no re-wrap):
			// the controller Deployment is genuinely absent here.
			wantMsg: "nvidia-dra-driver-gpu-controller not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.driverInstalled {
				objects = healthyDRADriverObjects()
			}
			client := k8sfake.NewClientset(objects...)
			withDRAAPIDiscovery(t, client)

			ctx := &validators.Context{
				Ctx:             context.Background(),
				Clientset:       client,
				DynamicClient:   newDRAFakeDynamicClient(),
				ValidationInput: tt.validation,
			}

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected a non-nil result (skip or failure)")
			}
			if isSkipError(err) != tt.wantSkip {
				t.Fatalf("error = %v, want skip=%t", err, tt.wantSkip)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %v, want message containing %q", err, tt.wantMsg)
			}
		})
	}
}

// TestCheckDRASupport_HelperErrorCodesPropagate pins the error CODES from the
// driver health probes now that CheckDRASupport propagates the helpers'
// structured errors as-is instead of re-wrapping everything as
// ErrCodeNotFound: an absent controller Deployment keeps the helper's
// ErrCodeNotFound, while a deployed-but-unavailable controller keeps
// ErrCodeInternal (the case the old re-wrap misclassified).
func TestCheckDRASupport_HelperErrorCodesPropagate(t *testing.T) {
	one := int32(1) // spec asks for 1 replica; status reports 0 available
	tests := []struct {
		name       string
		objects    []runtime.Object
		forbidGets bool // reactor: deployment Get returns Forbidden
		wantMsg    string
		wantTarget error
		notTarget  error
	}{
		{
			name:       "controller Deployment absent keeps ErrCodeNotFound",
			objects:    nil,
			wantMsg:    "not found",
			wantTarget: errors.New(errors.ErrCodeNotFound, ""),
			notTarget:  errors.New(errors.ErrCodeInternal, ""),
		},
		{
			name:       "Forbidden on the controller Get keeps ErrCodeInternal (no NotFound flattening)",
			forbidGets: true,
			wantMsg:    "failed to read",
			wantTarget: errors.New(errors.ErrCodeInternal, ""),
			notTarget:  errors.New(errors.ErrCodeNotFound, ""),
		},
		{
			name: "controller deployed but unavailable keeps ErrCodeInternal",
			objects: []runtime.Object{&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      draControllerDeployment,
					Namespace: draDriverNamespace,
				},
				Spec:   appsv1.DeploymentSpec{Replicas: &one},
				Status: appsv1.DeploymentStatus{AvailableReplicas: 0},
			}},
			wantMsg:    "not available",
			wantTarget: errors.New(errors.ErrCodeInternal, ""),
			notTarget:  errors.New(errors.ErrCodeNotFound, ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(tt.objects...)
			withDRAAPIDiscovery(t, client)
			if tt.forbidGets {
				client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "x",
						stderrors.New("rbac denied"))
				})
			}

			ctx := &validators.Context{
				Ctx:           context.Background(),
				Clientset:     client,
				DynamicClient: newDRAFakeDynamicClient(),
				// Recipe-scoped: reach the Deployment probe without the
				// standalone live-detection fallback.
				ValidationInput: validationInputWithRefs(enabledRef(draDriverComponentName)),
			}

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected a failure from the controller probe")
			}
			if isSkipError(err) {
				t.Fatalf("error = %v, want a hard failure, not a skip", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want message containing %q", err, tt.wantMsg)
			}
			if !stderrors.Is(err, tt.wantTarget) {
				t.Errorf("error = %v, want code %v to match", err, tt.wantTarget)
			}
			if stderrors.Is(err, tt.notTarget) {
				t.Errorf("error = %v — code %v must NOT match (re-wrap regression)", err, tt.notTarget)
			}
		})
	}
}

func TestCheckDRASupport_FailsWhenDriverPresentButAPIMissing(t *testing.T) {
	// Driver deployed but the cluster serves NO version of resource.k8s.io
	// (neither v1 nor v1beta2 nor v1beta1): a broken configuration must FAIL,
	// not skip.
	client := k8sfake.NewClientset(healthyDRADriverObjects()...)
	fakeDisc := client.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDisc.Resources = []*metav1.APIResourceList{}

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected failure when driver is installed but no DRA API version is served")
	}
	if isSkipError(err) {
		t.Fatalf("error = %v, want a hard failure, not a skip", err)
	}
	if !strings.Contains(err.Error(), "DRA driver is in scope but") ||
		!strings.Contains(err.Error(), apiGroupResourceK8sIO) ||
		!strings.Contains(err.Error(), versionV1beta1) {

		t.Fatalf("error = %v, want message about in-scope driver with no served %s version (tried versions listed)",
			err, apiGroupResourceK8sIO)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Errorf("error code = %v, want ErrCodeUnavailable", err)
	}
}

// TestCheckDRASupport_PassesOnBetaOnlyCluster verifies the K8s 1.32/1.33
// path: the cluster serves only resource.k8s.io/v1beta2 (no v1). The API
// gate must pass via version discovery, ResourceSlices must be validated
// through the dynamic client at the served beta group-version, and — in this
// supported ComputeDomain-only configuration (no gpu.nvidia.com DeviceClass)
// — the behavioral full-GPU allocation subtest must be recorded as not
// applicable (no test pod created).
func TestCheckDRASupport_PassesOnBetaOnlyCluster(t *testing.T) {
	const betaVersion = "v1beta2"
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
	withDRAAPIDiscoveryAt(t, client, apiGroupResourceK8sIO+"/"+betaVersion)
	created := markPodsSucceededOnCreate(client)

	dynClient := newDRAFakeDynamicClientAt(betaVersion,
		testResourceSliceAt(apiGroupResourceK8sIO+"/"+betaVersion,
			"cd-1", draDriverComputeDomain, "node1", 1, 1,
			map[string]interface{}{"nodeName": "node1"},
			[]interface{}{plainDevice("channel-0")}),
	)

	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     client,
		DynamicClient: dynClient,
	}

	if err := CheckDRASupport(ctx); err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want pass on a %s-only cluster", err, betaVersion)
	}
	if pod := findPodByPrefix(*created, gpuTestPodPrefix); pod != nil {
		t.Errorf("behavioral allocation pod %s created, want none (full-GPU DRA not enabled: compute-domain only)", pod.Name)
	}
}

// TestCheckDRASupport_RunsBehavioralAllocationOnBetaOnlyCluster verifies the
// behavioral full-GPU allocation subtest is NO LONGER v1-gated: on a
// v1beta2-only cluster with a usable gpu.nvidia.com DeviceClass and slices,
// the test pod runs and the ResourceClaim is created at the served beta
// group-version.
func TestCheckDRASupport_RunsBehavioralAllocationOnBetaOnlyCluster(t *testing.T) {
	const betaVersion = "v1beta2"
	apiVersion := apiGroupResourceK8sIO + "/" + betaVersion
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
	withDRAAPIDiscoveryAt(t, client, apiVersion)
	created := markPodsSucceededOnCreate(client)

	dynClient := newDRAFakeDynamicClientAt(betaVersion,
		testDeviceClassAt(apiVersion, draDriverGPU),
		testResourceSliceAt(apiVersion, "gpu-1", draDriverGPU, "node1", 1, 1,
			map[string]interface{}{"nodeName": "node1"},
			[]interface{}{plainDevice("gpu-0")}),
	)
	var createdClaim *unstructured.Unstructured
	dynClient.PrependReactor("create", "resourceclaims",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			createAction, ok := action.(k8stesting.CreateAction)
			if !ok {
				return false, nil, nil
			}
			if u, ok := createAction.GetObject().(*unstructured.Unstructured); ok {
				createdClaim = u.DeepCopy()
			}
			return false, nil, nil
		})

	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     client,
		DynamicClient: dynClient,
	}

	if err := CheckDRASupport(ctx); err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want behavioral allocation to pass on a %s-only cluster", err, betaVersion)
	}
	if findPodByPrefix(*created, gpuTestPodPrefix) == nil {
		t.Error("behavioral allocation pod was not created despite full-GPU DRA being usable at the beta version")
	}
	if createdClaim == nil {
		t.Fatal("behavioral allocation ResourceClaim was not created")
	}
	if got := createdClaim.GetAPIVersion(); got != apiVersion {
		t.Errorf("claim apiVersion = %q, want %q", got, apiVersion)
	}
}

// TestCheckDRASupport_BetaOnlySlicesStillValidated verifies that the robust
// slice validation is NOT bypassed on a beta-only cluster: slices that exist
// but do not validate (incomplete pool) still fail the check.
func TestCheckDRASupport_BetaOnlySlicesStillValidated(t *testing.T) {
	const betaVersion = "v1beta2"
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
	withDRAAPIDiscoveryAt(t, client, apiGroupResourceK8sIO+"/"+betaVersion)

	dynClient := newDRAFakeDynamicClientAt(betaVersion,
		// Incomplete pool: resourceSliceCount=2 but only one slice observed.
		testResourceSliceAt(apiGroupResourceK8sIO+"/"+betaVersion,
			"cd-1", draDriverComputeDomain, "pool-a", 1, 2,
			map[string]interface{}{"nodeName": "node1"},
			[]interface{}{plainDevice("channel-0")}),
	)

	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     client,
		DynamicClient: dynClient,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected failure when beta-served ResourceSlices do not pass validation")
	}
	if !strings.Contains(err.Error(), "none passed validation") {
		t.Fatalf("error = %v, want message about slices failing validation", err)
	}
}

// TestCheckDRASupport_DiscoveryErrorFailsClosed verifies that a non-NotFound
// discovery error is ambiguous and propagates — it must not be flattened
// into "version not served" and continue the preference scan.
func TestCheckDRASupport_DiscoveryErrorFailsClosed(t *testing.T) {
	client := k8sfake.NewClientset(healthyDRADriverObjects()...)
	// FakeDiscovery.ServerResourcesForGroupVersion invokes verb "get" on
	// resource "resource"; inject a transient apiserver error there.
	client.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver hiccup")
	})

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected discovery error to propagate")
	}
	if isSkipError(err) {
		t.Fatalf("error = %v, want a hard failure, not a skip", err)
	}
	if !strings.Contains(err.Error(), "failed to discover DRA API group-version") {
		t.Fatalf("error = %v, want discovery failure", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error code = %v, want ErrCodeInternal", err)
	}
}

func TestCheckDRASupport_PodListNotFoundKeepsProbing(t *testing.T) {
	// NotFound from the pod-list probe deterministically means "nothing
	// there" — the probe must continue to the Deployment/DaemonSet lookups
	// and, with nothing installed, skip rather than fail ErrCodeInternal.
	client := k8sfake.NewClientset()
	fakeDisc := client.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDisc.Resources = []*metav1.APIResourceList{}
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
	})

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected skip when nothing is installed")
	}
	if !isSkipError(err) {
		t.Fatalf("error = %v, want a skip — NotFound from the pod list must not fail the probe", err)
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("expected skip message about driver not installed, got: %v", err)
	}
}

func TestNvidiaDRADriverInstalled_PodListNotFoundWithHealthyDeployment(t *testing.T) {
	// NotFound from the pod List must not stop the probe: with a healthy
	// controller Deployment present, the driver counts as installed and the
	// returned pod list is EMPTY BUT NON-NIL, so the caller's inventory step
	// reuses it instead of repeating the List and turning the same NotFound
	// into ErrCodeInternal.
	replicas := int32(1)
	client := k8sfake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      draControllerDeployment,
			Namespace: draDriverNamespace,
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	})
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
	})

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	installed, pods, err := nvidiaDRADriverInstalled(ctx, draDriverNamespace)
	if err != nil {
		t.Fatalf("nvidiaDRADriverInstalled() error = %v, want nil", err)
	}
	if !installed {
		t.Error("installed = false, want true (healthy controller Deployment present)")
	}
	if pods == nil {
		t.Fatal("pods = nil, want an empty non-nil list — a nil list makes the caller repeat the NotFound List and fail")
	}
	if len(pods.Items) != 0 {
		t.Errorf("pods.Items = %d, want 0", len(pods.Items))
	}
}

func TestCheckDRASupport_FailsClosedOnDriverProbeError(t *testing.T) {
	// A transient API error while probing driver presence must surface as a
	// failure — not be flattened into "driver not installed" and skipped, and
	// NOT reclassified as ErrCodeTimeout: the probe returns instantly with the
	// probe context still alive, so the error must keep its original
	// ErrCodeInternal code (a timeout code would make an RBAC denial or
	// apiserver 5xx look retryable).
	client := k8sfake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver hiccup")
	})

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected failure on driver probe error")
	}
	if isSkipError(err) {
		t.Fatalf("error = %v, want a hard failure, not a skip", err)
	}
	if !strings.Contains(err.Error(), "failed to list pods") {
		t.Fatalf("error = %v, want pod list failure", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error code = %v, want ErrCodeInternal", err)
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error = %v — a non-timeout probe error must not be reclassified as ErrCodeTimeout", err)
	}
}

func TestCheckDRASupport_PassesComputeDomainOnlyWithoutBehavioralAllocation(t *testing.T) {
	// Supported NVIDIA DRA driver configuration: ComputeDomain-only, no
	// gpu.nvidia.com DeviceClass. The check must pass on driver health +
	// validated compute-domain ResourceSlices (complete current-generation
	// pool, untainted device, Ready schedulable node), recording the
	// behavioral GPU allocation subtest as not applicable, and must NOT
	// create any test pod.
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
	withDRAAPIDiscovery(t, client)
	created := markPodsSucceededOnCreate(client)

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
		DynamicClient: newDRAFakeDynamicClient(
			testResourceSlice("cd-1", draDriverComputeDomain, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("channel-0")}),
		),
	}

	if err := CheckDRASupport(ctx); err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want pass", err)
	}
	if pod := findPodByPrefix(*created, gpuTestPodPrefix); pod != nil {
		t.Errorf("behavioral allocation pod %s created, want none (full-GPU DRA not enabled)", pod.Name)
	}
}

// TestCheckDRASupport_FailsWhenNVIDIASlicesDoNotValidate verifies that
// NVIDIA ResourceSlices which merely exist do NOT satisfy the check: a slice
// only counts when it is in a complete, current-generation pool, advertises
// an untainted device, and resolves to a Ready, schedulable node.
func TestCheckDRASupport_FailsWhenNVIDIASlicesDoNotValidate(t *testing.T) {
	tests := []struct {
		name  string
		nodes []runtime.Object
		slice runtime.Object
	}{
		{
			name:  "incomplete pool (resourceSliceCount=2, one slice observed)",
			nodes: []runtime.Object{testNode("node1")},
			slice: testResourceSlice("cd-1", draDriverComputeDomain, "pool-a", 1, 2,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("channel-0")}),
		},
		{
			name:  "all devices tainted NoSchedule",
			nodes: []runtime.Object{testNode("node1")},
			slice: testResourceSlice("cd-1", draDriverComputeDomain, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{taintedDevice("channel-0", "NoSchedule")}),
		},
		{
			name:  "slice resolves to no Ready, schedulable node",
			nodes: []runtime.Object{testNode("node1", withUnschedulable())},
			slice: testResourceSlice("cd-1", draDriverComputeDomain, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("channel-0")}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(append(healthyDRADriverObjects(), tt.nodes...)...)
			withDRAAPIDiscovery(t, client)

			ctx := &validators.Context{
				Ctx:           context.Background(),
				Clientset:     client,
				DynamicClient: newDRAFakeDynamicClient(tt.slice),
			}

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected failure when NVIDIA ResourceSlices do not pass validation")
			}
			if isSkipError(err) {
				t.Fatalf("error = %v, want a hard failure, not a skip", err)
			}
			if !strings.Contains(err.Error(), "none passed validation") {
				t.Fatalf("error = %v, want message about slices failing validation", err)
			}
		})
	}
}

func TestValidateNVIDIAResourceSlices_NodeListTimeout(t *testing.T) {
	client := k8sfake.NewClientset()
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewServerTimeout(
			schema.GroupResource{Resource: "nodes"}, "list", 1)
	})
	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	version := strings.TrimPrefix(draAPIGroupVersion, apiGroupResourceK8sIO+"/")
	err := validateNVIDIAResourceSlices(ctx, newDRAFakeDynamicClient(), version)
	if err == nil {
		t.Fatal("expected the node list to time out")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error = %v, want ErrCodeTimeout", err)
	}
	if !strings.Contains(err.Error(), "timed out reading nodes for ResourceSlice validation") {
		t.Errorf("error = %v, want node-list timeout context", err)
	}
}

func TestCheckDRASupport_FailsWithoutNVIDIAResourceSlices(t *testing.T) {
	client := k8sfake.NewClientset(healthyDRADriverObjects()...)
	withDRAAPIDiscovery(t, client)

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
		DynamicClient: newDRAFakeDynamicClient(
			testResourceSlice("other-1", "other.example.com", "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("dev-0")}),
		),
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected failure when no NVIDIA driver ResourceSlices exist")
	}
	if isSkipError(err) {
		t.Fatalf("error = %v, want a hard failure, not a skip", err)
	}
	if !strings.Contains(err.Error(), "no ResourceSlices from NVIDIA DRA drivers") {
		t.Fatalf("error = %v, want message about missing NVIDIA ResourceSlices", err)
	}
}

// TestCheckDRASupport_UsesResolvedComponentRefNamespace verifies the driver
// probes (pod list, controller Deployment, kubelet plugin DaemonSet) target
// the namespace resolved from the recipe's enabled nvidia-dra-driver-gpu
// componentRef — e.g. recipes/mixins/os-talos.yaml overrides it to
// privileged-nvidia-dra-driver-gpu — and fall back to the default
// draDriverNamespace when the ref carries no namespace.
func TestCheckDRASupport_UsesResolvedComponentRefNamespace(t *testing.T) {
	const talosNamespace = "privileged-nvidia-dra-driver-gpu"
	tests := []struct {
		name string
		ref  recipe.ComponentRef
		// wantNamespace is where the driver objects live AND where every
		// probe must be observed.
		wantNamespace string
	}{
		{
			name:          "talos-style ref with custom namespace probes that namespace",
			ref:           recipe.ComponentRef{Name: draDriverComponentName, Namespace: talosNamespace},
			wantNamespace: talosNamespace,
		},
		{
			name:          "ref without a resolved namespace falls back to the default",
			ref:           recipe.ComponentRef{Name: draDriverComponentName},
			wantNamespace: draDriverNamespace,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A healthy driver installed ONLY in the expected namespace: were
			// a probe to target any other namespace, the check would fail.
			client := k8sfake.NewClientset(append(
				healthyDRADriverObjectsIn(tt.wantNamespace), testNode("node1"))...)
			withDRAAPIDiscovery(t, client)

			ctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
				DynamicClient: newDRAFakeDynamicClient(
					testResourceSlice("cd-1", draDriverComputeDomain, "node1", 1, 1,
						map[string]interface{}{"nodeName": "node1"},
						[]interface{}{plainDevice("channel-0")}),
				),
				ValidationInput: validationInputWithRefs(tt.ref),
			}

			if err := CheckDRASupport(ctx); err != nil {
				t.Fatalf("CheckDRASupport() error = %v, want pass with driver in namespace %s", err, tt.wantNamespace)
			}

			// Assert via the fake client's recorded actions that every driver
			// probe hit the resolved namespace.
			probes := map[string]bool{"pods": false, "deployments": false, "daemonsets": false}
			for _, action := range client.Actions() {
				res := action.GetResource().Resource
				if _, tracked := probes[res]; !tracked {
					continue
				}
				verb := action.GetVerb()
				if res == "pods" && verb != "list" {
					continue
				}
				if (res == "deployments" || res == "daemonsets") && verb != "get" {
					continue
				}
				probes[res] = true
				if got := action.GetNamespace(); got != tt.wantNamespace {
					t.Errorf("%s %s targeted namespace %q, want %q", verb, res, got, tt.wantNamespace)
				}
			}
			for res, sawProbe := range probes {
				if !sawProbe {
					t.Errorf("no probe recorded for %s — cannot verify the namespace it targeted", res)
				}
			}
		})
	}
}

// TestCheckDRASupport_FailsFastWhenDeadlineTooShortForCleanup verifies the
// dynamic work-budget gate for IN-SCOPE runs: once scoping determines DRA is
// expected (via the standalone read-only presence probe, or an enabled recipe
// componentRef), a deadline that cannot fit the cleanup reserve plus a
// minimum of work must fail with ErrCodeTimeout BEFORE creating any resource
// — read-only scoping probes are allowed, creates are not.
func TestCheckDRASupport_FailsFastWhenDeadlineTooShortForCleanup(t *testing.T) {
	tests := []struct {
		name       string
		validation *v1.ValidationInput
	}{
		{name: "standalone with driver installed", validation: nil},
		{name: "recipe enables driver", validation: validationInputWithRefs(enabledRef(draDriverComponentName))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
			withDRAAPIDiscovery(t, client)

			deadlineCtx, cancel := context.WithTimeout(context.Background(), gpuCheckMinWorkBudget)
			defer cancel()
			ctx := &validators.Context{
				Ctx:             deadlineCtx,
				Clientset:       client,
				DynamicClient:   newDRAFakeDynamicClient(),
				ValidationInput: tt.validation,
			}

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected fail-fast when the deadline cannot fit the cleanup reserve")
			}
			if isSkipError(err) {
				t.Fatalf("error = %v, want a hard failure, not a skip", err)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Errorf("error code = %v, want ErrCodeTimeout", err)
			}
			if !strings.Contains(err.Error(), "too short to guarantee cleanup") {
				t.Errorf("error = %v, want message about the timeout being too short to guarantee cleanup", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" {
					t.Errorf("fail-fast path issued create on %s — must not create any resource", action.GetResource().Resource)
				}
			}
		})
	}
}

// TestCheckDRASupport_StandaloneProbeBoundedWithoutParentDeadline verifies
// the standalone live-driver presence probe is bounded even when the parent
// context carries NO deadline (library use): the probe runs under
// draProbeTimeout — it must not inherit an unbounded context and hang — and a
// probe exceeding that budget surfaces as ErrCodeTimeout, not a skip and not
// a generic ErrCodeInternal.
func TestCheckDRASupport_StandaloneProbeBoundedWithoutParentDeadline(t *testing.T) {
	oldTimeout := draProbeTimeout
	draProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { draProbeTimeout = oldTimeout })

	client := k8sfake.NewClientset()
	// The fake clientset ignores the caller's context, so emulate a hanging
	// apiserver with a reactor that blocks past the probe budget and then
	// fails the way a real ctx-bound client does once its deadline expires.
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(4 * draProbeTimeout)
		return true, nil, context.DeadlineExceeded
	})

	ctx := &validators.Context{
		Ctx:       context.Background(), // deadline-less parent — the probe must bound itself
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected a timeout failure from the bounded probe")
	}
	if isSkipError(err) {
		t.Fatalf("error = %v, want a timeout failure, not a skip", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout", err)
	}
	if !strings.Contains(err.Error(), "presence probe did not complete") {
		t.Errorf("error = %v, want message attributing the timeout to the presence probe", err)
	}
}

// TestCheckDRASupport_SkipsSurviveShortDeadline pins the gate ordering: the
// scoping decision (pure recipe check, or the standalone read-only presence
// probe) runs BEFORE the work-budget gate, so an out-of-scope recipe or an
// uninstalled driver still yields its documented skip — not ErrCodeTimeout —
// under a deadline too short for the cleanup reserve. Nothing in these paths
// creates resources, so no cleanup headroom is needed.
func TestCheckDRASupport_SkipsSurviveShortDeadline(t *testing.T) {
	tests := []struct {
		name       string
		objects    []runtime.Object
		validation *v1.ValidationInput
		wantMsg    string
	}{
		{
			name:       "recipe omits driver component",
			objects:    nil,
			validation: validationInputWithRefs(enabledRef("gpu-operator")),
			wantMsg:    "out of scope",
		},
		{
			name:       "recipe disables driver despite installed driver",
			objects:    healthyDRADriverObjects(),
			validation: validationInputWithRefs(disabledRef(draDriverComponentName)),
			wantMsg:    "out of scope",
		},
		{
			name:       "standalone without driver installed",
			objects:    nil,
			validation: nil,
			wantMsg:    "not installed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(tt.objects...)

			deadlineCtx, cancel := context.WithTimeout(context.Background(), gpuCheckMinWorkBudget)
			defer cancel()
			ctx := &validators.Context{
				Ctx:             deadlineCtx,
				Clientset:       client,
				ValidationInput: tt.validation,
			}

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected a skip")
			}
			if !isSkipError(err) {
				t.Fatalf("error = %v, want a skip even under a too-short deadline", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want skip message containing %q", err, tt.wantMsg)
			}
			if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Errorf("error = %v — the skip must not be replaced by ErrCodeTimeout", err)
			}
		})
	}
}

func TestCheckDRASupport_RunsBehavioralAllocationWhenFullGPUDRAUsable(t *testing.T) {
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
	withDRAAPIDiscovery(t, client)
	created := markPodsSucceededOnCreate(client)

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("gpu-0")}),
		),
	}

	if err := CheckDRASupport(ctx); err != nil {
		t.Fatalf("CheckDRASupport() error = %v, want pass", err)
	}
	if findPodByPrefix(*created, gpuTestPodPrefix) == nil {
		t.Error("behavioral allocation pod was not created despite full-GPU DRA being usable")
	}
}

// TestCheckDRASupport_ClaimReadErrorClassification proves the behavioral
// subtest's ResourceClaim read is wired to the SHARED classifier
// (classifyK8sReadError): a true NotFound surfaces as ErrCodeNotFound and an
// apiserver timeout as ErrCodeTimeout — not everything as ErrCodeInternal.
func TestCheckDRASupport_ClaimReadErrorClassification(t *testing.T) {
	claimsGR := schema.GroupResource{Group: apiGroupResourceK8sIO, Resource: "resourceclaims"}
	tests := []struct {
		name       string
		getErr     error
		wantTarget error
		wantMsg    string
	}{
		{
			name:       "claim NotFound surfaces as ErrCodeNotFound",
			getErr:     k8serrors.NewNotFound(claimsGR, "gpu-claim"),
			wantTarget: errors.New(errors.ErrCodeNotFound, ""),
			wantMsg:    "not found",
		},
		{
			name:       "apiserver ServerTimeout surfaces as ErrCodeTimeout",
			getErr:     k8serrors.NewServerTimeout(claimsGR, "get", 1),
			wantTarget: errors.New(errors.ErrCodeTimeout, ""),
			wantMsg:    "timed out reading DRA test ResourceClaim",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
			withDRAAPIDiscovery(t, client)
			markPodsSucceededOnCreate(client)

			dynClient := newDRAFakeDynamicClient(
				testDeviceClass(draDriverGPU),
				testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
					map[string]interface{}{"nodeName": "node1"},
					[]interface{}{plainDevice("gpu-0")}),
			)
			// Fail only the claim GET (creates still succeed) so the
			// behavioral subtest reaches its post-wait claim read.
			dynClient.PrependReactor("get", "resourceclaims",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})

			ctx := &validators.Context{
				Ctx:           context.Background(),
				Clientset:     client,
				DynamicClient: dynClient,
			}

			err := CheckDRASupport(ctx)
			if err == nil {
				t.Fatal("expected the behavioral subtest's claim read to fail")
			}
			if !stderrors.Is(err, tt.wantTarget) {
				t.Errorf("error = %v, want code %v to match", err, tt.wantTarget)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want message containing %q", err, tt.wantMsg)
			}
		})
	}
}

// TestCheckDRASupport_BehavioralFailureStillRecordsPodLogs pins dra-support's
// diagnostic ordering: the behavioral test pod's status and container logs
// are recorded BEFORE the claim read and the phase verdict, so a failing
// allocation still ships its logs as evidence.
func TestCheckDRASupport_BehavioralFailureStillRecordsPodLogs(t *testing.T) {
	client := k8sfake.NewClientset(append(healthyDRADriverObjects(), testNode("node1"))...)
	withDRAAPIDiscovery(t, client)
	markPodsTerminalOnCreate(client, func(string) (corev1.PodPhase, int32) {
		return corev1.PodFailed, 1
	})

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("gpu-1", draDriverGPU, "node1", 1, 1,
				map[string]interface{}{"nodeName": "node1"},
				[]interface{}{plainDevice("gpu-0")}),
		),
	}

	var err error
	out := captureStdout(t, func() { err = CheckDRASupport(ctx) })
	if err == nil {
		t.Fatal("expected the behavioral allocation to fail")
	}
	if !strings.Contains(err.Error(), "phase=Failed") {
		t.Errorf("error = %v, want the pod-phase failure", err)
	}
	for _, want := range []string{
		"--- Pod status ---",
		"--- Pod logs (" + containerNameGPUTest + ") ---",
		"--- Pod logs (" + containerNameUnauthorized + ") ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("emitted evidence missing %q despite the failure", want)
		}
	}
	if got := podLogFetchCount(client); got < 2 {
		t.Errorf("pods/log fetches = %d, want >= 2 — failing behavioral pod must still record log artifacts", got)
	}
}

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
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// webhookConfig builds an admission configuration carrying a single named webhook
// pointing at the controller Service in the default self-install namespace.
func webhookConfig(kind, name, webhookName string) *unstructured.Unstructured {
	return webhookConfigIn(kind, name, webhookName, trainerNamespace)
}

// webhookConfigIn builds an admission configuration whose webhook targets the
// controller Service in the given namespace, which is how the probe discovers
// where Trainer is installed.
func webhookConfigIn(kind, name, webhookName, namespace string) *unstructured.Unstructured {
	obj := newTestObject("admissionregistration.k8s.io/v1", kind, "", name)
	if err := unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{
			keyName: webhookName,
			"clientConfig": map[string]any{
				"service": map[string]any{
					keyName:     trainerControllerService,
					"namespace": namespace,
				},
			},
		},
	}, "webhooks"); err != nil {
		panic(err)
	}
	return obj
}

// trainerInstallIn returns a complete Trainer installation laid out in the given
// namespace, as either supported deployment path produces.
func trainerInstallIn(namespace string) []runtime.Object {
	deploy := readyTrainerDeploymentNamed(namespace, trainerControllerDeployment)
	return []runtime.Object{
		establishedCRD(trainerCRDTrainJobs),
		establishedCRD(trainerCRDTrainingRuntimes),
		establishedCRD(jobSetCRDName),
		deploy,
		newTestObject("v1", "Service", namespace, trainerControllerService),
		webhookConfigIn("ValidatingWebhookConfiguration", trainerValidatingWebhookConfig,
			trainerValidatingWebhookName, namespace),
		webhookConfigIn("MutatingWebhookConfiguration", trainerMutatingWebhookConfig,
			trainerMutatingWebhookName, namespace),
	}
}

// malformedWebhookConfig builds an admission configuration whose entries are not
// objects, exercising the probe's tolerance of unexpected shapes.
func malformedWebhookConfig(kind, name string) *unstructured.Unstructured {
	obj := newTestObject("admissionregistration.k8s.io/v1", kind, "", name)
	if err := unstructured.SetNestedSlice(obj.Object,
		[]any{"not-an-object"}, "webhooks"); err != nil {
		panic(err)
	}
	return obj
}

func trainerControllerSvc() *unstructured.Unstructured {
	return newTestObject("v1", "Service", trainerNamespace, trainerControllerService)
}

// completeTrainerInstall returns every object the installed-state probe requires.
func completeTrainerInstall() []runtime.Object {
	return trainerInstallIn(trainerNamespace)
}

// withoutObject returns objs minus every object that drop matches.
func withoutObject(objs []runtime.Object, drop func(runtime.Object) bool) []runtime.Object {
	kept := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		if drop(o) {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

func dropByName(name string) func(runtime.Object) bool {
	return func(o runtime.Object) bool {
		u, ok := o.(*unstructured.Unstructured)
		return ok && u.GetName() == name
	}
}

// TestIsTrainerInstalled_CompleteInstall is the positive control, run against both
// supported layouts. The self-install overlay lands in kubeflow-system; the Helm
// chart the recipes deploy (registry.yaml pins defaultNamespace: kubeflow) lands in
// kubeflow. A probe that recognized only one would report the other as incomplete
// and reinstall over a healthy Trainer.
func TestIsTrainerInstalled_CompleteInstall(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
	}{
		{name: "self-install overlay layout", namespace: trainerNamespace},
		{name: "helm chart layout", namespace: "kubeflow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTrainerFakeClient(trainerInstallIn(tt.namespace)...)

			install, installed, err := isTrainerInstalled(context.Background(), client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !installed {
				t.Fatal("complete Trainer installation reported as not installed")
			}
			if install.Namespace != tt.namespace {
				t.Errorf("discovered namespace = %q, want %q", install.Namespace, tt.namespace)
			}
		})
	}
}

// TestTrainerUpstreamNamesLocked pins the object names the probe looks up to the
// literals both supported deployment paths actually emit. The fixtures above are
// built from these same constants, so they cannot catch a wrong value — only a
// literal comparison can.
//
// Values verified against kubeflow/trainer v2.2.0: the generic kubebuilder names
// in manifests/base/webhook/manifests.yaml are replaced by
// manifests/base/webhook/patch_{validating,mutating}.yaml, each an `op: replace`
// on /metadata/name. The kubeflow-trainer Helm chart renders the same two names.
// Getting these wrong makes the probe look up objects that never exist, so it can
// never report installed and every run reinstalls over a healthy Trainer.
func TestTrainerUpstreamNamesLocked(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"validating webhook configuration", trainerValidatingWebhookConfig, "validator.trainer.kubeflow.org"},
		{"mutating webhook configuration", trainerMutatingWebhookConfig, "defaulter.trainer.kubeflow.org"},
		{"validating webhook entry", trainerValidatingWebhookName, "validator.trainjob.trainer.kubeflow.org"},
		{"mutating webhook entry", trainerMutatingWebhookName, "defaulter.trainjob.trainer.kubeflow.org"},
		{"controller deployment", trainerControllerDeployment, "kubeflow-trainer-controller-manager"},
		{"controller service", trainerControllerService, "kubeflow-trainer-controller-manager"},
		{"jobset crd", jobSetCRDName, "jobsets.jobset.x-k8s.io"},
		{"jobset name label", jobSetNameLabel, "app.kubernetes.io/name"},
		{"jobset label value", jobSetLabelValue, "jobset"},
		{"trainjob crd", trainerCRDTrainJobs, "trainjobs.trainer.kubeflow.org"},
		{"trainingruntime crd", trainerCRDTrainingRuntimes, "trainingruntimes.trainer.kubeflow.org"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q (must match upstream v2.2.0)", tt.got, tt.want)
			}
		})
	}
}

// TestIsTrainerInstalled_PartialInstallRejected is the regression guard for the
// single-CRD probe: any missing piece of the Trainer dependency must report false
// so the caller reinstalls rather than driving a broken controller.
func TestIsTrainerInstalled_PartialInstallRejected(t *testing.T) {
	tests := []struct {
		name string
		objs []runtime.Object
	}{
		{
			name: "leaked TrainJob CRD from a failed install is not a complete install",
			objs: []runtime.Object{establishedCRD(trainerCRDTrainJobs)},
		},
		{
			name: "TrainingRuntime CRD missing",
			objs: withoutObject(completeTrainerInstall(), dropByName(trainerCRDTrainingRuntimes)),
		},
		{
			name: "controller Deployment missing",
			objs: withoutObject(completeTrainerInstall(), func(o runtime.Object) bool {
				u, ok := o.(*unstructured.Unstructured)
				return ok && u.GetKind() == "Deployment"
			}),
		},
		{
			name: "webhook Service missing",
			objs: withoutObject(completeTrainerInstall(), func(o runtime.Object) bool {
				u, ok := o.(*unstructured.Unstructured)
				return ok && u.GetKind() == "Service"
			}),
		},
		{
			name: "ValidatingWebhookConfiguration missing",
			objs: withoutObject(completeTrainerInstall(), func(o runtime.Object) bool {
				u, ok := o.(*unstructured.Unstructured)
				return ok && u.GetKind() == "ValidatingWebhookConfiguration"
			}),
		},
		{
			name: "MutatingWebhookConfiguration missing",
			objs: withoutObject(completeTrainerInstall(), func(o runtime.Object) bool {
				u, ok := o.(*unstructured.Unstructured)
				return ok && u.GetKind() == "MutatingWebhookConfiguration"
			}),
		},
		{
			name: "CRD present but not established",
			objs: append(
				withoutObject(completeTrainerInstall(), dropByName(trainerCRDTrainJobs)),
				newTestObject(apiGroupAPIExtensions+"/v1", "CustomResourceDefinition", "", trainerCRDTrainJobs),
			),
		},
		{
			name: "webhook configuration owned by an unrelated operator",
			objs: append(
				withoutObject(completeTrainerInstall(), dropByName(trainerValidatingWebhookConfig)),
				webhookConfig("ValidatingWebhookConfiguration", trainerValidatingWebhookConfig,
					"validator.other.example.com"),
			),
		},
		{
			name: "mutating webhook configuration owned by an unrelated operator",
			objs: append(
				withoutObject(completeTrainerInstall(), dropByName(trainerMutatingWebhookConfig)),
				webhookConfig("MutatingWebhookConfiguration", trainerMutatingWebhookConfig,
					"defaulter.other.example.com"),
			),
		},
		{
			name: "webhook configuration with a malformed entry",
			objs: append(
				withoutObject(completeTrainerInstall(), dropByName(trainerValidatingWebhookConfig)),
				malformedWebhookConfig("ValidatingWebhookConfiguration", trainerValidatingWebhookConfig),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTrainerFakeClient(tt.objs...)

			_, installed, err := isTrainerInstalled(context.Background(), client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if installed {
				t.Error("incomplete Trainer installation reported as installed")
			}
		})
	}
}

// TestIsTrainerInstalled_APIErrorSurfaces verifies the probe fails closed: an API
// error it cannot classify is returned rather than reported as "not installed",
// which would trigger a needless reinstall.
func TestIsTrainerInstalled_APIErrorSurfaces(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)
	client.PrependReactor("get", resourceCRDs, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	_, installed, err := isTrainerInstalled(context.Background(), client)
	if err == nil {
		t.Fatal("expected API error to surface, got nil")
	}
	if installed {
		t.Error("reported installed on API error, want not-installed")
	}
}

// TestIsTrainerInstalled_TerminatingObjectIsNotInstalled verifies an object being
// torn down by a concurrent run is not counted as present. Otherwise a run starting
// during another's teardown takes the already-installed branch and waits out the
// readiness timeout against a dying controller instead of reinstalling.
func TestIsTrainerInstalled_TerminatingObjectIsNotInstalled(t *testing.T) {
	terminating := establishedCRD(trainerCRDTrainJobs)
	terminating.SetDeletionTimestamp(&metav1.Time{Time: metav1.Now().Time})

	objs := append(withoutObject(completeTrainerInstall(), dropByName(trainerCRDTrainJobs)), terminating)
	client := newTrainerFakeClient(objs...)

	_, installed, err := isTrainerInstalled(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installed {
		t.Error("a terminating Trainer object was counted as installed")
	}
}

// TestApplyTrainerResources_RefusesForeignWebhookConfig verifies install fails
// closed rather than overwriting an admission configuration owned by another
// operator. Upstream names these generically, and because an update excludes the
// object from the rollback set, a clobbered foreign config is never restored.
func TestApplyTrainerResources_RefusesForeignWebhookConfig(t *testing.T) {
	foreign := webhookConfig("ValidatingWebhookConfiguration",
		trainerValidatingWebhookConfig, "validator.other.example.com")
	client := newTrainerFakeClient(foreign)

	objs := []*unstructured.Unstructured{
		webhookConfig("ValidatingWebhookConfiguration",
			trainerValidatingWebhookConfig, trainerValidatingWebhookName),
	}

	_, err := applyTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected install to refuse overwriting a foreign webhook configuration")
	}

	got, getErr := client.Resource(trainerValidatingWebhookGVR).
		Get(context.Background(), trainerValidatingWebhookConfig, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("unexpected error: %v", getErr)
	}
	entries, _, _ := unstructured.NestedSlice(got.Object, "webhooks")
	if len(entries) != 1 {
		t.Fatalf("webhooks = %d entries, want 1", len(entries))
	}
	if name := entries[0].(map[string]any)[keyName]; name != "validator.other.example.com" {
		t.Errorf("foreign webhook config was clobbered: entry name = %v", name)
	}
}

// TestApplyTrainerResources_UpdatesOwnWebhookConfig is the counterpart: a config
// that does carry Trainer entries is ours to re-apply, so the refusal above must
// not block the ordinary repair path.
func TestApplyTrainerResources_UpdatesOwnWebhookConfig(t *testing.T) {
	client := newTrainerFakeClient(
		webhookConfig("ValidatingWebhookConfiguration",
			trainerValidatingWebhookConfig, trainerValidatingWebhookName),
	)

	objs := []*unstructured.Unstructured{
		webhookConfig("ValidatingWebhookConfiguration",
			trainerValidatingWebhookConfig, trainerValidatingWebhookName),
	}

	if _, err := applyTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs); err != nil {
		t.Fatalf("re-applying our own webhook configuration should succeed, got: %v", err)
	}
}

// TestWaitForDeploymentReady_TerminalErrorFailsFast verifies an RBAC failure is
// reported for what it is instead of looping to a generic readiness timeout that
// hides the cause.
func TestWaitForDeploymentReady_TerminalErrorFailsFast(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "deployments"}, trainerControllerDeployment, errBoom)
	})

	err := waitForDeploymentReady(context.Background(), client,
		trainerNamespace, trainerControllerDeployment, time.Minute)
	if err == nil {
		t.Fatal("expected forbidden Get to fail fast, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeUnauthorized, "")) {
		t.Errorf("error code is not Unauthorized: %v", err)
	}
}

// TestWaitForJobSetControllerReady_AbsentIsNotAFailure verifies the bundled JobSet
// check is skipped when JobSet is managed elsewhere: the Trainer overlay lets that
// resource be omitted, so requiring it would break those clusters.
func TestWaitForJobSetControllerReady_AbsentIsNotAFailure(t *testing.T) {
	client := newTrainerFakeClient()

	if err := waitForJobSetControllerReady(context.Background(), client, trainerNamespace); err != nil {
		t.Errorf("absent bundled JobSet controller should be skipped, got: %v", err)
	}
}

// TestWaitForJobSetControllerReady_PresentButNotReady is the #1430 guard: a stale
// install whose JobSet controller is stuck in ImagePullBackOff must be caught here
// rather than surfacing much later as a TrainJob whose JobSet is never created.
//
// Run against the in-tree layout and an externally managed chart layout with a
// custom name, so only the shared label locates both reliably.
func TestWaitForJobSetControllerReady_PresentButNotReady(t *testing.T) {
	tests := []struct {
		name       string
		namespace  string
		deployment string
	}{
		{
			name:       "kustomize overlay layout",
			namespace:  trainerNamespace,
			deployment: "jobset-controller-manager",
		},
		{
			name:       "externally managed chart layout with a custom name",
			namespace:  "kubeflow",
			deployment: "kubeflow-trainer-jobset-controller",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertJobSetNotReady(t, tt.namespace, tt.deployment)
		})
	}
}

// assertJobSetNotReady drives waitForJobSetControllerReady against an unready
// JobSet controller and requires it to fail rather than silently skip.
func assertJobSetNotReady(t *testing.T, namespace, deployment string) {
	t.Helper()

	notReady := newTestObject("apps/v1", "Deployment", namespace, deployment)
	notReady.SetLabels(map[string]string{jobSetNameLabel: jobSetLabelValue})
	if err := unstructured.SetNestedField(notReady.Object, int64(0), "status", "readyReplicas"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	client := newTrainerFakeClient(notReady)

	// Let the readiness poll observe the unready controller once, then cancel from
	// the reactor: deterministic and timer-free.
	ctx, cancel := context.WithCancel(context.Background())
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})
	defer cancel()

	if err := waitForJobSetControllerReady(ctx, client, namespace); err == nil {
		t.Error("a present-but-unready JobSet controller must fail readiness")
	}
}

// TestIsTrainerInstalled_StopsOnCanceledContext verifies the probe aborts instead
// of issuing its remaining reads once the caller has given up.
func TestIsTrainerInstalled_StopsOnCanceledContext(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, installed, err := isTrainerInstalled(ctx, client)
	if err == nil {
		t.Fatal("expected canceled context to abort the probe, got nil error")
	}
	if installed {
		t.Error("reported installed on canceled context, want not-installed")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" {
			t.Errorf("probed %q after cancellation; the loop must abort first",
				action.GetResource().Resource)
		}
	}
}

// TestTrainerProbeErrorCode verifies a failed probe read keeps the distinction
// between "the cluster is unreachable right now" and "the code is broken". The
// probe error reaches the validation verdict, so collapsing everything to
// ErrCodeInternal would misreport a transient outage as a product defect.
func TestTrainerAPIErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want aicrErrors.ErrorCode
	}{
		{
			name: "canceled context is a timeout",
			err:  context.Canceled,
			want: aicrErrors.ErrCodeTimeout,
		},
		{
			name: "expired deadline is a timeout",
			err:  context.DeadlineExceeded,
			want: aicrErrors.ErrCodeTimeout,
		},
		{
			name: "apiserver unavailable is a service outage",
			err:  apierrors.NewServiceUnavailable("apiserver is down"),
			want: aicrErrors.ErrCodeUnavailable,
		},
		{
			name: "throttled request is a service outage",
			err:  apierrors.NewTooManyRequests("slow down", 1),
			want: aicrErrors.ErrCodeUnavailable,
		},
		{
			name: "server timeout is a service outage",
			err: apierrors.NewServerTimeout(
				schema.GroupResource{Resource: "services"}, "get", 1),
			want: aicrErrors.ErrCodeUnavailable,
		},
		{
			name: "request timeout is a service outage",
			err:  apierrors.NewTimeoutError("request timed out", 1),
			want: aicrErrors.ErrCodeUnavailable,
		},
		{
			name: "connectivity failure is a service outage",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errBoom},
			want: aicrErrors.ErrCodeUnavailable,
		},
		{
			name: "unrecognized failure stays internal",
			err:  errBoom,
			want: aicrErrors.ErrCodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trainerAPIErrorCode(tt.err); got != tt.want {
				t.Errorf("trainerAPIErrorCode() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsTrainerInstalled_UnavailableAPISurfacesAsUnavailable is the end-to-end
// half of the classification: an apiserver outage during the probe must not be
// reported to the caller as an internal fault.
func TestIsTrainerInstalled_UnavailableAPISurfacesAsUnavailable(t *testing.T) {
	client := newTrainerFakeClient(completeTrainerInstall()...)
	client.PrependReactor("get", resourceCRDs, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	_, _, err := isTrainerInstalled(context.Background(), client)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeUnavailable, "")) {
		t.Errorf("error code is not Unavailable: %v", err)
	}
}

// TestUpdateExistingTrainerResource_PreservesServiceClusterIPs pins why aborting on
// update failure is safe: re-applying the Trainer Service over a leftover one must
// carry the apiserver-assigned cluster IPs, which are immutable and absent from the
// rendered manifest. Dropping them makes every repair attempt fail.
func TestUpdateExistingTrainerResource_PreservesServiceClusterIPs(t *testing.T) {
	live := trainerControllerSvc()
	if err := unstructured.SetNestedField(live.Object, "10.96.0.42", "spec", "clusterIP"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := unstructured.SetNestedStringSlice(live.Object, []string{"10.96.0.42"}, "spec", "clusterIPs"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	client := newTrainerFakeClient(live)
	svcClient := client.Resource(trainerServiceGVR).Namespace(trainerNamespace)

	// The rendered manifest carries no cluster IP, as kustomize output does not.
	if err := updateExistingTrainerResource(context.Background(), svcClient, trainerControllerSvc()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := svcClient.Get(context.Background(), trainerControllerService, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ip, found, err := unstructured.NestedString(got.Object, "spec", "clusterIP")
	if err != nil || !found {
		t.Fatalf("clusterIP not preserved (found=%v err=%v)", found, err)
	}
	if ip != "10.96.0.42" {
		t.Errorf("clusterIP = %q, want 10.96.0.42", ip)
	}

	// clusterIPs is copied by a separate branch, and the apiserver rejects a
	// dropped clusterIPs exactly as it rejects a dropped clusterIP.
	ips, found, err := unstructured.NestedStringSlice(got.Object, "spec", "clusterIPs")
	if err != nil || !found {
		t.Fatalf("clusterIPs not preserved (found=%v err=%v)", found, err)
	}
	if len(ips) != 1 || ips[0] != "10.96.0.42" {
		t.Errorf("clusterIPs = %v, want [10.96.0.42]", ips)
	}
}

// TestUpdateExistingTrainerResource_PreservesDualStackClusterIPs covers the
// dual-stack shape, where dropping the second address is just as fatal as
// dropping the first.
func TestUpdateExistingTrainerResource_PreservesDualStackClusterIPs(t *testing.T) {
	want := []string{"10.96.0.42", "fd00::42"}

	live := trainerControllerSvc()
	if err := unstructured.SetNestedField(live.Object, want[0], "spec", "clusterIP"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := unstructured.SetNestedStringSlice(live.Object, want, "spec", "clusterIPs"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	client := newTrainerFakeClient(live)
	svcClient := client.Resource(trainerServiceGVR).Namespace(trainerNamespace)

	if err := updateExistingTrainerResource(context.Background(), svcClient, trainerControllerSvc()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := svcClient.Get(context.Background(), trainerControllerService, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ips, found, err := unstructured.NestedStringSlice(got.Object, "spec", "clusterIPs")
	if err != nil || !found {
		t.Fatalf("clusterIPs not preserved (found=%v err=%v)", found, err)
	}
	if len(ips) != len(want) || ips[0] != want[0] || ips[1] != want[1] {
		t.Errorf("clusterIPs = %v, want %v", ips, want)
	}
}

// TestUpdateExistingTrainerResource_RetriesOnConflict verifies a lost optimistic-
// concurrency race is retried rather than failing the install. Since an update
// failure now aborts the whole benchmark, a concurrent writer touching the same
// leftover Trainer object must not be able to sink an otherwise-good run.
func TestUpdateExistingTrainerResource_RetriesOnConflict(t *testing.T) {
	client := newTrainerFakeClient(trainerControllerSvc())
	svcClient := client.Resource(trainerServiceGVR).Namespace(trainerNamespace)

	var updates, reads int
	client.PrependReactor("update", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "services"}, trainerControllerService, errBoom)
		}
		return false, nil, nil
	})
	client.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		return false, nil, nil
	})

	if err := updateExistingTrainerResource(context.Background(), svcClient, trainerControllerSvc()); err != nil {
		t.Fatalf("conflict should have been retried, got: %v", err)
	}
	if updates != 2 {
		t.Errorf("update attempts = %d, want 2 (one conflict, one success)", updates)
	}
	// The retry is only correct if it re-reads: replaying the resourceVersion that
	// just conflicted would conflict forever against a real apiserver.
	if reads != 2 {
		t.Errorf("reads = %d, want 2 (each retry must re-read the live object)", reads)
	}
}

// TestUpdateExistingTrainerResource_DoesNotRetryOtherErrors verifies only conflicts
// are retried; a genuine failure surfaces on the first attempt rather than burning
// the backoff budget.
func TestUpdateExistingTrainerResource_DoesNotRetryOtherErrors(t *testing.T) {
	client := newTrainerFakeClient(trainerControllerSvc())
	svcClient := client.Resource(trainerServiceGVR).Namespace(trainerNamespace)

	var updates int
	client.PrependReactor("update", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	if err := updateExistingTrainerResource(context.Background(), svcClient, trainerControllerSvc()); err == nil {
		t.Fatal("expected non-conflict error to surface, got nil")
	}
	if updates != 1 {
		t.Errorf("update attempts = %d, want 1 (non-conflict errors must not retry)", updates)
	}
}

// TestTrainerResourceRefString verifies cleanup diagnostics name namespaced and
// cluster-scoped resources unambiguously.
func TestTrainerResourceRefString(t *testing.T) {
	tests := []struct {
		name string
		ref  trainerResourceRef
		want string
	}{
		{
			name: "namespaced resource includes its namespace",
			ref: trainerResourceRef{
				GVR:       schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
				Namespace: trainerNamespace, Name: "kubeflow-trainer-config",
			},
			want: "configmaps kubeflow-system/kubeflow-trainer-config",
		},
		{
			name: "cluster-scoped resource omits the namespace",
			ref:  trainerResourceRef{GVR: trainerCRDGVR, Name: trainerCRDTrainJobs},
			want: "customresourcedefinitions trainjobs.trainer.kubeflow.org",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIsTrainerInstalled_MutatingConfigReasonsAreDistinct pins the two shapes of a
// missing mutating webhook apart.
//
// "The configuration does not exist" and "the configuration exists but does not
// serve our webhook" need different remedies, and flattening both to the first tells
// an operator to create an object that is already on the cluster — which, on the
// generic upstream configuration names another operator may own, is the shape most
// likely to be hit. The validating-webhook discovery path already keeps them apart;
// this is the same contract on the mutating one.
func TestIsTrainerInstalled_MutatingConfigReasonsAreDistinct(t *testing.T) {
	tests := []struct {
		name         string
		objs         []runtime.Object
		wantContains string
		wantAbsent   string
	}{
		{
			name: "configuration absent",
			objs: withoutObject(completeTrainerInstall(), dropByName(trainerMutatingWebhookConfig)),
			wantContains: fmt.Sprintf("admission configuration %q is missing",
				trainerMutatingWebhookConfig),
		},
		{
			name: "configuration present but serving another operator's webhook",
			objs: append(
				withoutObject(completeTrainerInstall(), dropByName(trainerMutatingWebhookConfig)),
				webhookConfig("MutatingWebhookConfiguration", trainerMutatingWebhookConfig,
					"defaulter.other.example.com"),
			),
			wantContains: fmt.Sprintf("exists but does not contain the %q webhook",
				trainerMutatingWebhookName),
			wantAbsent: fmt.Sprintf("admission configuration %q is missing",
				trainerMutatingWebhookConfig),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			install, installed, err := isTrainerInstalled(context.Background(), newTrainerFakeClient(tt.objs...))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if installed {
				t.Fatal("incomplete Trainer installation reported as installed")
			}
			if !strings.Contains(install.Incomplete, tt.wantContains) {
				t.Errorf("reason = %q, want it to contain %q", install.Incomplete, tt.wantContains)
			}
			if tt.wantAbsent != "" && strings.Contains(install.Incomplete, tt.wantAbsent) {
				t.Errorf("reason %q tells the operator to create a configuration that "+
					"already exists", install.Incomplete)
			}
		})
	}
}

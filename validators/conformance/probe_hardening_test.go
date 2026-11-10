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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The #3703 probe-hardening tests: --image-pull-secret copies into the
// per-run namespace and onto every probe pod, the fast failure on a
// RuntimeClass whose handler the node does not serve, and the pin that keeps
// the observation-only set in step with the registered checks.

const (
	testPullSecretName  = "regcred"
	testValidatorNS     = "aicr-validation"
	testMissingHandler  = "does-not-exist-3703"
	sandboxFastFailWait = 10 * time.Second
)

func pullSecretRun(names ...string) *gpuTestRun {
	return &gpuTestRun{
		token:              "0123456789abcdef0123456789abcdef",
		namespace:          gpuTestNamespacePrefix + "pullsecret",
		podName:            gpuTestPodPrefix + "pullsecret",
		noAllocPodName:     noAllocProbePrefix + "pullsecret",
		imexPodName:        imexTestPodPrefix + "pullsecret",
		driverProbePodName: driverRuntimeProbePodPrefix + "pullsecret",
		imagePullSecrets:   names,
		validatorNamespace: testValidatorNS,
	}
}

func sourcePullSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testPullSecretName, Namespace: testValidatorNS},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}
}

func TestNewGPUTestRun_ReadsImagePullSecretEnv(t *testing.T) {
	t.Setenv(validators.ImagePullSecretsEnv, "regcred, mirror-cred")
	t.Setenv("AICR_NAMESPACE", testValidatorNS)
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun() error = %v", err)
	}
	if want := []string{"regcred", "mirror-cred"}; !slices.Equal(run.imagePullSecrets, want) {
		t.Errorf("imagePullSecrets = %v, want %v", run.imagePullSecrets, want)
	}
	if run.validatorNamespace != testValidatorNS {
		t.Errorf("validatorNamespace = %q, want %q", run.validatorNamespace, testValidatorNS)
	}
	wantRefs := []corev1.LocalObjectReference{{Name: "regcred"}, {Name: "mirror-cred"}}
	if got := run.imagePullSecretRefs(); !slices.Equal(got, wantRefs) {
		t.Errorf("imagePullSecretRefs() = %v, want %v", got, wantRefs)
	}

	t.Setenv(validators.ImagePullSecretsEnv, "")
	run, err = newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun() error = %v", err)
	}
	if run.imagePullSecrets != nil || run.imagePullSecretRefs() != nil {
		t.Errorf("imagePullSecrets = %v, refs %v, want none without the env var", run.imagePullSecrets, run.imagePullSecretRefs())
	}
}

func TestCreateGPUTestNamespace_CopiesImagePullSecrets(t *testing.T) {
	secretsGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	tests := []struct {
		name     string
		run      *gpuTestRun
		objects  []runtime.Object
		react    func(*k8sfake.Clientset)
		wantCode errors.ErrorCode // "" means success
		wantMsg  string
		// wantCopied, when set, is the Secret expected in the run namespace.
		wantCopied bool
	}{
		{
			name:       "copies the named Secret with the run's ownership label",
			run:        pullSecretRun(testPullSecretName),
			objects:    []runtime.Object{sourcePullSecret()},
			wantCopied: true,
		},
		{
			name: "refreshes a copy left by an ambiguous earlier create",
			run:  pullSecretRun(testPullSecretName),
			objects: []runtime.Object{sourcePullSecret(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: testPullSecretName, Namespace: gpuTestNamespacePrefix + "pullsecret"},
				Type:       corev1.SecretTypeDockerConfigJson,
				Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("stale")},
			}},
			wantCopied: true,
		},
		{
			name:     "a Secret the flag names but the validator namespace lacks fails closed",
			run:      pullSecretRun(testPullSecretName),
			wantCode: errors.ErrCodeNotFound,
			wantMsg:  "does not exist in the validator namespace " + testValidatorNS,
		},
		{
			name:    "a Forbidden read is classified",
			run:     pullSecretRun(testPullSecretName),
			objects: []runtime.Object{sourcePullSecret()},
			react: func(c *k8sfake.Clientset) {
				c.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewForbidden(secretsGVR.GroupResource(), testPullSecretName, stderrors.New("RBAC: get denied"))
				})
			},
			wantCode: errors.ErrCodeUnauthorized,
			wantMsg:  "RBAC: get denied",
		},
		{
			name:    "a failed write is classified",
			run:     pullSecretRun(testPullSecretName),
			objects: []runtime.Object{sourcePullSecret()},
			react: func(c *k8sfake.Clientset) {
				c.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewInternalError(stderrors.New("etcd unavailable"))
				})
			},
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "etcd unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset(tt.objects...)
			if tt.react != nil {
				tt.react(clientset)
			}
			err := createGPUTestNamespace(context.Background(), clientset, tt.run)
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %v, want substring %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("createGPUTestNamespace() error = %v", err)
			}
			if !tt.run.nsObserved {
				t.Error("nsObserved = false, want the namespace created before the copy")
			}
			if !tt.wantCopied {
				return
			}
			got, getErr := clientset.CoreV1().Secrets(tt.run.namespace).Get(context.Background(), testPullSecretName, metav1.GetOptions{})
			if getErr != nil {
				t.Fatalf("copied Secret missing from %s: %v", tt.run.namespace, getErr)
			}
			source := sourcePullSecret()
			if got.Type != source.Type || string(got.Data[corev1.DockerConfigJsonKey]) != string(source.Data[corev1.DockerConfigJsonKey]) {
				t.Errorf("copied Secret type/data = %s/%q, want the source's %s/%q",
					got.Type, got.Data[corev1.DockerConfigJsonKey], source.Type, source.Data[corev1.DockerConfigJsonKey])
			}
			if got.Labels[gpuTestRunLabel] != tt.run.token {
				t.Errorf("copied Secret label %s = %q, want the run token", gpuTestRunLabel, got.Labels[gpuTestRunLabel])
			}
		})
	}

	t.Run("no forwarded Secret touches no Secret API", func(t *testing.T) {
		clientset := k8sfake.NewClientset()
		secretCalls := 0
		clientset.PrependReactor("*", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			secretCalls++
			return false, nil, nil
		})
		if err := createGPUTestNamespace(context.Background(), clientset, pullSecretRun()); err != nil {
			t.Fatalf("createGPUTestNamespace() error = %v", err)
		}
		if secretCalls != 0 {
			t.Errorf("secret API calls = %d, want 0 without --image-pull-secret", secretCalls)
		}
	})
}

// TestProbePodsCarryImagePullSecrets pins that every probe the per-run
// namespace hosts references the copied Secrets, so a private mirror serves
// the probe image with the same credentials as the validator image.
func TestProbePodsCarryImagePullSecrets(t *testing.T) {
	run := pullSecretRun(testPullSecretName, "mirror-cred")
	refs := run.imagePullSecretRefs()
	podSpec := func(p *corev1.Pod) corev1.PodSpec { return p.Spec }
	deploySpec := func(d *appsv1.Deployment) corev1.PodSpec { return d.Spec.Template.Spec }
	backend, _ := buildGatewayTrafficBackend(run.namespace, gatewayTrafficPrimary, nil, refs)
	metricsPod, _ := buildAIMetricsWorkload(run.namespace, "run", time.Minute, nil, refs)
	tests := []struct {
		name string
		spec corev1.PodSpec
	}{
		{"DRA GPU test pod", podSpec(buildDRATestPod(run, nil))},
		{"device-plugin GPU test pod", podSpec(buildDevicePluginTestPod(run, nil, []string{"node1"}))},
		{"no-allocation probe pod", podSpec(buildNoAllocationProbePod(run, "node1"))},
		{"RuntimeClass bypass probe pod", podSpec(buildRuntimeClassBypassProbePod(run, "nvidia", "node1", nil))},
		{"driver runtime probe pod (DRA)", podSpec(buildDriverRuntimeProbePod(run, driverRuntimeProbeImage, nil, true, nil))},
		{"driver runtime probe pod (device plugin)", podSpec(buildDriverRuntimeProbePod(run, driverRuntimeProbeImage, nil, false, []string{"node1"}))},
		{"IMEX channel test pod", podSpec(buildIMEXTestPod(run, nil, []string{"node1"}))},
		{"gateway traffic backend", deploySpec(backend)},
		{"gateway traffic pod spec", gatewayTrafficPodSpec(corev1.Container{Name: "c"}, nil, refs)},
		{"HPA workload", deploySpec(buildHPADeployment(run.namespace, false, nil, nil, refs))},
		{"AI metrics workload", podSpec(metricsPod)},
		{"cluster-autoscaling workload", deploySpec(buildClusterAutoTestDeployment("scale", run.namespace, "gpu-pool", refs))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Equal(tt.spec.ImagePullSecrets, refs) {
				t.Errorf("ImagePullSecrets = %v, want %v", tt.spec.ImagePullSecrets, refs)
			}
		})
	}

	// The TrainJob has no pod spec of its own: the Secrets reach its node pod
	// through a spec.runtimePatches entry that Trainer merges into the
	// runtime's pod template.
	t.Run("robust-controller TrainJob", func(t *testing.T) {
		tj := buildValidTrainJob(run.namespace, refs)
		patches, found, err := unstructured.NestedSlice(tj.Object, "spec", "runtimePatches")
		if err != nil || !found || len(patches) != 1 {
			t.Fatalf("spec.runtimePatches = %v (found %v, err %v), want one entry", patches, found, err)
		}
		patch, ok := patches[0].(map[string]any)
		if !ok {
			t.Fatalf("runtimePatches[0] = %T, want a map", patches[0])
		}
		if manager, _, _ := unstructured.NestedString(patch, "manager"); manager != trainJobPatchManager {
			t.Errorf("manager = %q, want %q", manager, trainJobPatchManager)
		}
		jobs, found, err := unstructured.NestedSlice(patch, "trainingRuntimeSpec", "template", "spec", "replicatedJobs")
		if err != nil || !found || len(jobs) != 1 {
			t.Fatalf("replicatedJobs = %v (found %v, err %v), want the node job", jobs, found, err)
		}
		job, ok := jobs[0].(map[string]any)
		if !ok {
			t.Fatalf("replicatedJobs[0] = %T, want a map", jobs[0])
		}
		if name, _, _ := unstructured.NestedString(job, "name"); name != trainJobNodeReplicatedJob {
			t.Errorf("replicated job name = %q, want %q", name, trainJobNodeReplicatedJob)
		}
		secrets, found, err := unstructured.NestedSlice(job, "template", "spec", "template", "spec", "imagePullSecrets")
		if err != nil || !found {
			t.Fatalf("imagePullSecrets missing from the node pod template patch (err %v)", err)
		}
		got := make([]corev1.LocalObjectReference, 0, len(secrets))
		for _, s := range secrets {
			name, _, _ := unstructured.NestedString(s.(map[string]any), "name")
			got = append(got, corev1.LocalObjectReference{Name: name})
		}
		if !slices.Equal(got, refs) {
			t.Errorf("imagePullSecrets = %v, want %v", got, refs)
		}
	})

	t.Run("no Secret forwarded leaves the spec clean", func(t *testing.T) {
		if got := buildDRATestPod(pullSecretRun(), nil).Spec.ImagePullSecrets; got != nil {
			t.Errorf("ImagePullSecrets = %v, want nil", got)
		}
		if got := buildClusterAutoTestDeployment("scale", "ns", "gpu-pool", nil).Spec.Template.Spec.ImagePullSecrets; got != nil {
			t.Errorf("cluster-autoscaling ImagePullSecrets = %v, want nil", got)
		}
		if _, found, _ := unstructured.NestedSlice(buildValidTrainJob("ns", nil).Object, "spec", "runtimePatches"); found {
			t.Error("TrainJob carries spec.runtimePatches without a forwarded Secret; the bare TrainJob is the design")
		}
	})
}

// TestValidateClusterAutoscaling_ImagePullSecrets pins that the
// cluster-autoscaling run copies the forwarded Secrets into its namespace
// before the workload exists, and fails closed when a named Secret is not in
// the validator namespace, exactly as the gpuTestRun checks do.
func TestValidateClusterAutoscaling_ImagePullSecrets(t *testing.T) {
	shortenCleanupTimeout(t)
	t.Setenv(validators.ImagePullSecretsEnv, testPullSecretName)
	t.Setenv("AICR_NAMESPACE", testValidatorNS)

	t.Run("copied before the workload", func(t *testing.T) {
		c := k8sfake.NewClientset(sourcePullSecret())
		passClusterAutoscalingFakes(c, "gpu-pool")
		// Reactors record the order of the writes: the fake holds its lock
		// while a reactor runs, so the copy cannot be read back from inside one.
		copiedInto := ""
		c.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			copiedInto = action.GetNamespace()
			return false, nil, nil
		})
		var deploy *appsv1.Deployment
		c.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
			deploy = action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
			if copiedInto != deploy.Namespace {
				t.Errorf("Secret copied into %q, want %q before the Deployment was created", copiedInto, deploy.Namespace)
			}
			return false, nil, nil
		})
		if _, err := validateClusterAutoscaling(&validators.Context{Ctx: context.Background(), Clientset: c}, "gpu-pool"); err != nil {
			t.Fatalf("validateClusterAutoscaling() error = %v", err)
		}
		if deploy == nil {
			t.Fatal("no Deployment created")
		}
		copied, err := c.CoreV1().Secrets(deploy.Namespace).Get(context.Background(), testPullSecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("copied Secret missing from %s: %v", deploy.Namespace, err)
		}
		if copied.Labels[gpuTestRunLabel] == "" {
			t.Error("copied Secret lacks the run ownership label")
		}
		if want := []corev1.LocalObjectReference{{Name: testPullSecretName}}; !slices.Equal(deploy.Spec.Template.Spec.ImagePullSecrets, want) {
			t.Errorf("ImagePullSecrets = %v, want %v", deploy.Spec.Template.Spec.ImagePullSecrets, want)
		}
	})

	t.Run("missing Secret fails closed", func(t *testing.T) {
		c := k8sfake.NewClientset()
		passClusterAutoscalingFakes(c, "gpu-pool")
		deployments := 0
		c.PrependReactor("create", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			deployments++
			return false, nil, nil
		})
		_, err := validateClusterAutoscaling(&validators.Context{Ctx: context.Background(), Clientset: c}, "gpu-pool")
		errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
		if err == nil || !strings.Contains(err.Error(), testPullSecretName) {
			t.Errorf("error = %v, want it to name the missing Secret", err)
		}
		if deployments != 0 {
			t.Errorf("Deployment creates = %d, want 0 after the copy failed", deployments)
		}
	})
}

func TestPodAwaitingSandbox(t *testing.T) {
	running := creatingPod()
	running.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	finished := creatingPod()
	finished.Status.Phase = corev1.PodSucceeded
	initDone := creatingPod()
	initDone.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"not yet observed", nil, true},
		{"pending, ContainerCreating", creatingPod(), true},
		{"pending with no container status", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, true},
		{"a container is running", running, false},
		{"the pod finished", finished, false},
		{"an init container ran", initDone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podAwaitingSandbox(tt.pod); got != tt.want {
				t.Errorf("podAwaitingSandbox() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandboxCreateFailure(t *testing.T) {
	tests := []struct {
		name  string
		event *corev1.Event
		want  string // substring of the reason; "" means not a terminal failure
	}{
		{
			name: "containerd: no runtime configured for the handler",
			event: &corev1.Event{Reason: eventReasonFailedCreatePodSandBox,
				Message: `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to get sandbox runtime: no runtime for "` + testMissingHandler + `" is configured`},
			want: `no runtime for "` + testMissingHandler + `" is configured`,
		},
		{
			name:  "CRI-O: runtime handler not found",
			event: &corev1.Event{Reason: eventReasonFailedCreatePodSandBox, Message: `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to find runtime handler kata from runtime list map[crun:0x... runc:0x...]`},
			want:  "runtime handler",
		},
		{
			name:  "containerd: shim binary missing on the node",
			event: &corev1.Event{Reason: eventReasonFailedCreatePodSandBox, Message: `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to create containerd task: failed to create shim task: runtime "io.containerd.kata.v2" binary not installed "containerd-shim-kata-v2": file does not exist`},
			want:  "binary not installed",
		},
		{
			name:  "containerd: runc-type handler whose runtime binary is missing",
			event: &corev1.Event{Reason: eventReasonFailedCreatePodSandBox, Message: `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to create containerd task: failed to create shim task: OCI runtime create failed: exec: "kata-runtime": executable file not found in $PATH`},
			want:  "executable file not found",
		},
		{
			name:  "network setup failure is transient",
			event: &corev1.Event{Reason: eventReasonFailedCreatePodSandBox, Message: `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to setup network for sandbox "abc": plugin type="calico" failed (add): error getting ClusterInformation`},
			want:  "",
		},
		{
			name:  "another reason is ignored",
			event: &corev1.Event{Reason: "FailedScheduling", Message: `no runtime for "x" is configured`},
			want:  "",
		},
		{
			name:  "nil event",
			event: nil,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sandboxCreateFailure(tt.event)
			if tt.want == "" {
				if got != "" {
					t.Errorf("sandboxCreateFailure() = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) || !strings.HasPrefix(got, eventReasonFailedCreatePodSandBox+": ") {
				t.Errorf("sandboxCreateFailure() = %q, want %q prefixed with the reason", got, tt.want)
			}
		})
	}
}

func missingHandlerEvent(name, podName string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Reason:         eventReasonFailedCreatePodSandBox,
		Type:           corev1.EventTypeWarning,
		Message:        `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to get sandbox runtime: no runtime for "` + testMissingHandler + `" is configured`,
		InvolvedObject: corev1.ObjectReference{Kind: involvedObjectKindPod, Name: podName, Namespace: "test-ns"},
	}
}

func creatingPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "test-ns"},
		Spec:       corev1.PodSpec{NodeName: "node1", Containers: []corev1.Container{{Name: "probe"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "probe",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
}

func wantSandboxStuck(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the missing-handler sandbox failure to fail the wait")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	for _, want := range []string{"stuck", eventReasonFailedCreatePodSandBox, testMissingHandler} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want substring %q", err, want)
		}
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error = %v — a missing handler must not be classified as the pod timeout", err)
	}
	if elapsed >= sandboxFastFailWait {
		t.Errorf("wait took %v, want a fast failure well under the %v pod timeout", elapsed, sandboxFastFailWait)
	}
}

// TestWaitForTerminalPod_MissingHandlerFailsFast is the #3703 acceptance for
// probe pods: a RuntimeClass whose handler the node does not serve leaves the
// pod Pending with no status change, so the wait must read the kubelet's
// FailedCreatePodSandBox event and fail within seconds, both when the event
// already exists (the List before the watch) and when it arrives while the
// watch is open.
func TestWaitForTerminalPod_MissingHandlerFailsFast(t *testing.T) {
	t.Run("event emitted before the wait", func(t *testing.T) {
		client := k8sfake.NewClientset(creatingPod(), missingHandlerEvent("p.sandbox", "p"))
		start := time.Now()
		_, err := waitForTerminalPodWithin(context.Background(), client, "test-ns", "p", "sandbox probe pod", sandboxFastFailWait)
		wantSandboxStuck(t, err, time.Since(start))
	})

	t.Run("event emitted while the watch is open", func(t *testing.T) {
		client := k8sfake.NewClientset(creatingPod())
		eventWatch := watch.NewFake()
		client.PrependWatchReactor("events", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, eventWatch, nil
		})
		go func() {
			time.Sleep(200 * time.Millisecond)
			eventWatch.Add(missingHandlerEvent("p.sandbox", "p"))
		}()
		start := time.Now()
		_, err := waitForTerminalPodWithin(context.Background(), client, "test-ns", "p", "sandbox probe pod", sandboxFastFailWait)
		wantSandboxStuck(t, err, time.Since(start))
	})

	t.Run("another pod's event is not this pod's failure", func(t *testing.T) {
		// The fake tracker ignores field selectors, so the List returns the
		// sibling's event; production filters server-side and the wait
		// filters again by involved object.
		client := k8sfake.NewClientset(creatingPod(), missingHandlerEvent("q.sandbox", "q"))
		podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		go func() {
			time.Sleep(200 * time.Millisecond)
			done := creatingPod()
			done.Status.Phase = corev1.PodSucceeded
			done.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
			_ = client.Tracker().Update(podsGVR, done, "test-ns")
		}()
		pod, err := waitForTerminalPodWithin(context.Background(), client, "test-ns", "p", "sandbox probe pod", sandboxFastFailWait)
		if err != nil {
			t.Fatalf("waitForTerminalPodWithin() error = %v, want the pod's own completion", err)
		}
		if pod.Status.Phase != corev1.PodSucceeded {
			t.Errorf("phase = %s, want Succeeded", pod.Status.Phase)
		}
	})

	t.Run("a transient network event does not fail the wait", func(t *testing.T) {
		transient := missingHandlerEvent("p.net", "p")
		transient.Message = `Failed to create pod sandbox: rpc error: code = Unknown desc = failed to setup network for sandbox "abc": plugin type="calico" failed`
		client := k8sfake.NewClientset(creatingPod(), transient)
		podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		go func() {
			time.Sleep(200 * time.Millisecond)
			done := creatingPod()
			done.Status.Phase = corev1.PodSucceeded
			done.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
			_ = client.Tracker().Update(podsGVR, done, "test-ns")
		}()
		pod, err := waitForTerminalPodWithin(context.Background(), client, "test-ns", "p", "sandbox probe pod", sandboxFastFailWait)
		if err != nil {
			t.Fatalf("waitForTerminalPodWithin() error = %v, want the pod to complete past the transient event", err)
		}
		if pod.Status.Phase != corev1.PodSucceeded {
			t.Errorf("phase = %s, want Succeeded", pod.Status.Phase)
		}
	})

	t.Run("a stale event is ignored once the pod has started", func(t *testing.T) {
		// The handler was configured after the first sandbox failure: the
		// event remains, the pod is Running. The List must not fail it.
		started := creatingPod()
		started.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		client := k8sfake.NewClientset(started, missingHandlerEvent("p.sandbox", "p"))
		podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		go func() {
			time.Sleep(200 * time.Millisecond)
			done := creatingPod()
			done.Status.Phase = corev1.PodSucceeded
			done.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
			_ = client.Tracker().Update(podsGVR, done, "test-ns")
		}()
		pod, err := waitForTerminalPodWithin(context.Background(), client, "test-ns", "p", "sandbox probe pod", sandboxFastFailWait)
		if err != nil {
			t.Fatalf("waitForTerminalPodWithin() error = %v, want the started pod to run to completion past the stale event", err)
		}
		if pod.Status.Phase != corev1.PodSucceeded {
			t.Errorf("phase = %s, want Succeeded", pod.Status.Phase)
		}
	})

	t.Run("an event watch that cannot be opened leaves the pod watch in charge", func(t *testing.T) {
		client := k8sfake.NewClientset(creatingPod())
		client.PrependWatchReactor("events", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", stderrors.New("RBAC: watch denied"))
		})
		podsGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		go func() {
			time.Sleep(200 * time.Millisecond)
			done := creatingPod()
			done.Status.Phase = corev1.PodFailed
			done.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
			_ = client.Tracker().Update(podsGVR, done, "test-ns")
		}()
		pod, err := waitForTerminalPodWithin(context.Background(), client, "test-ns", "p", "sandbox probe pod", sandboxFastFailWait)
		if err != nil {
			t.Fatalf("waitForTerminalPodWithin() error = %v, want the pod watch to deliver the terminal phase", err)
		}
		if pod.Status.Phase != corev1.PodFailed {
			t.Errorf("phase = %s, want Failed", pod.Status.Phase)
		}
	})
}

// TestObservationOnlyChecksAreRegistered pins the one list of observation-only
// checks (pkg/validator/v1) to this image: every name in it must be a check
// this image runs, so pkg/evidence/cncf cannot refuse credit to a check that
// does not exist or stop refusing it after a rename.
func TestObservationOnlyChecksAreRegistered(t *testing.T) {
	registered := conformanceChecks()
	names := v1.ObservationOnlyChecks()
	if len(names) == 0 {
		t.Fatal("ObservationOnlyChecks() is empty; cluster-autoscaling records observations and must be listed")
	}
	for _, name := range names {
		if _, ok := registered[name]; !ok {
			t.Errorf("observation-only check %q is not registered in conformanceChecks()", name)
		}
	}
	if !v1.IsObservationOnlyCheck("cluster-autoscaling") {
		t.Error(`"cluster-autoscaling" is not observation-only, yet every passing path of CheckClusterAutoscaling ends in passAsObservation`)
	}
}

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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/pkg/validator/labels"
	v1 "aicr.run/pkg/validator/v1"
	"aicr.run/validators"
	"aicr.run/validators/internal/allocmode"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
)

const (
	testSnapshotNamespace = "snapshot"
	testDynamoNamespace   = "dynamo-system"
	testCheckpointName    = "checkpoint-abc123"
	testDynamoVersion     = "1.6.2"
	testAnswer            = "Paris"
)

// dgdFake is a fake cluster with Dynamo and Snapshot installed. It plays the
// parts of the Dynamo operator and the Snapshot node agent: when the DGD is
// created it gets the status dgdStatus gives it (a finished checkpoint and a
// Ready graph by default) and the pods a graph leaves behind (a capture pod that
// ended, a restored worker and a frontend, by default).
type dgdFake struct {
	clientset *k8sfake.Clientset
	dyn       *dynamicfake.FakeDynamicClient

	// dgds are the DGDs as the check submitted them.
	dgds []*unstructured.Unstructured

	// dgdStatus sets the DGD's status when it is created.
	dgdStatus func(dgd *unstructured.Unstructured)
	// pods returns the pods of the graph in the run's namespace.
	pods func(namespace string) []*corev1.Pod
	// snapshotJobs returns the SnapshotJobs Dynamo created for the graph.
	snapshotJobs func(namespace string) []*unstructured.Unstructured
	// answer is what the frontend returns; err fails the ask.
	answer func(call int) (string, error)
	// lingeringPods keeps the graph's pods when the DGD is deleted, as a Dynamo
	// that is slow to tear them down does; by default deleting the DGD removes them.
	lingeringPods bool
}

type dgdFakeOpt func(*dgdFake)

func conditionList(conds ...map[string]any) []any {
	out := make([]any, 0, len(conds))
	for _, c := range conds {
		out = append(out, c)
	}
	return out
}

func cond(t, status, reason, message string) map[string]any {
	return map[string]any{"type": t, "status": status, "reason": reason, "message": message}
}

// readyGraph is the status of a DGD whose checkpoint is taken and that is Ready.
func readyGraph(dgd *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(dgd.Object, "successful", "status", "state")
	_ = unstructured.SetNestedSlice(dgd.Object, conditionList(cond("Ready", "True", "all_resources_are_ready", "")), "status", "conditions")
	_ = unstructured.SetNestedMap(dgd.Object, map[string]any{
		"checkpointName": testCheckpointName, "ready": true,
	}, "status", "checkpoints", dgdWorker)
}

func graphPod(namespace, name, component string, created time.Time, mutate func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{dgdNameLabel: dgdName, dgdComponentLabel: component},
		},
		Spec:   corev1.PodSpec{NodeName: "node1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if mutate != nil {
		mutate(pod)
	}
	return pod
}

func capturePod(namespace string) *corev1.Pod {
	return graphPod(namespace, "checkpoint-capture", dgdWorker, time.Now().Add(-time.Hour), func(p *corev1.Pod) {
		p.Labels[snapshotJobLabel] = "job"
		p.Status.Phase = corev1.PodFailed
		p.Status.Conditions = nil
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: dgdMainContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}},
		}}
	})
}

// endedCapturePod is a capture pod whose container ended with the given exit.
func endedCapturePod(namespace string, exitCode int32, reason, message string) *corev1.Pod {
	pod := capturePod(namespace)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: dgdMainContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Reason: reason, Message: message}},
	}}
	return pod
}

func restoredWorker(namespace string) *corev1.Pod {
	return graphPod(namespace, "worker", dgdWorker, time.Now(), func(p *corev1.Pod) {
		p.Annotations = map[string]string{snapshotRestoreFromAnnotation: testCheckpointName}
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodConditionType(snapshotRestoredCondition), Status: corev1.ConditionTrue, Reason: "RestoreSucceeded",
		})
	})
}

func healthyGraphPods(namespace string) []*corev1.Pod {
	return []*corev1.Pod{
		capturePod(namespace),
		restoredWorker(namespace),
		graphPod(namespace, "frontend", dgdFrontend, time.Now(), nil),
	}
}

func newDGDFake(t *testing.T, opts ...dgdFakeOpt) *dgdFake {
	t.Helper()
	// The runtime image must come from the code under test, not from a developer
	// or CI shell that set the registry override.
	t.Setenv(validatorImageRegistryEnv, "")
	was := []any{dgdPollInterval, dgdCaptureTimeout, dgdRestoreTimeout, dgdAnswerTimeout, dgdTeardownTimeout, dgdFailedGrace}
	asker := askFrontend
	t.Cleanup(func() {
		dgdPollInterval, dgdCaptureTimeout, dgdRestoreTimeout = was[0].(time.Duration), was[1].(time.Duration), was[2].(time.Duration)
		dgdAnswerTimeout, dgdTeardownTimeout, dgdFailedGrace = was[3].(time.Duration), was[4].(time.Duration), was[5].(time.Duration)
		askFrontend = asker
	})
	dgdPollInterval = time.Millisecond
	dgdCaptureTimeout, dgdRestoreTimeout, dgdAnswerTimeout, dgdTeardownTimeout = 2*time.Second, 2*time.Second, 2*time.Second, 2*time.Second

	f := &dgdFake{dgdStatus: readyGraph, pods: healthyGraphPods}
	calls := 0
	f.answer = func(int) (string, error) {
		return `{"choices":[{"message":{"content":"` + testAnswer + `"}}]}`, nil
	}
	askFrontend = func(context.Context, *validators.Context, *dgdRun, *corev1.Pod) (string, error) {
		calls++
		return f.answer(calls)
	}
	for _, opt := range opts {
		opt(f)
	}

	objs := []runtime.Object{
		testNode("node1", withGPUAllocatable("1")),
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: nvidiaRuntimeClassName}, Handler: nvidiaRuntimeClassName},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: snapshotAgentName, Namespace: testSnapshotNamespace},
			Status:     appsv1.DaemonSetStatus{NumberReady: 1, DesiredNumberScheduled: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: snapshotOperatorName, Namespace: testSnapshotNamespace},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: dynamoOperatorName, Namespace: testDynamoNamespace},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		},
	}
	f.clientset = k8sfake.NewClientset(objs...)
	withDRAAPIDiscovery(t, f.clientset)

	crdGVR := schema.GroupVersionResource{Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs}
	f.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			draGVRAt("v1", "deviceclasses"):          "DeviceClassList",
			draGVRAt("v1", "resourceslices"):         "ResourceSliceList",
			draGVRAt("v1", "resourceclaims"):         "ResourceClaimList",
			draGVRAt("v1", "resourceclaimtemplates"): "ResourceClaimTemplateList",
			dgdGVR:                                   "DynamoGraphDeploymentList",
			snapshotJobGVR:                           "SnapshotJobList",
			crdGVR:                                   "CustomResourceDefinitionList",
		})
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": dgdCRDName},
		"status":   map[string]any{"conditions": conditionList(cond("Established", "True", "InitialNamesAccepted", ""))},
	}}
	if err := f.dyn.Tracker().Create(crdGVR, crd, ""); err != nil {
		t.Fatalf("add the DGD CRD: %v", err)
	}

	f.dyn.PrependReactor("create", "dynamographdeployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		dgd, ok := create.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		f.dgds = append(f.dgds, dgd.DeepCopy())
		f.dgdStatus(dgd)
		for _, pod := range f.pods(dgd.GetNamespace()) {
			if err := f.clientset.Tracker().Add(pod); err != nil {
				t.Fatalf("add pod %s: %v", pod.Name, err)
			}
		}
		if f.snapshotJobs != nil {
			for _, job := range f.snapshotJobs(dgd.GetNamespace()) {
				if err := f.dyn.Tracker().Create(snapshotJobGVR, job, dgd.GetNamespace()); err != nil {
					t.Fatalf("add SnapshotJob: %v", err)
				}
			}
		}
		return false, nil, nil // the tracker stores the DGD with its status
	})
	// Dynamo finalizes a deleted DGD by removing its pods.
	f.dyn.PrependReactor("delete", "dynamographdeployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if f.lingeringPods {
			return false, nil, nil
		}
		namespace := action.GetNamespace()
		pods, err := f.clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: dgdNameLabel + "=" + dgdName})
		if err != nil {
			t.Fatalf("list graph pods: %v", err)
		}
		for i := range pods.Items {
			if err := f.clientset.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), namespace, pods.Items[i].Name); err != nil {
				t.Fatalf("remove pod %s: %v", pods.Items[i].Name, err)
			}
		}
		return false, nil, nil
	})
	return f
}

func (f *dgdFake) context() *validators.Context {
	return &validators.Context{
		Ctx:           context.Background(),
		Clientset:     f.clientset,
		DynamicClient: f.dyn,
		ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{
			{Name: snapshotComponentName, Namespace: testSnapshotNamespace},
			{Name: dynamoPlatformComponentName, Namespace: testDynamoNamespace, Version: testDynamoVersion},
		}},
	}
}

func (f *dgdFake) run(t *testing.T) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = CheckDynamoSnapshotRestore(f.context()) })
	return out, err
}

func componentOf(t *testing.T, dgd *unstructured.Unstructured, name string) map[string]any {
	t.Helper()
	components, _, _ := unstructured.NestedSlice(dgd.Object, "spec", "components")
	for _, c := range components {
		if m, ok := c.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	t.Fatalf("DGD has no component %q", name)
	return nil
}

func podSpecOfComponent(t *testing.T, component map[string]any) corev1.PodSpec {
	t.Helper()
	raw, found, err := unstructured.NestedMap(component, "podTemplate", "spec")
	if err != nil || !found {
		t.Fatalf("component has no podTemplate.spec: found=%t err=%v", found, err)
	}
	var spec corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		t.Fatalf("podTemplate.spec does not decode as a PodSpec: %v", err)
	}
	return spec
}

func TestCheckDynamoSnapshotRestore_SkipsUnlessBothComponentsAreDeclared(t *testing.T) {
	for _, missing := range []string{snapshotComponentName, dynamoPlatformComponentName} {
		t.Run("without "+missing, func(t *testing.T) {
			f := newDGDFake(t)
			ctx := f.context()
			var refs []recipe.ComponentRef
			for _, ref := range ctx.ValidationInput.ComponentRefs {
				if ref.Name != missing {
					refs = append(refs, ref)
				}
			}
			ctx.ValidationInput = &v1.ValidationInput{ComponentRefs: refs}

			err := CheckDynamoSnapshotRestore(ctx)
			if !validators.IsSkip(err) {
				t.Fatalf("CheckDynamoSnapshotRestore() = %v, want a skip", err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("skip reason %q should name the missing component %s", err, missing)
			}
			if len(f.dgds) != 0 {
				t.Errorf("a skipped check created %d DGD(s), want none", len(f.dgds))
			}
			requireNoLeakedNamespace(t, f.clientset)
		})
	}
}

func TestCheckDynamoSnapshotRestore_PassesThroughDynamo(t *testing.T) {
	f := newDGDFake(t)
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	if len(f.dgds) != 1 {
		t.Fatalf("created %d DGD(s), want 1", len(f.dgds))
	}
	dgd := f.dgds[0]

	t.Run("DGD as submitted", func(t *testing.T) {
		if got := dgd.GetAnnotations()[dgdGroveAnnotation]; got != "false" {
			t.Errorf("%s = %q, want false: the check must not depend on how the cluster's Dynamo orchestrates workers", dgdGroveAnnotation, got)
		}
		if backend, _, _ := unstructured.NestedString(dgd.Object, "spec", "backendFramework"); backend != "vllm" {
			t.Errorf("backendFramework = %q, want vllm", backend)
		}
		worker := componentOf(t, dgd, dgdWorker)
		if worker["type"] != "worker" {
			t.Errorf("worker type = %v, want worker", worker["type"])
		}
		checkpoint, _, _ := unstructured.NestedMap(worker, "experimental", "checkpoint")
		if checkpoint["enabled"] != true || checkpoint["startupPolicy"] != "WaitForCheckpoint" {
			t.Errorf("experimental.checkpoint = %v, want enabled with startupPolicy WaitForCheckpoint", checkpoint)
		}
		spec := podSpecOfComponent(t, worker)
		c := spec.Containers[0]
		if want := "nvcr.io/nvidia/ai-dynamo/vllm-runtime:" + testDynamoVersion; c.Image != want {
			t.Errorf("worker image = %q, want %q (the recipe's dynamo-platform version)", c.Image, want)
		}
		if strings.Join(c.Command, " ") != "python3 -m dynamo.vllm" {
			t.Errorf("worker command = %v, want python3 -m dynamo.vllm unwrapped: restore admission rejects a shell wrapper", c.Command)
		}
		if !strings.Contains(strings.Join(c.Args, " "), "--model "+dgdModel) {
			t.Errorf("worker args = %v, want the model %s", c.Args, dgdModel)
		}
		if got := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; got.Value() != 1 {
			t.Errorf("worker GPU limit = %s, want 1", got.String())
		}
		if spec.RuntimeClassName == nil || *spec.RuntimeClassName != nvidiaRuntimeClassName {
			t.Errorf("worker runtimeClassName = %v, want %q when the cluster has it", spec.RuntimeClassName, nvidiaRuntimeClassName)
		}
		requirePodSpecOnNode(t, spec, "node1")
		if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != corev1.TolerationOpExists {
			t.Errorf("worker tolerations = %v, want the tolerate-all default", spec.Tolerations)
		}
		frontend := componentOf(t, dgd, dgdFrontend)
		if frontend["type"] != "frontend" {
			t.Errorf("frontend type = %v, want frontend", frontend["type"])
		}
		if got := podSpecOfComponent(t, frontend).Containers[0].Image; got != c.Image {
			t.Errorf("frontend image = %q, want the same runtime image as the worker (%q)", got, c.Image)
		}
		if _, hasGPU := podSpecOfComponent(t, frontend).Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
			t.Error("the frontend must not request a GPU")
		}
	})

	for _, want := range []string{
		"--- DynamoGraphDeployment after capture ---",
		"Checkpoint: " + testCheckpointName + " ready=true",
		"--- Checkpoint ---",
		"--- Restored worker ---",
		`restore-from: "` + testCheckpointName + `"`,
		"Restored: True (RestoreSucceeded)",
		"--- Frontend answer ---",
		testAnswer,
		"verdict: PASS",
		"--- Delete DynamoGraphDeployment ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	requireNoLeakedNamespace(t, f.clientset)
	// Only NotFound proves the delete: a Forbidden or a transport error would pass a
	// bare "an error came back" check while the DGD still exists.
	if _, getErr := f.dyn.Resource(dgdGVR).Namespace(dgd.GetNamespace()).Get(context.Background(), dgdName, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
		t.Errorf("reading the DGD after the check = %v, want NotFound: it must be deleted before the namespace cleanup", getErr)
	}
}

// requirePodSpecOnNode checks the spec is confined to exactly one node by node
// affinity.
func requirePodSpecOnNode(t *testing.T, spec corev1.PodSpec, node string) {
	t.Helper()
	pod := &corev1.Pod{Spec: spec}
	requireNodeNameAffinity(t, pod, []string{node})
}

func TestCheckDynamoSnapshotRestore_RuntimeImageTag(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		registry string
		want     string
		wantErr  string
	}{
		{name: "the recipe's version", version: "1.6.2", want: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.6.2"},
		{name: "a v prefix is dropped", version: "v1.7.0", want: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.7.0"},
		{name: "no version falls back to the default", version: "", want: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:" + strings.TrimPrefix(defaults.DynamoRuntimeImageTag, "v")},
		{name: "registry override", version: "1.6.2", registry: "registry.example.com/mirror", want: "registry.example.com/mirror/ai-dynamo/vllm-runtime:1.6.2"},
		{name: "not a version", version: "latest", wantErr: "not usable as a Dynamo runtime image tag"},
		{name: "build metadata", version: "1.6.2+build", wantErr: "not usable as a Dynamo runtime image tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t)
			t.Setenv(validatorImageRegistryEnv, tt.registry)
			ctx := f.context()
			ctx.ValidationInput.ComponentRefs[1].Version = tt.version

			var err error
			out := captureStdout(t, func() { err = CheckDynamoSnapshotRestore(ctx) })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("CheckDynamoSnapshotRestore() = %v, want an error containing %q", err, tt.wantErr)
				}
				if len(f.dgds) != 0 {
					t.Error("a bad version must fail before a DGD is created")
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
			}
			worker := podSpecOfComponent(t, componentOf(t, f.dgds[0], dgdWorker))
			if got := worker.Containers[0].Image; got != tt.want {
				t.Errorf("worker image = %q, want %q", got, tt.want)
			}
			frontend := podSpecOfComponent(t, componentOf(t, f.dgds[0], dgdFrontend))
			if got := frontend.Containers[0].Image; got != tt.want {
				t.Errorf("frontend image = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckDynamoSnapshotRestore_FailsWhenAnInstallIsMissing(t *testing.T) {
	tests := []struct {
		name    string
		break_  func(t *testing.T, f *dgdFake)
		wantErr string
	}{
		{
			name: "Dynamo operator missing",
			break_: func(t *testing.T, f *dgdFake) {
				mustDelete(t, f.clientset.AppsV1().Deployments(testDynamoNamespace).Delete(context.Background(), dynamoOperatorName, metav1.DeleteOptions{}))
			},
			wantErr: dynamoOperatorName,
		},
		{
			name: "DGD API not served",
			break_: func(t *testing.T, f *dgdFake) {
				crdGVR := schema.GroupVersionResource{Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs}
				mustDelete(t, f.dyn.Resource(crdGVR).Delete(context.Background(), dgdCRDName, metav1.DeleteOptions{}))
			},
			wantErr: "DynamoGraphDeployment API is not served",
		},
		{
			name: "Snapshot operator missing",
			break_: func(t *testing.T, f *dgdFake) {
				mustDelete(t, f.clientset.AppsV1().Deployments(testSnapshotNamespace).Delete(context.Background(), snapshotOperatorName, metav1.DeleteOptions{}))
			},
			wantErr: snapshotOperatorName,
		},
		{
			name: "Snapshot agent not ready",
			break_: func(t *testing.T, f *dgdFake) {
				ds, err := f.clientset.AppsV1().DaemonSets(testSnapshotNamespace).Get(context.Background(), snapshotAgentName, metav1.GetOptions{})
				mustDelete(t, err)
				ds.Status.NumberReady = 0
				_, err = f.clientset.AppsV1().DaemonSets(testSnapshotNamespace).Update(context.Background(), ds, metav1.UpdateOptions{})
				mustDelete(t, err)
			},
			wantErr: snapshotAgentName,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t)
			tt.break_(t, f)
			_, err := f.run(t)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CheckDynamoSnapshotRestore() = %v, want an error containing %q", err, tt.wantErr)
			}
			if validators.IsSkip(err) {
				t.Errorf("a missing install must fail, not skip: %v", err)
			}
			if len(f.dgds) != 0 {
				t.Error("a missing install must fail before any DGD is created")
			}
			requireNoLeakedNamespace(t, f.clientset)
		})
	}
}

func mustDelete(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
}

func TestCheckDynamoSnapshotRestore_CaptureFailures(t *testing.T) {
	pending := func(dgd *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(dgd.Object, "pending", "status", "state")
		_ = unstructured.SetNestedSlice(dgd.Object, conditionList(cond("Ready", "False", "waiting_for_checkpoint", "")), "status", "conditions")
		_ = unstructured.SetNestedMap(dgd.Object, map[string]any{"checkpointName": testCheckpointName}, "status", "checkpoints", dgdWorker)
	}
	snapshotJob := func(namespace string, failed bool) []*unstructured.Unstructured {
		status := "False"
		if failed {
			status = "True"
		}
		return []*unstructured.Unstructured{{Object: map[string]any{
			"apiVersion": "nvidia.com/v1alpha1", "kind": "SnapshotJob",
			"metadata": map[string]any{"name": testCheckpointName, "namespace": namespace, "labels": map[string]any{dgdNameLabel: dgdName}},
			"status": map[string]any{"conditions": conditionList(
				cond("Failed", status, "CheckpointFailed", "while dumping CUDA state: cuda-checkpoint exited 1")),
			},
		}}}
	}
	waiting := func(reason string) func(string) []*corev1.Pod {
		return func(namespace string) []*corev1.Pod {
			return []*corev1.Pod{graphPod(namespace, "capture", dgdWorker, time.Now(), func(p *corev1.Pod) {
				p.Labels[snapshotJobLabel] = "job"
				p.Status.Phase = corev1.PodPending
				p.Status.Conditions = nil
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: dgdMainContainer, Image: "img",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "pull denied"}},
				}}
			})}
		}
	}
	tests := []struct {
		name    string
		opts    []dgdFakeOpt
		timeout bool
		wantErr []string
	}{
		{
			name: "SnapshotJob failed",
			opts: []dgdFakeOpt{
				func(f *dgdFake) {
					f.dgdStatus = pending
					f.pods = func(string) []*corev1.Pod { return nil }
					f.snapshotJobs = func(ns string) []*unstructured.Unstructured { return snapshotJob(ns, true) }
				},
			},
			wantErr: []string{"checkpoint did not complete", "SnapshotJob " + testCheckpointName + " failed (CheckpointFailed)", "cuda-checkpoint exited 1"},
		},
		{
			name:    "capture pod cannot pull its image",
			opts:    []dgdFakeOpt{func(f *dgdFake) { f.dgdStatus = pending; f.pods = waiting("ImagePullBackOff") }},
			wantErr: []string{"checkpoint did not complete", "stuck: ImagePullBackOff", "pull denied"},
		},
		{
			name: "DGD stays failed",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				dgdFailedGrace = 0
				f.dgdStatus = func(dgd *unstructured.Unstructured) {
					pending(dgd)
					_ = unstructured.SetNestedField(dgd.Object, "failed", "status", "state")
				}
				f.pods = func(string) []*corev1.Pod { return nil }
			}},
			wantErr: []string{"the DynamoGraphDeployment stayed failed"},
		},
		{
			name: "capture pod crashed",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.dgdStatus = pending
				f.pods = func(namespace string) []*corev1.Pod {
					return []*corev1.Pod{endedCapturePod(namespace, 1, "Error", "engine failed to start")}
				}
			}},
			wantErr: []string{"failed:", "main exited 1 (Error): engine failed to start"},
		},
		{
			name: "checkpoint never ready",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.dgdStatus = pending
				f.pods = func(string) []*corev1.Pod { return nil }
			}},
			timeout: true,
			wantErr: []string{"did not report the worker's checkpoint ready", "Ready: False (waiting_for_checkpoint)", "no pods were created"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t, tt.opts...)
			if tt.timeout {
				dgdCaptureTimeout = 50 * time.Millisecond
			}
			out, err := f.run(t)
			if err == nil {
				t.Fatalf("CheckDynamoSnapshotRestore() = nil, want an error\n%s", out)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if tt.timeout {
				errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
			}
			requireNoLeakedNamespace(t, f.clientset)
		})
	}
}

func TestCheckDynamoSnapshotRestore_FailedSnapshotJobEndsTheWaitAtOnce(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = func(dgd *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(dgd.Object, "pending", "status", "state")
		}
		f.pods = func(string) []*corev1.Pod { return nil }
		f.snapshotJobs = func(ns string) []*unstructured.Unstructured {
			return []*unstructured.Unstructured{{Object: map[string]any{
				"apiVersion": "nvidia.com/v1alpha1", "kind": "SnapshotJob",
				"metadata": map[string]any{"name": "job", "namespace": ns, "labels": map[string]any{dgdNameLabel: dgdName}},
				"status":   map[string]any{"conditions": conditionList(cond("Failed", "True", "CheckpointFailed", "boom"))},
			}}}
		}
	})
	// A generous timeout: the failed SnapshotJob must end the wait, not the clock.
	dgdCaptureTimeout = time.Minute

	start := time.Now()
	_, err := f.run(t)
	if err == nil {
		t.Fatal("CheckDynamoSnapshotRestore() = nil, want an error")
	}
	if time.Since(start) > 30*time.Second {
		t.Error("a failed SnapshotJob must end the capture wait at once")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

func TestCheckDynamoSnapshotRestore_RestoreFailures(t *testing.T) {
	workerWith := func(mutate func(*corev1.Pod)) func(string) []*corev1.Pod {
		return func(namespace string) []*corev1.Pod {
			worker := restoredWorker(namespace)
			mutate(worker)
			return []*corev1.Pod{capturePod(namespace), worker, graphPod(namespace, "frontend", dgdFrontend, time.Now(), nil)}
		}
	}
	restoredCond := func(status corev1.ConditionStatus, reason, message string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				{Type: corev1.PodConditionType(snapshotRestoredCondition), Status: status, Reason: reason, Message: message},
			}
		}
	}
	notReadyDGD := func(f *dgdFake) {
		f.dgdStatus = func(dgd *unstructured.Unstructured) {
			readyGraph(dgd)
			_ = unstructured.SetNestedSlice(dgd.Object, conditionList(cond("Ready", "False", "some_resources_are_not_ready", "")), "status", "conditions")
		}
	}
	tests := []struct {
		name    string
		opts    []dgdFakeOpt
		timeout bool
		wantErr []string
	}{
		{
			name: "restore refused by the node agent",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = workerWith(restoredCond(corev1.ConditionFalse, "RestoreIncompatible", "while checking compatibility: kernel mismatch"))
			}},
			wantErr: []string{"did not restore from checkpoint " + testCheckpointName, "RestoreIncompatible", "while checking compatibility: kernel mismatch"},
		},
		{
			name: "restore failed",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = workerWith(restoredCond(corev1.ConditionFalse, "RestoreFailed", "while restoring CUDA state: boom"))
			}},
			wantErr: []string{"RestoreFailed", "while restoring CUDA state: boom"},
		},
		{
			name: "worker ended",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = workerWith(func(p *corev1.Pod) {
					p.Status.Phase = corev1.PodFailed
					p.Status.ContainerStatuses = []corev1.ContainerStatus{{
						Name: dgdMainContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 112, Reason: "Error"}},
					}}
				})
			}},
			wantErr: []string{"the worker pod ended", "main exited 112", "restore proxy exits 112"},
		},
		{
			name:    "worker started cold: no restore-from annotation",
			opts:    []dgdFakeOpt{func(f *dgdFake) { f.pods = workerWith(func(p *corev1.Pod) { p.Annotations = nil }) }},
			timeout: true,
			wantErr: []string{"was not Ready with a restored worker", `restore-from: ""`, `the checkpoint is "` + testCheckpointName + `"`},
		},
		{
			name: "worker restored from another checkpoint",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = workerWith(func(p *corev1.Pod) { p.Annotations[snapshotRestoreFromAnnotation] = "checkpoint-old" })
			}},
			timeout: true,
			wantErr: []string{"was not Ready with a restored worker", `restore-from: "checkpoint-old"`},
		},
		{
			name: "worker Ready but its Restored condition is not True",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = workerWith(func(p *corev1.Pod) {
					p.Status.Conditions = []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
						{Type: corev1.PodConditionType(snapshotRestoredCondition), Status: corev1.ConditionFalse, Reason: "RestoreInProgress"},
					}
				})
			}},
			timeout: true,
			wantErr: []string{"was not Ready with a restored worker", "Restored: False (RestoreInProgress)"},
		},
		{
			name: "worker Ready but never marked Restored",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = workerWith(func(p *corev1.Pod) {
					p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				})
			}},
			timeout: true,
			wantErr: []string{"was not Ready with a restored worker"},
		},
		{
			name:    "graph never Ready",
			opts:    []dgdFakeOpt{notReadyDGD},
			timeout: true,
			wantErr: []string{"was not Ready with a restored worker", "Ready: False (some_resources_are_not_ready)"},
		},
		{
			name: "no worker pod",
			opts: []dgdFakeOpt{func(f *dgdFake) {
				f.pods = func(namespace string) []*corev1.Pod { return []*corev1.Pod{capturePod(namespace)} }
			}},
			timeout: true,
			wantErr: []string{"no worker pod was created", "(capture)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t, tt.opts...)
			if tt.timeout {
				dgdRestoreTimeout = 50 * time.Millisecond
			}
			out, err := f.run(t)
			if err == nil {
				t.Fatalf("CheckDynamoSnapshotRestore() = nil, want an error\n%s", out)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if tt.timeout {
				errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
			}
			if !strings.Contains(out, "--- Restored worker ---") {
				t.Errorf("stdout must record the worker however the restore ended:\n%s", out)
			}
			requireNoLeakedNamespace(t, f.clientset)
		})
	}
}

func TestCheckDynamoSnapshotRestore_AnswerFailures(t *testing.T) {
	body := func(content string) string { return `{"choices":[{"message":{"content":"` + content + `"}}]}` }
	tests := []struct {
		name    string
		answer  func(call int) (string, error)
		wantErr []string
		fast    bool // fails on the first answer rather than at the timeout
	}{
		{
			name:    "wrong answer",
			answer:  func(int) (string, error) { return body("The capital is London."), nil },
			wantErr: []string{"want \"paris\"", "London"},
			fast:    true,
		},
		{
			name:    "mentions Paris without giving it as the answer",
			answer:  func(int) (string, error) { return body("Paris is not the capital of France."), nil },
			wantErr: []string{"want \"paris\"", "Paris is not the capital"},
			fast:    true,
		},
		{
			name:    "frontend never answers",
			answer:  func(int) (string, error) { return "", stderrors.New("HTTP 404: model not found") },
			wantErr: []string{"gave no answer", "HTTP 404: model not found"},
		},
		{
			name:    "response is not a chat completion",
			answer:  func(int) (string, error) { return "<html>bad gateway</html>", nil },
			wantErr: []string{"gave no answer", "not a chat completion"},
		},
		{
			name:    "empty content",
			answer:  func(int) (string, error) { return body(""), nil },
			wantErr: []string{"gave no answer", "has no content"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t, func(f *dgdFake) { f.answer = tt.answer })
			dgdAnswerTimeout = 100 * time.Millisecond
			if tt.fast {
				dgdAnswerTimeout = time.Minute
			}
			start := time.Now()
			out, err := f.run(t)
			if err == nil {
				t.Fatalf("CheckDynamoSnapshotRestore() = nil, want an error\n%s", out)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if tt.fast && time.Since(start) > 30*time.Second {
				t.Error("a wrong answer must end the wait at once")
			}
			requireNoLeakedNamespace(t, f.clientset)
		})
	}
}

func TestCheckDynamoSnapshotRestore_RetriesUntilTheFrontendHasTheModel(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.answer = func(call int) (string, error) {
			if call < 3 {
				return "", stderrors.New("HTTP 404: model not found")
			}
			return `{"choices":[{"message":{"content":"Paris."}}]}`, nil
		}
	})
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
}

func TestCheckDynamoSnapshotRestore_TeardownRunsBeforeTheNamespaceCleanup(t *testing.T) {
	f := newDGDFake(t)
	var order []string
	f.dyn.PrependReactor("delete", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "dgd")
		return false, nil, nil
	})
	var podsAtNamespaceDelete = -1
	f.clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "namespace")
		if podsAtNamespaceDelete < 0 {
			// The tracker, not the clientset: a clientset call inside its own reactor
			// would wait on the lock the reactor holds.
			obj, err := f.clientset.Tracker().List(corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithKind("Pod"), "")
			mustDelete(t, err)
			pods, ok := obj.(*corev1.PodList)
			if !ok {
				t.Fatalf("tracker listed %T, want a PodList", obj)
			}
			podsAtNamespaceDelete = len(pods.Items)
		}
		return false, nil, nil
	})
	if out, err := f.run(t); err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	if podsAtNamespaceDelete != 0 {
		t.Errorf("%d graph pods were left when the namespace was deleted, want the teardown to see them removed first", podsAtNamespaceDelete)
	}
	if len(order) < 2 || order[0] != "dgd" || order[1] != "namespace" {
		t.Errorf("deletes happened in order %v, want the DGD first, so Dynamo frees the GPU before the namespace goes", order)
	}
}

func TestDeleteDGDAndWait(t *testing.T) {
	t.Run("a DGD that never goes away is reported, not failed", func(t *testing.T) {
		f := newDGDFake(t)
		dgdTeardownTimeout = 50 * time.Millisecond
		ctx := f.context()
		r := &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, createAttempted: true}
		dgd, err := buildDGD(&dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img"}, nil, []string{"node1"}, nil)
		mustDelete(t, err)
		_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
		mustDelete(t, err)
		// A finalizer-like delete that does nothing: the DGD stays.
		f.dyn.PrependReactor("delete", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, nil
		})
		got := deleteDGDAndWait(ctx, f.dyn, r)
		if !strings.Contains(got, "still being removed") {
			t.Errorf("deleteDGDAndWait() = %q, want it to say the DGD was still being removed", got)
		}
	})
	t.Run("a pod that outlives the DGD keeps the teardown waiting", func(t *testing.T) {
		f := newDGDFake(t, func(f *dgdFake) { f.pods = func(string) []*corev1.Pod { return nil }; f.lingeringPods = true })
		dgdTeardownTimeout = 50 * time.Millisecond
		ctx := f.context()
		r := &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img", createAttempted: true}
		dgd, err := buildDGD(r, nil, []string{"node1"}, nil)
		mustDelete(t, err)
		_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
		mustDelete(t, err)
		mustDelete(t, f.clientset.Tracker().Add(graphPod("ns", "worker", dgdWorker, time.Now(), nil)))

		got := deleteDGDAndWait(ctx, f.dyn, r)
		if !strings.Contains(got, "still being removed") {
			t.Errorf("deleteDGDAndWait() = %q, want it to wait for the worker pod", got)
		}
		// Once the pod is gone the same teardown reports success.
		mustDelete(t, f.clientset.CoreV1().Pods("ns").Delete(context.Background(), "worker", metav1.DeleteOptions{}))
		dgdTeardownTimeout = 2 * time.Second
		if got = deleteDGDAndWait(ctx, f.dyn, r); !strings.Contains(got, "no DynamoGraphDeployment") && !strings.Contains(got, "deleted") {
			t.Errorf("deleteDGDAndWait() = %q after the pod ended", got)
		}
	})
	t.Run("no DGD", func(t *testing.T) {
		f := newDGDFake(t)
		got := deleteDGDAndWait(f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns"}, createAttempted: true})
		if !strings.Contains(got, "no DynamoGraphDeployment") {
			t.Errorf("deleteDGDAndWait() = %q", got)
		}
	})
}

func TestDGDWorkBudgetFor(t *testing.T) {
	t.Run("no deadline uses the fallback", func(t *testing.T) {
		got, err := dgdWorkBudgetFor(context.Background())
		if err != nil || got != dgdWorkBudget {
			t.Errorf("dgdWorkBudgetFor() = %s, %v, want %s", got, err, dgdWorkBudget)
		}
	})
	t.Run("the catalog timeout leaves a work budget", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
		defer cancel()
		got, err := dgdWorkBudgetFor(ctx)
		if err != nil {
			t.Fatalf("dgdWorkBudgetFor() error = %v", err)
		}
		// 35m minus the namespace cleanup reserve (90s) and the DGD teardown (3m).
		if got < 29*time.Minute || got > 31*time.Minute {
			t.Errorf("dgdWorkBudgetFor() = %s, want about 30m", got)
		}
	})
	t.Run("a short deadline fails before anything is created", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, err := dgdWorkBudgetFor(ctx)
		if err == nil || !strings.Contains(err.Error(), "check timeout too short") {
			t.Fatalf("dgdWorkBudgetFor() = %v, want a too-short error", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	})
}

func TestRequireDevicePluginWorker(t *testing.T) {
	usable := &allocmode.Mode{DevicePluginUsable: true, DevicePluginNodes: []string{"node1"}}
	tests := []struct {
		name    string
		policy  string
		mode    *allocmode.Mode
		wantErr string
	}{
		{name: "device plugin node", policy: v1.GPUAllocationPolicyUnspecified, mode: usable},
		{name: "explicit device plugin policy", policy: v1.GPUAllocationPolicyDevicePluginExtendedResource, mode: usable},
		{name: "DRA policy", policy: v1.GPUAllocationPolicyDRAResourceClaim, mode: usable, wantErr: "allocated through the device plugin"},
		{
			name: "no device plugin node", policy: v1.GPUAllocationPolicyUnspecified,
			mode:    &allocmode.Mode{DevicePluginDetail: "no node advertises nvidia.com/gpu"},
			wantErr: "no node advertises nvidia.com/gpu",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireDevicePluginWorker(tt.policy, tt.mode)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("requireDevicePluginWorker() error = %v", err)
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("requireDevicePluginWorker() = %v, want an error containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
			}
		})
	}
}

func TestSnapshotRestoreFailure(t *testing.T) {
	restored := func(status corev1.ConditionStatus, reason string) corev1.PodCondition {
		return corev1.PodCondition{Type: corev1.PodConditionType(snapshotRestoredCondition), Status: status, Reason: reason, Message: "msg"}
	}
	running := func(conds ...corev1.PodCondition) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: conds}}
	}
	tests := []struct {
		name string
		pod  corev1.Pod
		want string // substring of the failure; "" means the restore may still succeed
	}{
		{name: "pending", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}},
		{name: "in progress", pod: running(restored(corev1.ConditionFalse, "RestoreInProgress"))},
		{name: "succeeded", pod: running(restored(corev1.ConditionTrue, "RestoreSucceeded"))},
		{name: "failed", pod: running(restored(corev1.ConditionFalse, "RestoreFailed")), want: "RestoreFailed: msg"},
		{name: "incompatible", pod: running(restored(corev1.ConditionFalse, "RestoreIncompatible")), want: "RestoreIncompatible: msg"},
		{name: "partially succeeded", pod: running(restored(corev1.ConditionFalse, "RestorePartiallySucceeded")), want: "RestorePartiallySucceeded: msg"},
		{name: "pod ended", pod: corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, want: "the worker pod ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snapshotRestoreFailure(&tt.pod)
			if tt.want == "" && got != "" {
				t.Errorf("snapshotRestoreFailure() = %q, want no failure", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("snapshotRestoreFailure() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestWorkerAndFrontendPodSelection(t *testing.T) {
	now := time.Now()
	older := graphPod("ns", "worker-old", dgdWorker, now.Add(-time.Minute), nil)
	newer := graphPod("ns", "worker-new", dgdWorker, now, nil)
	capture := capturePod("ns")
	capture.CreationTimestamp = metav1.NewTime(now.Add(time.Hour)) // newest of all, but not a serving worker
	notReadyFrontend := graphPod("ns", "frontend-a", dgdFrontend, now, func(p *corev1.Pod) { p.Status.Conditions = nil })
	readyFrontend := graphPod("ns", "frontend-b", dgdFrontend, now, nil)

	pods := []corev1.Pod{*older, *capture, *newer, *notReadyFrontend, *readyFrontend}
	if got := workerPod(pods); got == nil || got.Name != "worker-new" {
		t.Errorf("workerPod() = %v, want the newest non-capture worker worker-new", got)
	}
	if got := frontendPod(pods); got == nil || got.Name != "frontend-b" {
		t.Errorf("frontendPod() = %v, want the Ready frontend frontend-b", got)
	}
	if got := workerPod([]corev1.Pod{*capture}); got != nil {
		t.Errorf("workerPod() = %v with only a capture pod, want nil", got)
	}
	if got := frontendPod([]corev1.Pod{*notReadyFrontend}); got != nil {
		t.Errorf("frontendPod() = %v with no Ready frontend, want nil", got)
	}
}

func TestRestoredWorkerReady(t *testing.T) {
	good := restoredWorker("ns")
	if !restoredWorkerReady(good, testCheckpointName) {
		t.Error("restoredWorkerReady() = false for a Ready, restored worker restored from the checkpoint")
	}
	if restoredWorkerReady(good, "other") {
		t.Error("restoredWorkerReady() = true for a worker restored from a different checkpoint")
	}
	if restoredWorkerReady(nil, testCheckpointName) {
		t.Error("restoredWorkerReady() = true for no pod")
	}
}

func TestCompletionContent(t *testing.T) {
	if got, err := completionContent(`{"choices":[{"message":{"content":" Paris "}}]}`); err != nil || got != " Paris " {
		t.Errorf("completionContent() = %q, %v", got, err)
	}
	for _, bad := range []string{"", "not json", `{"choices":[]}`, `{"choices":[{"message":{"content":"  "}}]}`} {
		if _, err := completionContent(bad); err == nil {
			t.Errorf("completionContent(%q) = nil error, want one", bad)
		}
	}
}

func TestDGDStatusParsing(t *testing.T) {
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	readyGraph(dgd)
	if cp := dgdCheckpoint(dgd); cp.name != testCheckpointName || !cp.ready {
		t.Errorf("dgdCheckpoint() = %+v", cp)
	}
	if !dgdIsReady(dgd) {
		t.Error("dgdIsReady() = false for a Ready graph")
	}
	if cp := dgdCheckpoint(nil); cp.name != "" || cp.ready {
		t.Errorf("dgdCheckpoint(nil) = %+v, want zero", cp)
	}
	if dgdIsReady(&unstructured.Unstructured{Object: map[string]any{}}) {
		t.Error("dgdIsReady() = true for a DGD with no status")
	}
	if !strings.Contains(describeDGD(dgd), "Checkpoint: "+testCheckpointName+" ready=true") {
		t.Errorf("describeDGD() = %q", describeDGD(dgd))
	}
}

// TestDynamoProtocolLiterals pins the strings Dynamo and Snapshot define. A
// change to one is a protocol change and must not slip through as a rename: the
// code and the tests would otherwise follow each other.
// TestFrontendScript runs the script against a local server with a dead proxy in
// the environment, as a pod may set, so a syntax error or a request that goes
// through the proxy fails here and not on a cluster.
func TestFrontendScript(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Paris"}}]}`))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(), python, "-I", "-c", frontendScript, dgdModel, dgdPrompt, u.Port())
	cmd.Env = append(os.Environ(), "HTTP_PROXY=http://127.0.0.1:1", "http_proxy=http://127.0.0.1:1")
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("frontend script failed: %v\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "Paris") {
		t.Errorf("frontend script printed %q, want the completion", out)
	}
}

func TestComponentNamespace(t *testing.T) {
	ctx := &validators.Context{ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "snapshot", Namespace: "custom"}}}}
	if got := componentNamespace(ctx, "snapshot", "fallback"); got != "custom" {
		t.Errorf("componentNamespace() = %q, want the componentRef's", got)
	}
	if got := componentNamespace(ctx, "dynamo-platform", "fallback"); got != "fallback" {
		t.Errorf("componentNamespace() = %q for an undeclared component, want the fallback", got)
	}
	ctx.ValidationInput.ComponentRefs[0].Namespace = ""
	if got := componentNamespace(ctx, "snapshot", "fallback"); got != "fallback" {
		t.Errorf("componentNamespace() = %q for a ref with no namespace, want the fallback", got)
	}
	if got := componentNamespace(&validators.Context{}, "snapshot", "fallback"); got != "fallback" {
		t.Errorf("componentNamespace() = %q without a validation input, want the fallback", got)
	}
}

func TestCheckDynamoSnapshotRestore_NoClusterIsInvalid(t *testing.T) {
	err := CheckDynamoSnapshotRestore(&validators.Context{Ctx: context.Background()})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestCheckDynamoSnapshotRestore_HonorsTolerations(t *testing.T) {
	f := newDGDFake(t)
	ctx := f.context()
	ctx.Tolerations = []corev1.Toleration{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	var err error
	out := captureStdout(t, func() { err = CheckDynamoSnapshotRestore(ctx) })
	if err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	for _, name := range []string{dgdWorker, dgdFrontend} {
		tolerations := podSpecOfComponent(t, componentOf(t, f.dgds[0], name)).Tolerations
		if len(tolerations) != 1 || tolerations[0].Key != "dedicated" {
			t.Errorf("%s tolerations = %v, want the context tolerations", name, tolerations)
		}
	}
}

func TestCheckDynamoSnapshotRestore_WithoutNvidiaRuntimeClass(t *testing.T) {
	f := newDGDFake(t)
	mustDelete(t, f.clientset.NodeV1().RuntimeClasses().Delete(context.Background(), nvidiaRuntimeClassName, metav1.DeleteOptions{}))
	if out, err := f.run(t); err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	if spec := podSpecOfComponent(t, componentOf(t, f.dgds[0], dgdWorker)); spec.RuntimeClassName != nil {
		t.Errorf("worker runtimeClassName = %q, want unset when the cluster has no %s class", *spec.RuntimeClassName, nvidiaRuntimeClassName)
	}
}

// TestDGDFailure_CaptureKilledByTheCheckpointIsNotAFailure pins the race a live
// run found: a capture pod exits 137 (Error) as the checkpoint dumps its process,
// seconds before Dynamo reports the checkpoint ready, so it must not fail the
// check while the DGD is still waiting.
func TestDGDFailure_CaptureKilledByTheCheckpointIsNotAFailure(t *testing.T) {
	waitingDGD := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedField(waitingDGD.Object, "pending", "status", "state")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want string // "" means no failure
	}{
		{name: "killed by the checkpoint", pod: endedCapturePod("ns", 137, "Error", "")},
		{name: "engine crashed", pod: endedCapturePod("ns", 1, "Error", ""), want: "failed:"},
		{name: "out of memory", pod: endedCapturePod("ns", 137, "OOMKilled", ""), want: "OOMKilled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t)
			if err := f.clientset.Tracker().Add(tt.pod); err != nil {
				t.Fatalf("add pod: %v", err)
			}
			r := &dgdRun{run: &gpuTestRun{namespace: "ns"}}
			got, err := dgdFailure(context.Background(), f.context(), f.dyn, r, waitingDGD, false)
			if err != nil {
				t.Fatalf("dgdFailure() error = %v", err)
			}
			if tt.want == "" && got != "" {
				t.Errorf("dgdFailure() = %q, want no failure", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("dgdFailure() = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestCheckDynamoSnapshotRestore_CheckpointingDisabledInTheOperator(t *testing.T) {
	f := newDGDFake(t)
	f.dyn.PrependReactor("create", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New(`admission webhook "vdynamographdeployment.kb.io" denied the request: ` +
			`spec.components[1].experimental.checkpoint: Forbidden: ` + checkpointDisabledMessage)
	})
	out, err := f.run(t)
	if err == nil {
		t.Fatalf("CheckDynamoSnapshotRestore() = nil, want an error\n%s", out)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
	for _, want := range []string{"dynamo-operator.checkpoint.enabled", "dynamo-snapshot mixin", checkpointDisabledMessage} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	requireNoLeakedNamespace(t, f.clientset)
}

// getReactor lets a test change what Dynamo's status says between polls: fn gets
// the call number and the stored DGD and may edit it in the tracker.
func (f *dgdFake) onDGDGet(t *testing.T, fn func(call int, stored *unstructured.Unstructured)) {
	t.Helper()
	calls := 0
	f.dyn.PrependReactor("get", "dynamographdeployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		get, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		obj, err := f.dyn.Tracker().Get(dgdGVR, get.GetNamespace(), get.GetName())
		if err != nil {
			return false, nil, nil
		}
		stored, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		fn(calls, stored)
		if err := f.dyn.Tracker().Update(dgdGVR, stored, get.GetNamespace()); err != nil {
			t.Fatalf("update DGD: %v", err)
		}
		return false, nil, nil
	})
}

func pendingGraph(dgd *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(dgd.Object, "pending", "status", "state")
	_ = unstructured.SetNestedSlice(dgd.Object, conditionList(cond("Ready", "False", "waiting_for_checkpoint", "")), "status", "conditions")
	_ = unstructured.SetNestedMap(dgd.Object, map[string]any{"checkpointName": testCheckpointName}, "status", "checkpoints", dgdWorker)
}

// TestCheckDynamoSnapshotRestore_FollowsTheGraphThroughItsPhases is the one test
// where the cluster changes between polls, as it does for real: the capture runs
// (the capture pod ends the way a checkpoint ends it), the checkpoint turns ready,
// and only then the restored worker and the frontend appear.
func TestCheckDynamoSnapshotRestore_FollowsTheGraphThroughItsPhases(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = pendingGraph
		f.pods = func(namespace string) []*corev1.Pod { return []*corev1.Pod{capturePod(namespace)} }
	})
	var gets int
	f.onDGDGet(t, func(call int, stored *unstructured.Unstructured) {
		gets = call
		if call == 4 { // the checkpoint turns ready
			_ = unstructured.SetNestedMap(stored.Object, map[string]any{"checkpointName": testCheckpointName, "ready": true}, "status", "checkpoints", dgdWorker)
		}
		if call == 7 { // the graph is Ready and the worker and frontend are up
			readyGraph(stored)
			for _, pod := range []*corev1.Pod{restoredWorker(stored.GetNamespace()), graphPod(stored.GetNamespace(), "frontend", dgdFrontend, time.Now(), nil)} {
				mustDelete(t, f.clientset.Tracker().Add(pod))
			}
		}
	})
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	if gets < 7 {
		t.Errorf("the check read the DGD %d times, want it to poll through the capture and the restore", gets)
	}
	if !strings.Contains(out, "verdict: PASS") {
		t.Errorf("stdout missing the verdict:\n%s", out)
	}
}

func TestCheckDynamoSnapshotRestore_ATransientFailedStateIsTolerated(t *testing.T) {
	f := newDGDFake(t)
	f.onDGDGet(t, func(call int, stored *unstructured.Unstructured) {
		readyGraph(stored)
		if call <= 3 { // Dynamo's operator reports failed for a reconcile error, then recovers
			_ = unstructured.SetNestedField(stored.Object, "failed", "status", "state")
			_ = unstructured.SetNestedSlice(stored.Object, conditionList(cond("Ready", "False", "reconcile_error", "webhook not ready")), "status", "conditions")
		}
	})
	if out, err := f.run(t); err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v: a failed state that clears must not fail the check\n%s", err, out)
	}
}

func TestCheckDynamoSnapshotRestore_APersistentFailedStateFails(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = func(dgd *unstructured.Unstructured) {
			pendingGraph(dgd)
			_ = unstructured.SetNestedField(dgd.Object, "failed", "status", "state")
		}
		f.pods = func(string) []*corev1.Pod { return nil }
	})
	dgdFailedGrace = 30 * time.Millisecond
	_, err := f.run(t)
	if err == nil || !strings.Contains(err.Error(), "stayed failed for 30ms") {
		t.Fatalf("CheckDynamoSnapshotRestore() = %v, want an error saying the DGD stayed failed", err)
	}
}

func TestCheckDynamoSnapshotRestore_ADeletedGraphFailsAtOnce(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = pendingGraph
		f.pods = func(string) []*corev1.Pod { return nil }
	})
	dgdCaptureTimeout = time.Minute // the disappearance must end the wait, not the clock
	f.dyn.PrependReactor("get", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return false, nil, nil
	})
	calls := 0
	f.dyn.PrependReactor("get", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls >= 2 {
			return true, nil, k8serrors.NewNotFound(dgdGVR.GroupResource(), dgdName)
		}
		return false, nil, nil
	})
	start := time.Now()
	_, err := f.run(t)
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("CheckDynamoSnapshotRestore() = %v, want an error saying the DGD disappeared", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Error("a DGD that was seen and is gone must end the wait at once")
	}
}

func TestCheckDynamoSnapshotRestore_ACanceledCheckDoesNotWaitForThePods(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = pendingGraph
	})
	dgdTeardownTimeout = time.Minute // a wait for the lingering pods would show here
	dgdCaptureTimeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.dyn.PrependReactor("create", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel() // SIGTERM arrives once the graph exists
		return false, nil, nil
	})
	vctx := f.context()
	vctx.Ctx = ctx

	var err error
	start := time.Now()
	out := captureStdout(t, func() { err = CheckDynamoSnapshotRestore(vctx) })
	if err == nil {
		t.Fatalf("CheckDynamoSnapshotRestore() = nil, want a cancellation error\n%s", out)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if time.Since(start) > 30*time.Second {
		t.Error("a canceled check must not wait for the graph's pods: SIGTERM leaves only the pod's termination grace")
	}
	for _, want := range []string{"--- Delete DynamoGraphDeployment ---", "not waiting for the pods", "deleted — confirmed absent"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q: the DGD delete and the namespace cleanup must still run:\n%s", want, out)
		}
	}
	requireNoLeakedNamespace(t, f.clientset)
}

func TestDGDFailure_StuckPodsCarryTheirReasons(t *testing.T) {
	f := newDGDFake(t)
	crashing := graphPod("ns", "worker", dgdWorker, time.Now(), func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:                 dgdMainContainer,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: "restore proxy lost the process"}},
		}}
	})
	mustDelete(t, f.clientset.Tracker().Add(crashing))
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	got, err := dgdFailure(context.Background(), f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns"}}, dgd, true)
	mustDelete(t, err)
	for _, want := range []string{"pod worker stuck", "CrashLoopBackOff", "last exit: main exited 1 (Error): restore proxy lost the process", "logs:"} {
		if !strings.Contains(got, want) {
			t.Errorf("dgdFailure() = %q, want it to contain %q", got, want)
		}
	}
}

func TestDGDFailure_ACaptureFailureIsIgnoredOnceTheCheckpointIsReady(t *testing.T) {
	f := newDGDFake(t)
	mustDelete(t, f.clientset.Tracker().Add(endedCapturePod("ns", 1, "Error", "unexpected")))
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	r := &dgdRun{run: &gpuTestRun{namespace: "ns"}}
	if got, err := dgdFailure(context.Background(), f.context(), f.dyn, r, dgd, false); err != nil || got == "" {
		t.Errorf("dgdFailure() = %q, %v, want a failure for a crashed capture pod before the checkpoint is ready", got, err)
	}
	if got, err := dgdFailure(context.Background(), f.context(), f.dyn, r, dgd, true); err != nil || got != "" {
		t.Errorf("dgdFailure() = %q, %v after the checkpoint is ready: the SnapshotJob, not the stale capture pod, decides", got, err)
	}
}

// TestDGDFailure_TheFailedStateClockRestartsAfterARecovery pins that the grace
// period is for one unbroken stretch of "failed": a graph that fails, recovers and
// fails again much later gets a fresh grace period, not the first one's leftovers.
func TestDGDFailure_TheFailedStateClockRestartsAfterARecovery(t *testing.T) {
	f := newDGDFake(t)
	dgdFailedGrace = 40 * time.Millisecond
	graphIn := func(state string) *unstructured.Unstructured {
		dgd := &unstructured.Unstructured{Object: map[string]any{}}
		_ = unstructured.SetNestedField(dgd.Object, state, "status", "state")
		return dgd
	}
	r := &dgdRun{run: &gpuTestRun{namespace: "ns"}}
	check := func(state string) string {
		got, err := dgdFailure(context.Background(), f.context(), f.dyn, r, graphIn(state), false)
		mustDelete(t, err)
		return got
	}

	if got := check("failed"); got != "" {
		t.Fatalf("first failed state = %q, want it tolerated inside the grace period", got)
	}
	if got := check("pending"); got != "" {
		t.Fatalf("recovered state = %q, want no failure", got)
	}
	time.Sleep(2 * dgdFailedGrace) // longer than the grace period measured from the first failure
	if got := check("failed"); got != "" {
		t.Fatalf("failed again after a recovery = %q, want a fresh grace period", got)
	}
	time.Sleep(2 * dgdFailedGrace)
	if got := check("failed"); !strings.Contains(got, "stayed failed") {
		t.Fatalf("a state failed for longer than the grace period = %q, want the failure", got)
	}
}

func TestDeleteDGDAndWait_LeavesAForeignNamespaceAlone(t *testing.T) {
	f := newDGDFake(t)
	got := deleteDGDAndWait(f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns", nsForeign: true}})
	if !strings.Contains(got, "nothing to delete") {
		t.Errorf("deleteDGDAndWait() = %q for a namespace this run does not own", got)
	}
}

func TestDetectNvidiaRuntimeClass(t *testing.T) {
	tests := []struct {
		name     string
		getErr   error
		want     string
		wantCode errors.ErrorCode
	}{
		{name: "present", want: nvidiaRuntimeClassName},
		{name: "absent falls back to the default runtime", getErr: k8serrors.NewNotFound(schema.GroupResource{Resource: "runtimeclasses"}, nvidiaRuntimeClassName)},
		{name: "denied is not absent", getErr: k8serrors.NewForbidden(schema.GroupResource{Resource: "runtimeclasses"}, nvidiaRuntimeClassName, stderrors.New("rbac")), wantCode: errors.ErrCodeUnauthorized},
		{name: "a failed read is not absent", getErr: stderrors.New("connection refused"), wantCode: errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDGDFake(t)
			if tt.getErr != nil {
				f.clientset.PrependReactor("get", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}
			got, err := detectNvidiaRuntimeClass(f.context())
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("detectNvidiaRuntimeClass() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestCheckDynamoSnapshotRestore_ADeniedRuntimeClassReadFailsBeforeTheGraph(t *testing.T) {
	f := newDGDFake(t)
	f.clientset.PrependReactor("get", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "runtimeclasses"}, nvidiaRuntimeClassName, stderrors.New("rbac"))
	})
	_, err := f.run(t)
	if err == nil || !strings.Contains(err.Error(), "runtimeclasses") {
		t.Fatalf("CheckDynamoSnapshotRestore() = %v, want an error naming the denied runtimeclasses read", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if len(f.dgds) != 0 {
		t.Error("a denied RuntimeClass read must fail before a DGD is created")
	}
	requireNoLeakedNamespace(t, f.clientset)
}

func TestSanitizeLine(t *testing.T) {
	if got := sanitizeLine("ok\nverdict: PASS\r\x1b[31mred\ttab"); strings.ContainsAny(got, "\n\r\t\x1b") || !strings.Contains(got, "verdict: PASS") {
		t.Errorf("sanitizeLine() = %q, want control characters replaced and the text kept", got)
	}
	if got := sanitizeLog("a\nb\tc\x1b[0m"); got != "a\nb\tc [0m" {
		t.Errorf("sanitizeLog() = %q, want line breaks and tabs kept and escapes replaced", got)
	}
}

func TestDescribeDGD_ControllerMessagesStayOnOneLine(t *testing.T) {
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedSlice(dgd.Object, conditionList(cond("Ready", "False", "reconcile_error", "boom\n--- Frontend answer ---\nverdict: PASS")), "status", "conditions")
	for _, line := range strings.Split(describeDGD(dgd), "\n") {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "verdict:") {
			t.Errorf("describeDGD() lets a controller message start the line %q", line)
		}
	}
}

func TestDGDFailure_ADeniedSnapshotJobListIsReportedAsPermission(t *testing.T) {
	f := newDGDFake(t)
	f.dyn.PrependReactor("list", "snapshotjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(snapshotJobGVR.GroupResource(), "", stderrors.New("rbac"))
	})
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	_, err := dgdFailure(context.Background(), f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns"}}, dgd, false)
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if !strings.Contains(err.Error(), "list snapshotjobs.nvidia.com") {
		t.Errorf("error %q lacks the verb and the resource", err)
	}
}

func TestCheckDynamoSnapshotRestore_ADeniedSnapshotJobListEndsTheCaptureWaitAtOnce(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) { f.dgdStatus = pendingGraph })
	dgdCaptureTimeout = time.Minute
	f.dyn.PrependReactor("list", "snapshotjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(snapshotJobGVR.GroupResource(), "", stderrors.New("rbac"))
	})
	start := time.Now()
	_, err := f.run(t)
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if time.Since(start) > 30*time.Second {
		t.Error("a denied list must end the wait, not run into the capture timeout")
	}
	requireNoLeakedNamespace(t, f.clientset)
}

func unschedulablePod(namespace string) *corev1.Pod {
	pod := capturePod(namespace)
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = nil
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/1 nodes are available: 1 Insufficient nvidia.com/gpu.",
	}}
	return pod
}

func TestDGDFailure_ATemporarilyUnschedulablePodIsNotAFailure(t *testing.T) {
	f := newDGDFake(t)
	mustDelete(t, f.clientset.Tracker().Add(unschedulablePod("ns")))
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	got, err := dgdFailure(context.Background(), f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns"}}, dgd, false)
	if err != nil || got != "" {
		t.Errorf("dgdFailure() = %q, %v for an Unschedulable pod, want it to keep polling", got, err)
	}
}

func TestCheckDynamoSnapshotRestore_AnUnschedulableCapturePodIsReportedAtTheTimeout(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = pendingGraph
		f.pods = func(namespace string) []*corev1.Pod { return []*corev1.Pod{unschedulablePod(namespace)} }
	})
	dgdCaptureTimeout = 200 * time.Millisecond
	_, err := f.run(t)
	if err == nil || !strings.Contains(err.Error(), "not scheduled: 0/1 nodes are available") {
		t.Fatalf("CheckDynamoSnapshotRestore() = %v, want the timeout to carry the scheduling reason", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
}

func TestCheckDynamoSnapshotRestore_AnUnschedulableCapturePodThatGetsAGPUPasses(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = pendingGraph
		f.pods = func(namespace string) []*corev1.Pod { return []*corev1.Pod{unschedulablePod(namespace)} }
	})
	f.onDGDGet(t, func(call int, stored *unstructured.Unstructured) {
		if call == 3 { // the GPU frees up: the capture runs and the graph comes up
			readyGraph(stored)
			ns := stored.GetNamespace()
			mustDelete(t, f.clientset.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ns, "checkpoint-capture"))
			for _, pod := range healthyGraphPods(ns) {
				mustDelete(t, f.clientset.Tracker().Add(pod))
			}
		}
	})
	if out, err := f.run(t); err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v: an Unschedulable pod that is placed later must not fail the check\n%s", err, out)
	}
}

// TestDGDRun_TheFailedClockFollowsEveryRead pins that the failed-state clock is
// updated by the read itself, not only by dgdFailure: a recovery that a wait sees
// and returns on (the checkpoint turning ready) must not leave the first failure's
// timestamp for the next wait to inherit.
func TestDGDRun_TheFailedClockFollowsEveryRead(t *testing.T) {
	f := newDGDFake(t)
	dgdFailedGrace = 40 * time.Millisecond
	r := &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img"}
	dgd, err := buildDGD(r, nil, []string{"node1"}, nil)
	mustDelete(t, err)
	_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
	mustDelete(t, err)
	setState := func(state string) {
		stored, getErr := f.dyn.Resource(dgdGVR).Namespace("ns").Get(context.Background(), dgdName, metav1.GetOptions{})
		mustDelete(t, getErr)
		_ = unstructured.SetNestedField(stored.Object, state, "status", "state")
		_, upErr := f.dyn.Resource(dgdGVR).Namespace("ns").Update(context.Background(), stored, metav1.UpdateOptions{})
		mustDelete(t, upErr)
	}
	read := func() *unstructured.Unstructured {
		obj, _, getErr := getDGD(context.Background(), f.dyn, r)
		mustDelete(t, getErr)
		return obj
	}

	setState("failed")
	_, firstErr := dgdFailure(context.Background(), f.context(), f.dyn, r, read(), false) // the capture wait sees it fail
	mustDelete(t, firstErr)
	time.Sleep(2 * dgdFailedGrace)
	setState("successful")
	read() // the recovery is read, and a wait returns on it without calling dgdFailure
	setState("failed")
	obj := read()
	if got, failErr := dgdFailure(context.Background(), f.context(), f.dyn, r, obj, true); failErr != nil || got != "" {
		t.Fatalf("dgdFailure() = %q, %v right after a new failure, want a fresh grace period", got, failErr)
	}
	time.Sleep(2 * dgdFailedGrace)
	obj = read()
	if got, _ := dgdFailure(context.Background(), f.context(), f.dyn, r, obj, true); !strings.Contains(got, "stayed failed") {
		t.Errorf("dgdFailure() = %q for a DGD failed throughout, want the grace period exhausted", got)
	}
}

func TestCheckDynamoSnapshotRestore_RestoreNeedsAWorkerFromTheCurrentPoll(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.dgdStatus = func(dgd *unstructured.Unstructured) {
			readyGraph(dgd)
			_ = unstructured.SetNestedSlice(dgd.Object, conditionList(cond("Ready", "False", "some_resources_are_not_ready", "")), "status", "conditions")
		}
	})
	dgdRestoreTimeout = 300 * time.Millisecond
	f.onDGDGet(t, func(call int, stored *unstructured.Unstructured) {
		if call == 3 { // the graph turns Ready in the same poll in which the worker is gone
			readyGraph(stored)
			mustDelete(t, f.clientset.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), stored.GetNamespace(), "worker"))
		}
	})
	_, err := f.run(t)
	if err == nil || !strings.Contains(err.Error(), "not Ready with a restored worker") {
		t.Fatalf("CheckDynamoSnapshotRestore() = %v, want a Ready graph without a worker in the poll not to pass the restore", err)
	}
}

func TestDeleteDGDAndWait_IsBoundToTheDGDThisRunCreated(t *testing.T) {
	create := func(t *testing.T, f *dgdFake, token string, uid string) {
		t.Helper()
		dgd, err := buildDGD(&dgdRun{run: &gpuTestRun{namespace: "ns", token: token}, image: "img"}, nil, []string{"node1"}, nil)
		mustDelete(t, err)
		if uid != "" {
			dgd.SetUID(types.UID(uid))
		}
		_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
		mustDelete(t, err)
	}
	exists := func(f *dgdFake) bool {
		_, err := f.dyn.Resource(dgdGVR).Namespace("ns").Get(context.Background(), dgdName, metav1.GetOptions{})
		return err == nil
	}
	t.Run("a run that never attempted a create deletes nothing", func(t *testing.T) {
		f := newDGDFake(t)
		create(t, f, "t", "")
		got := deleteDGDAndWait(f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}})
		if !strings.Contains(got, "nothing to delete") || !exists(f) {
			t.Errorf("deleteDGDAndWait() = %q, DGD exists=%t, want the DGD of a run that never created one left alone", got, exists(f))
		}
	})
	t.Run("a DGD without this run's label is left alone", func(t *testing.T) {
		f := newDGDFake(t)
		create(t, f, "someone-else", "")
		got := deleteDGDAndWait(f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, createAttempted: true})
		if !strings.Contains(got, "left untouched") || !exists(f) {
			t.Errorf("deleteDGDAndWait() = %q, DGD exists=%t, want a DGD this run does not own left alone", got, exists(f))
		}
	})
	t.Run("the delete carries the UID precondition", func(t *testing.T) {
		f := newDGDFake(t)
		create(t, f, "t", "uid-1")
		var got *metav1.Preconditions
		f.dyn.PrependReactor("delete", "dynamographdeployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if del, ok := action.(k8stesting.DeleteActionImpl); ok {
				got = del.DeleteOptions.Preconditions
			}
			return false, nil, nil
		})
		msg := deleteDGDAndWait(f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, createAttempted: true})
		if got == nil || got.UID == nil || *got.UID != "uid-1" {
			t.Errorf("delete preconditions = %v, want the DGD's UID (message %q)", got, msg)
		}
	})
	t.Run("the ownership label is the run's", func(t *testing.T) {
		dgd, err := buildDGD(&dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img"}, nil, nil, nil)
		mustDelete(t, err)
		if dgd.GetLabels()[labels.GPUTestRun] != "t" {
			t.Errorf("DGD labels = %v, want the run's %s label", dgd.GetLabels(), labels.GPUTestRun)
		}
	})
}

func TestCheckDynamoSnapshotRestore_ACreateThatFailsAmbiguouslyStillCleansUpItsDGD(t *testing.T) {
	f := newDGDFake(t)
	// The Create lands in the cluster but the response is lost.
	f.dyn.PrependReactor("create", "dynamographdeployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		if err := f.dyn.Tracker().Create(dgdGVR, create.GetObject(), create.GetNamespace()); err != nil {
			t.Fatalf("store DGD: %v", err)
		}
		return true, nil, stderrors.New("context deadline exceeded")
	})
	_, err := f.run(t)
	if err == nil {
		t.Fatal("CheckDynamoSnapshotRestore() = nil, want the create failure")
	}
	list, listErr := f.dyn.Resource(dgdGVR).List(context.Background(), metav1.ListOptions{})
	mustDelete(t, listErr)
	if len(list.Items) != 0 {
		t.Errorf("%d DGDs left after an ambiguous create, want the run's DGD cleaned up", len(list.Items))
	}
}

func TestNormalizeAnswer(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Paris", "paris"},
		{" Paris. ", "paris"},
		{"**Paris**", "paris"},
		{"\"Paris\"\n", "paris"},
		{"Paris is not the capital of France.", "paris is not the capital of france"},
		{"The capital is Paris", "the capital is paris"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeAnswer(tt.in); got != tt.want {
			t.Errorf("normalizeAnswer(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCheckDynamoSnapshotRestore_HonorsTheNodeSelectorOverride(t *testing.T) {
	f := newDGDFake(t)
	ctx := f.context()
	ctx.NodeSelector = map[string]string{"pool": "validation"}
	var err error
	out := captureStdout(t, func() { err = CheckDynamoSnapshotRestore(ctx) })
	if err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	for _, name := range []string{dgdWorker, dgdFrontend} {
		spec := podSpecOfComponent(t, componentOf(t, f.dgds[0], name))
		if got := spec.NodeSelector["pool"]; got != "validation" || len(spec.NodeSelector) != 1 {
			t.Errorf("%s node selector = %v, want the override {pool: validation}", name, spec.NodeSelector)
		}
	}
	// The GPU nodes the check found still confine the worker.
	requirePodSpecOnNode(t, podSpecOfComponent(t, componentOf(t, f.dgds[0], dgdWorker)), "node1")
}

func TestCheckDynamoSnapshotRestore_NoNodeSelectorLeavesThePodsUnselected(t *testing.T) {
	f := newDGDFake(t)
	if out, err := f.run(t); err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v\n%s", err, out)
	}
	for _, name := range []string{dgdWorker, dgdFrontend} {
		if sel := podSpecOfComponent(t, componentOf(t, f.dgds[0], name)).NodeSelector; len(sel) != 0 {
			t.Errorf("%s node selector = %v, want none when no override is set", name, sel)
		}
	}
}

// staleFailedWorker is an older worker pod that ended Failed (for example evicted)
// and still exists next to its replacement.
func staleFailedWorker(namespace string) *corev1.Pod {
	return graphPod(namespace, "worker-evicted", dgdWorker, time.Now().Add(-time.Hour), func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodFailed
		p.Status.Conditions = nil
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: dgdMainContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}
	})
}

func TestDGDFailure_AWorkerThatANewerOneReplacedIsNotAFailure(t *testing.T) {
	f := newDGDFake(t)
	for _, pod := range []*corev1.Pod{capturePod("ns"), staleFailedWorker("ns"), restoredWorker("ns")} {
		mustDelete(t, f.clientset.Tracker().Add(pod))
	}
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	got, err := dgdFailure(context.Background(), f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns"}}, dgd, true)
	if err != nil || got != "" {
		t.Errorf("dgdFailure() = (%q, %v), want no failure: the Failed worker was replaced by a Ready restored one", got, err)
	}
}

func TestDGDFailure_TheNewestWorkerThatFailedIsStillAFailure(t *testing.T) {
	f := newDGDFake(t)
	failed := staleFailedWorker("ns")
	failed.Name, failed.CreationTimestamp = "worker-current", metav1.NewTime(time.Now())
	mustDelete(t, f.clientset.Tracker().Add(failed))
	dgd := &unstructured.Unstructured{Object: map[string]any{}}
	got, err := dgdFailure(context.Background(), f.context(), f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns"}}, dgd, true)
	if err != nil || !strings.Contains(got, "worker-current failed") {
		t.Errorf("dgdFailure() = (%q, %v), want the newest worker's failure reported", got, err)
	}
}

func TestCheckDynamoSnapshotRestore_AReplacedFailedWorkerDoesNotFailTheRestore(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) {
		f.pods = func(ns string) []*corev1.Pod { return append(healthyGraphPods(ns), staleFailedWorker(ns)) }
	})
	if out, err := f.run(t); err != nil {
		t.Fatalf("CheckDynamoSnapshotRestore() error = %v, want a pass: the stale Failed worker was replaced\n%s", err, out)
	}
}

func TestCheckDynamoSnapshotRestore_ADeniedPodListEndsTheRestoreWaitAtOnce(t *testing.T) {
	f := newDGDFake(t)
	dgdRestoreTimeout = time.Minute
	dgdTeardownTimeout = 100 * time.Millisecond
	denied := true
	f.clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !denied {
			return false, nil, nil
		}
		return true, nil, k8serrors.NewForbidden(corev1.Resource("pods"), "", stderrors.New("rbac"))
	})
	// The teardown's own pod reads are not under test: lift the denial once the DGD is deleted.
	f.dyn.PrependReactor("delete", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		denied = false
		return false, nil, nil
	})
	start := time.Now()
	_, err := f.run(t)
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnauthorized)
	if err == nil || !strings.Contains(err.Error(), "list pods") {
		t.Errorf("error %q should name the missing permission (list pods)", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Error("a denied pod list must end the restore wait, not run into the restore timeout")
	}
}

func TestExecFrontendCompletion_ANonzeroExitIsNotAnAnswer(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "ns"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: dgdMainContainer}}},
	}
	const completion = `{"choices":[{"message":{"content":"Paris"}}]}`
	for _, tt := range []struct {
		name    string
		exit    error
		wantErr bool
	}{
		{name: "a clean exit returns the body", exit: nil},
		{name: "a failed process that flushed a completion is rejected", exit: k8sexec.CodeExitError{Err: stderrors.New("exit 1"), Code: 1}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := podExecHTTPContext(t, pod)
			restore := replacePodExecExecutorForTest(func(*rest.Config, string, string) (remotecommand.Executor, error) {
				return fakePodExecutor{stream: func(_ context.Context, opts remotecommand.StreamOptions) error {
					_, _ = opts.Stdout.Write([]byte(completion))
					_, _ = opts.Stderr.Write([]byte("urlopen error\nsecond line"))
					return tt.exit
				}}, nil
			})
			defer restore()
			r := &dgdRun{run: &gpuTestRun{namespace: "ns"}}
			body, err := execFrontendCompletion(context.Background(), ctx, r, &pod)
			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("execFrontendCompletion() = %q, nil, want an error for a nonzero exit", body)
			case tt.wantErr:
				if !strings.Contains(err.Error(), "exited with status 1") || strings.Contains(err.Error(), "\n") {
					t.Errorf("error %q should state the exit status on one bounded line", err)
				}
			case err != nil || body != completion:
				t.Fatalf("execFrontendCompletion() = (%q, %v), want the body", body, err)
			}
		})
	}
}

func TestDeleteDGDAndWait_ACancelDuringTheWaitStopsIt(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) { f.lingeringPods = true })
	dgdTeardownTimeout = time.Minute // the poll would run this long if it ignored the cancel
	dgd, err := buildDGD(&dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img"}, nil, []string{"node1"}, nil)
	mustDelete(t, err)
	_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
	mustDelete(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The DGD stays visible and its pods linger, so the wait cannot finish by itself.
	// The cancel comes from the wait's own first read of the DGD, so the test reaches
	// the in-wait branch whatever the timing.
	deleted := false
	f.dyn.PrependReactor("delete", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return true, nil, nil
	})
	f.dyn.PrependReactor("get", "dynamographdeployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deleted {
			cancel()
		}
		return false, nil, nil
	})
	vctx := f.context()
	vctx.Ctx = ctx

	start := time.Now()
	got := deleteDGDAndWait(vctx, f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, createAttempted: true})
	if time.Since(start) > 10*time.Second {
		t.Fatalf("deleteDGDAndWait() ran %s: a canceled check must leave the teardown wait at once so the namespace cleanup runs", time.Since(start))
	}
	if !strings.Contains(got, "canceled while the DynamoGraphDeployment was being removed") {
		t.Errorf("deleteDGDAndWait() = %q, want the message of the in-wait cancel", got)
	}
}

// TestDeleteDGDAndWait_ACancelBeforeTheTeardownStillDeletes: a check canceled
// before the teardown starts must still read and delete its DGD, with the wait for
// the pods skipped.
func TestDeleteDGDAndWait_ACancelBeforeTheTeardownStillDeletes(t *testing.T) {
	f := newDGDFake(t)
	dgd, err := buildDGD(&dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img"}, nil, []string{"node1"}, nil)
	mustDelete(t, err)
	_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
	mustDelete(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	vctx := f.context()
	vctx.Ctx = ctx

	got := deleteDGDAndWait(vctx, f.dyn, &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, createAttempted: true})
	if !strings.Contains(got, "the check was canceled: the DynamoGraphDeployment delete was issued") {
		t.Errorf("deleteDGDAndWait() = %q, want the delete issued and the pod wait skipped", got)
	}
	if _, err := f.dyn.Resource(dgdGVR).Namespace("ns").Get(context.Background(), dgdName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("DGD get error = %v, want NotFound: a canceled check must still delete its DGD", err)
	}
}

// TestDeleteDGDAndWait_WaitsForPodsWhenTheDGDIsAlreadyGone: the DGD object can be
// gone while a pod is still terminating; the namespace cleanup must not start
// before that pod is.
func TestDeleteDGDAndWait_WaitsForPodsWhenTheDGDIsAlreadyGone(t *testing.T) {
	f := newDGDFake(t, func(f *dgdFake) { f.pods = func(string) []*corev1.Pod { return nil } })
	dgdTeardownTimeout = 50 * time.Millisecond
	mustDelete(t, f.clientset.Tracker().Add(graphPod("ns", "worker", dgdWorker, time.Now(), nil)))
	r := &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, createAttempted: true}

	if got := deleteDGDAndWait(f.context(), f.dyn, r); !strings.Contains(got, "still being removed") {
		t.Errorf("deleteDGDAndWait() = %q, want it to wait for the worker pod although the DGD is gone", got)
	}
	mustDelete(t, f.clientset.CoreV1().Pods("ns").Delete(context.Background(), "worker", metav1.DeleteOptions{}))
	dgdTeardownTimeout = 2 * time.Second
	if got := deleteDGDAndWait(f.context(), f.dyn, r); !strings.Contains(got, "no DynamoGraphDeployment to delete") {
		t.Errorf("deleteDGDAndWait() = %q once the pod ended", got)
	}
}

// TestWaitForRestoredWorker_JudgesOneSnapshotOfThePods: the restore verdict and the
// failure scan read the pods once per poll, so a worker deleted between two lists
// cannot satisfy one and escape the other.
func TestWaitForRestoredWorker_JudgesOneSnapshotOfThePods(t *testing.T) {
	f := newDGDFake(t)
	r := &dgdRun{run: &gpuTestRun{namespace: "ns", token: "t"}, image: "img", checkpointName: testCheckpointName, createAttempted: true}
	dgd, err := buildDGD(r, nil, []string{"node1"}, nil)
	mustDelete(t, err)
	_, err = f.dyn.Resource(dgdGVR).Namespace("ns").Create(context.Background(), dgd, metav1.CreateOptions{})
	mustDelete(t, err)
	lists := 0
	f.clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		return false, nil, nil
	})

	if _, err := waitForRestoredWorker(f.context(), f.dyn, r); err != nil {
		t.Fatalf("waitForRestoredWorker() = %v, want success on a Ready, restored graph", err)
	}
	if lists != 1 {
		t.Errorf("pods listed %d times in one successful poll, want 1: the verdict and the failure scan must share a snapshot", lists)
	}
}

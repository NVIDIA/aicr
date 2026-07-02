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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

var (
	testSlinkyLoginSetGVR = schema.GroupVersionResource{
		Group:    "slinky.slurm.net",
		Version:  "v1beta1",
		Resource: "loginsets",
	}
	testSlinkyNodeSetGVR = schema.GroupVersionResource{
		Group:    "slinky.slurm.net",
		Version:  "v1beta1",
		Resource: "nodesets",
	}
)

func TestCheckSlinkySlurmHealthSkipsWithoutSlinkyComponent(t *testing.T) {
	ctx := &validators.Context{
		Ctx:        context.Background(),
		Clientset:  k8sfake.NewSimpleClientset(),
		RESTConfig: &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
		},
	}

	err := CheckSlinkySlurmHealth(ctx)
	if !isSkipLike(err, "slinky-slurm") {
		t.Fatalf("error = %v, want skip mentioning slinky-slurm", err)
	}
}

func TestCheckSlinkySlurmHealthRequiresContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  *validators.Context
		want string
	}{
		{
			name: "missing client",
			ctx: &validators.Context{
				Ctx:        context.Background(),
				RESTConfig: &rest.Config{Host: "https://example.test"},
				ValidationInput: &v1.ValidationInput{
					ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
				},
			},
			want: "kubernetes client",
		},
		{
			name: "missing rest config",
			ctx: &validators.Context{
				Ctx:       context.Background(),
				Clientset: k8sfake.NewSimpleClientset(),
				ValidationInput: &v1.ValidationInput{
					ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
				},
			},
			want: "RESTConfig",
		},
		{
			name: "missing validation",
			ctx: &validators.Context{
				Ctx:        context.Background(),
				Clientset:  k8sfake.NewSimpleClientset(),
				RESTConfig: &rest.Config{Host: "https://example.test"},
			},
			want: "validation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSlinkySlurmHealth(tt.ctx)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCheckSlinkySlurmHealthSkipsWhenSlinkyAPIUnavailable(t *testing.T) {
	ctx := &validators.Context{
		Ctx:        context.Background(),
		Clientset:  k8sfake.NewSimpleClientset(),
		RESTConfig: &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
		},
	}

	err := CheckSlinkySlurmHealth(ctx)
	if !isSkipLike(err, "Slinky Slurm API") {
		t.Fatalf("error = %v, want skip mentioning Slinky Slurm API", err)
	}
}

func TestCheckSlinkySlurmHealthFailsWhenSlinkyNamespaceMissing(t *testing.T) {
	ctx := slurmReadyTestContext(t, false)
	dynClient := newSlinkyDynamicClient(t)
	dynClient.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, slinkySlurmNamespace)
	})
	ctx.DynamicClient = dynClient

	err := CheckSlinkySlurmHealth(ctx)
	if err == nil || !strings.Contains(err.Error(), "failed to list Slinky Slurm NodeSets in namespace slurm") {
		t.Fatalf("error = %v, want namespace list failure", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "skip") {
		t.Fatalf("error = %v, want real failure not skip", err)
	}
}

func TestCheckSlinkySlurmHealthExecOutcomes(t *testing.T) {
	errBoom := errors.New(errors.ErrCodeInternal, "exec failed")
	tests := []struct {
		name    string
		result  podExecResult
		err     error
		wantErr string
	}{
		{name: "success", result: podExecResult{Stdout: "slinky-0\n", ExitCode: 0}},
		{name: "empty stdout", result: podExecResult{Stdout: "\n", ExitCode: 0}, wantErr: "empty stdout"},
		{name: "nonzero", result: podExecResult{Stderr: "srun failed", ExitCode: 1}, wantErr: "exit code 1"},
		{name: "exec error", err: errBoom, wantErr: "exec failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := replaceSlinkyExecForTest(func(
				context.Context,
				*validators.Context,
				string,
				string,
				[]string,
				podExecOptions,
			) (podExecResult, error) {

				return tt.result, tt.err
			})
			defer restore()

			err := CheckSlinkySlurmHealth(slurmReadyTestContext(t, false))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckSlinkySlurmHealthRunsAllHealthCommands(t *testing.T) {
	var gotCommands []string
	var gotOptions []podExecOptions
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		command []string,
		opts podExecOptions,
	) (podExecResult, error) {

		gotCommands = append(gotCommands, strings.Join(command, " "))
		gotOptions = append(gotOptions, opts)
		return podExecResult{Stdout: strings.Join(command, " ") + "\n"}, nil
	})
	defer restore()

	err := CheckSlinkySlurmHealth(slurmReadyTestContext(t, false))
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}

	wantCommands := []string{
		"scontrol ping",
		"/bin/sh -c " + slinkySlurmSinfoIdleMixShell,
		"srun --immediate=5 --time=0:03 hostname",
	}
	if strings.Join(gotCommands, ",") != strings.Join(wantCommands, ",") {
		t.Fatalf("commands = %v, want %v", gotCommands, wantCommands)
	}
	for _, got := range gotOptions {
		if got.DefaultContainerAnnotation != defaultContainerAnnotation || got.PreferredContainerName != slinkyLoginPodContainerName {
			t.Fatalf("pod exec options = %+v, want Slinky login pod options", got)
		}
	}
}

func TestCheckSlinkySlurmHealthStopsWhenContextCanceled(t *testing.T) {
	ctx := slurmReadyTestContext(t, false)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx.Ctx = runCtx

	var execCount int
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		_ []string,
		_ podExecOptions,
	) (podExecResult, error) {

		execCount++
		cancel()
		return podExecResult{Stdout: "ok\n"}, nil
	})
	defer restore()

	err := CheckSlinkySlurmHealth(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %v, want context canceled failure", err)
	}
	if execCount != 1 {
		t.Fatalf("exec count = %d, want 1", execCount)
	}
}

func TestCheckSlinkySlurmHealthDiscoversPodsFromSlinkyCRSelectors(t *testing.T) {
	var gotPodName string
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, podName string,
		_ []string,
		_ podExecOptions,
	) (podExecResult, error) {

		gotPodName = podName
		return podExecResult{Stdout: "ok\n"}, nil
	})
	defer restore()

	ctx := slurmCustomCRSelectorContext(t, false)
	err := CheckSlinkySlurmHealth(ctx)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if gotPodName != "custom-login-pod" {
		t.Fatalf("exec pod = %q, want custom-login-pod", gotPodName)
	}
}

func TestCheckSlinkySlurmHealthUsesComponentRefNamespace(t *testing.T) {
	const customNamespace = "custom-slurm"

	loginPod := readyLoginPod()
	loginPod.Namespace = customNamespace
	nodeSetPod := readyNodeSetPod()
	nodeSetPod.Namespace = customNamespace
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-node-0"}}

	clientset := k8sfake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: customNamespace}},
		node,
		loginPod,
		nodeSetPod,
	)
	addSlinkyDiscovery(t, clientset)

	var gotNamespace string
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		namespace string,
		_ string,
		_ []string,
		_ podExecOptions,
	) (podExecResult, error) {

		gotNamespace = namespace
		return podExecResult{Stdout: "ok\n"}, nil
	})
	defer restore()

	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newSlinkyDynamicClient(t, defaultLoginSetInNamespace(customNamespace), defaultNodeSetInNamespace(customNamespace)),
		RESTConfig:    &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent, Namespace: customNamespace}},
		},
	}

	err := CheckSlinkySlurmHealth(ctx)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if gotNamespace != customNamespace {
		t.Fatalf("exec namespace = %q, want %q", gotNamespace, customNamespace)
	}
}

func TestCheckSlinkySlurmHealthSelectsNewestReadyLoginPod(t *testing.T) {
	olderReady := readyLoginPod()
	olderReady.Name = "slinky-login-old"
	olderReady.CreationTimestamp = metav1.Unix(100, 0)

	terminatingReady := readyLoginPod()
	terminatingReady.Name = "slinky-login-terminating"
	terminatingReady.CreationTimestamp = metav1.Unix(300, 0)
	deletionTime := metav1.Unix(400, 0)
	terminatingReady.DeletionTimestamp = &deletionTime

	newerReady := readyLoginPod()
	newerReady.Name = "slinky-login-new"
	newerReady.CreationTimestamp = metav1.Unix(200, 0)

	clientset := k8sfake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: slinkySlurmNamespace}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-node-0"}},
		olderReady,
		terminatingReady,
		newerReady,
		readyNodeSetPod(),
	)
	addSlinkyDiscovery(t, clientset)

	var gotPodName string
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_ string,
		podName string,
		_ []string,
		_ podExecOptions,
	) (podExecResult, error) {

		gotPodName = podName
		return podExecResult{Stdout: "ok\n"}, nil
	})
	defer restore()

	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newSlinkyDynamicClient(t, defaultLoginSet(), defaultNodeSet()),
		RESTConfig:    &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
		},
	}

	err := CheckSlinkySlurmHealth(ctx)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if gotPodName != newerReady.Name {
		t.Fatalf("exec pod = %q, want %q", gotPodName, newerReady.Name)
	}
}

func TestCheckSlinkySlurmHealthFailsOnMalformedControllerRefName(t *testing.T) {
	ctx := slurmReadyTestContext(t, false)
	loginSet := defaultLoginSet()
	if err := unstructured.SetNestedField(loginSet.Object, map[string]any{"bad": "shape"},
		"spec", "controllerRef", "name"); err != nil {
		t.Fatalf("set malformed controllerRef.name: %v", err)
	}
	ctx.DynamicClient = newSlinkyDynamicClient(t, loginSet, defaultNodeSet())

	err := CheckSlinkySlurmHealth(ctx)
	if err == nil || !strings.Contains(err.Error(), "failed to read controllerRef.name") {
		t.Fatalf("error = %v, want malformed controllerRef.name read failure", err)
	}
}

func TestCheckSlinkySlurmHealthCollectsAllCommandFailures(t *testing.T) {
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		command []string,
		_ podExecOptions,
	) (podExecResult, error) {

		joined := strings.Join(command, " ")
		if strings.Contains(joined, "sinfo -h -Ne -t idle,mix") {
			return podExecResult{Stderr: "down", ExitCode: 1}, nil
		}
		return podExecResult{Stdout: "\n"}, nil
	})
	defer restore()

	err := CheckSlinkySlurmHealth(slurmReadyTestContext(t, false))
	if err == nil {
		t.Fatal("error = nil, want combined health failure")
	}
	for _, want := range []string{
		"scontrol ping: empty stdout",
		"sinfo idle/mix: exit code 1",
		"srun hostname: empty stdout",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want containing %q", err, want)
		}
	}
}

func slurmCustomCRSelectorContext(t *testing.T, kwok bool) *validators.Context {
	t.Helper()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-node-0"}}
	if kwok {
		node.Annotations = map[string]string{kwokNodeAnnotation: "fake"}
	}

	clientset := k8sfake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: slinkySlurmNamespace}},
		node,
		readyCustomLoginPod(),
		readyCustomNodeSetPod(),
	)
	addSlinkyDiscovery(t, clientset)

	return &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newSlinkyDynamicClient(t, customLoginSet(), customNodeSet()),
		RESTConfig:    &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
		},
	}
}

func TestCheckSlinkySlurmHealthSkipsWhenAllNodeSetPodsAreOnKWOKNodes(t *testing.T) {
	restore := replaceSlinkyExecForTest(func(
		context.Context,
		*validators.Context,
		string,
		string,
		[]string,
		podExecOptions,
	) (podExecResult, error) {

		t.Fatal("exec should not run when all NodeSet pods are on KWOK nodes")
		return podExecResult{}, nil
	})
	defer restore()

	err := CheckSlinkySlurmHealth(slurmReadyTestContext(t, true))
	if !isSkipLike(err, "KWOK") {
		t.Fatalf("error = %v, want KWOK skip", err)
	}
}

func TestCheckSlinkySlurmHealthDoesNotSkipWhenNodeSetPodIsUnbound(t *testing.T) {
	kwokNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "kwok-node-0",
			Annotations: map[string]string{kwokNodeAnnotation: "fake"},
		},
	}
	kwokPod := readyNodeSetPod()
	kwokPod.Name = "slinky-nodeset-kwok"
	kwokPod.Spec.NodeName = kwokNode.Name
	unboundPod := readyNodeSetPod()
	unboundPod.Name = "slinky-nodeset-unbound"
	unboundPod.Spec.NodeName = ""

	clientset := k8sfake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: slinkySlurmNamespace}},
		kwokNode,
		readyLoginPod(),
		kwokPod,
		unboundPod,
	)
	addSlinkyDiscovery(t, clientset)

	var execRan bool
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		_ []string,
		_ podExecOptions,
	) (podExecResult, error) {

		execRan = true
		return podExecResult{Stdout: "ok\n"}, nil
	})
	defer restore()

	ctx := &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newSlinkyDynamicClient(t, defaultLoginSet(), defaultNodeSet()),
		RESTConfig:    &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
		},
	}

	err := CheckSlinkySlurmHealth(ctx)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if !execRan {
		t.Fatal("exec did not run; unbound NodeSet pod must prevent KWOK skip")
	}
}

func TestCheckSlinkySlurmHealthFailsWithoutReadyLoginPod(t *testing.T) {
	ctx := slurmReadyTestContext(t, false)
	err := ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Delete(ctx.Ctx, "slinky-login-0", metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("delete login pod: %v", err)
	}

	err = CheckSlinkySlurmHealth(ctx)
	if err == nil || !strings.Contains(err.Error(), "ready login pod") {
		t.Fatalf("error = %v, want ready login pod failure", err)
	}
}

func slurmReadyTestContext(t *testing.T, kwok bool) *validators.Context {
	t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-node-0"},
	}
	if kwok {
		node.Annotations = map[string]string{kwokNodeAnnotation: "fake"}
	}

	clientset := k8sfake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: slinkySlurmNamespace}},
		node,
		readyLoginPod(),
		readyNodeSetPod(),
	)
	addSlinkyDiscovery(t, clientset)

	return &validators.Context{
		Ctx:           context.Background(),
		Clientset:     clientset,
		DynamicClient: newSlinkyDynamicClient(t, defaultLoginSet(), defaultNodeSet()),
		RESTConfig:    &rest.Config{Host: "https://example.test"},
		ValidationInput: &v1.ValidationInput{
			ComponentRefs: []recipe.ComponentRef{{Name: slinkySlurmComponent}},
		},
	}
}

func readyLoginPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "slinky-login-0",
			Namespace: slinkySlurmNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "slurm-login",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "worker-node-0",
			Containers: []corev1.Container{{Name: "login", Image: "slinky-login:test"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "login", Ready: true}},
		},
	}
}

func readyNodeSetPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "slinky-nodeset-0",
			Namespace: slinkySlurmNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "slurm-nodeset",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "worker-node-0",
			Containers: []corev1.Container{{Name: "slurmd", Image: "slinky-slurmd:test"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "slurmd", Ready: true}},
		},
	}
}

func readyCustomLoginPod() *corev1.Pod {
	pod := readyLoginPod()
	pod.Name = "custom-login-pod"
	pod.Labels = map[string]string{
		"app.kubernetes.io/name":     "login",
		"app.kubernetes.io/instance": "custom-login",
	}
	return pod
}

func readyCustomNodeSetPod() *corev1.Pod {
	pod := readyNodeSetPod()
	pod.Name = "custom-worker-0"
	pod.Labels = map[string]string{
		"app.kubernetes.io/name":     "slurmd",
		"app.kubernetes.io/instance": "custom-worker",
	}
	return pod
}

func defaultLoginSet() *unstructured.Unstructured {
	return slinkySetObject("LoginSet", "slinky-slurm-login-slinky", "app.kubernetes.io/name=slurm-login")
}

func defaultNodeSet() *unstructured.Unstructured {
	return slinkySetObject("NodeSet", "slinky-slurm-worker-slinky", "app.kubernetes.io/name=slurm-nodeset")
}

func defaultLoginSetInNamespace(namespace string) *unstructured.Unstructured {
	return slinkySetObjectInNamespace("LoginSet", "slinky-slurm-login-slinky", "app.kubernetes.io/name=slurm-login", namespace)
}

func defaultNodeSetInNamespace(namespace string) *unstructured.Unstructured {
	return slinkySetObjectInNamespace("NodeSet", "slinky-slurm-worker-slinky", "app.kubernetes.io/name=slurm-nodeset", namespace)
}

func customLoginSet() *unstructured.Unstructured {
	return slinkySetObject("LoginSet", "custom-login", "app.kubernetes.io/instance=custom-login,app.kubernetes.io/name=login")
}

func customNodeSet() *unstructured.Unstructured {
	return slinkySetObject("NodeSet", "custom-worker", "app.kubernetes.io/instance=custom-worker,app.kubernetes.io/name=slurmd")
}

func slinkySetObject(kind, name, selector string) *unstructured.Unstructured {
	return slinkySetObjectInNamespace(kind, name, selector, slinkySlurmNamespace)
}

func slinkySetObjectInNamespace(kind, name, selector, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "slinky.slurm.net/v1beta1",
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"controllerRef": map[string]any{
					"name":      slinkySlurmComponent,
					"namespace": namespace,
				},
			},
			"status": map[string]any{
				"selector": selector,
			},
		},
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "slinky.slurm.net",
		Version: "v1beta1",
		Kind:    kind,
	})
	return obj
}

func newSlinkyDynamicClient(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		testSlinkyLoginSetGVR: "LoginSetList",
		testSlinkyNodeSetGVR:  "NodeSetList",
	}, objects...)
}

func addSlinkyDiscovery(t *testing.T, clientset kubernetes.Interface) {
	t.Helper()
	discovery, ok := clientset.Discovery().(*fake.FakeDiscovery)
	if !ok {
		t.Fatalf("discovery client = %T, want *fake.FakeDiscovery", clientset.Discovery())
	}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "slinky.slurm.net/v1beta1",
		APIResources: []metav1.APIResource{
			{
				Name:       "loginsets",
				Kind:       "LoginSet",
				Namespaced: true,
			},
			{
				Name:       "nodesets",
				Kind:       "NodeSet",
				Namespaced: true,
			},
		},
	}}
}

func isSkipLike(err error, want string) bool {
	return err != nil &&
		(strings.Contains(err.Error(), want) || strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)))
}

func replaceSlinkyExecForTest(fn podExecFunc) func() {
	old := slinkyExecCommand
	slinkyExecCommand = fn
	return func() { slinkyExecCommand = old }
}

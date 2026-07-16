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
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestCheckSlinkySlurmHealthSkipsGPUContainerSmokeForKind(t *testing.T) {
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

	ctx := slurmReadyTestContext(t, false)
	ctx.ValidationInput.Criteria = recipe.Criteria{
		Service:     recipe.CriteriaServiceKind,
		Accelerator: recipe.CriteriaAcceleratorH100,
	}
	nodeSetPod, err := ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Get(
		ctx.Ctx, "slinky-nodeset-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeSet pod: %v", err)
	}
	nodeSetPod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1"),
	}
	if _, err = ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Update(
		ctx.Ctx, nodeSetPod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update NodeSet pod: %v", err)
	}

	out := captureStdout(t, func() { err = CheckSlinkySlurmHealth(ctx) })
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if !strings.Contains(out, "--- Slinky Slurm GPU container check ---") ||
		!strings.Contains(out, "Included: false") ||
		!strings.Contains(out, "recipe service is kind") {

		t.Fatalf("output = %q, want explicit Kind GPU-check skip artifact", out)
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

func TestCheckSlinkySlurmHealthFailsWhenGPURecipeLosesNodeSetResources(t *testing.T) {
	ctx := slurmReadyTestContext(t, false)
	ctx.ValidationInput.Criteria = recipe.Criteria{
		Service:     recipe.CriteriaServiceAKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
	}

	var execCount int
	restore := replaceSlinkyExecForTest(func(
		_ context.Context,
		_ *validators.Context,
		_, _ string,
		_ []string,
		_ podExecOptions,
	) (podExecResult, error) {

		execCount++
		return podExecResult{Stdout: "unexpected\n"}, nil
	})
	defer restore()

	var err error
	out := captureStdout(t, func() { err = CheckSlinkySlurmHealth(ctx) })
	if err == nil || !strings.Contains(err.Error(), "no NodeSet pod has a positive nvidia.com/gpu request or limit") {
		t.Fatalf("error = %v, want missing NodeSet GPU resource failure", err)
	}
	if execCount != 0 {
		t.Fatalf("exec count = %d, want 0 after fail-closed GPU decision", execCount)
	}
	if !strings.Contains(out, "--- Slinky Slurm GPU container check ---") ||
		!strings.Contains(out, "Included: false") ||
		!strings.Contains(out, "service=aks accelerator=h100") {

		t.Fatalf("output = %q, want missing-resource decision artifact", out)
	}
}

func TestNodeSetPodsRequestNVIDIAGPUs(t *testing.T) {
	resourceName := corev1.ResourceName(resourceNVIDIAGPU)
	tests := []struct {
		name      string
		resources corev1.ResourceRequirements
		want      bool
	}{
		{
			name: "no GPU resource",
		},
		{
			name: "zero GPU limit",
			resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{resourceName: resource.MustParse("0")},
			},
		},
		{
			name: "positive GPU limit",
			resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{resourceName: resource.MustParse("1")},
			},
			want: true,
		},
		{
			name: "positive GPU request",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{resourceName: resource.MustParse("1")},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := []corev1.Pod{{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Resources: tt.resources}},
				},
			}}
			if got := nodeSetPodsRequestNVIDIAGPUs(pods); got != tt.want {
				t.Fatalf("nodeSetPodsRequestNVIDIAGPUs() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestResolveSlinkySlurmGPUContainerImage(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		want     string
	}{
		{
			name: "Docker Hub default",
			want: "docker.io/library/alpine:3.23.3",
		},
		{
			name:     "private registry override",
			registry: "registry.example.com/aicr",
			want:     "registry.example.com/aicr/library/alpine:3.23.3",
		},
		{
			name:     "trailing slash is normalized",
			registry: "localhost:5001/",
			want:     "localhost:5001/library/alpine:3.23.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(validatorImageRegistryEnv, tt.registry)
			if got := resolveSlinkySlurmGPUContainerImage(); got != tt.want {
				t.Fatalf("resolveSlinkySlurmGPUContainerImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSlinkySlurmGPUContainerImageTagMatchesChartSidecars guards the
// slinkySlurmGPUContainerImage constant against drifting from the Alpine tag
// the slinky-slurm chart's initconf/logfile sidecars pin in the component
// values. The health check's srun-launched pull only stays covered by
// `aicr mirror list` (which extracts images from rendered charts) while both
// reference the same Alpine tag, so a Renovate bump of one without the other
// must fail loudly here rather than silently break air-gap mirroring.
func TestSlinkySlurmGPUContainerImageTagMatchesChartSidecars(t *testing.T) {
	const valuesPath = "../../recipes/components/slinky-slurm/values.yaml"

	idx := strings.LastIndex(slinkySlurmGPUContainerImage, ":")
	if idx < 0 {
		t.Fatalf("slinkySlurmGPUContainerImage %q has no tag", slinkySlurmGPUContainerImage)
	}
	tag := slinkySlurmGPUContainerImage[idx+1:]

	data, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("read component values: %v", err)
	}
	if !strings.Contains(string(data), `tag: "`+tag+`"`) {
		t.Errorf("Alpine tag %q from slinkySlurmGPUContainerImage not pinned in %s; "+
			"the sidecar tags and the health-check image constant have drifted — "+
			"the srun pull is no longer covered by `aicr mirror list`. Bump both to match.",
			tag, valuesPath)
	}
}

func TestCheckSlinkySlurmHealthRunsGPUContainerSmokeForGPUNodeSet(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		wantRef  string
	}{
		{
			name:    "Docker Hub default",
			wantRef: "docker.io/library/alpine:3.23.3",
		},
		{
			name:     "AICR_VALIDATOR_IMAGE_REGISTRY rewrite",
			registry: "registry.example.com/aicr",
			wantRef:  "registry.example.com/aicr/library/alpine:3.23.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(validatorImageRegistryEnv, tt.registry)
			ctx := slurmReadyTestContext(t, false)
			nodeSetPod, err := ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Get(
				ctx.Ctx, "slinky-nodeset-0", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get NodeSet pod: %v", err)
			}
			nodeSetPod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
				corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1"),
			}
			if _, err = ctx.Clientset.CoreV1().Pods(slinkySlurmNamespace).Update(
				ctx.Ctx, nodeSetPod, metav1.UpdateOptions{}); err != nil {
				t.Fatalf("update NodeSet pod: %v", err)
			}

			var gotCommands []string
			restore := replaceSlinkyExecForTest(func(
				_ context.Context,
				_ *validators.Context,
				_, _ string,
				command []string,
				_ podExecOptions,
			) (podExecResult, error) {

				gotCommands = append(gotCommands, strings.Join(command, " "))
				return podExecResult{Stdout: "ok\n"}, nil
			})
			defer restore()

			out := captureStdout(t, func() { err = CheckSlinkySlurmHealth(ctx) })
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			wantImage := "docker://" + tt.wantRef
			if !strings.Contains(out, "--- Slinky Slurm GPU container check ---") ||
				!strings.Contains(out, "Included: true") ||
				!strings.Contains(out, wantImage) {

				t.Fatalf("output = %q, want included GPU-check decision artifact with %s", out, wantImage)
			}

			wantGPUCommand := "srun --immediate=30 --time=1:00 --nodes=1 --ntasks=1 " +
				"--cpus-per-task=1 --mem=128M --gpus=1 " +
				"--container-image=" + wantImage + " cat /etc/os-release"
			if len(gotCommands) != len(slinkySlurmHealthCommands)+1 {
				t.Fatalf("commands = %v, want %d commands", gotCommands, len(slinkySlurmHealthCommands)+1)
			}
			if gotCommands[len(gotCommands)-1] != wantGPUCommand {
				t.Fatalf("GPU command = %q, want %q", gotCommands[len(gotCommands)-1], wantGPUCommand)
			}
		})
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

func TestRunnableSlinkyNodeSetPodsPreservesCancellation(t *testing.T) {
	tests := []struct {
		name               string
		cancelBeforeLookup bool
		unboundPod         bool
		wantNodeGets       int
	}{
		{
			name:               "canceled before Node lookup",
			cancelBeforeLookup: true,
			wantNodeGets:       0,
		},
		{
			name:               "canceled before unbound pod iteration",
			cancelBeforeLookup: true,
			unboundPod:         true,
			wantNodeGets:       0,
		},
		{
			name:         "canceled during Node lookup",
			wantNodeGets: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := slurmReadyTestContext(t, false)
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx.Ctx = runCtx

			clientset := ctx.Clientset.(*k8sfake.Clientset)
			if tt.unboundPod {
				pod, err := clientset.CoreV1().Pods(slinkySlurmNamespace).Get(
					context.Background(), "slinky-nodeset-0", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get NodeSet pod: %v", err)
				}
				pod.Spec.NodeName = ""
				if _, err := clientset.CoreV1().Pods(slinkySlurmNamespace).Update(
					context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update NodeSet pod: %v", err)
				}
			}
			var nodeGets int
			clientset.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				nodeGets++
				cancel()
				return true, nil, context.Canceled
			})
			if tt.cancelBeforeLookup {
				cancel()
			}

			_, err := runnableSlinkyNodeSetPods(ctx, slinkySlurmNamespace)
			if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Fatalf("error = %v, want timeout error", err)
			}
			if err == nil || !strings.Contains(err.Error(), "canceled while resolving NodeSet pod nodes") {
				t.Fatalf("error = %v, want NodeSet cancellation context", err)
			}
			if nodeGets != tt.wantNodeGets {
				t.Fatalf("Node Get calls = %d, want %d", nodeGets, tt.wantNodeGets)
			}
		})
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

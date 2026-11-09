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
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
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
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testRunaiProjectNamespace = "runai-aicr-gang"
	testRunaiQueue            = "aicr-gang"
)

// runaiRecipe is the shape of the shipped Run:ai leaves: runai-cluster
// enabled and AICR's kai-scheduler disabled.
func runaiRecipe() *v1.ValidationInput {
	return validationInputWithRefs(disabledRef(kaiSchedulerName), enabledRef(runaiClusterComponent))
}

func gangContext(input *v1.ValidationInput, objs ...runtime.Object) (*validators.Context, *k8sfake.Clientset) {
	clientset := k8sfake.NewSimpleClientset(objs...)
	return &validators.Context{Ctx: context.Background(), Clientset: clientset, ValidationInput: input}, clientset
}

// TestResolveGangTarget pins how the recipe selects the scheduler under test:
// KAI by default, Run:ai's bundled KAI once runai-cluster is declared, and a
// hard failure (not a Skip) when a Run:ai recipe arrives without its Project
// namespace.
func TestResolveGangTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		input            *v1.ValidationInput
		projectNamespace string
		wantComponent    string
		wantNamespace    string
		wantScheduler    string
		wantQueue        string
		wantTestNS       string
		wantErrCode      errors.ErrorCode
		wantErrContains  string
	}{
		{
			name:          "KAI recipe keeps the KAI target",
			input:         validationInputWithRefs(enabledRef(kaiSchedulerName)),
			wantComponent: kaiSchedulerName, wantNamespace: kaiSchedulerName,
			wantScheduler: kaiSchedulerName, wantQueue: kaiDefaultQueue,
		},
		{
			// The input is only an input on Run:ai; a KAI recipe keeps its
			// per-run namespace even when the variable is set.
			name:             "KAI recipe ignores a Project namespace",
			input:            validationInputWithRefs(enabledRef(kaiSchedulerName)),
			projectNamespace: testRunaiProjectNamespace,
			wantComponent:    kaiSchedulerName, wantNamespace: kaiSchedulerName,
			wantScheduler: kaiSchedulerName, wantQueue: kaiDefaultQueue,
		},
		{
			name:             "Run:ai recipe with the Project namespace",
			input:            runaiRecipe(),
			projectNamespace: testRunaiProjectNamespace,
			wantComponent:    runaiClusterComponent, wantNamespace: runaiNamespace,
			wantScheduler: runaiSchedulerName, wantTestNS: testRunaiProjectNamespace,
		},
		{
			name:             "Run:ai recipe trims the input",
			input:            runaiRecipe(),
			projectNamespace: "  " + testRunaiProjectNamespace + "\n",
			wantComponent:    runaiClusterComponent, wantNamespace: runaiNamespace,
			wantScheduler: runaiSchedulerName, wantTestNS: testRunaiProjectNamespace,
		},
		{
			name:            "Run:ai recipe without the input fails, not skips",
			input:           runaiRecipe(),
			wantErrCode:     errors.ErrCodeInvalidRequest,
			wantErrContains: defaults.EnvRunaiProjectNamespace,
		},
		{
			name:             "Run:ai recipe with a malformed namespace name fails",
			input:            runaiRecipe(),
			projectNamespace: "Runai/Project",
			wantErrCode:      errors.ErrCodeInvalidRequest,
			wantErrContains:  "not a valid namespace name",
		},
		{
			// A disabled runai-cluster is not declared (RecipeDeclares), so the
			// recipe is a plain KAI recipe.
			name:          "runai-cluster disabled falls back to KAI",
			input:         validationInputWithRefs(enabledRef(kaiSchedulerName), disabledRef(runaiClusterComponent)),
			wantComponent: kaiSchedulerName, wantNamespace: kaiSchedulerName,
			wantScheduler: kaiSchedulerName, wantQueue: kaiDefaultQueue,
		},
		{
			// Neither scheduler: the KAI target is returned so the
			// applicability gate can Skip on a clean NotFound.
			name:          "neither scheduler keeps the KAI target",
			input:         validationInputWithRefs(enabledRef("gpu-operator")),
			wantComponent: kaiSchedulerName, wantNamespace: kaiSchedulerName,
			wantScheduler: kaiSchedulerName, wantQueue: kaiDefaultQueue,
		},
		{
			name:          "no recipe context keeps the KAI target",
			input:         nil,
			wantComponent: kaiSchedulerName, wantNamespace: kaiSchedulerName,
			wantScheduler: kaiSchedulerName, wantQueue: kaiDefaultQueue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := &validators.Context{Ctx: context.Background(), ValidationInput: tt.input}
			target, err := resolveGangTarget(ctx, tt.projectNamespace)
			if tt.wantErrCode != "" {
				if err == nil {
					t.Fatalf("expected an error, got target %+v", target)
				}
				if validators.IsSkip(err) {
					t.Fatalf("a Run:ai recipe must fail, not skip: %v", err)
				}
				errorstest.WantReportedCode(t, err, tt.wantErrCode)
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error %q does not mention %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveGangTarget: %v", err)
			}
			if target.component != tt.wantComponent || target.namespace != tt.wantNamespace ||
				target.schedulerName != tt.wantScheduler || target.queue != tt.wantQueue ||
				target.testNamespace != tt.wantTestNS {

				t.Errorf("target = %+v, want component=%s namespace=%s scheduler=%s queue=%q testNamespace=%q",
					target, tt.wantComponent, tt.wantNamespace, tt.wantScheduler, tt.wantQueue, tt.wantTestNS)
			}
			if target.probeDeployment == "" || len(target.deployments) == 0 {
				t.Errorf("target %+v has no deployments to verify", target)
			}
		})
	}
}

// TestResolveRunaiQueue pins the queue lookup: the Project namespace's
// runai/queue label is the queue, and a namespace that is not a Project
// namespace is a configuration error.
func TestResolveRunaiQueue(t *testing.T) {
	t.Parallel()
	namespace := func(labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testRunaiProjectNamespace, Labels: labels}}
	}
	tests := []struct {
		name        string
		objs        []runtime.Object
		forbidden   bool
		wantQueue   string
		wantErrCode errors.ErrorCode
		wantErrHas  string
	}{
		{
			name:      "Project namespace yields its queue",
			objs:      []runtime.Object{namespace(map[string]string{runaiQueueLabel: testRunaiQueue, "project": testRunaiQueue})},
			wantQueue: testRunaiQueue,
		},
		{
			name:        "namespace without the queue label is not a Project namespace",
			objs:        []runtime.Object{namespace(map[string]string{"team": "x"})},
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  runaiQueueLabel,
		},
		{
			name:        "empty queue label is not a Project namespace",
			objs:        []runtime.Object{namespace(map[string]string{runaiQueueLabel: ""})},
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  runaiQueueLabel,
		},
		{
			name:        "missing namespace names the input",
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  defaults.EnvRunaiProjectNamespace,
		},
		{
			// An RBAC denial is not a configuration error on the operator's
			// side and must not be reported as one.
			name:       "forbidden read is not a configuration error",
			objs:       []runtime.Object{namespace(map[string]string{runaiQueueLabel: testRunaiQueue})},
			forbidden:  true,
			wantErrHas: "Project namespace",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clientset := k8sfake.NewSimpleClientset(tt.objs...)
			if tt.forbidden {
				clientset.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, testRunaiProjectNamespace, stderrors.New("rbac denied"))
				})
			}
			queue, err := resolveRunaiQueue(context.Background(), clientset, testRunaiProjectNamespace)
			if tt.wantErrCode == "" && !tt.forbidden {
				if err != nil {
					t.Fatalf("resolveRunaiQueue: %v", err)
				}
				if queue != tt.wantQueue {
					t.Errorf("queue = %q, want %q", queue, tt.wantQueue)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got queue %q", queue)
			}
			if tt.forbidden {
				if errorstest.ReportedCode(err) == errors.ErrCodeInvalidRequest {
					t.Errorf("forbidden read reported as a configuration error: %v", err)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantErrCode)
			}
			if !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErrHas)
			}
		})
	}
}

// TestNewGangTestRunTarget pins what the run takes from the target: the
// per-run namespace it owns on KAI, the Project namespace it does not own on
// Run:ai, and the scheduler name and queue the members and PodGroups carry.
func TestNewGangTestRunTarget(t *testing.T) {
	t.Parallel()
	runai := runaiGangTarget(testRunaiProjectNamespace)
	runai.queue = testRunaiQueue
	tests := []struct {
		name          string
		target        gangTarget
		wantNSPrefix  string
		wantNamespace string
		wantOwns      bool
		wantScheduler string
		wantQueue     string
	}{
		{
			name: "KAI owns a per-run namespace", target: kaiGangTarget(),
			wantNSPrefix: gangTestNSPrefix, wantOwns: true,
			wantScheduler: kaiSchedulerName, wantQueue: kaiDefaultQueue,
		},
		{
			name: "Run:ai borrows the Project namespace", target: runai,
			wantNamespace: testRunaiProjectNamespace, wantOwns: false,
			wantScheduler: runaiSchedulerName, wantQueue: testRunaiQueue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, err := newGangTestRun(tt.target)
			if err != nil {
				t.Fatalf("newGangTestRun: %v", err)
			}
			if tt.wantNamespace != "" && run.namespace != tt.wantNamespace {
				t.Errorf("namespace = %q, want %q", run.namespace, tt.wantNamespace)
			}
			if tt.wantNSPrefix != "" && !strings.HasPrefix(run.namespace, tt.wantNSPrefix) {
				t.Errorf("namespace = %q, want prefix %q", run.namespace, tt.wantNSPrefix)
			}
			if run.ownsNamespace != tt.wantOwns {
				t.Errorf("ownsNamespace = %v, want %v", run.ownsNamespace, tt.wantOwns)
			}
			run.setNegativeShape(gangNegativeShape{Members: 3})
			for i, pod := range []*corev1.Pod{buildGangTestPod(run, 0, nil), buildGangNegativePod(run, 1, nil)} {
				if pod.Spec.SchedulerName != tt.wantScheduler {
					t.Errorf("pod %d schedulerName = %q, want %q", i, pod.Spec.SchedulerName, tt.wantScheduler)
				}
				if pod.Namespace != run.namespace {
					t.Errorf("pod %d namespace = %q, want %q", i, pod.Namespace, run.namespace)
				}
			}
			for _, pg := range []*unstructured.Unstructured{buildPodGroup(run), buildNegativePodGroup(run)} {
				queue, _, _ := unstructured.NestedString(pg.Object, keySpec, "queue")
				if queue != tt.wantQueue {
					t.Errorf("PodGroup %s queue = %q, want %q", pg.GetName(), queue, tt.wantQueue)
				}
				if pg.GetNamespace() != run.namespace {
					t.Errorf("PodGroup %s namespace = %q, want %q", pg.GetName(), pg.GetNamespace(), run.namespace)
				}
			}
		})
	}
}

// TestCleanupGangTestResourcesLeavesRunaiProjectNamespace: the Project
// namespace belongs to the Project. Cleanup removes the run's pods and
// PodGroups from it and leaves the namespace itself in place.
func TestCleanupGangTestResourcesLeavesRunaiProjectNamespace(t *testing.T) {
	t.Parallel()
	target := runaiGangTarget(testRunaiProjectNamespace)
	target.queue = testRunaiQueue
	run, err := newGangTestRun(target)
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	shape, err := sizeGangNegativeShape(homogeneousNodes())
	if err != nil {
		t.Fatalf("sizeGangNegativeShape: %v", err)
	}
	run.setNegativeShape(shape)

	objs := make([]runtime.Object, 0, 1+gangMinMembers+len(run.negPods))
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: run.namespace, Labels: map[string]string{runaiQueueLabel: testRunaiQueue},
	}})
	for i := range gangMinMembers {
		objs = append(objs, buildGangTestPod(run, i, nil))
	}
	for i := range run.negPods {
		objs = append(objs, buildGangNegativePod(run, i, nil))
	}
	clientset := k8sfake.NewSimpleClientset(objs...)
	dynClient := newGangDynamicFake(t)
	for _, pg := range []*unstructured.Unstructured{buildPodGroup(run), buildNegativePodGroup(run)} {
		if _, createErr := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Create(context.Background(), pg, metav1.CreateOptions{}); createErr != nil {
			t.Fatalf("seed PodGroup %s: %v", pg.GetName(), createErr)
		}
	}

	if cleanupErr := cleanupGangTestResources(context.Background(), clientset, dynClient, run); cleanupErr != nil {
		t.Fatalf("cleanupGangTestResources: %v", cleanupErr)
	}
	if _, getErr := clientset.CoreV1().Namespaces().Get(context.Background(), run.namespace, metav1.GetOptions{}); getErr != nil {
		t.Fatalf("Project namespace %s must survive cleanup: %v", run.namespace, getErr)
	}
	pods, err := clientset.CoreV1().Pods(run.namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("%d test pods left in the Project namespace", len(pods.Items))
	}
	for _, group := range []string{run.groupName, run.negGroupName} {
		if _, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Get(context.Background(), group, metav1.GetOptions{}); err == nil {
			t.Errorf("PodGroup %s left in the Project namespace", group)
		}
	}
}

func availableDeployment(namespace, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
}

// TestVerifyGangSchedulerDeployments pins which Deployments prove each
// scheduler is running. On Run:ai the shard Deployments are discovered by
// prefix, and a namespace with the operands but no shard is a missing
// scheduler.
func TestVerifyGangSchedulerDeployments(t *testing.T) {
	t.Parallel()
	deployments := func(namespace string, names ...string) []runtime.Object {
		objs := make([]runtime.Object, 0, len(names))
		for _, name := range names {
			objs = append(objs, availableDeployment(namespace, name))
		}
		return objs
	}
	tests := []struct {
		name        string
		target      gangTarget
		objs        []runtime.Object
		wantErrCode errors.ErrorCode
		wantErrHas  string
	}{
		{
			name:   "KAI: every operand available",
			target: kaiGangTarget(),
			objs:   deployments(kaiSchedulerName, kaiSchedulerDeployments...),
		},
		{
			name:        "KAI: a missing operand fails",
			target:      kaiGangTarget(),
			objs:        deployments(kaiSchedulerName, kaiSchedulerDeployments[:len(kaiSchedulerDeployments)-1]...),
			wantErrCode: errors.ErrCodeNotFound,
			wantErrHas:  kaiSchedulerDeployments[len(kaiSchedulerDeployments)-1],
		},
		{
			name:   "Run:ai: operators, operands and one shard available",
			target: runaiGangTarget(testRunaiProjectNamespace),
			objs: append(deployments(runaiNamespace, runaiSchedulerDeployments...),
				availableDeployment(runaiNamespace, runaiSchedulerName+"-default")),
		},
		{
			name:   "Run:ai: several shards all verified",
			target: runaiGangTarget(testRunaiProjectNamespace),
			objs: append(deployments(runaiNamespace, runaiSchedulerDeployments...),
				availableDeployment(runaiNamespace, runaiSchedulerName+"-default"),
				availableDeployment(runaiNamespace, runaiSchedulerName+"-h100")),
		},
		{
			name:        "Run:ai: no shard Deployment is a missing scheduler",
			target:      runaiGangTarget(testRunaiProjectNamespace),
			objs:        deployments(runaiNamespace, runaiSchedulerDeployments...),
			wantErrCode: errors.ErrCodeNotFound,
			wantErrHas:  runaiSchedulerShardPrefix,
		},
		{
			// The bare scheduler name is not a shard: KAI names shards
			// <scheduler>-<shard>, and a stray Deployment must not pass for one.
			name:   "Run:ai: a Deployment named exactly runai-scheduler is not a shard",
			target: runaiGangTarget(testRunaiProjectNamespace),
			objs: append(deployments(runaiNamespace, runaiSchedulerDeployments...),
				availableDeployment(runaiNamespace, runaiSchedulerName)),
			wantErrCode: errors.ErrCodeNotFound,
			wantErrHas:  runaiSchedulerShardPrefix,
		},
		{
			name:   "Run:ai: a missing KAI operand fails",
			target: runaiGangTarget(testRunaiProjectNamespace),
			objs: append(deployments(runaiNamespace, "runai-operator", runaiKAIOperatorDeployment, "admission", "binder", "pod-grouper", "podgroup-controller"),
				availableDeployment(runaiNamespace, runaiSchedulerName+"-default")),
			wantErrCode: errors.ErrCodeNotFound,
			wantErrHas:  "queue-controller",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vctx, _ := gangContext(nil, tt.objs...)
			err := verifyGangSchedulerDeployments(vctx, tt.target, 500*time.Millisecond)
			if tt.wantErrCode == "" {
				if err != nil {
					t.Fatalf("verifyGangSchedulerDeployments: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			errorstest.WantReportedCode(t, err, tt.wantErrCode)
			if !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErrHas)
			}
		})
	}
}

// TestCheckGangSchedulingApplicability pins the check's verdict before any
// test pod exists: a Run:ai recipe without its Project namespace fails before
// the cluster is even read, a recipe with neither scheduler skips on a clean
// NotFound, and a recipe that declares a scheduler fails when its Deployment
// is absent.
func TestCheckGangSchedulingApplicability(t *testing.T) {
	tests := []struct {
		name             string
		input            *v1.ValidationInput
		projectNamespace string
		objs             []runtime.Object
		wantSkip         bool
		wantErrCode      errors.ErrorCode
		wantErrHas       string
		wantNoReads      bool
	}{
		{
			name:        "Run:ai recipe without the input fails before reading the cluster",
			input:       runaiRecipe(),
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  defaults.EnvRunaiProjectNamespace,
			wantNoReads: true,
		},
		{
			name:             "Run:ai recipe with the input but no Run:ai install fails closed",
			input:            runaiRecipe(),
			projectNamespace: testRunaiProjectNamespace,
			wantErrCode:      errors.ErrCodeNotFound,
			wantErrHas:       runaiNamespace + "/" + runaiKAIOperatorDeployment,
		},
		{
			name:     "neither scheduler declared and none installed skips",
			input:    validationInputWithRefs(enabledRef("gpu-operator")),
			wantSkip: true,
		},
		{
			// The Run:ai target is never probed on a recipe that does not
			// declare runai-cluster, so a Run:ai install alone cannot turn a
			// KAI recipe's missing scheduler into a pass.
			name:        "KAI recipe with only Run:ai installed fails closed",
			input:       validationInputWithRefs(enabledRef(kaiSchedulerName)),
			objs:        []runtime.Object{availableDeployment(runaiNamespace, runaiKAIOperatorDeployment)},
			wantErrCode: errors.ErrCodeNotFound,
			wantErrHas:  kaiSchedulerName + "/" + kaiSchedulerDeployments[0],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(defaults.EnvRunaiProjectNamespace, tt.projectNamespace)
			vctx, clientset := gangContext(tt.input, tt.objs...)
			var reads atomic.Int32
			clientset.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				reads.Add(1)
				return false, nil, nil
			})
			err := CheckGangScheduling(vctx)
			if tt.wantSkip {
				if !validators.IsSkip(err) {
					t.Fatalf("want Skip, got %v", err)
				}
				return
			}
			if err == nil || validators.IsSkip(err) {
				t.Fatalf("want a failure with code %s, got %v", tt.wantErrCode, err)
			}
			errorstest.WantReportedCode(t, err, tt.wantErrCode)
			if !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErrHas)
			}
			if tt.wantNoReads && reads.Load() != 0 {
				t.Errorf("the missing input must fail before any cluster read; saw %d", reads.Load())
			}
		})
	}
}

// TestRunGangPositivePhaseRunaiTarget runs the positive phase in a Run:ai
// Project namespace: the members request runai-scheduler, join the Project's
// queue, and no namespace is created. Forwarded image pull Secrets are
// referenced by the members but not copied into the Project namespace.
func TestRunGangPositivePhaseRunaiTarget(t *testing.T) {
	t.Setenv(validators.ImagePullSecretsEnv, "regcred")
	target := runaiGangTarget(testRunaiProjectNamespace)
	target.queue = testRunaiQueue
	run, err := newGangTestRun(target)
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	clientset := k8sfake.NewSimpleClientset()
	completePositivePodOnCreate(clientset, "node-a", time.Now())
	dynClient := newGangDynamicFake(t)
	vctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

	if _, phaseErr := runGangPositivePhase(vctx, dynClient, run); phaseErr != nil {
		t.Fatalf("runGangPositivePhase: %v", phaseErr)
	}
	pg, err := dynClient.Resource(podGroupGVR).Namespace(testRunaiProjectNamespace).Get(context.Background(), run.groupName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PodGroup not created in the Project namespace: %v", err)
	}
	if queue, _, _ := unstructured.NestedString(pg.Object, keySpec, "queue"); queue != testRunaiQueue {
		t.Errorf("PodGroup queue = %q, want the Project's queue aicr-gang", queue)
	}
	for i := range gangMinMembers {
		pod, err := clientset.CoreV1().Pods(testRunaiProjectNamespace).Get(context.Background(), run.pods[i], metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", run.pods[i], err)
		}
		if pod.Spec.SchedulerName != runaiSchedulerName {
			t.Errorf("%s schedulerName = %q, want %q", pod.Name, pod.Spec.SchedulerName, runaiSchedulerName)
		}
		if refs := pod.Spec.ImagePullSecrets; len(refs) != 1 || refs[0].Name != "regcred" {
			t.Errorf("%s imagePullSecrets = %v, want the forwarded regcred reference", pod.Name, refs)
		}
	}
	secrets, err := clientset.CoreV1().Secrets(testRunaiProjectNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("no Secret may be copied into the Project namespace; found %d", len(secrets.Items))
	}
	if _, err := clientset.CoreV1().Namespaces().Get(context.Background(), testRunaiProjectNamespace, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("the check must not create the Project namespace; Get returned %v", err)
	}
}

// Compile-time guard that the recipe helper types used above stay the ones
// the validator reads.
var _ = recipe.ComponentRef{}

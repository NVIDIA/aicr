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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
			objs:      []runtime.Object{namespace(map[string]string{labels.RunAIQueue: testRunaiQueue, "project": testRunaiQueue})},
			wantQueue: testRunaiQueue,
		},
		{
			name:        "namespace without the queue label is not a Project namespace",
			objs:        []runtime.Object{namespace(map[string]string{"team": "x"})},
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  labels.RunAIQueue,
		},
		{
			name:        "empty queue label is not a Project namespace",
			objs:        []runtime.Object{namespace(map[string]string{labels.RunAIQueue: ""})},
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  labels.RunAIQueue,
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
			objs:       []runtime.Object{namespace(map[string]string{labels.RunAIQueue: testRunaiQueue})},
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
				if got := pod.Labels[labels.GPUTestRun]; got != run.suffix {
					t.Errorf("pod %d label %s = %q, want the run suffix %q", i, labels.GPUTestRun, got, run.suffix)
				}
				if got := pod.Spec.TerminationGracePeriodSeconds; got == nil || *got != gangTerminationGracePeriod {
					t.Errorf("pod %d terminationGracePeriodSeconds = %s, want %d", i, formatGracePeriod(got), gangTerminationGracePeriod)
				}
			}
			wantPhase := []string{gangPhasePositive, gangPhaseNegative}
			for i, pg := range []*unstructured.Unstructured{buildPodGroup(run), buildNegativePodGroup(run)} {
				queue, _, _ := unstructured.NestedString(pg.Object, keySpec, "queue")
				if queue != tt.wantQueue {
					t.Errorf("PodGroup %s queue = %q, want %q", pg.GetName(), queue, tt.wantQueue)
				}
				if pg.GetNamespace() != run.namespace {
					t.Errorf("PodGroup %s namespace = %q, want %q", pg.GetName(), pg.GetNamespace(), run.namespace)
				}
				if got := pg.GetLabels()[labels.GangSchedulingPhase]; got != wantPhase[i] {
					t.Errorf("PodGroup %s label %s = %q, want %q", pg.GetName(), labels.GangSchedulingPhase, got, wantPhase[i])
				}
				if got := pg.GetLabels()[labels.GPUTestRun]; got != run.suffix {
					t.Errorf("PodGroup %s label %s = %q, want the run suffix %q", pg.GetName(), labels.GPUTestRun, got, run.suffix)
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
		Name: run.namespace, Labels: map[string]string{labels.RunAIQueue: testRunaiQueue},
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

// runaiCleanupRun returns a Run:ai-target run with two negative members.
func runaiCleanupRun(t *testing.T) *gangTestRun {
	t.Helper()
	target := runaiGangTarget(testRunaiProjectNamespace)
	target.queue = testRunaiQueue
	run, err := newGangTestRun(target)
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	run.setNegativeShape(gangNegativeShape{Members: 2})
	return run
}

// TestConfirmGangResourcesGone: in a Project namespace nothing subsumes the
// pod and PodGroup deletes, so a resource that outlives them is reported, and
// only an accepted deletion still Terminating is tolerated.
func TestConfirmGangResourcesGone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		seedPod     func(run *gangTestRun) *corev1.Pod
		seedGroup   bool
		failPodGets bool
		canceled    bool
		wantErrHas  []string
	}{
		{name: "nothing left"},
		{
			name:       "pod left",
			seedPod:    func(run *gangTestRun) *corev1.Pod { return buildGangNegativePod(run, 1, nil) },
			wantErrHas: []string{"left in namespace " + testRunaiProjectNamespace, "pod/gang-neg-worker-"},
		},
		{
			name:       "PodGroup left",
			seedGroup:  true,
			wantErrHas: []string{"left in namespace " + testRunaiProjectNamespace, "podgroup/gang-neg-group-"},
		},
		{
			name: "pod Terminating is an accepted deletion",
			seedPod: func(run *gangTestRun) *corev1.Pod {
				pod := buildGangTestPod(run, 0, nil)
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"example.com/hold"}
				return pod
			},
		},
		{
			name:        "pod unreadable",
			failPodGets: true,
			wantErrHas:  []string{"pod/gang-worker-", "unreadable"},
		},
		{
			name:       "budget spent before any read",
			canceled:   true,
			wantErrHas: []string{"could not be checked"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := runaiCleanupRun(t)
			var objs []runtime.Object
			if tt.seedPod != nil {
				objs = append(objs, tt.seedPod(run))
			}
			clientset := k8sfake.NewSimpleClientset(objs...)
			if tt.failPodGets {
				clientset.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", stderrors.New("denied"))
				})
			}
			dynClient := newGangDynamicFake(t)
			if tt.seedGroup {
				if _, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Create(
					context.Background(), buildNegativePodGroup(run), metav1.CreateOptions{}); err != nil {
					t.Fatalf("seed PodGroup: %v", err)
				}
			}
			podNames := append(append([]string{}, run.pods[:]...), run.negPods...)
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			if tt.canceled {
				cancel()
			}

			err := confirmGangResourcesGone(ctx, clientset, dynClient, run, podNames)
			if len(tt.wantErrHas) == 0 {
				if err != nil {
					t.Fatalf("confirmGangResourcesGone() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("confirmGangResourcesGone() = nil, want an error mentioning %q", tt.wantErrHas)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			for _, want := range tt.wantErrHas {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// TestGangCleanupArtifact: a failed cleanup is never recorded as a success,
// and a Project namespace is never described as a namespace deletion.
func TestGangCleanupArtifact(t *testing.T) {
	t.Parallel()
	kaiRun, err := newGangTestRun(kaiGangTarget())
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	runaiRun := runaiCleanupRun(t)
	cleanupErr := errors.New(errors.ErrCodeInternal, "boom")
	projectEquivalent := "kubectl delete pods,podgroups.scheduling.run.ai -n " + testRunaiProjectNamespace +
		" -l " + labels.GPUTestRun + "=" + runaiRun.suffix
	const (
		ownedTitle   = "Delete test namespace"
		projectTitle = "Delete test pods and PodGroups"
	)
	tests := []struct {
		name           string
		run            *gangTestRun
		cleanupErr     error
		wantTitle      string
		wantEquivalent string
		wantResultHas  []string
		wantResultNot  []string
	}{
		{
			name:           "owned namespace deleted",
			run:            kaiRun,
			wantTitle:      ownedTitle,
			wantEquivalent: "kubectl delete namespace " + kaiRun.namespace,
			wantResultHas:  []string{"Deleted gang test pods, PodGroups, and the " + kaiRun.namespace + " namespace."},
			wantResultNot:  []string{"FAILED"},
		},
		{
			name:           "owned namespace deletion failed",
			run:            kaiRun,
			cleanupErr:     cleanupErr,
			wantTitle:      ownedTitle,
			wantEquivalent: "kubectl delete namespace " + kaiRun.namespace,
			wantResultHas:  []string{"namespace deletion FAILED", "tools/cleanup", "boom"},
		},
		{
			name:           "Project namespace cleaned",
			run:            runaiRun,
			wantTitle:      projectTitle,
			wantEquivalent: projectEquivalent,
			wantResultHas:  []string{"Deleted gang test pods and PodGroups", "left in place"},
			wantResultNot:  []string{"FAILED"},
		},
		{
			name:           "Project namespace residue",
			run:            runaiRun,
			cleanupErr:     cleanupErr,
			wantTitle:      projectTitle,
			wantEquivalent: projectEquivalent,
			wantResultHas:  []string{"Run:ai Project namespace " + testRunaiProjectNamespace + " FAILED", "boom"},
			wantResultNot:  []string{"namespace deletion", "tools/cleanup", "Deleted gang test pods"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			title, equivalent, result := gangCleanupArtifact(tt.run, tt.cleanupErr)
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if !strings.HasPrefix(equivalent, tt.wantEquivalent) {
				t.Errorf("equivalent = %q, want prefix %q", equivalent, tt.wantEquivalent)
			}
			for _, want := range tt.wantResultHas {
				if !strings.Contains(result, want) {
					t.Errorf("result %q does not mention %q", result, want)
				}
			}
			for _, not := range tt.wantResultNot {
				if strings.Contains(result, not) {
					t.Errorf("result %q must not mention %q", result, not)
				}
			}
		})
	}
}

// ctxCheckingDynamic fails a PodGroup delete issued on a context that has
// already ended, as client-go's REST client does and the fake does not.
type ctxCheckingDynamic struct{ dynamic.Interface }

func (d ctxCheckingDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return ctxCheckingResource{d.Interface.Resource(gvr)}
}

type ctxCheckingResource struct {
	dynamic.NamespaceableResourceInterface
}

func (r ctxCheckingResource) Namespace(ns string) dynamic.ResourceInterface {
	return ctxCheckingNamespaced{r.NamespaceableResourceInterface.Namespace(ns)}
}

type ctxCheckingNamespaced struct{ dynamic.ResourceInterface }

func (r ctxCheckingNamespaced) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.ResourceInterface.Delete(ctx, name, opts, subresources...)
}

// TestCleanupGangTestResourcesDeletesPodGroupsOnSpentContext: the pod wait can
// use up the caller's cleanup context, and in a Project namespace nothing but
// these deletes removes the PodGroups, so they must not ride on that context.
func TestCleanupGangTestResourcesDeletesPodGroupsOnSpentContext(t *testing.T) {
	t.Parallel()
	run := runaiCleanupRun(t)
	clientset := k8sfake.NewSimpleClientset()
	fake := newGangDynamicFake(t)
	for _, pg := range []*unstructured.Unstructured{buildPodGroup(run), buildNegativePodGroup(run)} {
		if _, err := fake.Resource(podGroupGVR).Namespace(run.namespace).Create(context.Background(), pg, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed PodGroup %s: %v", pg.GetName(), err)
		}
	}
	spent, cancel := context.WithCancel(context.Background())
	cancel()

	if err := cleanupGangTestResources(spent, clientset, ctxCheckingDynamic{fake}, run); err != nil {
		t.Fatalf("cleanupGangTestResources on a spent context: %v", err)
	}
	for _, group := range []string{run.groupName, run.negGroupName} {
		if _, err := fake.Resource(podGroupGVR).Namespace(run.namespace).Get(context.Background(), group, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
			t.Errorf("PodGroup %s left in the Project namespace (Get returned %v)", group, err)
		}
	}
}

// TestGangNegativeWindowIgnoresOtherRunsInProject: a Project namespace is
// shared, so another validation's negative members, or the Pending ones an
// interrupted run left behind, sit beside this run's. They carry the same
// phase label; only the run label keeps them out of this run's window.
func TestGangNegativeWindowIgnoresOtherRunsInProject(t *testing.T) {
	t.Parallel()
	other, mine := runaiCleanupRun(t), runaiCleanupRun(t)
	if other.suffix == mine.suffix {
		t.Fatal("two runs drew the same suffix")
	}
	objs := make([]runtime.Object, 0, len(other.negPods)+len(mine.negPods))
	for i := range other.negPods {
		pod := buildGangNegativePod(other, i, nil)
		pod.Status.Phase = corev1.PodPending
		objs = append(objs, pod)
	}
	// A member of the other run that bound must not fail this one either.
	objs[0].(*corev1.Pod).Spec.NodeName = "node-a"
	for i := range mine.negPods {
		pod := buildGangNegativePod(mine, i, nil)
		pod.Status.Phase = corev1.PodPending
		objs = append(objs, pod)
	}
	clientset := k8sfake.NewSimpleClientset(objs...)
	const window = 300 * time.Millisecond

	obs, err := holdGangNegativeWindow(context.Background(), clientset, mine.namespace,
		mine.phaseSelector(gangPhaseNegative), mine.negPods, window)
	if err != nil {
		t.Fatalf("this run's window failed on another run's pods: %v", err)
	}
	if len(obs.Pods) != len(mine.negPods) {
		t.Errorf("observed %d pods, want this run's %d", len(obs.Pods), len(mine.negPods))
	}

	// The phase label alone sees both runs, which is the failure this guards.
	if _, err := holdGangNegativeWindow(context.Background(), clientset, mine.namespace,
		gangPhaseSelector(gangPhaseNegative), mine.negPods, window); err == nil {
		t.Error("a phase-only selector passed with another run's pods present; the fixture no longer reproduces the collision")
	}
}

// runaiCheckFixture is a cluster on which CheckGangScheduling reaches its
// functional test on the Run:ai target: the scheduler Deployments and a shard
// in runai, the two CRDs, and a forwarded image pull Secret in the validator
// namespace. project, when non-nil, is the Project namespace.
func runaiCheckFixture(t *testing.T, project *corev1.Namespace) (*validators.Context, *k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	t.Setenv(defaults.EnvRunaiProjectNamespace, testRunaiProjectNamespace)
	t.Setenv(validators.ImagePullSecretsEnv, "regcred")

	objs := []runtime.Object{
		availableDeployment(runaiNamespace, runaiSchedulerShardPrefix+"default"),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "regcred", Namespace: validators.ValidatorNamespace()},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
		},
	}
	for _, name := range runaiSchedulerDeployments {
		objs = append(objs, availableDeployment(runaiNamespace, name))
	}
	if project != nil {
		objs = append(objs, project)
	}
	vctx, clientset := gangContext(runaiRecipe(), objs...)

	crdGVR := schema.GroupVersionResource{Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs}
	crd := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			keyAPIVersion: apiGroupAPIExtensions + "/v1",
			keyKind:       "CustomResourceDefinition",
			keyMetadata:   map[string]any{keyName: name},
		}}
	}
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			podGroupGVR: "PodGroupList",
			crdGVR:      "CustomResourceDefinitionList",
		},
		crd("queues.scheduling.run.ai"), crd("podgroups.scheduling.run.ai"))
	vctx.DynamicClient = dynClient
	return vctx, clientset, dynClient
}

// TestCheckGangSchedulingRunaiProject drives the whole check on the Run:ai
// target up to its first test pod, which the fixture refuses so the run ends
// there. By then the check has decided everything that differs in a Project
// namespace it does not own: it reads the queue from the namespace, creates no
// namespace, and copies no image pull Secret where the Project's users could
// read it or where one they pre-created would be overwritten.
func TestCheckGangSchedulingRunaiProject(t *testing.T) {
	labelled := func(labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testRunaiProjectNamespace, Labels: labels}}
	}
	tests := []struct {
		name          string
		project       *corev1.Namespace
		wantErrCode   errors.ErrorCode
		wantErrHas    string
		wantPodGroups int
	}{
		{
			name:          "Project namespace: queue from its label, nothing created around it",
			project:       labelled(map[string]string{labels.RunAIQueue: testRunaiQueue}),
			wantErrCode:   errors.ErrCodeInternal,
			wantErrHas:    "positive phase failed",
			wantPodGroups: 1,
		},
		{
			name:        "Project namespace missing",
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  "not found",
		},
		{
			name:        "namespace without the queue label",
			project:     labelled(nil),
			wantErrCode: errors.ErrCodeInvalidRequest,
			wantErrHas:  labels.RunAIQueue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vctx, clientset, dynClient := runaiCheckFixture(t, tt.project)
			clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, stderrors.New("pod creation refused by the fixture")
			})
			var created []*unstructured.Unstructured
			dynClient.PrependReactor("create", "podgroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if create, ok := action.(k8stesting.CreateAction); ok {
					if pg, ok := create.GetObject().(*unstructured.Unstructured); ok {
						created = append(created, pg.DeepCopy())
					}
				}
				return false, nil, nil
			})

			err := CheckGangScheduling(vctx)
			if err == nil || validators.IsSkip(err) {
				t.Fatalf("CheckGangScheduling() = %v, want a failure with code %s", err, tt.wantErrCode)
			}
			errorstest.WantReportedCode(t, err, tt.wantErrCode)
			if !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErrHas)
			}

			if len(created) != tt.wantPodGroups {
				t.Fatalf("PodGroups created = %d, want %d", len(created), tt.wantPodGroups)
			}
			for _, pg := range created {
				if pg.GetNamespace() != testRunaiProjectNamespace {
					t.Errorf("PodGroup %s created in %q, want the Project namespace", pg.GetName(), pg.GetNamespace())
				}
				if queue, _, _ := unstructured.NestedString(pg.Object, keySpec, "queue"); queue != testRunaiQueue {
					t.Errorf("PodGroup %s queue = %q, want %q from the namespace's %s label", pg.GetName(), queue, testRunaiQueue, labels.RunAIQueue)
				}
			}
			for _, action := range clientset.Actions() {
				if action.GetVerb() != "create" && action.GetVerb() != "update" {
					continue
				}
				switch action.GetResource().Resource {
				case "namespaces":
					t.Errorf("the check issued %s on a namespace; it must not create the Project namespace", action.GetVerb())
				case "secrets":
					t.Errorf("the check issued %s on a Secret in %q; image pull Secrets must not be copied into the Project namespace",
						action.GetVerb(), action.GetNamespace())
				}
			}
			secrets, listErr := clientset.CoreV1().Secrets(testRunaiProjectNamespace).List(context.Background(), metav1.ListOptions{})
			if listErr != nil {
				t.Fatalf("list Secrets: %v", listErr)
			}
			if len(secrets.Items) != 0 {
				t.Errorf("%d Secret(s) in the Project namespace, want none", len(secrets.Items))
			}
		})
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
// queue, and reference the forwarded image pull Secrets. That the check
// neither creates the namespace nor copies those Secrets into it is asserted
// where that is decided, in TestCheckGangSchedulingRunaiProject.
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
}

func formatGracePeriod(p *int64) string {
	if p == nil {
		return "unset"
	}
	return strconv.FormatInt(*p, 10)
}

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
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/validators"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// gangTestNode builds a Node with the given allocatable CPU; ready and cordoned
// shape the Ready condition and spec.unschedulable.
func gangTestNode(name, cpu string, ready, cordoned bool) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
	if cpu != "" {
		node.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
	}
	return node
}

// gb300LikeNodes mirrors the live EKS GB300 conformance cluster: two 139-CPU
// GPU nodes and five small system nodes.
func gb300LikeNodes() []corev1.Node {
	return []corev1.Node{
		gangTestNode("gpu-0", "139580m", true, false),
		gangTestNode("gpu-1", "139580m", true, false),
		gangTestNode("sys-0", "3920m", true, false),
		gangTestNode("sys-1", "3920m", true, false),
		gangTestNode("sys-2", "3920m", true, false),
		gangTestNode("sys-3", "7910m", true, false),
		gangTestNode("sys-4", "7910m", true, false),
	}
}

func TestSizeGangNegativeShape(t *testing.T) {
	manyNodes := make([]corev1.Node, 0, 30)
	for i := range 30 {
		manyNodes = append(manyNodes, gangTestNode(fmt.Sprintf("n-%d", i), "64", true, false))
	}

	tests := []struct {
		name            string
		nodes           []corev1.Node
		wantMembers     int
		wantSchedulable int
		wantErrCode     errors.ErrorCode
	}{
		{
			name:            "heterogeneous cluster: nodes+1 members, one member fits a big node",
			nodes:           gb300LikeNodes(),
			wantMembers:     8,
			wantSchedulable: 7,
		},
		{
			name: "cordoned, NotReady and CPU-less nodes are excluded",
			nodes: []corev1.Node{
				gangTestNode("ready", "16", true, false),
				gangTestNode("cordoned", "16", true, true),
				gangTestNode("notready", "16", false, false),
				gangTestNode("nocpu", "", true, false),
			},
			wantMembers:     2,
			wantSchedulable: 1,
		},
		{
			name:            "member count is capped on large clusters",
			nodes:           manyNodes,
			wantMembers:     gangNegativeMaxMembers,
			wantSchedulable: 30,
		},
		{
			name:        "no schedulable node fails closed",
			nodes:       []corev1.Node{gangTestNode("cordoned", "16", true, true)},
			wantErrCode: errors.ErrCodeUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape, err := sizeGangNegativeShape(tt.nodes)
			if tt.wantErrCode != "" {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				errorstest.WantReportedCode(t, err, tt.wantErrCode)
				return
			}
			if err != nil {
				t.Fatalf("sizeGangNegativeShape: %v", err)
			}
			if shape.Members != tt.wantMembers {
				t.Errorf("Members = %d, want %d", shape.Members, tt.wantMembers)
			}
			if shape.SchedulableNodes != tt.wantSchedulable {
				t.Errorf("SchedulableNodes = %d, want %d", shape.SchedulableNodes, tt.wantSchedulable)
			}
			// The group must not fit even if every node were fully idle.
			if total := totalCPU(shape); total.Cmp(shape.AllocatableCPU) <= 0 {
				t.Errorf("total demand %s does not exceed allocatable %s", total.String(), shape.AllocatableCPU.String())
			}
			// The shape is tight: one member fewer demands the summed capacity
			// within the +1m per-member rounding, which is what distinguishes
			// all-or-nothing from a group no scheduler could ever place.
			oneLess := shape.CPUPerMember.MilliValue() * int64(shape.Members-1)
			if slack := oneLess - shape.AllocatableCPU.MilliValue(); slack < 0 || slack > int64(shape.Members-1) {
				t.Errorf("members-1 demand %dm vs allocatable %dm: slack %dm outside [0, %d]; shape is not tight",
					oneLess, shape.AllocatableCPU.MilliValue(), slack, shape.Members-1)
			}
			if got := shape.MemoryPerMember.String(); got != gangWorkerMemoryRequest {
				t.Errorf("MemoryPerMember = %s, want %s", got, gangWorkerMemoryRequest)
			}
		})
	}

	// On the heterogeneous cluster a single member fits the largest node, so
	// bound-pod detection is available there (KAI reports "found for k pods").
	shape, err := sizeGangNegativeShape(gb300LikeNodes())
	if err != nil {
		t.Fatalf("sizeGangNegativeShape: %v", err)
	}
	if big := resource.MustParse("139580m"); shape.CPUPerMember.Cmp(big) > 0 {
		t.Errorf("CPUPerMember %s exceeds the largest node %s; no member could fit", shape.CPUPerMember.String(), big.String())
	}

	// On a homogeneous cluster every member exceeds every node: no scheduler
	// binds anything, and the phase rests on the refusal condition alone.
	shape, err = sizeGangNegativeShape(homogeneousNodes())
	if err != nil {
		t.Fatalf("sizeGangNegativeShape(homogeneous): %v", err)
	}
	for _, node := range homogeneousNodes() {
		if cpu := node.Status.Allocatable[corev1.ResourceCPU]; shape.CPUPerMember.Cmp(cpu) <= 0 {
			t.Errorf("homogeneous: CPUPerMember %s fits node %s (%s); expected every member to exceed every node", shape.CPUPerMember.String(), node.Name, cpu.String())
		}
	}
}

// homogeneousNodes is four identical 64-CPU nodes: per-member CPU becomes
// 64001m, above any node, so bound-pod detection cannot fire.
func homogeneousNodes() []corev1.Node {
	return []corev1.Node{
		gangTestNode("eq-0", "64", true, false),
		gangTestNode("eq-1", "64", true, false),
		gangTestNode("eq-2", "64", true, false),
		gangTestNode("eq-3", "64", true, false),
	}
}

func podGroupWithConditions(name string, conditions []any) *unstructured.Unstructured {
	obj := map[string]any{
		keyAPIVersion: "scheduling.run.ai/v2alpha2",
		keyKind:       "PodGroup",
		keyMetadata:   map[string]any{keyName: name, keyNamespace: "ns"},
		keySpec:       map[string]any{"minMember": int64(8), "queue": "default-queue"},
	}
	if conditions != nil {
		obj["status"] = map[string]any{"schedulingConditions": conditions}
	}
	return &unstructured.Unstructured{Object: obj}
}

const kaiRefusalEight = "PodSchedulingErrors: Resources were found for 6 pods while 8 are required for gang scheduling. " +
	"Additional pods cannot be scheduled due to: no nodes with enough resources were found"

func TestEvaluateGangRefusal(t *testing.T) {
	tests := []struct {
		name       string
		conditions []any
		minMember  int
		wantMsg    string
		wantErr    string
	}{
		{
			name: "deprecated message field carries the refusal",
			conditions: []any{map[string]any{
				"type": "UnschedulableOnNodePool", "status": "True", "reason": "Unschedulable",
				"message": kaiRefusalEight,
			}},
			minMember: 8,
			wantMsg:   kaiRefusalEight,
		},
		{
			name: "reasons list carries the refusal when message is absent",
			conditions: []any{map[string]any{
				"type": "UnschedulableOnNodePool", "status": "True",
				"reasons": []any{map[string]any{"reason": "PodSchedulingErrors", "message": kaiRefusalEight}},
			}},
			minMember: 8,
			wantMsg:   kaiRefusalEight,
		},
		{
			name: "refusal for a different group size is not this group's refusal",
			conditions: []any{map[string]any{
				"type": "UnschedulableOnNodePool", "status": "True", "message": kaiRefusalEight,
			}},
			minMember: 2,
			wantErr:   "not credited",
		},
		{
			name: "generic fit error without gang semantics is not credited",
			conditions: []any{map[string]any{
				"type": "UnschedulableOnNodePool", "status": "True",
				"message": "PodSchedulingErrors: Resources were not found for pod ns/p due to: no nodes with enough resources",
			}},
			minMember: 8,
			wantErr:   "not credited",
		},
		{
			name: "a matching message with no status is not credited",
			conditions: []any{map[string]any{
				"type": "UnschedulableOnNodePool", "message": kaiRefusalEight,
			}},
			minMember: 8,
			wantErr:   "not credited",
		},
		{
			name: "a False condition is stale and not credited",
			conditions: []any{map[string]any{
				"type": "UnschedulableOnNodePool", "status": "False", "message": kaiRefusalEight,
			}},
			minMember: 8,
			wantErr:   "not credited",
		},
		{
			name:       "no schedulingConditions (stalled or absent scheduler) is not credited",
			conditions: nil,
			minMember:  8,
			wantErr:    "no schedulingConditions",
		},
		{
			name:       "empty schedulingConditions is not credited",
			conditions: []any{},
			minMember:  8,
			wantErr:    "no schedulingConditions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := evaluateGangRefusal(podGroupWithConditions("pg", tt.conditions), tt.minMember)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (msg=%q)", tt.wantErr, msg)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg != tt.wantMsg {
				t.Errorf("msg = %q, want %q", msg, tt.wantMsg)
			}
		})
	}
}

func TestGangBoundPodViolation(t *testing.T) {
	tests := []struct {
		name     string
		pod      corev1.Pod
		wantPart string
	}{
		{"pending and unbound", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, ""},
		{"bound but still pending", corev1.Pod{Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}, "bound to node n1"},
		{"bound and running", corev1.Pod{Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, "bound to node n1 and Running"},
		{"running without nodeName", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, "Running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gangBoundPodViolation(&tt.pod)
			if tt.wantPart == "" {
				if got != "" {
					t.Errorf("violation = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantPart) {
				t.Errorf("violation = %q, want it to contain %q", got, tt.wantPart)
			}
		})
	}
}

func negativeMemberPod(ns, name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{gangPhaseLabel: gangPhaseNegative},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestHoldGangNegativeWindow(t *testing.T) {
	const ns = "gang-ns"
	const window = 400 * time.Millisecond

	tests := []struct {
		name     string
		pods     []runtime.Object
		expected []string // defaults to neg-0, neg-1
		// bindMidWindow, when set, binds that pod via Update shortly after the
		// watch starts: this is the "gang scheduling disabled" control, where
		// the default scheduler places members one by one.
		bindMidWindow string
		// deleteMidWindow, when set, deletes that pod shortly after the watch
		// starts: a vanished member must fail, not pass by absence.
		deleteMidWindow string
		wantErr         string
		wantFast        bool // the failure must arrive before the window elapses
	}{
		{
			name: "all members stay unbound for the whole window",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodPending),
				negativeMemberPod(ns, "neg-1", "", corev1.PodPending),
			},
		},
		{
			name: "a member deleted mid-window fails by name",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodPending),
				negativeMemberPod(ns, "neg-1", "", corev1.PodPending),
			},
			deleteMidWindow: "neg-1",
			wantErr:         "member neg-1 was deleted during the verification window",
			wantFast:        true,
		},
		{
			name: "a member missing from the entry list fails by name",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodPending),
			},
			wantErr:  "member neg-1 is missing (1 of 2 members present)",
			wantFast: true,
		},
		{
			name: "a pod the phase did not create fails by name",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodPending),
				negativeMemberPod(ns, "neg-1", "", corev1.PodPending),
				negativeMemberPod(ns, "stray", "", corev1.PodPending),
			},
			wantErr:  "unexpected pod stray",
			wantFast: true,
		},
		{
			name: "a member bound before the window fails by name",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodPending),
				negativeMemberPod(ns, "neg-1", "node-a", corev1.PodPending),
			},
			wantErr:  "neg-1 is bound to node node-a",
			wantFast: true,
		},
		{
			name: "a Running member fails by name",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodRunning),
			},
			wantErr:  "neg-0 is Running",
			wantFast: true,
		},
		{
			name: "gang scheduling disabled: a member binding mid-window fails by name",
			pods: []runtime.Object{
				negativeMemberPod(ns, "neg-0", "", corev1.PodPending),
				negativeMemberPod(ns, "neg-1", "", corev1.PodPending),
			},
			bindMidWindow: "neg-1",
			wantErr:       "neg-1 is bound to node node-b",
			wantFast:      true,
		},
		{
			name:     "no members expected is vacuous and fails",
			pods:     nil,
			expected: []string{},
			wantErr:  "vacuous",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewSimpleClientset(tt.pods...)
			if tt.bindMidWindow != "" {
				go func() {
					time.Sleep(window / 4)
					pod, err := clientset.CoreV1().Pods(ns).Get(context.Background(), tt.bindMidWindow, metav1.GetOptions{})
					if err != nil {
						t.Errorf("get %s: %v", tt.bindMidWindow, err)
						return
					}
					pod.Spec.NodeName = "node-b"
					if _, err := clientset.CoreV1().Pods(ns).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
						t.Errorf("update %s: %v", tt.bindMidWindow, err)
					}
				}()
			}

			if tt.deleteMidWindow != "" {
				go func() {
					time.Sleep(window / 4)
					if err := clientset.CoreV1().Pods(ns).Delete(context.Background(), tt.deleteMidWindow, metav1.DeleteOptions{}); err != nil {
						t.Errorf("delete %s: %v", tt.deleteMidWindow, err)
					}
				}()
			}

			expected := tt.expected
			if expected == nil {
				expected = []string{"neg-0", "neg-1"}
			}
			start := time.Now()
			obs, err := holdGangNegativeWindow(context.Background(), clientset, ns, gangPhaseSelector(gangPhaseNegative), expected, window)
			elapsed := time.Since(start)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if elapsed < window {
					t.Errorf("window returned after %s, before the %s window elapsed", elapsed, window)
				}
				if len(obs.Pods) != len(tt.pods) {
					t.Errorf("observed %d pods, want %d", len(obs.Pods), len(tt.pods))
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if tt.wantFast && elapsed >= window {
				t.Errorf("a bound member must fail the window as soon as it is seen; took %s (window %s)", elapsed, window)
			}
		})
	}
}

func TestHoldGangNegativeWindowParentCanceled(t *testing.T) {
	const ns = "gang-ns"
	clientset := k8sfake.NewSimpleClientset(negativeMemberPod(ns, "neg-0", "", corev1.PodPending))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := holdGangNegativeWindow(ctx, clientset, ns, gangPhaseSelector(gangPhaseNegative), []string{"neg-0"}, 10*time.Second)
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if !strings.Contains(err.Error(), "canceled") {
		t.Errorf("want caller-canceled diagnostic, got %v", err)
	}
}

// completePositivePodOnCreate makes the fake cluster behave like a healthy
// gang scheduler for the positive case: each created positive member is stored
// already bound, scheduled at scheduledAt, and Succeeded. nodeName empty
// stores the pod Succeeded but unbound, which the check must reject.
func completePositivePodOnCreate(clientset *k8sfake.Clientset, nodeName string, scheduledAt time.Time) {
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := create.GetObject().(*corev1.Pod)
		if !ok || pod.Labels[gangPhaseLabel] != gangPhasePositive {
			return false, nil, nil
		}
		pod.Spec.NodeName = nodeName
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionTrue,
			LastTransitionTime: metav1.NewTime(scheduledAt),
		}}
		return false, nil, nil // fall through: the tracker stores the mutated pod
	})
}

// bindNegativePodOnCreate simulates a scheduler without gang semantics that
// binds non-fitting members individually.
func bindNegativePodOnCreate(clientset *k8sfake.Clientset, nodeName string) {
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := create.GetObject().(*corev1.Pod)
		if !ok || pod.Labels[gangPhaseLabel] != gangPhaseNegative {
			return false, nil, nil
		}
		pod.Spec.NodeName = nodeName
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
}

func newGangDynamicFake(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podGroupGVR: "PodGroupList"},
	)
}

// refuseGangOnCreate makes the fake KAI write its gang refusal to every
// PodGroup as it is created, naming that group's minMember.
func refuseGangOnCreate(dynClient *dynamicfake.FakeDynamicClient) {
	dynClient.PrependReactor("create", "podgroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pg, ok := create.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		minMember, _, _ := unstructured.NestedInt64(pg.Object, keySpec, "minMember")
		msg := fmt.Sprintf("PodSchedulingErrors: Resources were found for %d pods while %d are required for gang scheduling. "+
			"Additional pods cannot be scheduled due to: no nodes with enough resources were found", minMember-2, minMember)
		pg.Object["status"] = map[string]any{"schedulingConditions": []any{map[string]any{
			"type": "UnschedulableOnNodePool", "status": "True", "reason": "Unschedulable", "message": msg,
			"reasons": []any{map[string]any{"reason": "PodSchedulingErrors", "message": msg}},
		}}}
		return false, nil, nil
	})
}

func TestRunGangPositivePhase(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		nodeName string
		wantErr  string
	}{
		{name: "both members Succeeded and bound pass", nodeName: "node-a"},
		{name: "a Succeeded member without a node assignment fails", nodeName: "", wantErr: "no node assignment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, err := newGangTestRun()
			if err != nil {
				t.Fatalf("newGangTestRun: %v", err)
			}
			clientset := k8sfake.NewSimpleClientset()
			completePositivePodOnCreate(clientset, tt.nodeName, now)
			dynClient := newGangDynamicFake(t)
			vctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

			report, err := runGangPositivePhase(vctx, dynClient, run)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("runGangPositivePhase: %v", err)
			}
			if report == nil || report.CoScheduleSpan != 0 {
				t.Errorf("report = %+v, want a zero co-schedule span for identical timestamps", report)
			}
			// The positive members must carry the upstream requests so the
			// scheduler has something to co-schedule against.
			for i := range gangMinMembers {
				pod, err := clientset.CoreV1().Pods(run.namespace).Get(context.Background(), run.pods[i], metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get %s: %v", run.pods[i], err)
				}
				req := pod.Spec.Containers[0].Resources.Requests
				if cpu := req[corev1.ResourceCPU]; cpu.String() != gangWorkerCPURequest {
					t.Errorf("%s cpu request = %s, want %s", pod.Name, cpu.String(), gangWorkerCPURequest)
				}
				if mem := req[corev1.ResourceMemory]; mem.String() != gangWorkerMemoryRequest {
					t.Errorf("%s memory request = %s, want %s", pod.Name, mem.String(), gangWorkerMemoryRequest)
				}
				if _, hasGPU := req["nvidia.com/gpu"]; hasGPU {
					t.Errorf("%s requests a GPU; the check must stay CPU-only", pod.Name)
				}
				if pod.Annotations[podGroupNameAnnotation] != run.groupName {
					t.Errorf("%s pod-group-name annotation = %q, want %q", pod.Name, pod.Annotations[podGroupNameAnnotation], run.groupName)
				}
			}
		})
	}
}

func TestRunGangNegativePhase(t *testing.T) {
	const window = 300 * time.Millisecond
	nodes := gb300LikeNodes()
	nodeObjs := make([]runtime.Object, 0, len(nodes))
	for i := range nodes {
		nodeObjs = append(nodeObjs, &nodes[i])
	}

	homogeneous := homogeneousNodes()
	homogeneousObjs := make([]runtime.Object, 0, len(homogeneous))
	for i := range homogeneous {
		homogeneousObjs = append(homogeneousObjs, &homogeneous[i])
	}

	tests := []struct {
		name        string
		nodes       []runtime.Object
		wantMembers int
		refusal     bool // fake KAI writes the gang refusal
		bindMembers bool // fake default scheduler binds members (gang disabled)
		wantErr     string
		wantMsg     string
	}{
		{
			name:        "members stay unbound and KAI refuses the gang: credited",
			nodes:       nodeObjs,
			wantMembers: len(nodes) + 1,
			refusal:     true,
			wantMsg:     "while 8 are required for gang scheduling",
		},
		{
			name:        "members stay unbound but no refusal is recorded: not credited",
			nodes:       nodeObjs,
			wantMembers: len(nodes) + 1,
			refusal:     false,
			wantErr:     "not credited",
		},
		{
			name:        "gang scheduling disabled: a bound member fails by name even though KAI-shaped status exists",
			nodes:       nodeObjs,
			wantMembers: len(nodes) + 1,
			refusal:     true,
			bindMembers: true,
			wantErr:     "partial scheduling detected: non-fitting gang member gang-neg-worker-",
		},
		{
			// Homogeneous cluster: every member exceeds every node, so no
			// scheduler binds anything; KAI still writes "found for 0 pods
			// while 5 are required" and the phase is credited.
			name:        "homogeneous cluster: nothing can bind, refusal present: credited",
			nodes:       homogeneousObjs,
			wantMembers: len(homogeneous) + 1,
			refusal:     true,
			wantMsg:     "while 5 are required for gang scheduling",
		},
		{
			// Same cluster under a scheduler without gang semantics: nothing
			// binds either, so the missing refusal is the only signal and it
			// must fail the phase.
			name:        "homogeneous cluster: nothing can bind, no refusal: not credited",
			nodes:       homogeneousObjs,
			wantMembers: len(homogeneous) + 1,
			refusal:     false,
			wantErr:     "not credited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, err := newGangTestRun()
			if err != nil {
				t.Fatalf("newGangTestRun: %v", err)
			}
			clientset := k8sfake.NewSimpleClientset(tt.nodes...)
			if tt.bindMembers {
				bindNegativePodOnCreate(clientset, "gpu-0")
			}
			dynClient := newGangDynamicFake(t)
			if tt.refusal {
				refuseGangOnCreate(dynClient)
			}
			vctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}

			msg, err := runGangNegativePhase(vctx, dynClient, run, window)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (msg=%q)", tt.wantErr, msg)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("runGangNegativePhase: %v", err)
			}
			if !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("refusal %q does not contain %q", msg, tt.wantMsg)
			}
			if run.negShape.Members != tt.wantMembers || len(run.negPods) != run.negShape.Members {
				t.Errorf("negative shape = %+v with %d pod names, want %d members", run.negShape, len(run.negPods), tt.wantMembers)
			}
			pods, err := clientset.CoreV1().Pods(run.namespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: gangPhaseSelector(gangPhaseNegative),
			})
			if err != nil {
				t.Fatalf("list negative pods: %v", err)
			}
			if len(pods.Items) != run.negShape.Members {
				t.Errorf("created %d negative pods, want %d", len(pods.Items), run.negShape.Members)
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				if _, hasGPU := pod.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"]; hasGPU {
					t.Errorf("%s requests a GPU; the check must stay CPU-only", pod.Name)
				}
				if pod.Annotations[podGroupNameAnnotation] != run.negGroupName {
					t.Errorf("%s is not associated with the negative PodGroup", pod.Name)
				}
				if pod.Spec.SchedulerName != kaiSchedulerName {
					t.Errorf("%s schedulerName = %q, want %q", pod.Name, pod.Spec.SchedulerName, kaiSchedulerName)
				}
			}
		})
	}
}

// TestGangPhaseBudgetUnderThreeMinutes pins the runtime budget the issue sets:
// the positive completion bound plus the negative window must leave room for
// KAI readiness waits, pod creation and cleanup inside a three-minute check.
func TestGangPhaseBudgetUnderThreeMinutes(t *testing.T) {
	budget := defaults.GangPositiveCompletionTimeout + defaults.GangNegativeWindow
	if limit := 3 * time.Minute; budget >= limit {
		t.Errorf("positive bound %s + negative window %s = %s leaves no headroom under %s",
			defaults.GangPositiveCompletionTimeout, defaults.GangNegativeWindow, budget, limit)
	}
}

// TestCleanupGangTestResourcesDeletesNegativeMembers extends the namespace
// cleanup guard to the negative phase: its pods and PodGroup go with the run.
func TestCleanupGangTestResourcesDeletesNegativeMembers(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	shape, err := sizeGangNegativeShape(gb300LikeNodes())
	if err != nil {
		t.Fatalf("sizeGangNegativeShape: %v", err)
	}
	run.setNegativeShape(shape)

	objs := make([]runtime.Object, 0, 1+len(run.negPods))
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: run.namespace}})
	for _, name := range run.negPods {
		objs = append(objs, negativeMemberPod(run.namespace, name, "", corev1.PodPending))
	}
	clientset := k8sfake.NewSimpleClientset(objs...)
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podGroupGVR: "PodGroupList"},
		buildPodGroup(run), buildNegativePodGroup(run),
	)

	if err := cleanupGangTestResources(context.Background(), clientset, dynClient, run); err != nil {
		t.Fatalf("cleanupGangTestResources: %v", err)
	}
	for _, name := range run.negPods {
		if _, err := clientset.CoreV1().Pods(run.namespace).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			t.Errorf("negative pod %s still present after cleanup", name)
		}
	}
	for _, group := range []string{run.groupName, run.negGroupName} {
		if _, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Get(context.Background(), group, metav1.GetOptions{}); err == nil {
			t.Errorf("PodGroup %s still present after cleanup", group)
		}
	}
}

func TestWrapGangPhase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantCode errors.ErrorCode
	}{
		{"structured error keeps its code", errors.New(errors.ErrCodeUnavailable, "no nodes"), errors.ErrCodeUnavailable},
		{"plain error gets ErrCodeInternal", stderrors.New("boom"), errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := wrapGangPhase(tt.err, "negative phase failed")
			errorstest.WantReportedCode(t, got, tt.wantCode)
			if !strings.Contains(got.Error(), "negative phase failed") || !strings.Contains(got.Error(), tt.err.Error()) {
				t.Errorf("message = %q, want the phase and the cause", got.Error())
			}
		})
	}
}

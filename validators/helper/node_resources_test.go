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

package helper

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testVF = v1.ResourceName("nvidia.com/sriov_resource_rail_0")

// resourceContainer requests (and/or limits) n VFs; a negative value omits
// that side.
func resourceContainer(request, limit int64) v1.Container {
	c := v1.Container{Name: "c"}
	if request >= 0 {
		c.Resources.Requests = v1.ResourceList{testVF: *resource.NewQuantity(request, resource.DecimalSI)}
	}
	if limit >= 0 {
		c.Resources.Limits = v1.ResourceList{testVF: *resource.NewQuantity(limit, resource.DecimalSI)}
	}
	return c
}

func vfPod(name, node string, phase v1.PodPhase, containers ...v1.Container) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       v1.PodSpec{NodeName: node, Containers: containers},
		Status:     v1.PodStatus{Phase: phase},
	}
}

func TestPodEffectiveRequests(t *testing.T) {
	t.Parallel()
	always := v1.ContainerRestartPolicyAlways

	tests := []struct {
		name string
		pod  *v1.Pod
		want int64
	}{
		{name: "request", pod: vfPod("p", "n", v1.PodRunning, resourceContainer(2, -1)), want: 2},
		{name: "limit only counts the limit", pod: vfPod("p", "n", v1.PodRunning, resourceContainer(-1, 3)), want: 3},
		{name: "request wins over limit", pod: vfPod("p", "n", v1.PodRunning, resourceContainer(1, 1)), want: 1},
		{name: "containers sum", pod: vfPod("p", "n", v1.PodRunning, resourceContainer(1, -1), resourceContainer(2, -1)), want: 3},
		{
			name: "larger init container wins",
			pod: func() *v1.Pod {
				p := vfPod("p", "n", v1.PodRunning, resourceContainer(1, -1))
				p.Spec.InitContainers = []v1.Container{resourceContainer(4, -1)}
				return p
			}(),
			want: 4,
		},
		{
			name: "sidecar adds to the app containers and to later init containers",
			pod: func() *v1.Pod {
				p := vfPod("p", "n", v1.PodRunning, resourceContainer(1, -1))
				sidecar := resourceContainer(1, -1)
				sidecar.RestartPolicy = &always
				p.Spec.InitContainers = []v1.Container{sidecar, resourceContainer(2, -1)}
				return p
			}(),
			want: 3,
		},
		{name: "no resources", pod: vfPod("p", "n", v1.PodRunning, v1.Container{Name: "c"}), want: 0},
		{
			name: "pod overhead is added",
			pod: func() *v1.Pod {
				p := vfPod("p", "n", v1.PodRunning, resourceContainer(1, -1))
				p.Spec.Overhead = v1.ResourceList{testVF: *resource.NewQuantity(1, resource.DecimalSI)}
				return p
			}(),
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := PodEffectiveRequests(tt.pod)
			q := got[testVF]
			if q.Value() != tt.want {
				t.Errorf("PodEffectiveRequests()[%s] = %s, want %d", testVF, q.String(), tt.want)
			}
		})
	}
}

func TestNodeResourceRequests(t *testing.T) {
	t.Parallel()

	client := k8sfake.NewClientset(
		vfPod("running", "node-a", v1.PodRunning, resourceContainer(1, -1)),
		vfPod("pending", "node-a", v1.PodPending, resourceContainer(2, -1)),
		vfPod("done", "node-a", v1.PodSucceeded, resourceContainer(4, -1)),
		vfPod("failed", "node-a", v1.PodFailed, resourceContainer(8, -1)),
		vfPod("elsewhere", "node-b", v1.PodRunning, resourceContainer(16, -1)),
	)
	got, err := NodeResourceRequests(context.Background(), client, "node-a")
	if err != nil {
		t.Fatalf("NodeResourceRequests() error = %v", err)
	}
	if q := got[testVF]; q.Value() != 3 {
		t.Errorf("node-a requests %s = %s, want 3 (running + pending only)", testVF, q.String())
	}

	failing := k8sfake.NewClientset()
	failing.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver down")
	})
	if _, err := NodeResourceRequests(context.Background(), failing, "node-a"); err == nil {
		t.Error("NodeResourceRequests() error = nil on a failed List, want error")
	}
}

// TestPodEffectiveRequestsPodLevel covers the cpu and memory accounting the
// scheduler applies on top of the containers: pod-level requests replace the
// containers' sum for the resources they name, and overhead is added after.
func TestPodEffectiveRequestsPodLevel(t *testing.T) {
	t.Parallel()
	pod := vfPod("p", "n", v1.PodRunning, v1.Container{Name: "c", Resources: v1.ResourceRequirements{
		Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m"), v1.ResourceMemory: resource.MustParse("64Mi")},
	}})
	pod.Spec.Overhead = v1.ResourceList{v1.ResourceCPU: resource.MustParse("50m")}

	got := PodEffectiveRequests(pod)
	if q := got[v1.ResourceCPU]; q.MilliValue() != 150 {
		t.Errorf("cpu = %s, want 150m (100m container + 50m overhead)", q.String())
	}

	pod.Spec.Resources = &v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("256Mi")}}
	got = PodEffectiveRequests(pod)
	if q := got[v1.ResourceMemory]; q.Value() != 256*1024*1024 {
		t.Errorf("memory = %s, want the 256Mi pod-level request", q.String())
	}
	if q := got[v1.ResourceCPU]; q.MilliValue() != 150 {
		t.Errorf("cpu = %s, want 150m: pod-level requests name only memory", q.String())
	}
}

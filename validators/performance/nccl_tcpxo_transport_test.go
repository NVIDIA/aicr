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
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// workerLabels are the JobSet labels the watcher selects on. The test pods
// must carry them or the watcher never sees them — as with the real JobSet.
func workerLabels() map[string]string {
	return map[string]string{
		"jobset.sigs.k8s.io/jobset-name":        "nccl-all-reduce-tj",
		"jobset.sigs.k8s.io/replicatedjob-name": "node",
	}
}

func tcpxoInterfacesAnnotation(t *testing.T, mutate func(entries []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry) string {
	t.Helper()
	entries := []gkeTCXOInterfaceEntry{{InterfaceName: "eth0", Network: gkeTCXODefaultNetwork}}
	for i := 1; i <= 8; i++ {
		entries = append(entries, gkeTCXOInterfaceEntry{
			InterfaceName: fmt.Sprintf("eth%d", i),
			Network:       fmt.Sprintf("gpu-nic-%d", i-1),
		})
	}
	if mutate != nil {
		entries = mutate(entries)
	}
	var b strings.Builder
	b.WriteString("[")
	for i, e := range entries {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"interfaceName":%q,"network":%q}`, e.InterfaceName, e.Network)
	}
	b.WriteString("]")
	return b.String()
}

type podOpt func(*v1.Pod)

func withoutDaemon(p *v1.Pod) { p.Spec.InitContainers = nil }

func withInterfaces(t *testing.T, mutate func([]gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry) podOpt {
	t.Helper()
	return func(p *v1.Pod) {
		p.Annotations[gkeTCXOInterfacesAnnotation] = tcpxoInterfacesAnnotation(t, mutate)
	}
}

func withoutDaemonStarted(p *v1.Pod) { p.Status.InitContainerStatuses = nil }

func tcpxoWorkerPod(t *testing.T, name string, opts ...podOpt) *v1.Pod {
	t.Helper()
	started := true
	restartAlways := v1.ContainerRestartPolicyAlways
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "nccl-test",
			Labels:    workerLabels(),
			Annotations: map[string]string{
				gkeTCXODefaultAnnotation:    "eth0",
				gkeTCXOInterfacesAnnotation: tcpxoInterfacesAnnotation(t, nil),
			},
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{{
				Name:          gkeTCXODaemonContainer,
				RestartPolicy: &restartAlways,
			}},
			Containers: []v1.Container{{Name: "node"}},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			InitContainerStatuses: []v1.ContainerStatus{{
				Name:    gkeTCXODaemonContainer,
				Started: &started,
				State:   v1.ContainerState{Running: &v1.ContainerStateRunning{}},
			}},
		},
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
}

// runWatch starts the watcher, creates the pods through the fake clientset
// (delivering watch events), waits until every one is recorded, and stops.
// The returned watcher is already stopped; Assert evaluates the observations
// collected while the pods existed.
func runWatch(t *testing.T, pods ...*v1.Pod) *tcpxoWorkerWatcher {
	t.Helper()
	clientset := fake.NewClientset()
	w := startGKETCPXOWorkerWatch(context.Background(), clientset, "nccl-test")
	for _, pod := range pods {
		if _, err := clientset.CoreV1().Pods("nccl-test").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}
	if len(pods) > 0 {
		waitForRecorded(t, w, len(pods))
	}
	w.Stop()
	return w
}

func TestTCPXOWorkerWatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pods    []*v1.Pod
		workers int
		wantErr string
	}{
		{
			name:    "two wired workers pass",
			pods:    []*v1.Pod{tcpxoWorkerPod(t, "node-0"), tcpxoWorkerPod(t, "node-1")},
			workers: 2,
		},
		{
			name:    "one wired and one unwired worker fails",
			pods:    []*v1.Pod{tcpxoWorkerPod(t, "node-0"), tcpxoWorkerPod(t, "node-1", withoutDaemon)},
			workers: 2,
			wantErr: "has no tcpxo-daemon sidecar",
		},
		{
			name:    "daemon never started on one worker fails",
			pods:    []*v1.Pod{tcpxoWorkerPod(t, "node-0"), tcpxoWorkerPod(t, "node-1", withoutDaemonStarted)},
			workers: 2,
			wantErr: "observed started on 1 of 2",
		},
		{
			name:    "missing interfaces annotation fails",
			pods:    []*v1.Pod{tcpxoWorkerPod(t, "node-0", withInterfaces(t, func(e []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry { return e[:8] }))},
			workers: 1,
			wantErr: "want 9",
		},
		{
			name: "duplicate network fails",
			pods: []*v1.Pod{tcpxoWorkerPod(t, "node-0", withInterfaces(t, func(e []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
				e[5].Network = e[4].Network
				return e
			}))},
			workers: 1,
			wantErr: "two interfaces",
		},
		{
			name: "unknown interface name fails",
			pods: []*v1.Pod{tcpxoWorkerPod(t, "node-0", withInterfaces(t, func(e []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
				e[3].InterfaceName = "eth9"
				return e
			}))},
			workers: 1,
			wantErr: "want eth1..eth8",
		},
		{
			name: "eth0 not first fails",
			pods: []*v1.Pod{tcpxoWorkerPod(t, "node-0", withInterfaces(t, func(e []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
				e[0], e[1] = e[1], e[0]
				return e
			}))},
			workers: 1,
			wantErr: "want eth0→default",
		},
		{
			// Pair order follows the recipe's recorded order; the explicit
			// interfaceName keys are the mapping, not the list position.
			// Consistent with recipe-level validation, which accepts this.
			name: "reordered explicit pairs pass",
			pods: []*v1.Pod{tcpxoWorkerPod(t, "node-0", withInterfaces(t, func(e []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
				e[3], e[4] = e[4], e[3]
				return e
			}))},
			workers: 1,
		},
		{
			name:    "no workers observed fails",
			pods:    nil,
			workers: 2,
			wantErr: "no NCCL worker pods were observed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := runWatch(t, tt.pods...)
			err := w.Assert(tt.workers)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Assert() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Assert() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestTCPXOWorkerWatchTeardownRace reproduces the original defect: a sidecar
// that reached Started during the run but whose final object state reports
// Started=false (normal JobSet teardown of a completed worker) must not fail
// the assertion — the watcher recorded the Started observation while it was
// true.
func TestTCPXOWorkerWatchTeardownRace(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	w := startGKETCPXOWorkerWatch(context.Background(), clientset, "nccl-test")

	pod := tcpxoWorkerPod(t, "node-0")
	if _, err := clientset.CoreV1().Pods("nccl-test").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	waitForRecorded(t, w, 1)

	// The JobSet controller deletes completed workers; kubelet flips Started
	// to false as the sidecar terminates. Simulate the final state reaching us
	// before deletion.
	stopped := false
	updated := pod.DeepCopy()
	updated.Status.InitContainerStatuses[0].Started = &stopped
	updated.Status.InitContainerStatuses[0].State = v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 0}}
	if _, err := clientset.CoreV1().Pods("nccl-test").UpdateStatus(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	if err := clientset.CoreV1().Pods("nccl-test").Delete(context.Background(), "node-0", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	w.Stop()
	if err := w.Assert(1); err != nil {
		t.Fatalf("Assert() error = %v, want nil: the started observation must survive teardown", err)
	}
}

// waitForRecorded polls until the watcher has recorded n pods or the deadline
// passes. It reads the watcher's own count rather than Assert's errors, so a
// wiring failure under test cannot end the wait early.
func waitForRecorded(t *testing.T, w *tcpxoWorkerWatcher, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.recordedCount() == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watcher recorded %d pods, want %d", w.recordedCount(), n)
}

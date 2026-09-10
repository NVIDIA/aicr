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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func tcpxoInterfacesAnnotation(t *testing.T, mutate func(entries []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry) string {
	t.Helper()
	entries := []gkeTCXOInterfaceEntry{{InterfaceName: "eth0", Network: "default"}}
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

// tcpxoWorkerPod builds a Succeeded worker pod carrying the TCPXO wiring:
// the daemon native sidecar (started), and both networking annotations.
func tcpxoWorkerPod(t *testing.T, name string) *v1.Pod {
	t.Helper()
	started := true
	restartAlways := v1.ContainerRestartPolicyAlways
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "nccl-test",
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
			Phase: v1.PodSucceeded,
			InitContainerStatuses: []v1.ContainerStatus{{
				Name:    gkeTCXODaemonContainer,
				Started: &started,
			}},
		},
	}
}

func TestAssertGKETCPXOTransportRealized(t *testing.T) {
	t.Parallel()

	launcher := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "launcher", Namespace: "nccl-test"},
		Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "node"}}},
	}

	tests := []struct {
		name         string
		pods         []v1.Pod
		mutateWorker func(*v1.Pod)
		wantErr      string
	}{
		{
			name:    "two wired workers pass",
			pods:    []v1.Pod{*launcher, *tcpxoWorkerPod(t, "node-0"), *tcpxoWorkerPod(t, "node-1")},
			wantErr: "",
		},
		{
			name:    "no pods with the sidecar fails",
			pods:    []v1.Pod{*launcher},
			wantErr: "no pod",
		},
		{
			name: "missing interfaces annotation fails",
			mutateWorker: func(p *v1.Pod) {
				delete(p.Annotations, gkeTCXOInterfacesAnnotation)
			},
			wantErr: "missing networking.gke.io/interfaces",
		},
		{
			name: "wrong default interface fails",
			mutateWorker: func(p *v1.Pod) {
				p.Annotations[gkeTCXODefaultAnnotation] = "eth1"
			},
			wantErr: "want eth0",
		},
		{
			name: "eight entries instead of nine fails",
			mutateWorker: func(p *v1.Pod) {
				p.Annotations[gkeTCXOInterfacesAnnotation] = tcpxoInterfacesAnnotation(t,
					func(entries []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry { return entries[:8] })
			},
			wantErr: "want 9",
		},
		{
			// A network swap between two entries keeps every interface name in
			// order and every network unique — the pod assertion cannot see it,
			// by construction: catching it needs the intended mapping, which is
			// the recipe↔deployed comparison's job (#2297). Pinned here so the
			// boundary of this check is explicit, not discovered.
			name: "swapped networks between interfaces passes this layer",
			mutateWorker: func(p *v1.Pod) {
				p.Annotations[gkeTCXOInterfacesAnnotation] = tcpxoInterfacesAnnotation(t,
					func(entries []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
						// Swap eth3 and eth4's networks: every name valid,
						// every network unique — and the mapping is wrong.
						entries[3].Network, entries[4].Network = entries[4].Network, entries[3].Network
						return entries
					})
			},
			wantErr: "",
		},
		{
			name: "interface names out of order fails",
			mutateWorker: func(p *v1.Pod) {
				p.Annotations[gkeTCXOInterfacesAnnotation] = tcpxoInterfacesAnnotation(t,
					func(entries []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
						// eth3 and eth4 swap positions: the names are all
						// present and the networks unique, but the ordered
						// interface->network mapping is wrong.
						entries[3], entries[4] = entries[4], entries[3]
						return entries
					})
			},
			wantErr: "want",
		},
		{
			name: "duplicate network fails",
			mutateWorker: func(p *v1.Pod) {
				p.Annotations[gkeTCXOInterfacesAnnotation] = tcpxoInterfacesAnnotation(t,
					func(entries []gkeTCXOInterfaceEntry) []gkeTCXOInterfaceEntry {
						entries[5].Network = entries[4].Network
						return entries
					})
			},
			wantErr: "two interfaces",
		},
		{
			name: "sidecar never started fails",
			mutateWorker: func(p *v1.Pod) {
				notStarted := false
				p.Status.InitContainerStatuses[0].Started = &notStarted
			},
			wantErr: "never reported started",
		},
		{
			name: "sidecar status missing fails",
			mutateWorker: func(p *v1.Pod) {
				p.Status.InitContainerStatuses = nil
			},
			wantErr: "no init-container status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pods := tt.pods
			if tt.mutateWorker != nil {
				worker := tcpxoWorkerPod(t, "node-0")
				tt.mutateWorker(worker)
				pods = []v1.Pod{*launcher, *worker}
			}
			objs := make([]runtime.Object, 0, len(pods))
			for i := range pods {
				objs = append(objs, &pods[i])
			}
			clientset := fake.NewClientset(objs...)
			err := assertGKETCPXOTransportRealized(context.Background(), clientset, "nccl-test")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("assertGKETCPXOTransportRealized() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("assertGKETCPXOTransportRealized() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

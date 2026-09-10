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
	"encoding/json"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// gkeTCXOInterfacesAnnotation is the multi-NIC wiring the GPUDirect-TCPXO
// runtime must carry. gkeTCXODaemonContainer is the native sidecar that runs
// the RxDM manager next to every worker.
const (
	gkeTCXOInterfacesAnnotation = "networking.gke.io/interfaces"
	gkeTCXODefaultAnnotation    = "networking.gke.io/default-interface"
	gkeTCXODaemonContainer      = "tcpxo-daemon"

	// gkeTCXODefaultNetwork is the network name of the default (eth0)
	// interface in the interfaces annotation.
	gkeTCXODefaultNetwork = "default"
)

// assertGKETCPXOTransportRealized verifies the realized benchmark pods carried
// the TCPXO wiring. It exists because the marker-based check
// (verifyTransportFromLogs) is a no-op on exactly this path: NCCL_DEBUG=WARN
// is deliberate on GKE (INFO rotated the results table out of retrievable
// logs, #1712), so the "NCCL INFO Using network" banner never appears.
//
// The assertion is on the realized Pod, so it holds regardless of where the
// runtime came from — the validator's own testdata today, the shipped
// torch-distributed-tcpxo runtime once #2297 derives from it. It proves the
// wiring was present and started; the bandwidth floor (unreachable over TCP
// on eth0) proves traffic crossed it.
func assertGKETCPXOTransportRealized(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	pods, err := clientset.CoreV1().Pods(namespace).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to list benchmark pods for TCPXO transport assertion", err)
	}

	workers := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podHasTCXODaemon(pod) {
			continue
		}
		workers++
		if err := checkTCXOAnnotations(pod); err != nil {
			return err
		}
		if err := checkTCXODaemonStarted(pod); err != nil {
			return err
		}
	}
	if workers == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"no pod in namespace %q carries the %s sidecar; the benchmark ran without the TCPXO wiring "+
				"and its bandwidth number says nothing about the fabric", namespace, gkeTCXODaemonContainer))
	}
	return nil
}

// podHasTCXODaemon reports whether the pod spec includes the tcpxo-daemon
// native sidecar (an initContainer with restartPolicy: Always).
func podHasTCXODaemon(pod *v1.Pod) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == gkeTCXODaemonContainer {
			return true
		}
	}
	return false
}

// gkeTCXOInterfaceEntry mirrors one entry of the networking.gke.io/interfaces
// annotation payload.
type gkeTCXOInterfaceEntry struct {
	InterfaceName string `json:"interfaceName"`
	Network       string `json:"network"`
}

// checkTCXOAnnotations asserts the multi-NIC annotations: eth0 → default
// first, then exactly the eight GPU-NIC interfaces eth1..eth8 mapped to eight
// distinct networks — the a3-megagpu-8g contract the runtime records.
func checkTCXOAnnotations(pod *v1.Pod) error {
	if pod.Annotations[gkeTCXODefaultAnnotation] != "eth0" {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"pod %q: %s annotation is %q, want eth0", pod.Name, gkeTCXODefaultAnnotation,
			pod.Annotations[gkeTCXODefaultAnnotation]))
	}
	raw := pod.Annotations[gkeTCXOInterfacesAnnotation]
	if raw == "" {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"pod %q: missing %s annotation", pod.Name, gkeTCXOInterfacesAnnotation))
	}
	var entries []gkeTCXOInterfaceEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"pod %q: %s annotation is not valid JSON", pod.Name, gkeTCXOInterfacesAnnotation), err)
	}
	if len(entries) != 9 {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"pod %q: %s annotation has %d entries, want 9 (eth0 default + eth1..eth8 GPU NICs)",
			pod.Name, gkeTCXOInterfacesAnnotation, len(entries)))
	}
	if entries[0].InterfaceName != "eth0" || entries[0].Network != gkeTCXODefaultNetwork {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"pod %q: %s first entry = %s→%s, want eth0→default",
			pod.Name, gkeTCXOInterfacesAnnotation, entries[0].InterfaceName, entries[0].Network))
	}
	seenNetworks := make(map[string]struct{}, len(entries)-1)
	for i, entry := range entries[1:] {
		want := fmt.Sprintf("eth%d", i+1)
		if entry.InterfaceName != want {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
				"pod %q: %s entry %d = interface %q, want %q — the ordered interface→network "+
					"mapping is the contract, and a shifted mapping silently lands traffic on the wrong NIC",
				pod.Name, gkeTCXOInterfacesAnnotation, i+1, entry.InterfaceName, want))
		}
		if _, dup := seenNetworks[entry.Network]; dup {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
				"pod %q: %s maps two interfaces to network %q", pod.Name, gkeTCXOInterfacesAnnotation, entry.Network))
		}
		seenNetworks[entry.Network] = struct{}{}
	}
	return nil
}

// checkTCXODaemonStarted requires the native sidecar to report Started in its
// init-container status — proof the RxDM manager actually ran, not merely
// that the spec named it.
func checkTCXODaemonStarted(pod *v1.Pod) error {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != gkeTCXODaemonContainer {
			continue
		}
		if status.Started != nil && *status.Started {
			return nil
		}
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"pod %q: %s sidecar never reported started", pod.Name, gkeTCXODaemonContainer))
	}
	return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
		"pod %q: no init-container status for %s; the sidecar's start cannot be proven",
		pod.Name, gkeTCXODaemonContainer))
}

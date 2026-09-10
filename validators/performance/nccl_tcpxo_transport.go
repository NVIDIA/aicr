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
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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

	// gkeTCXOExpectedEntries is eth0 (default) plus eth1..eth8 (GPU NICs) on
	// the a3-megagpu-8g shape this assertion covers.
	gkeTCXOExpectedEntries = 9
)

// tcpxoWorkerRecord accumulates what the watcher has seen of one worker pod.
// daemonStarted is sticky: it records that the sidecar reached Started at some
// point while running. Reading it at assertion time would race JobSet's
// teardown of completed workers, which flips Started back to false.
type tcpxoWorkerRecord struct {
	wiringErr     error
	daemonStarted bool
}

// tcpxoWorkerWatcher observes benchmark worker pods as they run.
type tcpxoWorkerWatcher struct {
	mu      sync.Mutex
	records map[string]*tcpxoWorkerRecord
}

// startGKETCPXOWorkerWatch watches the benchmark's worker pods from TrainJob
// creation until stop is called. It answers the question the log-marker check
// cannot answer on GKE (NCCL_DEBUG=WARN suppresses the "Using network" banner,
// deliberately — #1712): did the realized worker pods carry and activate the
// TCPXO wiring?
//
// Observations are recorded while pods are alive because the JobSet controller
// deletes active worker Jobs as soon as the JobSet completes — by the time the
// launcher is known to have succeeded, the workers may already be gone, and a
// completed native sidecar's Started flag reads false. Asserting from state
// read only after completion would fail successful runs at random.
//
// The returned assert must be called after stop, once the benchmark outcome is
// known. The watcher is scoped to the benchmark's JobSet labels, so unrelated
// pods in the namespace are never inspected.
func startGKETCPXOWorkerWatch(ctx context.Context, clientset kubernetes.Interface, namespace string) (assert func(wantWorkers int) error, stop func()) {
	watchCtx, cancel := context.WithCancel(ctx)
	w := &tcpxoWorkerWatcher{records: make(map[string]*tcpxoWorkerRecord)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(watchCtx, clientset, namespace)
	}()

	stop = func() {
		cancel()
		<-done
	}
	assert = func(wantWorkers int) error { return w.assert(wantWorkers) }
	return assert, stop
}

func (w *tcpxoWorkerWatcher) run(ctx context.Context, clientset kubernetes.Interface, namespace string) {
	selector := fmt.Sprintf("jobset.sigs.k8s.io/jobset-name=%s,jobset.sigs.k8s.io/replicatedjob-name=%s",
		ncclTrainJobName, nodeJobName)
	for ctx.Err() == nil {
		w.watchOnce(ctx, clientset, namespace, selector)
	}
}

// watchOnce streams one watch session; the outer run loop re-establishes after
// the API server closes the channel, which it does routinely.
func (w *tcpxoWorkerWatcher) watchOnce(ctx context.Context, clientset kubernetes.Interface, namespace, selector string) {
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("TCPXO worker watch failed to start; retrying", "error", err)
			time.Sleep(time.Second)
		}
		return
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			pod, ok := event.Object.(*v1.Pod)
			if !ok || event.Type == watch.Deleted {
				continue
			}
			w.record(pod)
		}
	}
}

func (w *tcpxoWorkerWatcher) record(pod *v1.Pod) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rec := w.records[pod.Name]
	if rec == nil {
		rec = &tcpxoWorkerRecord{}
		w.records[pod.Name] = rec
	}
	// Pod wiring is immutable, but validate on every event so a bad object is
	// recorded the first time it is seen, not only on creation.
	rec.wiringErr = validateTCPXOWorkerWiring(pod)
	if tcpxoDaemonStarted(pod) {
		rec.daemonStarted = true
	}
}

// assert evaluates the collected records once the benchmark outcome is known.
// wantWorkers is the benchmark's worker count: that many workers must have
// been observed with the daemon provably started, and no observed worker may
// carry broken wiring.
func (w *tcpxoWorkerWatcher) assert(wantWorkers int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.records) == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			"no NCCL worker pods were observed during the benchmark; the TCPXO wiring cannot be attested")
	}
	var problems []string
	started := 0
	for name, rec := range w.records {
		if rec.wiringErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, rec.wiringErr))
		}
		if rec.daemonStarted {
			started++
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			"benchmark worker pods did not carry the TCPXO wiring: "+strings.Join(problems, "; "))
	}
	if started < wantWorkers {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
			"the %s sidecar was observed started on %d of %d benchmark workers; "+
				"the bandwidth result cannot attest to the fabric on the full cohort",
			gkeTCXODaemonContainer, started, wantWorkers))
	}
	return nil
}

// validateTCPXOWorkerWiring checks the immutable wiring on one benchmark
// worker pod: the daemon sidecar, the default interface, and the interfaces
// annotation. The annotation contract matches the recipe-level validation
// (ValidateGKETCPXOInterfaces): eth0 → default first (fixed by the manifest),
// then explicit interfaceName → network pairs covering eth1..eth8 exactly once
// with unique networks. Pair order in the annotation follows the recipe's
// recorded order; the explicit interfaceName keys are the mapping, not the
// list position.
func validateTCPXOWorkerWiring(pod *v1.Pod) error {
	if !podHasTCXODaemon(pod) {
		return fmt.Errorf("pod %q has no %s sidecar", pod.Name, gkeTCXODaemonContainer)
	}
	return checkTCXOAnnotations(pod)
}

// podHasTCXODaemon reports whether the pod spec includes the tcpxo-daemon
// native sidecar (an initContainer with restartPolicy: Always).
func podHasTCXODaemon(pod *v1.Pod) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == gkeTCXODaemonContainer && c.RestartPolicy != nil &&
			*c.RestartPolicy == v1.ContainerRestartPolicyAlways {

			return true
		}
	}
	return false
}

// tcpxoDaemonStarted reports whether the daemon's init-container status
// currently reports Started. The watcher OR-accumulates this into the record,
// so a normal termination at job completion never erases the proof.
func tcpxoDaemonStarted(pod *v1.Pod) bool {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == gkeTCXODaemonContainer {
			return status.Started != nil && *status.Started
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

// checkTCXOAnnotations asserts the multi-NIC annotations on a realized pod:
// eth0 → default first, then explicit pairs covering eth1..eth8 exactly once
// with distinct networks — the a3-megagpu-8g contract the runtime records.
func checkTCXOAnnotations(pod *v1.Pod) error {
	if pod.Annotations[gkeTCXODefaultAnnotation] != "eth0" {
		return fmt.Errorf("pod %q: %s annotation is %q, want eth0",
			pod.Name, gkeTCXODefaultAnnotation, pod.Annotations[gkeTCXODefaultAnnotation])
	}
	raw := pod.Annotations[gkeTCXOInterfacesAnnotation]
	if raw == "" {
		return fmt.Errorf("pod %q: missing %s annotation", pod.Name, gkeTCXOInterfacesAnnotation)
	}
	var entries []gkeTCXOInterfaceEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return fmt.Errorf("pod %q: %s annotation is not valid JSON: %w",
			pod.Name, gkeTCXOInterfacesAnnotation, err)
	}
	if len(entries) != gkeTCXOExpectedEntries {
		return fmt.Errorf("pod %q: %s annotation has %d entries, want %d (eth0 default + eth1..eth8 GPU NICs)",
			pod.Name, gkeTCXOInterfacesAnnotation, len(entries), gkeTCXOExpectedEntries)
	}
	if entries[0].InterfaceName != "eth0" || entries[0].Network != gkeTCXODefaultNetwork {
		return fmt.Errorf("pod %q: %s first entry = %s→%s, want eth0→default",
			pod.Name, gkeTCXOInterfacesAnnotation, entries[0].InterfaceName, entries[0].Network)
	}
	seenInterfaces := make(map[string]struct{}, gkeTCXOExpectedEntries-1)
	seenNetworks := make(map[string]struct{}, gkeTCXOExpectedEntries-1)
	for _, entry := range entries[1:] {
		if !gkeTCPXOInterfaceName(entry.InterfaceName) {
			return fmt.Errorf("pod %q: %s entry has interface %q, want eth1..eth8 — "+
				"a wrong interface name lands traffic on the wrong NIC",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.InterfaceName)
		}
		if _, dup := seenInterfaces[entry.InterfaceName]; dup {
			return fmt.Errorf("pod %q: %s repeats interface %q",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.InterfaceName)
		}
		seenInterfaces[entry.InterfaceName] = struct{}{}
		if _, dup := seenNetworks[entry.Network]; dup {
			return fmt.Errorf("pod %q: %s maps two interfaces to network %q",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.Network)
		}
		seenNetworks[entry.Network] = struct{}{}
	}
	if len(seenInterfaces) != gkeTCXOExpectedEntries-1 {
		return fmt.Errorf("pod %q: %s covers %d secondary interfaces, want eth1..eth8",
			pod.Name, gkeTCXOInterfacesAnnotation, len(seenInterfaces))
	}
	return nil
}

// gkeTCPXOInterfaceName mirrors the recipe layer's eth1..eth8 contract without
// importing it (the validator binary is built separately).
func gkeTCPXOInterfaceName(name string) bool {
	if len(name) != 4 || !strings.HasPrefix(name, "eth") {
		return false
	}
	return name[3] >= '1' && name[3] <= '8'
}

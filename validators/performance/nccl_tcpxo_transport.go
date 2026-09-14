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
	"strconv"
	"strings"
	"sync"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
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

	// gkeTCXOWorkerIndexLabel distinguishes the benchmark's worker pods. The
	// node job runs as a single replica with parallelism=numNodes, so
	// jobset job-index is always "0"; the completion index carries 0..N-1.
	gkeTCXOWorkerIndexLabel = "batch.kubernetes.io/job-completion-index"
)

// tcpxoWorkerRecord accumulates what the watcher has seen of one worker pod.
// daemonStarted is sticky: it records that the sidecar reached Started at some
// point while running. Reading it at assertion time would race JobSet's
// teardown of completed workers, which flips Started back to false.
// wiringErr is likewise sticky: once a pod is seen badly wired, a later event
// for the same pod must not erase it.
type tcpxoWorkerRecord struct {
	wiringErr     error
	daemonStarted bool
	jobIndex      string
}

// tcpxoWorkerWatcher observes benchmark worker pods as they run.
type tcpxoWorkerWatcher struct {
	mu      sync.Mutex
	records map[string]*tcpxoWorkerRecord
	cancel  context.CancelFunc
	done    chan struct{}
}

// startGKETCPXOWorkerWatch watches the benchmark's worker pods, recording
// what each one carried while it is alive. It answers the question the
// log-marker check cannot answer on GKE (NCCL_DEBUG=WARN suppresses the
// "Using network" banner, deliberately — #1712): did the realized worker
// pods carry and activate the TCPXO wiring?
//
// Start it BEFORE the benchmark's TrainJob is created, so no worker pod can
// predate the watch. The watch is a list-then-watch with an explicit
// resourceVersion: the List seeds any pod already present and the watch resumes
// from the list's RV, so an apiserver-initiated reconnect cannot silently drop a
// worker created during the gap. Assert only after Stop has joined the
// goroutine — records are complete exactly then.
//
// Observations are recorded while pods are alive because the JobSet
// controller deletes active worker Jobs as soon as the JobSet completes — by
// the time the launcher is known to have succeeded, the workers may already
// be gone, and a completed native sidecar's Started flag reads false.
// Asserting from state read only after completion would fail successful runs
// at random.
//
// The watcher is scoped to the benchmark's JobSet labels, so unrelated pods
// in the namespace are never inspected.
func startGKETCPXOWorkerWatch(ctx context.Context, clientset kubernetes.Interface, namespace string) *tcpxoWorkerWatcher {
	watchCtx, cancel := context.WithCancel(ctx)
	w := &tcpxoWorkerWatcher{
		records: make(map[string]*tcpxoWorkerRecord),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go func() {
		defer close(w.done)
		w.run(watchCtx, clientset, namespace)
	}()
	return w
}

// Stop cancels the watch and joins the goroutine. Idempotent.
func (w *tcpxoWorkerWatcher) Stop() {
	w.cancel()
	<-w.done
}

// Assert evaluates the collected records once the benchmark outcome is known.
// Call only after Stop: records are complete once the goroutine has joined.
// wantWorkers is the benchmark's worker count. Records are grouped by the
// pod's job index, so a restarted worker counts toward its own slot only —
// every slot must have at least one pod whose daemon provably started, and
// no observed pod may carry broken wiring.
func (w *tcpxoWorkerWatcher) Assert(wantWorkers int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.records) == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			"no NCCL worker pods were observed during the benchmark; the TCPXO wiring cannot be attested")
	}
	var problems []string
	startedByIndex := make(map[string]bool)
	for name, rec := range w.records {
		if rec.wiringErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, rec.wiringErr))
		}
		if rec.daemonStarted {
			startedByIndex[rec.jobIndex] = true
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			"benchmark worker pods did not carry the TCPXO wiring: "+strings.Join(problems, "; "))
	}
	for i := 0; i < wantWorkers; i++ {
		if !startedByIndex[strconv.Itoa(i)] {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf(
				"the %s sidecar was never observed started on worker index %d of %d; "+
					"a restarted pod cannot stand in for a slot that never ran the fabric",
				gkeTCXODaemonContainer, i, wantWorkers))
		}
	}
	return nil
}

// recordedCount reports how many distinct worker pods the watcher has seen.
// Tests use it to wait for observations; production reads only Assert.
func (w *tcpxoWorkerWatcher) recordedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.records)
}

func (w *tcpxoWorkerWatcher) run(ctx context.Context, clientset kubernetes.Interface, namespace string) {
	selector := fmt.Sprintf("jobset.sigs.k8s.io/jobset-name=%s,jobset.sigs.k8s.io/replicatedjob-name=%s",
		ncclTrainJobName, nodeJobName)
	for ctx.Err() == nil {
		// List-then-watch with an explicit resourceVersion. A raw Watch with an
		// empty RV starts at latest and replays nothing, so across an apiserver-
		// initiated reconnect it would miss a worker created in the gap; the List
		// both seeds records with pods already present and returns the RV the
		// watch resumes from, closing that gap. record is idempotent and sticky,
		// so re-listing already-seen pods is safe.
		rv := w.listOnce(ctx, clientset, namespace, selector)
		w.watchOnce(ctx, clientset, namespace, selector, rv)
		if ctx.Err() != nil {
			return
		}
		// Pace re-establishment: the API server rotates long watches every few
		// minutes, and an immediate reconnect under apiserver stress would spin.
		time.Sleep(time.Second)
	}
}

// listOnce records every currently-matching worker pod and returns the list's
// resourceVersion for the watch to resume from. A stale RV surfaces as a watch
// error and the outer loop re-lists, obtaining a fresh one.
func (w *tcpxoWorkerWatcher) listOnce(ctx context.Context, clientset kubernetes.Interface, namespace, selector string) string {
	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("TCPXO worker list failed; retrying", "error", err)
			time.Sleep(time.Second)
		}
		return ""
	}
	for i := range list.Items {
		w.record(&list.Items[i])
	}
	return list.GetResourceVersion()
}

// watchOnce streams one watch session from resourceVersion; the outer run loop
// re-lists and re-establishes after the API server closes the channel, which it
// does routinely.
func (w *tcpxoWorkerWatcher) watchOnce(ctx context.Context, clientset kubernetes.Interface, namespace, selector, resourceVersion string) {
	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{LabelSelector: selector, ResourceVersion: resourceVersion})
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
	// recorded the first time it is seen, not only on creation. Sticky: a later
	// event for the same pod must not erase a recorded failure.
	if err := validateTCPXOWorkerWiring(pod); err != nil && rec.wiringErr == nil {
		rec.wiringErr = err
	}
	if tcpxoDaemonStarted(pod) {
		rec.daemonStarted = true
	}
	// Key the per-worker slot on the job-completion-index, NOT the JobSet
	// job-index: Trainer v2.2.0 runs the node job as one replica with
	// parallelism=numNodes, so every worker carries job-index=0 and only the
	// completion index distinguishes worker 0..N-1.
	if idx := pod.Labels[gkeTCXOWorkerIndexLabel]; idx != "" {
		rec.jobIndex = idx
	}
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
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q has no %s sidecar", pod.Name, gkeTCXODaemonContainer))
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
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s annotation is %q, want eth0",
			pod.Name, gkeTCXODefaultAnnotation, pod.Annotations[gkeTCXODefaultAnnotation]))
	}
	raw := pod.Annotations[gkeTCXOInterfacesAnnotation]
	if raw == "" {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: missing %s annotation", pod.Name, gkeTCXOInterfacesAnnotation))
	}
	var entries []gkeTCXOInterfaceEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s annotation is not valid JSON",
			pod.Name, gkeTCXOInterfacesAnnotation), err)
	}
	if len(entries) != gkeTCXOExpectedEntries {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s annotation has %d entries, want %d (eth0 default + eth1..eth8 GPU NICs)",
			pod.Name, gkeTCXOInterfacesAnnotation, len(entries), gkeTCXOExpectedEntries))
	}
	if entries[0].InterfaceName != "eth0" || entries[0].Network != gkeTCXODefaultNetwork {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s first entry = %s→%s, want eth0→default",
			pod.Name, gkeTCXOInterfacesAnnotation, entries[0].InterfaceName, entries[0].Network))
	}
	seenInterfaces := make(map[string]struct{}, gkeTCXOExpectedEntries-1)
	seenNetworks := make(map[string]struct{}, gkeTCXOExpectedEntries-1)
	for _, entry := range entries[1:] {
		if !recipe.IsGKETCPXOInterfaceName(entry.InterfaceName) {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s entry has interface %q, want eth1..eth8 — "+
				"a wrong interface name lands traffic on the wrong NIC",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.InterfaceName))
		}
		if _, dup := seenInterfaces[entry.InterfaceName]; dup {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s repeats interface %q",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.InterfaceName))
		}
		seenInterfaces[entry.InterfaceName] = struct{}{}
		if entry.Network == gkeTCXODefaultNetwork {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s maps secondary interface %q to reserved network %q — eth0 already uses it; a secondary must name a distinct GPU NIC network",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.InterfaceName, entry.Network))
		}
		if _, dup := seenNetworks[entry.Network]; dup {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s maps two interfaces to network %q",
				pod.Name, gkeTCXOInterfacesAnnotation, entry.Network))
		}
		seenNetworks[entry.Network] = struct{}{}
	}
	if len(seenInterfaces) != gkeTCXOExpectedEntries-1 {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, fmt.Sprintf("pod %q: %s covers %d secondary interfaces, want eth1..eth8",
			pod.Name, gkeTCXOInterfacesAnnotation, len(seenInterfaces)))
	}
	return nil
}

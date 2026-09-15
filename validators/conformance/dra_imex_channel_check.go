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
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// Behavioral MNNVL ComputeDomain → ResourceClaimTemplate → IMEX channel
// subtest of the dra-support check (#1649).
//
// The supported NVIDIA DRA driver configuration is ComputeDomain-only, so on
// every shipped GB200/GB300-class cluster the full-GPU DRA subtest is not
// applicable and, before this subtest existed, dra-support certified DRA on
// driver health plus structural ResourceSlice validation alone. Structural
// validation is not allocation: a driver can publish well-formed
// compute-domain.nvidia.com slices and still fail to hand a pod an IMEX
// channel. This subtest exercises the flow the NCCL NVLS validator runs in
// production — create a ComputeDomain, wait for the driver to reconcile it
// into a ResourceClaimTemplate, consume one channel from that template on a
// minimal pod — so a conformance-only run gets a behavioral pass/fail signal.
//
// Verdict: the pod must reach Succeeded with its script having observed
// EXACTLY ONE channel character device under /dev/nvidia-caps-imex-channels.
// The kubelet does not start a pod whose ResourceClaim is not allocated,
// reserved, and prepared, so a pod that ran with the injected device proves
// the allocated/reserved transition happened. The generated claim's
// post-terminal state is recorded as best-effort EVIDENCE only: the
// resource-claim controller releases a completed pod's reservation and
// garbage-collects template-generated claims, so a claim read after the pod
// exits can legitimately find nothing (see recordIMEXGeneratedClaim).
const (
	// computeDomainAPIGroup is the API group of the NVIDIA DRA driver's
	// ComputeDomain CRD; the served version is pinned at v1beta1 by the
	// driver chart (see recipes/components/slinky-slurm/manifests/compute-domain.yaml).
	computeDomainAPIGroup = "resource.nvidia.com"

	// labelNVIDIAGPUClique is the node label carrying the MNNVL fabric
	// identity (<clusterUUID>.<cliqueID>). It is published by the NVIDIA
	// stack (GPU Feature Discovery, or the ComputeDomain kubelet plugin in
	// the pinned driver) only on nodes that are part of a multi-node NVLink
	// fabric, and the DRA driver itself uses it to form IMEX domains. Its
	// absence is the not-applicable signal: non-MNNVL nodes (e.g. H100)
	// still advertise compute-domain.nvidia.com ResourceSlices, so slice
	// presence alone cannot gate this subtest.
	labelNVIDIAGPUClique = "nvidia.com/gpu.clique"

	// Per-run resource name prefixes; each is followed by the run's 32-hex
	// token (see newGPUTestRun), staying within the 63-char DNS-1123 limit.
	imexComputeDomainPrefix = "imex-cd-"
	imexClaimTemplatePrefix = "imex-rct-"
	imexTestPodPrefix       = "imex-alloc-test-"

	// imexPodClaimName is the pod-local name of the IMEX channel claim
	// (spec.resourceClaims[].name); pod.status.resourceClaimStatuses is
	// matched on it to find the generated ResourceClaim.
	imexPodClaimName = "imex-channel"
	// containerNameIMEXTest is the probe container name.
	containerNameIMEXTest = "imex-test"
	// imexChannelDir is where the kubelet (via the driver's CDI spec) mounts
	// the allocated IMEX channel character device(s).
	imexChannelDir = "/dev/nvidia-caps-imex-channels"

	// computeDomainAllocationModeSingle allocates one channel per claim.
	computeDomainAllocationModeSingle = "Single"

	// imexChannelProbeScript asserts exactly one channel character device is
	// visible. `find` fails (non-zero, stderr) when the directory does not
	// exist, which the `|| true` folds into an empty list → count 0 → FAIL.
	// Same device-discovery logic as the Slinky IMEX channel check.
	imexChannelProbeScript = `channels=$(find ` + imexChannelDir + ` -maxdepth 1 -type c -name 'channel*' 2>/dev/null || true)
count=$(printf '%s\n' "$channels" | grep -c . || true)
echo "IMEX_CHANNEL_COUNT=$count"
echo "IMEX_CHANNEL_CANDIDATES:"
echo "$channels"
if [ "$count" -ne 1 ]; then
  echo "FAIL: expected exactly one IMEX channel device under ` + imexChannelDir + `"
  exit 1
fi
echo "PASS: IMEX channel allocated: $channels"`

	// Artifact labels for the subtest.
	artifactIMEXSubtest = "Behavioral IMEX channel allocation"
)

// computeDomainGVR addresses the NVIDIA DRA driver's ComputeDomain CRD.
var computeDomainGVR = schema.GroupVersionResource{
	Group:    computeDomainAPIGroup,
	Version:  versionV1beta1,
	Resource: "computedomains",
}

// imexClaimTemplateTimeout bounds the wait for the DRA driver to reconcile
// the ComputeDomain into its ResourceClaimTemplate. A package variable (not
// a const) so tests can shorten it, mirroring draProbeTimeout.
var imexClaimTemplateTimeout = defaults.DiagnosticTimeout

// imexCandidateNodes gates the subtest. Returns:
//   - candidates: eligible (Ready, schedulable) nodes that BOTH carry the
//     clique label AND advertise a usable compute-domain.nvidia.com device —
//     the probe pod is constrained to exactly these nodes so applicability,
//     structural validation, and placement all refer to the same nodes;
//   - cliqueNodes: eligible nodes carrying the clique label, regardless of
//     slices, so the caller can distinguish "no MNNVL fabric" (N/A) from
//     "MNNVL nodes exist but none has a usable compute-domain slice" (FAIL).
func imexCandidateNodes(eligible map[string]*corev1.Node, computeDomainNodes map[string]struct{}) (candidates, cliqueNodes []string) {
	cliqueSet := make(map[string]struct{})
	for name, node := range eligible {
		if node.Labels[labelNVIDIAGPUClique] == "" {
			continue
		}
		cliqueSet[name] = struct{}{}
	}
	candidateSet := make(map[string]struct{})
	for name := range cliqueSet {
		if _, ok := computeDomainNodes[name]; ok {
			candidateSet[name] = struct{}{}
		}
	}
	return sortedNodeNames(candidateSet), sortedNodeNames(cliqueSet)
}

// validateIMEXChannelAllocation runs the behavioral IMEX channel subtest when
// applicable (see imexCandidateNodes). version is the served resource.k8s.io
// API version at which the generated ResourceClaimTemplate and ResourceClaim
// are read; the ComputeDomain itself is always resource.nvidia.com/v1beta1.
//
// The return is NAMED so the deferred cleanup can fail an otherwise-passing
// subtest when cleanup terminally fails — a PASS that leaks the namespace,
// ComputeDomain, and allocated channel claim is not a pass. A primary test
// error is always preserved (never overwritten by the cleanup error).
func validateIMEXChannelAllocation(ctx *validators.Context, dynClient dynamic.Interface, version string, sv *sliceValidation) (err error) {
	candidates, cliqueNodes := imexCandidateNodes(sv.eligible, sv.usableByDriver[draDriverComputeDomain])
	if len(cliqueNodes) == 0 {
		recordRawTextArtifact(ctx, artifactIMEXSubtest, "",
			fmt.Sprintf("skipped (not applicable): no Ready, schedulable node carries the %s label — no multi-node NVLink (MNNVL) fabric, so ComputeDomain IMEX channel allocation does not apply; "+
				"DRA validated via driver health and validated ResourceSlices", labelNVIDIAGPUClique))
		return nil
	}
	if len(candidates) == 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"MNNVL node(s) [%s] carry the %s label but none advertises a usable %s ResourceSlice device — the ComputeDomain DRA driver is not serving the fabric nodes",
			strings.Join(cliqueNodes, ","), labelNVIDIAGPUClique, draDriverComputeDomain))
	}
	// One ComputeDomain channel claim per node: the driver advertises a
	// single channel device per node, so a node whose channel is already
	// allocated (a standing ComputeDomain such as the Slinky Slurm
	// slinky-slurm-imex claim, or a running MNNVL workload) cannot satisfy
	// the probe's Single claim — the pod would sit Pending until the deadline
	// and turn a healthy cluster into a spurious failure. Exclude occupied
	// nodes; when none is free the subtest is not applicable RIGHT NOW and
	// says which claims hold the channels.
	occupied, err := occupiedComputeDomainNodes(ctx.Ctx, dynClient, version, sv.poolNodes[draDriverComputeDomain])
	if err != nil {
		return err
	}
	free := make([]string, 0, len(candidates))
	occupiedLines := make([]string, 0, len(occupied))
	for _, node := range candidates {
		if holders, busy := occupied[node]; busy {
			occupiedLines = append(occupiedLines, fmt.Sprintf("%s held by %s", node, strings.Join(holders, ",")))
			continue
		}
		free = append(free, node)
	}
	recordRawTextArtifact(ctx, "IMEX candidate nodes",
		fmt.Sprintf("kubectl get nodes -l %s; kubectl get resourceclaims -A", labelNVIDIAGPUClique),
		fmt.Sprintf("Clique-labeled nodes:         %s\nWith usable compute-domain:   %s\nChannel already allocated:    %s\nProbe candidates:             %s",
			strings.Join(cliqueNodes, ","), strings.Join(candidates, ","),
			valueOrNone(strings.Join(occupiedLines, "; ")), valueOrNone(strings.Join(free, ","))))
	if len(free) == 0 {
		recordRawTextArtifact(ctx, artifactIMEXSubtest, "",
			fmt.Sprintf("skipped (not applicable): every MNNVL candidate node already holds a %s channel claim (%s) — the driver serves one ComputeDomain channel claim per node, so a probe claim could not be allocated while those workloads run; "+
				"ComputeDomain DRA validated via driver health and validated ResourceSlices",
				draDriverComputeDomain, strings.Join(occupiedLines, "; ")))
		return nil
	}
	candidates = free

	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}
	// Register cleanup BEFORE the first Create (ambiguous-create hazard, see
	// validateDRAAllocation). The ComputeDomain is namespaced, so deleting
	// the per-run namespace reconciles it, the generated template, the
	// generated claim, and the pod together. ComputeDomain and claim
	// finalizers can make termination outlast the cleanup budget; the
	// namespace controller finishes server-side (cleanupGPUTestNamespace
	// reports that as deletion-in-progress, not failure).
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		status, cleanupErr := cleanupGPUTestNamespace(ctx.Clientset, run)
		recordRawTextArtifact(ctx, "Delete IMEX test namespace",
			cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()

	if err = deployIMEXTestResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations, version, candidates); err != nil {
		return err
	}
	// Recorded AFTER the creates succeed so the evidence reports what was
	// actually constructed (per-run generated names; no static manifest).
	recordRawTextArtifact(ctx, "Created IMEX test resources",
		fmt.Sprintf("kubectl get computedomains,resourceclaimtemplates,resourceclaims,pods -n %s", run.namespace),
		fmt.Sprintf(
			"Test resources created via the Kubernetes API (ComputeDomain at %s/%s, claims at %s/%s):\nNamespace:             %s\nComputeDomain:         %s\nResourceClaimTemplate: %s (generated by the DRA driver)\nPod:                   %s (node affinity: %s)",
			computeDomainAPIGroup, versionV1beta1, apiGroupResourceK8sIO, version,
			run.namespace, run.computeDomainName, run.claimTemplateName, run.imexPodName, strings.Join(candidates, ",")))

	pod, err := waitForTerminalPod(ctx.Ctx, ctx.Clientset, run.namespace, run.imexPodName, "IMEX channel test pod")
	if err != nil {
		// The wait returns no pod for stuck (ImagePullBackOff, Unschedulable)
		// and deadline paths — exactly the runs whose diagnostics matter most
		// (claim never allocated → pod Pending until the deadline). Re-read
		// the pod best-effort so its status, logs, and claim evidence still
		// ship; the wait error stays the verdict.
		if last, getErr := ctx.Clientset.CoreV1().Pods(run.namespace).Get(ctx.Ctx, run.imexPodName, metav1.GetOptions{}); getErr == nil {
			recordIMEXPodEvidence(ctx, dynClient, version, last)
		} else {
			recordRawTextArtifact(ctx, "IMEX pod status",
				fmt.Sprintf("kubectl get pod %s -n %s -o wide", run.imexPodName, run.namespace),
				fmt.Sprintf("unavailable after wait failure: %v", getErr))
		}
		return err
	}

	// Evidence BEFORE the verdict: status, logs, and the generated claim's
	// (best-effort) post-terminal state ship even when the run fails.
	recordIMEXPodEvidence(ctx, dynClient, version, pod)

	if pod.Status.Phase != corev1.PodSucceeded {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"IMEX channel test pod phase=%s (want Succeeded) — ComputeDomain IMEX channel allocation failed or the pod did not observe exactly one channel device",
			pod.Status.Phase))
	}
	return nil
}

// deployIMEXTestResources creates the per-run namespace, the ComputeDomain,
// waits for the DRA driver to generate its ResourceClaimTemplate, then
// creates the probe pod that consumes one channel from that template.
// The template MUST exist before the pod is created: the kubelet rejects a
// pod referencing a missing ResourceClaimTemplate.
func deployIMEXTestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gpuTestRun, tolerations []corev1.Toleration, version string, nodeNames []string) error {
	if err := createGPUTestNamespace(ctx, clientset, run); err != nil {
		return err
	}
	if _, err := dynClient.Resource(computeDomainGVR).Namespace(run.namespace).Create(
		ctx, buildIMEXComputeDomain(run), metav1.CreateOptions{}); err != nil {
		// The fresh per-run namespace rules out AlreadyExists from a prior
		// run, so no adopt-and-update path is needed here.
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ComputeDomain", err)
	}
	if err := waitForIMEXClaimTemplate(ctx, dynClient, version, run); err != nil {
		return err
	}
	if err := createPodWhenSAReady(ctx, clientset, run.namespace, buildIMEXTestPod(run, tolerations, nodeNames)); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create IMEX channel test pod")
	}
	return nil
}

// buildIMEXComputeDomain returns the per-run ComputeDomain: numNodes 0 lets
// the driver size the domain from the pods that consume its channels, and
// Single allocation hands each claim exactly one channel. Same shape the
// NCCL NVLS validator applies in production.
func buildIMEXComputeDomain(run *gpuTestRun) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		keyAPIVersion: computeDomainAPIGroup + "/" + versionV1beta1,
		keyKind:       "ComputeDomain",
		keyMetadata: map[string]any{
			keyName:      run.computeDomainName,
			keyNamespace: run.namespace,
		},
		keySpec: map[string]any{
			"numNodes": int64(0),
			"channel": map[string]any{
				"allocationMode": computeDomainAllocationModeSingle,
				"resourceClaimTemplate": map[string]any{
					keyName: run.claimTemplateName,
				},
			},
		},
	}}
}

// waitForIMEXClaimTemplate waits, bounded by imexClaimTemplateTimeout (and
// the caller's work budget), until the DRA driver has reconciled the
// ComputeDomain into the ResourceClaimTemplate named in its spec. Watch API
// (repo rule: watch, don't poll), with Get fast paths before and after
// establishing the watch — the driver may reconcile between the two calls
// and the watch would not replay that Added event.
//
// Hiccup handling mirrors waitForTerminalPod: a transient Get or Watch
// failure (retryableWatchSetupErr: throttling, 5xx, apiserver timeouts,
// transport drops) and a watch channel that closes without the deadline
// firing both get a bounded 250ms→5s backoff and a restart, never an
// immediate failure; permanent errors (authorization, validation) surface at
// once. The deadline message names the budget that actually fired.
func waitForIMEXClaimTemplate(ctx context.Context, dynClient dynamic.Interface, version string, run *gpuTestRun) error {
	waitCtx, cancel := context.WithTimeout(ctx, imexClaimTemplateTimeout)
	defer cancel()

	rctClient := dynClient.Resource(draGVRAt(version, "resourceclaimtemplates")).Namespace(run.namespace)
	timeoutErr := func(cause error) error {
		window := fmt.Sprintf("within %s", imexClaimTemplateTimeout)
		if ctx.Err() != nil {
			window = "before the check's work budget ran out (less than the " + imexClaimTemplateTimeout.String() + " reconcile window remained)"
		}
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
			"DRA driver did not reconcile ComputeDomain %s into ResourceClaimTemplate %s %s",
			run.computeDomainName, run.claimTemplateName, window), cause)
	}
	const (
		backoffBase = 250 * time.Millisecond
		backoffCap  = 5 * time.Second
	)
	backoff := backoffBase
	// pause sleeps the current backoff (context-aware) and doubles it.
	pause := func(cause error) error {
		select {
		case <-waitCtx.Done():
			return timeoutErr(cause)
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > backoffCap {
			backoff = backoffCap
		}
		return nil
	}

	for {
		// present: nil,true when the template exists; a transient read error
		// is retried after backoff; a permanent one is classified and fatal.
		_, getErr := rctClient.Get(waitCtx, run.claimTemplateName, metav1.GetOptions{})
		switch {
		case getErr == nil:
			return nil
		case k8serrors.IsNotFound(getErr):
			// Not reconciled yet — establish the watch below.
		case waitCtx.Err() != nil:
			return timeoutErr(getErr)
		case retryableWatchSetupErr(getErr):
			slog.Warn("IMEX ResourceClaimTemplate read failed; backing off before retry",
				"template", run.claimTemplateName, "error", getErr)
			if err := pause(getErr); err != nil {
				return err
			}
			continue
		default:
			return classifyK8sReadError(getErr, fmt.Sprintf("IMEX ResourceClaimTemplate %s", run.claimTemplateName))
		}

		watcher, watchErr := rctClient.Watch(waitCtx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + run.claimTemplateName,
		})
		if watchErr != nil {
			if waitCtx.Err() != nil {
				return timeoutErr(watchErr)
			}
			if !retryableWatchSetupErr(watchErr) {
				return errors.Wrap(errors.ErrCodeInternal, "failed to watch IMEX ResourceClaimTemplate", watchErr)
			}
			slog.Warn("IMEX ResourceClaimTemplate watch setup failed; backing off before re-get and re-watch",
				"template", run.claimTemplateName, "error", watchErr)
			if err := pause(watchErr); err != nil {
				return err
			}
			continue
		}
		// Re-check after the watch is established (see doc comment). A
		// transient error here simply falls through to the watch, whose
		// restart loop re-Gets anyway.
		if _, err := rctClient.Get(waitCtx, run.claimTemplateName, metav1.GetOptions{}); err == nil {
			watcher.Stop()
			return nil
		}

		appeared, received := consumeTemplateWatch(waitCtx, watcher)
		if appeared {
			return nil
		}
		if waitCtx.Err() != nil {
			return timeoutErr(waitCtx.Err())
		}
		// Closed without cancellation (apiserver hiccup, LB drop): back off
		// (reset when the session delivered events), then re-Get + re-watch
		// rather than failing a healthy run.
		if received {
			backoff = backoffBase
		}
		if err := pause(nil); err != nil {
			return err
		}
	}
}

// consumeTemplateWatch drains an established ResourceClaimTemplate watch
// until the template appears (appeared=true), the context ends, or the
// channel closes (appeared=false — the caller's restart loop takes over).
// received reports whether at least one event arrived (backoff-reset signal).
func consumeTemplateWatch(ctx context.Context, watcher watch.Interface) (appeared, received bool) {
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, received
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return false, received
			}
			received = true
			if event.Type == watch.Added || event.Type == watch.Modified {
				return true, received
			}
		}
	}
}

// occupiedComputeDomainNodes returns, per node, the ResourceClaims currently
// holding that node's compute-domain.nvidia.com channel device, as
// "namespace/name" strings. Allocation results carry the POOL, which is
// resolved to a node through poolNodes (slice-derived); results whose pool
// is unknown are attributed to the pool name itself as a conservative
// fallback (the common case is pool == node name). Claims without an
// allocation are not occupants.
func occupiedComputeDomainNodes(ctx context.Context, dynClient dynamic.Interface, version string, poolNodes map[string]string) (map[string][]string, error) {
	claims, err := dynClient.Resource(draGVRAt(version, "resourceclaims")).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, classifyK8sReadError(err, "ResourceClaims for IMEX channel occupancy")
	}
	occupied := make(map[string][]string)
	for _, claim := range claims.Items {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "ResourceClaim occupancy scan canceled", ctxErr)
		}
		results, found, _ := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
		if !found {
			continue
		}
		holder := claim.GetNamespace() + "/" + claim.GetName()
		for _, r := range results {
			res, ok := r.(map[string]any)
			if !ok {
				continue
			}
			driver, _, _ := unstructured.NestedString(res, "driver")
			if driver != draDriverComputeDomain {
				continue
			}
			pool, _, _ := unstructured.NestedString(res, "pool")
			node, known := poolNodes[pool]
			if !known {
				node = pool
			}
			if node == "" {
				continue
			}
			occupied[node] = append(occupied[node], holder)
		}
	}
	return occupied, nil
}

// recordIMEXPodEvidence records the probe pod's status, container logs, and
// the generated claim's best-effort state — always BEFORE any verdict.
func recordIMEXPodEvidence(ctx *validators.Context, dynClient dynamic.Interface, version string, pod *corev1.Pod) {
	podLines := []string{
		fmt.Sprintf("Name:      %s/%s", pod.Namespace, pod.Name),
		fmt.Sprintf("Phase:     %s", pod.Status.Phase),
		fmt.Sprintf("Node:      %s", valueOrUnknown(pod.Spec.NodeName)),
		fmt.Sprintf("Waiting:   %s", podWaitingStatus(pod)),
		fmt.Sprintf("Claims:    %d", len(pod.Spec.ResourceClaims)),
		fmt.Sprintf("Generated: %s", valueOrUnknown(imexGeneratedClaimName(pod))),
	}
	if reason := podStuckReason(pod); reason != "" {
		podLines = append(podLines, "Stuck:     "+reason)
	}
	recordRawTextArtifact(ctx, "IMEX pod status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", pod.Name, pod.Namespace), strings.Join(podLines, "\n"))
	recordGPUPodContainerLogs(ctx, pod, "IMEX pod logs")
	recordIMEXGeneratedClaim(ctx, dynClient, version, pod)
}

// valueOrNone renders an empty string as "none" for evidence lines.
func valueOrNone(v string) string {
	if v == "" {
		return valueNone
	}
	return v
}

// buildIMEXTestPod returns the probe pod: one busybox container that
// consumes a channel from the generated ResourceClaimTemplate and asserts
// exactly one channel device is visible. No GPU request and no CUDA image —
// the subtest exercises ComputeDomain channel allocation, not GPU access —
// which also keeps it schedulable on nodes whose GPUs are all in use.
// nodeNames constrains placement via REQUIRED node affinity to the IMEX
// candidate nodes (see imexCandidateNodes). tolerations, when non-nil,
// replace the default tolerate-all policy.
func buildIMEXTestPod(run *gpuTestRun, tolerations []corev1.Toleration, nodeNames []string) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.imexPodName,
			Namespace: run.namespace,
		},
		Spec: corev1.PodSpec{
			Affinity:      nodeNameAffinity(nodeNames),
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   tolerations,
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      imexPodClaimName,
				ResourceClaimTemplateName: ptr.To(run.claimTemplateName),
			}},
			Containers: []corev1.Container{{
				Name:    containerNameIMEXTest,
				Image:   defaults.ProbeImage,
				Command: []string{"sh", "-c", imexChannelProbeScript},
				Resources: corev1.ResourceRequirements{
					Claims: []corev1.ResourceClaim{{Name: imexPodClaimName}},
				},
			}},
		},
	}
}

// imexGeneratedClaimName returns the name of the ResourceClaim the kubelet
// generated for the pod-local imexPodClaimName claim, matched by pod-local
// name (never the first entry), or "" when absent.
func imexGeneratedClaimName(pod *corev1.Pod) string {
	for _, s := range pod.Status.ResourceClaimStatuses {
		if s.Name == imexPodClaimName && s.ResourceClaimName != nil {
			return *s.ResourceClaimName
		}
	}
	return ""
}

// recordIMEXGeneratedClaim records the generated ResourceClaim's
// post-terminal state as best-effort evidence. It NEVER affects the verdict:
// once the pod completes, the resource-claim controller drops it from
// status.reservedFor, deallocates, and garbage-collects template-generated
// claims, so a missing status entry, a NotFound, or a deallocated claim are
// all legitimate outcomes of a SUCCESSFUL run. The behavioral verdict rests
// on the pod having run with the injected channel device.
func recordIMEXGeneratedClaim(ctx *validators.Context, dynClient dynamic.Interface, version string, pod *corev1.Pod) {
	const label = "Generated ResourceClaim status"
	const unavailable = "post-terminal claim state unavailable"
	equivalent := fmt.Sprintf("kubectl get resourceclaims -n %s -o wide", pod.Namespace)

	name := imexGeneratedClaimName(pod)
	if name == "" {
		recordRawTextArtifact(ctx, label, equivalent, fmt.Sprintf(
			"%s: pod.status.resourceClaimStatuses carries no generated claim name for pod-local claim %q; the behavioral verdict does not depend on it",
			unavailable, imexPodClaimName))
		return
	}
	obj, err := dynClient.Resource(draGVRAt(version, "resourceclaims")).Namespace(pod.Namespace).Get(
		ctx.Ctx, name, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(err):
		recordRawTextArtifact(ctx, label, equivalent, fmt.Sprintf(
			"%s: generated claim %s already deleted — the resource-claim controller garbage-collects a completed pod's template-generated claim; the behavioral verdict does not depend on it",
			unavailable, name))
		return
	case err != nil:
		recordRawTextArtifact(ctx, label, equivalent, fmt.Sprintf(
			"%s: failed to read generated claim %s: %v; the behavioral verdict does not depend on it", unavailable, name, err))
		return
	}
	_, allocated, _ := unstructured.NestedMap(obj.Object, "status", "allocation")
	reservedFor, _, _ := unstructured.NestedSlice(obj.Object, "status", "reservedFor")
	recordRawTextArtifact(ctx, label, equivalent, strings.Join([]string{
		fmt.Sprintf("Name:        %s/%s", pod.Namespace, name),
		fmt.Sprintf("Allocated:   %t", allocated),
		fmt.Sprintf("ReservedFor: %d", len(reservedFor)),
		"(post-terminal snapshot; a released or deallocated claim here is expected after the pod completed)",
	}, "\n"))
}

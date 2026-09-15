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
	recordRawTextArtifact(ctx, "IMEX candidate nodes",
		fmt.Sprintf("kubectl get nodes -l %s", labelNVIDIAGPUClique),
		fmt.Sprintf("Clique-labeled nodes:         %s\nWith usable compute-domain:   %s",
			strings.Join(cliqueNodes, ","), strings.Join(candidates, ",")))

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
		return err
	}

	// Evidence BEFORE the verdict: status, logs, and the generated claim's
	// (best-effort) post-terminal state ship even when the run fails.
	podLines := []string{
		fmt.Sprintf("Name:      %s/%s", pod.Namespace, pod.Name),
		fmt.Sprintf("Phase:     %s", pod.Status.Phase),
		fmt.Sprintf("Node:      %s", valueOrUnknown(pod.Spec.NodeName)),
		fmt.Sprintf("Claims:    %d", len(pod.Spec.ResourceClaims)),
		fmt.Sprintf("Generated: %s", valueOrUnknown(imexGeneratedClaimName(pod))),
	}
	recordRawTextArtifact(ctx, "IMEX pod status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", run.imexPodName, run.namespace), strings.Join(podLines, "\n"))
	recordGPUPodContainerLogs(ctx, pod, "IMEX pod logs")
	recordIMEXGeneratedClaim(ctx, dynClient, version, pod)

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

// waitForIMEXClaimTemplate waits, bounded by imexClaimTemplateTimeout,
// until the DRA driver has reconciled the ComputeDomain into the
// ResourceClaimTemplate named in its spec. Watch API (repo rule: watch, don't
// poll), with Get fast paths before and after establishing the watch — the
// driver may reconcile between the two calls and the watch would not replay
// that Added event — and a re-Get when the watch channel closes without the
// deadline firing (apiserver hiccup, LB drop), per the repo anti-pattern list.
func waitForIMEXClaimTemplate(ctx context.Context, dynClient dynamic.Interface, version string, run *gpuTestRun) error {
	waitCtx, cancel := context.WithTimeout(ctx, imexClaimTemplateTimeout)
	defer cancel()

	rctClient := dynClient.Resource(draGVRAt(version, "resourceclaimtemplates")).Namespace(run.namespace)
	timeoutErr := func(cause error) error {
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
			"DRA driver did not reconcile ComputeDomain %s into ResourceClaimTemplate %s within %s",
			run.computeDomainName, run.claimTemplateName, imexClaimTemplateTimeout), cause)
	}
	// present reports whether the template exists; a non-NotFound read error
	// is returned as-is (classified).
	present := func() (bool, error) {
		_, err := rctClient.Get(waitCtx, run.claimTemplateName, metav1.GetOptions{})
		switch {
		case err == nil:
			return true, nil
		case k8serrors.IsNotFound(err):
			return false, nil
		case waitCtx.Err() != nil:
			return false, timeoutErr(err)
		default:
			return false, classifyK8sReadError(err, fmt.Sprintf("IMEX ResourceClaimTemplate %s", run.claimTemplateName))
		}
	}

	for {
		if ok, err := present(); err != nil || ok {
			return err
		}
		watcher, err := rctClient.Watch(waitCtx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + run.claimTemplateName,
		})
		if err != nil {
			if waitCtx.Err() != nil {
				return timeoutErr(err)
			}
			return errors.Wrap(errors.ErrCodeInternal, "failed to watch IMEX ResourceClaimTemplate", err)
		}
		// Re-check after the watch is established (see doc comment).
		if ok, err := present(); err != nil || ok {
			watcher.Stop()
			return err
		}
		closed := false
		for !closed {
			select {
			case <-waitCtx.Done():
				watcher.Stop()
				return timeoutErr(waitCtx.Err())
			case event, ok := <-watcher.ResultChan():
				if !ok {
					// Closed without cancellation: loop back to re-Get and
					// re-watch rather than failing a healthy run.
					closed = true
					continue
				}
				if event.Type == watch.Added || event.Type == watch.Modified {
					watcher.Stop()
					return nil
				}
			}
		}
		watcher.Stop()
	}
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

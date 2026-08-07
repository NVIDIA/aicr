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
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	podutil "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	nvidiaSMIPodTemplateFile = "testdata/nvidia-smi-verify-pod.yaml"
	gpuCheckSuccessMsg       = "GPU_CHECK_SUCCESS"
	nvidiaSMILogContextLines = 20

	// gpuOperatorComponent is the recipe componentRef name that supplies the GPU
	// driver stack — and therefore the GPU nodes this check verifies. When the
	// resolved recipe declares it, a cluster with zero GPU nodes is a
	// declared-but-absent prerequisite (#2122), not benign inapplicability: a
	// GPU-less cluster must BLOCK the gate rather than PASS by skipping. Only a
	// recipe that does not declare gpu-operator (standalone runs carry no
	// ComponentRefs, #1327) treats GPU absence as genuine inapplicability.
	gpuOperatorComponent = "gpu-operator"

	// skipReason* are the low-cardinality enum codes emitted via EmitExtra when
	// the check skips. Unlike the human-readable RESULT/enumeration stdout lines
	// (which the default redaction policy strips), these survive minimal
	// redaction (see pkg/evidence/redact), so a signed bundle records WHY a check
	// was skipped. Each must accurately describe the observed condition and be
	// mirrored in redact.ctrfSkipReasons — a code missing from that closed set is
	// dropped from the published bundle.
	skipReasonNoGPUNodes            = "no-gpu-nodes"             // cluster has no GPU nodes at all
	skipReasonNoSchedulableGPUNodes = "no-schedulable-gpu-nodes" // GPU nodes exist but all cordoned/unschedulable
	skipReasonNodesBusy             = "nodes-busy"               // schedulable GPU nodes exist but are busy with workloads
)

// gpuNodeCoverage partitions check-nvidia-smi's discovered GPU nodes into the
// schedulable cohort actually validated and the cordoned cohort skipped. It
// exists so the disclosure text is a pure, independently testable function of
// the partition rather than interleaved fmt.Printf calls — and so every exit
// path (not just the success path) prints the same coverage line. See
// docs/contributor/validator.md and issue #1668.
type gpuNodeCoverage struct {
	total       int
	schedulable []v1.Node
	cordoned    []string
}

// partitionGpuNodes splits allNodes into schedulable and cordoned buckets.
func partitionGpuNodes(ctx context.Context, allNodes []helper.GpuNode) (gpuNodeCoverage, error) {
	c := gpuNodeCoverage{total: len(allNodes)}
	for _, n := range allNodes {
		select {
		case <-ctx.Done():
			return gpuNodeCoverage{}, errors.Wrap(errors.ErrCodeTimeout,
				"canceled while partitioning GPU nodes by cordon state", ctx.Err())
		default:
		}
		if n.Cordoned {
			c.cordoned = append(c.cordoned, n.Node.Name)
			continue
		}
		c.schedulable = append(c.schedulable, n.Node)
	}
	return c, nil
}

// enumerationLines renders the discovered-node listing: total/schedulable/
// cordoned counts, each schedulable node by name, and each cordoned node
// explicitly marked "skipped (cordoned)" rather than omitted from the count.
func (c gpuNodeCoverage) enumerationLines() []string {
	if c.total == 0 {
		return []string{"Found 0 GPU node(s)."}
	}
	lines := make([]string, 0, 1+len(c.schedulable)+len(c.cordoned))
	lines = append(lines, fmt.Sprintf("Found %d GPU node(s), %d schedulable, %d cordoned:",
		c.total, len(c.schedulable), len(c.cordoned)))
	for _, node := range c.schedulable {
		lines = append(lines, fmt.Sprintf("  %s", node.Name))
	}
	for _, name := range c.cordoned {
		lines = append(lines, fmt.Sprintf("  %s: skipped (cordoned)", name))
	}
	return lines
}

// coverageLine renders the nodesValidated disclosure for however many
// schedulable nodes were actually validated when the check exits — 0 when it
// exits before attempting any (all-cordoned skip, busy skip). The "RESULT: "
// prefix is the validator runtime's own convention (pkg/validator/validator.go
// resultSummaryPrefix) for echoing a stdout line into live CLI output at INFO
// level, not just the CTRF report's Stdout array — without it this line is
// only visible after the run, in the (possibly redacted) report.
func (c gpuNodeCoverage) coverageLine(validated int) string {
	if len(c.cordoned) == 0 {
		return fmt.Sprintf("RESULT: nodesValidated: %d/%d", validated, c.total)
	}
	return fmt.Sprintf("RESULT: nodesValidated: %d/%d (%d cordoned, skipped)",
		validated, c.total, len(c.cordoned))
}

func printLines(lines ...string) {
	for _, l := range lines {
		fmt.Println(l)
	}
}

// checkNvidiaSMI verifies that nvidia-smi works correctly on every schedulable
// GPU node. Cordoned GPU nodes narrow the verified scope, so they are
// reported explicitly (never silently dropped from the node count) and the
// evidence records how many of the cluster's GPU nodes were actually
// verified, on every exit path — see docs/contributor/validator.md and issue
// #1668.
func checkNvidiaSMI(ctx *validators.Context) error {
	allNodes, err := helper.FindGpuNodes(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return err
	}

	coverage, err := partitionGpuNodes(ctx.Ctx, allNodes)
	if err != nil {
		return err
	}
	printLines(coverage.enumerationLines()...)

	if len(allNodes) == 0 {
		printLines(coverage.coverageLine(0))
		// #2122 applicability gate. gpu-operator supplies the GPU nodes this check
		// verifies. When the recipe DECLARES it, zero GPU nodes is a
		// declared-but-absent prerequisite — fail closed so a GPU-less cluster
		// cannot PASS conformance by masquerading absence as an inapplicable Skip.
		// A recipe that does NOT declare gpu-operator (standalone #1327 runs carry
		// no ComponentRefs) keeps the original Skip: GPU verification is genuinely
		// out of scope. The coverageLine above satisfies the #1668 node-count
		// disclosure on both exit paths.
		if validators.RecipeDeclares(ctx, gpuOperatorComponent) {
			return errors.New(errors.ErrCodeNotFound,
				"recipe declares gpu-operator but the cluster has no GPU nodes to verify — "+
					"check node provisioning, the GPU Operator rollout, or validator RBAC")
		}
		emitExtraOrWarn(nvidiaSMISkipExtra(skipReasonNoGPUNodes))
		return validators.Skip("no GPU nodes found in the cluster")
	}

	gpuNodes := coverage.schedulable
	if len(gpuNodes) == 0 {
		printLines(coverage.coverageLine(0))
		// #2122 KEEP: this is a genuine scope-narrowing Skip, not a masqueraded
		// absence. The GPU-node prerequisite IS satisfied (GPU nodes exist); they
		// are merely administratively cordoned (Spec.Unschedulable) — an
		// intentional operator action, not an absent prerequisite or an infra
		// probe error. There is nothing schedulable to verify, so Skip with a
		// distinct, accurate code — never a false "no-gpu-nodes" in the signed
		// evidence.
		emitExtraOrWarn(nvidiaSMISkipExtra(skipReasonNoSchedulableGPUNodes))
		return validators.Skip(fmt.Sprintf(
			"all %d GPU node(s) are cordoned; nothing to verify", len(coverage.cordoned)))
	}

	// Check if any nodes are busy.
	// A probe error is treated as busy (fail-safe: don't run on a node we can't
	// clear), but tracked separately from CONFIRMED occupancy so an all-errors
	// outcome is not signed as "nodes-busy" — and, per #2122, is not skipped at
	// all. firstProbeErr preserves the probe's error class for the fail-closed
	// path below.
	var busyNodes []string
	var firstProbeErr error
	confirmedBusy := false
	for _, node := range gpuNodes {
		busy, busyErr := helper.IsNodeGpuBusy(ctx.Ctx, ctx.Clientset, node.Name)
		if busyErr != nil {
			slog.Warn("error checking busy status, treating as busy", "node", node.Name, "error", busyErr)
			busyNodes = append(busyNodes, node.Name)
			if firstProbeErr == nil {
				firstProbeErr = busyErr
			}
			continue
		}
		if busy {
			busyNodes = append(busyNodes, node.Name)
			confirmedBusy = true
		}
	}

	if len(busyNodes) > 0 {
		printLines(coverage.coverageLine(0))
		if confirmedBusy {
			// At least one node was CONFIRMED occupied: a legitimate
			// scope-narrowing Skip. Sign nodes-busy so the signed evidence records
			// the occupancy. Probe errors on other nodes are already logged above.
			emitExtraOrWarn(nvidiaSMISkipExtra(skipReasonNodesBusy))
			return validators.Skip(fmt.Sprintf("GPU nodes busy with existing workloads: %v", busyNodes))
		}
		// #2122 fail-closed: every "busy" node was actually a busy-probe ERROR —
		// the probe proved occupancy on no node. An infra error (RBAC denial,
		// timeout, transport failure) masquerading as a busy Skip would let an
		// unreadable cluster PASS conformance. Block the gate and preserve the
		// probe's error class instead of flattening it to a Skip.
		return errors.PropagateOrWrap(firstProbeErr, errors.ErrCodeInternal,
			"could not determine GPU node occupancy — the busy-probe failed on all schedulable GPU nodes")
	}

	fmt.Printf("All %d GPU node(s) available. Verifying...\n", len(gpuNodes))

	// Verify each GPU node
	results := make(map[string]error)
	for _, node := range gpuNodes {
		slog.Info("verifying node", "node", node.Name)
		if verifyErr := verifySingleGPUNode(ctx, node.Name); verifyErr != nil {
			results[node.Name] = verifyErr
			fmt.Printf("  %s: FAILED (%v)\n", node.Name, verifyErr)
		} else {
			results[node.Name] = nil
			fmt.Printf("  %s: OK\n", node.Name)
		}
	}

	// Report results
	var failedNodes []string
	validated := 0
	for nodeName, nodeErr := range results {
		if nodeErr != nil {
			failedNodes = append(failedNodes, fmt.Sprintf("%s (%v)", nodeName, nodeErr))
			continue
		}
		validated++
	}

	// Emit the structured coverage disclosure (survives minimal redaction)
	// alongside the human RESULT line, on BOTH the pass and fail paths, so a
	// signed bundle records how many of the cluster's GPU nodes were verified.
	emitExtraOrWarn(nvidiaSMICoverageExtra(validated, coverage.total))

	if len(failedNodes) > 0 {
		printLines(coverage.coverageLine(validated))
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GPU verification failed on %d/%d nodes: %v",
				len(failedNodes), len(gpuNodes), failedNodes))
	}

	printLines(coverage.coverageLine(validated))
	fmt.Printf("Successfully verified GPU on all %d schedulable node(s)\n", len(gpuNodes))
	return nil
}

// nvidiaSMICoverageExtra builds the structured coverage disclosure: how many GPU
// nodes passed verification (validated) out of the total present incl. cordoned
// (from the same node snapshot). Unlike the RESULT stdout line it survives
// minimal redaction. Values are counts only — never node names or IPs.
func nvidiaSMICoverageExtra(validated, total int) map[string]string {
	return map[string]string{
		"nodesValidated": strconv.Itoa(validated),
		"nodesTotal":     strconv.Itoa(total),
	}
}

// nvidiaSMISkipExtra builds the structured skip disclosure carrying only the
// low-cardinality reason enum code.
func nvidiaSMISkipExtra(reason string) map[string]string {
	return map[string]string{"skipReason": reason}
}

// emitExtraOrWarn emits structured extra evidence, logging (never failing) on
// error — a failed stdout write must not flip the check's verdict.
func emitExtraOrWarn(extra map[string]string) {
	if err := validators.EmitExtra(extra); err != nil {
		slog.Warn("failed to emit validator extra", "error", err)
	}
}

func verifySingleGPUNode(ctx *validators.Context, nodeName string) error {
	podSuffix := sanitizeNodeName(nodeName)
	templateData := map[string]string{
		"POD_SUFFIX": podSuffix,
		"NODE_NAME":  nodeName,
		"NAMESPACE":  ctx.Namespace,
		"IMAGE":      getNvidiaSMIImage(ctx),
	}

	slog.Info("deploying nvidia-smi verify pod",
		"node", nodeName,
		"podName", "nvidia-smi-verify-"+podSuffix,
		"image", templateData["IMAGE"],
		"namespace", ctx.Namespace)

	// Load pod from template. The template uses tolerate-all (operator: Exists)
	// since the pod is pinned to a specific node via nodeName.
	pod, err := helper.LoadPodFromTemplate(nvidiaSMIPodTemplateFile, templateData)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to load pod template", err)
	}

	createdPod, err := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Create(ctx.Ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create verification pod", err)
	}

	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		if cleanupErr := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Delete(cleanupCtx, createdPod.Name, metav1.DeleteOptions{}); cleanupErr != nil {
			slog.Warn("failed to cleanup pod", "namespace", createdPod.Namespace, "pod", createdPod.Name, "error", cleanupErr)
		}
	}()

	// Use pkg/k8s/pod utilities directly.
	waitErr := podutil.WaitForPodSucceeded(ctx.Ctx, ctx.Clientset, ctx.Namespace, createdPod.Name, defaults.PodWaitTimeout)

	podLogs, logErr := podutil.GetPodLogs(ctx.Ctx, ctx.Clientset, ctx.Namespace, createdPod.Name, "")
	if logErr != nil {
		slog.Warn("failed to get logs for pod", "node", nodeName, "error", logErr)
		podLogs = fmt.Sprintf("failed to retrieve pod logs: %v", logErr)
	}

	if waitErr != nil {
		logSnippet := getLogSnippet(podLogs, nvidiaSMILogContextLines)

		// Capture pod status and events for debugging.
		debugInfo := collectPodDebugInfo(ctx, createdPod.Name)

		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("pod failed on node %s\n%s\nFirst %d lines:\n%s",
				nodeName, debugInfo, nvidiaSMILogContextLines, logSnippet), waitErr)
	}

	return verifyNvidiaSMILogs(podLogs, createdPod)
}

func getLogSnippet(logs string, maxLines int) string {
	lines := strings.Split(logs, "\n")
	if len(lines) <= maxLines {
		return logs
	}
	return strings.Join(lines[:maxLines], "\n")
}

func verifyNvidiaSMILogs(podLogs string, pod *v1.Pod) error {
	requiredMarkerGroups := [][]string{
		{"NVIDIA-SMI"},
		{"Driver Version:", "KMD Version:"},
		{"CUDA Version:", "CUDA UMD Version:"},
		{gpuCheckSuccessMsg},
	}

	// Match case-insensitively: the renamed banner fields are only documented
	// via `nvidia-smi --version` deprecation text, which spells them lowercase
	// ("KMD version"), and no captured plain-`nvidia-smi` banner pins the
	// exact casing of the table header (issue #1667). Case-insensitivity
	// accepts either spelling — and future casing tweaks — without false
	// positives: the verified log is only nvidia-smi output plus the success
	// echo. The diagnostic below keeps the canonical casing for readability.
	logsLower := strings.ToLower(podLogs)

	var missing []string
	for _, markerGroup := range requiredMarkerGroups {
		found := false
		for _, marker := range markerGroup {
			if strings.Contains(logsLower, strings.ToLower(marker)) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, strings.Join(markerGroup, " or "))
		}
	}

	if len(missing) > 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("log verification failed for pod %s/%s: missing [%s]",
				pod.Namespace, pod.Name, strings.Join(missing, "; ")))
	}

	return nil
}

func getNvidiaSMIImage(_ *validators.Context) string {
	// Default image for most GPU types.
	// Future: read accelerator type from recipe to select GB200-specific image.
	return "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04"
}

// sanitizeNodeName converts a node name (e.g., ip-10-0-135-83.ec2.internal)
// into a valid DNS label for use in pod names (replaces dots with dashes).
func sanitizeNodeName(nodeName string) string {
	return strings.ReplaceAll(strings.ToLower(nodeName), ".", "-")
}

// collectPodDebugInfo captures container status and events for a failed pod.
func collectPodDebugInfo(ctx *validators.Context, podName string) string {
	var info strings.Builder

	pod, err := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Get(ctx.Ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf("could not retrieve pod status: %v", err)
	}

	// Container status
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			fmt.Fprintf(&info, "Container %s: waiting (%s: %s)\n", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
		if cs.State.Terminated != nil {
			fmt.Fprintf(&info, "Container %s: terminated (reason=%s exitCode=%d message=%s)\n",
				cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		}
	}

	// Pod events
	events, evtErr := ctx.Clientset.CoreV1().Events(ctx.Namespace).List(ctx.Ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + podName,
	})
	if evtErr == nil {
		for _, evt := range events.Items {
			if evt.Type == "Warning" {
				fmt.Fprintf(&info, "Event: %s — %s\n", evt.Reason, evt.Message)
			}
		}
	}

	if info.Len() == 0 {
		return "no additional debug info available"
	}
	return info.String()
}

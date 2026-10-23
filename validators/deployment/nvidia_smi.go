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
	"regexp"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/constraints"
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

	// skipReason* are the codes this check publishes when it skips.
	skipReasonNoGPUNodes            = validators.SkipReasonNoGPUNodes            // cluster has no GPU nodes at all
	skipReasonNoSchedulableGPUNodes = validators.SkipReasonNoSchedulableGPUNodes // GPU nodes exist but all cordoned/unschedulable
	skipReasonNodesBusy             = validators.SkipReasonNodesBusy             // schedulable GPU nodes exist but are busy with workloads

	// skipCause* label a single node's SKIPPED line and its bucket in the
	// RESULT line.
	skipCauseBusy       = "busy"
	skipCauseProbeError = "busy-probe error"
	skipCauseCordoned   = "cordoned"

	// gpuDriverVersionConstraint is the deployment-phase constraint name recipes
	// use to declare a host-driver floor (issue #1995). Platforms with
	// host-managed drivers (e.g. GKE A4X Max requiring R580.95.05+) cannot be
	// gated by GPU Operator version alone when driver.enabled is false; this
	// constraint is evaluated against the nvidia-smi banner on each verified
	// node. The value must carry a comparison operator (typically ">=") to
	// behave as a floor; a bare version is exact string match. Absent from the
	// recipe, the check keeps its original banner-presence behavior and does
	// not invent a floor. When the constraint is set but no node can be
	// measured (no GPU nodes, all cordoned, or all busy), the check fails closed
	// instead of Skip — a declared gate must not PASS unenforced.
	gpuDriverVersionConstraint = "Deployment.gpu-driver.version"
)

// nvidiaSMIDriverVersionRE extracts the host driver / KMD version from an
// nvidia-smi banner. Matches both legacy ("Driver Version:") and renamed
// ("KMD Version:") fields, case-insensitively, including the table-row layout
// where fields sit on one pipe-delimited line (issue #1667). Horizontal
// whitespace only — both between the field words and after the colon — so a
// split label ("Driver\nVersion:") or a version on the next line is not
// treated as the field. Caps at three numeric components — NVIDIA driver
// versions are Major.Minor.Patch. Truncation of a longer version
// (e.g. "580.95.05.1") is rejected in parseNvidiaSMIDriverVersion: Go's RE2
// engine has no negative lookahead.
var nvidiaSMIDriverVersionRE = regexp.MustCompile(
	`(?i)(?:driver|kmd)[ \t]+version:[ \t]*([0-9]+(?:\.[0-9]+){0,2})`)

// gpuNodeCoverage records check-nvidia-smi's per-node outcome across every
// discovered GPU node. Cordoned nodes are skipped before the busy-probe, busy
// nodes (and nodes whose busy-probe errored) are skipped after it, and the
// remaining schedulable nodes are verified and either pass or fail. The
// disclosure text is a pure function of these buckets so every exit path
// prints the same summary. See docs/contributor/validator.md and issues #1668
// and #3361.
type gpuNodeCoverage struct {
	total       int
	schedulable []v1.Node
	cordoned    []string
	busy        []string
	probeErrors []string
	passed      int
	failed      int
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

// enumerationLines renders the discovered-node header and a SKIPPED line for
// each cordoned node. Each schedulable node gets its own line once its
// outcome is known.
func (c gpuNodeCoverage) enumerationLines() []string {
	if c.total == 0 {
		return []string{"Found 0 GPU node(s)."}
	}
	lines := make([]string, 0, 1+len(c.cordoned))
	lines = append(lines, fmt.Sprintf("Found %d GPU node(s), %d schedulable, %d cordoned:",
		c.total, len(c.schedulable), len(c.cordoned)))
	for _, name := range c.cordoned {
		lines = append(lines, nodeSkippedLine(name, skipCauseCordoned))
	}
	return lines
}

func nodePassedLine(name string) string { return fmt.Sprintf("  %s: PASSED", name) }

func nodeFailedLine(name string, err error) string {
	return fmt.Sprintf("  %s: FAILED (%v)", name, err)
}

func nodeSkippedLine(name, cause string) string {
	return fmt.Sprintf("  %s: SKIPPED (%s)", name, cause)
}

func (c gpuNodeCoverage) skipped() int {
	return len(c.cordoned) + len(c.busy) + len(c.probeErrors)
}

// coverageLine renders the nodesValidated disclosure with the passed, failed,
// and skipped counts, and the skips broken down by cause. nodesValidated is
// the passed count, so it is 0 on the paths that exit before verifying any
// node. The busy-probe error count appears only when nonzero. The "RESULT: "
// prefix is resultSummaryPrefix in pkg/validator/validator.go.
func (c gpuNodeCoverage) coverageLine() string {
	if c.total == 0 {
		return "RESULT: nodesValidated: 0/0"
	}
	causes := fmt.Sprintf("%s %d", skipCauseBusy, len(c.busy))
	if len(c.probeErrors) > 0 {
		causes += fmt.Sprintf(", %s %d", skipCauseProbeError, len(c.probeErrors))
	}
	causes += fmt.Sprintf(", %s %d", skipCauseCordoned, len(c.cordoned))
	return fmt.Sprintf("RESULT: nodesValidated: %d/%d (passed %d, failed %d, skipped %d: %s)",
		c.passed, c.total, c.passed, c.failed, c.skipped(), causes)
}

// coverageExtra builds the structured coverage disclosure. nodesValidated is
// the passed count; nodesFailed and nodesSkipped account for the rest of
// nodesTotal, all from the same node snapshot, except nodes an expired
// deadline kept the check from reaching. Unlike the RESULT stdout line
// it survives minimal redaction. Values are counts only — never node names or
// IPs.
func (c gpuNodeCoverage) coverageExtra() map[string]string {
	return map[string]string{
		validators.ExtraKeyNodesValidated: strconv.Itoa(c.passed),
		validators.ExtraKeyNodesTotal:     strconv.Itoa(c.total),
		validators.ExtraKeyNodesFailed:    strconv.Itoa(c.failed),
		validators.ExtraKeyNodesSkipped:   strconv.Itoa(c.skipped()),
	}
}

// stopOnDeadContext publishes the coverage reached so far and returns a
// timeout (or cancellation) error. Nodes not yet reached are in no bucket, so
// passed, failed, and skipped can sum to less than total; they are never
// counted as failed.
func (c gpuNodeCoverage) stopOnDeadContext(ctx context.Context) error {
	printLines(c.coverageLine())
	emitExtraOrWarn(c.coverageExtra())
	return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout,
		fmt.Sprintf("check-nvidia-smi stopped before reaching every GPU node (%d passed, %d failed, %d skipped of %d)",
			c.passed, c.failed, c.skipped(), c.total))
}

func printLines(lines ...string) {
	for _, l := range lines {
		fmt.Println(l)
	}
}

// checkNvidiaSMI verifies that nvidia-smi works correctly on every schedulable
// GPU node that is not busy with an existing workload. Cordoned and busy GPU
// nodes narrow the verified scope, so each is reported as SKIPPED (never
// silently dropped from the node count), and the evidence records how many of
// the cluster's GPU nodes passed, failed, and were skipped, on every exit
// path — see docs/contributor/validator.md and issues #1668 and #3361.
func checkNvidiaSMI(ctx *validators.Context) error {
	return runNvidiaSMICheck(ctx, func(nodeName string) error {
		return verifySingleGPUNode(ctx, nodeName)
	})
}

// runNvidiaSMICheck is checkNvidiaSMI with per-node verification injected, so
// the outcome logic is testable without a pod lifecycle.
func runNvidiaSMICheck(ctx *validators.Context, verifyNode func(nodeName string) error) error {
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
		printLines(coverage.coverageLine())
		emitExtraOrWarn(coverage.coverageExtra())
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
		if err := failClosedIfUnmeasurableGPUDriverFloor(ctx,
			"no GPU nodes found in the cluster"); err != nil {
			return err
		}
		emitExtraOrWarn(nvidiaSMISkipExtra(skipReasonNoGPUNodes))
		return validators.Skip("no GPU nodes found in the cluster")
	}

	gpuNodes := coverage.schedulable
	if len(gpuNodes) == 0 {
		printLines(coverage.coverageLine())
		emitExtraOrWarn(coverage.coverageExtra())
		// #2122 KEEP: this is a genuine scope-narrowing Skip, not a masqueraded
		// absence. The GPU-node prerequisite IS satisfied (GPU nodes exist); they
		// are merely administratively cordoned (Spec.Unschedulable) — an
		// intentional operator action, not an absent prerequisite or an infra
		// probe error. There is nothing schedulable to verify, so Skip with a
		// distinct, accurate code — never a false "no-gpu-nodes" in the signed
		// evidence.
		reason := fmt.Sprintf(
			"all %d GPU node(s) are cordoned; nothing to verify", len(coverage.cordoned))
		if err := failClosedIfUnmeasurableGPUDriverFloor(ctx, reason); err != nil {
			return err
		}
		emitExtraOrWarn(nvidiaSMISkipExtra(skipReasonNoSchedulableGPUNodes))
		return validators.Skip(reason)
	}

	// Probe each schedulable node for GPU occupancy immediately before
	// verifying it, so a job that claims a node's GPUs while earlier nodes are
	// verified skips that node instead of failing it. A probe error is treated
	// as busy (fail-safe: don't run on a node we can't clear), but tracked
	// separately from CONFIRMED occupancy so an all-errors outcome is not
	// signed as "nodes-busy" — and, per #2122, is not skipped at all.
	// firstProbeErr preserves the probe's error class for that fail-closed
	// path.
	fmt.Printf("Verifying %d schedulable GPU node(s)...\n", len(gpuNodes))

	var failedNodes []string
	var firstProbeErr error
	for _, node := range gpuNodes {
		if ctx.Ctx.Err() != nil {
			return coverage.stopOnDeadContext(ctx.Ctx)
		}
		busy, busyErr := helper.IsNodeGpuBusy(ctx.Ctx, ctx.Clientset, node.Name)
		switch {
		case busyErr != nil:
			if ctx.Ctx.Err() != nil {
				// The deadline interrupted the probe; the node was not found busy.
				return coverage.stopOnDeadContext(ctx.Ctx)
			}
			slog.Warn("error checking busy status, treating as busy", "node", node.Name, "error", busyErr)
			coverage.probeErrors = append(coverage.probeErrors, node.Name)
			printLines(nodeSkippedLine(node.Name, skipCauseProbeError))
			if firstProbeErr == nil {
				firstProbeErr = busyErr
			}
			continue
		case busy:
			coverage.busy = append(coverage.busy, node.Name)
			printLines(nodeSkippedLine(node.Name, skipCauseBusy))
			continue
		}

		slog.Info("verifying node", "node", node.Name)
		if verifyErr := verifyNode(node.Name); verifyErr != nil {
			if ctx.Ctx.Err() != nil {
				// The deadline interrupted this node; it was not found failing.
				return coverage.stopOnDeadContext(ctx.Ctx)
			}
			coverage.failed++
			failedNodes = append(failedNodes, fmt.Sprintf("%s (%v)", node.Name, verifyErr))
			printLines(nodeFailedLine(node.Name, verifyErr))
			continue
		}
		coverage.passed++
		printLines(nodePassedLine(node.Name))
	}

	// Emit the structured coverage disclosure (survives minimal redaction)
	// alongside the human RESULT line, on BOTH the pass and fail paths, so a
	// signed bundle records partial coverage, not only the passed count.
	printLines(coverage.coverageLine())
	emitExtraOrWarn(coverage.coverageExtra())

	if coverage.passed+coverage.failed == 0 {
		return noFreeGPUNodeOutcome(ctx, coverage, firstProbeErr)
	}

	if len(failedNodes) > 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GPU verification failed on %d/%d verified nodes: %v",
				len(failedNodes), coverage.passed+coverage.failed, failedNodes))
	}

	if skipped := coverage.skipped(); skipped > 0 {
		fmt.Printf("Verified GPU on %d of %d GPU node(s); %d skipped\n",
			coverage.passed, coverage.total, skipped)
		return nil
	}
	fmt.Printf("Successfully verified GPU on all %d GPU node(s)\n", coverage.passed)
	return nil
}

// noFreeGPUNodeOutcome decides the result when the busy-probe left no node to
// verify. The caller has already published coverage.
func noFreeGPUNodeOutcome(ctx *validators.Context, coverage gpuNodeCoverage, firstProbeErr error) error {
	if len(coverage.busy) > 0 {
		// No node is free and at least one was CONFIRMED occupied. Without a
		// host-driver floor this is a legitimate scope-narrowing Skip. With a
		// declared floor, occupancy is not a reason to leave the gate
		// unevaluated (#1995): Skip is non-blocking, so a below-floor cluster
		// would PASS.
		reason := fmt.Sprintf("GPU nodes busy with existing workloads: %v",
			append(append([]string{}, coverage.busy...), coverage.probeErrors...))
		if err := failClosedIfUnmeasurableGPUDriverFloor(ctx, reason); err != nil {
			return err
		}
		emitExtraOrWarn(nvidiaSMISkipExtra(skipReasonNodesBusy))
		return validators.Skip(reason)
	}
	// #2122 fail-closed: the busy-probe errored on every schedulable node, so
	// it proved occupancy on none. An infra error (RBAC denial, timeout,
	// transport failure) masquerading as a busy Skip would let an unreadable
	// cluster PASS conformance. Block the gate and preserve the probe's error
	// class instead of flattening it to a Skip.
	return errors.PropagateOrWrap(firstProbeErr, errors.ErrCodeInternal,
		"could not determine GPU node occupancy — the busy-probe failed on all schedulable GPU nodes")
}

// nvidiaSMISkipExtra builds the structured skip disclosure carrying only the
// low-cardinality reason enum code.
func nvidiaSMISkipExtra(reason string) map[string]string {
	return map[string]string{"skipReason": reason}
}

// failClosedIfUnmeasurableGPUDriverFloor returns a blocking error when the
// recipe declares Deployment.gpu-driver.version but check-nvidia-smi cannot
// run per-node verification (no GPU nodes, all cordoned, or all busy). Skip on
// those paths is non-blocking; a declared floor that cannot be measured must
// not PASS (#1995). No constraint keeps the existing Skip.
func failClosedIfUnmeasurableGPUDriverFloor(ctx *validators.Context, reason string) error {
	expr, found := findDeploymentConstraint(ctx, gpuDriverVersionConstraint)
	if !found {
		return nil
	}
	return errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("%s %q is set but the host driver version could not be measured (%s)",
			gpuDriverVersionConstraint, expr, reason))
}

// emitExtraOrWarn calls validators.EmitExtraOrWarn.
func emitExtraOrWarn(extra map[string]string) { validators.EmitExtraOrWarn(extra) }

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

	if err := verifyNvidiaSMILogs(podLogs, createdPod); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("nvidia-smi log verification failed on node %s", nodeName), err)
	}
	return enforceGPUDriverVersionFloor(ctx, podLogs, nodeName)
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

// parseNvidiaSMIDriverVersion extracts the host driver version from nvidia-smi
// banner output. Accepts legacy "Driver Version:" and renamed "KMD Version:"
// fields (issue #1667), case-insensitively. Returns ErrCodeNotFound when no
// parseable version is present so callers can fail closed when a recipe floor
// requires one.
func parseNvidiaSMIDriverVersion(podLogs string) (string, error) {
	loc := nvidiaSMIDriverVersionRE.FindStringSubmatchIndex(podLogs)
	if len(loc) < 4 {
		return "", errors.New(errors.ErrCodeNotFound,
			"nvidia-smi output has no parseable Driver Version / KMD Version")
	}
	// loc[2]:loc[3] is the version capture. The next byte must be a field
	// terminator (EOF, whitespace, or '|'). A trailing '.' ("580.95.05.1")
	// or suffix ("-rc1") would otherwise truncate to a three-component
	// prefix that could falsely satisfy a floor. RE2 has no (?!...)
	// lookahead, so this check lives here rather than in the pattern
	// (issue #1995).
	end := loc[3]
	if !driverVersionFieldTerminated(podLogs, end) {
		return "", errors.New(errors.ErrCodeNotFound,
			"nvidia-smi driver version is not a three-component numeric field")
	}
	// The capture is `[0-9]+(?:\.[0-9]+){0,2}` so loc[2]:loc[3] is never empty
	// once FindStringSubmatchIndex returned a match.
	return podLogs[loc[2]:loc[3]], nil
}

// driverVersionFieldTerminated reports whether the character after a captured
// driver version is a valid field terminator. Real nvidia-smi banners follow
// the version with EOF, whitespace, or a table pipe. Anything else is a
// suffix that would silently truncate (".1", "-rc1", "foo") and must fail
// closed — RE2 has no negative lookahead, so this lives here rather than in
// nvidiaSMIDriverVersionRE.
func driverVersionFieldTerminated(podLogs string, end int) bool {
	if end >= len(podLogs) {
		return true
	}
	switch podLogs[end] {
	case '|', ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

// enforceGPUDriverVersionFloor evaluates Deployment.gpu-driver.version against
// the driver version parsed from nvidia-smi logs (issue #1995). No constraint
// in the recipe is a no-op — the check must not invent a floor. A constraint
// with an unreadable banner fails closed: a host-driver floor that cannot be
// measured must not PASS. Enumeration paths that would Skip (no GPU nodes,
// all cordoned, all busy) use failClosedIfUnmeasurableGPUDriverFloor for the
// same contract before per-node verification runs.
func enforceGPUDriverVersionFloor(ctx *validators.Context, podLogs, nodeName string) error {
	constraintExpr, found := findDeploymentConstraint(ctx, gpuDriverVersionConstraint)
	if !found {
		return nil
	}

	version, err := parseNvidiaSMIDriverVersion(podLogs)
	if err != nil {
		// Fail closed with a contextual message: a declared floor that cannot
		// be measured must not PASS. Keep ErrCodeNotFound so report consumers
		// can distinguish "banner unreadable" from a numeric miss.
		return errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("%s constraint %q is set but could not parse the host driver "+
				"version from nvidia-smi on node %s",
				gpuDriverVersionConstraint, constraintExpr, nodeName), err)
	}

	parsed, err := constraints.ParseConstraintExpression(constraintExpr)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s constraint", gpuDriverVersionConstraint), err)
	}

	passed, err := parsed.Evaluate(version)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("%s constraint evaluation failed on node %s",
				gpuDriverVersionConstraint, nodeName))
	}

	fmt.Printf("  %s: host driver %s, constraint %s → %v\n",
		nodeName, version, constraintExpr, passed)

	if !passed {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("host driver version %s on node %s does not satisfy %s %q",
				version, nodeName, gpuDriverVersionConstraint, constraintExpr))
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

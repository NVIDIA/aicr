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
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	k8spod "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

// grdmaPciTopoCheckOverrideRe matches the params line for
// NVreg_GrdmaPciTopoCheckOverride=1. Multiline-anchored: params carries one
// "Name: value" per line.
var grdmaPciTopoCheckOverrideRe = regexp.MustCompile(`(?m)^GrdmaPciTopoCheckOverride: 1$`)

// parseNVregFromParams reports whether the params content sets the override.
func parseNVregFromParams(content string) bool {
	return grdmaPciTopoCheckOverrideRe.MatchString(content)
}

// nvrmVersionRe extracts the driver version from /proc/driver/nvidia/version:
//
//	NVRM version: NVIDIA UNIX aarch64 Kernel Module  580.173.02  Wed ...
//
// Line-anchored on "NVRM version:" so the banner's GCC version cannot be
// mistaken for the driver's. Group 1 is the full version, group 2 the major.
var nvrmVersionRe = regexp.MustCompile(`(?m)^NVRM version:[^\n]*?\s((\d+)\.\d+(?:\.\d+)?)\b`)

// nvregR595Major is the first NVIDIA driver major version that removed
// NVreg_GrdmaPciTopoCheckOverride, substituting a PCIe topology requirement
// (see nvregOverrideRemovedHint).
const nvregR595Major = 595

// parseNVRMVersion extracts (fullVersion, major) from the version banner.
// ok=false means "cannot determine" — never "assume compatible".
func parseNVRMVersion(content string) (full string, major int, ok bool) {
	m := nvrmVersionRe.FindStringSubmatch(content)
	if m == nil {
		return "", 0, false
	}
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], major, true
}

// nvregVerdict is a node's preflight outcome, kept as a named type so the
// decision is a pure function of the two probed files — testable without a
// cluster or a GPU.
type nvregVerdict int

const (
	// nvregVersionUnknown: version unreadable, so nothing is established.
	// Deliberately the ZERO VALUE so an accidentally-empty result fails closed
	// rather than reading as a pass.
	nvregVersionUnknown nvregVerdict = iota
	// nvregOK: pre-R595 driver with the override set.
	nvregOK
	// nvregFlagMissing: pre-R595 driver, override absent. The operator can set it.
	nvregFlagMissing
	// nvregOverrideRemoved: R595+ removed the override, so the preflight has no
	// way to verify RDMA. Deliberately NOT named "incompatible": the code
	// establishes only that the parameter is gone. Whether the replacement
	// topology requirement is met is measured on EKS p6e and unmeasured on OKE.
	nvregOverrideRemoved
)

// nvregResult pairs a verdict with the driver version that produced it, so the
// operator message can name what was actually detected.
type nvregResult struct {
	verdict nvregVerdict
	version string // empty when the banner could not be parsed
}

// evaluateNVregPreflight decides a node's verdict from the two probed files.
// Version is consulted FIRST: on R595+ the flag cannot exist, so looking for it
// could only produce the misleading "set the flag" advice (#2459).
func evaluateNVregPreflight(versionFile, paramsFile string) nvregResult {
	full, major, ok := parseNVRMVersion(versionFile)
	if !ok {
		return nvregResult{verdict: nvregVersionUnknown}
	}
	if major >= nvregR595Major {
		return nvregResult{verdict: nvregOverrideRemoved, version: full}
	}
	if parseNVregFromParams(paramsFile) {
		return nvregResult{verdict: nvregOK, version: full}
	}
	return nvregResult{verdict: nvregFlagMissing, version: full}
}

const (
	// preflightPodNamePrefix is the generateName seed for the per-node probe
	// pods. Short so the full name (with the apiserver's random generateName
	// suffix) stays well inside the 63-character DNS-1123 label limit.
	preflightPodNamePrefix = "nccl-nvreg-probe-"

	// nvregProbeSeparator delimits the two files in the probe pod's stdout.
	nvregProbeSeparator = "===AICR-NVREG-SPLIT==="

	// nvregDocsHint is emitted when the flag is missing on a pre-R595 driver.
	// Covers both driver-ownership modes, and corrects the reload instruction:
	// deleting the driver pods does NOT reload the module, because
	// k8s-driver-manager keys on a digest of the ClusterPolicy spec rather than
	// the ConfigMap contents (#2459).
	nvregDocsHint = `NVreg_GrdmaPciTopoCheckOverride=1 is required on GB200 nodes; without it NCCL ` +
		`silently falls back to the Socket transport. Deleting the nvidia-driver ` +
		`DaemonSet pods does NOT apply it — k8s-driver-manager keys on a digest of the ` +
		`ClusterPolicy spec, not the ConfigMap. Procedure and the node-image ` +
		`(OKE oci-managed) variant: docs/user/validation.md, "GB200 NET preflight".`

	// nvregOverrideRemovedHint is emitted on R595+. It states only what the code
	// established — the override is gone — and does not assert the hardware is
	// incompatible: that is measured on EKS p6e and unmeasured on OKE.
	nvregOverrideRemovedHint = `NVIDIA driver R595 removed NVreg_GrdmaPciTopoCheckOverride, replacing it with a ` +
		`PCIe topology requirement this preflight does not check — so RDMA cannot be ` +
		`verified here. Measured to fail on EKS p6e; unmeasured on OKE. Remedy is a ` +
		`driver at R580 (AICR ships 580.173.02, #2383) — pinned via the ClusterPolicy, ` +
		`or via the node image where the GPU Operator does not own the driver. ` +
		`See docs/user/validation.md.`

	// nvregUnknownVersionHint is emitted when the version banner is unreadable.
	nvregUnknownVersionHint = `/proc/driver/nvidia/version could not be read, so neither the override nor the ` +
		`driver version could be established and the preflight fails rather than ` +
		`assume. Confirm the NVIDIA kernel module is loaded on every target node. ` +
		`See docs/user/validation.md.`
)

// splitNVregProbeOutput splits the probe pod's stdout into the version and
// params contents. A missing separator yields two empty strings, which
// evaluateNVregPreflight resolves to nvregVersionUnknown (fail-closed).
func splitNVregProbeOutput(out string) (versionFile, paramsFile string) {
	before, after, found := strings.Cut(out, nvregProbeSeparator)
	if !found {
		return "", ""
	}
	return before, after
}

// preflightGB200NetNVregFlag checks each target GPU node for the driver-side
// prerequisite of GPUDirect RDMA over its PCIe-attached NIC. It does not prove
// RDMA works end to end — it establishes that the one setting AICR controls is
// in place. Called only for the NET variant on GB200/EKS and GB200/OKE; NVLS
// traffic stays on NVLink-C2C and does not need it.
//
// The requirement is driver-version dependent: before R595 it is the
// NVreg_GrdmaPciTopoCheckOverride=1 module parameter (R580 is the version AICR
// pins); R595 removed that parameter entirely (#2459).
//
// One short-lived Pod per node reads /proc/driver/nvidia via a read-only
// hostPath; results are consolidated into a single error.
func preflightGB200NetNVregFlag(ctx *validators.Context, nodes []corev1.Node) error {
	if len(nodes) == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			"preflight called with no target nodes")
	}

	slog.Info("NET preflight: checking GPUDirect RDMA prerequisites on GPU nodes",
		"nodes", len(nodes))

	results, err := runPerNodeResultProbe(ctx, nodes, "NVreg", checkNVregOnNode)
	if err != nil {
		return err
	}

	if err := nvregPreflightOutcome(results); err != nil {
		return err
	}

	slog.Info("NET preflight passed: NVreg_GrdmaPciTopoCheckOverride=1 on all target nodes",
		"nodes", len(nodes))
	return nil
}

// nvregPreflightOutcome turns per-node verdicts into a single operator-facing
// error, or nil when every node is OK.
//
// EVERY failing category is reported, not just the first. A mid-rollout cluster
// can hold R595 and R580 nodes at once, and reporting only one kind would send
// the operator round the loop again after fixing it. Categories are ordered
// most-fundamental-first so the case no flag can fix leads.
func nvregPreflightOutcome(results map[string]nvregResult) error {
	var sections []string

	if removed := nodesWithVerdict(results, nvregOverrideRemoved); len(removed) > 0 {
		sections = append(sections, fmt.Sprintf(
			"GPUDirect RDMA cannot be verified on GPU nodes running driver R%d or later: %s. %s",
			nvregR595Major, describeNVregNodes(results, removed), nvregOverrideRemovedHint))
	}
	if unknown := nodesWithVerdict(results, nvregVersionUnknown); len(unknown) > 0 {
		sections = append(sections, fmt.Sprintf(
			"NVIDIA driver version could not be determined on GPU nodes: %s. %s",
			describeNVregNodes(results, unknown), nvregUnknownVersionHint))
	}
	if missing := nodesWithVerdict(results, nvregFlagMissing); len(missing) > 0 {
		sections = append(sections, fmt.Sprintf(
			"NVreg_GrdmaPciTopoCheckOverride=1 missing on GPU nodes: %s. %s",
			describeNVregNodes(results, missing), nvregDocsHint))
	}

	if len(sections) == 0 {
		return nil
	}
	return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, strings.Join(sections, " "))
}

// nodesWithVerdict returns the sorted node names carrying the given verdict.
// Sorted because the fan-out completes in nondeterministic order.
func nodesWithVerdict(results map[string]nvregResult, want nvregVerdict) []string {
	var out []string
	for nodeName, res := range results {
		if res.verdict == want {
			out = append(out, nodeName)
		}
	}
	slices.Sort(out)
	return out
}

// Node lists are bounded by BOTH a count and a byte budget. The result flows
// into a termination message capped at ValidatorMaxTerminationMsgBytes, and a
// count alone is not enough: a Kubernetes node name may be up to 253
// characters, so ten of them across three categories can blow the cap on their
// own and truncate the remediation — the actionable half of the message. The
// omitted remainder is always COUNTED, never silently dropped.
const (
	maxListedNodes = 10

	// maxListedNodeBytes is the per-category budget for the rendered node list.
	// Three categories plus the hints (~1.2 KiB) and headlines stay comfortably
	// inside ValidatorMaxTerminationMsgBytes.
	maxListedNodeBytes = 512
)

// describeNVregNodes renders "node (driver X)" so the message names the version
// actually detected, bounded by maxListedNodes.
func describeNVregNodes(results map[string]nvregResult, nodeNames []string) string {
	var (
		parts []string
		used  int
	)
	for i, nodeName := range nodeNames {
		entry := nodeName
		if v := results[nodeName].version; v != "" {
			entry = fmt.Sprintf("%s (driver %s)", nodeName, v)
		}
		// Stop on whichever bound bites first. Always emit at least one entry so
		// a single pathologically long name still names the node it is about.
		if i > 0 && (i >= maxListedNodes || used+len(entry) > maxListedNodeBytes) {
			break
		}
		parts = append(parts, entry)
		used += len(entry) + 2 // ", "
	}
	out := strings.Join(parts, ", ")
	if overflow := len(nodeNames) - len(parts); overflow > 0 {
		out += fmt.Sprintf(" (+%d more)", overflow)
	}
	return out
}

// checkNVregOnNode creates and waits on a short-lived probe pod that reads
// /proc/driver/nvidia/{version,params} on a specific node, and returns the
// verdict those two files imply. Returns an error only on failures that
// establish nothing about the node (pod schedule, image pull, log read).
func checkNVregOnNode(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) (nvregResult, error) {
	podsClient := clientset.CoreV1().Pods(namespace)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: preflightPodNamePrefix,
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/component":  "nccl-nvreg-preflight",
				"app.kubernetes.io/managed-by": "aicr-validator",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			// The probe only reads a hostPath and never calls the Kubernetes API,
			// so don't mount an API token onto it (defense-in-depth with the hostPath).
			AutomountServiceAccountToken: ptr.To(false),
			// Tolerate whatever taints the GPU nodes carry. The preflight
			// is cheap (busybox + cat) so we accept wherever scheduler
			// places us on the target node.
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   defaults.ProbeImage,
				Command: []string{shellBin, "-c"},
				// Emit both files with a sentinel between them and ALWAYS exit 0:
				// an exit status cannot distinguish "flag absent, go set it" from
				// "flag cannot exist on this driver" (#2459). A missing file
				// contributes empty content, which resolves to the fail-closed
				// nvregVersionUnknown.
				Args: []string{
					"cat /host-proc-nvidia/version 2>/dev/null; " +
						"echo '" + nvregProbeSeparator + "'; " +
						"cat /host-proc-nvidia/params 2>/dev/null; " +
						"exit 0",
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "proc-nvidia",
					MountPath: "/host-proc-nvidia",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "proc-nvidia",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/proc/driver/nvidia",
					},
				},
			}},
		},
	}

	created, err := podsClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nvregResult{}, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to create NVreg preflight pod", err)
	}
	// Cleanup runs on an independent context so it fires even when the
	// probe context has been canceled (timeout, parallel-sibling error).
	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.PreflightCleanupTimeout)
		defer cancel()
		if delErr := podsClient.Delete(cleanupCtx, created.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			slog.Warn("failed to delete NVreg preflight pod", "pod", created.Name, "err", delErr)
		}
	}()

	phase, err := waitForPreflightPodPhase(ctx, clientset, namespace, created.Name, defaults.DiagnosticTimeout)
	if err != nil {
		return nvregResult{}, err
	}

	// The script ends in `exit 0`, so a non-Succeeded phase means the body never
	// ran. Nothing is established, so this is an error rather than a verdict.
	if phase != corev1.PodSucceeded {
		return nvregResult{}, aicrErrors.New(aicrErrors.ErrCodeInternal,
			"NVreg preflight pod on node "+nodeName+" terminated in phase "+string(phase)+
				" without running the probe")
	}

	logs, logErr := k8spod.GetPodLogs(ctx, clientset, namespace, created.Name, "probe")
	if logErr != nil {
		return nvregResult{}, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"NVreg preflight pod succeeded but logs were unreadable", logErr)
	}

	versionFile, paramsFile := splitNVregProbeOutput(logs)
	return evaluateNVregPreflight(versionFile, paramsFile), nil
}

// waitForPreflightPodPhase watches a pod until it reaches a terminal phase
// (Succeeded or Failed). Uses the watch API per CLAUDE.md "Kubernetes
// Patterns" rather than polling. Returns the terminal phase on success.
func waitForPreflightPodPhase(ctx context.Context, clientset kubernetes.Interface, namespace, name string, timeout time.Duration) (corev1.PodPhase, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	podsClient := clientset.CoreV1().Pods(namespace)

	// Fast path: pod may already be terminal.
	if current, err := podsClient.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if p := current.Status.Phase; p == corev1.PodSucceeded || p == corev1.PodFailed {
			return p, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get preflight pod", err)
	}

	watcher, err := podsClient.Watch(waitCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch preflight pod", err)
	}
	defer watcher.Stop()

	// Re-check after the watch is established: the pod may have reached a
	// terminal phase between the first Get and the Watch call, in which case
	// the watch will not replay the transition.
	if current, err := podsClient.Get(waitCtx, name, metav1.GetOptions{}); err == nil {
		if p := current.Status.Phase; p == corev1.PodSucceeded || p == corev1.PodFailed {
			return p, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get preflight pod", err)
	}

	for {
		select {
		case <-waitCtx.Done():
			return "", aicrErrors.WrapWithContext(aicrErrors.ErrCodeTimeout,
				"NVreg preflight pod did not terminate in time", waitCtx.Err(),
				map[string]any{"pod": name})
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := waitCtx.Err(); ctxErr != nil {
					return "", aicrErrors.WrapWithContext(aicrErrors.ErrCodeTimeout,
						"NVreg preflight pod did not terminate in time", ctxErr,
						map[string]any{"pod": name})
				}
				// Watch closed without cancellation — re-Get before failing, in
				// case the pod reached a terminal phase during the closure window.
				current, getErr := podsClient.Get(waitCtx, name, metav1.GetOptions{})
				switch {
				case getErr == nil:
					if p := current.Status.Phase; p == corev1.PodSucceeded || p == corev1.PodFailed {
						return p, nil
					}
					return "", aicrErrors.New(aicrErrors.ErrCodeUnavailable,
						"preflight pod watch channel closed before pod terminated")
				case apierrors.IsNotFound(getErr):
					return "", aicrErrors.New(aicrErrors.ErrCodeUnavailable,
						"preflight pod watch channel closed and pod not found on re-check")
				case aicrErrors.IsTransient(getErr):
					return "", aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
						"preflight pod watch closed and re-check timed out", getErr)
				default:
					return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
						"preflight pod watch closed and re-check failed", getErr)
				}
			}
			if event.Type == watch.Deleted {
				return "", aicrErrors.New(aicrErrors.ErrCodeInternal,
					"preflight pod deleted before completion")
			}
			p, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				return p.Status.Phase, nil
			}
		}
	}
}

// gb200NetPreflightApplies reports whether the preflight check should run for
// the given (variant, accelerator, service) tuple. Keeps the call site at the
// top of validateNcclAllReduceBw uncluttered.
//
// EKS and OKE are the two GB200 NET fabrics that traverse a PCIe-attached NIC
// (EFA and ConnectX IB respectively), so both need the dma-buf prerequisite —
// the module flag before R595, and on R595+ a topology property this preflight
// cannot check, where it fails rather than assume.
// On OKE the flag reaches the driver only under gpuStack=operator-managed
// (the leaf's kernel-module-params ConfigMap needs a driver DaemonSet to
// consume it — see recipes/overlays/gb200-oke-training.yaml); under the
// default oci-managed profile the driver ships in the node image, so this
// preflight is the fail-closed gate that catches an image driver missing the
// flag before NCCL silently degrades to Socket (#2356 review).
func gb200NetPreflightApplies(variant ncclVariant, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) bool {
	return variant == variantNET &&
		accelerator == recipe.CriteriaAcceleratorGB200 &&
		(service == recipe.CriteriaServiceEKS || service == recipe.CriteriaServiceOKE)
}

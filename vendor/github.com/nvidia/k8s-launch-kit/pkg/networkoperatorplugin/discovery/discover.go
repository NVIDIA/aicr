// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Mellanox/doca-driver-build/entrypoint/pkg/mofedmodules"
	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/internal/pciids"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/internal/pfutil"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//go:embed ns-product-ids
var nsProductIDsData string

// nsProductIDs is the set of product codes / legacy product IDs for
// BlueField DPU devices (not SuperNICs). Devices whose PartNumber matches
// an entry in this set are marked as north-south traffic.
var nsProductIDs = parseNSProductIDs(nsProductIDsData)

func parseNSProductIDs(data string) map[string]bool {
	ids := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: ProductCode | LegacyProductId | ProductName
		parts := strings.SplitN(line, "|", 3)
		if len(parts) >= 2 {
			productCode := strings.TrimSpace(parts[0])
			legacyID := strings.TrimSpace(parts[1])
			if productCode != "" {
				ids[productCode] = true
			}
			if legacyID != "" {
				ids[legacyID] = true
			}
		}
	}
	return ids
}

// isNorthSouthDevice returns true if the device's part number matches
// a known BlueField DPU (non-SuperNIC) product ID from the ns-product-ids file.
func isNorthSouthDevice(partNumber string) bool {
	return nsProductIDs[partNumber]
}

// isBlueField3Device returns true if the PCI device ID is BlueField-3
// (Mellanox vendor 15b3, device 0xa2dc). BF3 is the only BlueField
// generation where both DPU and SuperNIC SKUs share a device ID, so
// classification has to fall back to part number against ns-product-ids.
// Older (BF2) and newer (BF4+) generations use distinct device IDs and
// stay on the default path — DPUs match ns-product-ids and become
// north-south; anything else flows through the frequency heuristic.
func isBlueField3Device(deviceID string) bool {
	return strings.EqualFold(deviceID, "a2dc")
}

// DiscoverClusterConfig walks the cluster and populates cfg with the
// discovered hardware topology. Library callers typically invoke it via
// Discover, which wraps it with default-options handling. The parent
// networkoperatorplugin package's NetworkOperatorPlugin.DiscoverClusterConfig
// method is a thin shim that delegates here.
//
// See Discover's doc for input/output semantics; the only difference here
// is that the configuration knobs (RESTConfig, NodeSelector,
// KeepNamespace, CollapseNicRails) come in via Options instead of as
// functional DiscoverOptions.
func DiscoverClusterConfig(ctx context.Context, c client.Client, restConfig *rest.Config, cfg *config.LaunchKitConfig, opts Options) error {
	uiOutput := ui.FromContext(ctx)

	if cfg.NetworkOperator == nil {
		return fmt.Errorf("networkOperator section is required in config for discovery")
	}

	bootstrapOpts := nicconfigdaemon.Options{
		Repository:       cfg.NetworkOperator.Repository,
		Version:          cfg.NetworkOperator.ComponentVersion,
		ImagePullSecrets: cfg.NetworkOperator.ImagePullSecrets,
	}

	// Pre-clean any leftover bootstrap from a prior crashed/killed run (the
	// teardown defer below only fires on a clean exit). Deleting the namespace
	// cascades the DaemonSet, pods, SA, ConfigMap, and any stale NicDevice CRs
	// in it; CRDs are intentionally preserved by Cleanup. Wait for the
	// namespace to fully clear before re-applying so we never deploy on top of
	// a Terminating namespace. Best-effort: a stuck namespace surfaces a
	// warning and Ensure's Create will fail with a clearer message if needed.
	uiOutput.Info("Cleaning up any leftover discovery bootstrap in namespace %q", nicconfigdaemon.Namespace)
	if cleanupErr := nicconfigdaemon.Cleanup(ctx, c); cleanupErr != nil {
		// Surface the root cause now, so the WaitForNamespaceDeleted timeout
		// below (if the delete never took) reads as a consequence, not a
		// mystery.
		log.Log.Error(cleanupErr, "pre-clean of discovery bootstrap reported an error; continuing",
			"namespace", nicconfigdaemon.Namespace)
		uiOutput.Warning("Pre-clean of namespace %q reported an error: %v", nicconfigdaemon.Namespace, cleanupErr)
	}
	if waitErr := nicconfigdaemon.WaitForNamespaceDeleted(ctx, c, 2*time.Minute); waitErr != nil {
		log.Log.Error(waitErr, "leftover bootstrap namespace did not clear before timeout",
			"namespace", nicconfigdaemon.Namespace)
		uiOutput.Warning("Leftover namespace %q is still terminating; bootstrap may fail: %v",
			nicconfigdaemon.Namespace, waitErr)
	}

	uiOutput.Info("Bootstrapping NIC configuration daemon in namespace %q", nicconfigdaemon.Namespace)
	log.Log.Info("Bootstrapping NIC configuration daemon for discovery",
		"namespace", nicconfigdaemon.Namespace, "image", bootstrapOpts.Image())
	if err := nicconfigdaemon.Ensure(ctx, c, bootstrapOpts); err != nil {
		return fmt.Errorf("failed to bootstrap NIC configuration daemon: %w", err)
	}

	if !opts.KeepNamespace {
		defer func() {
			uiOutput.Info("Tearing down namespace %q", nicconfigdaemon.Namespace)
			if cleanupErr := nicconfigdaemon.Cleanup(context.Background(), c); cleanupErr != nil {
				log.Log.Error(cleanupErr, "failed to tear down discover bootstrap namespace",
					"namespace", nicconfigdaemon.Namespace)
				uiOutput.Warning("Failed to delete namespace %q: %v", nicconfigdaemon.Namespace, cleanupErr)
			}
		}()
	} else {
		uiOutput.Info("--keep-namespace set, leaving namespace %q in place after discovery", nicconfigdaemon.Namespace)
	}

	// Wait for daemon pods to become Ready in the bootstrap namespace.
	expectedNodes, dsPods, _, err := waitForDaemonSetPods(ctx, c, uiOutput,
		nicconfigdaemon.Namespace, nicconfigdaemon.DaemonSetName, 5*time.Minute)
	if err != nil {
		return err
	}

	// Fetch node labels early — needed both for filtering and nodeSelector computation
	nodeLabels, err := fetchNodeLabels(ctx, c)
	if err != nil {
		log.Log.Error(err, "failed to fetch node labels; nodeSelectors will be empty")
		nodeLabels = map[string]map[string]string{}
	}

	// Filter the wait set to nodes that actually have an NVIDIA NIC. The daemon
	// runs on every node (no nodeSelector), but only NIC-bearing nodes will
	// ever report NicDevice CRs — waiting for the rest would time out. We
	// detect NICs by probing each pod's sysfs for PCI vendor 0x15b3 rather than
	// the NFD label, which may not exist yet at discover time. The
	// `--node-selector` value is deliberately NOT used here; it only flows into
	// the saved cluster-config nodeSelector (for deploy time, when NFD exists).
	if restConfig != nil {
		expectedNodes = filterNodesWithNICs(ctx, restConfig, nicconfigdaemon.Namespace, expectedNodes, dsPods)
		if len(expectedNodes) == 0 {
			return fmt.Errorf("no nodes with an NVIDIA NIC (PCI vendor 15b3) were found")
		}
	} else if len(opts.NodeSelector) > 0 {
		// Fallback when pod exec is unavailable (no REST config): keep the
		// historical label-based filter so callers without a REST config don't
		// regress.
		expectedNodes = filterNodesByLabels(expectedNodes, nodeLabels, opts.NodeSelector)
		if len(expectedNodes) == 0 {
			return fmt.Errorf("no nodes match the node selector %v", opts.NodeSelector)
		}
	}

	// Wait for all expected nodes to report their NicDevice resources
	if err := waitNicDevicesDiscovered(ctx, c, nicconfigdaemon.Namespace, expectedNodes); err != nil {
		return err
	}

	// Get NicDevice resources and build ClusterConfig from their statuses
	devices := &nicop.NicDeviceList{}
	if err := c.List(ctx, devices, client.InNamespace(nicconfigdaemon.Namespace)); err != nil {
		return err
	}

	clusterConfig, nsWarnings := buildClusterConfig(devices.Items, nodeLabels, opts.NodeSelector, opts.CollapseNicRails)
	cfg.ClusterConfig = clusterConfig

	for _, w := range nsWarnings {
		uiOutput.Warning("%s", w)
	}

	// Discover OFED-dependent kernel modules per group via pod exec.
	// Results are classified into third-party RDMA vs storage modules and
	// saved to config for user inspection. mlx5-prefixed modules (NVIDIA's own)
	// are silently filtered out.
	if restConfig != nil {
		for i := range cfg.ClusterConfig {
			group := &cfg.ClusterConfig[i]

			// Fill in missing machine/GPU product types by probing hardware
			// directly when GPU operator node labels are absent.
			needMachine := group.MachineType == ""
			needProduct := group.GPUType == ""
			if needMachine || needProduct {
				machine, product := discoverHardwareTypes(ctx, restConfig,
					nicconfigdaemon.Namespace, group.WorkerNodes, dsPods,
					needMachine, needProduct)
				if needMachine && machine != "" {
					group.MachineType = machine
					uiOutput.Info("Discovered machine type for group %s: %s", group.Identifier, machine)
				}
				if needProduct && product != "" {
					group.GPUType = product
					uiOutput.Info("Discovered GPU product type for group %s: %s", group.Identifier, product)
				}
			}

			// Probe GPU topology from nvidia-smi: populates NumaNode,
			// ConnectedGPU, GPUProximity per PF; if any PF has PIX to a GPU,
			// the PIX-gate override rewrites Traffic and re-runs rails.
			// Failures are non-fatal; when nvidia-smi is absent, today's
			// part-number classification continues to govern.
			discoverGPUTopology(ctx, restConfig,
				nicconfigdaemon.Namespace, group, dsPods)

			// Probe per-PF fabric type from active port state + subnet
			// manager presence (more reliable than firmware link_layer
			// alone). Populates PFConfig.LinkType / LinkTypeSource. Used by
			// the declarative defaults in `l8k generate` (Unit 8) to fill
			// `--fabric` from the discovered group.
			discoverGroupFabric(ctx, restConfig,
				nicconfigdaemon.Namespace, group, dsPods)

			// Try to enrich with a predefined topology preset for this
			// (machine, GPU) pair. presetmatch.MatchGroup runs the
			// shared lookup + deviation comparison (also used by
			// `l8k validate`); the discovery path then additionally
			// applies the preset onto the group so rail/NUMA topology
			// fields populate. Lookup is exact-match on (machineType,
			// gpuType) — both must be known for a preset to apply.
			matchResult := presetmatch.MatchGroup(*group)
			log.Log.V(1).Info("Preset match",
				"group", group.Identifier,
				"machineType", group.MachineType,
				"gpuType", group.GPUType,
				"status", string(matchResult.Status),
				"presetName", matchResult.PresetName,
				"deviationCount", len(matchResult.Deviations))
			switch matchResult.Status {
			case presetmatch.StatusMatch, presetmatch.StatusDeviation:
				// Record any deviations regardless of whether we enrich,
				// so `l8k validate` and every config load keep surfacing
				// the drift.
				group.PresetDeviation = matchResult.Deviations
				if presets.HasTopologyDeviation(matchResult.Deviations) {
					// Hardware drifts from the preset on PF count, PCI
					// address, or device ID (NIC model). Applying the
					// preset would clobber the live-discovered traffic/rail
					// classification: ApplyPreset matches PFs by PCI
					// address, so any address that coincidentally overlaps
					// inherits the preset's unrelated traffic/rail/GPU
					// fields (and rails are left non-contiguous because
					// ApplyPreset doesn't renumber). Skip enrichment and
					// keep the live results.
					uiOutput.Warning(
						"Preset for %s/%s NOT applied — discovered hardware deviates from the preset on %d field(s) (PF count / PCI address / device ID). Using live discovery results; see 'presetDeviation' in cluster-config.yaml.",
						group.MachineType, group.GPUType, len(matchResult.Deviations))
					break
				}
				// Exact topology match (or only benign deviations) — load
				// the Topology again and enrich rail/NUMA/GPU fields.
				// (MatchGroup intentionally doesn't mutate.)
				preset, err := presets.LoadPreset(group.MachineType, group.GPUType)
				switch {
				case err != nil:
					// MatchGroup already loaded this preset to produce the
					// match, so a reload failure here is unexpected — surface
					// it rather than silently falling back to live discovery.
					uiOutput.Warning(
						"Preset for %s/%s matched but could not be re-loaded to apply (%v). Using live discovery results.",
						group.MachineType, group.GPUType, err)
				case preset == nil:
					uiOutput.Warning(
						"Preset for %s/%s matched but was not found on re-load. Using live discovery results.",
						group.MachineType, group.GPUType)
				case presets.ApplyPreset(preset, group):
					uiOutput.Info("Applied preset configuration for %s", group.MachineType)
				default:
					// Loaded fine but the all-or-nothing apply declined (PCI
					// bijection not satisfied) — shouldn't happen given the
					// HasTopologyDeviation gate above, but don't claim a
					// preset we didn't actually apply.
					log.Log.V(1).Info("Preset matched but not applied",
						"group", group.Identifier,
						"machineType", group.MachineType,
						"gpuType", group.GPUType)
				}
			case presetmatch.StatusNotFound:
				// No catalog entry — discovery continues without
				// preset enrichment. Logged at V(1) only; not a
				// user-actionable warning.
			case presetmatch.StatusSkipped:
				// machineType / gpuType wasn't discovered. Discovery
				// already logs this via the hardware-type probes.
			}

			modules, err := discoverThirdPartyRDMAModules(ctx, restConfig,
				nicconfigdaemon.Namespace, group.WorkerNodes, dsPods)
			if err != nil {
				log.Log.Error(err, "failed to discover OFED-dependent modules", "group", group.Identifier)
				uiOutput.Warning("Could not discover OFED-dependent modules for group %s: %v", group.Identifier, err)
				continue
			}
			rdma, storage := classifyDiscoveredModules(modules)
			if len(rdma) > 0 {
				group.ThirdPartyRDMAModules = rdma
				cfg.DOCADriver.UnloadThirdPartyRDMAModules = true
				uiOutput.Info("Discovered %d third-party RDMA module(s) for group %s — enabled unloadThirdPartyRDMAModules",
					len(rdma), group.Identifier)
			}
			if len(storage) > 0 {
				group.StorageModules = storage
				cfg.DOCADriver.UnloadStorageModules = true
				uiOutput.Info("Discovered %d storage module(s) for group %s — enabled unloadStorageModules",
					len(storage), group.Identifier)
			}
		}
	}

	// Now that machineType and gpuType are settled (either from labels or
	// from the per-group hardware probes), assign each group a stable
	// machine label. This replaces the differential-label nodeSelector
	// algorithm: every node in the group is patched with
	// `nvidia.kubernetes-launch-kit.machine: <machineType>-<gpuType>` and
	// the group's Identifier + NodeSelector are aligned with that value.
	applyMachineLabelToGroups(ctx, c, cfg.ClusterConfig)

	// Phase summary — counts surfaced at info level so the default UX shows
	// progress without requiring --log-level=debug.
	totalEW, totalNS, presetMatches, deviationGroups, labelled := 0, 0, 0, 0, 0
	for _, g := range cfg.ClusterConfig {
		for _, pf := range g.PFs {
			switch pf.Traffic {
			case "east-west":
				totalEW++
			case "north-south":
				totalNS++
			}
		}
		if g.PresetApplied {
			presetMatches++
		}
		if len(g.PresetDeviation) > 0 {
			deviationGroups++
		}
		if g.NodeSelector[config.MachineLabelKey] != "" {
			labelled++
		}
	}
	log.Log.Info("Discovery summary",
		"groupCount", len(cfg.ClusterConfig),
		"eastWestPFs", totalEW,
		"northSouthPFsFiltered", totalNS,
		"presetMatches", presetMatches,
		"presetDeviationGroups", deviationGroups,
		"machineLabelledGroups", labelled)

	return nil
}

// applyMachineLabelToGroups walks each group and writes two l8k-specific
// labels onto every node in the group:
//
//   - MachineLabelKey = `<machineType>-<gpuType>` literal — per-source-group
//     identifier, written when both fields are resolved.
//   - GPULabelKey = `<gpuType>` literal — written when gpuType is resolved.
//     Used as the merged-group NodeSelector when source groups span
//     machineTypes but share a GPU type.
//
// Both label values bypass the Kubernetes 63-char limit by skipping the
// label entirely when the value would overflow (logged at debug). Group
// `Identifier` follows the resource-name convention (lowercase via
// `sanitizeIdentifier`); the label values keep their original case to
// match `nvidia.com/gpu.product`-style values.
//
// Groups whose machine label can't be computed (one input missing) keep
// their fallback identifier ("group-N") and an empty NodeSelector. The
// GPU label is still written when gpuType alone is resolved, so
// merged-group selection still works.
func applyMachineLabelToGroups(ctx context.Context, c client.Client, groups []config.ClusterConfig) {
	for i := range groups {
		g := &groups[i]
		machineLabel := config.MachineLabelValue(g.MachineType, g.GPUType)
		gpuLabel := config.GPULabelValue(g.GPUType)

		if machineLabel == "" {
			log.Log.V(1).Info("Skipping machine label: machineType/gpuType unresolved or value > 63 chars",
				"group", g.Identifier,
				"machineType", g.MachineType,
				"gpuType", g.GPUType)
		} else {
			log.Log.V(1).Info("Assigning machine label to group",
				"originalIdentifier", g.Identifier,
				"machineType", g.MachineType,
				"gpuType", g.GPUType,
				"labelValue", machineLabel,
				"nodes", len(g.WorkerNodes))
			g.Identifier = config.SanitizeIdentifier(machineLabel)
			g.NodeSelector = map[string]string{config.MachineLabelKey: machineLabel}
		}

		labels := map[string]string{}
		if machineLabel != "" {
			labels[config.MachineLabelKey] = machineLabel
		}
		if gpuLabel != "" {
			labels[config.GPULabelKey] = gpuLabel
		}
		if len(labels) == 0 {
			continue
		}
		for _, nodeName := range g.WorkerNodes {
			if err := patchNodeLabels(ctx, c, nodeName, labels); err != nil {
				log.Log.Error(err, "failed to patch node labels",
					"node", nodeName, "labels", labels)
				continue
			}
			log.Log.V(1).Info("Wrote labels to node",
				"node", nodeName, "labels", labels)
		}
	}
}

// patchNodeLabels applies one or more labels to a node via a
// strategic-merge patch. Idempotent — re-applying the same values is a
// no-op on the cluster side, and avoids the read-modify-write conflict
// risk of a full Update.
func patchNodeLabels(ctx context.Context, c client.Client, nodeName string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%q:%q", k, v))
	}
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"labels":{%s}}}`,
		strings.Join(parts, ",")))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	return c.Patch(ctx, node, client.RawPatch(k8stypes.StrategicMergePatchType, patch))
}

// dsReadiness summarizes the readiness of a DaemonSet's pods.
type dsReadiness struct {
	// readyNodes / readyPods are the nodes and pods whose pod is Ready (only
	// these can be exec'd into for the sysfs NIC probe and later hardware
	// probes).
	readyNodes []string
	readyPods  []corev1.Pod
	total      int // total DS pods found (Ready or not)
	ready      int // Ready pod count
	// stuck counts not-Ready pods in a waiting state that won't clear on its
	// own within our window (ImagePullBackOff, CrashLoopBackOff, …). Used to
	// stop waiting on unrelated nodes that will never become Ready.
	stuck int
}

// stuckWaitingReasons are container waiting reasons that won't resolve without
// operator intervention, so there's no point blocking discovery on them.
var stuckWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"CrashLoopBackOff":           true,
}

// podStuck reports whether any container (init or regular) is in a waiting
// state that won't recover on its own.
func podStuck(pod *corev1.Pod) bool {
	statuses := append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.State.Waiting != nil && stuckWaitingReasons[cs.State.Waiting.Reason] {
			return true
		}
	}
	return false
}

// checkDaemonSetPodsReady lists pods owned by the given DaemonSet in the
// provided namespace and summarizes their readiness. The returned error is
// non-nil only when the pod list can't be obtained or no DS pods exist yet
// (so callers can keep polling / fall back to an alternate namespace). Pods
// that are not Ready do NOT produce an error — the caller decides whether to
// keep waiting based on the dsReadiness counts. Since the discovery DaemonSet
// now runs on every node (no nodeSelector), requiring *all* pods Ready would
// let a single unrelated stuck node (e.g. ImagePullBackOff) block discovery
// for the whole timeout; readyNodes/readyPods therefore carry only the Ready
// subset, which is all we can probe anyway.
func checkDaemonSetPodsReady(ctx context.Context, c client.Client, namespace, daemonSetName string) (dsReadiness, error) {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return dsReadiness{}, err
	}

	var dsPods []corev1.Pod
	for _, pod := range podList.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" && owner.Name == daemonSetName {
				dsPods = append(dsPods, pod)
				break
			}
		}
	}

	if len(dsPods) == 0 {
		return dsReadiness{}, fmt.Errorf(
			"no pods found for DaemonSet %q in namespace %q; "+
				"use --network-operator-namespace to specify the correct namespace",
			daemonSetName, namespace)
	}

	st := dsReadiness{total: len(dsPods)}
	for i := range dsPods {
		pod := dsPods[i]
		if isPodReady(&pod) {
			st.ready++
			st.readyPods = append(st.readyPods, pod)
			if pod.Spec.NodeName != "" {
				st.readyNodes = append(st.readyNodes, pod.Spec.NodeName)
			}
		} else if podStuck(&pod) {
			st.stuck++
		}
	}

	return st, nil
}

// waitForDaemonSetPods polls until the given DaemonSet's pods are ready enough
// to proceed, or the timeout fires. "Ready enough" means either every pod is
// Ready, or every not-Ready pod is stuck (won't recover) and at least one pod
// is Ready — so an unrelated node that can't start the daemon doesn't block
// discovery. On timeout it still proceeds with whatever Ready pods exist
// (warning), and only hard-fails when no pod ever became Ready. Returns the
// Ready node names, the Ready pods (for pod exec), the namespace they were
// found in, and an error.
func waitForDaemonSetPods(parentCtx context.Context, c client.Client, uiOutput ui.Output, namespace, daemonSetName string, timeout time.Duration) ([]string, []corev1.Pod, string, error) {
	altNS := alternateNamespace(namespace)
	progressLabel := fmt.Sprintf("Waiting for %s pods in namespace %q (timeout: %s)", daemonSetName, namespace, timeout.Truncate(time.Second))
	if altNS != "" {
		progressLabel = fmt.Sprintf("Waiting for %s pods in namespace %q (also polling fallback %q; timeout: %s)", daemonSetName, namespace, altNS, timeout.Truncate(time.Second))
	}
	progress := uiOutput.StartProgress(progressLabel)

	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastErr error
	var last dsReadiness
	var lastNS string
	for {
		// Probe the primary namespace; fall back to the legacy alternate
		// namespace only when the primary has no DS pods at all.
		st, err := checkDaemonSetPodsReady(ctx, c, namespace, daemonSetName)
		foundNS := namespace
		if err != nil && altNS != "" {
			if altSt, altErr := checkDaemonSetPodsReady(ctx, c, altNS, daemonSetName); altErr == nil {
				st, err, foundNS = altSt, nil, altNS
			}
		}

		if err == nil {
			allReady := st.total > 0 && st.ready == st.total
			// Every remaining not-Ready pod is stuck and we already have a
			// node to probe — no point waiting out the timeout.
			blockedByStuck := st.ready >= 1 && st.ready+st.stuck == st.total
			switch {
			case allReady:
				progress.Success(fmt.Sprintf("Found %d ready pod(s) in namespace %q", st.ready, foundNS))
				return st.readyNodes, st.readyPods, foundNS, nil
			case blockedByStuck:
				progress.Success(fmt.Sprintf("Proceeding with %d ready pod(s) in namespace %q", st.ready, foundNS))
				uiOutput.Warning("%d %s pod(s) are stuck (e.g. ImagePullBackOff) and excluded from discovery; proceeding with %d ready node(s)",
					st.stuck, daemonSetName, st.ready)
				return st.readyNodes, st.readyPods, foundNS, nil
			default:
				last, lastNS = st, foundNS
				lastErr = fmt.Errorf("%d/%d %s pods ready", st.ready, st.total, daemonSetName)
			}
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			// Don't fail outright if some pods are Ready — a slow or stuck
			// unrelated node shouldn't abort discovery. NicDevice CRs are
			// awaited separately (10 min) downstream.
			if last.ready >= 1 {
				progress.Success(fmt.Sprintf("Proceeding with %d ready pod(s) in namespace %q", last.ready, lastNS))
				uiOutput.Warning("Timed out waiting for all %s pods to be Ready; proceeding with %d ready node(s)",
					daemonSetName, last.ready)
				return last.readyNodes, last.readyPods, lastNS, nil
			}
			progress.Fail("Timeout waiting for daemon pods")
			if altNS != "" {
				return nil, nil, "", fmt.Errorf("timeout waiting for %s pods to start in namespace %q (also checked fallback namespace %q); use --network-operator-namespace to specify the correct namespace: %w", daemonSetName, namespace, altNS, lastErr)
			}
			return nil, nil, "", fmt.Errorf("timeout waiting for %s pods to start in namespace %q: %w", daemonSetName, namespace, lastErr)
		case <-ticker.C:
			progress.Update(fmt.Sprintf("Waiting for %s pods...", daemonSetName))
		}
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// alternateNamespace returns the other common network operator namespace,
// or empty string if the current namespace isn't one of the two known defaults.
// Used by waitForDaemonSetPods as a legacy chart-rename fallback; for the
// self-contained discover bootstrap (namespace nicconfigdaemon.Namespace),
// it returns "" and the fallback path is a no-op.
func alternateNamespace(current string) string {
	switch current {
	case "nvidia-network-operator":
		return "network-operator"
	case "network-operator":
		return "nvidia-network-operator"
	default:
		return ""
	}
}

// waitNicDevicesDiscovered polls until NicDevice objects exist for all expected nodes in the given namespace.
func waitNicDevicesDiscovered(parentCtx context.Context, c client.Client, namespace string, expectedNodes []string) error {
	uiOutput := ui.FromContext(parentCtx)
	progress := uiOutput.StartProgress(fmt.Sprintf("Discovering network devices on %d node(s) (timeout: 10 min)", len(expectedNodes)))

	// Use a bounded timeout if none supplied
	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, 10*time.Minute)
		defer cancel()
	}

	expectedSet := make(map[string]bool, len(expectedNodes))
	for _, n := range expectedNodes {
		expectedSet[n] = true
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		list := &nicop.NicDeviceList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err == nil {
			discoveredNodes := make(map[string]bool)
			for _, d := range list.Items {
				if d.Status.Node != "" {
					discoveredNodes[d.Status.Node] = true
				}
			}

			allFound := true
			for node := range expectedSet {
				if !discoveredNodes[node] {
					allFound = false
					break
				}
			}

			if allFound && len(discoveredNodes) > 0 {
				progress.Success(fmt.Sprintf("Found %d device(s) on %d node(s)", len(list.Items), len(discoveredNodes)))
				return nil
			}

			progress.Update(fmt.Sprintf("Discovered devices on %d/%d node(s)...", len(discoveredNodes), len(expectedSet)))
		}

		select {
		case <-ctx.Done():
			progress.Fail("Timeout waiting for devices")
			return fmt.Errorf("timeout waiting for NicDevice resources from all nodes in namespace %q", namespace)
		case <-ticker.C:
		}
	}
}

// pfFingerprint identifies a PF by its device ID and PCI address (ignoring RDMA/net names).
type pfFingerprint struct {
	DeviceID   string
	PciAddress string
}

// nodePFEntry holds the full PF info discovered on a specific node.
type nodePFEntry struct {
	pfFingerprint
	RdmaDevice       string
	NetworkInterface string
	IsNorthSouth     bool // true when device is a DPU (not a SuperNIC)
	// IsExplicitEastWest is true when Stage 1 classified this PF as a
	// BlueField SuperNIC (BF chip + part number not in ns-product-ids).
	// The frequency heuristic must not flip it back to north-south, and
	// its presence triggers the "any non-matching unpinned PF on this
	// node is OOB / north-south" rule.
	IsExplicitEastWest bool
	PSID               string
	PartNumber         string
	// ModelName is NicDevice.Status.ModelName — the VPD model/description
	// string, which carries the physical port count ("2-port"/"Dual-port"/"1P").
	// Used to decide whether a multi-PF NIC is genuinely dual-port (keep a rail
	// per port) or multi-plane (collapse to one rail per NIC). Empty when VPD
	// couldn't be read.
	ModelName string
}

// fetchNodeLabels lists all Kubernetes nodes and returns a map of nodeName → labels.
func fetchNodeLabels(ctx context.Context, c client.Client) (map[string]map[string]string, error) {
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	result := make(map[string]map[string]string, len(nodeList.Items))
	for _, node := range nodeList.Items {
		result[node.Name] = node.Labels
	}
	return result, nil
}

// filterNodesByLabels returns only nodes whose labels match all entries in the selector.
func filterNodesByLabels(nodes []string, nodeLabels map[string]map[string]string, selector map[string]string) []string {
	var filtered []string
	for _, n := range nodes {
		labels := nodeLabels[n]
		if labels == nil {
			continue
		}
		match := true
		for k, v := range selector {
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

// isPowerOfTwo returns true if n is a positive power of two (2, 4, 8, 16, ...).
func isPowerOfTwo(n int) bool {
	return n >= 2 && (n&(n-1)) == 0
}

// classifyByPartNumberFrequency refines per-node PF traffic classification
// after Stage 1's deterministic rules (DPU list, BF SuperNIC). Two paths:
//
//  1. If any PF on the node is explicitly east-west (Stage 1 BF SuperNIC
//     pin), every other PF whose part number doesn't match an explicit-EW
//     part number is reclassified as north-south (treated as OOB / mgmt).
//     PFs already pinned north-south or explicit east-west are not touched.
//     This handles the common case where a node has BF SuperNICs for
//     east-west GPU traffic and a separate non-BF NIC (e.g. ConnectX-6 Lx)
//     wired up as the OOB management interface.
//
//  2. No explicit-EW pins: fall back to the original 5+-PF frequency
//     heuristic — the most common part number (preferring power-of-2
//     counts) is east-west; minority part numbers become north-south.
//     The tally only considers unpinned PFs, so DPU part numbers don't
//     skew the tiebreak.
//
// Returns warning messages for the caller to display to the user.
func classifyByPartNumberFrequency(nodeMap map[string]*nodeInfo) []string {
	var warnings []string
	for nodeName, ni := range nodeMap {
		// Path 1: explicit east-west pins exist for this node.
		ewPartNumbers := map[string]bool{}
		for _, pf := range ni.pfs {
			if pf.IsExplicitEastWest && pf.PartNumber != "" {
				ewPartNumbers[pf.PartNumber] = true
			}
		}
		if len(ewPartNumbers) > 0 {
			for i := range ni.pfs {
				if ni.pfs[i].IsNorthSouth || ni.pfs[i].IsExplicitEastWest {
					continue
				}
				if !ewPartNumbers[ni.pfs[i].PartNumber] {
					ni.pfs[i].IsNorthSouth = true
				}
			}
			continue
		}

		// Path 2: fall back to the legacy frequency heuristic.
		if len(ni.pfs) < 5 {
			continue
		}

		// Count PFs by part number, ignoring already-pinned ones so DPU
		// part numbers (Stage 1 north-south) don't poison the tiebreak.
		partCounts := map[string]int{}
		for _, pf := range ni.pfs {
			if pf.IsNorthSouth || pf.PartNumber == "" {
				continue
			}
			partCounts[pf.PartNumber]++
		}
		if len(partCounts) <= 1 {
			// All unpinned PFs share a part number — nothing to classify.
			continue
		}

		// Find the east-west part number: prefer the one with a power-of-2 count,
		// break ties by highest count then alphabetically.
		var ewPart string
		var ewCount int
		for part, count := range partCounts {
			better := false
			if ewPart == "" {
				better = true
			} else if isPowerOfTwo(count) && !isPowerOfTwo(ewCount) {
				better = true
			} else if isPowerOfTwo(count) == isPowerOfTwo(ewCount) {
				if count > ewCount {
					better = true
				} else if count == ewCount && part < ewPart {
					better = true
				}
			}
			if better {
				ewPart = part
				ewCount = count
			}
		}

		// Warn if E-W count is not a power of two
		if !isPowerOfTwo(ewCount) {
			warnings = append(warnings, fmt.Sprintf(
				"East-west NIC count (%d) is not a power of two on node %s. Please verify traffic classification in the config file.",
				ewCount, nodeName))
		}

		// Mark minority part numbers as north-south
		reclassified := false
		for i := range ni.pfs {
			if ni.pfs[i].PartNumber != ewPart && !ni.pfs[i].IsNorthSouth {
				ni.pfs[i].IsNorthSouth = true
				reclassified = true
			}
		}

		if !reclassified {
			warnings = append(warnings, fmt.Sprintf(
				"Could not identify north-south NICs on node %s. Please verify traffic classification and rail assignments in the config file.",
				nodeName))
		}
	}
	return warnings
}

// buildClusterConfig groups NicDevices by identical PF fingerprints across nodes
// and returns a ClusterConfig slice with one entry per group.
// nodeInfo holds discovered PF entries and capability flags for a single node.
type nodeInfo struct {
	pfs     []nodePFEntry
	hasRdma bool
	hasPCI  bool
}

// collapseGroupRails reduces a group's east-west PFs to one per NIC (one rail
// per NIC) via collapsePFsToOnePerNIC, while preserving every dual-port NIC's
// per-port PFs and leaving north-south PFs untouched. It returns the re-sorted
// PF slice and the number of PFs dropped; when nothing is collapsed it returns
// the input unchanged.
func collapseGroupRails(pfs []config.PFConfig) ([]config.PFConfig, int) {
	var ew, ns []config.PFConfig
	for _, pf := range pfs {
		if pf.Traffic == "east-west" {
			ew = append(ew, pf)
		} else {
			ns = append(ns, pf)
		}
	}
	collapsedEW, dropped := pfutil.CollapsePFsToOnePerNIC(ew)
	if dropped == 0 {
		return pfs, 0
	}
	out := append(collapsedEW, ns...)
	slices.SortFunc(out, func(a, b config.PFConfig) int {
		return strings.Compare(a.PciAddress, b.PciAddress)
	})
	return out, dropped
}

func buildClusterConfig(devices []nicop.NicDevice, nodeLabels map[string]map[string]string, nodeSelector map[string]string, collapseNicRails bool) ([]config.ClusterConfig, []string) {
	// Step 1: Build per-node PF map and track capabilities per node
	nodeMap := map[string]*nodeInfo{}

	for _, d := range devices {
		nodeName := d.Status.Node
		if nodeName == "" {
			continue
		}
		if nodeMap[nodeName] == nil {
			nodeMap[nodeName] = &nodeInfo{}
		}
		ni := nodeMap[nodeName]
		// Stage 1 classification:
		//   - Part number in ns-product-ids → BlueField DPU → north-south.
		//   - BlueField-3 chip (deviceID a2dc) with part number NOT in
		//     ns-product-ids → BlueField-3 SuperNIC → explicit east-west.
		//     BF3 is special-cased because DPU and SuperNIC SKUs share the
		//     same device ID; BF2/BF4 use distinct device IDs and DPUs
		//     among them are already covered by the ns-product-ids match.
		//   - Anything else → unclassified (default east-west, may be
		//     reclassified by the frequency heuristic in Stage 1.5).
		isDPU := isNorthSouthDevice(d.Status.PartNumber)
		isBF3SuperNIC := !isDPU && isBlueField3Device(d.Status.Type)
		var classification string
		switch {
		case isDPU:
			classification = "north-south (matched DPU part-number)"
		case isBF3SuperNIC:
			classification = "east-west (BF3 SuperNIC override)"
		default:
			classification = "unclassified (default east-west; may be reclassified by frequency heuristic)"
		}
		log.Log.V(2).Info("Classified NIC by traffic direction",
			"node", nodeName,
			"deviceID", d.Status.Type,
			"partNumber", d.Status.PartNumber,
			"classification", classification)
		for _, p := range d.Status.Ports {
			entry := nodePFEntry{
				pfFingerprint: pfFingerprint{
					DeviceID:   d.Status.Type,
					PciAddress: p.PCI,
				},
				RdmaDevice:         p.RdmaInterface,
				NetworkInterface:   p.NetworkInterface,
				IsNorthSouth:       isDPU,
				IsExplicitEastWest: isBF3SuperNIC,
				PSID:               d.Status.PSID,
				PartNumber:         d.Status.PartNumber,
				ModelName:          d.Status.ModelName,
			}
			ni.pfs = append(ni.pfs, entry)
			if p.RdmaInterface != "" {
				ni.hasRdma = true
			}
			if p.PCI != "" {
				ni.hasPCI = true
			}
		}
	}

	// Step 1.5: Apply part-number frequency heuristic for nodes with 5+ PFs.
	// The most common part number (ideally with a power-of-2 count) is east-west;
	// all other part numbers are north-south.
	nsWarnings := classifyByPartNumberFrequency(nodeMap)

	// Step 2: Compute PF fingerprint per node and group nodes
	type fingerprintKey string
	computeFingerprint := func(pfs []nodePFEntry) fingerprintKey {
		fps := make([]pfFingerprint, len(pfs))
		for i, p := range pfs {
			fps[i] = p.pfFingerprint
		}
		slices.SortFunc(fps, func(a, b pfFingerprint) int {
			if c := strings.Compare(a.DeviceID, b.DeviceID); c != 0 {
				return c
			}
			return strings.Compare(a.PciAddress, b.PciAddress)
		})
		parts := make([]string, len(fps))
		for i, fp := range fps {
			parts[i] = fp.DeviceID + ":" + fp.PciAddress
		}
		return fingerprintKey(strings.Join(parts, "|"))
	}

	// Group nodes by fingerprint, preserving order by first-seen node
	type nodeGroup struct {
		fingerprint fingerprintKey
		nodes       []string
		pfs         []nodePFEntry // representative PFs from first node
		hasRdma     bool
		hasPCI      bool
	}
	fingerprintOrder := []fingerprintKey{}
	groupMap := map[fingerprintKey]*nodeGroup{}

	// Sort node names for deterministic grouping
	sortedNodes := make([]string, 0, len(nodeMap))
	for n := range nodeMap {
		sortedNodes = append(sortedNodes, n)
	}
	slices.Sort(sortedNodes)

	for _, nodeName := range sortedNodes {
		ni := nodeMap[nodeName]
		fp := computeFingerprint(ni.pfs)
		log.Log.V(1).Info("Bucketing node by PCI fingerprint",
			"node", nodeName, "pfCount", len(ni.pfs), "fingerprint", string(fp))
		if g, ok := groupMap[fp]; ok {
			g.nodes = append(g.nodes, nodeName)
			g.hasRdma = g.hasRdma || ni.hasRdma
			g.hasPCI = g.hasPCI || ni.hasPCI
		} else {
			fingerprintOrder = append(fingerprintOrder, fp)
			groupMap[fp] = &nodeGroup{
				fingerprint: fp,
				nodes:       []string{nodeName},
				pfs:         ni.pfs,
				hasRdma:     ni.hasRdma,
				hasPCI:      ni.hasPCI,
			}
		}
	}

	// Drop groups that have no east-west PFs. Such groups carry only
	// north-south interfaces (e.g. BlueField DPUs / OOB NICs), are excluded
	// from every generated manifest anyway, and would otherwise clutter
	// cluster-config.yaml and consume an NV-IPAM subnet slice during
	// pre-allocation — shifting the real per-rail pools. Filtering here (before
	// the singleGroup decision and group-N numbering) keeps identifiers
	// contiguous over the kept groups.
	keptFingerprints := make([]fingerprintKey, 0, len(fingerprintOrder))
	for _, fp := range fingerprintOrder {
		g := groupMap[fp]
		hasEastWest := false
		for _, e := range g.pfs {
			if !e.IsNorthSouth {
				hasEastWest = true
				break
			}
		}
		if hasEastWest {
			keptFingerprints = append(keptFingerprints, fp)
		} else {
			log.Log.Info("Skipping discovered group with no east-west NICs (north-south only); not added to cluster-config",
				"nodes", g.nodes, "pfCount", len(g.pfs))
		}
	}
	fingerprintOrder = keptFingerprints

	// Step 3: Build ClusterConfig per group
	singleGroup := len(fingerprintOrder) == 1
	groups := make([]config.ClusterConfig, 0, len(fingerprintOrder))

	for i, fp := range fingerprintOrder {
		g := groupMap[fp]

		identifier := ""
		if !singleGroup {
			identifier = fmt.Sprintf("group-%d", i)
		}

		// Build PFs from the representative node's entries.
		// If multiple nodes exist in the group, RDMA/net device names may differ — omit them.
		pfs := make([]config.PFConfig, len(g.pfs))
		for j, entry := range g.pfs {
			traffic := "east-west"
			if entry.IsNorthSouth {
				traffic = "north-south"
			}
			pfs[j] = config.PFConfig{
				DeviceID:   entry.DeviceID,
				PciAddress: entry.PciAddress,
				Traffic:    traffic,
				PSID:       entry.PSID,
				PartNumber: entry.PartNumber,
				Model:      entry.ModelName,
			}
			if singleGroup || len(g.nodes) == 1 {
				// Safe to include RDMA/net device names when only one node in group
				pfs[j].RdmaDevice = entry.RdmaDevice
				pfs[j].NetworkInterface = entry.NetworkInterface
			}
		}

		slices.SortFunc(pfs, func(a, b config.PFConfig) int {
			return strings.Compare(a.PciAddress, b.PciAddress)
		})

		// Collapse multi-plane NICs to one rail per NIC (the default). A NIC
		// whose VPD model is genuinely dual-port keeps a rail per port; every
		// other NIC drops all but its master PF, since its sibling PFs are
		// planes of a single rail. North-south PFs are left untouched.
		// --collapse-nic-rails=false skips this and keeps one rail per PF.
		if collapseNicRails {
			collapsed, dropped := collapseGroupRails(pfs)
			if dropped > 0 {
				pfs = collapsed
				log.Log.Info("Collapsed multi-plane PFs to one rail per NIC; pass --collapse-nic-rails=false to keep one rail per PF",
					"groupIndex", i, "identifier", identifier,
					"droppedPFs", dropped, "eastWestRails", len(pfutil.FilterEastWestPFs(pfs)))
			}
		}

		// Assign rail numbers sequentially over the E/W set. No PF has
		// GPUProximity populated yet at this stage, so the helper's PIX-gate
		// branch is a no-op here; only the rail loop runs.
		reclassifyAndReassignRails(pfs)

		slices.Sort(g.nodes)

		// Extract machine/product type from common node labels
		commonLabels := computeCommonLabels(g.nodes, nodeLabels)
		machineType := commonLabels["nvidia.com/gpu.machine"]
		gpuType := commonLabels["nvidia.com/gpu.product"]
		log.Log.V(1).Info("Read GPU operator labels for group",
			"group", identifier,
			"nodes", g.nodes,
			"machineTypeFromLabel", machineType,
			"gpuTypeFromLabel", gpuType,
			"willFallBackToHardwareProbe", machineType == "" || gpuType == "")

		cc := config.ClusterConfig{
			Identifier:   identifier,
			MachineType:  machineType,
			GPUType:      gpuType,
			NodeSelector: nodeSelector,
			Capabilities: &config.ClusterCapabilities{
				Nodes: &config.NodesCapabilities{
					Rdma:  g.hasRdma,
					Sriov: g.hasPCI,
					Ib:    true, // TODO: detect from NicDevice
				},
			},
			PFs:         pfs,
			WorkerNodes: g.nodes,
		}

		groups = append(groups, cc)
	}

	// Step 4: Compute nodeSelectors per group — overrides the initial
	// value with discriminating labels when multiple groups exist.
	if len(groups) > 1 {
		computeNodeSelectors(groups, nodeLabels)
	}

	return groups, nsWarnings
}

// computeNodeSelectors assigns NodeSelectors to each group using ALL label keys
// where groups have differing common values. This ensures every group uses the
// same set of label keys (with different values), making the selectors consistent.
func computeNodeSelectors(groups []config.ClusterConfig, nodeLabels map[string]map[string]string) {
	n := len(groups)
	if n <= 1 {
		return
	}

	// Compute common labels per group (intersection of labels across all nodes)
	groupCommonLabels := make([]map[string]string, n)
	for i, g := range groups {
		groupCommonLabels[i] = computeCommonLabels(g.WorkerNodes, nodeLabels)
	}

	// Collect all label keys present in any group's common labels
	allKeys := map[string]bool{}
	for _, cl := range groupCommonLabels {
		for k := range cl {
			allKeys[k] = true
		}
	}

	// Find all label keys where at least two groups have different common values.
	// A key "differs" if: group A has value X and group B has value Y (Y != X),
	// or one group has the key in common and another doesn't.
	differingKeys := []string{}
	for k := range allKeys {
		values := map[string]bool{}
		missing := false
		for _, cl := range groupCommonLabels {
			v, ok := cl[k]
			if !ok {
				missing = true
			} else {
				values[v] = true
			}
		}
		if len(values) > 1 || (len(values) >= 1 && missing) {
			differingKeys = append(differingKeys, k)
		}
	}

	slices.Sort(differingKeys)

	// Deprioritize feature.node.kubernetes.io/* labels — only include them
	// if the remaining labels can't differentiate all groups on their own.
	var primaryKeys []string
	for _, k := range differingKeys {
		if !strings.HasPrefix(k, "feature.node.kubernetes.io/") {
			primaryKeys = append(primaryKeys, k)
		}
	}
	if canDifferentiate(primaryKeys, groupCommonLabels) {
		differingKeys = primaryKeys
	}
	// else: keep all differingKeys (primary + fallback) as-is

	// Assign ALL differing label keys to each group's NodeSelector
	for i := range groups {
		selector := map[string]string{}
		for _, k := range differingKeys {
			if v, ok := groupCommonLabels[i][k]; ok {
				selector[k] = v
			}
			// If this group doesn't have the key in common, omit it —
			// the key still discriminates because other groups DO have it.
		}
		if len(selector) > 0 {
			groups[i].NodeSelector = selector
		} else {
			log.Log.Info("Warning: could not compute a unique nodeSelector for group",
				"group", groups[i].Identifier, "nodes", groups[i].WorkerNodes)
		}
	}
}

// canDifferentiate returns true if the given label keys are sufficient to produce
// a unique fingerprint for each group (no two groups share the same key-value set).
func canDifferentiate(keys []string, groupLabels []map[string]string) bool {
	if len(keys) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, labels := range groupLabels {
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + labels[k]
		}
		fp := strings.Join(parts, ",")
		if seen[fp] {
			return false
		}
		seen[fp] = true
	}
	return true
}

// computeCommonLabels returns labels with identical values across all specified nodes.
func computeCommonLabels(nodes []string, nodeLabels map[string]map[string]string) map[string]string {
	if len(nodes) == 0 {
		return map[string]string{}
	}

	// Start with labels from the first node
	firstLabels := nodeLabels[nodes[0]]
	if firstLabels == nil {
		return map[string]string{}
	}

	common := make(map[string]string, len(firstLabels))
	for k, v := range firstLabels {
		if isNoisyLabel(k) {
			continue
		}
		common[k] = v
	}

	// Intersect with remaining nodes
	for _, node := range nodes[1:] {
		labels := nodeLabels[node]
		for k, v := range common {
			if labels[k] != v {
				delete(common, k)
			}
		}
	}

	return common
}

// isNoisyLabel returns true for labels that are node-specific or not useful for discrimination.
func isNoisyLabel(key string) bool {
	noisyPrefixes := []string{
		"kubernetes.io/metadata",
		"node.kubernetes.io/instance-type",
		"kubernetes.io/hostname",
		"kubernetes.io/arch",
		"kubernetes.io/os",
		"pod-security.kubernetes.io",
		"topology.kubernetes.io",
	}
	noisyExact := []string{
		"beta.kubernetes.io/arch",
		"beta.kubernetes.io/os",
		"kubernetes.io/hostname",
	}

	for _, prefix := range noisyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, exact := range noisyExact {
		if key == exact {
			return true
		}
	}
	return false
}

// ofedTargetModules is the list of MLX/OFED kernel modules to check for during
// pre-flight discovery. These modules (and any modules that depend on them) may
// need to be blacklisted when the DOCA driver is deployed.
var ofedTargetModules = []string{
	"mlx5_core", "mlx5_ib", "ib_umad", "ib_uverbs",
	"ib_ipoib", "rdma_cm", "rdma_ucm", "ib_core", "ib_cm",
}

// knownStorageModules is the set of storage-over-RDMA kernel modules handled by
// UNLOAD_STORAGE_MODULES in the driver container. The list is sourced from the
// canonical `mofedmodules.DefaultStorageModules` exported by doca-driver-build
// (entrypoint/pkg/mofedmodules) so l8k's classification stays in lockstep with
// what the driver container will actually unload.
var knownStorageModules = buildKnownStorageModulesSet()

func buildKnownStorageModulesSet() map[string]bool {
	m := make(map[string]bool, len(mofedmodules.DefaultStorageModules))
	for _, mod := range mofedmodules.DefaultStorageModules {
		m[mod] = true
	}
	return m
}

// parseMachineTypeFromDMI extracts and sanitizes a machine type string from
// raw /sys/class/dmi/id/product_name content. It trims whitespace/newlines
// and replaces spaces with dashes to match GPU operator label format.
// Returns empty string if input is blank after trimming.
func parseMachineTypeFromDMI(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(s, " ", "-")
}

// parseGPUProductFromNvidiaSmi extracts the first "Product Name" value from
// nvidia-smi -q output and sanitizes it to match GPU operator label format
// (spaces replaced with dashes). Returns empty string if no product name found.
func parseGPUProductFromNvidiaSmi(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "Product Name") {
			continue
		}
		parts := strings.SplitN(line, " : ", 2)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if name == "" {
			continue
		}
		return strings.ReplaceAll(name, " ", "-")
	}
	return ""
}

// discoverHardwareTypes attempts to discover machineType and gpuType by
// probing hardware directly when label-derived values are empty. It execs into
// a nic-configuration-daemon pod on one of the group's nodes.
// Returns (machineType, gpuType) — either or both may still be empty if
// discovery fails or hardware info is unavailable.
func discoverHardwareTypes(ctx context.Context, restConfig *rest.Config,
	namespace string, groupNodes []string, dsPods []corev1.Pod,
	needMachine, needProduct bool) (machineType, gpuType string) {

	targetPod := findDaemonPod(groupNodes, dsPods)
	if targetPod == nil {
		log.Log.Info("No nic-configuration-daemon pod found on group nodes; skipping hardware type probing")
		return "", ""
	}

	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	if needMachine {
		const dmiCmd = "cat /sys/class/dmi/id/product_name 2>/dev/null"
		log.Log.V(1).Info("Probing machine type via DMI",
			"pod", targetPod.Name, "command", dmiCmd)
		output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
			[]string{"/bin/sh", "-c", dmiCmd})
		if err != nil {
			log.Log.Error(err, "failed to read machine type from DMI", "pod", targetPod.Name)
		} else {
			machineType = parseMachineTypeFromDMI(output)
			log.Log.V(1).Info("Probed machine type from DMI",
				"pod", targetPod.Name,
				"rawOutput", truncateForLog(output, 200),
				"parsed", machineType)
		}
	}

	if needProduct {
		// Wrap nvidia-smi so the shell always exits 0: if the binary is absent
		// or crashes, stdout is simply empty and we fall through to the sysfs
		// fallback below instead of surfacing an Error-level exec failure.
		nvidiaSmiCmd := `if [ -x /host/usr/bin/nvidia-smi ]; then ` +
			`LD_LIBRARY_PATH=/host/usr/lib/x86_64-linux-gnu:/host/usr/lib/aarch64-linux-gnu:$LD_LIBRARY_PATH ` +
			`/host/usr/bin/nvidia-smi -q 2>/dev/null || true; fi`
		log.Log.V(1).Info("Probing GPU product type via nvidia-smi", "pod", targetPod.Name)
		output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
			[]string{"/bin/sh", "-c", nvidiaSmiCmd})
		if err != nil {
			log.Log.Error(err, "failed to exec nvidia-smi probe", "pod", targetPod.Name)
		} else {
			gpuType = parseGPUProductFromNvidiaSmi(output)
			log.Log.V(1).Info("Probed GPU product type via nvidia-smi",
				"pod", targetPod.Name,
				"parsed", gpuType,
				"willFallBackToSysfs", gpuType == "")
		}

		if gpuType == "" {
			log.Log.V(1).Info("Falling back to sysfs/pci.ids for GPU product type", "pod", targetPod.Name)
			sysfsOutput, sysfsErr := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
				[]string{"/bin/sh", "-c", sysfsNvidiaGPUIDCmd})
			if sysfsErr != nil {
				log.Log.Error(sysfsErr, "failed to exec sysfs GPU probe", "pod", targetPod.Name)
			} else {
				gpuType = parseGPUProductFromSysfs(sysfsOutput)
				log.Log.V(1).Info("Probed GPU product type via sysfs/pci.ids",
					"pod", targetPod.Name,
					"sysfsID", strings.TrimSpace(sysfsOutput),
					"parsed", gpuType)
				if gpuType == "" && strings.TrimSpace(sysfsOutput) != "" {
					log.Log.Info("GPU product type not resolved from sysfs device ID",
						"pod", targetPod.Name, "unresolvedID", strings.TrimSpace(sysfsOutput))
				}
			}
		}
	}

	return machineType, gpuType
}

// truncateForLog clips a string to maxLen characters and appends "…" when
// the input was longer. Used to keep V(1) probe logs readable for raw
// command output without overwhelming the log volume.
func truncateForLog(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// discoverGroupFabric probes the InfiniBand sysfs entries on a representative
// daemon pod for every east-west PF in `group` that has an RdmaDevice and,
// when the per-port verdicts unanimously agree on a value, sets
// `group.LinkType`. Otherwise the field is left empty — discovery couldn't
// determine the fabric type, and downstream code treats absence as
// "unknown".
//
// The verdict is the port's configured `link_layer` (sysfs file) — what
// the firmware says the port is wired for, regardless of whether the
// netdev is currently up. We deliberately ignore `state` (ACTIVE vs
// DOWN) and `sm_lid` (IB subnet manager presence) here: requiring an
// active link broke discovery on freshly-provisioned clusters where
// the switch wasn't yet plugged in, and the configured link_layer is
// what every downstream template needs anyway. An operator who
// reflashes a card to a different fabric needs to re-run discover, but
// that's the only failure mode we accept in exchange for the
// "discover before the cluster is wired up" win.
//
// Multi-node groups whose RdmaDevice is empty (per the existing
// per-node-vs-group safety rule) skip the probe — there's no ibdev name
// to point sysfs at — but other PFs in the group can still contribute.
func discoverGroupFabric(ctx context.Context, restConfig *rest.Config,
	namespace string, group *config.ClusterConfig, dsPods []corev1.Pod) {

	if restConfig == nil {
		return
	}
	targetPod := findDaemonPod(group.WorkerNodes, dsPods)
	if targetPod == nil {
		log.Log.V(1).Info("Skipping fabric probe: no daemon pod on group nodes",
			"group", group.Identifier)
		return
	}
	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	verdicts := map[string]int{} // linkType -> count of contributing PFs
	probed := 0
	for _, pf := range group.PFs {
		if pf.Traffic != "east-west" || pf.RdmaDevice == "" {
			continue
		}
		probed++
		linkType, raw, err := discoverPortFabric(ctx, restConfig, namespace,
			targetPod.Name, containerName, pf.RdmaDevice, 1)
		if err != nil {
			log.Log.V(1).Info("Fabric probe failed",
				"group", group.Identifier,
				"pod", targetPod.Name,
				"pci", pf.PciAddress,
				"rdmaDevice", pf.RdmaDevice,
				"error", err.Error())
			continue
		}
		log.Log.V(1).Info("Fabric port probe",
			"group", group.Identifier,
			"pod", targetPod.Name,
			"pci", pf.PciAddress,
			"rdmaDevice", pf.RdmaDevice,
			"linkType", linkType,
			"raw", raw)
		if linkType != "" {
			verdicts[linkType]++
		}
	}

	switch {
	case len(verdicts) == 1:
		for k := range verdicts {
			group.LinkType = k
			log.Log.V(1).Info("Group fabric resolved",
				"group", group.Identifier,
				"linkType", k,
				"probedPFs", probed,
				"contributingPFs", verdicts[k])
		}
	case len(verdicts) > 1:
		log.Log.V(1).Info("Group fabric ambiguous (probes disagree); leaving linkType unset",
			"group", group.Identifier,
			"probedPFs", probed,
			"verdicts", verdicts)
	default:
		log.Log.V(1).Info("Group fabric unresolved (no port reported a recognised link_layer); leaving linkType unset",
			"group", group.Identifier,
			"probedPFs", probed)
	}
}

// discoverPortFabric reads
// /sys/class/infiniband/<rdmaDevice>/ports/<port>/link_layer inside the
// daemon pod and returns the configured fabric for that port —
// "Ethernet", "InfiniBand", or "" when the file is empty / unreadable /
// unrecognised. The port's runtime state (ACTIVE / DOWN) is
// intentionally NOT consulted: discovery has to work on freshly
// provisioned clusters where the switch isn't yet plugged in.
//
// Tries `/sys/class/infiniband/...` first (works when the daemon pod
// shares host pid+net namespace and exposes the host sysfs at /sys),
// then falls back to `/host/sys/class/infiniband/...` for daemons that
// mount the host filesystem under /host (matches consts.HostPath =
// "/host" used by the rest of nic-configuration-operator). The first
// path that yields a recognised link_layer wins.
func discoverPortFabric(ctx context.Context, restConfig *rest.Config,
	namespace, podName, containerName, rdmaDevice string, port int) (string, string, error) {
	var lastErr error
	for _, base := range []string{
		fmt.Sprintf("/sys/class/infiniband/%s/ports/%d/link_layer", rdmaDevice, port),
		fmt.Sprintf("/host/sys/class/infiniband/%s/ports/%d/link_layer", rdmaDevice, port),
	} {
		cmd := fmt.Sprintf("cat %s", base)
		output, err := execInPod(ctx, restConfig, namespace, podName, containerName,
			[]string{"/bin/sh", "-c", cmd})
		if err != nil {
			lastErr = err
			log.Log.V(1).Info("Fabric port probe: read failed at this base",
				"rdmaDevice", rdmaDevice, "port", port, "base", base,
				"execErr", err.Error())
			continue
		}
		linkType, raw := parsePortFabricVerdict(output)
		if linkType != "" {
			return linkType, raw, nil
		}
		log.Log.V(1).Info("Fabric port probe: link_layer at this base not recognised",
			"rdmaDevice", rdmaDevice, "port", port, "base", base,
			"raw", raw)
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", nil
}

// parsePortFabricVerdict normalises a sysfs `link_layer` read into the
// l8k vocabulary ("Ethernet" / "InfiniBand"). The output may be the
// raw file content ("Ethernet\n"), a `cat`'s output with possible
// trailing newline, or empty when the file didn't exist. raw is the
// trimmed input echoed back for debug-log breadcrumbs.
func parsePortFabricVerdict(output string) (linkType, raw string) {
	raw = strings.TrimSpace(output)
	return normalizeLinkLayer(raw), raw
}

// normalizeLinkLayer canonicalises sysfs link_layer strings to the YAML
// vocabulary l8k uses elsewhere ("Ethernet" / "InfiniBand"). The kernel
// emits "Ethernet" and "InfiniBand" already, but accept common variants
// (case differences, whitespace) to avoid silent misclassification.
func normalizeLinkLayer(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ethernet":
		return "Ethernet"
	case "infiniband":
		return "InfiniBand"
	default:
		return ""
	}
}

// sysfsNvidiaGPUIDCmd emits the first NVIDIA (vendor 10de) GPU device ID found
// via sysfs. A node is assumed to be homogeneous across its NVIDIA GPUs, so we
// break after the first match. Class filter keeps only VGA (0x030000) and 3D
// controller (0x030200) so NVSwitch / audio / USB-C controllers under vendor
// 10de don't produce false positives. Output is a single line like "0x2335",
// or empty when no NVIDIA GPU is present; exit is always 0.
const sysfsNvidiaGPUIDCmd = `for d in /sys/bus/pci/devices/*; do
  v=$(cat "$d/vendor" 2>/dev/null)
  [ "$v" = "0x10de" ] || continue
  c=$(cat "$d/class" 2>/dev/null)
  case "$c" in 0x030000|0x030200) ;; *) continue ;; esac
  cat "$d/device" 2>/dev/null
  break
done`

// sysfsMellanoxNICPresentCmd echoes "present" if any PCI device under
// /sys/bus/pci/devices has vendor 0x15b3 (Mellanox / NVIDIA networking). Unlike
// the NVIDIA GPU vendor (0x10de, shared by audio/NVSwitch controllers), every
// 0x15b3 device is a NIC or DPU, so no PCI-class filter is needed. Breaks on
// the first match; output is empty when no NVIDIA NIC is present; exit is
// always 0. This replaces the NFD-label filter for deciding which nodes to
// wait on — NFD may not be installed yet at discover time.
const sysfsMellanoxNICPresentCmd = `for d in /sys/bus/pci/devices/*; do
  v=$(cat "$d/vendor" 2>/dev/null)
  if [ "$v" = "0x15b3" ]; then echo present; break; fi
done`

// mellanoxNICPresent reports whether the sysfs probe found an NVIDIA NIC.
// Kept as a tiny pure helper so the decision is unit-testable without a cluster.
func mellanoxNICPresent(output string) bool {
	return strings.TrimSpace(output) != ""
}

// filterNodesWithNICs returns the subset of candidateNodes that have an NVIDIA
// NIC (PCI vendor 0x15b3), determined by execing the sysfs probe in each node's
// daemon pod. Nodes without a daemon pod, or whose probe errors or reports no
// NIC, are dropped. Replaces the NFD-label filter (filterNodesByLabels) so
// discovery works on clusters where NFD is not yet installed.
func filterNodesWithNICs(ctx context.Context, restConfig *rest.Config,
	namespace string, candidateNodes []string, dsPods []corev1.Pod) []string {

	var withNICs []string
	for _, node := range candidateNodes {
		pod := findDaemonPod([]string{node}, dsPods)
		if pod == nil {
			log.Log.V(1).Info("No daemon pod on node; excluding from NIC wait set", "node", node)
			continue
		}
		containerName := ""
		if len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}
		output, err := execInPod(ctx, restConfig, namespace, pod.Name, containerName,
			[]string{"/bin/sh", "-c", sysfsMellanoxNICPresentCmd})
		if err != nil {
			log.Log.Error(err, "failed to probe node for NVIDIA NIC; excluding from wait set",
				"node", node, "pod", pod.Name)
			continue
		}
		present := mellanoxNICPresent(output)
		log.Log.V(1).Info("Probed node for NVIDIA NIC via sysfs vendor 0x15b3",
			"node", node, "pod", pod.Name, "present", present)
		if present {
			withNICs = append(withNICs, node)
		}
	}
	log.Log.Info("Filtered nodes by NVIDIA NIC presence",
		"candidates", len(candidateNodes), "withNICs", len(withNICs))
	return withNICs
}

// parseGPUProductFromSysfs resolves a single-line sysfs device-ID output
// (e.g. "0x2335\n") to a canonical GPUType via the embedded pci.ids table.
// Returns empty for blank input or an unknown device ID.
func parseGPUProductFromSysfs(output string) string {
	id := strings.TrimSpace(output)
	if id == "" {
		return ""
	}
	// The probe emits at most one ID; take the first line defensively.
	if nl := strings.IndexByte(id, '\n'); nl >= 0 {
		id = strings.TrimSpace(id[:nl])
	}
	return pciids.LookupNVIDIA(id)
}

// findDaemonPod returns a nic-configuration-daemon pod running on one of the
// given nodes. Returns nil if no pod is found on any of the nodes.
func findDaemonPod(groupNodes []string, dsPods []corev1.Pod) *corev1.Pod {
	nodeSet := make(map[string]bool, len(groupNodes))
	for _, n := range groupNodes {
		nodeSet[n] = true
	}
	for i := range dsPods {
		if nodeSet[dsPods[i].Spec.NodeName] {
			return &dsPods[i]
		}
	}
	return nil
}

// discoverThirdPartyRDMAModules execs into a nic-configuration-daemon pod on one
// of the group's nodes and discovers third-party RDMA modules that depend on
// OFED target modules via the kernel module holder graph.
func discoverThirdPartyRDMAModules(ctx context.Context, restConfig *rest.Config,
	namespace string, groupNodes []string, dsPods []corev1.Pod) ([]string, error) {

	if len(groupNodes) == 0 {
		return nil, nil
	}

	// Find a daemon pod running on one of the group's nodes.
	// All nodes in a group have identical hardware, so one is sufficient.
	targetPod := findDaemonPod(groupNodes, dsPods)
	if targetPod == nil {
		return nil, fmt.Errorf("no nic-configuration-daemon pod found on group nodes")
	}

	// Build a shell script that does full transitive BFS discovery of
	// third-party RDMA modules. It scans ALL /sys/module/*/holders/ to build a
	// complete reverse dependency map, then BFS-traverses from OFED target
	// modules upward through non-OFED holders. This matches the init
	// container's checker.go approach.
	modList := strings.Join(ofedTargetModules, " ")
	script := fmt.Sprintf(`targets="%s"
awk_script='
BEGIN {
  split(ENVIRON["TARGETS"], arr)
  for (i in arr) target[arr[i]] = 1
}
{
  mod = $1; holder = $2
  holders[mod] = holders[mod] " " holder
}
END {
  n = 0
  for (t in target) {
    split(holders[t], h)
    for (i in h) {
      if (h[i] != "" && !(h[i] in target) && !(h[i] in visited)) {
        visited[h[i]] = 1
        queue[n++] = h[i]
      }
    }
  }
  qi = 0
  while (qi < n) {
    cur = queue[qi++]
    split(holders[cur], h)
    for (i in h) {
      if (h[i] != "" && !(h[i] in target) && !(h[i] in visited)) {
        visited[h[i]] = 1
        queue[n++] = h[i]
      }
    }
  }
  for (m in visited) print m
}'
for mod_dir in /sys/module/*/; do
  mod=$(basename "$mod_dir")
  for dep in "$mod_dir"holders/*; do
    [ -e "$dep" ] && echo "$mod $(basename "$dep")"
  done
done | TARGETS="$targets" awk "$awk_script" | sort -u`, modList)

	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	log.Log.V(1).Info("Probing OFED-dependent kernel modules",
		"pod", targetPod.Name, "ofedTargets", ofedTargetModules)
	output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
		[]string{"/bin/sh", "-c", script})
	if err != nil {
		return nil, fmt.Errorf("exec in pod %q failed: %w", targetPod.Name, err)
	}
	modules := parseModuleList(output, ofedTargetModules)
	log.Log.V(1).Info("Discovered OFED-dependent kernel modules",
		"pod", targetPod.Name,
		"rawOutput", truncateForLog(output, 200),
		"parsed", modules)
	return modules, nil
}

// parseModuleList splits newline-separated module names, deduplicates, sorts them,
// and filters out any modules in the exclude list (the OFED target modules themselves).
func parseModuleList(output string, exclude []string) []string {
	excludeSet := make(map[string]bool, len(exclude))
	for _, m := range exclude {
		excludeSet[m] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		mod := strings.TrimSpace(line)
		if mod != "" && !excludeSet[mod] {
			seen[mod] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	modules := make([]string, 0, len(seen))
	for mod := range seen {
		modules = append(modules, mod)
	}
	sort.Strings(modules)
	return modules
}

// coreRdmaInfrastructureModules is the set of kernel-native RDMA core
// modules that MOFED's openibd unload sequence handles natively. Per
// upstream guidance in `mofedmodules.DefaultThirdPartyRDMAModules`'s
// doc comment ("Do NOT add core RDMA infrastructure modules (iw_cm,
// ib_cm, rdma_cm, rdma_ucm, ib_core, ib_uverbs, etc.)"), these are NOT
// third-party — they're shared kernel infrastructure that the driver
// container does not need to unload separately. l8k discovery silently
// drops them so they never appear as `thirdPartyRDMAModules` in
// cluster-config.yaml or trigger the `unloadThirdPartyRDMAModules`
// auto-enable + warning.
var coreRdmaInfrastructureModules = map[string]bool{
	"iw_cm":     true,
	"ib_cm":     true,
	"rdma_cm":   true,
	"rdma_ucm":  true,
	"ib_core":   true,
	"ib_uverbs": true,
}

// classifyDiscoveredModules splits a list of discovered OFED-dependent modules into
// third-party RDMA modules and storage modules. mlx5-prefixed modules (NVIDIA's own)
// and kernel-native RDMA core modules are silently dropped.
func classifyDiscoveredModules(modules []string) (rdma, storage []string) {
	var droppedMlx, droppedCore []string
	for _, mod := range modules {
		if strings.HasPrefix(mod, "mlx5") {
			droppedMlx = append(droppedMlx, mod)
			continue // NVIDIA module — always greenlit
		}
		if coreRdmaInfrastructureModules[mod] {
			droppedCore = append(droppedCore, mod)
			continue // Kernel-native RDMA core — MOFED's openibd handles it
		}
		if knownStorageModules[mod] {
			storage = append(storage, mod)
		} else {
			rdma = append(rdma, mod)
		}
	}
	log.Log.V(1).Info("Classified OFED-dependent modules",
		"total", len(modules),
		"thirdPartyRDMA", rdma,
		"storage", storage,
		"droppedMlx5Prefixed", droppedMlx,
		"droppedCoreRdma", droppedCore)
	return rdma, storage
}

// execInPod runs a command in a pod container and returns stdout.
// Thin wrapper around the shared kubeclient.ExecStdoutInPod helper —
// kept under this name so internal call sites in this package stay
// untouched.
func execInPod(ctx context.Context, restConfig *rest.Config,
	namespace, podName, containerName string, command []string) (string, error) {
	return kubeclient.ExecStdoutInPod(ctx, restConfig, namespace, podName, containerName, command)
}


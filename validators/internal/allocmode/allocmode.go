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

package allocmode

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// IsK8sTimeoutErr reports whether err represents a timeout rather than a
// generic failure: client-side context cancellation/deadline expiry, a
// Kubernetes apiserver Timeout/ServerTimeout status, a transport-level
// network timeout (*url.Error / net.Error with Timeout() true), or client-go's
// rate-limiter refusing because the wait would exceed the context deadline.
// Callers use it to map such errors to ErrCodeTimeout instead of
// ErrCodeInternal; RBAC and other transient API errors stay ErrCodeInternal.
func IsK8sTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	return stderrors.Is(err, context.Canceled) ||
		stderrors.Is(err, context.DeadlineExceeded) ||
		k8serrors.IsTimeout(err) ||
		k8serrors.IsServerTimeout(err) ||
		utilnet.IsTimeout(err) ||
		// x/time/rate emits "rate: Wait(n=..) would exceed context deadline"
		// as a PLAIN string (no deadline sentinel in the chain); client-go's
		// request.go wraps it with %w, so only the message survives to match.
		strings.Contains(err.Error(), "would exceed context deadline")
}

// ClassifyK8sReadError wraps a Kubernetes object-read failure with the error
// code matching its cause — the SHARED classifier for probe/object reads so
// call sites cannot diverge: a true NotFound keeps ErrCodeNotFound, timeout
// forms (see IsK8sTimeoutErr) map to ErrCodeTimeout, and everything else
// (RBAC denials, transient apiserver errors) stays ErrCodeInternal. subject
// names the object being read, e.g. "ResourceClaim gpu-claim-abc".
func ClassifyK8sReadError(err error, subject string) error {
	switch {
	case k8serrors.IsNotFound(err):
		return errors.Wrap(errors.ErrCodeNotFound, subject+" not found", err)
	case IsK8sTimeoutErr(err):
		return errors.Wrap(errors.ErrCodeTimeout, "timed out reading "+subject, err)
	default:
		return errors.Wrap(errors.ErrCodeInternal, "failed to read "+subject, err)
	}
}

// Package-local mirrors of the identifiers shared with the conformance
// checks' consts.go — duplicated as plain string literals so the extracted
// probe has no import cycle back into a validator binary. Keep in sync with
// validators/conformance/consts.go.
const (
	// apiGroupResourceK8sIO is the Kubernetes DRA API group.
	apiGroupResourceK8sIO = "resource.k8s.io"
	// versionV1beta1 is the oldest served DRA API version the probe accepts.
	versionV1beta1 = "v1beta1"
	// draAPIGroupVersion is the GA group-version, used as the test-fixture default.
	draAPIGroupVersion = apiGroupResourceK8sIO + "/v1"
	// draDriverGPU is the DRA driver (and DeviceClass) name for whole-GPU allocation.
	draDriverGPU = "gpu.nvidia.com"
	// draDriverComputeDomain is the DRA driver name for ComputeDomain (IMEX) channels.
	draDriverComputeDomain = "compute-domain.nvidia.com"
	// resourceNVIDIAGPU is the canonical extended-resource name for NVIDIA GPUs.
	resourceNVIDIAGPU = "nvidia.com/gpu"
)

// APIVersionPreference is the preference-ordered list of resource.k8s.io
// API versions the DRA checks accept, newest first. DRA graduated to GA (v1)
// in K8s 1.34, but the pinned NVIDIA DRA driver chart also supports K8s
// 1.32/1.33 clusters that serve only v1beta1/v1beta2 (the driver itself
// accepts v1|v1beta2|v1beta1), so the checks must not require v1.
var APIVersionPreference = []string{"v1", "v1beta2", versionV1beta1}

// GVRAt returns the GroupVersionResource for a resource.k8s.io resource at
// the given served API version (v1, v1beta2, or v1beta1).
func GVRAt(version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group: apiGroupResourceK8sIO, Version: version, Resource: resource,
	}
}

// DiscoverServedVersion returns the newest SERVED version of the
// resource.k8s.io API group (per APIVersionPreference) along with its
// discovered resource list. The probe honors ctx (see
// draGroupVersionResources) so a validator timeout can abort it. Only
// NotFound is treated as "this version is not served"; any other discovery
// error is ambiguous and propagates (fail closed). When no version is served
// it returns ("", nil, nil) — the caller decides whether that is a failure
// (driver in recipe scope) or not.
//
//nolint:unparam // the APIResourceList result is consumed by validators/conformance (API-resource evidence artifact)
func DiscoverServedVersion(ctx context.Context, clientset kubernetes.Interface) (string, *metav1.APIResourceList, error) {
	for _, version := range APIVersionPreference {
		gv := apiGroupResourceK8sIO + "/" + version
		resources, err := draGroupVersionResources(ctx, clientset, gv)
		if err == nil {
			return version, resources, nil
		}
		if !k8serrors.IsNotFound(err) {
			// Full timeout classification (context sentinels, apiserver
			// Timeout/ServerTimeout, transport, rate-limiter deadline) —
			// a deadline condition is not an apiserver fault.
			code := errors.ErrCodeInternal
			if IsK8sTimeoutErr(err) {
				code = errors.ErrCodeTimeout
			}
			return "", nil, errors.Wrap(code,
				fmt.Sprintf("failed to discover DRA API group-version %s", gv), err)
		}
	}
	return "", nil, nil
}

// draGroupVersionResources fetches the APIResourceList for gv with the
// caller's context. DiscoveryInterface.ServerResourcesForGroupVersion issues
// its request with context.TODO() internally (client-go), so an unresponsive
// apiserver could outlive the validator timeout and hang until the Job is
// killed; issuing the same GET through the discovery REST client keeps the
// request cancelable. Fake discovery clients in tests expose no RESTClient —
// fall back to the interface method there (test-only, in-memory, no I/O).
func draGroupVersionResources(ctx context.Context, clientset kubernetes.Interface, gv string) (*metav1.APIResourceList, error) {
	disc := clientset.Discovery()
	rc := disc.RESTClient()
	if rc == nil {
		// The interface method cannot carry ctx — honor it around the call
		// so a canceled probe still aborts (fake clients are in-memory, so
		// the call itself cannot hang).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		resources, err := disc.ServerResourcesForGroupVersion(gv)
		// Recheck AFTER the call: a cancellation racing the call (e.g. a
		// test reactor canceling mid-request) can still let it return
		// success — a canceled probe must report cancellation, not a result
		// observed under a dead context.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return resources, err
	}
	resources := &metav1.APIResourceList{}
	if err := rc.Get().AbsPath("/apis/" + gv).Do(ctx).Into(resources); err != nil {
		return nil, err
	}
	resources.GroupVersion = gv
	return resources, nil
}

// Mode reports which GPU allocation mechanisms are currently
// usable in the cluster. The CNCF secure_accelerator_access requirement
// permits either DRA or the device plugin; the NVIDIA DRA driver's supported
// configuration is ComputeDomain-only (no gpu.nvidia.com DeviceClass), with
// whole GPUs allocated by the device plugin (nvidia.com/gpu). Mirrors the
// detection semantics of kubernetes-sigs/ai-conformance#75. (#1327)
type Mode struct {
	// APIVersion is the served resource.k8s.io API version the DRA probe ran
	// against (per APIVersionPreference: v1, v1beta2, or v1beta1), or ""
	// when the cluster serves no version of the group. Consumers that create
	// DRA resources (e.g. the secure-access ResourceClaim) must use it so the
	// probe and the behavioral test operate at the same group-version.
	APIVersion string

	// DRAUsable is true when the gpu.nvidia.com DeviceClass exists AND at
	// least one complete, current-generation ResourceSlice pool from driver
	// gpu.nvidia.com advertises an untainted device reachable from a Ready,
	// schedulable node.
	DRAUsable bool
	// DRADetail explains why DRA is or is not usable.
	DRADetail string
	// DRANodes is the sorted set of Ready, schedulable node names with
	// usable full-GPU DRA devices.
	DRANodes []string
	// DRANodeDevices maps each DRANodes entry to its USABLE full-GPU DRA
	// device count (untainted devices in complete, current-generation pools).
	// Consumers that size DRA-mode workloads (e.g. inference-perf worker
	// count) must use this — NOT scalar allocatable nvidia.com/gpu, which on
	// dual-advertised nodes can differ from the DRA-usable device count.
	DRANodeDevices map[string]int
	// NodeLocalGPUSliceDevices maps node name → RAW node-local
	// gpu.nvidia.com device count: slices attributed by raw spec.nodeName
	// only, devices counted without pool/taint validation. This mirrors how
	// kai-scheduler v0.14.1 attributes slices to nodes
	// (kubernetes_lister.go:198, cluster_info.go:270 — kai then rejects
	// scalar device-plugin GPU pods on every counted node, node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix"))
	// and doubles as the SUPPORTED-configuration detector: in AICR's
	// supported full-GPU DRA state the NVIDIA driver publishes one
	// node-local pool per node, so a node with usable validated devices
	// (DRANodes) always appears here too. A node present here but absent
	// from DRANodes carries slices kai counts but AICR cannot validate.
	// Populated whenever the DRA API is served — independent of DeviceClass
	// existence, exactly like kai.
	NodeLocalGPUSliceDevices map[string]int
	// ForeignGPUSliceDrivers lists "gpu"-named ResourceSlice drivers OTHER
	// than gpu.nvidia.com (kai's driver predicate is lowercase-contains
	// "gpu" — dra.go:149 — so e.g. gpu.amd.com slices consume kai's shared
	// GPU capacity vector). AICR's inference validation does not model mixed
	// GPU DRA drivers: consumers fail fast when this is non-empty (#1652).
	// ComputeDomain drivers contain no "gpu" substring and never appear.
	ForeignGPUSliceDrivers []string
	// NonNodeLocalGPUSlices lists gpu.nvidia.com ResourceSlice names using
	// nodeSelector/allNodes/perDeviceNodeSelection topologies. kai cannot
	// node-attribute them (invisible to its capacity vector while the DRA
	// driver can still bind them), which AICR's inference validation does
	// not model: consumers fail fast when this is non-empty (#1652). The
	// conformance checks' generic topology validation is unaffected.
	NonNodeLocalGPUSlices []string
	// GPUPoolNodes maps each node-local gpu.nvidia.com ResourceSlice POOL
	// name to the node (spec.nodeName) whose slices publish it. The K8s API
	// documents that a pool name "is often the node name, but this is not
	// required" (resource/v1 ResourcePool.Name) — so occupancy attribution
	// must resolve pool → node through this slice-derived map, never through
	// pool==node name equality, which mis-attributes whenever a pool is
	// named after a DIFFERENT existing node. Pools listed in
	// AmbiguousGPUPools are excluded.
	GPUPoolNodes map[string]string
	// AmbiguousGPUPools lists gpu.nvidia.com pool names that node-local
	// slices of MULTIPLE nodes publish — pool → node attribution would be a
	// guess: consumers fail fast when this is non-empty (#1652). The NVIDIA
	// driver publishes one pool per node, so this is empty in AICR's
	// supported configurations.
	AmbiguousGPUPools []string

	// DevicePluginUsable is true when a Ready, schedulable node has
	// allocatable nvidia.com/gpu > 0. Kubernetes semantics: a Ready node
	// advertising scalar nvidia.com/gpu allocatable is device-plugin-backed
	// even when a DeviceClass maps that extended resource to DRA (KEP-5004
	// DRAExtendedResource) — the scheduler routes the extended-resource
	// request to DRA only on nodes where scalar allocatable is absent or
	// zero. See ExtendedResourceDRABacked for the recorded attribution caveat.
	DevicePluginUsable bool
	// DevicePluginDetail explains why the device plugin is or is not usable.
	DevicePluginDetail string
	// DevicePluginNodes is the sorted set of Ready, schedulable node names
	// advertising allocatable nvidia.com/gpu.
	DevicePluginNodes []string

	// DualAdvertisedNodes lists Ready, schedulable nodes where BOTH
	// mechanisms advertise GPUs. Dual advertisement risks GPU over-admission
	// (the same physical GPU allocatable through both paths). Currently a
	// warning only; a later PR promotes this to an error.
	DualAdvertisedNodes []string

	// ExtendedResourceDRABacked is true when a DeviceClass maps the
	// nvidia.com/gpu extended resource to DRA via spec.extendedResourceName
	// (KEP-5004 DRAExtendedResource). Recorded as attribution evidence only —
	// it does NOT clear DevicePluginUsable: on nodes with scalar allocatable
	// nvidia.com/gpu the request is device-plugin-served by definition; the
	// DRA mapping applies only where scalar allocatable is absent/zero.
	// Consumers that need deterministic per-node attribution should pin their
	// workload to a node with scalar allocatable (see the secure-access check).
	ExtendedResourceDRABacked bool
	// ExtendedResourceDetail explains the extended-resource attribution state.
	ExtendedResourceDetail string
}

// Summary renders the detection result for evidence artifacts.
func (m *Mode) Summary() string {
	lines := []string{
		fmt.Sprintf("DRA (%s):              usable=%t nodes=[%s]",
			draDriverGPU, m.DRAUsable, strings.Join(m.DRANodes, ",")),
		fmt.Sprintf("Device plugin (%s):  usable=%t nodes=[%s]",
			resourceNVIDIAGPU, m.DevicePluginUsable, strings.Join(m.DevicePluginNodes, ",")),
		m.DRADetail,
		m.DevicePluginDetail,
	}
	if m.ExtendedResourceDetail != "" {
		lines = append(lines, m.ExtendedResourceDetail)
	}
	// Standing eligibility caveat (#1620 review): node ELIGIBILITY here is
	// Ready + uncordoned only — node taints are NOT evaluated, so a Ready
	// node carrying e.g. dedicated=infra:NoSchedule still counts as usable.
	// Workloads are expected to carry matching tolerations (the validate
	// command's --toleration flag).
	lines = append(lines,
		"note: node eligibility is Ready+uncordoned only — node taints are not evaluated; workloads must tolerate any node taints")
	if len(m.DualAdvertisedNodes) > 0 {
		lines = append(lines, fmt.Sprintf(
			"WARNING: both mechanisms advertise GPUs on node(s) [%s] — GPU over-admission risk",
			strings.Join(m.DualAdvertisedNodes, ",")))
	}
	if len(m.NodeLocalGPUSliceDevices) > 0 {
		names := make(map[string]struct{}, len(m.NodeLocalGPUSliceDevices))
		for n := range m.NodeLocalGPUSliceDevices {
			names[n] = struct{}{}
		}
		parts := make([]string, 0, len(names))
		for _, n := range SortedNodeNames(names) {
			parts = append(parts, fmt.Sprintf("%s=%d", n, m.NodeLocalGPUSliceDevices[n]))
		}
		lines = append(lines, fmt.Sprintf(
			"node-local raw %s ResourceSlice devices (unvalidated, kai-attributable): [%s] — kai-scheduler rejects scalar device-plugin GPU pods on these nodes",
			draDriverGPU, strings.Join(parts, ",")))
	}
	if len(m.ForeignGPUSliceDrivers) > 0 {
		lines = append(lines, fmt.Sprintf(
			"WARNING: non-NVIDIA GPU DRA driver ResourceSlices present: [%s] — outside AICR's supported validation matrix (#1652)",
			strings.Join(m.ForeignGPUSliceDrivers, ",")))
	}
	if len(m.NonNodeLocalGPUSlices) > 0 {
		lines = append(lines, fmt.Sprintf(
			"WARNING: non-node-local %s ResourceSlice topology: [%s] — kai cannot node-attribute these; outside AICR's supported inference validation matrix (#1652)",
			draDriverGPU, strings.Join(m.NonNodeLocalGPUSlices, ",")))
	}
	if len(m.AmbiguousGPUPools) > 0 {
		lines = append(lines, fmt.Sprintf(
			"WARNING: %s ResourceSlice pool(s) published by multiple nodes: [%s] — pool → node attribution is ambiguous; outside AICR's supported inference validation matrix (#1652)",
			draDriverGPU, strings.Join(m.AmbiguousGPUPools, ",")))
	}
	return strings.Join(lines, "\n")
}

// Detect probes the cluster for usable GPU allocation
// mechanisms: full-GPU DRA (gpu.nvidia.com) and the device plugin
// (nvidia.com/gpu). The DRA probe runs at the newest SERVED resource.k8s.io
// API version (v1, v1beta2, or v1beta1 — see DiscoverServedVersion), so
// beta-only clusters (K8s 1.32/1.33) are detected as DRA-usable too. When
// both mechanisms are live on the same node(s) it emits a warning about GPU
// over-admission risk (it does NOT fail — a later PR promotes dual
// advertisement to an error).
func Detect(parent context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface) (*Mode, error) {
	ctx, cancel := context.WithTimeout(parent, defaults.CollectorK8sTimeout)
	defer cancel()

	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		// Shared classifier: timeout forms map to ErrCodeTimeout so a probe
		// cut off by its deadline is not misreported as an internal fault.
		return nil, ClassifyK8sReadError(err, "nodes for GPU allocation mode detection")
	}

	// Eligible nodes: Ready and schedulable (not cordoned).
	eligible := EligibleReadySchedulableNodes(nodeList)
	pluginNodes := make(map[string]struct{})
	for name, node := range eligible {
		if qty, ok := node.Status.Allocatable[corev1.ResourceName(resourceNVIDIAGPU)]; ok && qty.Sign() > 0 {
			pluginNodes[name] = struct{}{}
		}
	}

	version, _, err := DiscoverServedVersion(ctx, clientset)
	if err != nil {
		return nil, err
	}

	mode := &Mode{
		APIVersion:        version,
		DevicePluginNodes: SortedNodeNames(pluginNodes),
	}

	if len(pluginNodes) > 0 {
		mode.DevicePluginUsable = true
		mode.DevicePluginDetail = fmt.Sprintf(
			"device plugin usable: %d Ready, schedulable node(s) with allocatable %s",
			len(pluginNodes), resourceNVIDIAGPU)
	} else {
		mode.DevicePluginDetail = fmt.Sprintf(
			"device plugin not usable: no Ready, schedulable node advertises allocatable %s",
			resourceNVIDIAGPU)
	}

	if version == "" {
		// No served resource.k8s.io version: DRA (and any KEP-5004
		// extended-resource mapping) is deterministically absent.
		mode.DRADetail = fmt.Sprintf(
			"full-GPU DRA not usable: no served %s API version — DeviceClass %s unreachable",
			apiGroupResourceK8sIO, draDriverGPU)
		mode.ExtendedResourceDetail = fmt.Sprintf(
			"extended-resource attribution: DRA API not served — %s cannot be DRA-backed",
			resourceNVIDIAGPU)
		return mode, nil
	}

	// DRAExtendedResource attribution evidence: when a DeviceClass maps the
	// nvidia.com/gpu extended resource to DRA (KEP-5004), record it as a
	// per-node attribution caveat. It does NOT affect usability: a Ready node
	// advertising scalar nvidia.com/gpu allocatable is device-plugin-backed
	// even with such a mapping — DRA satisfies the extended-resource request
	// only where scalar allocatable is absent/zero.
	erBacked, erDetail, gpuClass, err := detectExtendedResourceDRABacked(ctx, dynClient, version)
	if err != nil {
		return nil, err
	}
	mode.ExtendedResourceDRABacked = erBacked
	mode.ExtendedResourceDetail = erDetail

	// List ResourceSlices ONCE (when the API is served) and derive both
	// views from the same items: kai's raw node classification (independent
	// of DeviceClass existence, exactly like kai) and AICR's validated
	// usability below.
	var sliceItems []unstructured.Unstructured
	if version != "" {
		sliceList, listErr := dynClient.Resource(GVRAt(version, "resourceslices")).List(ctx, metav1.ListOptions{})
		switch {
		case listErr == nil:
			sliceItems = sliceList.Items
		case k8serrors.IsNotFound(listErr):
			// ResourceSlice API not available at this version — nothing for
			// kai or the validated probe to see.
		default:
			return nil, ClassifyK8sReadError(listErr, "ResourceSlices")
		}
	}
	mode.NodeLocalGPUSliceDevices, mode.GPUPoolNodes, mode.ForeignGPUSliceDrivers,
		mode.NonNodeLocalGPUSlices, mode.AmbiguousGPUPools = scanGPUSliceTopology(sliceItems)

	draNodeDevices, draDetail, err := detectFullGPUDRA(ctx, version, eligible, sliceItems, gpuClass)
	if err != nil {
		return nil, err
	}
	draNodes := make(map[string]struct{}, len(draNodeDevices))
	for node := range draNodeDevices {
		draNodes[node] = struct{}{}
	}
	mode.DRAUsable = len(draNodes) > 0
	mode.DRANodes = SortedNodeNames(draNodes)
	mode.DRANodeDevices = draNodeDevices
	mode.DRADetail = draDetail

	// Dual-advertisement detection: warn (do not fail) when both mechanisms
	// are live on the same node set. Full-GPU DRA slices AND scalar
	// allocatable on the same node is a real over-admission risk — the same
	// physical GPU is reachable through both allocation paths — including
	// under a KEP-5004 extendedResourceName mapping.
	if mode.DRAUsable && mode.DevicePluginUsable {
		var dual []string
		for name := range draNodes {
			if _, ok := pluginNodes[name]; ok {
				dual = append(dual, name)
			}
		}
		slices.Sort(dual)
		if len(dual) > 0 {
			mode.DualAdvertisedNodes = dual
			// TODO(#1327 follow-up): promote dual advertisement to an error.
			slog.Warn("both full-GPU DRA (gpu.nvidia.com) and the device plugin (nvidia.com/gpu) advertise GPUs on the same node(s) — GPU over-admission risk: the same physical GPU can be admitted through both mechanisms",
				"nodes", strings.Join(dual, ","))
		}
	}

	return mode, nil
}

// detectFullGPUDRA determines whether full-GPU DRA is usable: the
// gpu.nvidia.com DeviceClass exists AND at least one ResourceSlice with
// spec.driver == gpu.nvidia.com in a complete, current-generation pool
// advertises an untainted device reachable from a Ready, schedulable node.
// version is the served resource.k8s.io API version discovered by
// DiscoverServedVersion (v1, v1beta2, or v1beta1). class is the
// gpu.nvidia.com DeviceClass from the attribution probe's single List (nil
// when absent — no second apiserver round-trip per Detect).
// Returns the set of usable node names and a human-readable detail.
func detectFullGPUDRA(ctx context.Context, version string, eligible map[string]*corev1.Node, sliceItems []unstructured.Unstructured, class *unstructured.Unstructured) (map[string]int, string, error) {
	// DeviceClass gpu.nvidia.com must exist.
	if class == nil {
		detail := fmt.Sprintf("full-GPU DRA not usable: DeviceClass %s not found", draDriverGPU)
		// Half-installation surface (#1620 review): the driver
		// publishing VALIDATED slices while the class is missing is a
		// broken install (class deleted / not created), not a benign
		// ComputeDomain-only configuration — name it so a downstream
		// N/A disposition is diagnosable from the evidence alone.
		usable, _, cntErr := UsableDriverDeviceCounts(ctx, sliceItems, draDriverGPU, eligible)
		if cntErr != nil {
			return nil, "", cntErr
		}
		if len(usable) > 0 {
			detail += fmt.Sprintf(
				" — but %d Ready, schedulable node(s) carry validated %s ResourceSlices: HALF-INSTALLED full-GPU DRA (driver publishing slices, DeviceClass missing or deleted)",
				len(usable), draDriverGPU)
		}
		return nil, detail, nil
	}
	// Known limitation: the class's CEL device selectors are NOT evaluated
	// (like DeviceTaintRule objects, that is scheduler territory) — a
	// selector-restricted class could exclude the advertised devices and
	// still be reported usable here. Surface the presence of selectors in
	// the detail so a false-usable verdict is diagnosable from evidence.
	selectorNote := ""
	if selectors, found, _ := unstructured.NestedSlice(class.Object, "spec", "selectors"); found && len(selectors) > 0 {
		selectorNote = fmt.Sprintf("; note: DeviceClass carries %d CEL selector(s), which this probe does not evaluate", len(selectors))
	}

	usable, seen, err := UsableDriverDeviceCounts(ctx, sliceItems, draDriverGPU, eligible)
	if err != nil {
		return nil, "", err
	}
	if seen == 0 {
		return nil, fmt.Sprintf(
			"full-GPU DRA not usable: DeviceClass %s exists but no ResourceSlices from driver %s",
			draDriverGPU, draDriverGPU), nil
	}
	if len(usable) == 0 {
		return nil, fmt.Sprintf(
			"full-GPU DRA not usable: DeviceClass %s exists but no complete, current-generation ResourceSlice pool advertises an untainted device on a Ready, schedulable node",
			draDriverGPU), nil
	}
	return usable, fmt.Sprintf(
		"full-GPU DRA usable: DeviceClass %s exists (%s/%s) and %d Ready, schedulable node(s) have advertised devices%s",
		draDriverGPU, apiGroupResourceK8sIO, version, len(usable), selectorNote), nil
}

// UsableDriverSliceNodes validates the ResourceSlices published by the given
// DRA driver and returns the set of Ready, schedulable node names reachable
// from at least one untainted advertised device. A slice only counts when it
// belongs to a complete, current-generation pool (see currentPoolSlices) —
// incomplete, stale, or inconsistent pools are ignored. The second return
// value is the number of slices the driver published (before validation), so
// callers can distinguish "driver publishes nothing" from "slices exist but
// none pass validation".
func UsableDriverSliceNodes(ctx context.Context, items []unstructured.Unstructured, driver string, eligible map[string]*corev1.Node) (map[string]struct{}, int, error) {
	counts, seen, err := UsableDriverDeviceCounts(ctx, items, driver, eligible)
	if err != nil {
		return nil, 0, err
	}
	usable := make(map[string]struct{}, len(counts))
	for node := range counts {
		usable[node] = struct{}{}
	}
	return usable, seen, nil
}

// scanGPUSliceTopology derives the inference-support detectors from the raw
// ResourceSlice list in one pass, mirroring kai-scheduler v0.14.1's
// classification where relevant (raw spec.nodeName attribution only; devices
// counted without pool/taint validation; driver predicate lowercase-contains
// "gpu" — dra.go:149):
//   - node-local gpu.nvidia.com device counts per node (the supported state);
//   - the pool → node attribution map from those same slices (the K8s API
//     does not require pool names to be node names, so occupancy accounting
//     resolves pools through this map, never name equality) plus the pools
//     published by multiple nodes, whose attribution is ambiguous
//     (unsupported, #1652);
//   - foreign "gpu"-named drivers (unsupported: mixed GPU DRA drivers, #1652);
//   - non-node-local gpu.nvidia.com slice names (unsupported topology for
//     kai-scheduled inference — kai cannot node-attribute them, #1652).
func scanGPUSliceTopology(items []unstructured.Unstructured) (nodeLocal map[string]int, poolNodes map[string]string, foreignDrivers, nonNodeLocal, ambiguousPools []string) {
	nodeLocal = make(map[string]int)
	poolNodes = make(map[string]string)
	seenForeign := make(map[string]struct{})
	ambiguous := make(map[string]struct{})
	for i := range items {
		spec, found, err := unstructured.NestedMap(items[i].Object, "spec")
		if err != nil || !found {
			continue
		}
		driver, _, _ := unstructured.NestedString(spec, "driver")
		if !strings.Contains(strings.ToLower(driver), "gpu") {
			continue // ComputeDomain and other non-GPU drivers: irrelevant here
		}
		if driver != draDriverGPU {
			if _, dup := seenForeign[driver]; !dup {
				seenForeign[driver] = struct{}{}
				foreignDrivers = append(foreignDrivers, driver)
			}
			continue
		}
		nodeName, _, _ := unstructured.NestedString(spec, "nodeName")
		if nodeName == "" {
			nonNodeLocal = append(nonNodeLocal, items[i].GetName())
			continue
		}
		devices, _, _ := unstructured.NestedSlice(spec, "devices")
		if len(devices) > 0 {
			// Zero-device node-local slices occur in the wild (upstream
			// dra-driver-nvidia-gpu #1008, GKE/COS). kai-scheduler sets
			// HasDRAGPUs only for a POSITIVE aggregate device count, so a
			// node whose slices are all empty must not appear in the raw
			// kai-attributable map — a zero entry would emit false
			// "kai rejects scalar pods here" evidence in Mode.Summary while
			// the consumers' > 0 guards (correctly) never fire.
			nodeLocal[nodeName] += len(devices)
		}
		poolName, _, _ := unstructured.NestedString(spec, "pool", "name")
		if poolName == "" {
			continue // no pool identity to attribute allocations through
		}
		if prev, mapped := poolNodes[poolName]; mapped && prev != nodeName {
			ambiguous[poolName] = struct{}{}
			continue
		}
		poolNodes[poolName] = nodeName
	}
	for pool := range ambiguous {
		delete(poolNodes, pool)
		ambiguousPools = append(ambiguousPools, pool)
	}
	slices.Sort(foreignDrivers)
	slices.Sort(nonNodeLocal)
	slices.Sort(ambiguousPools)
	return nodeLocal, poolNodes, foreignDrivers, nonNodeLocal, ambiguousPools
}

// UsableDriverDeviceCounts is UsableDriverSliceNodes with per-node usable
// DEVICE counts: for each Ready, schedulable node it returns how many
// untainted devices the driver advertises in complete, current-generation
// pools. Sizing consumers (DRA-mode worker counts) need the device count,
// not just node membership.
//
// Counting invariant (#1620 review): under allNodes, multi-node
// nodeSelector, or per-device allNodes topologies one physical device would
// increment the count for EVERY reachable node, over-reporting aggregate
// capacity N×. That state is deliberately unreachable for gpu.nvidia.com
// consumers: non-node-local topologies are surfaced via
// Mode.NonNodeLocalGPUSlices and rejected fail-fast (#1652) BEFORE any
// sizing reads these counts. If that fail-fast is ever relaxed, this
// counting must be revisited first.
func UsableDriverDeviceCounts(ctx context.Context, items []unstructured.Unstructured, driver string, eligible map[string]*corev1.Node) (map[string]int, int, error) {
	// Group the driver's slices by pool name (pool names are scoped per driver).
	pools := make(map[string][]*unstructured.Unstructured)
	seen := 0
	for i := range items {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, errors.Wrap(errors.ErrCodeTimeout,
				"ResourceSlice validation canceled", ctxErr)
		}
		s := &items[i]
		sliceDriver, _, _ := unstructured.NestedString(s.Object, "spec", "driver")
		if sliceDriver != driver {
			continue
		}
		seen++
		poolName, _, _ := unstructured.NestedString(s.Object, "spec", "pool", "name")
		pools[poolName] = append(pools[poolName], s)
	}

	counts := make(map[string]int)
	for poolName, poolSlices := range pools {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, errors.Wrap(errors.ErrCodeTimeout,
				"ResourceSlice validation canceled", ctxErr)
		}
		current, ok := currentPoolSlices(poolSlices)
		if !ok {
			slog.Debug("ignoring incomplete or inconsistent ResourceSlice pool",
				"driver", driver, "pool", poolName)
			continue
		}
		for _, s := range current {
			collectUsableDeviceCounts(s, eligible, counts)
		}
	}
	return counts, seen, nil
}

// detectExtendedResourceDRABacked reports whether any DeviceClass maps the
// nvidia.com/gpu extended resource to DRA via spec.extendedResourceName
// (KEP-5004 DRAExtendedResource). The result is recorded as attribution
// evidence: on nodes WITHOUT scalar allocatable nvidia.com/gpu such a mapping
// lets DRA serve the extended-resource request, so cluster-wide attribution
// of nvidia.com/gpu to the device plugin carries a per-node caveat. Only
// "DRA API not served" (NotFound) reports false without evidence; any other
// DeviceClass list error propagates (fail closed). version is the served
// resource.k8s.io API version to list DeviceClasses at.
// It also returns the gpu.nvidia.com DeviceClass object when present (nil
// otherwise), so detectFullGPUDRA can reuse the single List instead of a
// second per-Detect apiserver round-trip.
func detectExtendedResourceDRABacked(ctx context.Context, dynClient dynamic.Interface, version string) (bool, string, *unstructured.Unstructured, error) {
	classList, err := dynClient.Resource(GVRAt(version, "deviceclasses")).List(ctx, metav1.ListOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false, fmt.Sprintf(
				"extended-resource attribution: DRA API not served — %s cannot be DRA-backed",
				resourceNVIDIAGPU), nil, nil
		}
		return false, "", nil, ClassifyK8sReadError(err, "DeviceClasses for extended-resource attribution")
	}
	var gpuClass *unstructured.Unstructured
	backed := false
	detail := ""
	for i := range classList.Items {
		item := &classList.Items[i]
		if item.GetName() == draDriverGPU {
			gpuClass = item
		}
		if !backed {
			erName, _, _ := unstructured.NestedString(item.Object, "spec", "extendedResourceName")
			if erName == resourceNVIDIAGPU {
				backed = true
				detail = fmt.Sprintf(
					"extended-resource attribution: DeviceClass %s maps %s to DRA (spec.extendedResourceName) — allocatable %s may be served by DRA, not the device plugin",
					item.GetName(), resourceNVIDIAGPU, resourceNVIDIAGPU)
			}
		}
	}
	if !backed {
		detail = fmt.Sprintf(
			"extended-resource attribution: no DeviceClass maps %s to DRA", resourceNVIDIAGPU)
	}
	return backed, detail, gpuClass, nil
}

// currentPoolSlices returns the slices at the pool's maximum observed
// generation, or ok=false when the current generation is incomplete or
// inconsistent: every max-generation slice must report the same
// spec.pool.resourceSliceCount, the count must be > 0, the number of
// observed max-generation slices must equal it, and device names must be
// unique across the generation's slices. The Kubernetes allocator rejects
// pools with duplicate device names, so a pool that advertises them is
// invalid and must not count as usable capacity (fail closed). Slices below
// the max generation are ignored (stale rollout leftovers).
func currentPoolSlices(poolSlices []*unstructured.Unstructured) ([]*unstructured.Unstructured, bool) {
	maxGen := int64(0)
	first := true
	for _, s := range poolSlices {
		gen, _, _ := unstructured.NestedInt64(s.Object, "spec", "pool", "generation")
		if first || gen > maxGen {
			maxGen = gen
			first = false
		}
	}

	var current []*unstructured.Unstructured
	want := int64(-1)
	for _, s := range poolSlices {
		gen, _, _ := unstructured.NestedInt64(s.Object, "spec", "pool", "generation")
		if gen != maxGen {
			continue
		}
		count, found, err := unstructured.NestedInt64(s.Object, "spec", "pool", "resourceSliceCount")
		if err != nil || !found {
			return nil, false
		}
		if want == -1 {
			want = count
		} else if want != count {
			return nil, false
		}
		current = append(current, s)
	}
	if want <= 0 || int64(len(current)) != want {
		return nil, false
	}
	// Device names must be unique across the current generation's slices —
	// the Kubernetes allocator rejects pools that advertise duplicates
	// (mirrors dynamic-resource-allocation/structured pool validation).
	seenDevices := make(map[string]struct{})
	for _, s := range current {
		devices, _, _ := unstructured.NestedSlice(s.Object, "spec", "devices")
		for _, d := range devices {
			dev, ok := d.(map[string]interface{})
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(dev, "name")
			if name == "" {
				continue
			}
			if _, dup := seenDevices[name]; dup {
				return nil, false
			}
			seenDevices[name] = struct{}{}
		}
	}
	return current, true
}

// collectUsableDeviceCounts adds to out, per Ready/schedulable node name, the
// number of devices the slice advertises there without inline
// NoSchedule/NoExecute device taints. It resolves all four ResourceSlice
// topologies: nodeName, nodeSelector, allNodes, and perDeviceNodeSelection
// (where nodeName/nodeSelector/allNodes live on each device). Per-device
// fields are read through deviceFields, which normalizes the v1beta1 `basic`
// wrapper (v1beta1 nests taints and per-device topology under Device.Basic;
// v1beta2/v1 flatten them onto the device itself).
func collectUsableDeviceCounts(slice *unstructured.Unstructured, eligible map[string]*corev1.Node, out map[string]int) {
	spec, found, err := unstructured.NestedMap(slice.Object, "spec")
	if err != nil || !found {
		return
	}
	perDevice, _, _ := unstructured.NestedBool(spec, "perDeviceNodeSelection")

	var sliceNodes []string
	if !perDevice {
		sliceNodes = resolveTopologyNodes(spec, eligible)
		if len(sliceNodes) == 0 {
			return
		}
	}

	devices, _, _ := unstructured.NestedSlice(spec, "devices")
	for _, d := range devices {
		dev, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		fields := deviceFields(dev)
		if deviceHasSchedulingTaint(fields) {
			continue
		}
		nodes := sliceNodes
		if perDevice {
			nodes = resolveTopologyNodes(fields, eligible)
		}
		for _, n := range nodes {
			out[n]++
		}
	}
}

// deviceFields returns the map carrying a ResourceSlice device's detail
// fields (taints, per-device nodeName/nodeSelector/allNodes, attributes).
// In resource.k8s.io/v1beta1 the Device type is a `{name, basic}` union and
// every detail field lives under the `basic` wrapper (BasicDevice); v1beta2
// and v1 flattened BasicDevice onto the device itself. When a `basic` map is
// present, descend into it; otherwise the device map itself carries the
// fields.
func deviceFields(dev map[string]interface{}) map[string]interface{} {
	if basic, ok := dev["basic"].(map[string]interface{}); ok {
		return basic
	}
	return dev
}

// resolveTopologyNodes resolves a topology carrier (a ResourceSlice spec, or
// a device's field map when perDeviceNodeSelection is set — same field names
// across resource.k8s.io versions; v1beta1's `basic` wrapper is unwrapped by
// deviceFields before this is called) to the eligible node names it can reach.
func resolveTopologyNodes(obj map[string]interface{}, eligible map[string]*corev1.Node) []string {
	if nodeName, _, _ := unstructured.NestedString(obj, "nodeName"); nodeName != "" {
		if _, ok := eligible[nodeName]; ok {
			return []string{nodeName}
		}
		return nil
	}
	if allNodes, _, _ := unstructured.NestedBool(obj, "allNodes"); allNodes {
		names := make([]string, 0, len(eligible))
		for name := range eligible {
			names = append(names, name)
		}
		return names
	}
	if selObj, found, _ := unstructured.NestedMap(obj, "nodeSelector"); found {
		sel := &corev1.NodeSelector{}
		if convErr := runtime.DefaultUnstructuredConverter.FromUnstructured(selObj, sel); convErr != nil {
			slog.Debug("failed to decode ResourceSlice nodeSelector; treating as unreachable",
				"error", convErr)
			return nil
		}
		var names []string
		for name, node := range eligible {
			if nodeMatchesSelector(node, sel) {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

// deviceHasSchedulingTaint reports whether the device carries an inline
// device taint with effect NoSchedule or NoExecute. An unparseable taint
// entry is treated as tainted (fail closed).
//
// Known limitation: externally-applied DeviceTaintRule objects (beta
// DRADeviceTaints gate; served only at resource.k8s.io/v1alpha3 and v1beta2)
// are NOT consulted — evaluating their driver/pool/device selectors is
// scheduler territory, like DeviceClass CEL selectors. A rule tainting every
// GPU would make the probe report DRA usable while the toleration-less test
// claim cannot allocate; the resulting failure is noisy (pod Pending →
// timeout), not silent.
func deviceHasSchedulingTaint(dev map[string]interface{}) bool {
	taints, found, err := unstructured.NestedSlice(dev, "taints")
	if err != nil {
		return true
	}
	if !found {
		return false
	}
	for _, t := range taints {
		tm, ok := t.(map[string]interface{})
		if !ok {
			return true
		}
		effect, _, _ := unstructured.NestedString(tm, "effect")
		if effect == string(corev1.TaintEffectNoSchedule) || effect == string(corev1.TaintEffectNoExecute) {
			return true
		}
	}
	return false
}

// nodeMatchesSelector reports whether the node matches the NodeSelector
// (OR across terms, AND within a term). Minimal local implementation:
// k8s.io/component-helpers is not vendored, so this supports the operators
// the NVIDIA DRA driver emits — In, NotIn, Exists, DoesNotExist — on
// matchExpressions (node labels) and matchFields (metadata.name only).
// Unsupported operators (Gt, Lt) or field keys fail closed (no match).
func nodeMatchesSelector(node *corev1.Node, sel *corev1.NodeSelector) bool {
	for _, term := range sel.NodeSelectorTerms {
		if nodeMatchesTerm(node, term) {
			return true
		}
	}
	return false
}

func nodeMatchesTerm(node *corev1.Node, term corev1.NodeSelectorTerm) bool {
	// Per API semantics, a term with no requirements matches no objects.
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return false
	}
	for _, req := range term.MatchExpressions {
		val, exists := node.Labels[req.Key]
		if !matchNodeSelectorRequirement(req, val, exists) {
			return false
		}
	}
	for _, req := range term.MatchFields {
		if req.Key != "metadata.name" {
			return false
		}
		if !matchNodeSelectorRequirement(req, node.Name, true) {
			return false
		}
	}
	return true
}

func matchNodeSelectorRequirement(req corev1.NodeSelectorRequirement, val string, exists bool) bool {
	switch req.Operator { //nolint:exhaustive // Gt/Lt intentionally fall to the fail-closed default
	case corev1.NodeSelectorOpIn:
		return exists && slices.Contains(req.Values, val)
	case corev1.NodeSelectorOpNotIn:
		// Kubernetes NotIn semantics: also matches when the label key is
		// absent from the node (only a present, listed value excludes it).
		return !exists || !slices.Contains(req.Values, val)
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	default:
		// Gt/Lt unsupported by this minimal matcher — fail closed.
		return false
	}
}

// EligibleReadySchedulableNodes indexes the Ready, schedulable (uncordoned)
// nodes from the list by name.
func EligibleReadySchedulableNodes(nodeList *corev1.NodeList) map[string]*corev1.Node {
	eligible := make(map[string]*corev1.Node, len(nodeList.Items))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if nodeReadySchedulable(node) {
			eligible[node.Name] = node
		}
	}
	return eligible
}

// nodeReadySchedulable reports whether the node is Ready and not cordoned.
func nodeReadySchedulable(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func SortedNodeNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

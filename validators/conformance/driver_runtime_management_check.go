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
	"os"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// driver-runtime-management mirrors the upstream
// TestAcceleratorDriverRuntimeManagement (kubernetes-sigs/ai-conformance
// test/driver_runtime_management_test.go at fd9ba2d, KAR-0001): a pod built
// from an image that ships no accelerator driver or tools requests one GPU;
// inside it the container runtime configuration must have injected the vendor
// tools, the driver must initialize and report a version, and that version
// must match the one the platform advertises for the node. The platform's
// advertisement is read from the DRA ResourceSlice device attribute first and
// from the node's GPU Feature Discovery label otherwise; a platform that
// advertises nothing is recorded as a warning, never failed.
//
// Probe pod names and shape. The pod prefix plus the 32-char run token stays
// within the 63-char DNS-1123 limit. The RESULT prefixes are the upstream
// ones, so a log reads the same either way. The device pattern matches
// allocatable NVIDIA GPU device nodes and excludes the control devices
// (/dev/nvidiactl, /dev/nvidia-uvm, /dev/nvidia-caps); the probe requested
// exactly one GPU, so it must see exactly one.
//
// Probe image. The default is the upstream default (ubuntu:22.04), fully
// qualified like defaults.ProbeImage. A vanilla OS image is the point of the
// test: nvidia-smi and the NVML library can only appear inside it through the
// container runtime configuration (CDI spec or runtime hook), so finding them
// proves the injection path. Like that busybox probe and the NCCL workload
// image it is NOT rewritten by AICR_VALIDATOR_IMAGE_REGISTRY: that variable
// names where the validator images live (a dev lane points it at a personal
// namespace holding only those), and a live run with it set would otherwise
// pull <registry>/ubuntu:22.04 and fail on ErrImagePull. The env var carries
// a full replacement reference for air-gapped mirrors, forwarded from the
// orchestrator's shell by pkg/validator/v1 (the same literal is defined
// there; keep in sync); it mirrors the upstream -driver-runtime-image flag.
const (
	driverRuntimeProbePodPrefix = "driver-runtime-probe-"
	driverRuntimeProbeContainer = "prober"
	driverRuntimeProbeImage     = "docker.io/library/ubuntu:22.04"
	driverRuntimeImageEnv       = "AICR_DRIVER_RUNTIME_IMAGE"

	driverRuntimeCountPrefix        = "RESULT: ACCELERATOR_COUNT="
	driverRuntimeConfigOKPrefix     = "RESULT: RUNTIME_CONFIG_OK="
	driverRuntimeFunctionalPrefix   = "RESULT: DRIVER_FUNCTIONAL="
	driverRuntimeActualDriverPrefix = "RESULT: ACTUAL_DRIVER_VERSION="

	driverRuntimeDevicePattern   = "/dev/nvidia[0-9]*"
	driverRuntimeExpectedDevices = 1

	shellTrue                = "true"
	driverRuntimeVerdictPass = "PASS"
)

// Where the platform advertises the installed driver version, and the keys it
// is read from. The DRA attribute is what NVIDIA/k8s-dra-driver-gpu publishes
// as a version attribute on every gpu.nvidia.com device. The GFD label is the
// upstream test's node-metadata key, published by the gpu-operator gfd operand
// (enabled in recipes/components/gpu-operator); the split major/minor/rev
// labels are the deprecated form GFD still emits beside it and are the
// fallback an older GFD leaves as the only advertisement.
const (
	driverVersionMechanismDRA          = "dra"
	driverVersionMechanismNodeMetadata = "node-metadata"

	draDriverVersionAttribute = "driverVersion"
	gfdDriverVersionLabel     = "nvidia.com/cuda.driver-version.full"
	gfdLegacyDriverMajorLabel = "nvidia.com/cuda.driver.major"
	gfdLegacyDriverMinorLabel = "nvidia.com/cuda.driver.minor"
	gfdLegacyDriverRevLabel   = "nvidia.com/cuda.driver.rev"
)

// driverRuntimeProbeScript is the upstream NVIDIA driver probe verbatim in
// substance: count the GPU device nodes, locate nvidia-smi (PATH, /usr/bin,
// or the /usr/local/nvidia tree GKE's managed stack mounts without touching
// the container environment), query the driver version with a zero exit
// status and a purely numeric answer, and print the RESULT lines. It exits 0
// whatever it observed: the verdict belongs to verifyDriverRuntime, and a
// terminal pod is what lets the check collect complete logs.
const driverRuntimeProbeScript = `count=0
for device in ` + driverRuntimeDevicePattern + `; do
  [ -e "$device" ] || continue
  count=$((count + 1))
done
echo "` + driverRuntimeCountPrefix + `$count"
if [ -d /usr/local/nvidia/lib64 ]; then
  export LD_LIBRARY_PATH="/usr/local/nvidia/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi
SMI_BIN=""
for candidate in "$(command -v nvidia-smi 2>/dev/null)" /usr/bin/nvidia-smi /usr/local/nvidia/bin/nvidia-smi; do
  if [ -n "$candidate" ] && [ -x "$candidate" ]; then
    SMI_BIN="$candidate"
    break
  fi
done
TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
  TIMEOUT_CMD="timeout 30"
fi
RUNTIME_OK="false"
DRIVER_OK="false"
ACTUAL_DRIVER=""
if [ -n "$SMI_BIN" ] && [ "$count" -gt 0 ]; then
  RUNTIME_OK="true"
  QUERY_OUT="$($TIMEOUT_CMD "$SMI_BIN" --query-gpu=driver_version --format=csv,noheader,nounits 2>/dev/null)"
  QUERY_RC=$?
  QUERY_VER="$(printf '%s\n' "$QUERY_OUT" | head -n 1 | tr -d '[:space:]')"
  if [ "$QUERY_RC" -eq 0 ] && [ -n "$QUERY_VER" ] && [ -z "$(printf '%s' "$QUERY_VER" | tr -d '0-9.')" ]; then
    DRIVER_OK="true"
    ACTUAL_DRIVER="$QUERY_VER"
  else
    echo "nvidia-smi exit status $QUERY_RC, output: $QUERY_OUT"
  fi
fi
if [ "$DRIVER_OK" != "true" ] && [ -r /proc/driver/nvidia/version ]; then
  cat /proc/driver/nvidia/version
fi
echo "` + driverRuntimeConfigOKPrefix + `$RUNTIME_OK"
echo "` + driverRuntimeFunctionalPrefix + `$DRIVER_OK"
echo "` + driverRuntimeActualDriverPrefix + `$ACTUAL_DRIVER"
exit 0`

// advertisedDriverVersion is the driver version the platform advertises for
// one node and where it was read from.
type advertisedDriverVersion struct {
	Mechanism string
	NodeName  string
	// Version is empty when the platform advertises none; Key names the
	// attribute or label it was read from.
	Version string
	Key     string
	// Notes carries what was consulted on the way to the result (a DRA
	// listing with no attribute, a legacy label composition), for evidence.
	Notes []string
}

// driverRuntimeProbeResults is what the probe container observed.
type driverRuntimeProbeResults struct {
	AcceleratorCount    int64
	HasAcceleratorCount bool
	RuntimeConfigOK     bool
	DriverFunctional    bool
	ActualDriverVersion string
	// Complete is true when every RESULT line was present.
	Complete bool
}

// CheckDriverRuntimeManagement validates the CNCF driver_runtime_management
// requirement (SHOULD at v1.37, MUST at v1.38): the platform's accelerator
// driver and container runtime configuration are installed and compatible on
// a GPU node, proven from inside a vanilla-image pod, and the version the
// driver reports matches the one the platform advertises.
//
// The probe pod requests its GPU the way secure-accelerator-access does: the
// recipe's allocation policy selects the mechanism (DRA ResourceClaim or
// device-plugin extended resource), the device-plugin pod is constrained to
// the probe's device-plugin nodes, and the context tolerations apply. Every
// resource lives in a per-run namespace whose cleanup is registered before
// the first Create.
//
// Skipped when slinky-slurm is in the recipe: the NodeSet holds every GPU, so
// a GPU-requesting probe pod would never schedule.
func CheckDriverRuntimeManagement(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if recipeHasComponent(ctx, "slinky-slurm") {
		return validators.SkipWithReason(validators.SkipReasonSlurmManagedGPUs,
			"Slurm-managed GPU allocation (slinky-slurm in recipe): the NodeSet holds every GPU, so a Kubernetes-scheduled driver probe pod cannot be granted one")
	}

	// Bound all work so one bounded namespace cleanup plus margin fit inside
	// the check's deadline (gpuCheckWorkBudget); fail fast before creating
	// anything when they cannot.
	budget, err := gpuCheckWorkBudget(ctx.Ctx, driverRuntimeWorkBudget)
	if err != nil {
		return err
	}
	workCtx, cancelWork := ctx.Timeout(budget)
	defer cancelWork()
	work := *ctx
	work.Ctx = workCtx
	ctx = &work

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	mode, err := detectGPUAllocationMode(ctx.Ctx, ctx.Clientset, dynClient)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "GPU allocation mode",
		"kubectl get deviceclass gpu.nvidia.com; kubectl get resourceslices; kubectl get nodes -o wide",
		mode.Summary())

	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	recordRawTextArtifact(ctx, "GPU allocation policy", "", fmt.Sprintf("configured policy: %s", policy))
	validators.EmitExtraOrWarn(validators.GPUAllocationPolicyExtra(policy))
	if verifyErr := verifyGPUAllocationPolicy(policy, mode); verifyErr != nil {
		return verifyErr
	}

	image := resolveDriverRuntimeProbeImage()
	recordRawTextArtifact(ctx, "Driver runtime probe image", "",
		fmt.Sprintf("image: %s (override: %s)", image, driverRuntimeImageOverrideNote()))

	switch {
	case policy == validatorv1.GPUAllocationPolicyDRAResourceClaim,
		policy == validatorv1.GPUAllocationPolicyUnspecified && mode.DRAUsable:
		recordRawTextArtifact(ctx, "Driver probe allocation", "", "mode exercised: DRA (gpu.nvidia.com ResourceClaim)")
		validators.EmitExtraOrWarn(validators.GPUAllocationResolvedExtra(true, false))
		return runDriverRuntimeTest(ctx, dynClient, mode.APIVersion, true, nil, image)
	case policy == validatorv1.GPUAllocationPolicyDevicePluginExtendedResource,
		policy == validatorv1.GPUAllocationPolicyUnspecified && mode.DevicePluginUsable:
		recordRawTextArtifact(ctx, "Driver probe allocation", "", "mode exercised: device plugin (nvidia.com/gpu limits)")
		validators.EmitExtraOrWarn(validators.GPUAllocationResolvedExtra(false, false))
		return runDriverRuntimeTest(ctx, dynClient, mode.APIVersion, false, mode.DevicePluginNodes, image)
	default:
		return errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"no usable GPU allocation mechanism for the driver runtime probe: %s; %s",
			mode.DRADetail, mode.DevicePluginDetail))
	}
}

// resolveDriverRuntimeProbeImage returns the probe image: the
// AICR_DRIVER_RUNTIME_IMAGE reference verbatim when set, else the default.
// Neither validator image override (AICR_VALIDATOR_IMAGE_REGISTRY,
// AICR_VALIDATOR_IMAGE_TAG) applies; see driverRuntimeProbeImage.
func resolveDriverRuntimeProbeImage() string {
	if override := strings.TrimSpace(os.Getenv(driverRuntimeImageEnv)); override != "" {
		return override
	}
	return driverRuntimeProbeImage
}

func driverRuntimeImageOverrideNote() string {
	if strings.TrimSpace(os.Getenv(driverRuntimeImageEnv)) != "" {
		return driverRuntimeImageEnv
	}
	return valueNone
}

// runDriverRuntimeTest creates the per-run namespace and the probe pod
// (with a ResourceClaim in DRA mode), waits for the probe to finish, reads the
// advertised version for the node it landed on, and compares. The return is
// NAMED so the deferred cleanup can fail an otherwise-passing check when
// cleanup terminally fails; a primary error is never overwritten.
func runDriverRuntimeTest(ctx *validators.Context, dynClient dynamic.Interface, draVersion string, useDRA bool, pluginNodes []string, image string) (err error) {
	if !useDRA && len(pluginNodes) == 0 {
		return errors.New(errors.ErrCodeInternal, "device plugin driver runtime probe invoked without any device-plugin node")
	}
	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}
	// Register cleanup BEFORE the first Create: an ambiguously created pod
	// would otherwise stay bound and hold a GPU (see cleanupGPUTestNamespace).
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		status, cleanupErr := cleanupGPUTestNamespace(ctx.Clientset, run)
		recordRawTextArtifact(ctx, "Cleanup test namespace", cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()

	if err = createGPUTestNamespace(ctx.Ctx, ctx.Clientset, run); err != nil {
		return err
	}
	if useDRA {
		claim := buildResourceClaim(run, draVersion)
		if _, err = dynClient.Resource(draGVRAt(draVersion, "resourceclaims")).Namespace(run.namespace).Create(
			ctx.Ctx, claim, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create ResourceClaim for the driver runtime probe", err)
		}
	}
	pod := buildDriverRuntimeProbePod(run, image, ctx.Tolerations, useDRA, pluginNodes)
	if err = createPodWhenSAReady(ctx.Ctx, ctx.Clientset, run.namespace, pod); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create driver runtime probe pod")
	}

	pod, err = waitForTerminalPod(ctx.Ctx, ctx.Clientset, run.namespace, run.driverProbePodName, "driver runtime probe pod")
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Driver runtime probe pod status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", run.driverProbePodName, run.namespace),
		fmt.Sprintf("Name:   %s/%s\nPhase:  %s\nNode:   %s\nImage:  %s",
			pod.Namespace, pod.Name, pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName), image))

	// Logs BEFORE any verdict: they are the primary diagnostic.
	logs, logErr := readDriverRuntimeProbeLogs(ctx.Ctx, ctx.Clientset, pod)
	if logErr != nil {
		recordRawTextArtifact(ctx, "Driver runtime probe pod logs",
			fmt.Sprintf("kubectl logs %s -n %s", pod.Name, pod.Namespace), fmt.Sprintf("failed to read logs: %v", logErr))
		return logErr
	}
	recordRawTextArtifact(ctx, "Driver runtime probe pod logs",
		fmt.Sprintf("kubectl logs %s -n %s", pod.Name, pod.Namespace), logs)

	if pod.Spec.NodeName == "" {
		return errors.New(errors.ErrCodeInternal, "driver runtime probe pod reached a terminal phase without a node name")
	}
	advertised, err := inspectAdvertisedDriverVersion(ctx.Ctx, ctx.Clientset, dynClient, draVersion, useDRA, pod.Spec.NodeName)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Advertised driver version",
		fmt.Sprintf("kubectl get node %s -o jsonpath='{.metadata.labels}'; kubectl get resourceslices -o yaml", pod.Spec.NodeName),
		advertised.describe())

	probe := parseDriverRuntimeProbeLogs(logs)
	verdictErr := verifyDriverRuntime(advertised, probe, pod)
	recordRawTextArtifact(ctx, "Driver runtime compatibility", "", describeDriverRuntimeVerdict(advertised, probe, verdictErr))
	return verdictErr
}

// buildDriverRuntimeProbePod returns the probe pod: one container from the
// vanilla image that requests exactly one GPU, through the run's ResourceClaim
// in DRA mode or nvidia.com/gpu limits otherwise. In device-plugin mode the
// pod is constrained by required node affinity to the detected device-plugin
// nodes (nodeNameAffinity); in DRA mode it is left to the scheduler, since a
// spec.nodeName pin bypasses the dynamicresources plugin and leaves the claim
// unallocated. tolerations, when non-nil, replace the tolerate-all default.
func buildDriverRuntimeProbePod(run *gpuTestRun, image string, tolerations []corev1.Toleration, useDRA bool, pluginNodes []string) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	container := corev1.Container{
		Name:    driverRuntimeProbeContainer,
		Image:   image,
		Command: []string{posixShell, "-c", driverRuntimeProbeScript},
	}
	spec := corev1.PodSpec{
		RestartPolicy:    corev1.RestartPolicyNever,
		Tolerations:      tolerations,
		ImagePullSecrets: run.imagePullSecretRefs(),
	}
	if useDRA {
		spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name:              gpuClaimName,
			ResourceClaimName: helper.StrPtr(run.claimName),
		}}
		container.Resources.Claims = []corev1.ResourceClaim{{Name: gpuClaimName}}
	} else {
		spec.Affinity = nodeNameAffinity(pluginNodes)
		container.Resources.Limits = corev1.ResourceList{
			corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse(strconv.Itoa(driverRuntimeExpectedDevices)),
		}
	}
	spec.Containers = []corev1.Container{container}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: run.driverProbePodName, Namespace: run.namespace},
		Spec:       spec,
	}
}

// readDriverRuntimeProbeLogs returns the probe container's logs.
func readDriverRuntimeProbeLogs(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod) (string, error) {
	logBytes, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(
		pod.Name, &corev1.PodLogOptions{Container: driverRuntimeProbeContainer}).DoRaw(ctx)
	if err != nil {
		return "", classifyK8sReadError(err, "driver runtime probe pod logs")
	}
	return string(logBytes), nil
}

// parseDriverRuntimeProbeLogs reads the RESULT lines. Complete is set only
// when every line was present; a missing line means the probe did not run to
// its end and the check fails rather than judging partial output.
func parseDriverRuntimeProbeLogs(logs string) driverRuntimeProbeResults {
	var res driverRuntimeProbeResults
	var sawRuntimeOK, sawDriverOK, sawActualDriver bool
	for raw := range strings.SplitSeq(logs, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, driverRuntimeCountPrefix):
			if n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, driverRuntimeCountPrefix)), 10, 64); err == nil {
				res.AcceleratorCount = n
				res.HasAcceleratorCount = true
			}
		case strings.HasPrefix(line, driverRuntimeConfigOKPrefix):
			res.RuntimeConfigOK = resultLineTrue(line, driverRuntimeConfigOKPrefix)
			sawRuntimeOK = true
		case strings.HasPrefix(line, driverRuntimeFunctionalPrefix):
			res.DriverFunctional = resultLineTrue(line, driverRuntimeFunctionalPrefix)
			sawDriverOK = true
		case strings.HasPrefix(line, driverRuntimeActualDriverPrefix):
			res.ActualDriverVersion = strings.TrimSpace(strings.TrimPrefix(line, driverRuntimeActualDriverPrefix))
			sawActualDriver = true
		}
	}
	res.Complete = res.HasAcceleratorCount && sawRuntimeOK && sawDriverOK && sawActualDriver
	return res
}

// resultLineTrue reports whether a RESULT line's value, after its prefix, is
// the shell literal true.
func resultLineTrue(line, prefix string) bool {
	return strings.TrimSpace(strings.TrimPrefix(line, prefix)) == shellTrue
}

// inspectAdvertisedDriverVersion resolves the driver version the platform
// advertises for nodeName. DRA ResourceSlice device attributes are preferred
// whenever the cluster serves the DRA API (KAR-0001 asks platforms to use DRA
// once the accelerator exposes the version there); node labels are the
// fallback. In DRA mode a failed slice listing is an error, because the claim
// was allocated from that node's slices; otherwise the listing is a
// best-effort read recorded in Notes. Advertising nothing is not an error.
func inspectAdvertisedDriverVersion(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, draVersion string, draMode bool, nodeName string) (*advertisedDriverVersion, error) {
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, classifyK8sReadError(err, fmt.Sprintf("GPU node %s", nodeName))
	}
	info := &advertisedDriverVersion{NodeName: node.Name}

	if draVersion != "" {
		found, listErr := draNodeDriverVersion(ctx, dynClient, draVersion, node.Name)
		switch {
		case listErr != nil && draMode:
			return nil, listErr
		case listErr != nil:
			info.Notes = append(info.Notes, fmt.Sprintf("DRA ResourceSlices unread (%v); falling back to node metadata", listErr))
		case found.version != "":
			info.Mechanism = driverVersionMechanismDRA
			info.Key, info.Version = draDriverVersionAttribute, found.version
			return info, nil
		case draMode && found.devices == 0:
			return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"no %s ResourceSlice device for node %s; the probe pod's claim was allocated from this node so it must back slices", draDriverGPU, node.Name))
		case found.devices == 0:
			info.Notes = append(info.Notes, fmt.Sprintf("no %s ResourceSlice device for node %s; falling back to node metadata",
				draDriverGPU, node.Name))
		default:
			info.Notes = append(info.Notes, fmt.Sprintf("WARNING: the %d %s ResourceSlice device(s) for node %s expose no %s attribute; falling back to node metadata",
				found.devices, draDriverGPU, node.Name, draDriverVersionAttribute))
		}
	}

	info.Mechanism = driverVersionMechanismNodeMetadata
	info.Key, info.Version = nodeDriverVersion(node)
	if info.Version != "" && info.Key != gfdDriverVersionLabel {
		info.Notes = append(info.Notes, "composed from the legacy GFD major/minor/rev labels; the full label is absent")
	}
	if info.Version == "" {
		info.Notes = append(info.Notes, fmt.Sprintf(
			"WARNING: node %s advertises no accelerator driver version (checked %s and the legacy %s/%s/%s labels); driver and runtime compatibility is verified by the in-container probe alone",
			node.Name, gfdDriverVersionLabel, gfdLegacyDriverMajorLabel, gfdLegacyDriverMinorLabel, gfdLegacyDriverRevLabel))
	}
	return info, nil
}

// draNodeDevices is what the gpu.nvidia.com ResourceSlices say about one
// node: how many devices they publish for it and the first driverVersion
// attribute among them ("" when none advertises one).
type draNodeDevices struct {
	devices int
	version string
}

// draNodeDriverVersion reads the gpu.nvidia.com ResourceSlice devices
// published for nodeName (node-local slices, the topology the NVIDIA driver
// uses) and the first driverVersion attribute among them, as a version or
// string attribute value.
func draNodeDriverVersion(ctx context.Context, dynClient dynamic.Interface, version, nodeName string) (draNodeDevices, error) {
	var found draNodeDevices
	slices, err := dynClient.Resource(draGVRAt(version, "resourceslices")).List(ctx, metav1.ListOptions{})
	if err != nil {
		return found, classifyK8sReadError(err, "DRA ResourceSlices")
	}
	for i := range slices.Items {
		spec, hasSpec, _ := unstructured.NestedMap(slices.Items[i].Object, "spec")
		if !hasSpec {
			continue
		}
		if driver, _, _ := unstructured.NestedString(spec, "driver"); driver != draDriverGPU {
			continue
		}
		if sliceNode, _, _ := unstructured.NestedString(spec, "nodeName"); sliceNode != nodeName {
			continue
		}
		devices, _, _ := unstructured.NestedSlice(spec, "devices")
		for _, d := range devices {
			dev, ok := d.(map[string]any)
			if !ok {
				continue
			}
			// v1beta1 nests device detail under `basic`; v1beta2 and v1
			// flatten it onto the device.
			if basic, isBasic := dev["basic"].(map[string]any); isBasic {
				dev = basic
			}
			found.devices++
			if found.version == "" {
				found.version = deviceAttributeValue(dev, draDriverVersionAttribute)
			}
		}
	}
	return found, nil
}

// deviceAttributeValue returns the version or string value of the named
// device attribute, or "" when absent or empty.
func deviceAttributeValue(dev map[string]any, name string) string {
	attrs, found, _ := unstructured.NestedMap(dev, "attributes")
	if !found {
		return ""
	}
	attr, ok := attrs[name].(map[string]any)
	if !ok {
		return ""
	}
	for _, field := range []string{"version", "string"} {
		if v, isString := attr[field].(string); isString && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// nodeDriverVersion returns the driver version the node's GFD labels (or
// annotations, which upstream also consults) advertise: the full label when
// present, else the legacy major.minor.rev composition when all three parts
// are present. The returned key names the source.
func nodeDriverVersion(node *corev1.Node) (key, version string) {
	lookup := func(k string) string {
		if v := strings.TrimSpace(node.Labels[k]); v != "" {
			return v
		}
		return strings.TrimSpace(node.Annotations[k])
	}
	if v := lookup(gfdDriverVersionLabel); v != "" {
		return gfdDriverVersionLabel, v
	}
	major, minor, rev := lookup(gfdLegacyDriverMajorLabel), lookup(gfdLegacyDriverMinorLabel), lookup(gfdLegacyDriverRevLabel)
	if major != "" && minor != "" && rev != "" {
		return gfdLegacyDriverMajorLabel + "+" + gfdLegacyDriverMinorLabel + "+" + gfdLegacyDriverRevLabel,
			major + "." + minor + "." + rev
	}
	return "", ""
}

func (a *advertisedDriverVersion) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Node:      %s\nMechanism: %s\n", a.NodeName, a.Mechanism)
	if a.Version == "" {
		b.WriteString("Version:   <none advertised>\n")
	} else {
		fmt.Fprintf(&b, "Version:   %s\nKey:       %s\n", a.Version, a.Key)
	}
	for _, n := range a.Notes {
		b.WriteString(n)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// verifyDriverRuntime is the verdict: the probe printed every RESULT line,
// saw exactly the requested device count, found the injected vendor tools,
// got a functional driver that reported a version, and that version matches
// the advertised one at shared precision when the platform advertises any.
// Every failure is a disproven capability (ErrCodeInternal), the code the
// other GPU allocation checks use for a failed subtest.
func verifyDriverRuntime(advertised *advertisedDriverVersion, probe driverRuntimeProbeResults, pod *corev1.Pod) error {
	if !probe.Complete {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"driver runtime probe did not report every RESULT line (pod phase %s, exit code %d); the probe cannot be judged from partial output",
			pod.Status.Phase, podExitCode(pod)))
	}
	if probe.AcceleratorCount != driverRuntimeExpectedDevices {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"container saw %d accelerator device(s), expected %d", probe.AcceleratorCount, driverRuntimeExpectedDevices))
	}
	if !probe.RuntimeConfigOK {
		return errors.New(errors.ErrCodeInternal,
			"container runtime configuration did not inject the accelerator driver tools (no nvidia-smi via CDI or a runtime hook) into the vanilla-image container")
	}
	if !probe.DriverFunctional || probe.ActualDriverVersion == "" {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"accelerator driver failed to initialize or report a numeric version inside the container (DRIVER_FUNCTIONAL=%t, ACTUAL_DRIVER_VERSION=%q)",
			probe.DriverFunctional, probe.ActualDriverVersion))
	}
	if advertised.Version != "" && !versionsCompatible(advertised.Version, probe.ActualDriverVersion) {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"platform %s mechanism advertises driver version %q (%s) on node %s, which does not match the driver version %q observed in the container",
			advertised.Mechanism, advertised.Version, advertised.Key, advertised.NodeName, probe.ActualDriverVersion))
	}
	return nil
}

func describeDriverRuntimeVerdict(advertised *advertisedDriverVersion, probe driverRuntimeProbeResults, verdictErr error) string {
	advertisedText := "<none advertised> (recorded as a warning; the in-container probe alone verifies the stack)"
	if advertised.Version != "" {
		advertisedText = fmt.Sprintf("%s via %s [%s]", advertised.Version, advertised.Mechanism, advertised.Key)
	}
	verdict := driverRuntimeVerdictPass
	if verdictErr != nil {
		verdict = "FAIL: " + verdictErr.Error()
	}
	return fmt.Sprintf("devices seen:       %d (expected %d)\nruntime config ok:  %t\ndriver functional:  %t\nobserved version:   %s\nadvertised version: %s\ncomparison:         shared numeric precision (580.65 matches 580.65.06; 580.65.6 matches 580.65.06)\nverdict:            %s",
		probe.AcceleratorCount, driverRuntimeExpectedDevices, probe.RuntimeConfigOK, probe.DriverFunctional,
		valueOrUnknown(probe.ActualDriverVersion), advertisedText, verdict)
}

// versionsCompatible reports whether two version strings describe the same
// version at the precision they share (upstream versionsCompatible): numeric
// segments compare as integers, so "580.65.06" matches "580.65.6", and a
// shorter version matches a longer one when every shared segment agrees
// ("580.65" and "580.65.06", in either order). Empty strings never match;
// non-numeric strings must be equal ignoring case.
func versionsCompatible(a, b string) bool {
	aParts, okA := parseNumericVersionParts(a)
	bParts, okB := parseNumericVersionParts(b)
	if !okA || !okB {
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		return a != "" && strings.EqualFold(a, b)
	}
	for i := 0; i < min(len(aParts), len(bParts)); i++ {
		if aParts[i] != bParts[i] {
			return false
		}
	}
	return true
}

// parseNumericVersionParts splits "v1.2.3-rc1+meta" into [1 2 3]; it reports
// false for anything non-numeric.
func parseNumericVersionParts(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	if v == "" {
		return nil, false
	}
	rawParts := strings.Split(v, ".")
	parts := make([]int, 0, len(rawParts))
	for _, p := range rawParts {
		n, err := strconv.Atoi(p)
		if p == "" || err != nil || n < 0 {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

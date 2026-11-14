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
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// workload-sandboxing mirrors the upstream TestWorkloadSandboxing
// (kubernetes-sigs/ai-conformance test/workload_sandboxing_test.go at fd9ba2d,
// KAR-0020): the platform provides a sandboxed RuntimeClass (gVisor, Kata or
// another kernel-isolating runtime) that isolates a workload from the host
// node's kernel, processes, filesystem and network. A probe pod runs on that
// RuntimeClass and reports four isolation probes; the same probe runs in an
// unsandboxed control pod pinned to the node the sandboxed pod landed on, and
// the two must observe different kernel identities, because the isolation
// probes alone cannot tell a sandbox from a plain runc container.
//
// Three deliberate departures from upstream. Upstream SKIPS when no sandboxed
// RuntimeClass is found; this check FAILS with "unmet prerequisite: no
// sandboxed RuntimeClass" (#3615: under the upstream rules a MUST-level N/A is
// allowed only for the conditional cluster_autoscaling, so a platform without
// a sandbox runtime is Not Implemented at SHOULD and blocked once
// workload_sandboxing is MUST at v1.38, never not-applicable). Upstream keeps
// the probe pods Running with a trailing sleep and polls their logs; this
// check lets the probe script exit 0 and waits for a terminal pod, the shape
// every other probe in this package uses, so the logs are complete when read.
// Upstream's probe image is busybox; this check defaults to
// docker.io/library/ubuntu:22.04, the driver-runtime-management image, so the
// two vanilla probes share one mirror story (AICR_SANDBOX_PROBE_IMAGE).
//
// No accelerator is requested: a sandbox runtime on a CPU pool satisfies the
// requirement, and sandboxed GPU support is a separate decision.
//
// Probe image. The default is a vanilla OS image, fully qualified like
// defaults.ProbeImage. Like that busybox probe and the NCCL workload image it
// is NOT rewritten by AICR_VALIDATOR_IMAGE_REGISTRY (that variable names where
// the validator images live); AICR_SANDBOX_PROBE_IMAGE carries a full
// replacement reference for air-gapped mirrors, forwarded from the
// orchestrator's shell by pkg/validator/v1 (the same literal is defined there;
// keep in sync). AICR_SANDBOX_RUNTIME_CLASS names the RuntimeClass to test
// instead of auto-detecting one, mirroring the upstream -sandbox-runtime-class
// flag, and is forwarded the same way.
//
// sandboxProbeTestLabel is the upstream label stamped on both probe pods.
// sandboxMissingPrerequisite is the failure message prefix #3615 names for a
// cluster with no sandboxed RuntimeClass. sandboxSelectionOverride and
// sandboxSelectionDetected name how the RuntimeClass under test was chosen,
// and sandboxMatchFieldName and sandboxMatchFieldHandler which RuntimeClass
// field a keyword matched, for the evidence artifact.
const (
	sandboxProbePodPrefix   = "sandbox-probe-"
	sandboxControlPodPrefix = "sandbox-control-"
	sandboxProbeContainer   = "prober"
	sandboxProbeImage       = "docker.io/library/ubuntu:22.04"
	sandboxProbeImageEnv    = "AICR_SANDBOX_PROBE_IMAGE"
	sandboxRuntimeClassEnv  = "AICR_SANDBOX_RUNTIME_CLASS"

	sandboxProbeTestLabel = "ai-conformance.kubernetes.io/test"
	sandboxProbeTestValue = "workload-sandboxing"

	sandboxProbeCPURequest    = "50m"
	sandboxProbeMemoryRequest = "64Mi"
	sandboxProbeCPULimit      = "200m"
	sandboxProbeMemoryLimit   = "128Mi"

	sandboxMissingPrerequisite = "unmet prerequisite: no sandboxed RuntimeClass"

	sandboxSelectionOverride = "named by " + sandboxRuntimeClassEnv
	sandboxSelectionDetected = "auto-detected"
	sandboxMatchFieldName    = "name"
	sandboxMatchFieldHandler = "handler"
)

// The probe script's output lines, verbatim from upstream so a log reads the
// same either way. SANDBOX_INFO lines carry what the pod observed;
// SANDBOX_PROBE lines carry each probe's PASS/FAIL verdict; the completed
// marker is the last line the script prints. sandboxInfoUnknown is what the
// script prints for a value it could not read; the parser normalizes it to ""
// (no information).
const (
	sandboxInfoKernelReleasePrefix = "SANDBOX_INFO: KERNEL_RELEASE="
	sandboxInfoKernelVersionPrefix = "SANDBOX_INFO: KERNEL_VERSION="
	sandboxInfoBootIDPrefix        = "SANDBOX_INFO: BOOT_ID="
	sandboxInfoPIDCountPrefix      = "SANDBOX_INFO: PID_COUNT="
	sandboxInfoInterfacesPrefix    = "SANDBOX_INFO: INTERFACES="

	sandboxProbePIDIsolationPrefix    = "SANDBOX_PROBE: PID_ISOLATION="
	sandboxProbeKernelIsolationPrefix = "SANDBOX_PROBE: KERNEL_ISOLATION="
	sandboxProbeFSIsolationPrefix     = "SANDBOX_PROBE: FS_ISOLATION="
	sandboxProbeNetIsolationPrefix    = "SANDBOX_PROBE: NET_ISOLATION="
	sandboxProbeCompletedMarker       = "SANDBOX_PROBE: COMPLETED"

	sandboxProbePass   = "PASS"
	sandboxProbeFail   = "FAIL"
	sandboxInfoUnknown = "unknown"
)

// sandboxRuntimeKeywords are the upstream isKnownSandboxedRuntime keywords: a
// RuntimeClass whose name or handler contains one is a known sandboxed
// runtime (gVisor's runsc, Kata including AKS's kata-mshv-vm-isolation,
// Quark, libkrun). Lower-cased substring match, in this order.
var sandboxRuntimeKeywords = []string{"gvisor", "runsc", "kata", "sandboxed", "quark", "krun"}

// sandboxProbeScript is the upstream probe script in substance, run by both
// pods. It prints the kernel identity, then the four isolation probes:
//
//  1. PID isolation: no host daemon (matched on comm, the executable name,
//     because this script is PID 1's cmdline) is visible in /proc.
//  2. Kernel and memory isolation: /dev/mem and /dev/kmem are not readable.
//  3. Filesystem isolation: /etc/kubernetes, /var/lib/kubelet and
//     /var/log/pods are not present.
//  4. Network isolation: no host bridge, CNI or bond interface is listed
//     (/proc/net/dev first, since gVisor's minimal sysfs does not populate
//     /sys/class/net).
//
// It exits 0 whatever it observed: the verdict belongs to
// verifyWorkloadSandboxing, and a terminal pod is what lets the check collect
// complete logs. Upstream instead sleeps to keep the pod Running and polls.
const sandboxProbeScript = `# 0. Kernel identity: a sandbox with its own kernel reports values distinct from the host.
echo "` + sandboxInfoKernelReleasePrefix + `$(uname -r 2>/dev/null || echo ` + sandboxInfoUnknown + `)"
echo "` + sandboxInfoKernelVersionPrefix + `$(cat /proc/version 2>/dev/null || echo ` + sandboxInfoUnknown + `)"
echo "` + sandboxInfoBootIDPrefix + `$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo ` + sandboxInfoUnknown + `)"

# 1. PID Isolation: check visible process count and ensure no host daemons.
pid_pass=1
pid_count=0
if [ -d /proc ]; then
  pid_count=$(ls -d /proc/[0-9]* 2>/dev/null | wc -l)
  for p in /proc/[0-9]*/comm; do
    [ -f "$p" ] || continue
    read -r comm < "$p" 2>/dev/null || continue
    case "$comm" in
      kubelet|containerd|containerd-shim*|dockerd|crio|systemd-journal) pid_pass=0 ;;
    esac
  done
fi
echo "` + sandboxInfoPIDCountPrefix + `$pid_count"
if [ "$pid_pass" -eq 1 ]; then
  echo "` + sandboxProbePIDIsolationPrefix + sandboxProbePass + `"
else
  echo "` + sandboxProbePIDIsolationPrefix + sandboxProbeFail + `"
fi

# 2. Kernel & Memory Isolation: check raw memory device access
kernel_pass=1
if head -c 1 /dev/mem 2>/dev/null; then
  kernel_pass=0
fi
if head -c 1 /dev/kmem 2>/dev/null; then
  kernel_pass=0
fi
if [ "$kernel_pass" -eq 1 ]; then
  echo "` + sandboxProbeKernelIsolationPrefix + sandboxProbePass + `"
else
  echo "` + sandboxProbeKernelIsolationPrefix + sandboxProbeFail + `"
fi

# 3. Filesystem Isolation: ensure host paths are not mounted
fs_pass=1
for host_path in /etc/kubernetes /var/lib/kubelet /var/log/pods; do
  if [ -d "$host_path" ]; then
    fs_pass=0
  fi
done
if [ "$fs_pass" -eq 1 ]; then
  echo "` + sandboxProbeFSIsolationPrefix + sandboxProbePass + `"
else
  echo "` + sandboxProbeFSIsolationPrefix + sandboxProbeFail + `"
fi

# 4. Network Isolation: ensure host interfaces/bridges are not present.
net_pass=1
iface_list=""
if [ -r /proc/net/dev ]; then
  iface_list=$(awk -F: 'NR > 2 { gsub(/ /, "", $1); printf "%s ", $1 }' /proc/net/dev 2>/dev/null)
elif [ -d /sys/class/net ]; then
  iface_list=$(ls /sys/class/net 2>/dev/null | tr '\n' ' ')
fi
for b in $iface_list; do
  case "$b" in
    docker0|cbr0|flannel*|cni0|br-*|bond*|dummy*) net_pass=0 ;;
  esac
done
echo "` + sandboxInfoInterfacesPrefix + `$iface_list"
if [ "$net_pass" -eq 1 ]; then
  echo "` + sandboxProbeNetIsolationPrefix + sandboxProbePass + `"
else
  echo "` + sandboxProbeNetIsolationPrefix + sandboxProbeFail + `"
fi

echo "` + sandboxProbeCompletedMarker + `"
exit 0`

// sandboxKernelIdentity is what a workload observes about the kernel it runs
// on. A sandbox with its own kernel (gVisor's sentry, a Kata guest VM)
// reports a different identity than a plain container sharing the host
// kernel. A field the pod could not read is "".
type sandboxKernelIdentity struct {
	Release string // uname -r
	Version string // /proc/version
	BootID  string // /proc/sys/kernel/random/boot_id
}

// sandboxProbeResults is the parsed output of one probe pod.
type sandboxProbeResults struct {
	PIDIsolation    bool
	KernelIsolation bool
	FSIsolation     bool
	NetIsolation    bool
	// Completed is true when the script printed its final marker; the probes
	// of a pod that did not complete are not judged.
	Completed  bool
	Kernel     sandboxKernelIdentity
	Interfaces string
	PIDCount   int
}

// sandboxRuntimeSelection records which RuntimeClass the check tests and why,
// for the evidence artifact. Class is nil when none qualified; Candidates
// lists every RuntimeClass consulted.
type sandboxRuntimeSelection struct {
	Class      *nodev1.RuntimeClass
	Source     string
	Reason     string
	Candidates []string
}

// CheckWorkloadSandboxing validates the CNCF workload_sandboxing requirement
// (SHOULD at v1.37, MUST at v1.38 per KAR-0020): the platform provides a
// sandboxed RuntimeClass, and a pod on it is isolated from the host kernel,
// processes, filesystem and network, as shown by the four upstream probes and
// a kernel identity distinct from an unsandboxed pod on the same node.
//
// It fails, never skips, when no sandboxed RuntimeClass exists: a missing
// runtime is a product gap the requirement counts as Not Implemented. It does
// not skip on Slinky Slurm recipes either: the probe requests no GPU, so the
// NodeSet holding every GPU does not keep it from scheduling.
func CheckWorkloadSandboxing(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// Bound all work so one bounded namespace cleanup plus margin fit inside
	// the check's deadline (gpuCheckWorkBudget); fail fast before creating
	// anything when they cannot.
	budget, err := gpuCheckWorkBudget(ctx.Ctx, workloadSandboxingWorkBudget)
	if err != nil {
		return err
	}
	workCtx, cancelWork := ctx.Timeout(budget)
	defer cancelWork()
	work := *ctx
	work.Ctx = workCtx
	ctx = &work

	selection, err := resolveSandboxedRuntimeClass(ctx.Ctx, ctx.Clientset)
	recordRawTextArtifact(ctx, "Sandboxed RuntimeClass", "kubectl get runtimeclass -o wide", selection.describe())
	if err != nil {
		return err
	}

	image := resolveSandboxProbeImage()
	recordRawTextArtifact(ctx, "Sandbox probe image", "",
		fmt.Sprintf("image: %s (override: %s)", image, sandboxProbeImageOverrideNote()))

	return runWorkloadSandboxingTest(ctx, selection.Class, image)
}

// resolveSandboxedRuntimeClass returns the RuntimeClass to test: the one
// AICR_SANDBOX_RUNTIME_CLASS names when set (accepted as given, as upstream
// accepts its flag; the kernel identity comparison is what catches a class
// that is no sandbox), otherwise the first RuntimeClass by name that a
// sandboxRuntimeKeywords entry matches in its name or handler. The selection
// is returned even on error so the caller can record what was consulted.
//
// A named class that does not exist is ErrCodeInvalidRequest (operator input
// naming nothing); a listing or read failure carries its classified code;
// nothing found is ErrCodeUnavailable with the #3615 prerequisite message.
func resolveSandboxedRuntimeClass(ctx context.Context, clientset kubernetes.Interface) (sandboxRuntimeSelection, error) {
	var selection sandboxRuntimeSelection
	if override := strings.TrimSpace(os.Getenv(sandboxRuntimeClassEnv)); override != "" {
		rc, err := clientset.NodeV1().RuntimeClasses().Get(ctx, override, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return selection, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"%s names RuntimeClass %q, which does not exist on this cluster", sandboxRuntimeClassEnv, override))
			}
			return selection, classifyK8sReadError(err, "RuntimeClass "+override)
		}
		selection.Class = rc
		selection.Source = sandboxSelectionOverride
		selection.Candidates = []string{describeRuntimeClass(rc)}
		if keyword, field := sandboxRuntimeKeyword(rc); keyword != "" {
			selection.Reason = fmt.Sprintf("%s contains the known sandboxed runtime keyword %q", field, keyword)
		} else {
			selection.Reason = "neither name nor handler matches a known sandboxed runtime keyword; accepted as named, the kernel identity comparison decides"
		}
		return selection, nil
	}

	list, err := clientset.NodeV1().RuntimeClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return selection, classifyK8sReadError(err, "RuntimeClasses")
	}
	classes := list.Items
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
	selection.Candidates = make([]string, 0, len(classes))
	for i := range classes {
		selection.Candidates = append(selection.Candidates, describeRuntimeClass(&classes[i]))
	}
	for i := range classes {
		rc := &classes[i]
		if keyword, field := sandboxRuntimeKeyword(rc); keyword != "" {
			selection.Class = rc
			selection.Source = sandboxSelectionDetected
			selection.Reason = fmt.Sprintf("%s contains the known sandboxed runtime keyword %q", field, keyword)
			return selection, nil
		}
	}
	return selection, errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
		"%s: %d RuntimeClass(es) exist and none names or handles a known sandboxed runtime (%s); the platform does not implement workload_sandboxing until one is provided, or set %s to name the class to test",
		sandboxMissingPrerequisite, len(classes), strings.Join(sandboxRuntimeKeywords, ", "), sandboxRuntimeClassEnv))
}

// sandboxRuntimeKeyword returns the first sandboxRuntimeKeywords entry the
// RuntimeClass's lower-cased name or handler contains, and which field
// matched ("name" or "handler"); "" when none does.
func sandboxRuntimeKeyword(rc *nodev1.RuntimeClass) (keyword, field string) {
	name := strings.ToLower(rc.Name)
	handler := strings.ToLower(rc.Handler)
	for _, kw := range sandboxRuntimeKeywords {
		switch {
		case strings.Contains(name, kw):
			return kw, sandboxMatchFieldName
		case strings.Contains(handler, kw):
			return kw, sandboxMatchFieldHandler
		}
	}
	return "", ""
}

func describeRuntimeClass(rc *nodev1.RuntimeClass) string {
	return fmt.Sprintf("%s (handler: %s)", rc.Name, valueOrUnknown(rc.Handler))
}

func (s *sandboxRuntimeSelection) describe() string {
	var b strings.Builder
	if s.Class == nil {
		b.WriteString("Selected:  <none>\n")
	} else {
		fmt.Fprintf(&b, "Selected:  %s\nHandler:   %s\nSource:    %s\nReason:    %s\n",
			s.Class.Name, valueOrUnknown(s.Class.Handler), s.Source, s.Reason)
	}
	fmt.Fprintf(&b, "Detection: name or handler contains one of %s\n", strings.Join(sandboxRuntimeKeywords, ", "))
	if len(s.Candidates) == 0 {
		b.WriteString("RuntimeClasses consulted: <none>")
	} else {
		b.WriteString("RuntimeClasses consulted:\n  " + strings.Join(s.Candidates, "\n  "))
	}
	return b.String()
}

// resolveSandboxProbeImage returns the probe image: the
// AICR_SANDBOX_PROBE_IMAGE reference verbatim when set, else the default.
// Neither validator image override (AICR_VALIDATOR_IMAGE_REGISTRY,
// AICR_VALIDATOR_IMAGE_TAG) applies; see sandboxProbeImage.
func resolveSandboxProbeImage() string {
	if override := strings.TrimSpace(os.Getenv(sandboxProbeImageEnv)); override != "" {
		return override
	}
	return sandboxProbeImage
}

func sandboxProbeImageOverrideNote() string {
	if strings.TrimSpace(os.Getenv(sandboxProbeImageEnv)) != "" {
		return sandboxProbeImageEnv
	}
	return valueNone
}

// runWorkloadSandboxingTest creates the per-run namespace, runs the sandboxed
// probe pod, then the unsandboxed control pod pinned to the node the sandboxed
// pod landed on, and judges both outputs. The return is NAMED so the deferred
// cleanup can fail an otherwise-passing check when cleanup terminally fails; a
// primary error is never overwritten.
func runWorkloadSandboxingTest(ctx *validators.Context, rc *nodev1.RuntimeClass, image string) (err error) {
	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}
	// Register cleanup BEFORE the first Create: an ambiguously created pod
	// would otherwise stay behind (see cleanupGPUTestNamespace).
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

	// The sandboxed pod goes through the scheduler so the RuntimeClass's
	// scheduling constraints (nodeSelector, tolerations) apply. It carries the
	// context tolerations, and with none set this package's tolerate-all
	// default, which upstream does not grant its sandboxed pod: wanted so a
	// sandbox runtime on a tainted GPU pool is reachable, at the cost that a
	// cordoned or NotReady node inside the RuntimeClass's node set can absorb
	// the pod and the check then waits the full pod timeout before failing.
	sandboxedPod, sandboxedLogs, err := runSandboxProbePod(ctx, run.namespace,
		buildSandboxProbePod(run, run.sandboxProbePodName, image, rc.Name, "", ctx.Tolerations),
		"sandboxed probe pod")
	if err != nil {
		return err
	}
	node := sandboxedPod.Spec.NodeName
	if node == "" {
		return errors.New(errors.ErrCodeInternal, "sandboxed probe pod reached a terminal phase without a node name")
	}

	// The control pod is pinned with spec.nodeName, bypassing the scheduler so
	// NoSchedule taints on a dedicated sandbox pool do not block it, and
	// tolerates everything so a NoExecute taint there does not evict it: it is
	// CPU-only and short-lived, and it must land beside the sandboxed pod,
	// whose pool taints only the RuntimeClass's own scheduling tolerations
	// carry (upstream does the same).
	controlPod, controlLogs, err := runSandboxProbePod(ctx, run.namespace,
		buildSandboxProbePod(run, run.sandboxControlPodName, image, "", node, nil),
		"unsandboxed control pod")
	if err != nil {
		return err
	}

	sandboxed := parseSandboxProbeLogs(sandboxedLogs)
	control := parseSandboxProbeLogs(controlLogs)
	verdictErr := verifyWorkloadSandboxing(rc, node, sandboxed, control, sandboxedPod, controlPod)
	recordRawTextArtifact(ctx, "Workload sandboxing verdict", "",
		describeWorkloadSandboxingVerdict(rc, node, sandboxed, control, verdictErr))
	return verdictErr
}

// runSandboxProbePod creates pod, waits for it to reach a terminal phase and
// reads its logs, recording the status and logs as artifacts (logs BEFORE any
// verdict: they are the primary diagnostic). what names the pod in artifacts
// and errors.
func runSandboxProbePod(ctx *validators.Context, namespace string, pod *corev1.Pod, what string) (*corev1.Pod, string, error) {
	if err := createPodWhenSAReady(ctx.Ctx, ctx.Clientset, namespace, pod); err != nil {
		return nil, "", errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create "+what)
	}
	terminal, err := waitForTerminalPod(ctx.Ctx, ctx.Clientset, namespace, pod.Name, what)
	if err != nil {
		return nil, "", err
	}
	recordRawTextArtifact(ctx, capitalizeFirst(what)+" status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", terminal.Name, terminal.Namespace),
		fmt.Sprintf("Name:          %s/%s\nPhase:         %s\nReason:        %s\nMessage:       %s\nNode:          %s\nRuntimeClass:  %s\nImage:         %s",
			terminal.Namespace, terminal.Name, terminal.Status.Phase, valueOrNone(terminal.Status.Reason), valueOrNone(terminal.Status.Message),
			valueOrUnknown(terminal.Spec.NodeName), runtimeClassOrNone(terminal), pod.Spec.Containers[0].Image))

	logs, logErr := readSandboxProbeLogs(ctx.Ctx, ctx.Clientset, terminal)
	if logErr != nil {
		// A pod the kubelet rejected at admission (a spec.nodeName pin lands
		// without the scheduler's fit check: OutOfcpu, OutOfmemory, an
		// unsatisfied node affinity) is Failed with no container to read
		// logs from; the pod-level reason is the diagnostic then.
		if terminal.Status.Reason != "" || terminal.Status.Message != "" {
			logErr = wrapWithCode(logErr, errors.ErrCodeInternal, fmt.Sprintf(
				"%s %s (reason=%s, message=%s)", what, strings.ToLower(string(terminal.Status.Phase)),
				valueOrNone(terminal.Status.Reason), valueOrNone(terminal.Status.Message)))
		}
		recordRawTextArtifact(ctx, capitalizeFirst(what)+" logs",
			fmt.Sprintf("kubectl logs %s -n %s", terminal.Name, terminal.Namespace), fmt.Sprintf("failed to read logs: %v", logErr))
		return nil, "", logErr
	}
	recordRawTextArtifact(ctx, capitalizeFirst(what)+" logs",
		fmt.Sprintf("kubectl logs %s -n %s", terminal.Name, terminal.Namespace), logs)
	return terminal, logs, nil
}

// buildSandboxProbePod returns a probe pod: one container from image running
// the probe script, with the upstream CPU-only requests and limits.
// runtimeClassName selects the sandbox ("" for a plain, unsandboxed pod);
// nodeName, when set, pins the pod. tolerations, when non-nil, replace the
// tolerate-all default.
func buildSandboxProbePod(run *gpuTestRun, name, image, runtimeClassName, nodeName string, tolerations []corev1.Toleration) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	var rcName *string
	if runtimeClassName != "" {
		rcName = helper.StrPtr(runtimeClassName)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: run.namespace,
			Labels:    map[string]string{sandboxProbeTestLabel: sandboxProbeTestValue},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: rcName,
			NodeName:         nodeName,
			Tolerations:      tolerations,
			ImagePullSecrets: run.imagePullSecretRefs(),
			RestartPolicy:    corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    sandboxProbeContainer,
				Image:   image,
				Command: []string{posixShell, "-c", sandboxProbeScript},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(sandboxProbeCPURequest),
						corev1.ResourceMemory: resource.MustParse(sandboxProbeMemoryRequest),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(sandboxProbeCPULimit),
						corev1.ResourceMemory: resource.MustParse(sandboxProbeMemoryLimit),
					},
				},
			}},
		},
	}
}

// readSandboxProbeLogs returns the probe container's logs.
func readSandboxProbeLogs(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod) (string, error) {
	logBytes, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(
		pod.Name, &corev1.PodLogOptions{Container: sandboxProbeContainer}).DoRaw(ctx)
	if err != nil {
		return "", classifyK8sReadError(err, fmt.Sprintf("pod %s/%s logs", pod.Namespace, pod.Name))
	}
	return string(logBytes), nil
}

// parseSandboxProbeLogs reads the SANDBOX_INFO and SANDBOX_PROBE lines
// (upstream parseProbeLogs). A probe is passed only by its explicit PASS
// line; anything else, including a missing line, is a failure.
func parseSandboxProbeLogs(logs string) sandboxProbeResults {
	var res sandboxProbeResults
	for raw := range strings.SplitSeq(logs, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == sandboxProbePIDIsolationPrefix+sandboxProbePass:
			res.PIDIsolation = true
		case line == sandboxProbeKernelIsolationPrefix+sandboxProbePass:
			res.KernelIsolation = true
		case line == sandboxProbeFSIsolationPrefix+sandboxProbePass:
			res.FSIsolation = true
		case line == sandboxProbeNetIsolationPrefix+sandboxProbePass:
			res.NetIsolation = true
		case line == sandboxProbeCompletedMarker:
			res.Completed = true
		case strings.HasPrefix(line, sandboxInfoKernelReleasePrefix):
			res.Kernel.Release = sandboxInfoValue(line, sandboxInfoKernelReleasePrefix)
		case strings.HasPrefix(line, sandboxInfoKernelVersionPrefix):
			res.Kernel.Version = sandboxInfoValue(line, sandboxInfoKernelVersionPrefix)
		case strings.HasPrefix(line, sandboxInfoBootIDPrefix):
			res.Kernel.BootID = sandboxInfoValue(line, sandboxInfoBootIDPrefix)
		case strings.HasPrefix(line, sandboxInfoInterfacesPrefix):
			res.Interfaces = sandboxInfoValue(line, sandboxInfoInterfacesPrefix)
		case strings.HasPrefix(line, sandboxInfoPIDCountPrefix):
			if count, err := strconv.Atoi(sandboxInfoValue(line, sandboxInfoPIDCountPrefix)); err == nil {
				res.PIDCount = count
			}
		}
	}
	return res
}

// sandboxInfoValue extracts an info value; the script prints "unknown" for
// values it could not read, which is normalized to "" (no information).
func sandboxInfoValue(line, prefix string) string {
	v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if v == sandboxInfoUnknown {
		return ""
	}
	return v
}

// kernelIdentitiesDiffer reports whether the sandboxed pod observed a kernel
// identity distinct from the unsandboxed control pod, and which fields
// differed. A field that either side could not read is ignored, so a runtime
// that does not expose boot_id is judged on release and version alone
// (upstream kernelIdentitiesDiffer).
func kernelIdentitiesDiffer(sandboxed, control sandboxKernelIdentity) (bool, []string) {
	var differing []string
	compare := func(field, a, b string) {
		if a == "" || b == "" {
			return
		}
		if a != b {
			differing = append(differing, field)
		}
	}
	compare("release", sandboxed.Release, control.Release)
	compare("version", sandboxed.Version, control.Version)
	compare("boot_id", sandboxed.BootID, control.BootID)
	return len(differing) > 0, differing
}

// verifyWorkloadSandboxing is the verdict: both probes ran to completion, the
// sandboxed pod passed all four isolation probes, and its kernel identity
// differs from the control pod's. Every failing probe is named (upstream
// reports them as four subtests); every failure is a disproven capability
// (ErrCodeInternal), the code the other probe checks use for a failed
// subtest.
func verifyWorkloadSandboxing(rc *nodev1.RuntimeClass, node string, sandboxed, control sandboxProbeResults, sandboxedPod, controlPod *corev1.Pod) error {
	if !sandboxed.Completed {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"sandboxed probe pod did not print %q (pod phase %s, exit code %d); the isolation probes cannot be judged from partial output",
			sandboxProbeCompletedMarker, sandboxedPod.Status.Phase, podExitCode(sandboxedPod)))
	}
	if !control.Completed {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"unsandboxed control pod did not print %q (pod phase %s, exit code %d); the kernel identity comparison cannot be judged from partial output",
			sandboxProbeCompletedMarker, controlPod.Status.Phase, podExitCode(controlPod)))
	}

	var failed []string
	if !sandboxed.PIDIsolation {
		failed = append(failed, fmt.Sprintf(
			"process isolation: a host daemon (kubelet, containerd, dockerd, crio or systemd-journal) is visible in the sandboxed pod's /proc (%d PIDs visible)", sandboxed.PIDCount))
	}
	if !sandboxed.KernelIsolation {
		failed = append(failed, "kernel and memory isolation: /dev/mem or /dev/kmem is readable in the sandboxed pod")
	}
	if differs, _ := kernelIdentitiesDiffer(sandboxed.Kernel, control.Kernel); !differs {
		failed = append(failed, fmt.Sprintf(
			"kernel identity: the sandboxed pod observes the same kernel identity as the unsandboxed control pod on node %s (release %q, version %q, boot_id %q; unreadable fields are not compared), so handler %q provides no kernel boundary; a sandbox such as gVisor or Kata exposes a guest kernel distinct from the host",
			node, sandboxed.Kernel.Release, sandboxed.Kernel.Version, sandboxed.Kernel.BootID, rc.Handler))
	}
	if !sandboxed.FSIsolation {
		failed = append(failed, "filesystem isolation: a host path (/etc/kubernetes, /var/lib/kubelet or /var/log/pods) is present in the sandboxed pod")
	}
	if !sandboxed.NetIsolation {
		failed = append(failed, fmt.Sprintf(
			"network isolation: a host bridge, CNI or bond interface is visible in the sandboxed pod (interfaces: %s)", valueOrUnknown(sandboxed.Interfaces)))
	}
	if len(failed) > 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"RuntimeClass %q (handler %q) failed %d isolation probe(s): %s", rc.Name, rc.Handler, len(failed), strings.Join(failed, "; ")))
	}
	return nil
}

func describeWorkloadSandboxingVerdict(rc *nodev1.RuntimeClass, node string, sandboxed, control sandboxProbeResults, verdictErr error) string {
	differs, fields := kernelIdentitiesDiffer(sandboxed.Kernel, control.Kernel)
	identity := "same as the control pod"
	if differs {
		identity = "differs from the control pod in " + strings.Join(fields, ", ")
	}
	verdict := sandboxProbePass
	if verdictErr != nil {
		verdict = sandboxProbeFail + ": " + verdictErr.Error()
	}
	return fmt.Sprintf(
		"runtimeClass:              %s (handler %s) on node %s\nsandboxed probe completed: %t\ncontrol probe completed:   %t\nprocess isolation:         %s (%d PIDs visible)\nkernel/memory isolation:   %s\nfilesystem isolation:      %s\nnetwork isolation:         %s (interfaces: %s)\nsandboxed kernel:          %s\ncontrol kernel:            %s\nkernel identity:           %s\nverdict:                   %s",
		rc.Name, rc.Handler, node, sandboxed.Completed, control.Completed,
		passFail(sandboxed.PIDIsolation), sandboxed.PIDCount,
		passFail(sandboxed.KernelIsolation), passFail(sandboxed.FSIsolation),
		passFail(sandboxed.NetIsolation), valueOrUnknown(sandboxed.Interfaces),
		sandboxed.Kernel.describe(), control.Kernel.describe(), identity, verdict)
}

func (k sandboxKernelIdentity) describe() string {
	return fmt.Sprintf("release=%s version=%s boot_id=%s",
		valueOrUnknown(k.Release), valueOrUnknown(k.Version), valueOrUnknown(k.BootID))
}

func passFail(ok bool) string {
	if ok {
		return sandboxProbePass
	}
	return sandboxProbeFail
}

func runtimeClassOrNone(pod *corev1.Pod) string {
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName == "" {
		return valueNone
	}
	return *pod.Spec.RuntimeClassName
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

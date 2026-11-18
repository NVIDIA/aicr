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
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"aicr.run/pkg/constraints"
	"aicr.run/pkg/errors"
	"aicr.run/validators"
	"aicr.run/validators/internal/allocmode"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// snapshotComponent is the recipe componentRef for Snapshot
// (github.com/ai-dynamo/snapshot, GPU checkpoint/restore). It is not the
// `aicr snapshot` command, which captures cluster state.
const snapshotComponent = "snapshot"

const (
	// The chart installs into this namespace unless the recipe says otherwise,
	// and creates this claim unless storage.pvc.create is false.
	snapshotDefaultNamespace = "snapshot"
	snapshotClaimName        = "snapshot-pvc"

	// The operator Deployment mounts the checkpoint claim as this volume, whatever
	// the claim is called, so the claim the install actually uses can be read from
	// it (a recipe may set storage.pvc.name and storage.pvc.create=false).
	snapshotOperatorDeployment = "snapshot-operator"
	snapshotCheckpointVolume   = "checkpoints"

	// Snapshot's documented prerequisites (the project README, "Prerequisites",
	// as of v0.2.0): Linux kernel 5.13 or newer, containerd or CRI-O, GPU
	// Operator 26.3 or newer with CUDA driver 580 or newer and MIG disabled, a
	// ReadWriteMany storage class, and a namespace that allows a privileged
	// DaemonSet. They follow the documentation, so re-read them when the Snapshot
	// version AICR pins moves.
	snapshotKernelFloor      = ">= 5.13"
	snapshotKernelMajor      = 5
	snapshotKernelMinor      = 13
	snapshotGPUOperatorFloor = ">= v26.3.0"
	snapshotDriverMajorFloor = 580

	// Labels the GPU Operator's feature discovery puts on GPU nodes.
	snapshotGPUPresentLabel = "nvidia.com/gpu.present"
	// The driver major version: the current GPU Feature Discovery label, then the
	// deprecated one older GPU Operator releases emit (see the conformance
	// driver-runtime-management check, which reads both).
	snapshotDriverMajorLabel       = "nvidia.com/cuda.driver-version.major"
	snapshotLegacyDriverMajorLabel = "nvidia.com/cuda.driver.major"
	snapshotMIGConfigLabel         = "nvidia.com/mig.config"
	snapshotGPUProductLabel        = "nvidia.com/gpu.product"
	snapshotMIGDisabled            = "all-disabled"
	// Under the mixed MIG strategy GPU Feature Discovery labels each profile's
	// resource, for example nvidia.com/mig-1g.10gb.count=7, and leaves the GPU
	// product label as it is.
	snapshotMIGResourcePrefix = "nvidia.com/mig-"
	snapshotMIGCountSuffix    = ".count"

	snapshotPodSecurityLabel = "pod-security.kubernetes.io/enforce"
	snapshotPrivilegedLevel  = "privileged"
	// The name prefix of the pod the namespace check asks the API server to admit
	// as a dry run, so it never exists.
	snapshotProbePodPrefix = "aicr-snapshot-admission-probe-"
	snapshotProbeImage     = "registry.k8s.io/pause:3.10"

	// The agent DaemonSet is named "<release>-agent" by the chart.
	snapshotAgentDaemonSetSuffix = "-agent"

	// The node label the operator's nodeSelector uses to pin it to one node.
	snapshotHostnameLabel = "kubernetes.io/hostname"

	// The name of the requirement that reports whether the GPU nodes could be found.
	snapshotGPUNodesName = "gpu-nodes"
)

// snapshotAgentHostPaths are the host directories Snapshot's node agent mounts
// (the chart's daemonset.yaml at v0.2.0). The admission probe mounts the same
// shapes, because a policy can allow a privileged container and still refuse
// hostPath volumes.
var snapshotAgentHostPaths = []string{
	"/proc",
	"/sys/fs/cgroup",
	"/var/lib/kubelet/pods",
	"/var/lib/kubelet/pod-resources",
	"/var/lib/kubelet/seccomp",
	"/run/containerd",
	"/var/lib/containerd",
}

// snapshotRequirement is the outcome of one prerequisite. code is set only when
// the requirement could not be read for a reason worth naming (a denied read, a
// timeout); every other failure is a cluster that does not meet it.
type snapshotRequirement struct {
	name   string
	ok     bool
	detail string
	code   errors.ErrorCode
}

// snapshotReadCode names the error code for a failed read of a prerequisite: a
// denied read is UNAUTHORIZED (the validator cannot see it, which is an RBAC
// problem, not a cluster fault), a timeout is TIMEOUT, anything else INTERNAL.
func snapshotReadCode(err error) errors.ErrorCode {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errors.ErrCodeUnauthorized
	case allocmode.IsK8sTimeoutErr(err):
		return errors.ErrCodeTimeout
	default:
		return errors.ErrCodeInternal
	}
}

// snapshotFailureCode is the code for the aggregate failure: a read that was
// denied names RBAC and wins, then a timeout, otherwise INTERNAL.
func snapshotFailureCode(failed []snapshotRequirement) errors.ErrorCode {
	code := errors.ErrCodeInternal
	for _, r := range failed {
		if r.code == errors.ErrCodeUnauthorized {
			return errors.ErrCodeUnauthorized
		}
		if r.code == errors.ErrCodeTimeout {
			code = errors.ErrCodeTimeout
		}
	}
	return code
}

// checkSnapshotRequirements verifies that the cluster meets Snapshot's
// prerequisites. It checks every one and reports all that fail, so an operator
// fixing a cluster sees the whole list in one run.
//
// It skips only when the recipe does not declare snapshot, and fails closed once
// it does (#2122): a declared component whose prerequisites cannot be read is
// not a cluster that is ready for it. A claim or namespace that does not exist
// yet is not a failure, since the bundle may not have been applied or may name a
// different claim; the check fails only on evidence that the store cannot work.
func checkSnapshotRequirements(ctx *validators.Context) error {
	if !validators.RecipeDeclares(ctx, snapshotComponent) {
		return validators.SkipWithReason(validators.SkipReasonComponentNotInRecipe,
			snapshotComponent+" not declared in recipe, so its requirements are not checked")
	}

	var results []snapshotRequirement
	var gpuNodes []corev1.Node
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: snapshotGPUPresentLabel + "=true",
	})
	nodesKnown := err == nil
	if nodesKnown {
		gpuNodes = nodes.Items
		results = append(results, snapshotGPUNodesRequirement(gpuNodes))
		if len(gpuNodes) > 0 {
			results = append(results,
				snapshotKernelRequirement(gpuNodes),
				snapshotRuntimeRequirement(gpuNodes),
				snapshotDriverRequirement(gpuNodes),
				snapshotMIGRequirement(gpuNodes),
			)
		}
	} else {
		// The node-derived requirements cannot be evaluated, but the GPU Operator,
		// the claim and the namespace do not depend on them, so they still report.
		results = append(results, snapshotRequirement{name: snapshotGPUNodesName, code: snapshotReadCode(err),
			detail: "could not list GPU nodes, so the node requirements (kernel, container-runtime, driver, mig) were not evaluated: " + err.Error()})
	}
	results = append(results,
		snapshotGPUOperatorRequirement(ctx),
		snapshotStoreRequirement(ctx, snapshotNamespace(ctx), gpuNodes, nodesKnown),
		snapshotNamespaceRequirement(ctx, snapshotNamespace(ctx)),
	)

	var failed []snapshotRequirement
	var details []string
	for _, r := range results {
		status := "PASS"
		if !r.ok {
			status = "FAIL"
			failed = append(failed, r)
			details = append(details, r.name+": "+r.detail)
		}
		fmt.Printf("%s %s: %s\n", status, r.name, r.detail)
	}
	if len(failed) > 0 {
		return errors.New(snapshotFailureCode(failed),
			"snapshot requirements not met: "+strings.Join(details, "; "))
	}
	return nil
}

// snapshotNamespace returns the namespace the recipe installs snapshot into.
func snapshotNamespace(ctx *validators.Context) string {
	if ctx.ValidationInput != nil {
		for _, ref := range ctx.ValidationInput.ComponentRefs {
			if ref.Name == snapshotComponent && ref.Namespace != "" {
				return ref.Namespace
			}
		}
	}
	return snapshotDefaultNamespace
}

func snapshotGPUNodesRequirement(nodes []corev1.Node) snapshotRequirement {
	if len(nodes) == 0 {
		return snapshotRequirement{name: snapshotGPUNodesName, detail: "no node carries " + snapshotGPUPresentLabel +
			"=true; Snapshot's agent runs only on GPU nodes"}
	}
	return snapshotRequirement{name: snapshotGPUNodesName, ok: true, detail: fmt.Sprintf("%d GPU node(s)", len(nodes))}
}

func snapshotKernelRequirement(nodes []corev1.Node) snapshotRequirement {
	var bad []string
	for _, n := range nodes {
		if kernel := n.Status.NodeInfo.KernelVersion; !snapshotKernelSupported(kernel) {
			bad = append(bad, fmt.Sprintf("%s runs %q", n.Name, kernel))
		}
	}
	if len(bad) > 0 {
		return snapshotRequirement{name: "kernel", detail: strings.Join(bad, ", ") + "; Snapshot needs " + snapshotKernelFloor}
	}
	return snapshotRequirement{name: "kernel", ok: true, detail: "every GPU node meets " + snapshotKernelFloor}
}

func snapshotRuntimeRequirement(nodes []corev1.Node) snapshotRequirement {
	var bad []string
	for _, n := range nodes {
		if rt := n.Status.NodeInfo.ContainerRuntimeVersion; !snapshotRuntimeSupported(rt) {
			bad = append(bad, fmt.Sprintf("%s runs %q", n.Name, rt))
		}
	}
	if len(bad) > 0 {
		return snapshotRequirement{name: "container-runtime", detail: strings.Join(bad, ", ") + "; Snapshot needs containerd or CRI-O"}
	}
	return snapshotRequirement{name: "container-runtime", ok: true, detail: "every GPU node runs containerd or CRI-O"}
}

func snapshotDriverRequirement(nodes []corev1.Node) snapshotRequirement {
	var bad []string
	for _, n := range nodes {
		raw, found := n.Labels[snapshotDriverMajorLabel]
		if !found {
			raw, found = n.Labels[snapshotLegacyDriverMajorLabel]
		}
		major, err := strconv.Atoi(raw)
		switch {
		case !found || err != nil:
			bad = append(bad, fmt.Sprintf("%s does not report %s", n.Name, snapshotDriverMajorLabel))
		case major < snapshotDriverMajorFloor:
			bad = append(bad, fmt.Sprintf("%s runs driver %d", n.Name, major))
		}
	}
	if len(bad) > 0 {
		return snapshotRequirement{name: "driver", detail: strings.Join(bad, ", ") +
			fmt.Sprintf("; Snapshot needs driver %d or newer", snapshotDriverMajorFloor)}
	}
	return snapshotRequirement{name: "driver", ok: true, detail: fmt.Sprintf("every GPU node reports driver %d or newer", snapshotDriverMajorFloor)}
}

func snapshotMIGRequirement(nodes []corev1.Node) snapshotRequirement {
	var bad []string
	for _, n := range nodes {
		if snapshotMIGEnabled(n.Labels) {
			bad = append(bad, n.Name)
		}
	}
	if len(bad) > 0 {
		return snapshotRequirement{name: "mig", detail: "MIG is enabled on " + strings.Join(bad, ", ") + "; Snapshot needs MIG disabled"}
	}
	return snapshotRequirement{name: "mig", ok: true, detail: "no GPU node has MIG enabled"}
}

func snapshotGPUOperatorRequirement(ctx *validators.Context) snapshotRequirement {
	version, pinnedImage, err := getGPUOperatorVersion(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return snapshotRequirement{name: gpuOperatorComponent, detail: "could not detect the GPU Operator version: " + err.Error(), code: snapshotReadCode(err)}
	}
	// A digest is not a version. When the operator image is pinned by digest
	// alone and the Deployment carries no version label or annotation, the floor
	// cannot be evaluated, so the requirement fails closed, as gpu-operator-version
	// does for a declared constraint (#1995).
	if version == "" {
		return snapshotRequirement{name: gpuOperatorComponent, detail: fmt.Sprintf(
			"could not read the GPU Operator version: image %s is pinned by digest only; reference it as repo:tag@sha256:<digest>, or set the app.kubernetes.io/version label to the deployed version",
			pinnedImage)}
	}
	parsed, err := constraints.ParseConstraintExpression(snapshotGPUOperatorFloor)
	if err != nil {
		return snapshotRequirement{name: gpuOperatorComponent, detail: "invalid version floor " + snapshotGPUOperatorFloor + ": " + err.Error()}
	}
	ok, err := parsed.Evaluate(version)
	if err != nil {
		return snapshotRequirement{name: gpuOperatorComponent, detail: fmt.Sprintf("could not compare GPU Operator %s with %s: %v", version, snapshotGPUOperatorFloor, err)}
	}
	if !ok {
		return snapshotRequirement{name: gpuOperatorComponent, detail: fmt.Sprintf("GPU Operator %s; Snapshot needs %s", version, snapshotGPUOperatorFloor)}
	}
	return snapshotRequirement{name: gpuOperatorComponent, ok: true, detail: fmt.Sprintf("GPU Operator %s meets %s", version, snapshotGPUOperatorFloor)}
}

// snapshotClaimInUse returns the name of the checkpoint claim and the operator
// Deployment, which is nil when the install is not there yet. When the Deployment
// exists, the claim is the one it mounts, which is how a recipe that sets
// storage.pvc.name is verified; before the install the chart's default name is
// the best guess, but only when the Deployment is clean-NotFound. Any other read
// error (a denied read, a timeout) is returned, because the claim in use is then
// unknown and the default claim must not stand in for it.
func snapshotClaimInUse(ctx *validators.Context, namespace string) (name string, deploy *appsv1.Deployment, err error) {
	deploy, err = ctx.Clientset.AppsV1().Deployments(namespace).Get(ctx.Ctx, snapshotOperatorDeployment, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return snapshotClaimName, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == snapshotCheckpointVolume && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
			return v.PersistentVolumeClaim.ClaimName, deploy, nil
		}
	}
	return snapshotClaimName, deploy, nil
}

// snapshotSharedNodeProblem decides whether a ReadWriteOnce claim can serve both
// of Snapshot's consumers. The agent mounts the claim on each GPU node, and the
// operator Deployment mounts it too, on whichever node it is scheduled. A
// ReadWriteOnce claim attaches to one node, so the exception holds only when
// there is a single GPU node and the operator runs on that node: its pods are
// there, or its nodeSelector pins it there, or the cluster has no other node. It
// returns "" when the exception holds, otherwise the reason it does not; code is
// set when the reason is a failed read.
func snapshotSharedNodeProblem(ctx *validators.Context, namespace string, deploy *appsv1.Deployment, gpuNodes []corev1.Node, nodesKnown bool) (problem string, code errors.ErrorCode) {
	if !nodesKnown {
		return "the GPU nodes could not be listed, so whether the agent and the operator share a node is unknown", ""
	}
	if len(gpuNodes) == 0 {
		return "", "" // gpu-nodes already fails; there is no agent to share with.
	}
	if len(gpuNodes) > 1 {
		return fmt.Sprintf("the cluster has %d GPU nodes, whose agents all mount it", len(gpuNodes)), ""
	}
	// The operator's nodeSelector names a hostname label value, which Kubernetes
	// does not require to equal the node name, so compare with the node's label.
	gpuNode := gpuNodes[0].Name
	gpuHost := gpuNodes[0].Labels[snapshotHostnameLabel]
	if gpuHost == "" {
		gpuHost = gpuNode
	}

	// The current template decides where a replacement pod goes. During a rolling
	// update an old pod may still run on the GPU node while the template already
	// pins its replacement elsewhere, so a conflicting pin fails before any
	// observed placement is accepted.
	pinned := ""
	if deploy != nil {
		pinned = deploy.Spec.Template.Spec.NodeSelector[snapshotHostnameLabel]
	}
	if pinned != "" && pinned != gpuHost {
		return fmt.Sprintf("the snapshot operator Deployment pins its pods to %s=%s, not the GPU node (%s), and a ReadWriteOnce claim attaches to one node",
			snapshotHostnameLabel, pinned, gpuHost), ""
	}

	if deploy != nil && deploy.Spec.Selector != nil {
		selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
		if err == nil {
			pods, listErr := ctx.Clientset.CoreV1().Pods(namespace).List(ctx.Ctx, metav1.ListOptions{LabelSelector: selector.String()})
			if listErr != nil {
				return "could not list the snapshot operator pods to see where they run: " + listErr.Error(), snapshotReadCode(listErr)
			}
			placed := false
			for i := range pods.Items {
				node := pods.Items[i].Spec.NodeName
				if node == "" {
					continue
				}
				if node != gpuNode {
					return fmt.Sprintf("the snapshot operator pod %s runs on node %s, not the GPU node %s, and a ReadWriteOnce claim attaches to one node",
						pods.Items[i].Name, node, gpuNode), ""
				}
				placed = true
			}
			if placed {
				return "", ""
			}
		}
	}

	if pinned == gpuHost {
		return "", ""
	}
	all, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return "could not list the nodes to see where the snapshot operator can run: " + err.Error(), snapshotReadCode(err)
	}
	if len(all.Items) <= 1 {
		return "", ""
	}
	return fmt.Sprintf("the snapshot operator also mounts the claim and can be scheduled on any of the %d nodes; pin it to the GPU node (operator.nodeSelector %s=%s) or use a ReadWriteMany claim",
		len(all.Items), snapshotHostnameLabel, gpuHost), ""
}

// snapshotStoreRequirement checks the checkpoint claim. It must be bound and a
// filesystem claim. A ReadWriteMany claim is what a multi-node cluster needs; a
// ReadWriteOnce claim works only while the agent and the operator, which both
// mount it, share the one GPU node (see snapshotSharedNodeProblem). Before the
// install a claim that does not exist is left unverified rather than failed; once
// the operator is deployed, the claim it mounts must exist. gpuNodes is only
// meaningful when nodesKnown is true.
func snapshotStoreRequirement(ctx *validators.Context, namespace string, gpuNodes []corev1.Node, nodesKnown bool) snapshotRequirement {
	name := "checkpoint-store"
	claimName, deploy, err := snapshotClaimInUse(ctx, namespace)
	installed := deploy != nil
	if err != nil {
		return snapshotRequirement{name: name, code: snapshotReadCode(err),
			detail: fmt.Sprintf("could not read the %s Deployment in %s, so the checkpoint claim it mounts is unknown: %v",
				snapshotOperatorDeployment, namespace, err)}
	}
	claim, err := ctx.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx.Ctx, claimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if installed {
			return snapshotRequirement{name: name, detail: fmt.Sprintf(
				"claim %s/%s, which the snapshot operator mounts, does not exist", namespace, claimName)}
		}
		return snapshotRequirement{name: name, ok: true,
			detail: fmt.Sprintf("claim %s/%s not found, so the checkpoint store was not verified", namespace, claimName)}
	}
	if err != nil {
		return snapshotRequirement{name: name, code: snapshotReadCode(err),
			detail: fmt.Sprintf("could not read claim %s/%s: %v", namespace, claimName, err)}
	}
	if claim.Status.Phase != corev1.ClaimBound {
		return snapshotRequirement{name: name, detail: fmt.Sprintf(
			"claim %s/%s is %s; Snapshot's claim is ReadWriteMany and needs a storage class that supports it",
			namespace, claimName, claim.Status.Phase)}
	}
	// Snapshot mounts the claim as a filesystem (volumeMounts); a raw block claim
	// cannot serve it whatever its access mode.
	if claim.Spec.VolumeMode != nil && *claim.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return snapshotRequirement{name: name, detail: fmt.Sprintf(
			"claim %s/%s has volumeMode Block; Snapshot mounts the checkpoint claim as a filesystem", namespace, claimName)}
	}
	readWriteOnce := false
	for _, mode := range claim.Spec.AccessModes {
		if mode == corev1.ReadWriteMany {
			return snapshotRequirement{name: name, ok: true, detail: fmt.Sprintf("claim %s/%s is bound and ReadWriteMany", namespace, claimName)}
		}
		if mode == corev1.ReadWriteOnce {
			readWriteOnce = true
		}
	}
	// The single-node exception is for a claim that one node can write, which is
	// ReadWriteOnce, and only while the agent and the operator share that node. A
	// ReadOnlyMany or ReadWriteOncePod claim is not that.
	if readWriteOnce {
		problem, code := snapshotSharedNodeProblem(ctx, namespace, deploy, gpuNodes, nodesKnown)
		if problem == "" {
			return snapshotRequirement{name: name, ok: true, detail: fmt.Sprintf(
				"claim %s/%s is bound and ReadWriteOnce; accepted because the agent and the operator share the only GPU node", namespace, claimName)}
		}
		return snapshotRequirement{name: name, code: code, detail: fmt.Sprintf(
			"claim %s/%s is ReadWriteOnce, which works only when the agent and the operator share one node, and %s", namespace, claimName, problem)}
	}
	return snapshotRequirement{name: name, detail: fmt.Sprintf(
		"claim %s/%s is bound with access modes %v; Snapshot writes checkpoints to it, so it needs ReadWriteMany (ReadWriteOnce is accepted when the agent and the operator share the only GPU node)",
		namespace, claimName, claim.Spec.AccessModes)}
}

// snapshotNamespaceRequirement checks that the namespace admits Snapshot's
// privileged node agent. A namespace that does not exist yet is not a failure:
// the install creates it. A namespace that enforces a level other than
// privileged fails. A namespace with no enforce label is not assumed to allow
// it, because the cluster's Pod Security default applies in that case, so the API
// server is asked to admit a privileged pod as a dry run.
func snapshotNamespaceRequirement(ctx *validators.Context, namespace string) snapshotRequirement {
	name := "privileged-namespace"
	ns, err := ctx.Clientset.CoreV1().Namespaces().Get(ctx.Ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return snapshotRequirement{name: name, ok: true, detail: "namespace " + namespace + " does not exist yet; label it privileged if the cluster enforces Pod Security"}
	}
	if err != nil {
		return snapshotRequirement{name: name, code: snapshotReadCode(err),
			detail: fmt.Sprintf("could not read namespace %s: %v", namespace, err)}
	}
	level := ns.Labels[snapshotPodSecurityLabel]
	switch {
	case level == snapshotPrivilegedLevel:
		return snapshotRequirement{name: name, ok: true, detail: "namespace " + namespace + " enforces the privileged Pod Security level"}
	case level != "":
		return snapshotRequirement{name: name, detail: fmt.Sprintf(
			"namespace %s enforces the %q Pod Security level; the node agent is privileged and needs %q", namespace, level, snapshotPrivilegedLevel)}
	}
	return snapshotAdmitsPrivilegedPod(ctx, name, namespace, snapshotAgentServiceAccount(ctx, namespace))
}

// snapshotAgentServiceAccount returns the service account the deployed node agent
// runs as, or "" when the agent is not installed or cannot be read. A policy may
// admit privileged pods for that account only, so the probe runs as it. Before the
// install there is no account to name, and a pod that names one that does not
// exist is refused by admission whatever the policy says, so the probe then uses
// the namespace default.
func snapshotAgentServiceAccount(ctx *validators.Context, namespace string) string {
	sets, err := ctx.Clientset.AppsV1().DaemonSets(namespace).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	for i := range sets.Items {
		if !strings.HasSuffix(sets.Items[i].Name, snapshotAgentDaemonSetSuffix) {
			continue
		}
		if sa := sets.Items[i].Spec.Template.Spec.ServiceAccountName; sa != "" {
			return sa
		}
	}
	return ""
}

// snapshotAdmitsPrivilegedPod asks the API server whether the namespace admits a
// privileged pod, with a server-side dry run, so admission decides (the cluster's
// Pod Security default and any policy webhook) and nothing is created.
func snapshotAdmitsPrivilegedPod(ctx *validators.Context, name, namespace, serviceAccount string) snapshotRequirement {
	privileged := true
	hostPathType := corev1.HostPathUnset
	volumes := make([]corev1.Volume, 0, len(snapshotAgentHostPaths))
	for i, path := range snapshotAgentHostPaths {
		volumes = append(volumes, corev1.Volume{
			Name:         fmt.Sprintf("host-%d", i),
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &hostPathType}},
		})
	}
	// The probe has the shape of Snapshot's node agent (privileged container, host
	// PID, IPC and network, hostPath volumes), so a policy that allows a privileged
	// container but refuses host namespaces or host paths is caught. Small requests
	// and limits keep a namespace ResourceQuota from refusing the dry run for a
	// reason that has nothing to do with admission of the agent.
	probe := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: snapshotProbePodPrefix, Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: serviceAccount,
			HostPID:            true,
			HostIPC:            true,
			HostNetwork:        true,
			Volumes:            volumes,
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           snapshotProbeImage,
				SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
	if _, err := ctx.Clientset.CoreV1().Pods(namespace).Create(ctx.Ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		if allocmode.IsK8sTimeoutErr(err) {
			// No admission decision was received, so the label advice below would be wrong.
			return snapshotRequirement{name: name, code: errors.ErrCodeTimeout, detail: fmt.Sprintf(
				"namespace %s has no %s label and the privileged-pod admission dry run timed out, so admission could not be evaluated: %v",
				namespace, snapshotPodSecurityLabel, err)}
		}
		return snapshotRequirement{name: name, detail: fmt.Sprintf(
			"namespace %s has no %s label and the API server would not admit a privileged pod there (dry run): %v; the node agent is privileged, so label the namespace %s=%s",
			namespace, snapshotPodSecurityLabel, err, snapshotPodSecurityLabel, snapshotPrivilegedLevel)}
	}
	return snapshotRequirement{name: name, ok: true, detail: "namespace " + namespace + " admits a privileged pod (server-side dry run)"}
}

// snapshotKernelRelease picks the major and minor version off a kernel release.
// Releases carry more numeric components (Azure Linux reports 6.6.78.1-1.azl3),
// which a three-part version parser rejects, and only major.minor matters here.
var snapshotKernelRelease = regexp.MustCompile(`^(\d+)\.(\d+)`)

// snapshotKernelSupported reports whether a node's kernel release meets
// snapshotKernelFloor. A release that does not parse does not.
func snapshotKernelSupported(kernel string) bool {
	m := snapshotKernelRelease.FindStringSubmatch(strings.TrimSpace(kernel))
	if m == nil {
		return false
	}
	major, errMajor := strconv.Atoi(m[1])
	minor, errMinor := strconv.Atoi(m[2])
	if errMajor != nil || errMinor != nil {
		return false
	}
	return major > snapshotKernelMajor || (major == snapshotKernelMajor && minor >= snapshotKernelMinor)
}

// snapshotRuntimeSupported reports whether a node's container runtime, as the
// kubelet reports it (for example "containerd://2.3.7"), is one Snapshot drives.
func snapshotRuntimeSupported(version string) bool {
	return strings.HasPrefix(version, "containerd://") || strings.HasPrefix(version, "cri-o://")
}

// snapshotMIGEnabled reports clear evidence that MIG is on: a MIG config other
// than all-disabled, a GPU product that names a MIG slice, or a MIG profile
// resource with a positive count (the mixed strategy labels those and leaves the
// product as it is). Labels that say nothing about MIG, such as a bare strategy,
// are not evidence.
func snapshotMIGEnabled(labels map[string]string) bool {
	if cfg := labels[snapshotMIGConfigLabel]; cfg != "" && cfg != snapshotMIGDisabled {
		return true
	}
	if strings.Contains(labels[snapshotGPUProductLabel], "-MIG-") {
		return true
	}
	for key, value := range labels {
		if !strings.HasPrefix(key, snapshotMIGResourcePrefix) || !strings.HasSuffix(key, snapshotMIGCountSuffix) {
			continue
		}
		if count, err := strconv.Atoi(value); err == nil && count > 0 {
			return true
		}
	}
	return false
}

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
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	k8spod "aicr.run/pkg/k8s/pod"
	"aicr.run/pkg/validator/labels"
	"aicr.run/validators"
	"aicr.run/validators/helper"
)

const (
	// multusNetworksAnnotation requests secondary networks for a pod, as
	// "<namespace>/<name>" references to NetworkAttachmentDefinitions.
	multusNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

	attachmentProbeContainer    = "probe"
	attachmentProbeGenerateName = "secondary-net-probe-"
	attachmentProbeComponent    = "secondary-network-attachment"

	// probeRunAsUser is busybox's nobody: listing /sys/class/net and running
	// `ip -o addr` need no privilege, so the probe passes a restricted
	// Pod Security admission.
	probeRunAsUser int64 = 65534

	// kwokNodeAnnotation marks a KWOK simulated node, where no CNI ever runs.
	kwokNodeAnnotation = "kwok.x-k8s.io/node"

	eventReasonFailedCreatePodSandBox = "FailedCreatePodSandBox"

	// kubeletUnexpectedAdmissionError is the reason the kubelet's device
	// manager gives a pod it rejects at admission, and
	// kubeletDevicesUnavailable starts the message naming the resource when
	// the rejection is that the last unit was taken.
	kubeletUnexpectedAdmissionError = "UnexpectedAdmissionError"
	kubeletDevicesUnavailable       = "requested number of devices unavailable for "

	// kubeletOutOfResourcePrefix starts the reason the kubelet's resource
	// predicate gives a pod it rejects ("OutOf<resource>"). The device manager
	// admits a device-plugin resource first, so for such a resource it means
	// the plugin is not registered on that kubelet.
	kubeletOutOfResourcePrefix = "OutOf"

	// schedulerInsufficientPrefix starts the scheduler's reason, inside an
	// Unschedulable message, that a node lacks a free unit of a resource.
	schedulerInsufficientPrefix = "Insufficient "

	// nodeNameField is the only field a node affinity's matchFields accepts.
	nodeNameField = "metadata.name"

	// maxProbeDiagnosticBytes caps a kubelet event message quoted in a failure.
	maxProbeDiagnosticBytes = 1024
)

// attachmentCheckConfig is the probe budget and fan-out. Production uses the
// pkg/defaults values; tests shrink the timeout.
type attachmentCheckConfig struct {
	probeTimeout  time.Duration
	reportReserve time.Duration
	concurrency   int
}

// attachmentStatus is one network's verdict.
type attachmentStatus int

const (
	attachmentPassed attachmentStatus = iota
	attachmentFailed
	// attachmentSkippedBusy: every schedulable node advertising the
	// network's resource has all of it requested by other pods.
	attachmentSkippedBusy
	// attachmentSkippedUnschedulable: every node advertising the resource is
	// cordoned.
	attachmentSkippedUnschedulable
)

// attachmentOutcome is the verdict on one declared network. node is empty
// when no probe ran.
type attachmentOutcome struct {
	network   secondaryNetwork
	node      string
	status    attachmentStatus
	detail    string
	notes     []string
	addrLines []string
}

func (o attachmentOutcome) failed(detail string) attachmentOutcome {
	o.status, o.detail = attachmentFailed, detail
	return o
}

func (o attachmentOutcome) busy(detail string) attachmentOutcome {
	o.status, o.detail = attachmentSkippedBusy, detail
	return o
}

// resultLine renders the per-network line. The "RESULT: " prefix is
// resultSummaryPrefix in pkg/validator/validator.go, which echoes the line to
// the CLI.
func (o attachmentOutcome) resultLine() string {
	on := ""
	if o.node != "" {
		on = " on node " + o.node
	}
	switch o.status {
	case attachmentPassed:
		return fmt.Sprintf("RESULT: %s: PASSED%s (%s)", o.network.key(), on, o.detail)
	case attachmentFailed:
		return fmt.Sprintf("RESULT: %s: FAILED%s (%s)", o.network.key(), on, o.detail)
	case attachmentSkippedBusy:
		return fmt.Sprintf("RESULT: %s: SKIPPED (busy: %s)", o.network.key(), o.detail)
	case attachmentSkippedUnschedulable:
		return fmt.Sprintf("RESULT: %s: SKIPPED (unschedulable: %s)", o.network.key(), o.detail)
	}
	return fmt.Sprintf("RESULT: %s: UNKNOWN (%s)", o.network.key(), o.detail)
}

// checkSecondaryNetworkAttachment proves that every secondary network the
// recipe ships can actually be attached to a pod (#3474). The device plugin
// advertising a VF and the SR-IOV node state reading Succeeded say nothing
// about Multus: when Multus does not attach the network, a pod requesting it
// gets the VF and no interface, silently falling back to the pod network.
//
// For each NetworkAttachmentDefinition the recipe renders
// (discoverSecondaryNetworks), it runs one short-lived pod, all networks in
// parallel, placed by the scheduler on one Ready, uncordoned node that has a
// free unit of the network's resource, annotated with the network and
// requesting one unit. The pod must finish, and its Multus network-status must
// show the attachment with an interface the pod can see, an address from the
// network's pool and outside the pod network when the network declares IPAM,
// and a PCI address for an SR-IOV VF (evaluateAttachment). The Multus failure
// modes surface as:
//   - no Multus in the node's CNI chain (NVKE as the image ships): the pod runs
//     on the primary network alone and fails on the missing network-status
//     entry;
//   - Multus present but broken (an expired token, a stale config): sandbox
//     creation fails and so does the check, with the kubelet's
//     FailedCreatePodSandBox message;
//   - a node whose Multus never wrote its config, where the runtime waits for
//     it (OKE GB300 under CRI-O): the node stays NotReady and the network
//     fails on that.
//
// The check skips when the recipe ships no secondary network, when the GPU
// and network nodes are KWOK simulated, and when no network could be probed
// because every candidate was busy or cordoned; a network with such
// candidates is disclosed as SKIPPED while the others are still probed. A
// network whose resource no node advertises, or whose uncordoned candidates
// are all NotReady, fails, and so does any API error.
func checkSecondaryNetworkAttachment(ctx *validators.Context) error {
	return runSecondaryNetworkAttachment(ctx, defaultAttachmentCheckConfig())
}

// defaultAttachmentCheckConfig is the production probe budget and fan-out.
func defaultAttachmentCheckConfig() attachmentCheckConfig {
	return attachmentCheckConfig{
		probeTimeout:  defaults.SecondaryNetworkProbeTimeout,
		reportReserve: defaults.SecondaryNetworkProbeReportReserve,
		concurrency:   defaults.SecondaryNetworkProbeConcurrency,
	}
}

// runSecondaryNetworkAttachment is checkSecondaryNetworkAttachment with the
// probe budget injected.
func runSecondaryNetworkAttachment(ctx *validators.Context, cfg attachmentCheckConfig) error {
	if ctx.ValidationInput == nil {
		return validators.SkipWithReason(validators.SkipReasonNoValidationInput,
			"no recipe validation input, so no secondary network is declared")
	}
	declared, err := discoverSecondaryNetworks(ctx.Ctx, nil, enabledComponentRefs(ctx.ValidationInput.ComponentRefs))
	if err != nil {
		return err
	}
	if len(declared.networks) == 0 {
		return validators.SkipWithReason(validators.SkipReasonComponentNotInRecipe,
			"the recipe ships no NetworkAttachmentDefinition, SriovNetwork or SriovIBNetwork, "+
				"so there is no secondary network to attach")
	}
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	printLines(declaredNetworkLines(declared.networks)...)

	nodes, err := listAttachmentNodes(ctx)
	if err != nil {
		return err
	}
	if simulatedCluster(nodes, declared.networks) {
		return validators.SkipWithReason(validators.SkipReasonSimulatedNodes,
			"every GPU and secondary-network node is a KWOK simulated node, where no CNI runs")
	}

	var podCIDRs []netip.Prefix
	if declared.anyIPAM() {
		if podCIDRs, err = clusterPodCIDRs(nodes); err != nil {
			return err
		}
		printLines("Pod network CIDRs: " + formatPrefixes(podCIDRs))
	}

	plan, err := planAttachmentProbes(ctx, nodes, declared.networks)
	if err != nil {
		return err
	}
	if len(plan.probes) == 0 {
		return reportAttachmentOutcomes(plan.resolved)
	}

	ns, err := newProbeNamespace()
	if err != nil {
		return err
	}
	result := probeNetworks(ctx, cfg, ns, plan, declared.poolSubnets, podCIDRs)
	return withProbeCleanup(result, ns.delete(ctx.Clientset)) //nolint:contextcheck // cleanup outlives the check budget
}

// probeNetworks creates the probe namespace, runs the planned probes in it,
// and reports every network's outcome.
func probeNetworks(ctx *validators.Context, cfg attachmentCheckConfig, ns *probeNamespace, plan attachmentPlan,
	poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) error {

	if err := ns.create(ctx.Ctx, ctx.Clientset); err != nil {
		return err
	}
	printLines(fmt.Sprintf("Probing %d network(s) from namespace %s...", len(plan.probes), ns.name))
	outcomes := make([]attachmentOutcome, 0, len(plan.resolved)+len(plan.probes))
	outcomes = append(outcomes, plan.resolved...)
	outcomes = append(outcomes, runAttachmentProbes(ctx, cfg, ns, plan.probes, poolSubnets, podCIDRs)...)
	return reportAttachmentOutcomes(outcomes)
}

// withProbeCleanup folds the namespace cleanup into the check's result: a
// failed cleanup replaces a pass or a skip, so a leaked probe holding a VF is
// never reported as clean, and is logged beside an existing failure.
func withProbeCleanup(result, cleanupErr error) error {
	if cleanupErr == nil {
		return result
	}
	if result == nil || validators.IsSkip(result) {
		return cleanupErr
	}
	slog.Error("probe namespace cleanup failed", "error", cleanupErr)
	return result
}

// declaredNetworkLines lists the networks discovery derived.
func declaredNetworkLines(networks []secondaryNetwork) []string {
	lines := make([]string, 0, 1+len(networks))
	lines = append(lines, fmt.Sprintf("Recipe ships %d secondary network(s):", len(networks)))
	for _, n := range networks {
		ipam := "no IPAM"
		if n.IPAMDeclared {
			ipam = "IPAM"
			if len(n.IPAMPools) > 0 {
				ipam += " from pool(s) " + strings.Join(n.IPAMPools, ",")
			}
		}
		lines = append(lines, fmt.Sprintf("  %s: resource %s, %s, %s", n.key(), valueOr(n.Resource, "<none>"), ipam, n.Source))
	}
	return lines
}

// listAttachmentNodes lists every node once; planning, simulation detection
// and the pod-network CIDRs all read the same snapshot.
func listAttachmentNodes(ctx *validators.Context) ([]corev1.Node, error) {
	listCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()
	list, err := ctx.Clientset.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, validators.Capability{Subject: "cluster nodes"}.RequireList(err)
	}
	return list.Items, nil
}

func isSimulatedNode(node *corev1.Node) bool {
	_, ok := node.Annotations[kwokNodeAnnotation]
	return ok
}

func isGPUNode(node *corev1.Node) bool {
	q, ok := node.Status.Allocatable[corev1.ResourceName(helper.GpuResourceName)]
	return ok && !q.IsZero()
}

// advertisesResource reports whether node has a positive allocatable count of
// resource.
func advertisesResource(node *corev1.Node, resourceName string) bool {
	q, ok := node.Status.Allocatable[corev1.ResourceName(resourceName)]
	return ok && q.Value() > 0
}

// simulatedCluster reports whether every GPU node and every node advertising
// a declared network's resource is a KWOK node, and there is at least one.
func simulatedCluster(nodes []corev1.Node, networks []secondaryNetwork) bool {
	relevant, simulated := 0, 0
	for i := range nodes {
		node := &nodes[i]
		isRelevant := isGPUNode(node)
		for _, n := range networks {
			if n.Resource != "" && advertisesResource(node, n.Resource) {
				isRelevant = true
			}
		}
		if !isRelevant {
			continue
		}
		relevant++
		if isSimulatedNode(node) {
			simulated++
		}
	}
	return relevant > 0 && simulated == relevant
}

// nodeReady reports whether node's Ready condition is True.
func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// probeCandidates partitions the non-simulated nodes that can serve network
// (those advertising its resource, or every GPU node for a network with no
// resource) into the Ready, uncordoned ones a probe can use, the uncordoned
// ones that are NotReady, and a count of the cordoned ones.
type probeCandidates struct {
	ready    []*corev1.Node
	notReady []string
	cordoned int
}

func (c probeCandidates) total() int { return len(c.ready) + len(c.notReady) + c.cordoned }

func candidatesFor(sorted []corev1.Node, network secondaryNetwork) probeCandidates {
	var c probeCandidates
	for i := range sorted {
		node := &sorted[i]
		if isSimulatedNode(node) {
			continue
		}
		serves := (network.Resource == "" && isGPUNode(node)) ||
			(network.Resource != "" && advertisesResource(node, network.Resource))
		switch {
		case !serves:
		case node.Spec.Unschedulable:
			c.cordoned++
		case !nodeReady(node):
			c.notReady = append(c.notReady, node.Name)
		default:
			c.ready = append(c.ready, node)
		}
	}
	return c
}

// probeAssignment is one network and the node its probe runs on.
type probeAssignment struct {
	network secondaryNetwork
	node    string
}

// attachmentPlan is the probes to run and the networks already decided
// without one.
type attachmentPlan struct {
	probes   []probeAssignment
	resolved []attachmentOutcome
}

// planAttachmentProbes picks one node per network: among the Ready,
// uncordoned candidates (candidatesFor), the first by name with a unit of the
// resource no pod requests. Units are reserved as they are assigned, so two
// networks sharing a resource never count the same free unit.
//
// A network no node advertises fails. When no candidate is Ready and
// uncordoned, a NotReady uncordoned candidate fails the network, since a node
// that should serve the fabric and cannot is a fault (#2122), while
// candidates that are all cordoned, an operator's deliberate choice, skip it.
// A network whose Ready candidates are all fully requested is skipped as
// busy. Reading a node's pod requests failing is an error, never "busy".
func planAttachmentProbes(ctx *validators.Context, nodes []corev1.Node, networks []secondaryNetwork) (attachmentPlan, error) {
	sorted := append([]corev1.Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	picker := freeUnitPicker{ctx: ctx, inUse: map[string]corev1.ResourceList{}, reserved: map[string]int64{}}

	var plan attachmentPlan
	for _, network := range networks {
		c := candidatesFor(sorted, network)
		res := valueOr(network.Resource, "a GPU")
		switch {
		case c.total() == 0:
			plan.resolved = append(plan.resolved, attachmentOutcome{network: network}.failed(notAdvertisedDetail(network)))
			continue
		case len(c.ready) == 0 && len(c.notReady) > 0:
			plan.resolved = append(plan.resolved, attachmentOutcome{network: network}.failed(fmt.Sprintf(
				"no Ready node advertising %s can run the probe: %d uncordoned node(s) are NotReady %s and %d cordoned; "+
					"check the kubelet and the node's CNI, such as a Multus that never wrote its config",
				res, len(c.notReady), formatNames(c.notReady), c.cordoned)))
			continue
		case len(c.ready) == 0:
			plan.resolved = append(plan.resolved, attachmentOutcome{
				network: network, status: attachmentSkippedUnschedulable,
				detail: fmt.Sprintf("all %d node(s) advertising %s are cordoned", c.cordoned, res),
			})
			continue
		}
		node, err := picker.pick(c.ready, network.Resource)
		if err != nil {
			return attachmentPlan{}, err
		}
		if node == "" {
			plan.resolved = append(plan.resolved, attachmentOutcome{network: network}.busy(fmt.Sprintf(
				"every unit of %s on the %d Ready node(s) advertising it is requested by other pods",
				network.Resource, len(c.ready))))
			continue
		}
		plan.probes = append(plan.probes, probeAssignment{network: network, node: node})
	}
	return plan, nil
}

func notAdvertisedDetail(network secondaryNetwork) string {
	if network.Resource == "" {
		return "the network names no resource and the cluster has no GPU node to probe it from"
	}
	return fmt.Sprintf("no node advertises allocatable %s, so no pod can request the network's device "+
		"(see the RDMA fabric gate of expected-resources)", network.Resource)
}

// freeUnitPicker finds a node with a free unit of a resource, reading each
// node's pod requests once.
type freeUnitPicker struct {
	ctx      *validators.Context
	inUse    map[string]corev1.ResourceList
	reserved map[string]int64 // key: node + "/" + resource
}

// pick returns the first node in ready with a free unit of resourceName and
// reserves it, or "" when every one is fully requested. A network with no
// resource takes the first node.
func (p *freeUnitPicker) pick(ready []*corev1.Node, resourceName string) (string, error) {
	if resourceName == "" {
		return ready[0].Name, nil
	}
	res := corev1.ResourceName(resourceName)
	for _, node := range ready {
		if err := p.ctx.Ctx.Err(); err != nil {
			return "", errors.WrapCtxErr(err, errors.ErrCodeTimeout, "selecting nodes for the attachment probes")
		}
		requests, ok := p.inUse[node.Name]
		if !ok {
			listCtx, cancel := p.ctx.Timeout(defaults.ResourceVerificationTimeout)
			read, err := helper.NodeResourceRequests(listCtx, p.ctx.Clientset, node.Name)
			cancel()
			if err != nil {
				return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal,
					fmt.Sprintf("could not determine how much %s is free on node %s", resourceName, node.Name))
			}
			p.inUse[node.Name], requests = read, read
		}
		allocatable := node.Status.Allocatable[res]
		requested := requests[res]
		key := node.Name + "/" + resourceName
		if allocatable.Value()-requested.Value()-p.reserved[key] >= 1 {
			p.reserved[key]++
			return node.Name, nil
		}
	}
	return "", nil
}

// runAttachmentProbes runs every probe, at most cfg.concurrency at a time.
// All probes share one wait deadline: cfg.probeTimeout from now, cut so that
// cfg.reportReserve of the check budget remains for the report.
func runAttachmentProbes(ctx *validators.Context, cfg attachmentCheckConfig, ns *probeNamespace,
	probes []probeAssignment, poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) []attachmentOutcome {

	deadline := time.Now().Add(cfg.probeTimeout)
	if checkDeadline, ok := ctx.Ctx.Deadline(); ok && checkDeadline.Add(-cfg.reportReserve).Before(deadline) {
		deadline = checkDeadline.Add(-cfg.reportReserve)
	}

	outcomes := make([]attachmentOutcome, len(probes))
	var g errgroup.Group
	g.SetLimit(cfg.concurrency)
	for i, p := range probes {
		g.Go(func() error {
			outcomes[i] = probeAttachment(ctx, ns, p, deadline, poolSubnets, podCIDRs)
			return nil
		})
	}
	// Every probe records its verdict in outcomes and returns nil.
	_ = g.Wait()
	return outcomes
}

// probeAttachment runs one network's probe pod and judges its attachment.
// The pod is deleted with the probe namespace.
func probeAttachment(ctx *validators.Context, ns *probeNamespace, p probeAssignment, deadline time.Time,
	poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) attachmentOutcome {

	out := attachmentOutcome{network: p.network, node: p.node}
	pods := ctx.Clientset.CoreV1().Pods(ns.name)

	createCtx, cancelCreate := ctx.Timeout(defaults.ResourceVerificationTimeout)
	created, err := pods.Create(createCtx, buildAttachmentProbePod(ns, p, ctx.Tolerations), metav1.CreateOptions{})
	cancelCreate()
	if err != nil {
		return out.failed(fmt.Sprintf("the probe pod was not created: %v", err))
	}

	waitCtx, cancelWait := context.WithDeadline(ctx.Ctx, deadline)
	pod, err := waitForProbePod(waitCtx, ctx.Clientset, ns.name, created.Name)
	cancelWait()
	if err != nil {
		if insufficient, message, unscheduled := probeUnschedulable(pod, p.network.Resource); unscheduled {
			if insufficient {
				return out.busy(fmt.Sprintf("the scheduler found no free %s on %s within the probe budget: %s",
					p.network.Resource, p.node, message))
			}
			return out.failed(fmt.Sprintf("the scheduler could not place the probe on %s: %s", p.node, message))
		}
		return out.failed(stuckProbeDetail(ctx, ns.name, created.Name, pod, err))
	}
	if pod.Status.Phase == corev1.PodFailed {
		return failedProbeOutcome(out, pod)
	}

	logCtx, cancelLogs := ctx.Timeout(defaults.ResourceVerificationTimeout)
	logs, err := k8spod.GetPodLogs(logCtx, ctx.Clientset, ns.name, created.Name, attachmentProbeContainer)
	cancelLogs()
	if err != nil {
		return out.failed(fmt.Sprintf("the probe's output could not be read: %v", err))
	}
	interfaces, addrLines, err := parseProbeOutput(logs)
	if err != nil {
		return out.failed(err.Error())
	}

	eval := evaluateAttachment(p.network, pod.Annotations[multusNetworkStatusAnnotation], interfaces, poolSubnets, podCIDRs)
	out.notes = eval.notes
	if len(eval.problems) > 0 {
		return out.failed(strings.Join(eval.problems, "; "))
	}
	out.status, out.detail, out.addrLines = attachmentPassed, eval.summary, addrLinesFor(addrLines, eval.iface)
	return out
}

// probeUnschedulable reports whether the scheduler marked pod Unschedulable
// and its message, and whether the message is that the node has no free unit
// of resourceName: busy, not a fault.
func probeUnschedulable(pod *corev1.Pod, resourceName string) (insufficient bool, message string, unscheduled bool) {
	if pod == nil {
		return false, "", false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return resourceName != "" && strings.Contains(c.Message, schedulerInsufficientPrefix+resourceName), c.Message, true
		}
	}
	return false, "", false
}

// failedProbeOutcome judges a probe pod in phase Failed. Only the device
// manager's rejection for the last unit of the network's own resource is
// busy, a race the scheduler lost to a pod bound outside it; every other
// failure, OutOf<resource> included, is a fault.
func failedProbeOutcome(out attachmentOutcome, pod *corev1.Pod) attachmentOutcome {
	res := out.network.Resource
	switch {
	case res != "" && pod.Status.Reason == kubeletUnexpectedAdmissionError &&
		strings.Contains(pod.Status.Message, kubeletDevicesUnavailable+res):
		return out.busy(fmt.Sprintf("the kubelet on %s had no free %s at admission; another pod took the last unit",
			out.node, res))
	case strings.HasPrefix(pod.Status.Reason, kubeletOutOfResourcePrefix):
		return out.failed(fmt.Sprintf("the kubelet on %s rejected the probe with %s: the device plugin for %s may not be registered on that node (%s)",
			out.node, pod.Status.Reason, valueOr(res, "the resource"), pod.Status.Message))
	default:
		return out.failed("the probe pod failed: " + podStateText(pod))
	}
}

// buildAttachmentProbePod is the probe for one network: placed by the
// scheduler on its node through a required node affinity, so its unit of the
// network's resource is accounted with every other pod's and the probe can
// never take a unit a workload was bound for. It requests one unit, is
// annotated with the network, tolerates every taint unless the run overrides
// tolerations, has no API token, and passes a restricted Pod Security
// admission.
func buildAttachmentProbePod(ns *probeNamespace, p probeAssignment, tolerations []corev1.Toleration) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	podLabels := ns.labels()
	podLabels[labels.Component] = attachmentProbeComponent

	var resources corev1.ResourceRequirements
	if p.network.Resource != "" {
		one := corev1.ResourceList{corev1.ResourceName(p.network.Resource): *resource.NewQuantity(1, resource.DecimalSI)}
		resources = corev1.ResourceRequirements{Limits: one, Requests: one.DeepCopy()}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: attachmentProbeGenerateName,
			Namespace:    ns.name,
			Labels:       podLabels,
			Annotations:  map[string]string{multusNetworksAnnotation: p.network.key()},
		},
		Spec: corev1.PodSpec{
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchFields: []corev1.NodeSelectorRequirement{{
							Key: nodeNameField, Operator: corev1.NodeSelectorOpIn, Values: []string{p.node},
						}},
					}},
				},
			}},
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			Tolerations:                  tolerations,
			ImagePullSecrets:             ns.imagePullSecretRefs(),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To(probeRunAsUser),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:      attachmentProbeContainer,
				Image:     defaults.ProbeImage,
				Command:   []string{"sh", "-c", attachmentProbeScript},
				Resources: resources,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

func podTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// waitForProbePod watches the probe pod until it reaches a terminal phase and
// returns it. When ctx ends first it returns the pod as last observed (nil if
// never read) with ErrCodeTimeout. A watch that closes or reports an error is
// re-established from a fresh Get, never taken as a verdict.
func waitForProbePod(ctx context.Context, client kubernetes.Interface, namespace, name string) (*corev1.Pod, error) {
	pods := client.CoreV1().Pods(namespace)
	var last *corev1.Pod
	timedOut := func() error {
		return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting for probe pod "+name+" to finish")
	}
	for {
		current, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return last, timedOut()
			}
			return last, errors.Wrap(errors.ErrCodeInternal, "failed to read probe pod "+name, err)
		}
		last = current
		if podTerminal(current) {
			return current, nil
		}

		watcher, err := pods.Watch(ctx, metav1.ListOptions{
			FieldSelector:   "metadata.name=" + name,
			ResourceVersion: current.ResourceVersion,
		})
		if err != nil {
			if ctx.Err() != nil {
				return last, timedOut()
			}
			return last, errors.Wrap(errors.ErrCodeInternal, "failed to watch probe pod "+name, err)
		}
		terminal, latest := drainProbeWatch(ctx, watcher, name)
		watcher.Stop()
		if latest != nil {
			last = latest
		}
		if terminal != nil {
			return terminal, nil
		}
		// The watch ended without a verdict. Pause before re-reading so a
		// stream that closes at once cannot spin.
		select {
		case <-ctx.Done():
			return last, timedOut()
		case <-time.After(defaults.PodPollInterval):
		}
	}
}

// drainProbeWatch returns the pod once an event shows it terminal, or a nil
// terminal when ctx ends or the watch closes or reports an error. latest is
// the last pod the watch delivered.
func drainProbeWatch(ctx context.Context, watcher watch.Interface, name string) (terminal, latest *corev1.Pod) {
	for {
		select {
		case <-ctx.Done():
			return nil, latest
		case event, ok := <-watcher.ResultChan():
			if !ok || event.Type == watch.Error {
				return nil, latest
			}
			pod, isPod := event.Object.(*corev1.Pod)
			if !isPod || pod.Name != name {
				continue
			}
			latest = pod
			if podTerminal(pod) {
				return pod, latest
			}
		}
	}
}

// stuckProbeDetail explains a scheduled probe that did not finish. While the
// pod is still waiting for its sandbox, the kubelet's latest
// FailedCreatePodSandBox event is the cause: Multus and the network's CNI run
// in sandbox creation, so a broken Multus (an expired token, "Unauthorized",
// or a stale config) or a failing CNI surfaces there. A node with no Multus
// at all creates the sandbox and fails later, on network-status.
func stuckProbeDetail(ctx *validators.Context, namespace, name string, pod *corev1.Pod, waitErr error) string {
	if pod == nil || pod.Status.Phase == corev1.PodPending || pod.Status.Phase == "" {
		if msg := latestSandboxFailure(ctx, namespace, name); msg != "" {
			return fmt.Sprintf("the probe pod never started: %s: %s", eventReasonFailedCreatePodSandBox, msg)
		}
	}
	state := "never observed"
	if pod != nil {
		state = podStateText(pod)
	}
	return fmt.Sprintf("the probe pod did not finish (%s): %v", state, waitErr)
}

// latestSandboxFailure returns the message of the pod's most recent
// FailedCreatePodSandBox event, or "" when there is none or the events cannot
// be read (the caller then reports the wait error instead).
func latestSandboxFailure(ctx *validators.Context, namespace, name string) string {
	listCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()
	events, err := ctx.Clientset.CoreV1().Events(namespace).List(listCtx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name,
	})
	if err != nil {
		slog.Warn("could not read probe pod events", "pod", name, "error", err)
		return ""
	}
	var latest *corev1.Event
	for i := range events.Items {
		ev := &events.Items[i]
		// Filtered again here: not every apiserver or fake honors the field
		// selector, and another pod's failure must never be attributed here.
		if ev.InvolvedObject.Name != name || ev.Reason != eventReasonFailedCreatePodSandBox {
			continue
		}
		if latest == nil || eventTime(ev).After(eventTime(latest)) {
			latest = ev
		}
	}
	if latest == nil {
		return ""
	}
	msg := strings.TrimSpace(latest.Message)
	if len(msg) > maxProbeDiagnosticBytes {
		msg = msg[:maxProbeDiagnosticBytes] + "..."
	}
	return msg
}

// eventTime prefers LastTimestamp, which repeats of the same event bump, over
// EventTime and FirstTimestamp.
func eventTime(ev *corev1.Event) time.Time {
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time
	}
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time
	}
	return ev.FirstTimestamp.Time
}

// podStateText renders a pod's phase, reason and first waiting container.
func podStateText(pod *corev1.Pod) string {
	parts := []string{"phase " + valueOr(string(pod.Status.Phase), "<none>")}
	if pod.Status.Reason != "" {
		parts = append(parts, "reason "+pod.Status.Reason)
	}
	if pod.Status.Message != "" {
		parts = append(parts, "message "+pod.Status.Message)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			waiting := fmt.Sprintf("container %s waiting: %s", cs.Name, w.Reason)
			if w.Message != "" {
				waiting += " (" + w.Message + ")"
			}
			parts = append(parts, waiting)
			break
		}
	}
	return strings.Join(parts, ", ")
}

// attachmentCoverage counts the outcomes. The structured extra reuses the
// node-coverage keys: each probe attaches one network on one node, so a count
// of probes is a count of node attachments.
type attachmentCoverage struct {
	total, passed, failed, busy, unschedulable int
}

func (c attachmentCoverage) skipped() int { return c.busy + c.unschedulable }

func (c attachmentCoverage) line() string {
	return fmt.Sprintf("RESULT: attachmentsValidated: %d/%d (passed %d, failed %d, skipped %d: busy %d, unschedulable %d)",
		c.passed, c.total, c.passed, c.failed, c.skipped(), c.busy, c.unschedulable)
}

func (c attachmentCoverage) extra() map[string]string {
	return map[string]string{
		validators.ExtraKeyNodesValidated: strconv.Itoa(c.passed),
		validators.ExtraKeyNodesTotal:     strconv.Itoa(c.total),
		validators.ExtraKeyNodesFailed:    strconv.Itoa(c.failed),
		validators.ExtraKeyNodesSkipped:   strconv.Itoa(c.skipped()),
	}
}

func noNetworkProbedReason(c attachmentCoverage) string {
	return fmt.Sprintf("no secondary network could be probed (%d busy, %d unschedulable)", c.busy, c.unschedulable)
}

// reportAttachmentOutcomes prints every network's line and the coverage,
// publishes the coverage extra, and returns the verdict: a failure naming
// every failed network, else a pass when any network attached, else a skip:
// nodes-busy when at least one network was busy, no-schedulable-gpu-nodes
// when every one was skipped because its candidates are cordoned.
func reportAttachmentOutcomes(outcomes []attachmentOutcome) error {
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].network.key() < outcomes[j].network.key() })
	cov := attachmentCoverage{total: len(outcomes)}
	var failures []string
	for _, o := range outcomes {
		printLines(o.resultLine())
		for _, note := range o.notes {
			printLines("  note: " + note)
		}
		for _, line := range o.addrLines {
			printLines("  ip -o addr: " + line)
		}
		switch o.status {
		case attachmentPassed:
			cov.passed++
		case attachmentFailed:
			cov.failed++
			where := ""
			if o.node != "" {
				where = " on node " + o.node
			}
			failures = append(failures, fmt.Sprintf("%s%s: %s", o.network.key(), where, o.detail))
		case attachmentSkippedBusy:
			cov.busy++
		case attachmentSkippedUnschedulable:
			cov.unschedulable++
		}
	}
	printLines(cov.line())
	emitExtraOrWarn(cov.extra())

	switch {
	case len(failures) > 0:
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"secondary network attachment failed for %d of %d network(s): %s",
			len(failures), cov.total, strings.Join(failures, "; ")))
	case cov.passed > 0:
		fmt.Printf("Attached %d of %d secondary network(s); %d skipped\n", cov.passed, cov.total, cov.skipped())
		return nil
	case cov.busy > 0:
		return validators.SkipWithReason(validators.SkipReasonNodesBusy, noNetworkProbedReason(cov))
	default:
		return validators.SkipWithReason(validators.SkipReasonNoSchedulableGPUNodes, noNetworkProbedReason(cov))
	}
}

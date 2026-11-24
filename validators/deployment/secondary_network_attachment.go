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
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
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

	// kubeletUnexpectedAdmissionError is the Failed pod reason of a kubelet
	// admission rejection, and kubeletDevicesUnavailable the start of its
	// message when no unit of the named resource could be allocated.
	kubeletUnexpectedAdmissionError = "UnexpectedAdmissionError"
	kubeletDevicesUnavailable       = "requested number of devices unavailable for "

	// kubeletOutOfResourcePrefix starts the Failed pod reason "OutOf<resource>".
	kubeletOutOfResourcePrefix = "OutOf"

	// schedulerInsufficientPrefix starts, inside an Unschedulable message, the
	// resource a node lacks ("Insufficient <resource>").
	schedulerInsufficientPrefix = "Insufficient "

	// nodeNameField is the node field a node affinity's matchFields selects on.
	nodeNameField = "metadata.name"

	// maxProbeDiagnosticBytes caps a kubelet event message quoted in a failure.
	maxProbeDiagnosticBytes = 1024
)

// attachmentCheckConfig is the probe budget and fan-out, and how a probe's
// output is read. Production uses the pkg/defaults values and the pod log;
// tests shrink the timeout and serve each probe its own output.
type attachmentCheckConfig struct {
	probeTimeout  time.Duration
	reportReserve time.Duration
	concurrency   int
	podLogs       podLogsFunc
}

// podLogsFunc reads one container's log.
type podLogsFunc func(ctx context.Context, client kubernetes.Interface, namespace, pod, container string) (string, error)

// attachmentStatus is one network's verdict.
type attachmentStatus int

const (
	attachmentPassed attachmentStatus = iota
	attachmentFailed
	// attachmentSkippedBusy: every schedulable node advertising the
	// network's resource has all of it requested by other pods.
	attachmentSkippedBusy
	// attachmentSkippedUnschedulable: every node advertising the resource is
	// cordoned or carries a taint the run's tolerations do not cover.
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
// parallel, placed by the scheduler on one Ready, uncordoned node whose taints
// the run tolerates and that has a free unit of the network's resource,
// annotated with the network and requesting one unit. The pod must finish, and
// its Multus network-status must show the attachment with an interface the pod
// can see; when the network declares IPAM, addresses configured on that
// interface, inside the declared range and outside the pod network; and a PCI
// address for an SR-IOV VF (evaluateAttachment). The Multus failure
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
// because every candidate was busy, cordoned or excluded by the run's
// tolerations; a network with such
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
		podLogs:       k8spod.GetPodLogs,
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

	plan, err := planAttachmentProbes(ctx, nodes, declared.networks, "")
	if err != nil {
		return err
	}
	if len(plan.probes) == 0 {
		return reportAttachmentOutcomes(append(plan.resolved, heldByRunOutcomes(plan.deferred, "no probe ran")...))
	}

	ns, err := newProbeNamespace()
	if err != nil {
		return err
	}
	result := probeNetworks(ctx, cfg, ns, nodes, plan, declared.poolSubnets, podCIDRs)
	return withProbeCleanup(result, ns.delete(ctx.Clientset)) //nolint:contextcheck // cleanup outlives the check budget
}

// probeNetworks creates the probe namespace, runs the planned probes in it,
// and reports every network's outcome. Networks deferred because this run's
// own probes held the only free units are planned again after each wave, once
// those probes have finished and released them, within the same probe
// budget; one still waiting when no probe can run is reported with that
// cause, never as a workload holding the unit.
func probeNetworks(ctx *validators.Context, cfg attachmentCheckConfig, ns *probeNamespace, nodes []corev1.Node,
	plan attachmentPlan, poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) error {

	if err := ns.create(ctx.Ctx, ctx.Clientset); err != nil {
		return err
	}
	deadline := probeDeadline(ctx, cfg)
	outcomes := append([]attachmentOutcome(nil), plan.resolved...)
	probes, deferred := plan.probes, plan.deferred
	for {
		printLines(fmt.Sprintf("Probing %d network(s) from namespace %s...", len(probes), ns.name))
		outcomes = append(outcomes, runAttachmentProbes(ctx, cfg, ns, probes, deadline, poolSubnets, podCIDRs)...)
		if len(deferred) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			outcomes = append(outcomes, heldByRunOutcomes(deferred, "the probe budget ran out before it was released")...)
			break
		}
		next, err := planAttachmentProbes(ctx, nodes, deferred, ns.name)
		if err != nil {
			for _, network := range deferred {
				outcomes = append(outcomes, attachmentOutcome{network: network}.failed(
					fmt.Sprintf("the network could not be planned again after this run's other probes: %v", err)))
			}
			break
		}
		outcomes = append(outcomes, next.resolved...)
		if len(next.probes) == 0 {
			outcomes = append(outcomes, heldByRunOutcomes(next.deferred, "those probes did not finish and release it")...)
			break
		}
		probes, deferred = next.probes, next.deferred
	}
	return reportAttachmentOutcomes(outcomes)
}

// heldByRunOutcomes reports networks that were not probed because every unit
// of their resource that no workload holds is held by this run's own probes
// of other networks. They count as busy, and the detail names the cause.
func heldByRunOutcomes(networks []secondaryNetwork, why string) []attachmentOutcome {
	out := make([]attachmentOutcome, 0, len(networks))
	for _, network := range networks {
		out = append(out, attachmentOutcome{network: network}.busy(fmt.Sprintf(
			"every unit of %s that no workload holds is held by this run's probe of another network sharing it, and %s",
			network.Resource, why)))
	}
	return out
}

// probeDeadline is the wait deadline every probe shares: cfg.probeTimeout
// from now, cut so that cfg.reportReserve of the check budget remains for the
// report.
func probeDeadline(ctx *validators.Context, cfg attachmentCheckConfig) time.Time {
	deadline := time.Now().Add(cfg.probeTimeout)
	if checkDeadline, ok := ctx.Ctx.Deadline(); ok && checkDeadline.Add(-cfg.reportReserve).Before(deadline) {
		deadline = checkDeadline.Add(-cfg.reportReserve)
	}
	return deadline
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
// ones that are NotReady, and counts of the cordoned ones and of the Ready,
// uncordoned ones whose taints the run's tolerations do not cover.
type probeCandidates struct {
	ready       []*corev1.Node
	notReady    []string
	cordoned    int
	untolerated int
}

func (c probeCandidates) total() int {
	return len(c.ready) + len(c.notReady) + c.cordoned + c.untolerated
}

// candidatesFor partitions sorted for network. tolerations is the probe's
// override; nil is the probe's tolerate-all default, under which no taint
// excludes a node.
func candidatesFor(sorted []corev1.Node, network secondaryNetwork, tolerations []corev1.Toleration) probeCandidates {
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
		case tolerations != nil && hasUntoleratedTaint(node, tolerations):
			c.untolerated++
		default:
			c.ready = append(c.ready, node)
		}
	}
	return c
}

// hasUntoleratedTaint reports whether node carries a NoSchedule or NoExecute
// taint that no toleration covers, which keeps the scheduler from placing a
// probe there.
func hasUntoleratedTaint(node *corev1.Node, tolerations []corev1.Toleration) bool {
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := slices.ContainsFunc(tolerations, func(t corev1.Toleration) bool {
			return t.ToleratesTaint(logr.Discard(), taint, false)
		})
		if !tolerated {
			return true
		}
	}
	return false
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
	// deferred are the networks whose only units no workload holds are held
	// by this run, reserved for another network's probe or requested by one
	// of its unfinished probe pods.
	deferred []secondaryNetwork
}

// planAttachmentProbes picks one node per network: among the Ready,
// uncordoned candidates whose taints the run's tolerations cover
// (candidatesFor), the first by name with a unit of the resource no pod
// requests. Units are reserved as they are assigned, so two
// networks sharing a resource never count the same free unit. ownNamespace is
// the probe namespace, whose unfinished pods are this run's own; "" before it
// exists.
//
// A network no node advertises fails. When no candidate is Ready and
// uncordoned, a NotReady uncordoned candidate fails the network, since a node
// that should serve the fabric and cannot is a fault (#2122), while
// candidates that are all cordoned or excluded by the run's tolerations, an
// operator's deliberate choices, skip it.
// A network whose Ready candidates are all fully requested by other pods is
// skipped as busy; one that waits only on units this run holds is deferred.
// Reading a node's pod requests failing is an error, never "busy".
func planAttachmentProbes(ctx *validators.Context, nodes []corev1.Node, networks []secondaryNetwork,
	ownNamespace string) (attachmentPlan, error) {

	sorted := append([]corev1.Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	picker := freeUnitPicker{ctx: ctx, ownNamespace: ownNamespace, usage: map[string]nodeUsage{}, reserved: map[string]int64{}}

	var plan attachmentPlan
	for _, network := range networks {
		c := candidatesFor(sorted, network, ctx.Tolerations)
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
				detail: unschedulableDetail(c, res),
			})
			continue
		}
		node, heldByRun, err := picker.pick(c.ready, network.Resource)
		if err != nil {
			return attachmentPlan{}, err
		}
		switch {
		case node == "" && heldByRun:
			plan.deferred = append(plan.deferred, network)
			continue
		case node == "":
			plan.resolved = append(plan.resolved, attachmentOutcome{network: network}.busy(fmt.Sprintf(
				"every unit of %s on the %d Ready node(s) advertising it is requested by other pods",
				network.Resource, len(c.ready))))
			continue
		}
		plan.probes = append(plan.probes, probeAssignment{network: network, node: node})
	}
	return plan, nil
}

func unschedulableDetail(c probeCandidates, res string) string {
	if c.untolerated == 0 {
		return fmt.Sprintf("all %d node(s) advertising %s are cordoned", c.cordoned, res)
	}
	return fmt.Sprintf("all %d node(s) advertising %s are cordoned (%d) or carry a taint the run's tolerations do not cover (%d)",
		c.total(), res, c.cordoned, c.untolerated)
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
	ctx          *validators.Context
	ownNamespace string
	usage        map[string]nodeUsage
	reserved     map[string]int64 // key: node + "/" + resource
}

// nodeUsage is what a node's unfinished pods request: other workloads', and
// this run's own probe pods'.
type nodeUsage struct {
	others, own corev1.ResourceList
}

// pick returns the first node in ready with a unit of resourceName that no
// pod requests and this run has not reserved, and reserves it. With none,
// heldByRun reports whether some node has a unit no other workload requests,
// held only by this run. A network with no resource takes the first node.
func (p *freeUnitPicker) pick(ready []*corev1.Node, resourceName string) (node string, heldByRun bool, err error) {
	if resourceName == "" {
		return ready[0].Name, false, nil
	}
	res := corev1.ResourceName(resourceName)
	for _, n := range ready {
		if err := p.ctx.Ctx.Err(); err != nil {
			return "", false, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "selecting nodes for the attachment probes")
		}
		usage, ok := p.usage[n.Name]
		if !ok {
			listCtx, cancel := p.ctx.Timeout(defaults.ResourceVerificationTimeout)
			others, own, readErr := helper.NodeResourceRequestsSplit(listCtx, p.ctx.Clientset, n.Name, inNamespace(p.ownNamespace))
			cancel()
			if readErr != nil {
				return "", false, errors.PropagateOrWrap(readErr, errors.ErrCodeInternal,
					fmt.Sprintf("could not determine how much %s is free on node %s", resourceName, n.Name))
			}
			usage = nodeUsage{others: others, own: own}
			p.usage[n.Name] = usage
		}
		allocatable := n.Status.Allocatable[res]
		othersHold, ownHold := usage.others[res], usage.own[res]
		key := n.Name + "/" + resourceName
		notOthers := allocatable.Value() - othersHold.Value()
		if notOthers-ownHold.Value()-p.reserved[key] >= 1 {
			p.reserved[key]++
			return n.Name, false, nil
		}
		if notOthers >= 1 {
			heldByRun = true
		}
	}
	return "", heldByRun, nil
}

// inNamespace selects the pods of namespace; "" selects none.
func inNamespace(namespace string) func(*corev1.Pod) bool {
	if namespace == "" {
		return nil
	}
	return func(pod *corev1.Pod) bool { return pod.Namespace == namespace }
}

// runAttachmentProbes runs every probe, at most cfg.concurrency at a time,
// all waiting to the shared deadline (probeDeadline).
func runAttachmentProbes(ctx *validators.Context, cfg attachmentCheckConfig, ns *probeNamespace, probes []probeAssignment,
	deadline time.Time, poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) []attachmentOutcome {

	outcomes := make([]attachmentOutcome, len(probes))
	var g errgroup.Group
	g.SetLimit(cfg.concurrency)
	for i, p := range probes {
		g.Go(func() error {
			outcomes[i] = probeAttachment(ctx, cfg.podLogs, ns, p, deadline, poolSubnets, podCIDRs)
			return nil
		})
	}
	// Every probe records its verdict in outcomes and returns nil.
	_ = g.Wait()
	return outcomes
}

// probeAttachment runs one network's probe pod and judges its attachment.
// The pod is deleted with the probe namespace.
func probeAttachment(ctx *validators.Context, podLogs podLogsFunc, ns *probeNamespace, p probeAssignment, deadline time.Time,
	poolSubnets map[string]netip.Prefix, podCIDRs []netip.Prefix) attachmentOutcome {

	out := attachmentOutcome{network: p.network, node: p.node}
	pods := ctx.Clientset.CoreV1().Pods(ns.name)

	// A probe queued behind the others may start late; neither its create nor
	// its wait may run into the report reserve.
	if !time.Now().Before(deadline) {
		return out.failed("the probe budget ran out before this probe could start; " +
			"the probes ahead of it held every slot until the deadline")
	}
	createDeadline := time.Now().Add(defaults.ResourceVerificationTimeout)
	if deadline.Before(createDeadline) {
		createDeadline = deadline
	}
	createCtx, cancelCreate := context.WithDeadline(ctx.Ctx, createDeadline)
	created, err := pods.Create(createCtx, buildAttachmentProbePod(ns, p, ctx.Tolerations), metav1.CreateOptions{})
	cancelCreate()
	if err != nil {
		return out.failed(fmt.Sprintf("the probe pod was not created: %v", err))
	}

	pod, verdict, decided := awaitProbe(ctx, out, ns.name, created.Name, deadline)
	if decided {
		return verdict
	}
	if pod.Status.Phase == corev1.PodFailed {
		if deviceShortageAtAdmission(pod, p.network.Resource) {
			return contentionOutcome(ctx, out, ns.name, "the kubelet had no unit at admission")
		}
		return failedProbeOutcome(out, pod)
	}

	if podLogs == nil {
		podLogs = k8spod.GetPodLogs
	}
	logCtx, cancelLogs := ctx.Timeout(defaults.ResourceVerificationTimeout)
	logs, err := podLogs(logCtx, ctx.Clientset, ns.name, created.Name, attachmentProbeContainer)
	cancelLogs()
	if err != nil {
		return out.failed(fmt.Sprintf("the probe's output could not be read: %v", err))
	}
	interfaces, addrLines, err := parseProbeOutput(logs)
	if err != nil {
		return out.failed(err.Error())
	}

	eval := evaluateAttachment(p.network, pod.Annotations[multusNetworkStatusAnnotation], interfaces, addrLines, poolSubnets, podCIDRs)
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

// awaitProbe waits for the probe pod until the deadline. It returns the
// terminal pod with decided false, or decided true with the verdict on a probe
// that did not finish. A wait stopped by an API error, or by the check's own
// context, fails with that error, never busy. At the deadline the pod is read
// once more, so the verdict never rests on a state a closed watch left stale:
// a pod that finished meanwhile is judged as finished, an Unschedulable one
// for lack of a unit of the network's resource is busy only with evidence
// (contentionOutcome), any other Unschedulable message fails with it, and a
// pod stuck after scheduling fails with stuckProbeDetail.
func awaitProbe(ctx *validators.Context, out attachmentOutcome, namespace, name string,
	deadline time.Time) (pod *corev1.Pod, verdict attachmentOutcome, decided bool) {

	waitCtx, cancelWait := context.WithDeadline(ctx.Ctx, deadline)
	pod, waitErr := waitForProbePod(waitCtx, ctx.Clientset, namespace, name)
	reachedDeadline := stderrors.Is(waitCtx.Err(), context.DeadlineExceeded)
	cancelWait()
	if waitErr == nil {
		return pod, attachmentOutcome{}, false
	}
	if !reachedDeadline {
		return nil, out.failed(fmt.Sprintf("the probe pod could not be observed to the end of the probe budget: %v", waitErr)), true
	}

	readCtx, cancelRead := ctx.Timeout(defaults.ResourceVerificationTimeout)
	fresh, err := ctx.Clientset.CoreV1().Pods(namespace).Get(readCtx, name, metav1.GetOptions{})
	cancelRead()
	if err != nil {
		return nil, out.failed(fmt.Sprintf("the probe pod did not finish within the probe budget and could not be read again: %v", err)), true
	}
	if podTerminal(fresh) {
		return fresh, attachmentOutcome{}, false
	}
	if insufficient, message, unscheduled := probeUnschedulable(fresh, out.network.Resource); unscheduled {
		if insufficient {
			return nil, contentionOutcome(ctx, out, namespace, "the scheduler found no free unit within the probe budget ("+message+")"), true
		}
		return nil, out.failed(fmt.Sprintf("the scheduler could not place the probe on %s: %s", out.node, message)), true
	}
	return nil, out.failed(stuckProbeDetail(ctx, namespace, name, fresh, waitErr)), true
}

// deviceShortageAtAdmission reports whether the kubelet rejected pod because
// no unit of resourceName could be allocated (contentionOutcome decides
// whether that is busy).
func deviceShortageAtAdmission(pod *corev1.Pod, resourceName string) bool {
	return resourceName != "" && pod.Status.Reason == kubeletUnexpectedAdmissionError &&
		strings.Contains(pod.Status.Message, kubeletDevicesUnavailable+resourceName)
}

// contentionOutcome decides a shortage of the network's resource on its node
// (what names the signal) by evidence of competing allocation. It is busy
// only when pods outside this run's probe namespace request at least one unit
// and every allocatable unit. Otherwise nothing the check can see holds the
// missing units: the node's devices may have turned unhealthy after planning,
// or this run's own probes hold them, and the network fails. An error
// reading the node or its pods fails too.
func contentionOutcome(ctx *validators.Context, out attachmentOutcome, ownNamespace, what string) attachmentOutcome {
	res := out.network.Resource
	readCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()
	node, err := ctx.Clientset.CoreV1().Nodes().Get(readCtx, out.node, metav1.GetOptions{})
	if err != nil {
		return out.failed(fmt.Sprintf("%s for %s on %s, and the node could not be read to tell contention from a fault: %v",
			what, res, out.node, err))
	}
	others, _, err := helper.NodeResourceRequestsSplit(readCtx, ctx.Clientset, out.node, inNamespace(ownNamespace))
	if err != nil {
		return out.failed(fmt.Sprintf("%s for %s on %s, and its pods could not be read to tell contention from a fault: %v",
			what, res, out.node, err))
	}
	allocatable := node.Status.Allocatable[corev1.ResourceName(res)]
	requested := others[corev1.ResourceName(res)]
	if requested.Value() >= 1 && requested.Value() >= allocatable.Value() {
		return out.busy(fmt.Sprintf("%s on %s; other workloads request %d of %d allocatable %s",
			what, out.node, requested.Value(), allocatable.Value(), res))
	}
	return out.failed(fmt.Sprintf("%s on %s although other workloads request only %d of %d allocatable %s: "+
		"a unit may be unhealthy or allocatable may have dropped since planning", what, out.node, requested.Value(), allocatable.Value(), res))
}

// failedProbeOutcome judges a probe pod in phase Failed for any reason other
// than a device-allocation shortage. Every such failure is a fault. For a
// device-plugin resource, OutOf<resource> means the plugin is not registered
// on the node: the kubelet admits such a resource through its device manager
// before the resource predicate that gives this reason runs.
func failedProbeOutcome(out attachmentOutcome, pod *corev1.Pod) attachmentOutcome {
	res := out.network.Resource
	switch {
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

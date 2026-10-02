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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
)

// gkeAcceleratorLabel selects a3-megagpu-8g GPU nodes on GKE — the nodes that
// must map all 8 GPU NIC PCI slots for TCPXO. It is a3-megagpu-8g-specific, like
// gkenet.RequiredGPUNICInterfaces.
const gkeAcceleratorLabel = "cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb"

// Coverage keys for EmitExtra (constants so the package's literal-occurrence count
// stays under the goconst threshold).
const (
	topoKeyValidated = "nodesValidated"
	topoKeyTotal     = "nodesTotal"
)

// checkGKEGPUNICTopology is the case-1 arm of #2265: a GPU node pool provisioned
// with a gVNIC additional network takes a GPU NIC PCI slot, leaving 7/8 GPUs
// usable while every Network object exists (the census passes clean). Read each
// a3 GPU node's networking.gke.io/nic-info annotation and fail closed on the
// displacement signatures that are detectable from it (an interface beyond
// eth0..eth8, or fewer than 8 of eth1..eth8). Reads node annotations; the sibling
// gke-gpu-nic-networks check reads cluster-scoped Network CRs.
//
// Fail-closed contract: a node whose annotation is PRESENT but unparseable fails
// the check; a node whose annotation is ABSENT is unverified (a documented
// limitation, surfaced as a Skip when nothing could be verified).
func checkGKEGPUNICTopology(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes clientset is not available")
	}

	// The prerequisite belongs to gke-nccl-tcpxo: a recipe that does not declare
	// the component is not asking for TCPXO, so node topology is not this check's
	// business. Mirrors the sibling check's declaration gate.
	if !validators.RecipeDeclares(ctx, tcpxoComponent) {
		return validators.Skip(
			tcpxoComponent + " not declared in recipe — GPUDirect TCPXO networking is inapplicable")
	}

	// Coverage is recorded once the node list succeeded (a list error must not
	// report nodesTotal: 0, which would misread as an empty cluster).
	totalNodes := 0
	verifiedCount := 0
	listed := false
	defer func() {
		if !listed {
			return
		}
		emitExtraOrWarn(map[string]string{
			topoKeyValidated: strconv.Itoa(verifiedCount),
			topoKeyTotal:     strconv.Itoa(totalNodes),
		})
	}()

	listCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(listCtx, metav1.ListOptions{LabelSelector: gkeAcceleratorLabel})
	if err != nil {
		capability := validators.Capability{Component: tcpxoComponent, Subject: "a3 GPU nodes (nodes)"}
		return capability.RequireList(err)
	}
	listed = true
	totalNodes = len(nodes.Items)
	if len(nodes.Items) == 0 {
		// A recipe declaring gke-nccl-tcpxo expects a3 GPU nodes; an empty result on a
		// declared capability fails (never a vacuous pass or a skip), matching the
		// capability contract.
		return errors.New(errors.ErrCodeNotFound, nicTopologyMsg("the cluster has no a3-megagpu-8g GPU nodes", remediationNoNodes))
	}

	// The expected GPU NIC Network set for the north-interfaces join (which Network
	// each ethN must sit on). Reuses the sibling census's discovery. A list error
	// blocks (fail closed); an empty set means the displacement join cannot run.
	gpuSet := map[string]bool{}
	if ctx.DynamicClient != nil {
		gpuNICList, derr := gkenet.DiscoverGPUNICNetworks(ctx.Ctx, ctx.DynamicClient)
		if derr != nil {
			capability := validators.Capability{Component: tcpxoComponent, Subject: "GPU NIC networks (networks)"}
			return capability.RequireList(derr)
		}
		for _, n := range gpuNICList {
			gpuSet[n] = true
		}
	}

	var problems []string
	var unverified []string
	var displacementUnverified []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		annotation := node.Annotations[gkenet.NICInfoAnnotation]
		if annotation == "" {
			unverified = append(unverified, node.Name)
			slog.Warn("GPU node has no nic-info annotation; NIC topology unverified", "node", node.Name)
			continue
		}
		info, err := gkenet.ParseNICInfo(annotation)
		if err != nil {
			// A present-but-unparseable annotation fails closed: the node carries a
			// nic-info value we cannot trust, so we must not skip it into silence.
			problems = append(problems, fmt.Sprintf("node %q has an unparseable %s annotation: %v", node.Name, gkenet.NICInfoAnnotation, err))
			continue
		}
		verifiedCount++

		if len(info.ExtraInterfaces) > 0 {
			problems = append(problems, fmt.Sprintf("node %q has unexpected interface(s) beyond eth0..eth8: %s (gVNIC displacement signature)", node.Name, strings.Join(info.ExtraInterfaces, ",")))
		}
		if info.GPUNICInterfaces < gkenet.RequiredGPUNICInterfaces {
			problems = append(problems, fmt.Sprintf("node %q maps %d of %d GPU NIC interfaces (missing: %s)",
				node.Name, info.GPUNICInterfaces, gkenet.RequiredGPUNICInterfaces, strings.Join(info.SortedMissing(), ",")))
		}

		// Displacement detection via the north-interfaces join: which Network does
		// each ethN actually sit on? Catches the uniform gVNIC displacement that
		// name/PCI alone cannot see.
		northAnnotation := node.Annotations[gkenet.NorthInterfacesAnnotation]
		switch northByIP, nerr := gkenet.ParseNorthInterfaces(northAnnotation); {
		case northAnnotation == "":
			// Can't run the join without north-interfaces; the nic-info shape checks
			// above still ran. Note displacement unverified rather than failing.
			slog.Warn("GPU node has no north-interfaces annotation; NIC displacement unverified", "node", node.Name)
			displacementUnverified = append(displacementUnverified, node.Name)
		case nerr != nil:
			problems = append(problems, fmt.Sprintf("node %q has an unparseable %s annotation: %v", node.Name, gkenet.NorthInterfacesAnnotation, nerr))
		case len(gpuSet) > 0:
			if displaced := gkenet.DisplacedGPUNICInterfaces(info, northByIP, gpuSet); len(displaced) > 0 {
				problems = append(problems, fmt.Sprintf("node %q has GPU NIC interface(s) mapped to a non-GPU network (gVNIC displacement): %s", node.Name, strings.Join(displaced, ",")))
			}
		default:
			// The annotation parsed but no GPU NIC networks were discovered, so the join
			// cannot validate displacement. Do not count the node as verified for it
			// (the empty Network set itself is the sibling census's failure, not ours).
			slog.Warn("GPU NIC network set is empty; NIC displacement unverified", "node", node.Name)
			displacementUnverified = append(displacementUnverified, node.Name)
		}
	}

	if len(problems) > 0 {
		return errors.New(errors.ErrCodeConflict, nicTopologyMsg(strings.Join(problems, "; "), remediationDisplacement))
	}
	if verifiedCount == 0 {
		return validators.Skip(fmt.Sprintf("could not verify NIC topology on any of the %d a3 GPU node(s) (%d without a nic-info annotation)", totalNodes, len(unverified)))
	}
	if len(unverified) > 0 || len(displacementUnverified) > 0 {
		fmt.Printf("Verified NIC topology on %d a3 GPU node(s); %d node(s) have no nic-info annotation (unverified); %d node(s) lack north-interfaces (displacement unverified)\n", verifiedCount, len(unverified), len(displacementUnverified))
		return nil
	}
	fmt.Printf("Verified NIC topology on %d a3 GPU node(s): all map %d GPU NIC interfaces (eth1..eth8)\n",
		verifiedCount, gkenet.RequiredGPUNICInterfaces)
	return nil
}

// nicTopologyMsg builds the operator-facing message for a NIC-topology failure
// (case 1). detail names what was observed; remediation is specific to the
// failure kind (displacement vs absent nodes).
func nicTopologyMsg(detail, remediation string) string {
	return fmt.Sprintf(
		"recipe declares %s but %s — GPUDirect TCPXO needs all 8 GPU NIC PCI slots mapped to "+
			"eth1..eth8. %s Verify with: kubectl get node <gpu-node> -o jsonpath='{.metadata.annotations.networking\\.gke\\.io/nic-info}' "+
			"(see docs/integrator/gke-tcpxo-networking.md)",
		tcpxoComponent, detail, remediation)
}

const (
	remediationDisplacement = "Re-provision the GPU node pool WITHOUT a gVNIC additional network (it takes a GPU NIC PCI slot)."
	remediationNoNodes      = "No a3-megagpu-8g nodes were found — check the GPU pool exists, is not scaled to zero, and carries cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb."
)

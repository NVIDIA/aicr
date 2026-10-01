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
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
)

// gkeAcceleratorLabel selects a3-megagpu-8g GPU nodes on GKE — the nodes that
// must map all 8 GPU NIC PCI slots for TCPXO.
const gkeAcceleratorLabel = "cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb"

// Coverage keys for EmitExtra (constants so the package's literal-occurrence count
// stays under the goconst threshold).
const (
	topoKeyValidated  = "nodesValidated"
	topoKeyUnverified = "nodesUnverified"
	topoKeyTotal      = "nodesTotal"
)

// checkGKEGPUNICTopology is the case-1 arm of #2265: a GPU node pool provisioned
// with a gVNIC additional network takes a GPU NIC PCI slot, leaving 7/8 GPUs
// usable while every Network object exists (the census passes clean). Read each
// a3 GPU node's networking.gke.io/nic-info annotation and fail closed on the
// displacement. Reads node annotations; the sibling gke-gpu-nic-networks check
// reads cluster-scoped Network CRs.
// nodeTopology pairs a node name with its parsed NIC topology (for pool-consistency).
type nodeTopology struct {
	name string
	info *gkenet.NodeNICInfo
}

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

	// Coverage is recorded on EVERY exit path (including list-error and zero-node),
	// so the result states how much was actually verified.
	totalNodes := 0
	verifiedCount := 0
	unverifiedCount := 0
	defer func() {
		emitExtraOrWarn(map[string]string{
			topoKeyValidated:  strconv.Itoa(verifiedCount),
			topoKeyUnverified: strconv.Itoa(unverifiedCount),
			topoKeyTotal:      strconv.Itoa(totalNodes),
		})
	}()

	listCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(listCtx, metav1.ListOptions{LabelSelector: gkeAcceleratorLabel})
	if err != nil {
		capability := validators.Capability{Component: tcpxoComponent, Subject: "a3 GPU nodes (nodes)"}
		return capability.RequireList(err)
	}
	totalNodes = len(nodes.Items)
	if len(nodes.Items) == 0 {
		// A recipe declaring gke-nccl-tcpxo expects a3 GPU nodes; an empty result on a
		// declared capability fails (never a vacuous pass or a skip), matching the
		// capability contract.
		return errors.New(errors.ErrCodeNotFound, nicTopologyMsg("the cluster has no a3-megagpu-8g GPU nodes", remediationNoNodes))
	}

	var problems []string
	var verified []nodeTopology
	flaggedMissing := map[string]bool{}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		annotation := node.Annotations[gkenet.NICInfoAnnotation]
		if annotation == "" {
			unverifiedCount++
			slog.Warn("GPU node has no nic-info annotation; NIC topology unverified", "node", node.Name)
			continue
		}
		info, err := gkenet.ParseNICInfo(annotation)
		if err != nil {
			unverifiedCount++
			slog.Warn("GPU node nic-info annotation unparseable; NIC topology unverified", "node", node.Name, "error", err)
			continue
		}
		verified = append(verified, nodeTopology{name: node.Name, info: info})

		if len(info.ExtraInterfaces) > 0 {
			problems = append(problems, fmt.Sprintf("node %q has unexpected interface(s) beyond eth0..eth8: %s (gVNIC displacement signature)", node.Name, strings.Join(info.ExtraInterfaces, ",")))
		}
		if info.GPUNICInterfaces < gkenet.RequiredGPUNICInterfaces {
			problems = append(problems, fmt.Sprintf("node %q maps %d of %d GPU NIC interfaces (missing: %s)",
				node.Name, info.GPUNICInterfaces, gkenet.RequiredGPUNICInterfaces, strings.Join(info.SortedMissing(), ",")))
			flaggedMissing[node.Name] = true
		}
		if dev := info.ObservedGPUNICSlotDeviation(); len(dev) > 0 {
			slog.Warn("node GPU NIC PCI layout deviates from the observed a3 norm (informational only)", "node", node.Name, "deviation", dev)
		}
	}

	// Pool consistency: a node whose GPU-NIC PCI layout differs from the rest of
	// its pool is displaced. Only meaningful when >1 node was verified.
	problems = append(problems, poolConsistencyProblems(verified, flaggedMissing)...)
	if len(problems) > 0 {
		// The remediation depends on the failure kind: a pool-layout mismatch means
		// inspect the listed nodes, a displacement signature means re-provision without
		// a gVNIC additional network.
		remediation := remediationDisplacement
		for _, p := range problems {
			if strings.Contains(p, "distinct GPU NIC PCI layouts") || strings.Contains(p, "differs from the pool majority") {
				remediation = remediationPoolMismatch
				break
			}
		}
		return errors.New(errors.ErrCodeConflict, nicTopologyMsg(strings.Join(problems, "; "), remediation))
	}
	if len(verified) == 0 {
		return validators.Skip(fmt.Sprintf("could not verify NIC topology on any of the %d a3 GPU node(s) (%d unverified)", totalNodes, unverifiedCount))
	}
	if unverifiedCount > 0 {
		fmt.Printf("Verified NIC topology on %d a3 GPU node(s); %d node(s) could not be verified (no/invalid nic-info annotation)\n", len(verified), unverifiedCount)
		return nil
	}
	fmt.Printf("Verified NIC topology on %d a3 GPU node(s): all map %d GPU NIC interfaces (eth1..eth8)\n",
		len(verified), gkenet.RequiredGPUNICInterfaces)
	return nil
}

// nicTopologyMsg builds the operator-facing message for a NIC-topology failure
// (case 1). detail names what was observed; remediation is specific to the failure
// kind (displacement, pool mismatch, or absent nodes) — not a constant blame.
func nicTopologyMsg(detail, remediation string) string {
	return fmt.Sprintf(
		"recipe declares %s but %s — GPUDirect TCPXO needs all 8 GPU NIC PCI slots mapped to "+
			"eth1..eth8. %s Verify with: kubectl get node <gpu-node> -o jsonpath='{.metadata.annotations.networking\\.gke\\.io/nic-info}' "+
			"(see docs/integrator/gke-tcpxo-networking.md)",
		tcpxoComponent, detail, remediation)
}

const (
	remediationDisplacement = "Re-provision the GPU node pool WITHOUT a gVNIC additional network (it takes a GPU NIC PCI slot)."
	remediationPoolMismatch = "Inspect nic-info on the listed nodes; their GPU NIC PCI layout differs from the pool (a displaced or heterogeneous node)."
	remediationNoNodes      = "No a3-megagpu-8g nodes were found — check the GPU pool exists, is not scaled to zero, and carries cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb."
)

// poolConsistencyProblems groups verified nodes by their GPU-NIC PCI layout. A
// node whose layout differs from the rest of its pool is displaced — but only
// when one layout is a strict majority (else we can't tell which side is wrong
// and must not blame). Nodes already flagged for missing interfaces are skipped
// so they are not reported twice. Returns the problem lines ("" when consistent).
func poolConsistencyProblems(verified []nodeTopology, flaggedMissing map[string]bool) []string {
	if len(verified) < 2 {
		return nil
	}
	byLayout := map[string][]string{}
	for _, nt := range verified {
		if flaggedMissing[nt.name] {
			continue
		}
		key := strings.Join(nt.info.GPUNICPCIs, ",")
		byLayout[key] = append(byLayout[key], nt.name)
	}
	if len(byLayout) < 2 {
		return nil
	}
	// Find a strict-majority layout, if any.
	majority := ""
	for layout, members := range byLayout {
		if len(members) > len(verified)/2 {
			majority = layout
			break
		}
	}
	var problems []string
	if majority != "" {
		for layout, members := range byLayout {
			if layout == majority {
				continue
			}
			for _, name := range members {
				problems = append(problems, fmt.Sprintf("node %q GPU NIC PCI layout differs from the pool majority (%d of %d nodes)", name, len(byLayout[majority]), len(verified)))
			}
		}
		return problems
	}
	var layouts []string
	for layout, members := range byLayout {
		layouts = append(layouts, fmt.Sprintf("{%s}: nodes %s", layout, strings.Join(members, ",")))
	}
	sort.Strings(layouts)
	problems = append(problems, "pool has "+strconv.Itoa(len(byLayout))+" distinct GPU NIC PCI layouts (no majority to blame): "+strings.Join(layouts, "; "))
	return problems
}

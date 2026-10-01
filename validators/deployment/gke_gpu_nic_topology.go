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

	listCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(listCtx, metav1.ListOptions{LabelSelector: gkeAcceleratorLabel})
	if err != nil {
		// Mirror the sibling's probe-error classification (RBAC/timeout/transport get
		// the matching typed code, never masquerading as inapplicable).
		capability := validators.Capability{Component: tcpxoComponent, Subject: "a3 GPU nodes (nodes)"}
		return capability.RequireList(err)
	}
	if len(nodes.Items) == 0 {
		// A recipe declaring gke-nccl-tcpxo expects a3 GPU nodes; an empty result on a
		// declared capability fails (never a vacuous pass or a skip), matching the
		// capability contract.
		return errors.New(errors.ErrCodeNotFound, nicTopologyMsg("the cluster has no a3-megagpu-8g GPU nodes"))
	}

	type nodeTopology struct {
		name string
		info *gkenet.NodeNICInfo
	}
	var problems []string
	var verified []nodeTopology
	unverified := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		annotation := node.Annotations[gkenet.NICInfoAnnotation]
		if annotation == "" {
			unverified++
			slog.Warn("GPU node has no nic-info annotation; NIC topology unverified", "node", node.Name)
			continue
		}
		info, err := gkenet.ParseNICInfo(annotation)
		if err != nil {
			unverified++
			slog.Warn("GPU node nic-info annotation unparseable; NIC topology unverified", "node", node.Name, "error", err)
			continue
		}
		verified = append(verified, nodeTopology{name: node.Name, info: info})

		for _, extra := range info.ExtraInterfaces {
			problems = append(problems, fmt.Sprintf("node %q has unexpected interface(s) %s beyond eth0..eth8 (gVNIC displacement signature)", node.Name, extra))
		}
		if info.GPUNICInterfaces < gkenet.RequiredGPUNICInterfaces {
			problems = append(problems, fmt.Sprintf("node %q maps %d of %d GPU NIC interfaces (missing: %s)",
				node.Name, info.GPUNICInterfaces, gkenet.RequiredGPUNICInterfaces, strings.Join(info.SortedMissing(), ",")))
		}
		if dev := info.ObservedGPUNICSlotDeviation(); len(dev) > 0 {
			slog.Warn("node GPU NIC PCI layout deviates from the observed a3 norm (informational only)", "node", node.Name, "deviation", dev)
		}
	}

	// Pool consistency: a node whose GPU-NIC PCI set differs from the rest of its
	// pool is displaced. Only meaningful when >1 node was verified.
	if len(verified) > 1 {
		reference := strings.Join(verified[0].info.GPUNICPCIs, ",")
		for _, nt := range verified[1:] {
			if got := strings.Join(nt.info.GPUNICPCIs, ","); got != reference {
				problems = append(problems, fmt.Sprintf("node %q GPU NIC PCI set (%v) differs from the pool (%v)", nt.name, nt.info.GPUNICPCIs, reference))
			}
		}
	}

	// Coverage is recorded on every exit path so the result states how much was
	// actually verified (validated / unverified / total).
	total := len(nodes.Items)
	defer func() {
		_ = validators.EmitExtra(map[string]string{
			topoKeyValidated:  strconv.Itoa(len(verified)),
			topoKeyUnverified: strconv.Itoa(unverified),
			topoKeyTotal:      strconv.Itoa(total),
		})
	}()

	if len(problems) > 0 {
		return errors.New(errors.ErrCodeConflict, nicTopologyMsg(strings.Join(problems, "; ")))
	}
	if len(verified) == 0 {
		return validators.Skip(fmt.Sprintf("could not verify NIC topology on any of the %d a3 GPU node(s) (%d unverified)", total, unverified))
	}
	if unverified > 0 {
		fmt.Printf("Verified NIC topology on %d a3 GPU node(s); %d node(s) could not be verified (no/invalid nic-info annotation)\n", len(verified), unverified)
		return nil
	}
	fmt.Printf("Verified NIC topology on %d a3 GPU node(s): all map %d GPU NIC interfaces (eth1..eth8)\n",
		len(verified), gkenet.RequiredGPUNICInterfaces)
	return nil
}

// nicTopologyMsg builds the operator-facing message for a NIC-topology failure
// (case 1). detail names what was observed; the remediation is constant.
func nicTopologyMsg(detail string) string {
	return fmt.Sprintf(
		"recipe declares %s but %s — GPUDirect TCPXO needs all 8 GPU NIC PCI slots mapped to "+
			"eth1..eth8. Re-provision the GPU node pool WITHOUT a gVNIC additional network (it takes "+
			"a GPU NIC PCI slot). Verify with: kubectl get node <gpu-node> -o jsonpath='{.metadata.annotations.networking\\.gke\\.io/nic-info}' "+
			"(see docs/integrator/gke-tcpxo-networking.md)",
		tcpxoComponent, detail)
}

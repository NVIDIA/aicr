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
	"log/slog"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
)

// gkeAcceleratorLabel selects a3-megagpu-8g GPU nodes on GKE — the nodes that
// must report 8 GPU NIC PCI mappings for TCPXO.
const gkeAcceleratorLabel = "cloud.google.com/gke-accelerator=nvidia-h100-mega-80gb"

// checkGKEGPUINCTopology is the case-1 arm of #2265: a GPU node whose gVNIC
// additional network displaced a GPU NIC PCI slot delivers only 7/8 GPUs to
// TCPXO while every Network object exists, so the census passes clean. Read each
// GPU node's networking.gke.io/nic-info annotation and fail closed on the
// displacement. This reads node annotations; the sibling gke-gpu-nic-networks
// check reads cluster-scoped Network CRs.
func checkGKEGPUINCTopology(ctx *validators.Context) error {
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

	nodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{LabelSelector: gkeAcceleratorLabel})
	if err != nil {
		// An RBAC/timeout/transport error is not evidence of inapplicability — block.
		return errors.Wrap(errors.ErrCodeInternal, "failed to list GPU nodes for NIC topology", err)
	}
	if len(nodes.Items) == 0 {
		// No a3 GPU nodes — nothing to check topology on (the census arm owns the
		// zero-network case). Not a topology failure.
		slog.Info("no a3-megagpu-8g GPU nodes found; nothing to check NIC topology on")
		return nil
	}

	checked := 0
	for i := range nodes.Items {
		node := &nodes.Items[i]
		annotation, ok := node.Annotations[gkenet.NICInfoAnnotation]
		if !ok || annotation == "" {
			// The annotation is absent — we cannot verify topology. Documented
			// limitation, not a failure: do not false-fail clusters that don't
			// populate it. (Confirmed live before relying on this signal.)
			slog.Warn("GPU node lacks the nic-info annotation; cannot verify NIC topology",
				"node", node.Name, "annotation", gkenet.NICInfoAnnotation)
			continue
		}
		checked++
		info, err := gkenet.ParseNICInfo(annotation)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to parse %s on node %q", gkenet.NICInfoAnnotation, node.Name), err)
		}
		if info.GPUNICInterfaces < gkenet.RequiredGPUNICInterfaces {
			return errors.New(errors.ErrCodeNotFound, nicTopologyMsg(fmt.Sprintf(
				"node %q maps %d of %d GPU NIC interfaces (missing: %s)", node.Name, info.GPUNICInterfaces,
				gkenet.RequiredGPUNICInterfaces, strings.Join(info.MissingGPUNICs, ","))))
		}
	}
	if checked == 0 {
		fmt.Printf("Could not verify NIC topology: no GPU node carries the %s annotation (documented limitation)\n", gkenet.NICInfoAnnotation)
		return nil
	}
	fmt.Printf("Verified NIC topology on %d GPU node(s): all map %d GPU NIC interfaces (eth1..eth8)\n",
		checked, gkenet.RequiredGPUNICInterfaces)
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

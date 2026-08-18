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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
)

// tcpxoComponent is the recipe componentRef that supplies GPUDirect TCPXO.
const tcpxoComponent = "gke-nccl-tcpxo"

// checkGKEGPUNICNetworks verifies the cluster has the GKE multi-NIC networking
// objects GPUDirect TCPXO depends on.
//
// The gke-nccl-tcpxo component ships two DaemonSets, and both roll out cleanly
// on a cluster that has zero Network / GKENetworkParamSet objects — so the
// component's health check reports Synced+Healthy while TCPXO cannot function.
// Without this check the gap surfaces hours later as a performance-phase abort
// in the NCCL benchmark's own discovery, with no bandwidth number produced.
//
// The Network CRs are infrastructure: creating and binding them belongs to
// cluster provisioning, not AICR. This check only detects their absence and
// names the prerequisite, at the deployment phase where it is actionable.
func checkGKEGPUNICNetworks(ctx *validators.Context) error {
	if ctx.DynamicClient == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "dynamic client is not available")
	}

	slog.Info("listing GKE networks", "gvr", gkenet.NetworkGVR.String())

	gpuNICs, listErr := gkenet.DiscoverGPUNICNetworks(ctx.Ctx, ctx.DynamicClient)

	// Applicability gate (#2122). A List error NEVER skips — an RBAC denial or
	// an apiserver hiccup is not evidence that TCPXO is inapplicable. Note the
	// CRD being absent entirely surfaces here as a NotFound-shaped list error
	// on a non-served group-version, which RequireList also blocks on.
	if err := (validators.Capability{
		Component: tcpxoComponent,
		Subject:   "GKE Networks (networks.networking.gke.io)",
	}).RequireList(listErr); err != nil {
		return err
	}

	// The prerequisite belongs to gke-nccl-tcpxo: a recipe that does not declare
	// the component is not asking for TCPXO, so its cluster's networking is not
	// this check's business. This also covers the #1327 standalone-run boundary,
	// where there is no recipe context at all. Placed AFTER RequireList so an
	// infrastructure error still blocks on an undeclared recipe.
	if !validators.RecipeDeclares(ctx, tcpxoComponent) {
		return validators.Skip(
			tcpxoComponent + " not declared in recipe — GPUDirect TCPXO networking is inapplicable")
	}

	// Evidence to stdout.
	fmt.Printf("Found %d GPU NIC network(s) (need %d):\n", len(gpuNICs), gkenet.RequiredGPUNICNetworks)
	for _, name := range gpuNICs {
		fmt.Printf("  %s\n", name)
	}

	if len(gpuNICs) < gkenet.RequiredGPUNICNetworks {
		return errors.New(errors.ErrCodeNotFound, fmt.Sprintf(
			"recipe declares %s but the cluster has %d of %d GPU NIC networks — "+
				"GPUDirect TCPXO requires Network and GKENetworkParamSet objects bound into the cluster. "+
				"These are provisioned with the cluster, not by AICR, and multi-networking cannot be "+
				"enabled after cluster creation. Verify with: kubectl get network.networking.gke.io "+
				"(see docs/integrator/gke-tcpxo-networking.md)",
			tcpxoComponent, len(gpuNICs), gkenet.RequiredGPUNICNetworks))
	}

	return nil
}

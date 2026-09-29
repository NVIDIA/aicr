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
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	capability := validators.Capability{
		Component: tcpxoComponent,
		Subject:   "GKE Networks (networks.networking.gke.io)",
		AbsentMsg: absentPrerequisiteMsg("the cluster does not serve the networks.networking.gke.io " +
			"API at all, so it has 0"),
		InapplicableMsg: tcpxoComponent + " not declared in recipe and the cluster has no GKE Network " +
			"API — cluster does not use GPUDirect TCPXO",
	}

	// An ABSENT Network API is clean absence, not an infrastructure failure: the
	// CRD arrives with --enable-multi-networking, so a cluster created without it
	// legitimately does not serve this GVR. Route that shape through Require,
	// which is declaration-gated — an undeclared recipe skips, a declared one gets
	// the actionable message. RequireList would classify it as a blocking INTERNAL
	// error, which both false-fails an undeclared recipe and hides the missing
	// prerequisite behind "failed to read" on a declared one.
	if apierrors.IsNotFound(listErr) {
		// present is unused here: Require consults it only when probeErr is nil.
		return capability.Require(ctx, listErr, false)
	}

	// Every other list error blocks regardless of declaration — an RBAC denial or
	// an apiserver hiccup is not evidence that TCPXO is inapplicable.
	if err := capability.RequireList(listErr); err != nil {
		return err
	}

	// The prerequisite belongs to gke-nccl-tcpxo: a recipe that does not declare
	// the component is not asking for TCPXO, so its cluster's networking is not
	// this check's business. This also covers the #1327 standalone-run boundary,
	// where there is no recipe context at all.
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
		return errors.New(errors.ErrCodeNotFound, absentPrerequisiteMsg(fmt.Sprintf(
			"the cluster has %d of %d", len(gpuNICs), gkenet.RequiredGPUNICNetworks)))
	}

	// Case 2 (#2265): an existing-but-unready or mis-bound Network must not count
	// toward the census — the fabric is unusable until every gpu-nic Network is
	// Ready and its GKENetworkParamSet binding is intact.
	if err := verifyNetworkReadinessAndBinding(ctx); err != nil {
		return err
	}

	return verifyDeliveredRuntimeWiring(ctx, gpuNICs)
}

// verifyDeliveredRuntimeWiring is the runtime-specific arm of this check
// (#2297): when the recipe ships the torch-distributed-tcpxo runtime with a
// recorded GPU-NIC mapping, the DEPLOYED ClusterTrainingRuntime must carry
// exactly that mapping, and every network it selects must exist on this
// cluster. Both are failures, not findings — the first catches the generated
// artifact being modified outside AICR, the second catches a recipe value that
// is well-formed but wrong for this cluster.
//
// It is gated on the recipe-derived predicate, not on gke-nccl-tcpxo being
// declared: base h100-gke-cos-training is a supported recipe that has TCPXO but
// ships no Kubeflow runtime and records no mapping, and an unconditional
// extension would false-fail it. The same primitives run in the performance
// validator before it derives its benchmark, so `--phase performance` does not
// depend on this phase having run.
func verifyDeliveredRuntimeWiring(ctx *validators.Context, gpuNICs []string) error {
	var refs []recipe.ComponentRef
	if ctx.ValidationInput != nil {
		refs = ctx.ValidationInput.ComponentRefs
	}
	recorded, delivered, err := gkenet.FabricRuntimeDelivered(refs)
	if err != nil {
		return err
	}
	if !delivered {
		return nil
	}
	shipped, err := gkenet.ReadDeployedTCPXORuntime(ctx.Ctx, ctx.DynamicClient)
	if err != nil {
		return err
	}
	deployed, err := gkenet.DeployedTCPXOMapping(shipped)
	if err != nil {
		return err
	}
	if err := gkenet.VerifyMappingMatchesRecipe(recorded, deployed); err != nil {
		return err
	}
	if err := gkenet.VerifyNetworksExist(deployed, gpuNICs); err != nil {
		return err
	}
	fmt.Printf("Deployed %s carries the recipe's %d-interface GPU NIC mapping, and every selected network exists on the cluster\n",
		gkenet.TCPXORuntimeName, len(deployed))
	return nil
}

// verifyNetworkReadinessAndBinding is the case-2 arm (#2265): a Network that
// exists but is not Ready, or whose GKENetworkParamSet binding is broken, means
// the fabric is unusable even though the census found eight names. Fail closed,
// naming the network and the remediation. A discovery error blocks (never skips).
//
// Enabling assumption: Networks are Ready by the time the deployment phase runs
// (post-install). A Network still mid-provisioning at validate time fails closed
// here — intended, but it means a slow-to-bind Network surfaces as a deployment
// failure rather than a retry.
func verifyNetworkReadinessAndBinding(ctx *validators.Context) error {
	statuses, err := gkenet.DiscoverGPUNICNetworkStatus(ctx.Ctx, ctx.DynamicClient)
	if err != nil {
		// The census's list already succeeded, so a read error here is an apiserver
		// hiccup, not evidence of inapplicability — block, never skip.
		return errors.Wrap(errors.ErrCodeInternal, "failed to read GKE Network readiness/binding status", err)
	}
	for _, st := range statuses {
		if !st.Ready {
			return errors.New(errors.ErrCodeNotFound, networkCapabilityMsg(fmt.Sprintf(
				"Network %q is not Ready (%s)", st.Name, st.Detail)))
		}
		if !st.ParamsReady {
			return errors.New(errors.ErrCodeNotFound, networkCapabilityMsg(fmt.Sprintf(
				"Network %q binding to GKENetworkParamSet %q is not ready (%s)", st.Name, st.ParamSetName, st.Detail)))
		}
	}
	return nil
}

// networkCapabilityMsg builds the operator-facing message for an existing-but-
// unusable Network (case 2). detail names what was observed; the remediation is
// the constant contract.
func networkCapabilityMsg(detail string) string {
	return fmt.Sprintf(
		"recipe declares %s and the cluster has the GPU NIC networks, but %s — the Network and its "+
			"GKENetworkParamSet must both exist and be Ready for GPUDirect TCPXO to function. "+
			"These are provisioned with the cluster, not by AICR. "+
			"Inspect with: kubectl get network.networking.gke.io <name> -o yaml (status.conditions Ready/ParamsReady) "+
			"and kubectl get gkenetworkparamset.networking.gke.io (see docs/integrator/gke-tcpxo-networking.md)",
		tcpxoComponent, detail)
}

// absentPrerequisiteMsg builds the operator-facing message for a missing GPU NIC
// networking prerequisite. detail names what was actually observed; the rest is
// the constant remediation, kept in one place so the absent-API path and the
// short-count path cannot drift.
//
// The message names the required naming convention because it is a real way to
// hit a zero count on an otherwise correctly provisioned cluster: discovery
// matches the substring against the NETWORK name only, and Google's own sample
// manifests name the Device networks vpc1..vpc8.
func absentPrerequisiteMsg(detail string) string {
	return fmt.Sprintf(
		"recipe declares %s but %s GPU NIC networks — GPUDirect TCPXO requires one Network "+
			"per GPU NIC, each bound to a GKENetworkParamSet and each with %q in its own "+
			"metadata.name (this check counts Network names; it does not verify the "+
			"GKENetworkParamSet binding or readiness). "+
			"These are provisioned with the cluster, not by AICR, and multi-networking "+
			"(--enable-multi-networking) cannot be enabled after cluster creation. "+
			"Verify with: kubectl get network.networking.gke.io "+
			"(see docs/integrator/gke-tcpxo-networking.md)",
		tcpxoComponent, detail, recipe.GPUNICNameSubstring)
}

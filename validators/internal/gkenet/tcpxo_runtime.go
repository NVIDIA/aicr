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

package gkenet

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// This file holds the pieces of the #2297 shipped-runtime contract that both
// validator images need: the deployment phase compares the recipe's recorded
// mapping against the deployed ClusterTrainingRuntime and the live cluster, and
// the performance phase runs the same comparison before deriving its benchmark
// from that runtime. Keeping them here — a package both images already import —
// is what lets `aicr validate --phase performance` verify recipe -> runtime ->
// cluster on its own instead of trusting that the deployment phase ran first.

const (
	// TCPXORuntimeName is the ClusterTrainingRuntime the kubeflow-trainer
	// component ships for GPUDirect-TCPXO (torch-distributed-tcpxo-cluster-
	// training-runtime.yaml). It is cluster-scoped, so there is exactly one.
	TCPXORuntimeName = "torch-distributed-tcpxo"

	// InterfacesAnnotation carries the ordered GKE multi-NIC mapping on the
	// worker pod template: eth0 -> default first, then eth1..eth8 -> GPU NICs.
	InterfacesAnnotation = "networking.gke.io/interfaces"
	// DefaultInterfaceAnnotation names the pod's primary interface (eth0).
	DefaultInterfaceAnnotation = "networking.gke.io/default-interface"

	// TCPXONodeJob is the replicatedJob the shipped runtime places its worker
	// pod template under. It is the same name the benchmark runtime uses, which
	// is what makes the derivation a template swap rather than a rename.
	TCPXONodeJob = "node"

	defaultInterface = "eth0"
	defaultNetwork   = "default"
)

// ClusterTrainingRuntimeGVR addresses Kubeflow Trainer's cluster-scoped runtime
// catalog. The namespaced sibling (trainingruntimes) is what the benchmark
// creates for itself; this is what the recipe ships.
var ClusterTrainingRuntimeGVR = schema.GroupVersionResource{
	Group: "trainer.kubeflow.org", Version: "v1alpha1", Resource: "clustertrainingruntimes",
}

// FabricRuntimeDelivered reports whether the recipe ships a fabric-wired runtime,
// and returns the eth1..eth8 -> network mapping it recorded for it.
//
// It is answered from the recipe alone and needs BOTH halves #2297 names: the
// enabled kubeflow-trainer componentRef must declare the TCPXO runtime manifest
// (the same test recipe.ShipsGKETCPXORuntime applies — a mapping without the
// artifact describes nothing) AND carry the typed tcpxoInterfaces override. It
// is never answered from finding a live runtime, because a stray runtime must
// not change who owns the evidence. Declaring kubeflow-trainer is NOT
// sufficient: the platform-kubeflow mixin ships only torch-distributed, so
// every kubeflow leaf except h100-gke-cos-training-kubeflow returns
// (nil, false, nil). A recorded override that fails to normalize is an error,
// not "not delivered": the recipe claims a runtime it cannot describe, and
// silently downgrading that to the capability-fixture path would hide exactly
// the divergence this exists to catch.
func FabricRuntimeDelivered(refs []recipe.ComponentRef) ([]recipe.NetworkInterfaceMapping, bool, error) {
	for i := range refs {
		ref := &refs[i]
		if ref.Name != recipe.KubeflowTrainerComponentName || !ref.IsEnabled() {
			continue
		}
		if !slices.Contains(ref.ManifestFiles, recipe.GKETCPXORuntimeManifest) {
			// Overrides without the runtime manifest (an external --data recipe
			// that copied the mapping but not the artifact) do not ship anything.
			return nil, false, nil
		}
		raw, ok := ref.Overrides[recipe.GKETCPXOInterfacesOverrideKey]
		if !ok {
			return nil, false, nil
		}
		mapping, err := recipe.NormalizeGKETCPXOInterfaces(raw)
		if err != nil {
			return nil, false, errors.Wrap(errors.ErrCodeInvalidRequest,
				"recipe records a "+recipe.GKETCPXOInterfacesOverrideKey+" override on "+
					recipe.KubeflowTrainerComponentName+" that does not normalize", err)
		}
		return mapping, true, nil
	}
	return nil, false, nil
}

// ReadDeployedTCPXORuntime fetches the shipped ClusterTrainingRuntime from the
// live API. The manifest in the repo is a Helm template and cannot be parsed at
// validation time; reading the deployed object is also what turns the derived
// benchmark into evidence about what was actually installed rather than about
// the recipe author's intent. A NotFound is returned unwrapped-by-code as
// ErrCodeNotFound so callers can distinguish "not deployed" from an apiserver
// fault.
func ReadDeployedTCPXORuntime(ctx context.Context, dyn dynamic.Interface) (*unstructured.Unstructured, error) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	obj, err := dyn.Resource(ClusterTrainingRuntimeGVR).Get(getCtx, TCPXORuntimeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("the recipe ships ClusterTrainingRuntime %q but it is not deployed", TCPXORuntimeName), err)
		}
		return nil, errors.Wrap(ReadErrorCode(err),
			fmt.Sprintf("failed to read ClusterTrainingRuntime %q", TCPXORuntimeName), err)
	}
	return obj, nil
}

// ReadErrorCode classifies a non-NotFound Kubernetes read failure: an expired
// deadline is ErrCodeTimeout and an operator abort is ErrCodeCanceled — both
// are outcomes of the run's context, not product faults — and anything else
// is ErrCodeInternal. Callers already handle NotFound before reaching this.
func ReadErrorCode(err error) errors.ErrorCode {
	switch {
	case stderrors.Is(err, context.DeadlineExceeded):
		return errors.ErrCodeTimeout
	case stderrors.Is(err, context.Canceled):
		return errors.ErrCodeCanceled
	default:
		return errors.ErrCodeInternal
	}
}

// NodeTemplateOf returns the shipped runtime's worker PodTemplateSpec — the
// "node" replicatedJob's template.spec.template — as a map holding both
// "metadata" and "spec". Both halves matter: the GKE fabric annotations live in
// template.metadata, before spec begins, so a caller that copied only the
// PodSpec would drop the wiring entirely (Kubeflow models the two separately in
// PodTemplatePatch for the same reason). The returned map is a deep copy
// (unstructured.NestedMap copies), so mutating it does not change obj; callers
// that need to mutate the runtime must write the map back.
func NodeTemplateOf(obj *unstructured.Unstructured) (map[string]any, error) {
	jobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("ClusterTrainingRuntime %q has no spec.template.spec.replicatedJobs", obj.GetName()))
	}
	for _, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(job, "name"); name != TCPXONodeJob {
			continue
		}
		tmpl, ok, err := unstructured.NestedMap(job, "template", "spec", "template")
		if err != nil || !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("ClusterTrainingRuntime %q: %q replicatedJob has no template.spec.template", obj.GetName(), TCPXONodeJob))
		}
		return tmpl, nil
	}
	return nil, errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("ClusterTrainingRuntime %q declares no %q replicatedJob", obj.GetName(), TCPXONodeJob))
}

// DeployedTCPXOMapping extracts the eth1..eth8 -> network mapping the deployed
// runtime will actually give its workers, from the interfaces annotation on the
// node template. It is the "deployed" leg of the three-way comparison.
func DeployedTCPXOMapping(obj *unstructured.Unstructured) ([]recipe.NetworkInterfaceMapping, error) {
	tmpl, err := NodeTemplateOf(obj)
	if err != nil {
		return nil, err
	}
	ann, _, _ := unstructured.NestedStringMap(tmpl, "metadata", "annotations")
	raw, ok := ann[InterfacesAnnotation]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("ClusterTrainingRuntime %q node template carries no %s annotation — the deployed runtime is not fabric-wired",
				obj.GetName(), InterfacesAnnotation))
	}
	if def := ann[DefaultInterfaceAnnotation]; def != defaultInterface {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("ClusterTrainingRuntime %q node template has %s=%q, want %q",
				obj.GetName(), DefaultInterfaceAnnotation, def, defaultInterface))
	}
	return ParseInterfacesAnnotation(raw)
}

// ParseInterfacesAnnotation decodes a networking.gke.io/interfaces value and
// returns the secondary (eth1..eth8) entries as a mapping, after asserting the
// leading eth0 -> default entry. It does not re-validate the mapping's shape
// beyond that; callers compare it against the recipe's already-validated value
// (VerifyMappingMatchesRecipe) or run recipe.ValidateGKETCPXOInterfaces.
func ParseInterfacesAnnotation(raw string) ([]recipe.NetworkInterfaceMapping, error) {
	var entries []recipe.NetworkInterfaceMapping
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, InterfacesAnnotation+" annotation is not valid JSON", err)
	}
	if len(entries) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, InterfacesAnnotation+" annotation is empty")
	}
	if entries[0].InterfaceName != defaultInterface || entries[0].Network != defaultNetwork {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s annotation first entry = %s->%s, want %s->%s",
				InterfacesAnnotation, entries[0].InterfaceName, entries[0].Network, defaultInterface, defaultNetwork))
	}
	return entries[1:], nil
}

// VerifyMappingMatchesRecipe is the recipe-vs-deployed leg: the deployed
// runtime must carry EXACTLY the ordered mapping the recipe recorded. Order is
// part of the contract — ethN is bound to the Nth GPU NIC — so this is an
// element-wise comparison, and any difference is a failure rather than a
// recorded finding. #2296's bundler-side rejection prevents this in-band; this
// arm catches the generated artifact being modified outside AICR.
func VerifyMappingMatchesRecipe(recorded, deployed []recipe.NetworkInterfaceMapping) error {
	if len(recorded) != len(deployed) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("deployed %s runtime maps %d GPU NIC interfaces but the recipe records %d; regenerate the bundle from the recipe",
				TCPXORuntimeName, len(deployed), len(recorded)))
	}
	for i := range recorded {
		if recorded[i] != deployed[i] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("deployed %s runtime maps %s->%s at position %d but the recipe records %s->%s; the deployed artifact diverges from the recipe",
					TCPXORuntimeName, deployed[i].InterfaceName, deployed[i].Network, i+1,
					recorded[i].InterfaceName, recorded[i].Network))
		}
	}
	return nil
}

// VerifyNetworksExist is the deployed-vs-cluster leg: every network the runtime
// selects must exist on this cluster. It is an explicit SET comparison, never
// index-wise: DiscoverGPUNICNetworks returns names sorted alphabetically, which
// happens to match numeric order for eight single-digit suffixes today and
// would silently break on any naming that does not — so the discovered list is
// treated as a set, and no ordering is inferred from it.
func VerifyNetworksExist(deployed []recipe.NetworkInterfaceMapping, discovered []string) error {
	have := make(map[string]struct{}, len(discovered))
	for _, n := range discovered {
		have[n] = struct{}{}
	}
	var missing []string
	for _, m := range deployed {
		if _, ok := have[m.Network]; !ok {
			missing = append(missing, m.InterfaceName+"->"+m.Network)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("deployed %s runtime selects GPU NIC networks that do not exist on this cluster: %s (cluster has: %s)",
				TCPXORuntimeName, strings.Join(missing, ", "), strings.Join(discovered, ", ")))
	}
	return nil
}

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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Resource prefixes applied when a manifest does not carry one:
//   - k8s-rdma-shared-dev-plugin defaults to "rdma"
//     (github.com/Mellanox/k8s-rdma-shared-dev-plugin, rdmaHcaResourcePrefix).
//   - sriov-network-device-plugin inside a NicClusterPolicy defaults to
//     "intel.com" (github.com/k8snetworkplumbingwg/sriov-network-device-plugin,
//     resourcePrefix).
//   - SriovNetworkNodePolicy resources are prefixed by the SR-IOV Network
//     Operator, which the network-operator chart pins to "nvidia.com"
//     (sriov-network-operator.operator.resourcePrefix); the policy itself
//     carries only resourceName.
const (
	rdmaSharedDefaultResourcePrefix = "rdma"
	sriovDefaultResourcePrefix      = "intel.com"
	sriovOperatorResourcePrefix     = "nvidia.com"
)

// nicClusterPolicyDoc is the minimal NicClusterPolicy shape the fabric gate
// needs: which device-plugin block(s) the policy deploys and their embedded
// plugin config. Both configs arrive as JSON strings, verbatim from the
// plugin's own config-file format.
type nicClusterPolicyDoc struct {
	Kind string `json:"kind"`
	Spec struct {
		RdmaSharedDevicePlugin *nicDevicePluginSpec `json:"rdmaSharedDevicePlugin"`
		SriovDevicePlugin      *nicDevicePluginSpec `json:"sriovDevicePlugin"`
	} `json:"spec"`
}

type nicDevicePluginSpec struct {
	Config string `json:"config"`
}

// nicResourceEntry is the shared shape of one resource declaration inside
// either plugin's JSON config.
type nicResourceEntry struct {
	ResourcePrefix string `json:"resourcePrefix"`
	ResourceName   string `json:"resourceName"`
}

// sriovNetworkNodePolicyDoc is the minimal SriovNetworkNodePolicy shape the
// fabric gate needs: the resource the SR-IOV Network Operator's device plugin
// advertises for the VFs the policy creates.
type sriovNetworkNodePolicyDoc struct {
	Kind string `json:"kind"`
	Spec struct {
		ResourceName string `json:"resourceName"`
	} `json:"spec"`
}

// rdmaFabricResources derives the extended-resource names the recipe's fabric
// actually advertises, by rendering the relevant manifests with each
// component's effective values and reading the resource declarations:
//   - the device-plugin config embedded in ncpRef's NicClusterPolicy spec
//     (rdmaSharedDevicePlugin on AKS "rdma/hca_shared_devices_a" and OKE GB200
//     "nvidia.com/mlnxnics"; sriovDevicePlugin on OKE L40S "nvidia.com/mlnxnics");
//   - every SriovNetworkNodePolicy any component in refs attaches (NVKE GB200,
//     one resource per IB rail), which the NicClusterPolicy does not mention
//     because the SR-IOV Network Operator owns that device plugin.
//
// Deriving the resources from the manifests — instead of hardcoding a
// per-provider constant — keeps verifyRDMAFabricReady polling for exactly what
// the recipe's own fabric will publish, so a new provider or an edited
// resourceName can never reintroduce the poll-forever mismatch (#2356 review).
// TestRDMAFabricResources_RealManifests pins the AKS manifest's parse to
// helper.AKSRdmaSharedResource so this parser and the NCCL consumer's constant
// cannot drift.
//
// Fail-closed contract: any read, render, or parse failure is an error, and so
// is a marker-matched manifest set that declares no resource at all. "Could not
// derive the fabric resource" must never degrade into "skip the fabric gate".
// The result is sorted and de-duplicated; the gate polls each entry.
func rdmaFabricResources(goCtx context.Context, ncpRef recipe.ComponentRef, refs []recipe.ComponentRef) ([]string, error) {
	resources := map[string]struct{}{}

	for _, path := range ncpRef.ManifestFiles {
		if !strings.Contains(path, nicClusterPolicyManifestMarker) {
			continue
		}
		rendered, err := renderComponentManifest(goCtx, ncpRef, path, "NicClusterPolicy")
		if err != nil {
			return nil, err
		}
		found, perr := nicClusterPolicyResources(rendered)
		if perr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to parse NicClusterPolicy manifest %s for component %s", path, ncpRef.Name), perr)
		}
		for _, r := range found {
			resources[r] = struct{}{}
		}
	}

	for _, ref := range refs {
		for _, path := range ref.ManifestFiles {
			if !strings.Contains(path, sriovNodePolicyManifestMarker) {
				continue
			}
			rendered, err := renderComponentManifest(goCtx, ref, path, "SriovNetworkNodePolicy")
			if err != nil {
				return nil, err
			}
			found, perr := sriovNodePolicyResources(rendered)
			if perr != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to parse SriovNetworkNodePolicy manifest %s for component %s", path, ref.Name), perr)
			}
			for _, r := range found {
				resources[r] = struct{}{}
			}
		}
	}

	if len(resources) == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("component %s declares a NicClusterPolicy manifest but no device-plugin "+
				"resource could be derived from it or from any SriovNetworkNodePolicy manifest; "+
				"the RDMA fabric readiness gate cannot run", ncpRef.Name))
	}
	out := make([]string, 0, len(resources))
	for r := range resources {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

// renderComponentManifest loads one of ref's manifest files from the embedded
// data provider and renders it with the component's effective values. kind
// names the expected document in error messages only.
func renderComponentManifest(goCtx context.Context, ref recipe.ComponentRef, path, kind string) ([]byte, error) {
	select {
	case <-goCtx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"deployment validation canceled while deriving the RDMA fabric resource", goCtx.Err())
	default:
	}
	values, err := recipe.GetComponentValuesWithContext(goCtx, nil, &ref)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to resolve effective values for component %s", ref.Name), err)
	}
	chartName := ref.Chart
	if chartName == "" {
		chartName = ref.Name
	}
	content, err := recipe.GetManifestContentWithContext(goCtx, nil, path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to load %s manifest %s for component %s", kind, path, ref.Name), err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: ref.Name,
		Namespace:     ref.Namespace,
		ChartName:     chartName,
		ChartVersion:  ref.Version,
		Values:        values,
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to render %s manifest %s for component %s", kind, path, ref.Name), err)
	}
	return rendered, nil
}

// nicClusterPolicyResources extracts every "<prefix>/<name>" extended resource
// declared by the NicClusterPolicy document(s) in rendered manifest content.
// Non-NicClusterPolicy documents are skipped; a document that fails to decode
// is an error (fail closed — a malformed policy must not read as "no fabric").
func nicClusterPolicyResources(rendered []byte) ([]string, error) {
	var out []string
	for _, doc := range strings.Split(string(rendered), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var ncp nicClusterPolicyDoc
		if err := yaml.Unmarshal([]byte(doc), &ncp); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode manifest document", err)
		}
		if ncp.Kind != "NicClusterPolicy" {
			continue
		}
		if p := ncp.Spec.RdmaSharedDevicePlugin; p != nil {
			var cfg struct {
				ConfigList []nicResourceEntry `json:"configList"`
			}
			if err := json.Unmarshal([]byte(p.Config), &cfg); err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					"failed to decode rdmaSharedDevicePlugin config JSON", err)
			}
			for _, e := range cfg.ConfigList {
				r, rerr := qualifiedNICResource(e, rdmaSharedDefaultResourcePrefix)
				if rerr != nil {
					return nil, rerr
				}
				out = append(out, r)
			}
		}
		if p := ncp.Spec.SriovDevicePlugin; p != nil {
			var cfg struct {
				ResourceList []nicResourceEntry `json:"resourceList"`
			}
			if err := json.Unmarshal([]byte(p.Config), &cfg); err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					"failed to decode sriovDevicePlugin config JSON", err)
			}
			for _, e := range cfg.ResourceList {
				r, rerr := qualifiedNICResource(e, sriovDefaultResourcePrefix)
				if rerr != nil {
					return nil, rerr
				}
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// sriovNodePolicyResources extracts the "<prefix>/<name>" extended resource of
// every SriovNetworkNodePolicy document in rendered manifest content. Other
// documents (the IPPool and SriovIBNetwork that accompany a policy) are
// skipped; a policy without resourceName is an error (fail closed).
func sriovNodePolicyResources(rendered []byte) ([]string, error) {
	var out []string
	for _, doc := range strings.Split(string(rendered), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var pol sriovNetworkNodePolicyDoc
		if err := yaml.Unmarshal([]byte(doc), &pol); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to decode manifest document", err)
		}
		if pol.Kind != "SriovNetworkNodePolicy" {
			continue
		}
		if pol.Spec.ResourceName == "" {
			return nil, errors.New(errors.ErrCodeInternal,
				"SriovNetworkNodePolicy declares no resourceName")
		}
		out = append(out, sriovOperatorResourcePrefix+"/"+pol.Spec.ResourceName)
	}
	return out, nil
}

// qualifiedNICResource joins a config entry into the "<prefix>/<name>"
// extended-resource form, applying the plugin's documented default prefix
// when the entry omits one. An entry with no resourceName is an error —
// "<prefix>/" is not a resource any plugin will ever advertise, and letting
// it through would make the readiness gate poll a phantom until timeout
// instead of naming the malformed config.
func qualifiedNICResource(e nicResourceEntry, defaultPrefix string) (string, error) {
	if e.ResourceName == "" {
		return "", errors.New(errors.ErrCodeInternal,
			"device-plugin config entry declares no resourceName")
	}
	prefix := e.ResourcePrefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	return prefix + "/" + e.ResourceName, nil
}

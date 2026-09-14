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

// Upstream device-plugin default resource prefixes, applied when a
// NicClusterPolicy config entry omits resourcePrefix. Pinned here so a
// prefix-less config still derives the exact resource name the plugin will
// advertise:
//   - k8s-rdma-shared-dev-plugin defaults to "rdma"
//     (github.com/Mellanox/k8s-rdma-shared-dev-plugin, rdmaHcaResourcePrefix).
//   - sriov-network-device-plugin defaults to "intel.com"
//     (github.com/k8snetworkplumbingwg/sriov-network-device-plugin,
//     resourcePrefix).
const (
	rdmaSharedDefaultResourcePrefix = "rdma"
	sriovDefaultResourcePrefix      = "intel.com"
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

// rdmaFabricResource derives the extended-resource name the recipe's
// NicClusterPolicy actually advertises, by rendering the ComponentRef's
// NicClusterPolicy manifest(s) with the component's effective values and
// reading the device-plugin config embedded in the policy spec
// (rdmaSharedDevicePlugin on AKS "rdma/hca_shared_devices_a" and OKE GB200
// "nvidia.com/mlnxnics"; sriovDevicePlugin on OKE L40S "nvidia.com/mlnxnics").
//
// Deriving the resource from the manifest — instead of hardcoding a
// per-provider constant — keeps verifyRDMAFabricReady polling for exactly what
// the recipe's own fabric will publish, so a new provider or an edited
// resourceName can never reintroduce the poll-forever mismatch (#2356 review).
// TestRDMAFabricResource_AKSPin pins the AKS manifest's parse to
// helper.AKSRdmaSharedResource so this parser and the NCCL consumer's constant
// cannot drift.
//
// Fail-closed contract: any read, render, or parse failure is an error, and so
// are a marker-matched manifest set that declares no device-plugin resource or
// one that declares more than one distinct resource (the gate polls a single
// uniform resource across the cohort). "Could not derive the fabric resource"
// must never degrade into "skip the fabric gate".
func rdmaFabricResource(goCtx context.Context, ref recipe.ComponentRef) (string, error) {
	values, err := recipe.GetComponentValuesWithContext(goCtx, nil, &ref)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to resolve effective values for component %s", ref.Name), err)
	}
	chartName := ref.Chart
	if chartName == "" {
		chartName = ref.Name
	}
	renderInput := manifest.RenderInput{
		ComponentName: ref.Name,
		Namespace:     ref.Namespace,
		ChartName:     chartName,
		ChartVersion:  ref.Version,
		Values:        values,
	}

	resources := map[string]struct{}{}
	for _, path := range ref.ManifestFiles {
		if !strings.Contains(path, nicClusterPolicyManifestMarker) {
			continue
		}
		select {
		case <-goCtx.Done():
			return "", errors.Wrap(errors.ErrCodeTimeout,
				"deployment validation canceled while deriving the RDMA fabric resource", goCtx.Err())
		default:
		}
		content, err := recipe.GetManifestContentWithContext(goCtx, nil, path)
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to load NicClusterPolicy manifest %s for component %s", path, ref.Name), err)
		}
		rendered, rerr := manifest.Render(content, renderInput)
		if rerr != nil {
			return "", errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to render NicClusterPolicy manifest %s for component %s", path, ref.Name), rerr)
		}
		found, perr := nicClusterPolicyResources(rendered)
		if perr != nil {
			return "", errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to parse NicClusterPolicy manifest %s for component %s", path, ref.Name), perr)
		}
		for _, r := range found {
			resources[r] = struct{}{}
		}
	}

	switch len(resources) {
	case 1:
		for r := range resources {
			return r, nil
		}
		panic("unreachable")
	case 0:
		return "", errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("component %s declares a NicClusterPolicy manifest but no device-plugin "+
				"resource could be derived from it; the RDMA fabric readiness gate cannot run", ref.Name))
	default:
		names := make([]string, 0, len(resources))
		for r := range resources {
			names = append(names, r)
		}
		sort.Strings(names)
		return "", errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("component %s declares multiple distinct RDMA fabric resources %v; "+
				"the readiness gate polls a single uniform resource and cannot arbitrate", ref.Name, names))
	}
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

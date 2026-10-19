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

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// SubtypeOKEAddons is the K8s measurement subtype carrying the projection
// of an `oci ce cluster list-addons` dump (see providerpools.go for the
// per-provider projection contract this follows).
const SubtypeOKEAddons = "oke-addons"

// okeNvidiaGPUPluginAddonName is the OKE cluster add-on that auto-installs
// NVIDIA's device plugin. Its control-plane state is the canonical
// gpuStack qualification signal for the OKE family: present and ACTIVE
// means Oracle's plugin owns nvidia.com/gpu (oci-managed); absent means
// the pools are operator-managed. Per-node label disablement
// (oci.oraclecloud.com/disable-gpu-device-plugin) is out of contract —
// it leaves the add-on installed, which this projection reports as
// installed.
const okeNvidiaGPUPluginAddonName = "NvidiaGpuPlugin"

// okeNvidiaNetworkOperatorAddonName is the opt-in OKE add-on that installs
// NVIDIA's Network Operator, which reconciles the same NicClusterPolicy as
// AICR's network-operator component.
const okeNvidiaNetworkOperatorAddonName = "NvidiaNetworkOperator"

// okeAddonInstalled and okeAddonAbsent are the only add-on readings a
// declared constraint matches.
const (
	okeAddonInstalled = "installed"
	okeAddonAbsent    = "absent"
)

// okeAddon is the narrow shape read from each `data[]` entry. Unknown
// fields are intentionally ignored: the dump carries much more than the
// projection consumes, and rejecting unknown fields would couple AICR to
// the OCI CLI's output evolution.
type okeAddon struct {
	Name           string `json:"name"`
	LifecycleState string `json:"lifecycle-state"`
}

// okeAddonsDump is the top-level `oci ce cluster list-addons` envelope.
// The OCI CLI emits opc-next-page only when more pages remain, which is
// how a dump captured without --all is detected.
type okeAddonsDump struct {
	Data     []okeAddon `json:"data"`
	NextPage string     `json:"opc-next-page"`
}

// ProjectOKEAddons returns the oke-addons subtype for the
// `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`
// dump at path. It reports addon-count, plus the state of the NvidiaGpuPlugin
// (nvidia-gpu-plugin) and NvidiaNetworkOperator (nvidia-network-operator)
// add-ons as one of:
//
//   - installed: present with lifecycle-state ACTIVE.
//   - absent: not in the dump, e.g. NvidiaGpuPlugin removed with
//     terraform-oci-okecluster's NvidiaGpuPlugin = { remove = true }.
//   - addon-<state>: present in any other lifecycle state, lowercased
//     (e.g. addon-deleting). No declared constraint matches it, so
//     resolution fails closed naming the state.
//
// A missing, unreadable, malformed, or partially paginated file is an
// error, never a missing reading, so a bad path cannot steer a profile
// decision.
func ProjectOKEAddons(ctx context.Context, path string) (measurement.Subtype, error) {
	addons, err := readOKEAddons(ctx, path)
	if err != nil {
		return measurement.Subtype{}, err
	}

	data := map[string]measurement.Reading{
		"addon-count":             measurement.Int(len(addons)),
		"nvidia-gpu-plugin":       measurement.Str(okeAddonState(addons, okeNvidiaGPUPluginAddonName)),
		"nvidia-network-operator": measurement.Str(okeAddonState(addons, okeNvidiaNetworkOperatorAddonName)),
	}
	return measurement.Subtype{Name: SubtypeOKEAddons, Data: data}, nil
}

// okeAddonState returns the reading for the add-on called name, as listed on
// ProjectOKEAddons. Name and lifecycle state match case-insensitively.
func okeAddonState(addons []okeAddon, name string) string {
	for _, addon := range addons {
		if !strings.EqualFold(addon.Name, name) {
			continue
		}
		if strings.EqualFold(addon.LifecycleState, "ACTIVE") {
			return okeAddonInstalled
		}
		// A non-ACTIVE add-on (e.g. mid-delete) is neither installed nor
		// absent, and the marker names the state the control plane reported.
		return "addon-" + strings.ToLower(addon.LifecycleState)
	}
	return okeAddonAbsent
}

// readOKEAddons loads and decodes the add-ons dump through the shared
// provider-pools reader (see providerpools.go for the bounded-read rules
// and the pattern a new provider's projection follows).
func readOKEAddons(ctx context.Context, path string) ([]okeAddon, error) {
	raw, err := readBoundedPoolsFile(ctx, path, "OKE cluster add-ons", defaults.MaxOKEAddonsBytes)
	if err != nil {
		return nil, err
	}

	var dump okeAddonsDump
	if err := json.Unmarshal(raw, &dump); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to decode OKE add-ons file %q: expected the JSON object "+
				"emitted by `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`", path), err)
	}
	// json.Unmarshal accepts a top-level `null` (or an object without a
	// data key) leaving Data nil — that is not the documented oci output
	// and must not masquerade as a successful zero-add-on projection: a
	// live OKE cluster always reports its system add-ons (CoreDNS,
	// KubeProxy, …).
	if dump.Data == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to decode OKE add-ons file %q: no data array, expected the "+
				"JSON object emitted by `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`", path))
	}
	// A partial page could omit an installed add-on and project it absent.
	if dump.NextPage != "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("OKE add-ons file %q is a partial page (opc-next-page is set), re-capture with "+
				"`oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`", path))
	}
	return dump.Data, nil
}

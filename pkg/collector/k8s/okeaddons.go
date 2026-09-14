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

// Normalized nvidia-gpu-plugin readings. Only the two plain values match
// declared profile constraints; every other observed state projects a
// marker that matches no constraint, so resolution fails closed with the
// observed state as the actual (the AKS Managed/Mixed precedent).
const (
	okeGPUPluginInstalled = "installed"
	okeGPUPluginAbsent    = "absent"
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
type okeAddonsDump struct {
	Data []okeAddon `json:"data"`
}

// ProjectOKEAddons reads an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`
// dump and projects the NvidiaGpuPlugin add-on's control-plane state into
// the oke-addons subtype:
//
//   - NvidiaGpuPlugin present with lifecycle-state ACTIVE →
//     nvidia-gpu-plugin: installed.
//   - NvidiaGpuPlugin absent from the list → nvidia-gpu-plugin: absent
//     (the shape `terraform-oci-okecluster` produces with
//     NvidiaGpuPlugin = { remove = true }).
//   - Present in any other lifecycle state (UPDATING, DELETING, FAILED,
//     NEEDS_ATTENTION, …) → the state is projected verbatim, lowercased
//     with an "addon-" prefix (e.g. addon-deleting) — a marker no declared
//     constraint accepts, so resolution fails closed naming the observed
//     state.
//
// The file is operator-supplied via an explicit flag, so every failure here
// is an error, never a degraded-but-successful measurement: a typoed path
// or truncated dump must not masquerade as "reading unavailable" and steer
// a profile decision.
func ProjectOKEAddons(ctx context.Context, path string) (measurement.Subtype, error) {
	addons, err := readOKEAddons(ctx, path)
	if err != nil {
		return measurement.Subtype{}, err
	}

	plugin := okeGPUPluginAbsent
	for _, addon := range addons {
		if !strings.EqualFold(addon.Name, okeNvidiaGPUPluginAddonName) {
			continue
		}
		if strings.EqualFold(addon.LifecycleState, "ACTIVE") {
			plugin = okeGPUPluginInstalled
		} else {
			// Preserve the observed state fail-closed: neither profile
			// value's constraint accepts it, and the resolution error
			// names it so the operator can see what the control plane
			// reported (an add-on mid-delete must not qualify either
			// ownership mode).
			plugin = "addon-" + strings.ToLower(addon.LifecycleState)
		}
		break
	}

	data := map[string]measurement.Reading{
		"addon-count":       measurement.Int(len(addons)),
		"nvidia-gpu-plugin": measurement.Str(plugin),
	}
	return measurement.Subtype{Name: SubtypeOKEAddons, Data: data}, nil
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
	return dump.Data, nil
}

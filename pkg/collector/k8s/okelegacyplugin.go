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
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

const (
	// SubtypeOKELegacyPlugin records conflict evidence for OKE's legacy
	// (pre-cluster-add-on) NVIDIA device plugin: the nvidia-gpu-device-plugin
	// DaemonSet the OKE control plane ships into kube-system via the legacy
	// Kubernetes addon-manager. That DaemonSet is invisible to
	// `oci ce cluster list-addons` (the source of the oke-addons projection),
	// so the gpuStack operator-managed profile value constrains this reading
	// to fail closed when the legacy plugin would double-advertise
	// nvidia.com/gpu alongside the GPU Operator's plugin (#1327).
	SubtypeOKELegacyPlugin = "oke-legacy-plugin"

	okeLegacyPluginNamespace = "kube-system"
	okeLegacyPluginDSName    = "nvidia-gpu-device-plugin"

	// okeLegacyAddonManagerLabel/Mode identify a DaemonSet reconciled by the
	// legacy Kubernetes addon-manager — Oracle's shipping mechanism for the
	// pre-add-on device plugin. A same-named DaemonSet without this label is
	// not Oracle's legacy plugin and must not trip the constraint.
	okeLegacyAddonManagerLabel = "addonmanager.kubernetes.io/mode"
	okeLegacyAddonManagerMode  = "Reconcile"

	// okeLegacyKeyPlugin is the collapsed reading the profile constraint
	// matches; okeLegacyKeyDaemonSet carries the uncollapsed detail for
	// snapshot transparency.
	okeLegacyKeyPlugin    = "nvidia-gpu-device-plugin"
	okeLegacyKeyDaemonSet = "daemonset"

	// Collapsed states (okeLegacyKeyPlugin). The constraint accepts exactly
	// "none"; "active" and "unknown" both fail it closed.
	okeLegacyPluginNone    = "none"
	okeLegacyPluginActive  = "active"
	okeLegacyPluginUnknown = "unknown"

	// Detail states (okeLegacyKeyDaemonSet).
	okeLegacyDSAbsent    = "absent"
	okeLegacyDSUnlabeled = "unlabeled"
	okeLegacyDSDisabled  = "disabled"
	okeLegacyDSActive    = "active"
	okeLegacyDSUnknown   = "unknown"
)

type okeLegacyPluginSummary struct {
	plugin    string
	daemonSet string
}

func (s okeLegacyPluginSummary) subtype() measurement.Subtype {
	return measurement.Subtype{Name: SubtypeOKELegacyPlugin, Data: map[string]measurement.Reading{
		okeLegacyKeyPlugin:    measurement.Str(s.plugin),
		okeLegacyKeyDaemonSet: measurement.Str(s.daemonSet),
	}}
}

// unknownOKELegacyPluginSubtype is the no-inspection value: "we could not
// look" must never read as "no legacy plugin", so a clientless or failed
// collection yields "unknown", which no profile constraint accepts.
func unknownOKELegacyPluginSubtype() measurement.Subtype {
	return okeLegacyPluginSummary{plugin: okeLegacyPluginUnknown, daemonSet: okeLegacyDSUnknown}.subtype()
}

// collectOKELegacyPlugin observes whether OKE's legacy addon-manager-shipped
// NVIDIA device plugin can schedule onto this cluster's nodes. It records
// presence evidence only — it never infers device-plugin health.
//
// Collapse rules (okeLegacyKeyPlugin):
//   - "none":   the DaemonSet is absent, is present without the addon-manager
//     Reconcile label (an unrelated same-named workload), or is present with
//     desiredNumberScheduled == 0 (every eligible node opted out via the
//     oci.oraclecloud.com/disable-gpu-device-plugin node-pool label, or no
//     node matches Oracle's GPU affinity).
//   - "active": the labeled DaemonSet targets at least one node — deploying a
//     recipe whose GPU Operator advertises nvidia.com/gpu would
//     double-advertise (#1327).
//   - "unknown": the API could not be consulted; fails constraints closed.
//
// desiredNumberScheduled is not an omitempty field in appsv1.DaemonSetStatus,
// so 0 is a trustworthy "targets nothing" observation, not a missing value.
func (k *Collector) collectOKELegacyPlugin(ctx context.Context) measurement.Subtype {
	ds, err := k.ClientSet.AppsV1().DaemonSets(okeLegacyPluginNamespace).
		Get(ctx, okeLegacyPluginDSName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return okeLegacyPluginSummary{plugin: okeLegacyPluginNone, daemonSet: okeLegacyDSAbsent}.subtype()
		}
		// Fail closed on everything else (RBAC, timeout, transport): the
		// distinction between "absent" and "could not look" is the whole
		// point of this reading.
		slog.Warn("OKE legacy device-plugin collection failed; recording unknown",
			"namespace", okeLegacyPluginNamespace, "daemonset", okeLegacyPluginDSName, "error", err)
		return unknownOKELegacyPluginSubtype()
	}

	if ds.Labels[okeLegacyAddonManagerLabel] != okeLegacyAddonManagerMode {
		return okeLegacyPluginSummary{plugin: okeLegacyPluginNone, daemonSet: okeLegacyDSUnlabeled}.subtype()
	}
	if ds.Status.DesiredNumberScheduled == 0 {
		return okeLegacyPluginSummary{plugin: okeLegacyPluginNone, daemonSet: okeLegacyDSDisabled}.subtype()
	}
	return okeLegacyPluginSummary{plugin: okeLegacyPluginActive, daemonSet: okeLegacyDSActive}.subtype()
}

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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

const (
	// SubtypeOKEGPUPlugin records how OKE's NVIDIA device plugin hands
	// allocated GPUs to containers. The NvidiaGpuPlugin add-on's
	// configuration is not in `oci ce cluster list-addons` (the source of the
	// oke-addons projection), so it is read from the DaemonSet the plugin
	// runs as, whichever mechanism owns it.
	SubtypeOKEGPUPlugin = "oke-gpu-plugin"

	okeGPUPluginKeyStrategy = "device-list-strategy"

	okeGPUPluginStrategyFlag = "device-list-strategy"
	okeGPUPluginStrategyEnv  = "DEVICE_LIST_STRATEGY"

	// okeGPUPluginDefaultStrategy is the device plugin's own default when
	// neither the flag nor the env var is set.
	okeGPUPluginDefaultStrategy = "envvar"

	// okeGPUPluginAbsent and okeGPUPluginUnknown are the non-strategy
	// readings: no DaemonSet to inspect, or the API could not be consulted.
	// No constraint accepts either.
	okeGPUPluginAbsent  = "absent"
	okeGPUPluginUnknown = "unknown"
)

func okeGPUPluginSubtype(strategy string) measurement.Subtype {
	return measurement.Subtype{Name: SubtypeOKEGPUPlugin, Data: map[string]measurement.Reading{
		okeGPUPluginKeyStrategy: measurement.Str(strategy),
	}}
}

// unknownOKEGPUPluginSubtype is the no-inspection value: "we could not look"
// must never read as a strategy.
func unknownOKEGPUPluginSubtype() measurement.Subtype {
	return okeGPUPluginSubtype(okeGPUPluginUnknown)
}

// collectOKEGPUPlugin reads the device-list strategy of the
// nvidia-gpu-device-plugin DaemonSet in kube-system: the
// --device-list-strategy flag on a container's command or args, else its
// DEVICE_LIST_STRATEGY env var, else the plugin default (envvar). A missing
// DaemonSet reads "absent"; any other API failure reads "unknown".
func (k *Collector) collectOKEGPUPlugin(ctx context.Context) measurement.Subtype {
	ds, err := k.ClientSet.AppsV1().DaemonSets(okeGPUPluginNamespace).
		Get(ctx, okeGPUPluginDSName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return okeGPUPluginSubtype(okeGPUPluginAbsent)
		}
		slog.Warn("OKE GPU plugin collection failed; recording unknown",
			"namespace", okeGPUPluginNamespace, "daemonset", okeGPUPluginDSName, "error", err)
		return unknownOKEGPUPluginSubtype()
	}
	return okeGPUPluginSubtype(deviceListStrategy(ds.Spec.Template.Spec.Containers))
}

// deviceListStrategy returns the strategy the device plugin container runs
// with. The flag, with one or two leading dashes and its value after "=" or
// as the next argument, wins over the env var, as in the plugin's own flag
// parsing.
func deviceListStrategy(containers []corev1.Container) string {
	for _, c := range containers {
		argv := append(append([]string{}, c.Command...), c.Args...)
		for i, arg := range argv {
			name, ok := strings.CutPrefix(arg, "-")
			if !ok {
				continue
			}
			name = strings.TrimPrefix(name, "-")
			if v, ok := strings.CutPrefix(name, okeGPUPluginStrategyFlag+"="); ok && v != "" {
				return v
			}
			if name == okeGPUPluginStrategyFlag && i+1 < len(argv) && argv[i+1] != "" {
				return argv[i+1]
			}
		}
	}
	for _, c := range containers {
		for _, e := range c.Env {
			if e.Name == okeGPUPluginStrategyEnv && e.Value != "" {
				return e.Value
			}
		}
	}
	return okeGPUPluginDefaultStrategy
}

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

package recipe

import (
	"testing"
)

// TestVR200NVSentinelNRIHostMountReenable pins the metadata-collector
// re-enable wiring nvsentinel v1.22.0 unlocked on the VR200/RKE2 preview
// coordinates (NVIDIA/NVSentinel#1742). It resolves each VR200 leaf from
// the real embedded catalog and asserts the effective values that make
// metadata-collector schedulable on the host-managed-driver + CDI/NRI
// path: labeler.assumeDriverInstalled=true so the driver.installed
// label is applied without a driver pod (#2175), metadata-collector's
// runtimeClassName EXPLICITLY empty so the chart omits the field from
// the pod spec (no `nvidia` RuntimeClass exists on NRI-mode clusters
// and admission would otherwise reject every pod), and the host's
// libnvidia-ml.so.1 mounted read-only into /usr/local/nvidia/lib with
// LD_LIBRARY_PATH pointing at that directory. The hostPath source is
// under /usr/lib/aarch64-linux-gnu because the Vera Rubin reference
// image ships Ubuntu 26.04 arm64; only the NVML file is mounted, because
// that directory also holds the host glibc, which the collector image's
// older loader cannot use.
//
// The catalog and stock-render goldens catch that SOMETHING changed on
// these leaves, but as opaque digests they cannot say which field
// changed or in what direction. This test names each field so a future
// edit that drops the runtimeClassName override, mislabels the
// LD_LIBRARY_PATH mount, or forgets readOnly gets a targeted failure
// rather than a mechanical golden update.
func TestVR200NVSentinelNRIHostMountReenable(t *testing.T) {
	t.Parallel()

	vr200Ubuntu := func(intent CriteriaIntentType, platform CriteriaPlatformType) *Criteria {
		return &Criteria{
			Service:     CriteriaServiceRKE2,
			Accelerator: CriteriaAcceleratorVR200,
			OS:          CriteriaOSUbuntu,
			Intent:      intent,
			Platform:    platform,
		}
	}

	tests := []struct {
		name     string
		criteria *Criteria
	}{
		{
			name:     "training leaf edits the block directly",
			criteria: vr200Ubuntu(CriteriaIntentTraining, ""),
		},
		{
			name:     "training-kubeflow leaf inherits the block through vr200-rke2-ubuntu-training",
			criteria: vr200Ubuntu(CriteriaIntentTraining, CriteriaPlatformKubeflow),
		},
		{
			name:     "inference-dynamo leaf inherits the block through vr200-rke2-ubuntu-inference",
			criteria: vr200Ubuntu(CriteriaIntentInference, CriteriaPlatformDynamo),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, "")
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile: %v", err)
			}

			// Control: a leaf that silently lost nvsentinel would
			// satisfy every "is set" check below vacuously.
			ref := result.GetComponentRef(nvsentinelComponent)
			if ref == nil {
				t.Fatal("nvsentinel componentRef missing — the assertions below would be vacuous")
			}
			if !ref.IsEnabled() {
				t.Fatal("nvsentinel is disabled — the assertions below would be vacuous")
			}

			values, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
			}

			// labeler.assumeDriverInstalled: without it the
			// driver.installed label is never applied and three
			// DaemonSets — including metadata-collector — sit at zero
			// desired pods with no event (#2175).
			assume, ok := nestedBool(values, "labeler", "assumeDriverInstalled")
			if !ok {
				t.Fatalf("labeler.assumeDriverInstalled unset, want true. Without it three DaemonSets silently sit at zero desired pods (#2175).")
			}
			if !assume {
				t.Fatalf("labeler.assumeDriverInstalled = false, want true on host-managed-driver clusters (#2175).")
			}

			// The subchart must actually be on. Every assertion below
			// reads the metadata-collector override block, which an
			// overlay can keep verbatim while re-adding the
			// global.metadataCollector.enabled=false this change removed
			// — the chart then renders no DaemonSet and the host-mount
			// wiring below is asserted against a pod spec that does not
			// exist. Absent is the wanted state (the chart defaults the
			// subchart on); only an explicit false is a regression.
			if enabled, set := nestedMetadataCollectorEnabled(values); set && !enabled {
				t.Fatal("global.metadataCollector.enabled = false — the subchart renders no DaemonSet, so every host-mount assertion below would pass against nothing.")
			}

			collector, ok := values["metadata-collector"].(map[string]any)
			if !ok {
				t.Fatal("metadata-collector overrides missing — the assertions below would be vacuous")
			}

			// runtimeClassName must be present AND explicitly "".
			// Left unset it falls back to the chart default `nvidia`,
			// which no NRI-mode cluster registers, so admission
			// rejects every pod. The chart makes it conditional in
			// v1.22.0, so an empty value omits the field from the pod
			// spec entirely.
			runtimeRaw, present := collector["runtimeClassName"]
			if !present {
				t.Fatal("metadata-collector.runtimeClassName unset, want explicit \"\". Left unset the chart injects `nvidia` and admission rejects every pod on NRI-mode clusters (NVIDIA/NVSentinel#1742).")
			}
			runtime, isString := runtimeRaw.(string)
			if !isString {
				t.Fatalf("metadata-collector.runtimeClassName = %T %v, want string", runtimeRaw, runtimeRaw)
			}
			if runtime != "" {
				t.Fatalf("metadata-collector.runtimeClassName = %q, want \"\" to omit the field from the pod spec (NRI-mode clusters do not register a RuntimeClass named `nvidia`).", runtime)
			}

			// extraEnv must carry LD_LIBRARY_PATH pointing at the
			// mount below, otherwise the driver libraries are on disk
			// but the loader cannot find them.
			extraEnv, ok := collector["extraEnv"].([]any)
			if !ok {
				t.Fatalf("metadata-collector.extraEnv = %T %v, want a list containing an LD_LIBRARY_PATH entry (NVML cannot bind without it).", collector["extraEnv"], collector["extraEnv"])
			}
			if !hasEnvVar(extraEnv, "LD_LIBRARY_PATH", "/usr/local/nvidia/lib") {
				t.Fatalf("metadata-collector.extraEnv missing LD_LIBRARY_PATH=/usr/local/nvidia/lib. Without it libnvidia-ml.so is on disk but unlinked. Got: %v", extraEnv)
			}

			// additionalVolumeMounts must place libnvidia-ml.so.1 inside
			// the LD_LIBRARY_PATH directory above, read-only. readOnly
			// matters because the container runs privileged and a
			// writable mount invites it to mutate host driver files.
			mounts, ok := collector["additionalVolumeMounts"].([]any)
			if !ok {
				t.Fatalf("metadata-collector.additionalVolumeMounts = %T %v, want a list containing the nvidia-ml mount.", collector["additionalVolumeMounts"], collector["additionalVolumeMounts"])
			}
			if !hasVolumeMount(mounts, "nvidia-ml", "/usr/local/nvidia/lib/libnvidia-ml.so.1", true) {
				t.Fatalf("metadata-collector.additionalVolumeMounts missing name=nvidia-ml mountPath=/usr/local/nvidia/lib/libnvidia-ml.so.1 readOnly=true. Got: %v", mounts)
			}

			// additionalHostVolumes must supply only the NVML file from
			// /usr/lib/aarch64-linux-gnu — the Vera Rubin reference
			// image is Ubuntu 26.04 arm64. Mounting the whole directory
			// puts the host glibc (2.43) on LD_LIBRARY_PATH and the
			// collector (glibc 2.35 image) exits 127 on a symbol lookup
			// error before NVML is ever loaded.
			volumes, ok := collector["additionalHostVolumes"].([]any)
			if !ok {
				t.Fatalf("metadata-collector.additionalHostVolumes = %T %v, want a list containing the nvidia-ml hostPath.", collector["additionalHostVolumes"], collector["additionalHostVolumes"])
			}
			if !hasHostPathVolume(volumes, "nvidia-ml", "/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1", "File") {
				t.Fatalf("metadata-collector.additionalHostVolumes missing name=nvidia-ml hostPath.path=/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1 hostPath.type=File. Got: %v", volumes)
			}
			for _, v := range volumes {
				vol, _ := v.(map[string]any)
				hp, _ := vol["hostPath"].(map[string]any)
				if hp["path"] == "/usr/lib/aarch64-linux-gnu" {
					t.Fatalf("metadata-collector mounts the whole /usr/lib/aarch64-linux-gnu, which puts the host glibc on LD_LIBRARY_PATH. Mount libnvidia-ml.so.1 only. Got: %v", volumes)
				}
			}
		})
	}
}

// nestedMetadataCollectorEnabled reads global.metadataCollector.enabled,
// reporting whether an overlay set it at all. Unset is the wanted state
// on the VR200 leaves: the chart defaults the subchart on, so only an
// explicit value can turn it off.
func nestedMetadataCollectorEnabled(values map[string]any) (value, set bool) {
	global, ok := values["global"].(map[string]any)
	if !ok {
		return false, false
	}
	collector, ok := global["metadataCollector"].(map[string]any)
	if !ok {
		return false, false
	}
	enabled, ok := collector["enabled"].(bool)

	return enabled, ok
}

// hasEnvVar reports whether env carries a Kubernetes-shaped env var
// with the given name and value.
func hasEnvVar(env []any, name, value string) bool {
	for _, entry := range env {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == name && m["value"] == value {
			return true
		}
	}
	return false
}

// hasVolumeMount reports whether mounts carries a Kubernetes-shaped
// volumeMount matching name, mountPath, and readOnly.
func hasVolumeMount(mounts []any, name, mountPath string, readOnly bool) bool {
	for _, entry := range mounts {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] != name || m["mountPath"] != mountPath {
			continue
		}
		ro, hasRO := m["readOnly"].(bool)
		if !hasRO || ro != readOnly {
			continue
		}
		return true
	}
	return false
}

// hasHostPathVolume reports whether volumes carries a Kubernetes-shaped
// hostPath volume matching name, path, and type.
func hasHostPathVolume(volumes []any, name, path, hostPathType string) bool {
	for _, entry := range volumes {
		v, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if v["name"] != name {
			continue
		}
		hostPath, ok := v["hostPath"].(map[string]any)
		if !ok {
			continue
		}
		if hostPath["path"] != path || hostPath["type"] != hostPathType {
			continue
		}
		return true
	}
	return false
}

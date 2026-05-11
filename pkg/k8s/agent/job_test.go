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

package agent

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe/oskind"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildPodSpec_TalosSkipsSystemDHostPath(t *testing.T) {
	tests := []struct {
		name           string
		os             string
		wantHostPath   bool
		wantAICROSEnv  string
		wantHostMounts []string
	}{
		{
			name:           "talos: no systemd hostPath, AICR_OS env set",
			os:             oskind.Talos,
			wantHostPath:   false,
			wantAICROSEnv:  oskind.Talos,
			wantHostMounts: nil,
		},
		{
			name:           "ubuntu: keeps systemd hostPath, AICR_OS env set",
			os:             "ubuntu",
			wantHostPath:   true,
			wantAICROSEnv:  "ubuntu",
			wantHostMounts: []string{"run-systemd", "host-os-release"},
		},
		{
			name:           "empty OS: keeps systemd hostPath (legacy default), no AICR_OS env",
			os:             "",
			wantHostPath:   true,
			wantAICROSEnv:  "",
			wantHostMounts: []string{"run-systemd", "host-os-release"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDeployer(fake.NewClientset(), Config{
				Namespace:          "default",
				ServiceAccountName: "aicr",
				JobName:            "aicr",
				Image:              "test:latest",
				Output:             "cm://default/aicr-snapshot",
				Privileged:         true,
				OS:                 tt.os,
			})
			job := d.buildJob()
			spec := job.Spec.Template.Spec

			gotHostMounts := []string{}
			for _, v := range spec.Volumes {
				if v.HostPath != nil {
					gotHostMounts = append(gotHostMounts, v.Name)
				}
			}
			if tt.wantHostPath && len(gotHostMounts) == 0 {
				t.Errorf("expected host-path volumes for OS=%q, got none", tt.os)
			}
			if !tt.wantHostPath && len(gotHostMounts) != 0 {
				t.Errorf("expected no host-path volumes for OS=%q, got %v", tt.os, gotHostMounts)
			}
			for _, want := range tt.wantHostMounts {
				found := false
				for _, got := range gotHostMounts {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected host-path volume %q for OS=%q, missing (have %v)", want, tt.os, gotHostMounts)
				}
			}

			// Verify container VolumeMounts mirror the volume gating.
			if len(spec.Containers) != 1 {
				t.Fatalf("expected 1 container, got %d", len(spec.Containers))
			}
			gotMountPaths := []string{}
			for _, m := range spec.Containers[0].VolumeMounts {
				gotMountPaths = append(gotMountPaths, m.MountPath)
			}
			hasRunSystemD := false
			hasHostOSRelease := false
			for _, p := range gotMountPaths {
				if p == "/run/systemd" {
					hasRunSystemD = true
				}
				if p == "/etc/os-release" {
					hasHostOSRelease = true
				}
			}
			if tt.os == oskind.Talos && (hasRunSystemD || hasHostOSRelease) {
				t.Errorf("Talos pod must not mount /run/systemd or /etc/os-release; got %v", gotMountPaths)
			}
			if tt.wantHostPath && (!hasRunSystemD || !hasHostOSRelease) {
				t.Errorf("non-Talos privileged pod must mount /run/systemd and /etc/os-release; got %v", gotMountPaths)
			}

			// Verify AICR_OS env var. Distinguish "absent" from
			// "present-but-empty": the in-pod parser treats both the
			// same today, but the agent should never emit AICR_OS at
			// all when OS is unset (avoids cluttering the env with a
			// no-op variable that can confuse log greps and shells).
			gotOSEnv := ""
			foundOSEnv := false
			for _, e := range spec.Containers[0].Env {
				if e.Name == "AICR_OS" {
					gotOSEnv = e.Value
					foundOSEnv = true
					break
				}
			}
			wantPresent := tt.wantAICROSEnv != ""
			if foundOSEnv != wantPresent {
				t.Errorf("AICR_OS env presence = %v, want %v (OS=%q)", foundOSEnv, wantPresent, tt.os)
			}
			if foundOSEnv && gotOSEnv != tt.wantAICROSEnv {
				t.Errorf("AICR_OS env value = %q, want %q (OS=%q)", gotOSEnv, tt.wantAICROSEnv, tt.os)
			}
		})
	}
}

func TestToLocalObjectReferences(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []corev1.LocalObjectReference
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "empty slice",
			in:   []string{},
			want: nil,
		},
		{
			name: "single item",
			in:   []string{"my-secret"},
			want: []corev1.LocalObjectReference{
				{Name: "my-secret"},
			},
		},
		{
			name: "multiple items",
			in:   []string{"secret-a", "secret-b", "secret-c"},
			want: []corev1.LocalObjectReference{
				{Name: "secret-a"},
				{Name: "secret-b"},
				{Name: "secret-c"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toLocalObjectReferences(tt.in)

			if tt.want == nil {
				if got != nil {
					t.Errorf("toLocalObjectReferences(%v) = %v, want nil", tt.in, got)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("toLocalObjectReferences(%v) len = %d, want %d", tt.in, len(got), len(tt.want))
			}

			for i := range tt.want {
				if got[i].Name != tt.want[i].Name {
					t.Errorf("toLocalObjectReferences(%v)[%d].Name = %q, want %q",
						tt.in, i, got[i].Name, tt.want[i].Name)
				}
			}
		})
	}
}

func TestBuildPodSpec_RuntimeClassName(t *testing.T) {
	tests := []struct {
		name             string
		runtimeClassName string
		wantSet          bool
	}{
		{
			name:             "not set when empty",
			runtimeClassName: "",
			wantSet:          false,
		},
		{
			name:             "set when configured",
			runtimeClassName: "nvidia",
			wantSet:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Deployer{
				config: Config{
					RuntimeClassName: tt.runtimeClassName,
					Image:            "test-image:latest",
				},
			}
			spec := d.buildPodSpec([]string{"snapshot"})

			if tt.wantSet {
				if spec.RuntimeClassName == nil {
					t.Fatal("RuntimeClassName is nil, want non-nil")
				}
				if *spec.RuntimeClassName != tt.runtimeClassName {
					t.Errorf("RuntimeClassName = %q, want %q", *spec.RuntimeClassName, tt.runtimeClassName)
				}
			} else if spec.RuntimeClassName != nil {
				t.Errorf("RuntimeClassName = %q, want nil", *spec.RuntimeClassName)
			}
		})
	}
}

func TestBuildEnvVars_RuntimeClassName(t *testing.T) {
	tests := []struct {
		name             string
		runtimeClassName string
		wantEnvVar       bool
	}{
		{
			name:             "NVIDIA_VISIBLE_DEVICES absent when no runtime class",
			runtimeClassName: "",
			wantEnvVar:       false,
		},
		{
			name:             "NVIDIA_VISIBLE_DEVICES=all when runtime class set",
			runtimeClassName: "nvidia",
			wantEnvVar:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Deployer{
				config: Config{
					RuntimeClassName: tt.runtimeClassName,
				},
			}
			envVars := d.buildEnvVars()

			var found bool
			for _, env := range envVars {
				if env.Name == "NVIDIA_VISIBLE_DEVICES" {
					found = true
					if env.Value != "all" {
						t.Errorf("NVIDIA_VISIBLE_DEVICES = %q, want %q", env.Value, "all")
					}
					break
				}
			}

			if found != tt.wantEnvVar {
				t.Errorf("NVIDIA_VISIBLE_DEVICES present = %v, want %v", found, tt.wantEnvVar)
			}
		})
	}
}

func TestBuildPodSpec_RequireGPU_And_RuntimeClassName_Independent(t *testing.T) {
	d := &Deployer{
		config: Config{
			Privileged:       true,
			RequireGPU:       true,
			RuntimeClassName: "",
			Image:            "test-image:latest",
		},
	}
	spec := d.buildPodSpec([]string{"snapshot"})

	if spec.RuntimeClassName != nil {
		t.Errorf("RuntimeClassName should be nil when only RequireGPU is set, got %q", *spec.RuntimeClassName)
	}

	container := spec.Containers[0]
	gpuFound := false
	for name := range container.Resources.Limits {
		if name == "nvidia.com/gpu" {
			gpuFound = true
		}
	}
	if !gpuFound {
		t.Error("nvidia.com/gpu resource limit not found when RequireGPU is true")
	}
}

func TestBuildPodSpec_RuntimeClassName_With_NodeSelector(t *testing.T) {
	nodeSelector := map[string]string{
		"nvidia.com/gpu.present": "true",
	}
	d := &Deployer{
		config: Config{
			RuntimeClassName: "nvidia",
			NodeSelector:     nodeSelector,
			Image:            "test-image:latest",
		},
	}
	spec := d.buildPodSpec([]string{"snapshot"})

	if spec.RuntimeClassName == nil {
		t.Fatal("RuntimeClassName is nil, want non-nil")
	}
	if *spec.RuntimeClassName != "nvidia" {
		t.Errorf("RuntimeClassName = %q, want %q", *spec.RuntimeClassName, "nvidia")
	}

	if len(spec.NodeSelector) != 1 {
		t.Fatalf("NodeSelector has %d entries, want 1", len(spec.NodeSelector))
	}
	if spec.NodeSelector["nvidia.com/gpu.present"] != "true" {
		t.Errorf("NodeSelector[nvidia.com/gpu.present] = %q, want %q",
			spec.NodeSelector["nvidia.com/gpu.present"], "true")
	}

	envVars := d.buildEnvVars()
	var nvidiaEnvFound bool
	for _, env := range envVars {
		if env.Name == "NVIDIA_VISIBLE_DEVICES" && env.Value == "all" {
			nvidiaEnvFound = true
			break
		}
	}
	if !nvidiaEnvFound {
		t.Error("NVIDIA_VISIBLE_DEVICES=all not found when RuntimeClassName is set with NodeSelector")
	}
}

// containerResources extracts the (Requests, Limits) from the first
// container of the built Job's pod template, so tests can assert on
// the merged-vs-default behavior without re-encoding the Pod struct.
func containerResources(t *testing.T, d *Deployer) corev1.ResourceRequirements {
	t.Helper()
	job := d.buildJob()
	if got := len(job.Spec.Template.Spec.Containers); got != 1 {
		t.Fatalf("expected 1 container, got %d", got)
	}
	return job.Spec.Template.Spec.Containers[0].Resources
}

func quantityEq(t *testing.T, name string, got resource.Quantity, want string) {
	t.Helper()
	wantQ := resource.MustParse(want)
	if got.Cmp(wantQ) != 0 {
		t.Errorf("%s: got %s, want %s", name, got.String(), want)
	}
}

func TestApplyPrivilegedSettings_ResourceOverrides(t *testing.T) {
	tests := []struct {
		name         string
		overrideReq  corev1.ResourceList
		overrideLim  corev1.ResourceList
		requireGPU   bool
		wantRequests map[corev1.ResourceName]string
		wantLimits   map[corev1.ResourceName]string
	}{
		{
			name: "no overrides preserves the privileged defaults",
			wantRequests: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "1", corev1.ResourceMemory: "4Gi", corev1.ResourceEphemeralStorage: "2Gi",
			},
			wantLimits: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "2", corev1.ResourceMemory: "8Gi", corev1.ResourceEphemeralStorage: "4Gi",
			},
		},
		{
			name: "partial override only replaces specified keys",
			overrideReq: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			overrideLim: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			wantRequests: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "1", corev1.ResourceMemory: "512Mi", corev1.ResourceEphemeralStorage: "2Gi",
			},
			wantLimits: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "2", corev1.ResourceMemory: "1Gi", corev1.ResourceEphemeralStorage: "4Gi",
			},
		},
		{
			name: "RequireGPU adds default nvidia.com/gpu=1 when caller did not supply one",
			overrideLim: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			requireGPU: true,
			wantRequests: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "1", corev1.ResourceMemory: "4Gi", corev1.ResourceEphemeralStorage: "2Gi",
			},
			wantLimits: map[corev1.ResourceName]string{
				corev1.ResourceCPU:                    "2",
				corev1.ResourceMemory:                 "1Gi",
				corev1.ResourceEphemeralStorage:       "4Gi",
				corev1.ResourceName("nvidia.com/gpu"): "1",
			},
		},
		{
			name: "RequireGPU does not overwrite caller-supplied nvidia.com/gpu limit",
			overrideLim: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("4"),
			},
			requireGPU: true,
			wantRequests: map[corev1.ResourceName]string{
				corev1.ResourceCPU: "1", corev1.ResourceMemory: "4Gi", corev1.ResourceEphemeralStorage: "2Gi",
			},
			wantLimits: map[corev1.ResourceName]string{
				corev1.ResourceCPU:                    "2",
				corev1.ResourceMemory:                 "8Gi",
				corev1.ResourceEphemeralStorage:       "4Gi",
				corev1.ResourceName("nvidia.com/gpu"): "4",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDeployer(fake.NewClientset(), Config{
				Namespace:          "default",
				ServiceAccountName: "aicr",
				JobName:            "aicr",
				Image:              "test:latest",
				Output:             "cm://default/aicr-snapshot",
				Privileged:         true,
				RequireGPU:         tt.requireGPU,
				Requests:           tt.overrideReq,
				Limits:             tt.overrideLim,
			})
			r := containerResources(t, d)
			for name, want := range tt.wantRequests {
				got, ok := r.Requests[name]
				if !ok {
					t.Errorf("missing request %q", name)
					continue
				}
				quantityEq(t, "request "+string(name), got, want)
			}
			for name, want := range tt.wantLimits {
				got, ok := r.Limits[name]
				if !ok {
					t.Errorf("missing limit %q", name)
					continue
				}
				quantityEq(t, "limit "+string(name), got, want)
			}
		})
	}
}

func TestApplyRestrictedSettings_ResourceOverrides(t *testing.T) {
	d := NewDeployer(fake.NewClientset(), Config{
		Namespace:          "default",
		ServiceAccountName: "aicr",
		JobName:            "aicr",
		Image:              "test:latest",
		Output:             "cm://default/aicr-snapshot",
		Privileged:         false,
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("50m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	})
	r := containerResources(t, d)

	// Overridden keys.
	quantityEq(t, "request cpu", r.Requests[corev1.ResourceCPU], "50m")
	quantityEq(t, "limit memory", r.Limits[corev1.ResourceMemory], "256Mi")
	// Defaults preserved for unspecified keys.
	quantityEq(t, "request memory", r.Requests[corev1.ResourceMemory], "256Mi")
	quantityEq(t, "request ephemeral-storage", r.Requests[corev1.ResourceEphemeralStorage], "256Mi")
	quantityEq(t, "limit cpu", r.Limits[corev1.ResourceCPU], "500m")
	quantityEq(t, "limit ephemeral-storage", r.Limits[corev1.ResourceEphemeralStorage], "512Mi")
}

func TestMergeResourceList(t *testing.T) {
	defaults := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}
	override := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("1Gi"),
	}
	got := mergeResourceList(defaults, override)
	quantityEq(t, "cpu (default preserved)", got[corev1.ResourceCPU], "100m")
	quantityEq(t, "memory (overridden)", got[corev1.ResourceMemory], "1Gi")

	// mergeResourceList must not mutate its inputs.
	quantityEq(t, "defaults.memory unchanged", defaults[corev1.ResourceMemory], "256Mi")
}

func TestMustParseQuantity(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"cpu cores", "2"},
		{"memory", "8Gi"},
		{"millicores", "100m"},
		{"storage", "4Gi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := mustParseQuantity(tt.input)
			if q.String() != tt.input {
				t.Errorf("mustParseQuantity(%q) = %q, want %q", tt.input, q.String(), tt.input)
			}
		})
	}
}

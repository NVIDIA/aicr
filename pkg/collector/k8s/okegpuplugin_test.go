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
	stderrors "errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func gpuPluginDS(containers ...corev1.Container) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: okeGPUPluginDSName, Namespace: okeGPUPluginNamespace},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: containers},
		}},
	}
}

// TestCollectOKEGPUPlugin_Strategy pins how the device-list strategy is read
// from the add-on DaemonSet: the flag in command or args, then the env var,
// then the plugin default. A missing DaemonSet reads absent and an API
// failure reads unknown, so the constraint fails closed.
func TestCollectOKEGPUPlugin_Strategy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		objects  []runtime.Object
		getError error
		want     string
	}{
		{
			name: "flag in command (the add-on's rendering)",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name:    "nvidia-gpu-device-plugin",
				Command: []string{"nvidia-device-plugin", "--pass-device-specs=true", "--device-list-strategy=volume-mounts"},
			})},
			want: "volume-mounts",
		},
		{
			name: "flag in args",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name: "plugin",
				Args: []string{"--device-list-strategy=cdi-cri"},
			})},
			want: "cdi-cri",
		},
		{
			name: "flag value as the next arg",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name: "plugin",
				Args: []string{"--device-list-strategy", "volume-mounts"},
			})},
			want: "volume-mounts",
		},
		{
			name: "single-dash flag",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name: "plugin",
				Args: []string{"-device-list-strategy=volume-mounts"},
			})},
			want: "volume-mounts",
		},
		{
			name: "single-dash flag value as the next arg",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name: "plugin",
				Args: []string{"-device-list-strategy", "volume-mounts"},
			})},
			want: "volume-mounts",
		},
		{
			name: "env var when no flag",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name: "plugin",
				Env:  []corev1.EnvVar{{Name: okeGPUPluginStrategyEnv, Value: "volume-mounts"}},
			})},
			want: "volume-mounts",
		},
		{
			name: "flag wins over env var",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name:    "plugin",
				Command: []string{"nvidia-device-plugin", "--device-list-strategy=envvar"},
				Env:     []corev1.EnvVar{{Name: okeGPUPluginStrategyEnv, Value: "volume-mounts"}},
			})},
			want: "envvar",
		},
		{
			name: "neither flag nor env reads the plugin default",
			objects: []runtime.Object{gpuPluginDS(corev1.Container{
				Name:    "plugin",
				Command: []string{"nvidia-device-plugin", "--pass-device-specs=true"},
			})},
			want: okeGPUPluginDefaultStrategy,
		},
		{
			name: "absent daemonset",
			want: okeGPUPluginAbsent,
		},
		{
			name:     "api error reads unknown (fail closed)",
			getError: stderrors.New("apiserver unavailable"),
			want:     okeGPUPluginUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientset := fakeclient.NewClientset(tt.objects...)
			if tt.getError != nil {
				clientset.PrependReactor("get", "daemonsets",
					func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.getError
					})
			}
			k := &Collector{ClientSet: clientset}

			subtype := k.collectOKEGPUPlugin(context.Background())

			if subtype.Name != SubtypeOKEGPUPlugin {
				t.Fatalf("subtype name = %q, want %q", subtype.Name, SubtypeOKEGPUPlugin)
			}
			if got := subtype.Data[okeGPUPluginKeyStrategy].Any(); got != tt.want {
				t.Errorf("%s = %v, want %q", okeGPUPluginKeyStrategy, got, tt.want)
			}
		})
	}
}

// TestUnknownOKEGPUPluginSubtype pins the no-inspection value used by
// emptyK8sMeasurement: a clientless snapshot must read unknown, never a
// strategy.
func TestUnknownOKEGPUPluginSubtype(t *testing.T) {
	t.Parallel()

	subtype := unknownOKEGPUPluginSubtype()
	if got := subtype.Data[okeGPUPluginKeyStrategy].Any(); got != okeGPUPluginUnknown {
		t.Errorf("%s = %v, want %q", okeGPUPluginKeyStrategy, got, okeGPUPluginUnknown)
	}
}

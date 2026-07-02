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

package tuning

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestExtractPackagePins_Synthetic(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     map[string]string
		wantErr  bool
	}{
		{
			name: "dependsOn versions are not leaked",
			manifest: `spec:
  packages:
    nvidia-setup-kernel:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-setup
      version: "0.2.2"
      dependsOn:
        nvidia-tuned: "9.9.9"
    nvidia-tuned:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned
      version: "0.3.0"
      dependsOn:
        nvidia-setup-kernel: "0.2.2"
{{- end }}`,
			want: map[string]string{"nvidia-setup": "0.2.2", "nvidia-tuned": "0.3.0"},
		},
		{
			name: "gke tuning key with gke image",
			manifest: `spec:
  packages:
    tuning:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuning-gke
      version: "0.1.2"
      configMap:
        accelerator: {{ $cust.accelerator }}
{{- end }}`,
			want: map[string]string{"nvidia-tuning-gke": "0.1.2"},
		},
		{
			name: "unquoted version and unrecognized image are fine",
			manifest: `spec:
  packages:
    no-op:
      image: ghcr.io/nvidia/skyhook-packages/shellscript
      version: 1.1.1
{{- end }}`,
			want: map[string]string{"shellscript": "1.1.1"},
		},
		{
			name: "recognized image missing version errors",
			manifest: `spec:
  packages:
    nvidia-tuned:
      image: ghcr.io/nvidia/nodewright-packages/nvidia-tuned
      interrupt:
        type: reboot
{{- end }}`,
			wantErr: true,
		},
		{
			name: "conflicting versions for same image error",
			manifest: `spec:
  packages:
    a:
      image: ghcr.io/x/nvidia-setup
      version: "0.2.2"
    b:
      image: ghcr.io/x/nvidia-setup
      version: "0.3.0"
{{- end }}`,
			wantErr: true,
		},
		{
			name:     "no packages block errors",
			manifest: "spec:\n  runtimeRequired: true\n",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractPackagePins([]byte(tt.manifest))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractPackagePins_RealManifests(t *testing.T) {
	// Version-agnostic: asserts each real manifest yields the expected image
	// basenames with non-empty versions, so a pin bump does not break this test.
	cases := map[string][]string{
		"components/nodewright-customizations/manifests/tuning.yaml":         {"nvidia-setup", "nvidia-tuned"},
		"components/nodewright-customizations/manifests/tuning-generic.yaml": {"nvidia-tuned"},
		"components/nodewright-customizations/manifests/tuning-gke.yaml":     {"nvidia-tuning-gke"},
		"components/nodewright-customizations/manifests/bcm-setup.yaml":      {"nvidia-setup"},
		"components/nodewright-customizations/manifests/no-op.yaml":          {"shellscript"},
	}
	for path, wantKeys := range cases {
		t.Run(path, func(t *testing.T) {
			content, err := recipe.GetManifestContentWithContext(context.Background(), nil, path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			pins, err := extractPackagePins(content)
			if err != nil {
				t.Fatalf("extract %s: %v", path, err)
			}
			for _, k := range wantKeys {
				if v, ok := pins[k]; !ok || strings.TrimSpace(v) == "" {
					t.Errorf("%s: expected non-empty pin for %q, got %q (all: %v)", path, k, v, pins)
				}
			}
		})
	}
}

func TestClassifyPins(t *testing.T) {
	tests := []struct {
		name       string
		pins       map[string]string
		wantSetup  PackagePin
		wantTuning PackagePin
	}{
		{"full", map[string]string{"nvidia-setup": "0.2.2", "nvidia-tuned": "0.3.0"},
			PackagePin{"nvidia-setup", "0.2.2"}, PackagePin{"nvidia-tuned", "0.3.0"}},
		{"gke", map[string]string{"nvidia-tuning-gke": "0.1.2"},
			PackagePin{}, PackagePin{"nvidia-tuning-gke", "0.1.2"}},
		{"bcm", map[string]string{"nvidia-setup": "0.3.0"},
			PackagePin{"nvidia-setup", "0.3.0"}, PackagePin{}},
		{"noop", map[string]string{"shellscript": "1.1.1"}, PackagePin{}, PackagePin{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, tu := classifyPins(tt.pins)
			if s != tt.wantSetup || tu != tt.wantTuning {
				t.Errorf("got (%+v, %+v), want (%+v, %+v)", s, tu, tt.wantSetup, tt.wantTuning)
			}
		})
	}
}

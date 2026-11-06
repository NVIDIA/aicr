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
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

const (
	okeGB300RDMANetManifest = "components/network-operator/manifests/rdma-net-oke-gb300.yaml"
	okeGB300RDMACNIManifest = "components/network-operator/manifests/rdma-cni-oke-gb300.yaml"
)

func renderNetworkOperatorManifest(t *testing.T, path string, values map[string]any) string {
	t.Helper()
	content, err := GetEmbeddedFS().ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "network-operator",
		Namespace:     "nvidia-network-operator",
		ChartName:     "network-operator",
		ChartVersion:  "26.7.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render %s: %v", path, err)
	}
	return string(rendered)
}

// TestOKEGB300RDMANetFollowsRDMACNI pins how rdmaCni.enabled selects the
// rdma-net attachment: unset or false attaches with host-device alone and
// installs nothing (shared RDMA namespace mode), true chains the rdma CNI
// after host-device and installs it (exclusive mode).
func TestOKEGB300RDMANetFollowsRDMACNI(t *testing.T) {
	tests := []struct {
		name        string
		values      map[string]any
		wantPlugins []string
		wantDS      bool
	}{
		{"unset", map[string]any{}, []string{"host-device"}, false},
		{"disabled", map[string]any{"rdmaCni": map[string]any{"enabled": false}}, []string{"host-device"}, false},
		{"enabled", map[string]any{"rdmaCni": map[string]any{"enabled": true}}, []string{"host-device", "rdma"}, true},
		{"enabled as string", map[string]any{"rdmaCni": map[string]any{"enabled": "true"}}, []string{"host-device", "rdma"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nad := renderNetworkOperatorManifest(t, okeGB300RDMANetManifest, tt.values)
			_, after, found := strings.Cut(nad, "config: |")
			if !found {
				t.Fatalf("rendered NAD has no config block:\n%s", nad)
			}
			var config struct {
				Plugins []struct {
					Type string `json:"type"`
				} `json:"plugins"`
			}
			if err := json.Unmarshal([]byte(after), &config); err != nil {
				t.Fatalf("NAD config is not valid JSON: %v\n%s", err, after)
			}
			got := make([]string, 0, len(config.Plugins))
			for _, p := range config.Plugins {
				got = append(got, p.Type)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantPlugins, ",") {
				t.Errorf("rdma-net plugins = %v, want %v", got, tt.wantPlugins)
			}

			ds := renderNetworkOperatorManifest(t, okeGB300RDMACNIManifest, tt.values)
			if gotDS := strings.Contains(ds, "kind: DaemonSet"); gotDS != tt.wantDS {
				t.Errorf("rdma-cni DaemonSet rendered = %v, want %v", gotDS, tt.wantDS)
			}
		})
	}
}

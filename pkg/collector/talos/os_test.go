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

package talos

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestOSCollector_PopulatesReleaseAndExtensions(t *testing.T) {
	tests := []struct {
		name             string
		nodeInfo         corev1.NodeSystemInfo
		labels           map[string]string
		wantRelease      map[string]string
		wantExtensions   map[string]string
		wantSubtypeCount int
	}{
		{
			name: "talos node with release and nvidia extensions",
			nodeInfo: corev1.NodeSystemInfo{
				OSImage:         "Talos (v1.7.6)",
				KernelVersion:   "6.6.32-talos",
				OperatingSystem: "linux",
			},
			labels: map[string]string{
				"extensions.talos.dev/nvidia-container-toolkit":       "1.16.1-v1.7.6",
				"extensions.talos.dev/nvidia-open-gpu-kernel-modules": "555.42.06-v1.7.6",
				"kubernetes.io/hostname":                              "talos-node-0",
			},
			wantRelease: map[string]string{
				keySource:              sourceTalosNodeInfo,
				keyOSReleaseID:         "talos",
				keyOSReleasePrettyName: "Talos (v1.7.6)",
				keyOSReleaseVersionID:  "1.7.6",
				keyKernelVersion:       "6.6.32-talos",
				keyOperatingSystem:     "linux",
			},
			wantExtensions: map[string]string{
				keySource:                        sourceTalosNodeInfo,
				"nvidia-container-toolkit":       "1.16.1-v1.7.6",
				"nvidia-open-gpu-kernel-modules": "555.42.06-v1.7.6",
			},
			wantSubtypeCount: 2,
		},
		{
			name: "talos node without extension labels emits release only",
			nodeInfo: corev1.NodeSystemInfo{
				OSImage:       "Talos (v1.7.6)",
				KernelVersion: "6.6.32-talos",
			},
			labels: map[string]string{
				"kubernetes.io/hostname": "talos-node-0",
			},
			wantRelease: map[string]string{
				keySource:              sourceTalosNodeInfo,
				keyOSReleaseID:         "talos",
				keyOSReleasePrettyName: "Talos (v1.7.6)",
				keyOSReleaseVersionID:  "1.7.6",
				keyKernelVersion:       "6.6.32-talos",
			},
			wantSubtypeCount: 1,
		},
		{
			name: "non-Talos OSImage emits PRETTY_NAME but leaves ID/VERSION_ID unset",
			nodeInfo: corev1.NodeSystemInfo{
				OSImage: "Ubuntu 22.04.5 LTS",
			},
			wantRelease: map[string]string{
				keySource:              sourceTalosNodeInfo,
				keyOSReleasePrettyName: "Ubuntu 22.04.5 LTS",
			},
			wantSubtypeCount: 1,
		},
		{
			name: "label with empty suffix is ignored",
			nodeInfo: corev1.NodeSystemInfo{
				OSImage: "Talos (v1.7.6)",
			},
			labels: map[string]string{
				"extensions.talos.dev/":            "should-be-ignored",
				"extensions.talos.dev/iscsi-tools": "v0.1.4",
			},
			wantRelease: map[string]string{
				keySource:              sourceTalosNodeInfo,
				keyOSReleaseID:         "talos",
				keyOSReleasePrettyName: "Talos (v1.7.6)",
				keyOSReleaseVersionID:  "1.7.6",
			},
			wantExtensions: map[string]string{
				keySource:     sourceTalosNodeInfo,
				"iscsi-tools": "v0.1.4",
			},
			wantSubtypeCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "talos-node-0",
					Labels: tt.labels,
				},
				Status: corev1.NodeStatus{NodeInfo: tt.nodeInfo},
			}
			cs := fake.NewSimpleClientset(node)

			c := NewOSCollector(WithClientSet(cs), WithNodeName("talos-node-0"))
			m, err := c.Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if m.Type != measurement.TypeOS {
				t.Errorf("Type = %q, want %q", m.Type, measurement.TypeOS)
			}
			if len(m.Subtypes) != tt.wantSubtypeCount {
				t.Fatalf("len(Subtypes) = %d, want %d (subtypes: %v)", len(m.Subtypes), tt.wantSubtypeCount, subtypeNames(m.Subtypes))
			}

			release := m.GetSubtype(SubtypeRelease)
			if release == nil {
				t.Fatalf("missing %q subtype", SubtypeRelease)
			}
			assertSubtypeData(t, release, tt.wantRelease)
			// Assert keys not in wantRelease are absent — protects against
			// regressions where a parser change starts emitting misleading
			// fields (e.g., ID=red for a RHEL OSImage).
			for _, k := range []string{keyOSReleaseID, keyOSReleaseVersionID} {
				if _, want := tt.wantRelease[k]; want {
					continue
				}
				if release.Has(k) {
					got, _ := release.GetString(k)
					t.Errorf("release subtype unexpectedly has %q = %q", k, got)
				}
			}

			ext := m.GetSubtype(SubtypeExtensions)
			if len(tt.wantExtensions) > 0 {
				if ext == nil {
					t.Fatalf("missing %q subtype, want one with %d readings", SubtypeExtensions, len(tt.wantExtensions))
				}
				assertSubtypeData(t, ext, tt.wantExtensions)
			} else if ext != nil {
				t.Errorf("expected no %q subtype, got %v", SubtypeExtensions, ext.Data)
			}
		})
	}
}

func TestOSCollector_GracefulDegradation(t *testing.T) {
	// Intentionally do NOT pass WithNodeName so the collector exercises
	// the env-resolution fallback chain (NODE_NAME, KUBERNETES_NODE_NAME,
	// HOSTNAME). All three are cleared here so resolveNodeName returns ""
	// and Collect should return an empty TypeOS measurement.
	t.Setenv("NODE_NAME", "")
	t.Setenv("KUBERNETES_NODE_NAME", "")
	t.Setenv("HOSTNAME", "")

	c := NewOSCollector(WithClientSet(fake.NewSimpleClientset()))
	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error = %v, want nil (graceful)", err)
	}
	if m.Type != measurement.TypeOS {
		t.Errorf("Type = %q, want %q", m.Type, measurement.TypeOS)
	}
	if len(m.Subtypes) != 0 {
		t.Errorf("len(Subtypes) = %d, want 0", len(m.Subtypes))
	}
}

func TestParseOSImage(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantID      string
		wantVersion string
	}{
		// Talos-format strings that should parse.
		{"talos with v-prefix", "Talos (v1.7.6)", "talos", "1.7.6"},
		{"talos without v-prefix", "Talos (1.7.6)", "talos", "1.7.6"},
		{"talos lowercase tag", "talos (v1.7.6)", "talos", "1.7.6"},

		// Non-Talos formats: parser declines to invent ID/VERSION_ID.
		// The caller (buildReleaseSubtype) leaves those keys unset, which
		// is more honest than emitting a misleading parse.
		{"ubuntu LTS", "Ubuntu 22.04.5 LTS", "", ""},
		{"rhel parenthesized codename", "Red Hat Enterprise Linux 9.4 (Plow)", "", ""},
		{"single word no parens", "Flatcar", "", ""},
		{"empty", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotVersion := parseOSImage(tt.input)
			if gotID != tt.wantID {
				t.Errorf("id = %q, want %q", gotID, tt.wantID)
			}
			if gotVersion != tt.wantVersion {
				t.Errorf("version = %q, want %q", gotVersion, tt.wantVersion)
			}
		})
	}
}

func subtypeNames(subs []measurement.Subtype) []string {
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.Name
	}
	return out
}

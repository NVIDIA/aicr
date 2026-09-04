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
	"os"
	"path/filepath"
	"testing"
)

// writeOKEAddonsFile writes a fixture dump and returns its path.
func writeOKEAddonsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "addons.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestProjectOKEAddons pins the normalization matrix for the canonical OKE
// gpuStack qualification signal: the NvidiaGpuPlugin add-on's control-plane
// state from `oci ce cluster list-addons`. Only installed/absent match
// declared profile constraints; every other observed state must project a
// marker that matches neither, failing resolution closed with the observed
// state as the actual.
func TestProjectOKEAddons(t *testing.T) {
	t.Parallel()
	// Shaped like real `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`
	// output: a data[] array of add-on objects with kebab-case keys.
	dgxcShaped := `{"data": [
		{"name": "CertManager", "lifecycle-state": "ACTIVE"},
		{"name": "CoreDNS", "lifecycle-state": "ACTIVE"},
		{"name": "KubeProxy", "lifecycle-state": "UPDATING"}
	]}`
	tests := []struct {
		name       string
		content    string
		wantPlugin string
		wantCount  int
		wantErr    bool
	}{
		{
			name: "add-on ACTIVE → installed (oci-managed shape)",
			content: `{"data": [
				{"name": "CoreDNS", "lifecycle-state": "ACTIVE"},
				{"name": "NvidiaGpuPlugin", "lifecycle-state": "ACTIVE"}
			]}`,
			wantPlugin: "installed",
			wantCount:  2,
		},
		{
			// The terraform-oci-okecluster shape: NvidiaGpuPlugin removed at
			// creation, only system add-ons remain.
			name:       "add-on absent → absent (operator-managed shape)",
			content:    dgxcShaped,
			wantPlugin: "absent",
			wantCount:  3,
		},
		{
			// Mid-delete must qualify NEITHER ownership mode.
			name: "add-on DELETING → fail-closed marker naming the state",
			content: `{"data": [
				{"name": "NvidiaGpuPlugin", "lifecycle-state": "DELETING"}
			]}`,
			wantPlugin: "addon-deleting",
			wantCount:  1,
		},
		{
			name: "add-on NEEDS_ATTENTION → fail-closed marker",
			content: `{"data": [
				{"name": "NvidiaGpuPlugin", "lifecycle-state": "NEEDS_ATTENTION"}
			]}`,
			wantPlugin: "addon-needs_attention",
			wantCount:  1,
		},
		{
			// Name matching is case-insensitive: the control plane owns the
			// canonical casing and AICR must not silently miss a variant.
			name: "case-insensitive add-on name and state",
			content: `{"data": [
				{"name": "nvidiagpuplugin", "lifecycle-state": "active"}
			]}`,
			wantPlugin: "installed",
			wantCount:  1,
		},
		{
			// A live OKE cluster always reports system add-ons; an empty
			// data array is a plausible dump, projecting absent.
			name:       "empty data array → absent",
			content:    `{"data": []}`,
			wantPlugin: "absent",
			wantCount:  0,
		},
		{
			name:    "top-level null rejected",
			content: `null`,
			wantErr: true,
		},
		{
			name:    "object without data key rejected",
			content: `{"items": []}`,
			wantErr: true,
		},
		{
			name:    "not JSON rejected",
			content: `not json`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeOKEAddonsFile(t, tt.content)
			subtype, err := ProjectOKEAddons(t.Context(), path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ProjectOKEAddons() error = nil, want error (operator input fails loud)")
				}
				return
			}
			if err != nil {
				t.Fatalf("ProjectOKEAddons() error = %v", err)
			}
			if subtype.Name != SubtypeOKEAddons {
				t.Errorf("subtype name = %q, want %q", subtype.Name, SubtypeOKEAddons)
			}
			if got := subtype.Data["nvidia-gpu-plugin"].Any(); got != tt.wantPlugin {
				t.Errorf("nvidia-gpu-plugin = %v, want %q", got, tt.wantPlugin)
			}
			if got := subtype.Data["addon-count"].Any(); got != tt.wantCount {
				t.Errorf("addon-count = %v, want %d", got, tt.wantCount)
			}
		})
	}

	t.Run("missing file fails loud", func(t *testing.T) {
		t.Parallel()
		if _, err := ProjectOKEAddons(t.Context(), filepath.Join(t.TempDir(), "absent.json")); err == nil {
			t.Fatal("ProjectOKEAddons() error = nil for a missing file")
		}
	})
}

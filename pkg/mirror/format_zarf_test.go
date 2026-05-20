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

package mirror

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderZarf(t *testing.T) {
	tests := []struct {
		name        string
		list        *MirrorList
		wantErr     bool
		wantParts   []string
		wantNoParts []string
	}{
		{
			name: "valid list with OCI and HTTPS charts",
			list: testMirrorList(),
			wantParts: []string{
				"kind: ZarfPackageConfig",
				"name: aicr",
				"description: NVIDIA AI Cluster Runtime",
				"name: aicr-images",
				"required: true",
				"nvcr.io/nvidia/gpu-operator:v25.3.0",
				"registry.k8s.io/sig-storage/csi-provisioner:v5.2.0",
				// OCI chart: url should include chart name
				"url: oci://ghcr.io/nvidia/gpu-operator",
				"version: v25.3.0",
				"namespace: gpu-operator",
				// HTTPS chart: url is the repo, repoName is chart
				"url: https://kubernetes-sigs.github.io/aws-ebs-csi-driver",
				"repoName: aws-ebs-csi-driver",
			},
		},
		{
			name: "images only, no charts",
			list: &MirrorList{
				Images: []string{"nginx:latest"},
			},
			wantParts: []string{
				"kind: ZarfPackageConfig",
				"- nginx:latest",
			},
			wantNoParts: []string{
				"charts:",
			},
		},
		{
			name:    "nil list",
			list:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := renderZarf(&buf, tt.list)
			if (err != nil) != tt.wantErr {
				t.Errorf("renderZarf() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			output := buf.String()
			for _, part := range tt.wantParts {
				if !strings.Contains(output, part) {
					t.Errorf("renderZarf() output missing %q\nGot:\n%s", part, output)
				}
			}
			for _, noPart := range tt.wantNoParts {
				if strings.Contains(output, noPart) {
					t.Errorf("renderZarf() output should not contain %q\nGot:\n%s", noPart, output)
				}
			}
		})
	}
}

func TestRenderZarfOCIChartURL(t *testing.T) {
	list := &MirrorList{
		Charts: []ChartRef{
			{
				Name:       "kai-scheduler",
				Repository: "oci://ghcr.io/kai-scheduler",
				Chart:      "kai-scheduler",
				Version:    "v0.2.71",
				Namespace:  "kai-scheduler",
			},
		},
	}

	var buf bytes.Buffer
	if err := renderZarf(&buf, list); err != nil {
		t.Fatalf("renderZarf() error = %v", err)
	}

	output := buf.String()
	// OCI chart: URL should be oci://repo/chart (no trailing slash duplication)
	if !strings.Contains(output, "url: oci://ghcr.io/kai-scheduler/kai-scheduler") {
		t.Errorf("OCI chart URL not correctly formed\nGot:\n%s", output)
	}
	// OCI charts should NOT have repoName
	if strings.Contains(output, "repoName:") {
		t.Errorf("OCI chart should not have repoName\nGot:\n%s", output)
	}
}

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

package snapshotter

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestLoadFromFile(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		filePath    string // override; skip writing yamlContent
		wantErr     bool
		errContain  string
		wantCode    errors.ErrorCode // optional structured code assertion
	}{
		{
			name:       "nonexistent file returns error",
			filePath:   "/tmp/does-not-exist-aicr-snapshot-test.yaml",
			wantErr:    true,
			errContain: "/tmp/does-not-exist-aicr-snapshot-test.yaml",
		},
		{
			name:        "supported apiVersion loads",
			yamlContent: "kind: Snapshot\napiVersion: " + FullAPIVersion + "\nmeasurements:\n  - type: K8s\n",
			wantErr:     false,
		},
		{
			name:        "Release N target apiVersion loads",
			yamlContent: "kind: Snapshot\napiVersion: " + header.GroupVersionV1 + "\nmeasurements:\n  - type: K8s\n",
			wantErr:     false,
		},
		{
			name:        "empty apiVersion allowed for backward compat",
			yamlContent: "kind: Snapshot\nmeasurements:\n  - type: K8s\n",
			wantErr:     false,
		},
		{
			name:        "unsupported apiVersion rejected",
			yamlContent: "kind: Snapshot\napiVersion: aicr.nvidia.com/v1alpha1\nmeasurements: []\n",
			wantErr:     true,
			errContain:  `apiVersion "aicr.nvidia.com/v1alpha1"`,
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "split apiVersion rejected",
			yamlContent: "kind: Snapshot\napiVersion: aicr.run/v1alpha1\nmeasurements: []\n",
			wantErr:     true,
			errContain:  `apiVersion "aicr.run/v1alpha1"`,
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "wrong kind rejected",
			yamlContent: "kind: AICRConfig\napiVersion: " + FullAPIVersion + "\nspec:\n  foo: bar\n",
			wantErr:     true,
			errContain:  `has kind "AICRConfig"`,
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "arbitrary YAML with no kind and no measurements rejected",
			yamlContent: "foo: bar\n",
			wantErr:     true,
			errContain:  "no usable measurements",
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "kind Snapshot with empty measurements rejected",
			yamlContent: "kind: Snapshot\napiVersion: " + FullAPIVersion + "\nmeasurements: []\n",
			wantErr:     true,
			errContain:  "no usable measurements",
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "snapshot with only a nil measurement entry rejected",
			yamlContent: "kind: Snapshot\nmeasurements:\n  - null\n",
			wantErr:     true,
			errContain:  "no usable measurements",
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "snapshot with only a typeless measurement object rejected",
			yamlContent: "kind: Snapshot\nmeasurements:\n  - {}\n",
			wantErr:     true,
			errContain:  "no usable measurements",
			wantCode:    errors.ErrCodeInvalidRequest,
		},
		{
			name:        "kind-less snapshot with measurements allowed for backward compat",
			yamlContent: "measurements:\n  - type: K8s\n",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapFile := tt.filePath
			if snapFile == "" {
				dir := t.TempDir()
				snapFile = filepath.Join(dir, "snapshot.yaml")
				if err := os.WriteFile(snapFile, []byte(tt.yamlContent), 0o600); err != nil {
					t.Fatalf("failed to write test snapshot file: %v", err)
				}
			}

			_, err := LoadFromFile(t.Context(), snapFile)

			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
			if tt.wantCode != "" {
				if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
					t.Errorf("error = %v, want structured code %q", err, tt.wantCode)
				}
			}
		})
	}
}

func TestLoadFromFile_CustomResourceDetectionRoundTrip(t *testing.T) {
	snapshot := NewSnapshot()
	snapshot.Kind = header.KindSnapshot
	snapshot.APIVersion = header.GroupVersion
	snapshot.Measurements = []*measurement.Measurement{{
		Type: measurement.TypeK8s,
		Subtypes: []measurement.Subtype{
			{
				Name: "slinky-slurm",
				Data: map[string]measurement.Reading{
					"collection-state": measurement.Str("detected"),
				},
				Items: []measurement.ItemEntry{{
					Context: map[string]string{
						"id":   "controller/slurm/cluster",
						"kind": "Controller",
					},
					Data: map[string]measurement.Reading{
						"cluster-name": measurement.Str("cluster"),
					},
				}},
			},
			{
				Name: "mariadb-operator",
				Data: map[string]measurement.Reading{
					"collection-state": measurement.Str("absent"),
				},
			},
		},
	}}

	body, err := serializer.MarshalYAMLDeterministic(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.yaml")
	if writeErr := os.WriteFile(path, body, 0o600); writeErr != nil {
		t.Fatalf("write snapshot: %v", writeErr)
	}

	loaded, err := LoadFromFile(t.Context(), path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	var k8s *measurement.Measurement
	for _, candidate := range loaded.Measurements {
		if candidate.Type == measurement.TypeK8s {
			k8s = candidate
			break
		}
	}
	if k8s == nil {
		t.Fatal("K8s measurement missing after round-trip")
	}
	slinky := k8s.GetSubtype("slinky-slurm")
	if slinky == nil || len(slinky.Items) != 1 {
		t.Fatalf("Slinky items after round-trip = %+v", slinky)
	}
	if got := slinky.Items[0].Context["id"]; got != "controller/slurm/cluster" {
		t.Errorf("Slinky item id = %q, want canonical id", got)
	}
	mariaDB := k8s.GetSubtype("mariadb-operator")
	if mariaDB == nil || mariaDB.Data["collection-state"].Any() != "absent" {
		t.Fatalf("MariaDB subtype after round-trip = %+v", mariaDB)
	}
}

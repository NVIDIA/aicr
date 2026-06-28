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
			yamlContent: "kind: Snapshot\napiVersion: " + FullAPIVersion + "\nmeasurements: []\n",
			wantErr:     false,
		},
		{
			name:        "empty apiVersion allowed for backward compat",
			yamlContent: "kind: Snapshot\nmeasurements: []\n",
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

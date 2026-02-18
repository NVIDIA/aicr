// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package helper

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPodFromTemplate(t *testing.T) {
	tests := []struct {
		name         string
		template     string
		data         map[string]string
		wantPodName  string
		wantNodeName string
		wantErr      bool
	}{
		{
			name: "successful template substitution",
			template: `apiVersion: v1
kind: Pod
metadata:
  name: test-pod-${NODE_NAME}
  namespace: ${NAMESPACE}
spec:
  nodeName: ${NODE_NAME}
  containers:
  - name: test
    image: ${IMAGE}`,
			data: map[string]string{
				"NODE_NAME": "gpu-node-1",
				"NAMESPACE": "default",
				"IMAGE":     "test:latest",
			},
			wantPodName:  "test-pod-gpu-node-1",
			wantNodeName: "gpu-node-1",
			wantErr:      false,
		},
		{
			name: "handles missing file",
			template: `apiVersion: v1
kind: Pod
metadata:
  name: test`,
			data:    map[string]string{},
			wantErr: true,
		},
		{
			name: "handles invalid YAML",
			template: `this is not valid yaml
invalid: [unclosed`,
			data:    map[string]string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary template file for valid templates
			if !tt.wantErr || tt.name == "handles invalid YAML" {
				tmpfile, err := os.CreateTemp("", "pod-template-*.yaml")
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(tmpfile.Name())

				_, writeErr := tmpfile.Write([]byte(tt.template))
				if writeErr != nil {
					t.Fatal(writeErr)
				}
				closeErr := tmpfile.Close()
				if closeErr != nil {
					t.Fatal(closeErr)
				}

				pod, err := loadPodFromTemplate(tmpfile.Name(), tt.data)
				if (err != nil) != tt.wantErr {
					t.Errorf("loadPodFromTemplate() error = %v, wantErr %v", err, tt.wantErr)
					return
				}

				if !tt.wantErr {
					if pod.Name != tt.wantPodName {
						t.Errorf("pod.Name = %v, want %v", pod.Name, tt.wantPodName)
					}
					if pod.Spec.NodeName != tt.wantNodeName {
						t.Errorf("pod.Spec.NodeName = %v, want %v", pod.Spec.NodeName, tt.wantNodeName)
					}
				}
			} else {
				// Test with non-existent file
				_, err := loadPodFromTemplate(filepath.Join(os.TempDir(), "nonexistent-file.yaml"), tt.data)
				if (err != nil) != tt.wantErr {
					t.Errorf("loadPodFromTemplate() error = %v, wantErr %v", err, tt.wantErr)
				}
			}
		})
	}
}

func TestLoadPodFromTemplate_MultipleSubstitutions(t *testing.T) {
	template := `apiVersion: v1
kind: Pod
metadata:
  name: ${NAME_PREFIX}-${NODE_NAME}
  namespace: ${NAMESPACE}
  labels:
    node: ${NODE_NAME}
    env: ${ENV}
spec:
  nodeName: ${NODE_NAME}`

	tmpfile, err := os.CreateTemp("", "pod-template-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	_, writeErr := tmpfile.Write([]byte(template))
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	closeErr := tmpfile.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	data := map[string]string{
		"NAME_PREFIX": "test",
		"NODE_NAME":   "node-1",
		"NAMESPACE":   "production",
		"ENV":         "prod",
	}

	pod, err := loadPodFromTemplate(tmpfile.Name(), data)
	if err != nil {
		t.Fatalf("loadPodFromTemplate() failed: %v", err)
	}

	if pod.Name != "test-node-1" {
		t.Errorf("pod.Name = %v, want test-node-1", pod.Name)
	}
	if pod.Namespace != "production" {
		t.Errorf("pod.Namespace = %v, want production", pod.Namespace)
	}
	if pod.Spec.NodeName != "node-1" {
		t.Errorf("pod.Spec.NodeName = %v, want node-1", pod.Spec.NodeName)
	}
	if pod.Labels["node"] != "node-1" {
		t.Errorf("pod.Labels[node] = %v, want node-1", pod.Labels["node"])
	}
	if pod.Labels["env"] != "prod" {
		t.Errorf("pod.Labels[env] = %v, want prod", pod.Labels["env"])
	}
}

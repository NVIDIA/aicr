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

package helper

import (
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestLoadPodFromTemplate(t *testing.T) {
	validTemplate := `apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
spec:
  containers:
  - name: worker
    image: ${IMAGE}
    command: ["echo", "hello"]
`

	tests := []struct {
		name        string
		template    string
		data        map[string]string
		wantPodName string
		wantNS      string
		wantImage   string
		wantErr     bool
	}{
		{
			name:     "valid template with substitutions",
			template: validTemplate,
			data: map[string]string{
				"POD_NAME":  "test-pod",
				"NAMESPACE": "test-ns",
				"IMAGE":     "nvidia/cuda:12.0",
			},
			wantPodName: "test-pod",
			wantNS:      "test-ns",
			wantImage:   "nvidia/cuda:12.0",
		},
		{
			name:     "no substitution data leaves placeholders",
			template: validTemplate,
			data:     map[string]string{},
			// Placeholders remain as literal strings in the YAML.
			wantPodName: "${POD_NAME}",
			wantNS:      "${NAMESPACE}",
			wantImage:   "${IMAGE}",
		},
		{
			name:        "nil data map",
			template:    validTemplate,
			data:        nil,
			wantPodName: "${POD_NAME}",
			wantNS:      "${NAMESPACE}",
			wantImage:   "${IMAGE}",
		},
		{
			name:     "invalid YAML",
			template: "not: [valid: yaml: {",
			data:     map[string]string{},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "pod.yaml")
			if err := os.WriteFile(tmpFile, []byte(tt.template), 0600); err != nil {
				t.Fatalf("failed to write temp file: %v", err)
			}

			pod, err := LoadPodFromTemplate(tmpFile, tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadPodFromTemplate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if pod.Name != tt.wantPodName {
				t.Errorf("pod.Name = %q, want %q", pod.Name, tt.wantPodName)
			}
			if pod.Namespace != tt.wantNS {
				t.Errorf("pod.Namespace = %q, want %q", pod.Namespace, tt.wantNS)
			}
			if len(pod.Spec.Containers) == 0 {
				t.Fatal("pod has no containers")
			}
			if pod.Spec.Containers[0].Image != tt.wantImage {
				t.Errorf("container image = %q, want %q", pod.Spec.Containers[0].Image, tt.wantImage)
			}
		})
	}
}

func TestLoadPodFromTemplateFileNotFound(t *testing.T) {
	_, err := LoadPodFromTemplate("/nonexistent/path/pod.yaml", nil)
	if err == nil {
		t.Error("LoadPodFromTemplate() expected error for missing file, got nil")
	}
}

func TestCheckPodRunningOrTerminal(t *testing.T) {
	tests := []struct {
		name     string
		phase    v1.PodPhase
		wantDone bool
		wantErr  bool
	}{
		{"running", v1.PodRunning, true, false},
		{"succeeded", v1.PodSucceeded, true, false},
		{"failed", v1.PodFailed, true, true},
		{"pending", v1.PodPending, false, false},
		{"unknown", v1.PodPhase("Unknown"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{Status: v1.PodStatus{Phase: tt.phase}}
			done, err := checkPodRunningOrTerminal(pod)
			if done != tt.wantDone {
				t.Errorf("checkPodRunningOrTerminal() done = %v, want %v", done, tt.wantDone)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("checkPodRunningOrTerminal() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

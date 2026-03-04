// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package performance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestSanitizeTarPath(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		targetDir string
		entryPath string
		wantErr   bool
	}{
		{
			name:      "valid path inside target",
			targetDir: tmpDir,
			entryPath: "trainer-2.1.0/manifests/base/kustomization.yaml",
			wantErr:   false,
		},
		{
			name:      "path traversal attempt",
			targetDir: tmpDir,
			entryPath: "../../../etc/passwd",
			wantErr:   true,
		},
		{
			name:      "path with double dots in middle",
			targetDir: tmpDir,
			entryPath: "foo/../../etc/passwd",
			wantErr:   true,
		},
		{
			name:      "simple filename",
			targetDir: tmpDir,
			entryPath: "README.md",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeTarPath(tt.targetDir, tt.entryPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("sanitizeTarPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got == "" {
					t.Error("sanitizeTarPath() returned empty path for valid input")
				}
				// Verify the returned path is under targetDir
				rel, err := filepath.Rel(tt.targetDir, got)
				if err != nil || rel == ".." || rel[:3] == "../" {
					t.Errorf("sanitizeTarPath() returned path outside target: %v", got)
				}
			}
		})
	}
}

func TestIsCRDEstablished(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{
			name: "CRD with Established=True",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Established",
								"status": "True",
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "CRD with Established=False",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Established",
								"status": "False",
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "CRD without Established condition",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "NamesAccepted",
								"status": "True",
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "CRD with no status",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{},
			},
			want: false,
		},
		{
			name: "CRD with empty conditions",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCRDEstablished(tt.obj)
			if got != tt.want {
				t.Errorf("isCRDEstablished() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTrainerConstants(t *testing.T) {
	if trainerArchiveURL == "" {
		t.Error("trainerArchiveURL is empty")
	}
	if trainerKustomizePath == "" {
		t.Error("trainerKustomizePath is empty")
	}
	if trainerCRDName == "" {
		t.Error("trainerCRDName is empty")
	}
	if maxExtractedFileSize <= 0 {
		t.Errorf("maxExtractedFileSize = %d, want > 0", maxExtractedFileSize)
	}
}

func TestIsTrainerInstalled(t *testing.T) {
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}

	tests := []struct {
		name    string
		objects []runtime.Object
		want    bool
		wantErr bool
	}{
		{
			name: "trainer CRD exists",
			objects: []runtime.Object{
				&unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "apiextensions.k8s.io/v1",
						"kind":       "CustomResourceDefinition",
						"metadata": map[string]interface{}{
							"name": trainerCRDName,
						},
					},
				},
			},
			want:    true,
			wantErr: false,
		},
		{
			name:    "trainer CRD not found",
			objects: []runtime.Object{},
			want:    false,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
				map[schema.GroupVersionResource]string{
					crdGVR: "CustomResourceDefinitionList",
				},
				tt.objects...)

			got, err := isTrainerInstalled(context.Background(), client)
			if (err != nil) != tt.wantErr {
				t.Errorf("isTrainerInstalled() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("isTrainerInstalled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteTrainer(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "configmaps",
	}

	scheme := runtime.NewScheme()
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "test-ns",
			},
		},
	}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr: "ConfigMapList",
		},
		obj)

	refs := []trainerResourceRef{
		{GVR: gvr, Namespace: "test-ns", Name: "test-cm"},
	}

	// Verify resource exists
	_, err := client.Resource(gvr).Namespace("test-ns").Get(context.Background(), "test-cm", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Resource should exist before delete: %v", err)
	}

	deleteTrainer(client, refs)

	// Verify resource is deleted
	_, err = client.Resource(gvr).Namespace("test-ns").Get(context.Background(), "test-cm", metav1.GetOptions{})
	if err == nil {
		t.Error("Resource should be deleted after deleteTrainer")
	}
}

func TestDeleteTrainerClusterScoped(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles",
	}

	scheme := runtime.NewScheme()
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata": map[string]interface{}{
				"name": "test-role",
			},
		},
	}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			gvr: "ClusterRoleList",
		},
		obj)

	refs := []trainerResourceRef{
		{GVR: gvr, Namespace: "", Name: "test-role"},
	}

	deleteTrainer(client, refs)

	_, err := client.Resource(gvr).Get(context.Background(), "test-role", metav1.GetOptions{})
	if err == nil {
		t.Error("Cluster-scoped resource should be deleted")
	}
}

func TestDeleteTrainerEmpty(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClient(scheme)

	// Should not panic with empty slice
	deleteTrainer(client, nil)
	deleteTrainer(client, []trainerResourceRef{})
}

func TestCleanupNCCLResources(t *testing.T) {
	scheme := runtime.NewScheme()

	tj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "trainer.kubeflow.org/v1alpha1",
			"kind":       "TrainJob",
			"metadata": map[string]interface{}{
				"name":      ncclTrainJobName,
				"namespace": "test-ns",
			},
		},
	}

	rt := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "trainer.kubeflow.org/v1alpha1",
			"kind":       "TrainingRuntime",
			"metadata": map[string]interface{}{
				"name":      ncclTrainingRuntimeName,
				"namespace": "test-ns",
			},
		},
	}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			trainJobGVR:        "TrainJobList",
			trainingRuntimeGVR: "TrainingRuntimeList",
		},
		tj, rt)

	// Should not panic, should delete both resources
	cleanupNCCLResources(client, "test-ns")

	// Verify both deleted
	_, err := client.Resource(trainJobGVR).Namespace("test-ns").Get(context.Background(), ncclTrainJobName, metav1.GetOptions{})
	if err == nil {
		t.Error("TrainJob should be deleted")
	}
	_, err = client.Resource(trainingRuntimeGVR).Namespace("test-ns").Get(context.Background(), ncclTrainingRuntimeName, metav1.GetOptions{})
	if err == nil {
		t.Error("TrainingRuntime should be deleted")
	}
}

func TestCleanupNCCLResourcesNotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			trainJobGVR:        "TrainJobList",
			trainingRuntimeGVR: "TrainingRuntimeList",
		})

	// Should not panic when resources don't exist
	cleanupNCCLResources(client, "test-ns")
}

func TestExtractTarGzValid(t *testing.T) {
	// Create a valid tar.gz in memory
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Add a directory
	if err := tw.WriteHeader(&tar.Header{
		Name:     "test-dir/",
		Typeflag: tar.TypeDir,
		Mode:     0750,
	}); err != nil {
		t.Fatalf("Failed to write dir header: %v", err)
	}

	// Add a file
	content := []byte("hello world")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "test-dir/test.txt",
		Typeflag: tar.TypeReg,
		Mode:     0640,
		Size:     int64(len(content)),
	}); err != nil {
		t.Fatalf("Failed to write file header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("Failed to write file content: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("Failed to close tar writer: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("Failed to close gzip writer: %v", err)
	}

	tmpDir := t.TempDir()
	err := extractTarGz(&buf, tmpDir)
	if err != nil {
		t.Fatalf("extractTarGz() error = %v", err)
	}

	// Verify extracted file
	extracted, err := os.ReadFile(filepath.Join(tmpDir, "test-dir", "test.txt"))
	if err != nil {
		t.Fatalf("Failed to read extracted file: %v", err)
	}
	if string(extracted) != "hello world" {
		t.Errorf("Extracted content = %q, want %q", string(extracted), "hello world")
	}
}

func TestExtractTarGzInvalidInput(t *testing.T) {
	tmpDir := t.TempDir()

	// Test with invalid gzip data
	invalidGzip := filepath.Join(tmpDir, "bad.tar.gz")
	if err := os.WriteFile(invalidGzip, []byte("not a gzip file"), 0640); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	f, err := os.Open(filepath.Clean(invalidGzip))
	if err != nil {
		t.Fatalf("Failed to open test file: %v", err)
	}
	defer f.Close()

	err = extractTarGz(f, tmpDir)
	if err == nil {
		t.Error("extractTarGz() expected error for invalid gzip data, got nil")
	}
}

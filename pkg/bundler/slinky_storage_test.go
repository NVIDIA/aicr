// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bundler

import (
	"bytes"
	stderrors "errors"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const sharedStorageManifestPath = slinkySharedStoragePreManifestPath

func TestMaterializeSlinkySharedStorage(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(map[string]any)
		unsupported bool
		wantErr     bool
		wantErrText string
		wantPaths   bool
		wantExtra   bool
	}{
		{
			name: "disabled is no-op",
		},
		{
			name: "missing enabled is no-op",
			mutate: func(values map[string]any) {
				delete(values["storage"].(map[string]any), "enabled")
			},
		},
		{
			name: "storage must be an object",
			mutate: func(values map[string]any) {
				values["storage"] = "not-an-object"
			},
			wantErr:     true,
			wantErrText: "storage must be an object",
		},
		{
			name: "enabled must be a boolean",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = "true"
			},
			wantErr:     true,
			wantErrText: "storage.enabled must be a boolean",
		},
		{
			name: "enabled adds login and nodeset mounts",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
			},
			wantPaths: true,
		},
		{
			name: "enabled adds mounts to every configured set including disabled",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				values["loginsets"].(map[string]any)["secondary"] = map[string]any{"enabled": false}
				values["nodesets"].(map[string]any)["secondary"] = map[string]any{"enabled": false}
			},
			wantPaths: true,
			wantExtra: true,
		},
		{
			name: "unsupported storage field is rejected while disabled",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabld"] = true
			},
			wantErr:     true,
			wantErrText: "unsupported fields: enabld",
		},
		{
			name: "missing class is rejected",
			mutate: func(values map[string]any) {
				storage := values["storage"].(map[string]any)
				storage["enabled"] = true
				storage["home"].(map[string]any)["storageClassName"] = ""
			},
			wantErr:     true,
			wantErrText: "--shared-storage-class",
		},
		{
			name: "invalid class name is rejected",
			mutate: func(values map[string]any) {
				storage := values["storage"].(map[string]any)
				storage["enabled"] = true
				storage["home"].(map[string]any)["storageClassName"] = "rwx: invalid"
			},
			wantErr:     true,
			wantErrText: "storage.home.storageClassName",
		},
		{
			name: "invalid loginsets object is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				values["loginsets"] = "not-an-object"
			},
			wantErr:     true,
			wantErrText: "loginsets must be an object",
		},
		{
			name: "invalid set object is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				values["loginsets"].(map[string]any)["slinky"] = "not-an-object"
			},
			wantErr:     true,
			wantErrText: "loginsets.slinky must be an object",
		},
		{
			name: "invalid container object is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				values["loginsets"].(map[string]any)["slinky"] = map[string]any{
					"login": "not-an-object",
				}
			},
			wantErr:     true,
			wantErrText: "loginsets.slinky.login must be an object",
		},
		{
			name: "invalid pod spec object is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				values["nodesets"].(map[string]any)["slinky"] = map[string]any{
					"podSpec": "not-an-object",
				}
			},
			wantErr:     true,
			wantErrText: "nodesets.slinky.podSpec must be an object",
		},
		{
			name: "invalid mount list is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				component.SetValueByPath(values, "loginsets.slinky.login.volumeMounts", "not-a-list")
			},
			wantErr:     true,
			wantErrText: "loginsets.slinky.login.volumeMounts must be a list",
		},
		{
			name: "unsupported recipe is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
			},
			unsupported: true,
			wantErr:     true,
		},
		{
			name: "invalid size is rejected",
			mutate: func(values map[string]any) {
				storage := values["storage"].(map[string]any)
				storage["enabled"] = true
				storage["data"].(map[string]any)["size"] = "not-a-quantity"
			},
			wantErr:     true,
			wantErrText: "must be a positive Kubernetes quantity",
		},
		{
			name: "duplicate claim names are rejected",
			mutate: func(values map[string]any) {
				storage := values["storage"].(map[string]any)
				storage["enabled"] = true
				storage["data"].(map[string]any)["name"] = "shared-home"
			},
			wantErr:     true,
			wantErrText: "storage.home.name and storage.data.name must be different",
		},
		{
			name: "conflicting existing mount is rejected",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				component.SetValueByPath(values, "loginsets.slinky.login.volumeMounts", []any{
					map[string]any{"name": "shared-home", "mountPath": "/somewhere-else"},
				})
			},
			wantErr:     true,
			wantErrText: "contains a conflicting \"shared-home\" entry",
		},
		{
			name: "matching existing mount is not duplicated",
			mutate: func(values map[string]any) {
				values["storage"].(map[string]any)["enabled"] = true
				component.SetValueByPath(values, "loginsets.slinky.login.volumeMounts", []any{
					map[string]any{"name": "shared-home", "mountPath": "/home"},
				})
			},
			wantPaths: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validSlinkyStorageValues(false)
			if tt.mutate != nil {
				tt.mutate(values)
			}

			err := materializeSlinkySharedStorage(values, !tt.unsupported)
			if (err != nil) != tt.wantErr {
				t.Fatalf("materializeSlinkySharedStorage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Fatalf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
				}
				if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErrText)
				}
				return
			}

			paths := []string{
				"loginsets.slinky.login.volumeMounts",
				"loginsets.slinky.podSpec.volumes",
				"nodesets.slinky.slurmd.volumeMounts",
				"nodesets.slinky.podSpec.volumes",
			}
			if tt.wantExtra {
				paths = append(paths,
					"loginsets.secondary.login.volumeMounts",
					"loginsets.secondary.podSpec.volumes",
					"nodesets.secondary.slurmd.volumeMounts",
					"nodesets.secondary.podSpec.volumes",
				)
			}
			for _, path := range paths {
				got, exists := component.GetValueByPath(values, path)
				if exists != tt.wantPaths {
					t.Errorf("%s exists = %v, want %v (value %v)", path, exists, tt.wantPaths, got)
					continue
				}
				if tt.wantPaths {
					entries, ok := got.([]any)
					if !ok {
						t.Errorf("%s type = %T, want []any", path, got)
						continue
					}
					if len(entries) != 2 {
						t.Errorf("%s entries = %d, want 2", path, len(entries))
					}
				}
			}
		})
	}
}

func TestMaterializeSlinkySharedStorageNormalizesVolumeValues(t *testing.T) {
	values := validSlinkyStorageValues(true)
	home := values["storage"].(map[string]any)["home"].(map[string]any)
	home["name"] = " shared-home "
	home["storageClassName"] = " home-sc "
	home["size"] = " 100Gi "

	if err := materializeSlinkySharedStorage(values, true); err != nil {
		t.Fatalf("materializeSlinkySharedStorage() error = %v", err)
	}

	assertStringValueAtPath(t, values, "storage.home.name", "shared-home")
	assertStringValueAtPath(t, values, "storage.home.storageClassName", "home-sc")
	assertStringValueAtPath(t, values, "storage.home.size", "100Gi")
}

func TestAppendNamedObject(t *testing.T) {
	tests := []struct {
		name        string
		entries     []any
		entry       map[string]any
		wantLen     int
		wantErrText string
	}{
		{
			name:    "appends a new named object",
			entries: []any{map[string]any{"name": "existing"}},
			entry:   map[string]any{"name": "new"},
			wantLen: 2,
		},
		{
			name:    "matching object is idempotent",
			entries: []any{map[string]any{"name": "shared-home", "mountPath": "/home"}},
			entry:   map[string]any{"name": "shared-home", "mountPath": "/home"},
			wantLen: 1,
		},
		{
			name:        "conflicting object is rejected",
			entries:     []any{map[string]any{"name": "shared-home", "mountPath": "/other"}},
			entry:       map[string]any{"name": "shared-home", "mountPath": "/home"},
			wantErrText: "contains a conflicting \"shared-home\" entry",
		},
		{
			name:        "non-object entry is rejected",
			entries:     []any{"not-an-object"},
			entry:       map[string]any{"name": "shared-home", "mountPath": "/home"},
			wantErrText: "entries must be objects",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appendNamedObject(tt.entries, tt.entry, "loginsets.slinky.login.volumeMounts")
			if tt.wantErrText != "" {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Fatalf("error = %v, want %s", err, errors.ErrCodeInvalidRequest)
				}
				if !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("appendNamedObject() error = %v", err)
			}
			if len(got) != tt.wantLen {
				t.Errorf("len(appendNamedObject()) = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestApplySharedStorageClassOverride(t *testing.T) {
	const (
		homePath = "storage.home.storageClassName"
		dataPath = "storage.data.storageClassName"
	)
	tests := []struct {
		name     string
		options  []config.Option
		wantHome string
		wantData string
	}{
		{
			name: "generic class does not affect shared paths",
			options: []config.Option{
				config.WithStorageClass("gp3"),
			},
		},
		{
			name: "shared class populates both paths",
			options: []config.Option{
				config.WithSharedStorageClass("efs-sc"),
			},
			wantHome: "efs-sc",
			wantData: "efs-sc",
		},
		{
			name: "per-volume scalar override wins",
			options: []config.Option{
				config.WithSharedStorageClass("efs-sc"),
				config.WithValueOverrides(map[string]map[string]string{
					"slinkyslurm": {homePath: "home-sc"},
				}),
			},
			wantHome: "home-sc",
			wantData: "efs-sc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(tt.options...)
			bundler, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			values := map[string]any{}
			if tt.wantHome == "home-sc" {
				component.SetValueByPath(values, homePath, "home-sc")
			}

			if applyErr := bundler.applySharedStorageClassOverride(slinkySlurmComponentName, values, nil); applyErr != nil {
				t.Fatalf("applySharedStorageClassOverride() error = %v", applyErr)
			}

			assertStringValueAtPath(t, values, homePath, tt.wantHome)
			assertStringValueAtPath(t, values, dataPath, tt.wantData)
		})
	}
}

func TestApplySharedStorageClassOverrideRejectsInvalidValues(t *testing.T) {
	cfg := config.NewConfig(config.WithSharedStorageClass("efs-sc"))
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	applyErr := bundler.applySharedStorageClassOverride(
		slinkySlurmComponentName,
		map[string]any{"storage": "not-an-object"},
		nil,
	)
	if !stderrors.Is(applyErr, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("error = %v, want %s", applyErr, errors.ErrCodeInvalidRequest)
	}
}

func TestSlinkySharedStorageManifest(t *testing.T) {
	content, err := recipe.GetEmbeddedFS().ReadFile(sharedStorageManifestPath)
	if err != nil {
		t.Fatalf("read shared storage manifest: %v", err)
	}

	tests := []struct {
		name     string
		enabled  bool
		wantPVCs int
	}{
		{name: "disabled renders no PVCs"},
		{name: "enabled renders home and data PVCs", enabled: true, wantPVCs: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validSlinkyStorageValues(tt.enabled)
			rendered, renderErr := manifest.Render(content, manifest.RenderInput{
				ComponentName: slinkySlurmComponentName,
				Namespace:     "slurm",
				ChartName:     "slurm",
				ChartVersion:  "1.2.0",
				Values:        values,
			})
			if renderErr != nil {
				t.Fatalf("Render() error = %v", renderErr)
			}
			got := string(rendered)
			if count := strings.Count(got, "kind: PersistentVolumeClaim"); count != tt.wantPVCs {
				t.Fatalf("PVC count = %d, want %d:\n%s", count, tt.wantPVCs, got)
			}
			if tt.enabled {
				for _, want := range []string{
					`name: "shared-home"`,
					`name: "shared-data"`,
					"namespace: slurm",
					`storageClassName: "rwx-sc"`,
					`helm.sh/resource-policy: "keep"`,
					`argocd.argoproj.io/sync-options: "Delete=false,Prune=false"`,
					"- ReadWriteMany",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("rendered manifest missing %q:\n%s", want, got)
					}
				}
			}
		})
	}
}

func TestSlinkySharedStorageManifestComponentBinding(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry() error = %v", err)
	}
	componentConfig := registry.Get(slinkySlurmComponentName)
	if componentConfig == nil {
		t.Fatalf("registry component %q not found", slinkySlurmComponentName)
	}
	content, err := recipe.GetEmbeddedFS().ReadFile(slinkySharedStoragePreManifestPath)
	if err != nil {
		t.Fatalf("read shared storage manifest: %v", err)
	}
	expectedBinding := `index .Values "` + componentConfig.Name + `"`
	if !strings.Contains(string(content), expectedBinding) {
		t.Fatalf("shared storage manifest does not bind registry component %q with %q",
			componentConfig.Name, expectedBinding)
	}

	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: componentConfig.Name,
		Namespace:     "slurm",
		ChartName:     componentConfig.Helm.DefaultChart,
		ChartVersion:  componentConfig.Helm.DefaultVersion,
		Values:        validSlinkyStorageValues(true),
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if got := len(decodeSlinkyPVCs(t, rendered)); got != 2 {
		t.Fatalf("rendered PVC count = %d, want 2:\n%s", got, rendered)
	}
}

func TestSlinkySharedStorageMixedStorageClassesRender(t *testing.T) {
	const homePath = "storage.home.storageClassName"
	values := validSlinkyStorageValues(true)
	component.SetValueByPath(values, homePath, " home-sc ")
	delete(values["storage"].(map[string]any)["data"].(map[string]any), "storageClassName")

	cfg := config.NewConfig(
		config.WithSharedStorageClass("shared-sc"),
		config.WithValueOverrides(map[string]map[string]string{
			"slinkyslurm": {homePath: " home-sc "},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err = bundler.applySharedStorageClassOverride(slinkySlurmComponentName, values, nil); err != nil {
		t.Fatalf("applySharedStorageClassOverride() error = %v", err)
	}
	if err = materializeSlinkySharedStorage(values, true); err != nil {
		t.Fatalf("materializeSlinkySharedStorage() error = %v", err)
	}

	content, err := recipe.GetEmbeddedFS().ReadFile(sharedStorageManifestPath)
	if err != nil {
		t.Fatalf("read shared storage manifest: %v", err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: slinkySlurmComponentName,
		Namespace:     "slurm",
		ChartName:     "slurm",
		ChartVersion:  "1.2.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	pvcs := decodeSlinkyPVCs(t, rendered)
	assertPVCStorageClass(t, pvcs, "shared-home", "home-sc")
	assertPVCStorageClass(t, pvcs, "shared-data", "shared-sc")
}

func TestSlinkySharedStorageManifestQuotesStringScalars(t *testing.T) {
	content, err := recipe.GetEmbeddedFS().ReadFile(sharedStorageManifestPath)
	if err != nil {
		t.Fatalf("read shared storage manifest: %v", err)
	}

	values := validSlinkyStorageValues(true)
	home := values["storage"].(map[string]any)["home"].(map[string]any)
	home["name"] = "true"
	home["storageClassName"] = "false"
	home["size"] = "1"
	if err = materializeSlinkySharedStorage(values, true); err != nil {
		t.Fatalf("materializeSlinkySharedStorage() error = %v", err)
	}

	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: slinkySlurmComponentName,
		Namespace:     "slurm",
		ChartName:     "slurm",
		ChartVersion:  "1.2.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	pvcs := decodeSlinkyPVCs(t, rendered)
	pvc, found := pvcs["true"]
	if !found {
		t.Fatalf("rendered PVC with name true not found:\n%s", rendered)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "false" {
		t.Errorf("storageClassName = %v, want false", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "1" {
		t.Errorf("storage request = %q, want 1", got.String())
	}
}

func decodeSlinkyPVCs(t *testing.T, rendered []byte) map[string]corev1.PersistentVolumeClaim {
	t.Helper()
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	pvcs := make(map[string]corev1.PersistentVolumeClaim)
	for {
		var pvc corev1.PersistentVolumeClaim
		if err := decoder.Decode(&pvc); err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered PVC: %v\n%s", err, rendered)
		}
		if pvc.Name == "" {
			continue
		}
		if _, exists := pvcs[pvc.Name]; exists {
			t.Fatalf("duplicate rendered PVC %q:\n%s", pvc.Name, rendered)
		}
		pvcs[pvc.Name] = pvc
	}
	return pvcs
}

func assertPVCStorageClass(
	t *testing.T,
	pvcs map[string]corev1.PersistentVolumeClaim,
	name string,
	want string,
) {

	t.Helper()
	pvc, exists := pvcs[name]
	if !exists {
		t.Fatalf("PVC %q not found", name)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != want {
		t.Errorf("PVC %q storageClassName = %v, want %q", name, pvc.Spec.StorageClassName, want)
	}
}

func validSlinkyStorageValues(enabled bool) map[string]any {
	volume := func(name string) map[string]any {
		return map[string]any{
			"name":             name,
			"storageClassName": "rwx-sc",
			"size":             "100Gi",
		}
	}
	return map[string]any{
		"storage": map[string]any{
			"enabled": enabled,
			"home":    volume("shared-home"),
			"data":    volume("shared-data"),
		},
		"loginsets": map[string]any{
			"slinky": map[string]any{"enabled": true},
		},
		"nodesets": map[string]any{
			"slinky": map[string]any{"enabled": true},
		},
	}
}

func assertStringValueAtPath(t *testing.T, values map[string]any, path, want string) {
	t.Helper()
	got, exists := component.GetValueByPath(values, path)
	if want == "" {
		if exists {
			t.Errorf("%s = %v, want absent", path, got)
		}
		return
	}
	if !exists {
		t.Errorf("%s is absent, want %q", path, want)
		return
	}
	if got != want {
		t.Errorf("%s = %v, want %q", path, got, want)
	}
}

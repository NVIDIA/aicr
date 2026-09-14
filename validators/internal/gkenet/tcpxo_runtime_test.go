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

package gkenet

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func mapping(nets ...string) []recipe.NetworkInterfaceMapping {
	out := make([]recipe.NetworkInterfaceMapping, 0, len(nets))
	for i, n := range nets {
		out = append(out, recipe.NetworkInterfaceMapping{InterfaceName: "eth" + string(rune('1'+i)), Network: n})
	}
	return out
}

func eightNets(prefix string) []string {
	out := make([]string, 0, 8)
	for i := range 8 {
		out = append(out, prefix+"-gpu-nic-"+string(rune('0'+i)))
	}
	return out
}

func annotationFor(m []recipe.NetworkInterfaceMapping) string {
	s := `[{"interfaceName":"eth0","network":"default"}`
	for _, e := range m {
		s += `,{"interfaceName":"` + e.InterfaceName + `","network":"` + e.Network + `"}`
	}
	return s + "]"
}

// ctrFixture builds a ClusterTrainingRuntime shaped like the shipped
// torch-distributed-tcpxo manifest: one "node" replicatedJob whose worker
// template carries the GKE annotations in template.metadata.
func ctrFixture(ann map[string]any) *unstructured.Unstructured {
	tmpl := map[string]any{
		"metadata": map[string]any{"annotations": ann},
		"spec":     map[string]any{"containers": []any{map[string]any{"name": "node"}}},
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "ClusterTrainingRuntime",
		"metadata":   map[string]any{"name": TCPXORuntimeName},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"replicatedJobs": []any{map[string]any{
				"name":     TCPXONodeJob,
				"template": map[string]any{"spec": map[string]any{"template": tmpl}},
			}},
		}}},
	}}
}

func TestFabricRuntimeDelivered(t *testing.T) {
	good := mapping(eightNets("c1")...)
	rawGood := make([]any, 0, len(good))
	for _, e := range good {
		rawGood = append(rawGood, map[string]any{"interfaceName": e.InterfaceName, "network": e.Network})
	}
	tests := []struct {
		name          string
		refs          []recipe.ComponentRef
		wantDelivered bool
		wantErr       bool
	}{
		{"not declared", []recipe.ComponentRef{{Name: "gpu-operator"}}, false, false},
		{"declared without override", []recipe.ComponentRef{{
			Name: recipe.KubeflowTrainerComponentName, ManifestFiles: []string{recipe.GKETCPXORuntimeManifest}}}, false, false},
		{"declared with manifest and override", []recipe.ComponentRef{{
			Name:          recipe.KubeflowTrainerComponentName,
			ManifestFiles: []string{recipe.GKETCPXORuntimeManifest},
			Overrides:     map[string]any{recipe.GKETCPXOInterfacesOverrideKey: rawGood},
		}}, true, false},
		{"override without the runtime manifest is not delivered", []recipe.ComponentRef{{
			Name:      recipe.KubeflowTrainerComponentName,
			Overrides: map[string]any{recipe.GKETCPXOInterfacesOverrideKey: rawGood},
		}}, false, false},
		{"declared but disabled", []recipe.ComponentRef{{
			Name:          recipe.KubeflowTrainerComponentName,
			ManifestFiles: []string{recipe.GKETCPXORuntimeManifest},
			Overrides:     map[string]any{"enabled": false, recipe.GKETCPXOInterfacesOverrideKey: rawGood},
		}}, false, false},
		{"well-shaped but incomplete mapping fails closed", []recipe.ComponentRef{{
			Name:          recipe.KubeflowTrainerComponentName,
			ManifestFiles: []string{recipe.GKETCPXORuntimeManifest},
			Overrides:     map[string]any{recipe.GKETCPXOInterfacesOverrideKey: rawGood[:7]},
		}}, false, true},
		{"malformed override fails closed", []recipe.ComponentRef{{
			Name:          recipe.KubeflowTrainerComponentName,
			ManifestFiles: []string{recipe.GKETCPXORuntimeManifest},
			Overrides:     map[string]any{recipe.GKETCPXOInterfacesOverrideKey: "eth1=x"},
		}}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, delivered, err := FabricRuntimeDelivered(tt.refs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if delivered != tt.wantDelivered {
				t.Fatalf("delivered = %v, want %v", delivered, tt.wantDelivered)
			}
			if delivered && len(got) != 8 {
				t.Errorf("mapping len = %d, want 8", len(got))
			}
		})
	}
}

// TestFabricRuntimeDeliveredReadsWhatGenerationRecords guards the two exported
// identifiers against drift: the predicate must find the override under the
// same component name and override key that recipe generation uses.
func TestFabricRuntimeDeliveredReadsWhatGenerationRecords(t *testing.T) {
	want := mapping(eightNets("gen")...)
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
		Name: recipe.KubeflowTrainerComponentName, ManifestFiles: []string{recipe.GKETCPXORuntimeManifest}}}}
	// Hand-build the record in the shape recipe generation writes (a list of
	// {interfaceName, network} maps under the exported override key), so the
	// reader is tested against that shape without needing a full catalog.
	raw := make([]any, 0, len(want))
	for _, e := range want {
		raw = append(raw, map[string]any{"interfaceName": e.InterfaceName, "network": e.Network})
	}
	r.ComponentRefs[0].Overrides = map[string]any{recipe.GKETCPXOInterfacesOverrideKey: raw}
	got, delivered, err := FabricRuntimeDelivered(r.ComponentRefs)
	if err != nil || !delivered {
		t.Fatalf("delivered=%v err=%v", delivered, err)
	}
	if err := VerifyMappingMatchesRecipe(want, got); err != nil {
		t.Fatalf("round-trip mismatch: %v", err)
	}
}

func TestParseInterfacesAnnotation(t *testing.T) {
	good := mapping(eightNets("c1")...)
	tests := []struct {
		name    string
		raw     string
		wantLen int
		wantErr string
	}{
		{"valid nine entries", annotationFor(good), 8, ""},
		{"not json", "eth0=default", 0, "not valid JSON"},
		{"empty list", "[]", 0, "is empty"},
		{"wrong first entry", `[{"interfaceName":"eth1","network":"x"}]`, 0, "first entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInterfacesAnnotation(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestDeployedTCPXOMapping(t *testing.T) {
	good := mapping(eightNets("c1")...)
	tests := []struct {
		name    string
		obj     *unstructured.Unstructured
		wantErr string
	}{
		{"wired runtime", ctrFixture(map[string]any{
			InterfacesAnnotation: annotationFor(good), DefaultInterfaceAnnotation: "eth0"}), ""},
		{"missing interfaces annotation", ctrFixture(map[string]any{DefaultInterfaceAnnotation: "eth0"}), "not fabric-wired"},
		{"wrong default interface", ctrFixture(map[string]any{
			InterfacesAnnotation: annotationFor(good), DefaultInterfaceAnnotation: "eth1"}), "want \"eth0\""},
		{"no node job", &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": TCPXORuntimeName},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"replicatedJobs": []any{map[string]any{"name": "launcher"}}}}}}}, "declares no \"node\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeployedTCPXOMapping(tt.obj)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := VerifyMappingMatchesRecipe(good, got); err != nil {
				t.Errorf("deployed mapping != fixture: %v", err)
			}
		})
	}
}

func TestVerifyMappingMatchesRecipe(t *testing.T) {
	a := mapping(eightNets("c1")...)
	reordered := append([]recipe.NetworkInterfaceMapping(nil), a...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	tests := []struct {
		name     string
		recorded []recipe.NetworkInterfaceMapping
		deployed []recipe.NetworkInterfaceMapping
		wantErr  string
	}{
		{"identical", a, append([]recipe.NetworkInterfaceMapping(nil), a...), ""},
		{"reordered is a mismatch", a, reordered, "at position 1"},
		{"length differs", a, a[:7], "maps 7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyMappingMatchesRecipe(tt.recorded, tt.deployed)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyNetworksExist(t *testing.T) {
	deployed := mapping(eightNets("c1")...)
	// Discovered names deliberately NOT in the deployed order, plus an extra
	// network the runtime does not use: membership, not position, is the test.
	discovered := []string{"c1-gpu-nic-7", "c1-gpu-nic-0", "unused-gpu-nic-9", "c1-gpu-nic-3",
		"c1-gpu-nic-1", "c1-gpu-nic-5", "c1-gpu-nic-2", "c1-gpu-nic-6", "c1-gpu-nic-4"}
	if err := VerifyNetworksExist(deployed, discovered); err != nil {
		t.Fatalf("all present, order-independent: unexpected error %v", err)
	}
	err := VerifyNetworksExist(deployed, discovered[:4])
	if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Fatalf("missing networks must be ErrCodeNotFound, got %v", err)
	}
}

func TestReadDeployedTCPXORuntimeNotFound(t *testing.T) {
	sch := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, map[schema.GroupVersionResource]string{
		ClusterTrainingRuntimeGVR: "ClusterTrainingRuntimeList",
	})
	_, err := ReadDeployedTCPXORuntime(t.Context(), dyn)
	if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Fatalf("absent runtime must surface as ErrCodeNotFound, got %v", err)
	}
	obj := ctrFixture(map[string]any{InterfacesAnnotation: annotationFor(mapping(eightNets("c1")...)), DefaultInterfaceAnnotation: "eth0"})
	dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, map[schema.GroupVersionResource]string{
		ClusterTrainingRuntimeGVR: "ClusterTrainingRuntimeList",
	}, obj)
	got, err := ReadDeployedTCPXORuntime(t.Context(), dyn)
	if err != nil || got.GetName() != TCPXORuntimeName {
		t.Fatalf("expected deployed runtime, got %v err=%v", got, err)
	}
}

// TestReadDeployedTCPXORuntimeAPIError pins the non-NotFound branch: an
// apiserver fault must surface as ErrCodeInternal, never be mistaken for the
// runtime being absent (which would file a transient error as a deployment
// defect).
func TestReadDeployedTCPXORuntimeAPIError(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ClusterTrainingRuntimeGVR: "ClusterTrainingRuntimeList",
	})
	dyn.PrependReactor("get", "clustertrainingruntimes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver: connection reset")
	})
	_, err := ReadDeployedTCPXORuntime(t.Context(), dyn)
	if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("apiserver fault must be ErrCodeInternal, got %v", err)
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Fatal("apiserver fault must not be classified as NotFound")
	}
}

func TestNodeTemplateOfMalformedShapes(t *testing.T) {
	tests := []struct {
		name    string
		obj     *unstructured.Unstructured
		wantErr string
	}{
		{"no replicatedJobs", &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": TCPXORuntimeName},
			"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{}}}}},
			"has no spec.template.spec.replicatedJobs"},
		{"non-map entry is skipped, node job lacks pod template", &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": TCPXORuntimeName},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"replicatedJobs": []any{"not-a-map", map[string]any{"name": TCPXONodeJob, "template": map[string]any{"spec": map[string]any{}}}}}}}}},
			"has no template.spec.template"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NodeTemplateOf(tt.obj)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestReadDeployedTCPXORuntimeClassifiesContextFailures: a stalled or canceled
// read is an outcome of the run's context, not an internal fault.
func TestReadDeployedTCPXORuntimeClassifiesContextFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want errors.ErrorCode
	}{
		{"deadline", context.DeadlineExceeded, errors.ErrCodeTimeout},
		{"canceled", context.Canceled, errors.ErrCodeCanceled},
		{"other", stderrors.New("boom"), errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			dyn.PrependReactor("get", "clustertrainingruntimes", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.err
			})
			_, err := ReadDeployedTCPXORuntime(context.Background(), dyn)
			if err == nil || !stderrors.Is(err, errors.New(tt.want, "")) {
				t.Fatalf("want %s, got %v", tt.want, err)
			}
		})
	}
}

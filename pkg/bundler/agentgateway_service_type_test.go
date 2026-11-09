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

package bundler

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestMake_AgentgatewayServiceTypeOverride verifies that the scalar
// --set agentgateway:serviceType=ClusterIP reaches the AgentgatewayParameters
// the bundler renders into the agentgateway-post chart, and that the source
// ranges it injects by default are dropped from the ClusterIP Service. This is the
// supported path to a Programmed Gateway on a cluster without a LoadBalancer
// provider (#3714).
func TestMake_AgentgatewayServiceTypeOverride(t *testing.T) {
	cfg := config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
		agentgatewayComponentName: {"serviceType": "ClusterIP"},
	}))
	b, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tmpDir := t.TempDir()
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.run/v1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{{
			Name:          agentgatewayComponentName,
			Version:       "v1.5.0",
			Type:          "helm",
			Source:        "oci://cr.agentgateway.dev/charts",
			ManifestFiles: []string{"components/agentgateway/manifests/inference-gateway.yaml"},
		}},
	}
	if _, makeErr := b.Make(context.Background(), recipeResult, tmpDir); makeErr != nil {
		t.Fatalf("Make() error = %v", makeErr)
	}

	matches, err := filepath.Glob(filepath.Join(tmpDir, "*-agentgateway-post", "templates", "inference-gateway.yaml"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("agentgateway-post rendered manifest: matches %v, err %v", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	if strings.Contains(string(data), "{{") {
		t.Fatalf("%s still holds template directives; the bundler renders post manifests at generation time:\n%s",
			matches[0], data)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var params map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode %s: %v", matches[0], err)
		}
		if doc["kind"] == "AgentgatewayParameters" {
			params = doc
		}
	}
	if params == nil {
		t.Fatalf("no AgentgatewayParameters rendered:\n%s", data)
	}
	spec, _ := params["spec"].(map[string]any)
	service, _ := spec["service"].(map[string]any)
	svcSpec, _ := service["spec"].(map[string]any)
	if got := svcSpec["type"]; got != "ClusterIP" {
		t.Errorf("spec.service.spec.type = %#v, want ClusterIP; rendered:\n%s", got, data)
	}
	if ranges, present := svcSpec["loadBalancerSourceRanges"]; present {
		t.Errorf("loadBalancerSourceRanges = %v rendered on a ClusterIP Service; the apiserver rejects that field "+
			"on any type but LoadBalancer", ranges)
	}
}

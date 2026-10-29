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
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// promqlScopeRenderTimeout covers an OCI or repository pull of the chart on a
// cold cache, which takes longer than defaults.ComponentRenderTimeout allows.
const promqlScopeRenderTimeout = 90 * time.Second

// TestPromQLScrapeScopeRendersOnChart renders the pinned kube-prometheus-stack
// chart with a stock Dynamo bundle's values and checks that the Prometheus CR
// carries the scope the bundler wrote, and that the chart renders nothing but
// the operator and Prometheus.
//
// Pulls the chart over the network, so -short skips it.
func TestPromQLScrapeScopeRendersOnChart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), promqlScopeRenderTimeout)
	defer cancel()

	result, err := recipe.NewBuilder().BuildFromCriteria(ctx, &recipe.Criteria{
		Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
		Intent: recipe.CriteriaIntentInference, OS: recipe.CriteriaOSUbuntu, Platform: recipe.CriteriaPlatformDynamo,
	})
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}
	b, err := New(WithConfig(config.NewConfig(config.WithBundlers([]string{testPromQLServer}))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	if _, err = b.Make(ctx, result, dir); err != nil {
		t.Fatalf("Make: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*-"+testPromQLServer, "values.yaml"))
	if len(matches) != 1 {
		t.Fatalf("stack values under %s = %v, want one file", dir, matches)
	}

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get(testPromQLServer)
	if comp == nil {
		t.Fatalf("%s not found in component registry", testPromQLServer)
	}
	rendered, err := aicrhelm.RenderChart(ctx, aicrhelm.ChartInput{
		Name:       testPromQLServer,
		Chart:      comp.Helm.DefaultChart,
		Repository: comp.Helm.DefaultRepository,
		Version:    comp.Helm.DefaultVersion,
		Namespace:  comp.Helm.DefaultNamespace,
		ValuesPath: matches[0],
	})
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, rendered)
	}

	var prometheus map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec map[string]any `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered chart: %v", err)
		}
		switch doc.Kind {
		case "Prometheus":
			prometheus = doc.Spec
		case "DaemonSet", "StatefulSet", "Alertmanager", "ThanosRuler", "PrometheusRule":
			t.Errorf("chart renders %s %s, want only the operator and Prometheus", doc.Kind, doc.Metadata.Name)
		case "Deployment":
			if doc.Metadata.Name != "kube-prometheus-operator" {
				t.Errorf("chart renders Deployment %s, want only the operator", doc.Metadata.Name)
			}
		}
	}
	if prometheus == nil {
		t.Fatal("chart renders no Prometheus")
	}

	scopeNamespaces := []any{"dynamo-system", "gpu-operator"}
	wantNamespaceSelector := map[string]any{"matchExpressions": []any{map[string]any{
		"key": namespaceNameLabel, "operator": "In", "values": scopeNamespaces,
	}}}
	assertYAMLEqual(t, "serviceMonitorNamespaceSelector", prometheus["serviceMonitorNamespaceSelector"], wantNamespaceSelector)
	assertYAMLEqual(t, "podMonitorNamespaceSelector", prometheus["podMonitorNamespaceSelector"], wantNamespaceSelector)
	assertYAMLEqual(t, "serviceMonitorSelector", prometheus["serviceMonitorSelector"], promqlMatchAllSelector())
	assertYAMLEqual(t, "podMonitorSelector", prometheus["podMonitorSelector"], promqlMatchAllSelector())

	classes, ok := prometheus["scrapeClasses"].([]any)
	if !ok || len(classes) != 1 {
		t.Fatalf("scrapeClasses = %v, want one class", prometheus["scrapeClasses"])
	}
	class, _ := classes[0].(map[string]any)
	if class["name"] != promqlScrapeClassName || class["default"] != true {
		t.Errorf("scrape class = %v, want the default %s", class, promqlScrapeClassName)
	}
	relabelings, _ := class["metricRelabelings"].([]any)
	if len(relabelings) != 1 {
		t.Fatalf("metricRelabelings = %v, want one keep rule", class["metricRelabelings"])
	}
	keep, _ := relabelings[0].(map[string]any)
	if regex, _ := keep["regex"].(string); keep["action"] != "keep" || regex == "" {
		t.Errorf("metricRelabelings[0] = %v, want a keep rule with a regex", keep)
	}
}

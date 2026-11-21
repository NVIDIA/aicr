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

package chart

import (
	"maps"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// podMonitorAPI is the API the PodMonitor needs the cluster to serve.
const podMonitorAPI = "monitoring.coreos.com/v1/PodMonitor"

// renderServing renders as render does, on a cluster serving apis.
func renderServing(t *testing.T, apis []string, set ...string) []unstructured.Unstructured {
	t.Helper()
	args := make([]string, 0, 7+2*len(apis)+2*len(set))
	args = append(args, "template", release, chartDir, "--namespace", operatorNS, "-f", valuesFile)
	for _, api := range apis {
		args = append(args, "--api-versions", api)
	}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	objs, out, err := renderArgs(ctxFor(t), args...)
	if err != nil {
		t.Fatal(out)
	}
	return objs
}

func podMonitors(objs []unstructured.Unstructured) []unstructured.Unstructured {
	var found []unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == "PodMonitor" {
			found = append(found, o)
		}
	}
	return found
}

// The PodMonitor renders only when enabled on a cluster that serves its
// kind, so an install before the Prometheus Operator CRDs still succeeds.
func TestPodMonitorRendersOnlyWhenEnabledAndServed(t *testing.T) {
	served := []string{podMonitorAPI}
	for _, tc := range []struct {
		name string
		apis []string
		set  []string
		want int
	}{
		{"disabled by default", served, nil, 0},
		{"enabled, kind not served", nil, []string{"operator.podMonitor.enabled=true"}, 0},
		{"enabled and served", served, []string{"operator.podMonitor.enabled=true"}, 1},
	} {
		if got := len(podMonitors(renderServing(t, tc.apis, tc.set...))); got != tc.want {
			t.Errorf("%s: %d PodMonitors, want %d", tc.name, got, tc.want)
		}
	}
}

// The PodMonitor selects exactly the operator's pods on their metrics port,
// with the labels a Prometheus selects it by and the chart's own labels
// kept.
func TestPodMonitorScrapesTheOperator(t *testing.T) {
	objs := renderServing(t, []string{podMonitorAPI}, "operator.podMonitor.enabled=true",
		"operator.podMonitor.labels.release=prometheus", "operator.podMonitor.labels.app\\.kubernetes\\.io/name=other",
		"operator.podMonitor.interval=30s")
	monitors := podMonitors(objs)
	if len(monitors) != 1 {
		t.Fatalf("%d PodMonitors", len(monitors))
	}
	pm := monitors[0]
	if pm.GetNamespace() != operatorNS {
		t.Errorf("namespace %q, want %q", pm.GetNamespace(), operatorNS)
	}
	labels := pm.GetLabels()
	if labels["release"] != "prometheus" || labels["app.kubernetes.io/name"] != "aicr-operator" {
		t.Errorf("labels %v: want release=prometheus and the chart's app.kubernetes.io/name", labels)
	}

	selector, _, _ := unstructured.NestedStringMap(pm.Object, "spec", "selector", "matchLabels")
	podLabels, _, _ := unstructured.NestedStringMap(find(t, objs, "Deployment", release).Object, "spec", "template", "metadata", "labels")
	if len(selector) == 0 || !maps.Equal(selector, podLabels) {
		t.Errorf("selector %v does not match the operator pod's labels %v", selector, podLabels)
	}
	endpoints, _, _ := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	if len(endpoints) != 1 {
		t.Fatalf("endpoints %v", endpoints)
	}
	ep, _ := endpoints[0].(map[string]any)
	if ep["port"] != "metrics" || ep["interval"] != "30s" {
		t.Errorf("endpoint %v, want port metrics every 30s", ep)
	}
	if !slices.Contains(containerPorts(t, objs), "metrics") {
		t.Error("the operator container has no port named metrics")
	}
}

func containerPorts(t *testing.T, objs []unstructured.Unstructured) []string {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(find(t, objs, "Deployment", release).Object, "spec", "template", "spec", "containers")
	var names []string
	for _, c := range containers {
		c, _ := c.(map[string]any)
		ports, _, _ := unstructured.NestedSlice(c, "ports")
		for _, p := range ports {
			if p, ok := p.(map[string]any); ok {
				name, _ := p["name"].(string)
				names = append(names, name)
			}
		}
	}
	return names
}

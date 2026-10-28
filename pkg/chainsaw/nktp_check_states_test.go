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

package chainsaw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nktpNamespace = "nktp"

// nktpCluster is a synthetic NKTP release, keyed by kind/name, that each case
// starts healthy and then faults.
type nktpCluster struct {
	crd        map[string]any
	apiService map[string]any
	objects    map[string]map[string]any
	slices     []map[string]any
	pods       []map[string]any
}

func nktpDeployment(name string, generation, replicas, available, updated, observed int64) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": nktpNamespace, "generation": generation},
		"spec":     map[string]any{"replicas": replicas},
		"status": map[string]any{
			"replicas": replicas, "availableReplicas": available,
			"updatedReplicas": updated, "observedGeneration": observed,
		},
	}
}

func nktpStatefulSet(name string, replicas, ready int64, revision string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": name, "namespace": nktpNamespace, "generation": int64(2)},
		"spec":     map[string]any{"replicas": replicas},
		"status": map[string]any{
			"replicas": replicas, "readyReplicas": ready, "updatedReplicas": replicas,
			"currentRevision": revision, "updateRevision": "rev-2", "observedGeneration": int64(2),
		},
	}
}

func nktpDaemonSet(name string, desired, ready, updated int64) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"name": name, "namespace": nktpNamespace, "generation": int64(1)},
		"status": map[string]any{
			"desiredNumberScheduled": desired, "numberReady": ready,
			"updatedNumberScheduled": updated, "observedGeneration": int64(1),
		},
	}
}

// nktpLGTMSlice is the EndpointSlice the lgtm Service owns, with one endpoint
// per ready value; a nil ready leaves the condition unset.
func nktpLGTMSlice(ready ...*bool) map[string]any {
	endpoints := make([]any, 0, len(ready))
	for _, r := range ready {
		conditions := map[string]any{}
		if r != nil {
			conditions["ready"] = *r
		}
		endpoints = append(endpoints, map[string]any{"addresses": []any{"10.0.0.7"}, "conditions": conditions})
	}
	return map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata": map[string]any{
			"name": "lgtm-x7k2p", "namespace": nktpNamespace,
			"labels": map[string]any{"kubernetes.io/service-name": "lgtm"},
		},
		"addressType": "IPv4",
		"endpoints":   endpoints,
	}
}

func nktpMetricsAPIService(available string) map[string]any {
	return map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
		"metadata": map[string]any{"name": "v1beta1.metrics.k8s.io"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Available", "status": available},
		}},
	}
}

func nktpPod(name, phase string, initWaiting string) map[string]any {
	status := map[string]any{
		"phase": phase,
		"containerStatuses": []any{
			map[string]any{"name": "otc-container", "state": map[string]any{"running": map[string]any{}}},
		},
	}
	if initWaiting != "" {
		status["initContainerStatuses"] = []any{
			map[string]any{"name": "otc-sidecar", "state": map[string]any{"waiting": map[string]any{"reason": initWaiting}}},
		}
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": nktpNamespace},
		"status":   status,
	}
}

func healthyNKTPCluster() *nktpCluster {
	c := &nktpCluster{
		crd: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": "opentelemetrycollectors.opentelemetry.io"},
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "NamesAccepted", "status": "True"},
				map[string]any{"type": "Established", "status": "True"},
			}},
		},
		apiService: nktpMetricsAPIService("True"),
		objects:    map[string]map[string]any{},
		slices:     []map[string]any{nktpLGTMSlice(new(true))},
		pods:       []map[string]any{nktpPod("nktp-gateway-metrics-collector-0", "Running", "")},
	}
	for _, name := range []string{
		"nktp-operator", "lgtm",
		"nktp-daemonset-targetallocator", "nktp-large-target-metrics-targetallocator", "nktp-watchdog-targetallocator",
		"nktp-ksm-unscheduled-pods-0", "nktp-ksm-workloads-0", "nktp-ksm-networking-0",
		"nktp-ksm-storage-0", "nktp-ksm-configuration-0", "nktp-ksm-cluster-0",
	} {
		c.objects["Deployment/"+name] = nktpDeployment(name, 3, 1, 1, 1, 3)
	}
	for _, name := range []string{
		"nktp-gateway-metrics-collector", "nktp-gateway-logs-collector", "nktp-gateway-traces-collector",
		"nktp-watchdog-collector", "nktp-large-target-metrics-collector", "nktp-cluster-events-collector",
	} {
		c.objects["StatefulSet/"+name] = nktpStatefulSet(name, 2, 2, "rev-2")
	}
	for _, name := range []string{"nktp-daemonset-collector", "nktp-ksm-pods", "nktp-node-exporter"} {
		c.objects["DaemonSet/"+name] = nktpDaemonSet(name, 4, 4, 4)
	}
	return c
}

// TestNKTPHealthCheckClusterStates drives the shipped NKTP health check
// through the in-process executor against synthetic cluster states. Each
// failing case pins the resource name the failure must report, so a case
// cannot pass because a different assertion happened to catch it.
func TestNKTPHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/nktp/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	tests := []struct {
		name       string
		fault      func(c *nktpCluster)
		wantPass   bool
		wantOutput string
	}{
		{name: "healthy release, otel-lgtm enabled, platform metrics API", wantPass: true},
		{name: "otel-lgtm disabled: no Deployment, no Service slice", wantPass: true, fault: func(c *nktpCluster) {
			delete(c.objects, "Deployment/lgtm")
			c.slices = nil
		}},
		{name: "otel-lgtm endpoint ready condition unset counts as ready", wantPass: true,
			fault: func(c *nktpCluster) { c.slices = []map[string]any{nktpLGTMSlice(nil)} }},
		{name: "otel-lgtm enabled but its Deployment deleted", wantOutput: "EndpointSlice nktp/lgtm-x7k2p",
			fault: func(c *nktpCluster) {
				delete(c.objects, "Deployment/lgtm")
				c.slices = []map[string]any{nktpLGTMSlice()}
			}},
		{name: "otel-lgtm pod not ready", wantOutput: "EndpointSlice nktp/lgtm-x7k2p",
			fault: func(c *nktpCluster) { c.slices = []map[string]any{nktpLGTMSlice(new(false))} }},
		{name: "metrics-server enabled and rolled out", wantPass: true, fault: func(c *nktpCluster) {
			c.objects["Deployment/nktp-metrics-server"] = nktpDeployment("nktp-metrics-server", 1, 1, 1, 1, 1)
		}},
		{name: "metrics-server enabled but its Deployment deleted", wantOutput: "v1beta1.metrics.k8s.io",
			fault: func(c *nktpCluster) { c.apiService = nktpMetricsAPIService("False") }},
		{name: "no resource metrics API at all", wantOutput: "v1beta1.metrics.k8s.io",
			fault: func(c *nktpCluster) { c.apiService = nil }},
		{name: "collector CRD missing", fault: func(c *nktpCluster) { c.crd = nil },
			wantOutput: "opentelemetrycollectors.opentelemetry.io"},
		{name: "collector CRD not Established", wantOutput: "opentelemetrycollectors.opentelemetry.io",
			fault: func(c *nktpCluster) {
				c.crd["status"] = map[string]any{"conditions": []any{
					map[string]any{"type": "Established", "status": "False"},
				}}
			}},
		{name: "operator missing", fault: func(c *nktpCluster) { delete(c.objects, "Deployment/nktp-operator") },
			wantOutput: "nktp-operator"},
		{name: "operator mid-upgrade on its old ReplicaSet", wantOutput: "nktp-operator",
			fault: func(c *nktpCluster) {
				c.objects["Deployment/nktp-operator"] = nktpDeployment("nktp-operator", 4, 1, 1, 0, 4)
			}},
		{name: "operator scaled to zero", wantOutput: "nktp-operator",
			fault: func(c *nktpCluster) {
				c.objects["Deployment/nktp-operator"] = nktpDeployment("nktp-operator", 3, 0, 0, 0, 3)
			}},
		{name: "gateway never built by the operator", wantOutput: "nktp-gateway-traces-collector",
			fault: func(c *nktpCluster) { delete(c.objects, "StatefulSet/nktp-gateway-traces-collector") }},
		{name: "watchdog partly ready", wantOutput: "nktp-watchdog-collector",
			fault: func(c *nktpCluster) {
				c.objects["StatefulSet/nktp-watchdog-collector"] = nktpStatefulSet("nktp-watchdog-collector", 2, 1, "rev-2")
			}},
		{name: "cluster events mid-rollout", wantOutput: "nktp-cluster-events-collector",
			fault: func(c *nktpCluster) {
				c.objects["StatefulSet/nktp-cluster-events-collector"] = nktpStatefulSet("nktp-cluster-events-collector", 1, 1, "rev-1")
			}},
		{name: "node collector scheduled nowhere", wantOutput: "nktp-daemonset-collector",
			fault: func(c *nktpCluster) {
				c.objects["DaemonSet/nktp-daemonset-collector"] = nktpDaemonSet("nktp-daemonset-collector", 0, 0, 0)
			}},
		{name: "node collector on the old revision", wantOutput: "nktp-daemonset-collector",
			fault: func(c *nktpCluster) {
				c.objects["DaemonSet/nktp-daemonset-collector"] = nktpDaemonSet("nktp-daemonset-collector", 4, 4, 3)
			}},
		{name: "target allocator missing", wantOutput: "nktp-large-target-metrics-targetallocator",
			fault: func(c *nktpCluster) { delete(c.objects, "Deployment/nktp-large-target-metrics-targetallocator") }},
		{name: "kube-state-metrics shard unavailable", wantOutput: "nktp-ksm-cluster-0",
			fault: func(c *nktpCluster) {
				c.objects["Deployment/nktp-ksm-cluster-0"] = nktpDeployment("nktp-ksm-cluster-0", 3, 1, 0, 1, 3)
			}},
		{name: "kube-state-metrics pods DaemonSet partly ready", wantOutput: "nktp-ksm-pods",
			fault: func(c *nktpCluster) {
				c.objects["DaemonSet/nktp-ksm-pods"] = nktpDaemonSet("nktp-ksm-pods", 4, 3, 4)
			}},
		{name: "node-exporter missing", wantOutput: "nktp-node-exporter",
			fault: func(c *nktpCluster) { delete(c.objects, "DaemonSet/nktp-node-exporter") }},
		{name: "otel-lgtm present but unavailable", wantOutput: "Deployment nktp/lgtm",
			fault: func(c *nktpCluster) {
				c.objects["Deployment/lgtm"] = nktpDeployment("lgtm", 3, 1, 0, 1, 3)
			}},
		{name: "metrics-server present but scaled to zero", wantOutput: "nktp-metrics-server",
			fault: func(c *nktpCluster) {
				c.objects["Deployment/nktp-metrics-server"] = nktpDeployment("nktp-metrics-server", 1, 0, 0, 0, 1)
			}},
		{name: "collector pod pending", wantOutput: "Pod nktp/nktp-gateway-logs-collector-1",
			fault: func(c *nktpCluster) {
				c.pods = append(c.pods, nktpPod("nktp-gateway-logs-collector-1", "Pending", ""))
			}},
		{name: "collector sidecar crash-looping", wantOutput: "Pod nktp/nktp-watchdog-collector-0",
			fault: func(c *nktpCluster) {
				c.pods = append(c.pods, nktpPod("nktp-watchdog-collector-0", "Running", "CrashLoopBackOff"))
			}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := healthyNKTPCluster()
			if tt.fault != nil {
				tt.fault(c)
			}
			fetcher := newFakeFetcher()
			if c.crd != nil {
				fetcher.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "",
					"opentelemetrycollectors.opentelemetry.io", c.crd)
			}
			if c.apiService != nil {
				fetcher.addGet("apiregistration.k8s.io/v1", "APIService", "", "v1beta1.metrics.k8s.io", c.apiService)
			}
			if c.slices != nil {
				fetcher.addList("discovery.k8s.io/v1", "EndpointSlice", nktpNamespace, c.slices)
			}
			for key, obj := range c.objects {
				kind, name, _ := strings.Cut(key, "/")
				fetcher.addGet("apps/v1", kind, nktpNamespace, name, obj)
			}
			fetcher.addList("v1", "Pod", nktpNamespace, c.pods)

			result := runChainsawTestInProcess(context.Background(), "nktp", string(data), 2*time.Second, fetcher)
			if result.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", result.Passed, tt.wantPass, result.Output)
			}
			if tt.wantOutput != "" && !strings.Contains(result.Output, tt.wantOutput) {
				t.Fatalf("output = %q, want it to name %q: the wrong assertion caught this state",
					result.Output, tt.wantOutput)
			}
		})
	}
}

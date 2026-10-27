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

package validations

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	logCollectorMixinFile = "nvsentinel-log-collector"
	// The images docs/user/container-images.md discloses for the mixin. The
	// static BOM cannot see a mixin-gated subchart, so these assertions are
	// what keep that note honest.
	logCollectorImage       = "ghcr.io/nvidia/nvsentinel/log-collector:v1.28.0"
	fileServerCleanupImage  = "ghcr.io/nvidia/nvsentinel/file-server-cleanup:v1.28.0"
	fileServerNginxImage    = "docker.io/bitnamilegacy/nginx:1.27.2-debian-12-r2"
	fileServerInitImage     = "docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"
	fileServerExporterImage = "quay.io/martinhelmich/prometheus-nginxlog-exporter:v1.10.0"
	fileServerName          = "nvsentinel-incluster-file-server"
)

// containerEnv returns a container's literal env values by name.
func containerEnv(container map[string]any) map[string]string {
	out := map[string]string{}
	list, _ := container["env"].([]any)
	for _, e := range list {
		entry, _ := e.(map[string]any)
		name, _ := entry["name"].(string)
		if value, ok := entry["value"].(string); ok {
			out[name] = value
		}
	}
	return out
}

// renderedConfigMapKey returns the value of key in the rendered ConfigMap
// that carries it.
func renderedConfigMapKey(t *testing.T, rendered, key string) string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode rendered chart: %v", err)
		}
		if doc["kind"] != "ConfigMap" {
			continue
		}
		data, _ := doc["data"].(map[string]any)
		if v, ok := data[key].(string); ok {
			return v
		}
	}
	t.Fatalf("no rendered ConfigMap carries %s", key)
	return ""
}

// TestNVSentinelLogCollectorChartRender renders the pinned nvsentinel chart
// with the remediation step and the log collector mixin composed, and asserts
// what #2619 promises reaches the workloads: every image the docs list, the
// file server's Service at the address the collector uploads to under any
// release name (the render helper's differs from every deployer's), its volume
// size and the storage class --storage-class writes, the cleanup cycle, and a
// collection Job with must-gather off.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelSlurmDrainMonitorChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelLogCollectorChartRender`.
func TestNVSentinelLogCollectorChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	values := nvsentinelRenderedValues(t, store, "nvsentinel-remediation", logCollectorMixinFile)
	// The value --storage-class writes through nvsentinel's storageClassPaths;
	// the injection itself is covered by the bundle check in the e2e.
	mergeValuesForRender(values, map[string]any{
		"incluster-file-server": map[string]any{"persistence": map[string]any{"storageClassName": "aicr-rwo"}},
	})
	out := renderNVSentinel(t, values)

	// The collector uploads to this Service: same name, namespace and port.
	collector, _ := values["fault-remediation"].(map[string]any)["logCollector"].(map[string]any)
	uploadURL, _ := collector["uploadURL"].(string)
	service := renderedObject(t, out, "Service", fileServerName)
	svcSpec, _ := service["spec"].(map[string]any)
	// helm template omits metadata.namespace, so the namespace is the one
	// every deployer installs nvsentinel into.
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	namespace := registry.Get("nvsentinel").Helm.DefaultNamespace
	if want := "http://" + fileServerName + "." + namespace + ".svc.cluster.local/upload"; uploadURL != want {
		t.Errorf("logCollector.uploadURL = %q, want the rendered Service %q", uploadURL, want)
	}
	ports, _ := svcSpec["ports"].([]any)
	httpPort := false
	for _, p := range ports {
		port, _ := p.(map[string]any)
		httpPort = httpPort || (port["name"] == "http" && port["port"] == 80)
	}
	if !httpPort {
		t.Errorf("file server Service ports = %v, want http on port 80, which the upload URL omits", ports)
	}

	claim := renderedObject(t, out, "PersistentVolumeClaim", fileServerName+"-data")
	claimSpec, _ := claim["spec"].(map[string]any)
	if got := claimSpec["storageClassName"]; got != "aicr-rwo" {
		t.Errorf("file server claim storageClassName = %v, want aicr-rwo from storageClassPaths", got)
	}
	if got := stringList(claimSpec["accessModes"]); !slices.Equal(got, []string{"ReadWriteOnce"}) {
		t.Errorf("file server claim accessModes = %v, want [ReadWriteOnce]", got)
	}
	requests, _ := claimSpec["resources"].(map[string]any)["requests"].(map[string]any)
	if requests["storage"] != "50Gi" {
		t.Errorf("file server claim size = %v, want 50Gi", requests["storage"])
	}

	server := podContainers(t, renderedObject(t, out, "Deployment", fileServerName))
	images := make([]string, 0, len(server))
	for _, c := range server {
		image, _ := c["image"].(string)
		images = append(images, image)
	}
	slices.Sort(images)
	want := []string{fileServerNginxImage, fileServerInitImage, fileServerCleanupImage, fileServerExporterImage}
	slices.Sort(want)
	if !slices.Equal(images, want) {
		t.Errorf("file server images = %v, want %v", images, want)
	}
	cleanup := containerEnv(server["log-cleanup"])
	if cleanup["LOG_RETENTION_DAYS"] != "7" || cleanup["LOG_CLEANUP_SLEEP_INTERVAL"] != "3600" {
		t.Errorf("log-cleanup env = %v, want LOG_RETENTION_DAYS=7 and LOG_CLEANUP_SLEEP_INTERVAL=3600", cleanup)
	}

	remediation := podContainers(t, renderedObject(t, out, "Deployment", "fault-remediation"))
	if got := containerEnv(remediation["fault-remediation"])["LOG_COLLECTOR_TIMEOUT"]; got != "10m" {
		t.Errorf("fault-remediation LOG_COLLECTOR_TIMEOUT = %q, want 10m", got)
	}

	var job map[string]any
	if err := yaml.Unmarshal([]byte(renderedConfigMapKey(t, out, "log-collector-job.yaml")), &job); err != nil {
		t.Fatalf("decode log-collector-job.yaml: %v", err)
	}
	jobContainers := podContainers(t, job)
	jobContainer := jobContainers["log-collector"]
	if jobContainer["image"] != logCollectorImage {
		t.Errorf("log collector Job image = %v, want %s", jobContainer["image"], logCollectorImage)
	}
	env := containerEnv(jobContainer)
	if env["ENABLE_GPU_OPERATOR_MUST_GATHER"] != "false" {
		t.Errorf("log collector ENABLE_GPU_OPERATOR_MUST_GATHER = %q, want false", env["ENABLE_GPU_OPERATOR_MUST_GATHER"])
	}
	if env["UPLOAD_URL_BASE"] != uploadURL {
		t.Errorf("log collector UPLOAD_URL_BASE = %q, want %q", env["UPLOAD_URL_BASE"], uploadURL)
	}
	if env["GPU_OPERATOR_NAMESPACE"] != "gpu-operator" {
		t.Errorf("log collector GPU_OPERATOR_NAMESPACE = %q, want gpu-operator", env["GPU_OPERATOR_NAMESPACE"])
	}

	// Negative control: the remediation step alone pulls in none of it.
	base := renderNVSentinel(t, nvsentinelRenderedValues(t, store, "nvsentinel-remediation"))
	for _, image := range []string{logCollectorImage, fileServerNginxImage, fileServerCleanupImage} {
		if strings.Contains(base, image) {
			t.Errorf("%s renders without the %s mixin", image, logCollectorMixinFile)
		}
	}
}

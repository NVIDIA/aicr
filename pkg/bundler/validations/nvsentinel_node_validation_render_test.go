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
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	lifecycleManagerName = "lifecycle-manager"
	// lifecycleManagerImage is the image docs/user/container-images.md
	// discloses for the mixin. The static BOM renders the default values, in
	// which this subchart is off, so this assertion keeps that note honest.
	lifecycleManagerImage = "ghcr.io/nvidia/nvsentinel/lifecycle-manager:v1.28.0"
	// dcgmDiagTestImage is named only inside lifecycle-manager's config.yaml.
	dcgmDiagTestImage = "nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04"
	// lifecycleManagerLabelPrefix is the chart's cordon label prefix, which
	// the mixin restates.
	lifecycleManagerLabelPrefix = "k8saas.nvidia.com/"
)

var (
	// dcgmServiceRe finds the cluster-local Services the dcgm-diag-test
	// command tries, and dcgmPortRe the port it dials them on.
	dcgmServiceRe = regexp.MustCompile(`\b[a-z0-9-]+\.[a-z0-9-]+\.svc\b`)
	dcgmPortRe    = regexp.MustCompile(`--host\s+"[^"]*:(\d+)"`)
)

// lifecycleManagerConfig returns the parsed spec of lifecycle-manager's
// config.yaml.
func lifecycleManagerConfig(t *testing.T, rendered string) map[string]any {
	t.Helper()
	configMap := renderedObject(t, rendered, "ConfigMap", lifecycleManagerName)
	data, _ := configMap["data"].(map[string]any)
	raw, _ := data["config.yaml"].(string)
	if raw == "" {
		t.Fatal("lifecycle-manager ConfigMap has no config.yaml")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parsing lifecycle-manager config.yaml: %v\n%s", err, raw)
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("lifecycle-manager config.yaml has no spec:\n%s", raw)
	}
	return spec
}

// TestNVSentinelNodeValidationChartRender renders the pinned nvsentinel chart
// with the node-validation mixin composed and asserts what #3088 promises
// reaches the workload: the pinned lifecycle-manager image with only its
// ValidationRequest controller, new-node validation keeping the cordoned and
// not-under-quarantine criteria and requesting dcgm-diag-test, and that test's image and the DCGM
// address CheckNVSentinelNodeValidationCoherent assumes. A chart bump that
// moves the address fails here rather than leaving the gate checking the
// wrong Service.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelHealthEventsAnalyzerChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelNodeValidationChartRender`.
func TestNVSentinelNodeValidationChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, nodeValidationMixin))

	deployment := renderedObject(t, out, "Deployment", lifecycleManagerName)
	container, ok := podContainers(t, deployment)[lifecycleManagerName]
	if !ok {
		t.Fatal("lifecycle-manager Deployment has no lifecycle-manager container")
	}
	if got := container["image"]; got != lifecycleManagerImage {
		t.Errorf("lifecycle-manager image = %v, want %s", got, lifecycleManagerImage)
	}
	args := stringList(container["args"])
	for _, want := range []string{"--enable-validation-controller=true", "--enable-maintenance-controller=false"} {
		if !slices.Contains(args, want) {
			t.Errorf("lifecycle-manager args %v do not carry %s", args, want)
		}
	}

	spec := lifecycleManagerConfig(t, out)
	newNode, _ := spec["newNodeValidation"].(map[string]any)
	if newNode == nil {
		t.Fatal("config.yaml has no newNodeValidation; new GPU nodes would never be validated")
	}
	if tests := stringList(newNode["newNodeTests"]); !slices.Equal(tests, []string{nodeValidationDCGMTest}) {
		t.Errorf("newNodeValidation.newNodeTests = %v, want [%s]", tests, nodeValidationDCGMTest)
	}
	criteria, _ := newNode["criteria"].([]any)
	for name, want := range map[string]string{
		nodeValidationCordonedCriterion:   nodeValidationCordonedExpression,
		nodeValidationQuarantineCriterion: nodeValidationQuarantineExpression,
	} {
		found := slices.ContainsFunc(criteria, func(item any) bool {
			criterion, _ := item.(map[string]any)
			expression, _ := criterion["expression"].(string)
			return criterion["name"] == name && strings.Join(strings.Fields(expression), " ") == want
		})
		if !found {
			t.Errorf("newNodeValidation.criteria %v lack the %s criterion %q", criteria, name, want)
		}
	}
	gate, _ := spec["schedulingGate"].(map[string]any)
	cordon, _ := gate["cordon"].(map[string]any)
	if cordon["remove"] != true || cordon["labelPrefix"] != lifecycleManagerLabelPrefix {
		t.Errorf("schedulingGate.cordon = %v, want remove: true and labelPrefix %s", cordon, lifecycleManagerLabelPrefix)
	}

	tests, _ := spec["tests"].(map[string]any)
	dcgmTest, _ := tests[nodeValidationDCGMTest].(map[string]any)
	if dcgmTest == nil {
		t.Fatalf("config.yaml tests = %v, want %s defined", tests, nodeValidationDCGMTest)
	}
	if got := dcgmTest["image"]; got != dcgmDiagTestImage {
		t.Errorf("%s image = %v, want %s", nodeValidationDCGMTest, got, dcgmDiagTestImage)
	}
	command := strings.Join(stringList(dcgmTest["command"]), "\n")
	services := dcgmServiceRe.FindAllString(command, -1)
	slices.Sort(services)
	services = slices.Compact(services)
	wantServices := []string{
		"nvidia-dcgm-dra." + nodeValidationDCGMNamespace + ".svc",
		gpuOperatorDCGMService + "." + nodeValidationDCGMNamespace + ".svc",
	}
	if !slices.Equal(services, wantServices) {
		t.Errorf("%s command reaches %v, want exactly %v: CheckNVSentinelNodeValidationCoherent checks the GPU "+
			"Operator that serves those", nodeValidationDCGMTest, services, wantServices)
	}
	ports := dcgmPortRe.FindAllStringSubmatch(command, -1)
	if len(ports) == 0 {
		t.Errorf("%s command dials no --host \"<host>:<port>\":\n%s", nodeValidationDCGMTest, command)
	}
	for _, port := range ports {
		if port[1] != gpuOperatorDCGMPort {
			t.Errorf("%s command dials port %s, want %s", nodeValidationDCGMTest, port[1], gpuOperatorDCGMPort)
		}
	}

	// Negative control: without the mixin nothing of lifecycle-manager renders.
	base := renderNVSentinel(t, nvsentinelRenderedValues(t, store, ""))
	if strings.Contains(base, lifecycleManagerImage) {
		t.Errorf("%s renders without the %s mixin", lifecycleManagerImage, nodeValidationMixin)
	}
}

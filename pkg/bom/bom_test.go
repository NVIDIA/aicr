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

package bom

import (
	"bytes"
	"strings"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

func sampleResults() []ComponentResult {
	return []ComponentResult{
		{
			Name:        "gpu-operator",
			DisplayName: "gpu-operator",
			Type:        "helm",
			Repository:  "https://helm.ngc.nvidia.com/nvidia",
			Chart:       "nvidia/gpu-operator",
			Version:     "v25.3.0",
			Namespace:   "gpu-operator",
			Pinned:      true,
			Images: []string{
				"nvcr.io/nvidia/gpu-operator:v25.3.0",
				"nvcr.io/nvidia/k8s/dcgm-exporter:4.2.0",
			},
		},
		{
			Name:    "nfd",
			Type:    "helm",
			Chart:   "node-feature-discovery",
			Version: "",
			Pinned:  false,
			Images:  []string{"registry.k8s.io/nfd/node-feature-discovery:v0.18.1"},
			Warnings: []string{
				"helm template render warning",
			},
		},
	}
}

func TestBuildBOM_Structure(t *testing.T) {
	doc := BuildBOM(Metadata{
		Name:        "aicr",
		Version:     "v0.12.1",
		Description: "NVIDIA AI Cluster Runtime",
		ToolName:    "aicr-bom",
		ToolVersion: "v0.12.1",
	}, sampleResults())

	if doc.SerialNumber == "" || !strings.HasPrefix(doc.SerialNumber, "urn:uuid:") {
		t.Errorf("expected urn:uuid: serial number, got %q", doc.SerialNumber)
	}
	if doc.Metadata == nil || doc.Metadata.Component == nil {
		t.Fatal("metadata.component is nil")
	}
	if doc.Metadata.Component.Name != "aicr" {
		t.Errorf("metadata.component.name = %q, want aicr", doc.Metadata.Component.Name)
	}
	if doc.Metadata.Component.Supplier == nil || doc.Metadata.Component.Supplier.Name != "NVIDIA Corporation" {
		t.Errorf("metadata.component.supplier should default to NVIDIA Corporation")
	}

	if doc.Components == nil {
		t.Fatal("components is nil")
	}
	// 2 component apps + 3 unique container images = 5
	if got := len(*doc.Components); got != 5 {
		t.Errorf("components count: got %d, want 5", got)
	}

	// Ensure each image has a purl prefixed with pkg:oci/
	containers := 0
	for _, c := range *doc.Components {
		if c.Type == cdx.ComponentTypeContainer {
			containers++
			if !strings.HasPrefix(c.PackageURL, "pkg:oci/") {
				t.Errorf("container component %q missing OCI purl: %q", c.Name, c.PackageURL)
			}
		}
	}
	if containers != 3 {
		t.Errorf("container components: got %d, want 3", containers)
	}

	if doc.Dependencies == nil {
		t.Fatal("dependencies is nil")
	}
	// Root dep + one per component with images = 1 + 2 = 3
	if got := len(*doc.Dependencies); got != 3 {
		t.Errorf("dependencies count: got %d, want 3", got)
	}
}

func TestBuildBOM_DefaultsApplied(t *testing.T) {
	doc := BuildBOM(Metadata{}, nil)
	if doc.Metadata.Component.Name != "aicr" {
		t.Errorf("empty metadata should default name to aicr, got %q", doc.Metadata.Component.Name)
	}
	if doc.Metadata.Component.Supplier.Name != "NVIDIA Corporation" {
		t.Errorf("empty metadata should default supplier")
	}
}

func TestBuildBOM_DeduplicatesImages(t *testing.T) {
	results := []ComponentResult{
		{Name: "a", Type: "helm", Images: []string{"img:v1", "shared/x:v1"}},
		{Name: "b", Type: "helm", Images: []string{"shared/x:v1", "img:v2"}},
	}
	doc := BuildBOM(Metadata{Name: "aicr"}, results)
	containers := 0
	for _, c := range *doc.Components {
		if c.Type == cdx.ComponentTypeContainer {
			containers++
		}
	}
	if containers != 3 {
		t.Errorf("dedup failed: got %d unique images, want 3", containers)
	}
}

func TestWriteMarkdown(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMarkdown(&buf, Metadata{Name: "aicr", Version: "v0.12.1", Description: "AICR"}, sampleResults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"# AICR — Container Image Inventory",
		"Components: **2**",
		"Unique images: **3**",
		"gpu-operator",
		"nvcr.io/nvidia/gpu-operator:v25.3.0",
		"helm template render warning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestWriteMarkdown_DeterministicSuppressesGenerationLine(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMarkdown(&buf, Metadata{
		Name:          "aicr",
		Version:       "v0.13.0",
		Description:   "AICR",
		Deterministic: true,
	}, sampleResults())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "_Generated ") {
		t.Errorf("deterministic output should omit the _Generated ..._ line, got:\n%s", out)
	}
	if !strings.Contains(out, "Components: **2**") {
		t.Errorf("deterministic output should still contain the table content")
	}
}

func TestWriteMarkdown_EmptyComponentNoImages(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMarkdown(&buf, Metadata{Name: "aicr"}, []ComponentResult{
		{Name: "empty", Type: "helm"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "_No images extracted._") {
		t.Errorf("expected empty-component marker in output:\n%s", buf.String())
	}
}

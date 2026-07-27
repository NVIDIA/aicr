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

package k8s

import (
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const slinkyV1Beta1SchemaFixture = "slinky-v1.2.0-crd-schema.yaml"

func TestSlinkyProjectionPathsMatchPinnedCRDs(t *testing.T) {
	t.Parallel()

	schemas := loadSlinkyV1Beta1Schemas(t)
	tests := []struct {
		kind     string
		path     []string
		wantType string
	}{
		{
			kind:     slinkyControllerKind,
			path:     []string{slinkyFieldClusterName},
			wantType: "string",
		},
		{
			kind:     slinkyControllerKind,
			path:     []string{slinkyFieldExternal},
			wantType: "boolean",
		},
		{
			kind: slinkyControllerKind,
			path: []string{
				slinkyFieldAccountingRef,
				slinkyFieldName,
			},
			wantType: "string",
		},
		{
			kind: slinkyNodeSetKind,
			path: []string{
				slinkyFieldControllerRef,
				slinkyFieldName,
			},
			wantType: "string",
		},
		{
			kind: slinkyNodeSetKind,
			path: []string{
				slinkyFieldPartition,
				slinkyFieldEnabled,
			},
			wantType: "boolean",
		},
		{
			kind: slinkyLoginSetKind,
			path: []string{
				slinkyFieldControllerRef,
				slinkyFieldName,
			},
			wantType: "string",
		},
		{
			kind: slinkyRestAPIKind,
			path: []string{
				slinkyFieldControllerRef,
				slinkyFieldName,
			},
			wantType: "string",
		},
		{
			kind:     slinkyAccountingKind,
			path:     []string{slinkyFieldExternal},
			wantType: "boolean",
		},
	}

	for _, tt := range tests {
		name := tt.kind + "." + strings.Join(tt.path, ".")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			properties, ok := schemas[tt.kind]
			if !ok {
				t.Fatalf("schema for kind %q not found", tt.kind)
			}
			typePath := make([]string, 0, len(tt.path)*2)
			for i, field := range tt.path {
				typePath = append(typePath, field)
				if i < len(tt.path)-1 {
					typePath = append(typePath, "properties")
				}
			}
			typePath = append(typePath, "type")
			got, found, err := unstructured.NestedString(properties, typePath...)
			if err != nil {
				t.Fatalf("read schema path %q: %v", strings.Join(typePath, "."), err)
			}
			if !found {
				t.Fatalf(
					"production projection path %q is absent from pinned %s schema",
					strings.Join(tt.path, "."),
					slinkyV1Beta1SchemaFixture,
				)
			}
			if got != tt.wantType {
				t.Errorf(
					"schema type at %q = %q, want %q",
					strings.Join(tt.path, "."),
					got,
					tt.wantType,
				)
			}
		})
	}
}

func loadSlinkyV1Beta1Schemas(t *testing.T) map[string]map[string]any {
	t.Helper()

	path := filepath.Join("testdata", slinkyV1Beta1SchemaFixture)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open pinned Slinky CRD schema fixture: %v", err)
	}
	defer file.Close()

	schemas := make(map[string]map[string]any)
	decoder := yaml.NewDecoder(file)
	for {
		document := make(map[string]any)
		if err := decoder.Decode(&document); err != nil {
			if stderrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode pinned Slinky CRD schema fixture: %v", err)
		}

		kind, found, err := unstructured.NestedString(document, "spec", "names", "kind")
		if err != nil || !found || kind == "" {
			t.Fatalf("fixture document has invalid CRD kind: found=%t error=%v", found, err)
		}
		versions, found, err := unstructured.NestedSlice(document, "spec", "versions")
		if err != nil || !found {
			t.Fatalf("fixture schema for %s has invalid versions: found=%t error=%v", kind, found, err)
		}

		versionFound := false
		for _, rawVersion := range versions {
			version, ok := rawVersion.(map[string]any)
			if !ok || version["name"] != "v1beta1" {
				continue
			}
			properties, found, err := unstructured.NestedMap(
				version,
				"schema",
				"openAPIV3Schema",
				"properties",
				slinkyFieldSpec,
				"properties",
			)
			if err != nil || !found {
				t.Fatalf(
					"fixture schema for %s has invalid spec properties: found=%t error=%v",
					kind,
					found,
					err,
				)
			}
			schemas[kind] = properties
			versionFound = true
			break
		}
		if !versionFound {
			t.Fatalf("fixture schema for %s lacks v1beta1", kind)
		}
	}

	if len(schemas) != 5 {
		t.Fatalf("loaded %d Slinky schemas, want 5", len(schemas))
	}
	return schemas
}

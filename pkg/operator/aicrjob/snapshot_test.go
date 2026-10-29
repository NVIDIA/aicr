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

package aicrjob

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

const snapshotFixture = "snapshot-kind-no-gpu.json"

func TestParseSnapshotFixture(t *testing.T) {
	t.Parallel()
	s, err := ParseSnapshot(fixture(t, snapshotFixture), "v1.0.0")
	if err != nil {
		t.Fatalf("ParseSnapshot() = %v", err)
	}
	types := map[string]bool{}
	for _, m := range s.Measurements {
		types[m.Type] = true
	}
	for _, want := range []string{"GPU", "K8s", "NodeTopology", "OS", "SystemD"} {
		if !types[want] {
			t.Errorf("measurement %s missing; have %v", want, types)
		}
	}
	// A kind node without a GPU: AICR names the service and nothing else.
	fp := s.Fingerprint
	if fp == nil || fp.Service.Value != "kind" || fp.Service.Source != "k8s.node.provider" || fp.Accelerator.Value != "" || fp.OS.Value != "" {
		t.Errorf("fingerprint = %+v", fp)
	}
}

func TestParseSnapshotRejects(t *testing.T) {
	t.Parallel()
	base := fixture(t, snapshotFixture)
	edit := func(f func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	tests := []struct {
		name string
		data []byte
	}{
		{"other kind", edit(func(m map[string]any) { m["kind"] = "RecipeResult" })},
		{"other apiVersion", edit(func(m map[string]any) { m["apiVersion"] = "aicr.run/v1alpha2" })},
		// AICR's sample snapshots predate the version stamp; the gate needs it.
		{"no version", edit(func(m map[string]any) { delete(m["metadata"].(map[string]any), "version") })},
		{"other version", edit(func(m map[string]any) { m["metadata"].(map[string]any)["version"] = "1.1.0" })},
		{"null measurements", edit(func(m map[string]any) { m["measurements"] = nil })},
		{"untyped measurement", edit(func(m map[string]any) { m["measurements"].([]any)[0].(map[string]any)["type"] = "" })},
		{"not JSON", []byte("kind: Snapshot")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseSnapshot(tt.data, "v1.0.0"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("ParseSnapshot() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

// Contract test against AICR's published schema: every field the operator
// reads exists in the schema, and every field it requires is read.
func TestSnapshotMatchesAICRSchema(t *testing.T) {
	t.Parallel()
	type object struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Items      json.RawMessage            `json:"items"`
	}
	decode := func(raw []byte) object {
		var o object
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
		return o
	}
	check := func(path string, o object, typ reflect.Type) {
		ours := jsonFields(typ)
		for _, f := range ours {
			if _, ok := o.Properties[f]; !ok {
				t.Errorf("operator reads %s%s, which AICR's schema does not define", path, f)
			}
		}
		for _, f := range o.Required {
			if !contains(ours, f) {
				t.Errorf("AICR's schema requires %s%s, which the operator does not read", path, f)
			}
		}
	}
	root := decode(fixture(t, filepath.Join("schemas", "Snapshot.schema.json")))
	check("", root, reflect.TypeFor[Snapshot]())
	measurement := decode(decode(root.Properties["measurements"]).Items)
	check("measurements[].", measurement, reflect.TypeFor[Measurement]())
	subtype := decode(decode(measurement.Properties["subtypes"]).Items)
	check("measurements[].subtypes[].", subtype, reflect.TypeFor[Subtype]())
}

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

package aicr

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCriteriaSerializationUsesStableLowercaseKeys(t *testing.T) {
	criteria := Criteria{Service: "eks", Nodes: 0}

	jsonBytes, err := json.Marshal(criteria)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var jsonFields map[string]any
	if err = json.Unmarshal(jsonBytes, &jsonFields); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := jsonFields["service"]; got != "eks" {
		t.Errorf("JSON service = %v, want eks", got)
	}
	if _, ok := jsonFields["Service"]; ok {
		t.Error("JSON must not expose the Go field name Service")
	}
	if _, ok := jsonFields["nodes"]; ok {
		t.Error("JSON must omit an unspecified nodes value")
	}

	yamlBytes, err := yaml.Marshal(criteria)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var yamlFields map[string]any
	if err = yaml.Unmarshal(yamlBytes, &yamlFields); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if got := yamlFields["service"]; got != "eks" {
		t.Errorf("YAML service = %v, want eks", got)
	}
	if _, ok := yamlFields["Service"]; ok {
		t.Error("YAML must not expose the Go field name Service")
	}
	if _, ok := yamlFields["nodes"]; ok {
		t.Error("YAML must omit an unspecified nodes value")
	}
}

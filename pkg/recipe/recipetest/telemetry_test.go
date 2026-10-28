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

package recipetest

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestTelemetryDataProvider(t *testing.T) {
	embedded, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("load embedded registry: %v", err)
	}
	registry, err := recipe.GetComponentRegistryFor(TelemetryDataProvider(t, DefaultPromQLFixture()))
	if err != nil {
		t.Fatalf("load layered registry: %v", err)
	}
	if registry.Count() != embedded.Count() {
		t.Errorf("layered registry has %d components, want %d", registry.Count(), embedded.Count())
	}
	for name, want := range DefaultPromQLFixture() {
		got := *registry.Get(name)
		if !reflect.DeepEqual(got.Telemetry, want) {
			t.Errorf("%s telemetry = %+v, want %+v", name, got.Telemetry, want)
		}
		if !reflect.DeepEqual(got.Helm, embedded.Get(name).Helm) || got.Class != embedded.Get(name).Class {
			t.Errorf("%s differs from its embedded entry beyond telemetry", name)
		}
	}
}

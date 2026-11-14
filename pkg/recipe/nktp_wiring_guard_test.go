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

package recipe_test

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	nktpComponent  = "nktp"
	nktpValuesFile = "components/nktp/values.yaml"
	// Chart default true; prometheus-operator-crds owns these CRDs.
	nktpInstallPrometheusCRDsPath = "opentelemetry-kube-stack.crds.installPrometheus"
)

// TestNKTPComponentRefsUseAICRValues fails when a leaf that enables nktp does
// not resolve its values from components/nktp/values.yaml with the chart's
// Prometheus CRDs off. Without that file the chart installs its own copies of
// the monitoring.coreos.com CRDs, which Flux's CreateReplace would then
// replace on every upgrade.
func TestNKTPComponentRefsUseAICRValues(t *testing.T) {
	ctx := context.Background()
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	var checked int
	for _, leaf := range leaves {
		if leaf.Err != nil {
			continue
		}
		ref := leaf.Result.GetComponentRef(nktpComponent)
		if ref == nil || !ref.IsEnabled() {
			continue
		}
		checked++
		if ref.ValuesFile != nktpValuesFile {
			t.Errorf("%s: componentRef %s sets valuesFile %q, want %q",
				leaf.Entry.Name, nktpComponent, ref.ValuesFile, nktpValuesFile)
		}
		values, valuesErr := leaf.Result.GetValuesForComponentWithContext(ctx, nktpComponent)
		if valuesErr != nil {
			t.Errorf("%s: resolve %s values: %v", leaf.Entry.Name, nktpComponent, valuesErr)
			continue
		}
		if got, ok := component.GetValueByPath(values, nktpInstallPrometheusCRDsPath); !ok || got != false {
			t.Errorf("%s: %s = %v, want false", leaf.Entry.Name, nktpInstallPrometheusCRDsPath, got)
		}
	}
	if checked == 0 {
		t.Fatal("no leaf enables nktp; this test would prove nothing")
	}
}

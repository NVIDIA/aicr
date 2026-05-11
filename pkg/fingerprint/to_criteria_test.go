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

package fingerprint

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestToCriteria_Populated(t *testing.T) {
	fp := h100Fingerprint()
	c := fp.ToCriteria()
	if c.Service != recipe.CriteriaServiceEKS {
		t.Errorf("Service = %v, want eks", c.Service)
	}
	if c.Accelerator != recipe.CriteriaAcceleratorH100 {
		t.Errorf("Accelerator = %v, want h100", c.Accelerator)
	}
	if c.OS != recipe.CriteriaOSUbuntu {
		t.Errorf("OS = %v, want ubuntu", c.OS)
	}
	if c.Nodes != 12 {
		t.Errorf("Nodes = %d, want 12", c.Nodes)
	}
	// Intent and Platform are not detectable -> stay any.
	if c.Intent != recipe.CriteriaIntentAny {
		t.Errorf("Intent = %v, want any", c.Intent)
	}
	if c.Platform != recipe.CriteriaPlatformAny {
		t.Errorf("Platform = %v, want any", c.Platform)
	}
}

func TestToCriteria_Empty(t *testing.T) {
	c := (&Fingerprint{}).ToCriteria()
	nonDefault := c.Service != recipe.CriteriaServiceAny ||
		c.Accelerator != recipe.CriteriaAcceleratorAny ||
		c.OS != recipe.CriteriaOSAny || c.Nodes != 0
	if nonDefault {
		t.Errorf("expected all-any criteria from empty fingerprint, got %+v", c)
	}
}

func TestToCriteria_Nil(t *testing.T) {
	var fp *Fingerprint
	c := fp.ToCriteria()
	if c == nil {
		t.Fatal("ToCriteria on nil fingerprint should return non-nil criteria")
	}
	if c.Service != recipe.CriteriaServiceAny {
		t.Errorf("expected any service from nil fingerprint, got %v", c.Service)
	}
}

func TestToCriteria_UnknownDimensionStaysAny(t *testing.T) {
	// An unknown service value should not be parsed; the criteria stays "any".
	fp := &Fingerprint{Service: Dimension{Value: "unknown-cloud"}}
	c := fp.ToCriteria()
	// ParseCriteriaServiceType returns Any + error for unknown values, so
	// our code preserves NewCriteria's any default.
	if c.Service != recipe.CriteriaServiceAny {
		t.Errorf("Service = %v, want any (unknown value should not parse)", c.Service)
	}
}

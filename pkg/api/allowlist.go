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

package api

import (
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// validateAgainstAllowLists runs the upstream pkg/recipe.AllowLists check
// for an in-tree handler holding both a facade *aicr.AllowLists and a
// freshly-parsed *recipe.Criteria. The handler runs this explicit pre-check
// so the user-facing error message stays exactly the one the legacy handler
// emitted; the Client's internal enforcement on Resolve/MakeBundle remains a
// backstop for callers that go straight to the facade methods.
//
// The translation is a thin string-slice → enum-slice projection; it is not
// in the hot path (handlers run it once per request).
func validateAgainstAllowLists(al *aicr.AllowLists, c *recipe.Criteria) error {
	if al == nil {
		return nil
	}
	internal := &recipe.AllowLists{
		Accelerators: convertToTypes[recipe.CriteriaAcceleratorType](al.Accelerators),
		Services:     convertToTypes[recipe.CriteriaServiceType](al.Services),
		Intents:      convertToTypes[recipe.CriteriaIntentType](al.Intents),
		OSTypes:      convertToTypes[recipe.CriteriaOSType](al.OSTypes),
	}
	return internal.ValidateCriteria(c)
}

func convertToTypes[T ~string](in []string) []T {
	if len(in) == 0 {
		return nil
	}
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = T(v)
	}
	return out
}

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

import "github.com/NVIDIA/aicr/pkg/recipe"

// ToCriteria projects the fingerprint into a recipe.Criteria so the
// recipe builder can match against a captured cluster (the
// `aicr recipe --snapshot` flow). Dimensions the fingerprint did not
// resolve become "any" via the recipe package's enum parsers — a
// fingerprint with no accelerator detected yields a generic Criteria
// that matches recipes pinning any accelerator.
//
// Intent and Platform are recipe-author choices the cluster cannot
// reveal, so they always come back as "any"; callers wanting to drive
// recipe selection by intent or platform must layer that on top of
// ToCriteria from the CLI flag side.
func (f *Fingerprint) ToCriteria() *recipe.Criteria {
	c := recipe.NewCriteria()
	if f == nil {
		return c
	}
	if v, err := recipe.ParseCriteriaServiceType(f.Service.Value); err == nil {
		c.Service = v
	}
	if v, err := recipe.ParseCriteriaAcceleratorType(f.Accelerator.Value); err == nil {
		c.Accelerator = v
	}
	if v, err := recipe.ParseCriteriaOSType(f.OS.Value); err == nil {
		c.OS = v
	}
	if f.NodeCount.Value > 0 {
		c.Nodes = f.NodeCount.Value
	}
	return c
}

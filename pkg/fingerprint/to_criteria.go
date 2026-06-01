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

// ToCriteria projects the fingerprint into a recipe.Criteria, resolving each
// enum value against reg so non-OSS values contributed by a `--data` overlay
// are honored. A nil reg falls back to a fresh ephemeral registry (only the
// hardcoded OSS fast-path values will validate); callers that need overlay
// values to flow through should pass the registry bound to their Client
// (via Client.CriteriaRegistry).
//
// Intent and Platform are recipe-author choices the cluster cannot reveal,
// so they always come back as "any"; callers wanting to drive recipe
// selection by intent or platform must layer that on top of ToCriteria
// from the CLI flag side.
func (f *Fingerprint) ToCriteria(reg *recipe.CriteriaRegistry) *recipe.Criteria {
	c := recipe.NewCriteria()
	if f == nil {
		return c
	}
	if reg == nil {
		reg = recipe.NewCriteriaRegistry()
	}
	if v, err := reg.ParseService(f.Service.Value); err == nil {
		c.Service = v
	}
	if v, err := reg.ParseAccelerator(f.Accelerator.Value); err == nil {
		c.Accelerator = v
	}
	if v, err := reg.ParseOS(f.OS.Value); err == nil {
		c.OS = v
	}
	if f.NodeCount.Value > 0 {
		c.Nodes = f.NodeCount.Value
	}
	return c
}

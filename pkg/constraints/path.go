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

package constraints

import (
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// ConstraintPath is a parsed fully qualified constraint path.
//
// Parsing and extraction live in pkg/measurement (issue #1783): the recipe
// loader validates constraint paths against the measurement catalog at load
// time, and pkg/constraints cannot be imported from pkg/recipe — this package
// imports pkg/recipe for recipe.Constraint, so the dependency only runs one
// way. Co-locating the path grammar with the catalog also keeps the two in
// sync: the catalog's job is to describe exactly the paths Extract resolves.
type ConstraintPath = measurement.Path

// ParseConstraintPath parses a fully qualified constraint path.
//
// This is well-formedness only. Callers that need the path to actually be
// addressable — that a supported snapshot producer can emit it — want
// measurement.ValidatePath, which the recipe loader applies to every
// constraint name at load time.
func ParseConstraintPath(path string) (*ConstraintPath, error) {
	return measurement.ParsePath(path)
}

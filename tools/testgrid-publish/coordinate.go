// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

// Local implementation of the coordinate mapping from NVIDIA/aicr#1409
// (pkg/recipe/coordinate.go). Once that PR merges and is tagged, replace
// this file with:
//
//	import "github.com/NVIDIA/aicr/pkg/recipe"
//	coord, err := recipe.CoordinateFor(criteria)
//
// The API here is intentionally identical to #1409 so the switch is a
// find-replace with no logic change.

import "github.com/NVIDIA/aicr/pkg/errors"

const criteriaAnyValue = "any"

// Coordinate is the canonical placement of an evidence bundle.
// See https://github.com/NVIDIA/aicr/issues/1272 and PR #1409.
type Coordinate struct {
	Group     string // service, e.g. "eks"
	Dashboard string // accelerator-os, e.g. "h100-ubuntu"
	Tab       string // intent[-platform], e.g. "training" or "training-kubeflow"
}

// Path returns "<group>/<dashboard>/<tab>" — the GCS infix for
// groups/{Path}/{build-id}/.
func (co Coordinate) Path() string {
	return co.Group + "/" + co.Dashboard + "/" + co.Tab
}

func (co Coordinate) String() string { return co.Path() }

// RecipeCriteria holds the resolved dimensions extracted from recipe.yaml.
type RecipeCriteria struct {
	Service      string // e.g. "eks"
	Accelerator  string // e.g. "h100"
	OS           string // e.g. "ubuntu"
	Intent       string // e.g. "training"
	Platform     string // optional, e.g. "kubeflow"
	K8sVersion   string // observed major.minor, e.g. "1.30"
	K8sConstraint string // declared constraint, e.g. ">=1.28"
}

// CoordinateFor maps resolved criteria to a Coordinate.
// Matches pkg/recipe.CoordinateFor from NVIDIA/aicr#1409 exactly.
//
// Required: service, accelerator, os, intent (concrete, non-"any").
// Optional: platform (bare intent tab when empty or "any").
func CoordinateFor(c RecipeCriteria) (Coordinate, error) {
	service, err := requireConcrete("service", c.Service)
	if err != nil {
		return Coordinate{}, err
	}
	accelerator, err := requireConcrete("accelerator", c.Accelerator)
	if err != nil {
		return Coordinate{}, err
	}
	os, err := requireConcrete("os", c.OS)
	if err != nil {
		return Coordinate{}, err
	}
	intent, err := requireConcrete("intent", c.Intent)
	if err != nil {
		return Coordinate{}, err
	}

	tab := intent
	if p := c.Platform; p != "" && p != criteriaAnyValue {
		tab = intent + "-" + p
	}

	return Coordinate{
		Group:     service,
		Dashboard: accelerator + "-" + os,
		Tab:       tab,
	}, nil
}

func requireConcrete(dim, value string) (string, error) {
	if value == "" || value == criteriaAnyValue {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			dim+` dimension must be concrete (got "`+value+`")`)
	}
	return value, nil
}

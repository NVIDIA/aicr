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

package recipe

import (
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Coordinate is the canonical placement of a recipe within the
// service/accelerator/intent navigation space. It is the single shared
// mapping imported by GP4/GP5, TG2/TG3/TG4a, and RQ1 so every consumer
// derives the same location from resolved Criteria.
//
// See docs/design/012-recipe-coordinate-mapping.md.
type Coordinate struct {
	// Group is the service dimension (e.g. "eks").
	Group string

	// Dashboard is the accelerator-os pair (e.g. "h100-ubuntu").
	Dashboard string

	// Tab is the intent, optionally suffixed with the platform when one
	// is set (e.g. "training" or "training-kubeflow").
	Tab string
}

// Path returns the coordinate as "<group>/<dashboard>/<tab>". The
// host/navigation scheme built around this path is owned by the
// consumer (GP5/TG4a); this package only emits the canonical segments.
func (co Coordinate) Path() string {
	return co.Group + "/" + co.Dashboard + "/" + co.Tab
}

// String returns the same value as Path so a Coordinate prints as its
// canonical path.
func (co Coordinate) String() string {
	return co.Path()
}

// CoordinateFor maps resolved Criteria to its canonical Coordinate. It
// consumes already-resolved Criteria and never parses metadata.name.
//
// The function is pure: no clock, no maps, no registry, no I/O — the same
// Criteria always yields the same Coordinate.
//
// The service, accelerator, os, and intent dimensions are required and
// must be concrete; a nil Criteria, an empty value, or the "any" wildcard
// fails closed with ErrCodeInvalidRequest naming the offending dimension.
// The platform dimension is optional: an empty or "any" platform yields a
// bare intent tab, otherwise the tab is "<intent>-<platform>".
func CoordinateFor(c *Criteria) (Coordinate, error) {
	if c == nil {
		return Coordinate{}, errors.New(errors.ErrCodeInvalidRequest, "criteria is nil")
	}

	service, err := requireConcrete("service", string(c.Service))
	if err != nil {
		return Coordinate{}, err
	}
	accelerator, err := requireConcrete("accelerator", string(c.Accelerator))
	if err != nil {
		return Coordinate{}, err
	}
	os, err := requireConcrete("os", string(c.OS))
	if err != nil {
		return Coordinate{}, err
	}
	intent, err := requireConcrete("intent", string(c.Intent))
	if err != nil {
		return Coordinate{}, err
	}

	tab := intent
	if platform := string(c.Platform); platform != "" && platform != CriteriaAnyValue {
		if _, err := rejectPathSeparator("platform", platform); err != nil {
			return Coordinate{}, err
		}
		tab = intent + "-" + platform
	}

	return Coordinate{
		Group:     service,
		Dashboard: accelerator + "-" + os,
		Tab:       tab,
	}, nil
}

// requireConcrete returns value when it is a usable coordinate segment, or an
// ErrCodeInvalidRequest naming dim. A value is usable when it is non-empty,
// not the "any" wildcard, and free of the "/" segment separator.
func requireConcrete(dim, value string) (string, error) {
	if value == "" || value == CriteriaAnyValue {
		return "", errors.New(errors.ErrCodeInvalidRequest, dim+" dimension must be concrete (got empty or \"any\")")
	}
	return rejectPathSeparator(dim, value)
}

// rejectPathSeparator returns value unless it contains "/", the coordinate
// segment separator. A "/" in any dimension (e.g. a "--data"-seeded service
// "acme/ncp") would inject a spurious path segment and silently mis-place the
// recipe, so it fails closed with ErrCodeInvalidRequest naming dim.
func rejectPathSeparator(dim, value string) (string, error) {
	if strings.ContainsRune(value, '/') {
		return "", errors.New(errors.ErrCodeInvalidRequest, dim+" dimension must not contain \"/\" (the coordinate path separator)")
	}
	return value, nil
}

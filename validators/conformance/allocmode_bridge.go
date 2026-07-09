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

package main

import (
	"github.com/NVIDIA/aicr/validators/internal/allocmode"
)

// The GPU allocation-mode probe lives in validators/internal/allocmode so the
// performance validator (inference-perf) can share the exact capability
// detection the conformance checks use. These aliases keep the conformance
// package's established local names — the probe was extracted verbatim from
// this package (see allocmode.Detect).
var (
	detectGPUAllocationMode       = allocmode.Detect
	draGVRAt                      = allocmode.GVRAt
	discoverServedDRAAPIVersion   = allocmode.DiscoverServedVersion
	usableDriverSliceNodes        = allocmode.UsableDriverSliceNodes
	eligibleReadySchedulableNodes = allocmode.EligibleReadySchedulableNodes
	sortedNodeNames               = allocmode.SortedNodeNames
	draAPIVersionPreference       = allocmode.APIVersionPreference
	classifyK8sReadError          = allocmode.ClassifyK8sReadError
	verifyGPUAllocationPolicy     = allocmode.Verify
)

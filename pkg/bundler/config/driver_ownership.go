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

package config

import "github.com/NVIDIA/aicr/pkg/allocpolicy"

// DriverOwnershipFamily names the registry components that share one set of
// GPU driver-ownership leaf paths.
type DriverOwnershipFamily struct {
	// ComponentNames are the canonical registry name first, then its
	// OpenShift variant.
	ComponentNames []string
	// Paths are the leaf value paths, relative to each component's values,
	// that decide who owns the GPU driver.
	Paths []string
}

// GPUOperatorDriverOwnership is the GPU Operator family.
var GPUOperatorDriverOwnership = DriverOwnershipFamily{
	ComponentNames: []string{allocpolicy.ComponentGPUOperator, allocpolicy.ComponentGPUOperatorOCP},
	Paths:          []string{"driver.enabled", "toolkit.enabled", "hostPaths.driverInstallDir"},
}

// DRADriverOwnership is the NVIDIA DRA GPU driver family.
var DRADriverOwnership = DriverOwnershipFamily{
	ComponentNames: []string{allocpolicy.ComponentDRADriver, allocpolicy.ComponentDRADriverOCP},
	Paths:          []string{"nvidiaDriverRoot"},
}

// DriverOwnershipFamilies returns every driver-ownership family.
func DriverOwnershipFamilies() []DriverOwnershipFamily {
	return []DriverOwnershipFamily{GPUOperatorDriverOwnership, DRADriverOwnership}
}

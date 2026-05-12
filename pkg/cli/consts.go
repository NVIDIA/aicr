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

package cli

// Cross-file string constants shared across CLI command definitions.
// Command names, flag names, category labels, and well-known values live
// here so the same literal is not declared in multiple files. File-local
// strings stay with their owning file.

// Command names (urfave/cli command.Name values).
const (
	cmdNameSnapshot = "snapshot"
	cmdNameRecipe   = "recipe"
)

// Flag names (urfave/cli flag.Name values).
const (
	flagOutput = "output"
)

// Category labels (urfave/cli flag.Category values, grouping flags in help output).
const (
	catInput             = "Input"
	catOutput            = "Output"
	catDeployment        = "Deployment"
	catScheduling        = "Scheduling"
	catOCIRegistry       = "OCI Registry"
	catQueryParameters   = "Query Parameters"
	catAgentDeployment   = "Agent Deployment"
	catValidationControl = "Validation Control"
	catEvidence          = "Evidence"
)

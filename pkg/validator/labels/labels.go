// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package labels provides shared label constants for validation resources.
package labels

// Standard Kubernetes label keys.
const (
	Name      = "app.kubernetes.io/name"
	Component = "app.kubernetes.io/component"
	ManagedBy = "app.kubernetes.io/managed-by"
)

// AICR-specific label keys.
const (
	JobType    = "aicr.nvidia.com/job-type"
	RunID      = "aicr.nvidia.com/run-id"
	Validator  = "aicr.nvidia.com/validator"
	Phase      = "aicr.nvidia.com/phase"
	ReportType = "aicr.nvidia.com/report-type"
)

// Common label values.
const (
	ValueAICR       = "aicr"
	ValueValidation = "validation"
	ValueValidator  = "aicr-validator"
)

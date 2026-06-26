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

// Package labels provides shared label constants for validation resources.
package labels

import "github.com/NVIDIA/aicr/pkg/header"

// Standard Kubernetes label keys.
const (
	Name      = "app.kubernetes.io/name"
	Component = "app.kubernetes.io/component"
	ManagedBy = "app.kubernetes.io/managed-by"
)

// AICR-specific label keys, keyed on the canonical AICR API domain.
const (
	JobType    = header.Domain + "/job-type"
	RunID      = header.Domain + "/run-id"
	Validator  = header.Domain + "/validator"
	Phase      = header.Domain + "/phase"
	ReportType = header.Domain + "/report-type"
)

// Common label values.
const (
	ValueAICR       = "aicr"
	ValueValidation = "validation"
	ValueValidator  = "aicr-validator"
)

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

import (
	"github.com/NVIDIA/aicr/pkg/header"
	k8slabels "github.com/NVIDIA/aicr/pkg/k8s/labels"
)

// Standard Kubernetes label keys. These are aliases of pkg/k8s/labels, which
// is the single source of truth shared with the snapshot agent
// (pkg/k8s/agent) — validator code should keep referring to them as
// labels.Name etc. rather than importing pkg/k8s/labels directly.
const (
	Name      = k8slabels.Name
	Component = k8slabels.Component
	ManagedBy = k8slabels.ManagedBy
	RunID     = k8slabels.RunID
)

// AICR-specific label keys, keyed on the canonical AICR API domain.
const (
	JobType    = header.Domain + "/job-type"
	Validator  = header.Domain + "/validator"
	Phase      = header.Domain + "/phase"
	ReportType = header.Domain + "/report-type"
)

// Common label values.
const (
	ValueAICR       = k8slabels.ValueAICR
	ValueValidation = "validation"
	ValueValidator  = "aicr-validator"
)

// Component label values for per-run benchmark namespaces, stamped
// alongside ManagedBy so a stale-namespace prune can scope its List to
// exactly one benchmark server-side, instead of matching a hand-maintained
// name prefix.
const (
	ValueNCCLPerf      = "nccl-perf"
	ValueInferencePerf = "inference-perf"
)

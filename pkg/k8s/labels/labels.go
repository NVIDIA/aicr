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

// Package labels provides shared Kubernetes label constants used by both
// the validator (pkg/validator) and the snapshot agent (pkg/k8s/agent), so
// neither has to import the other to agree on label keys and values.
package labels

import "github.com/NVIDIA/aicr/pkg/header"

// Standard Kubernetes label keys.
const (
	Name      = "app.kubernetes.io/name"
	Component = "app.kubernetes.io/component"
	ManagedBy = "app.kubernetes.io/managed-by"
)

// RunID scopes every resource to the run that created it.
const RunID = header.Domain + "/run-id"

// Common label values.
const (
	// ValueAICR is the shared app name.
	ValueAICR = "aicr"

	// ValueSnapshotAgent identifies snapshot-agent-owned resources.
	ValueSnapshotAgent = "snapshot-agent"
)

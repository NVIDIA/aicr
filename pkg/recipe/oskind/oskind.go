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

// Package oskind is the single source of truth for the string values of
// the OS recipe criterion. It is intentionally a leaf package with no
// internal dependencies so it can be imported from anywhere in the tree
// (pkg/recipe for the typed CriteriaOSType constants, pkg/collector for
// factory backend selection, pkg/k8s/agent for pod-manifest gating,
// pkg/snapshotter for env-var validation, and the CLI) without
// reintroducing the pkg/recipe -> pkg/snapshotter -> pkg/collector
// import cycle that previously forced these constants to be duplicated
// in three places with manual "must stay in sync" comments.
package oskind

import "strings"

// Supported OS criterion values. Keep this list aligned with the
// `os` enum in api/aicr/v1/server.yaml and the user-facing OS lists
// in docs/.
const (
	Any         = "any"
	Ubuntu      = "ubuntu"
	RHEL        = "rhel"
	COS         = "cos"
	AmazonLinux = "amazonlinux"
	Talos       = "talos"
)

// All returns every supported OS value (excluding `Any`) sorted
// alphabetically. Used by the recipe package's GetCriteriaOSTypes and
// any caller that needs to enumerate concrete OS choices.
func All() []string {
	return []string{AmazonLinux, COS, RHEL, Talos, Ubuntu}
}

// IsKnown reports whether s is one of the supported OS values, including
// `Any`. Aliases (e.g., al2/al2023 -> amazonlinux) are NOT recognized
// here; callers that need alias normalization should use
// recipe.ParseCriteriaOSType.
func IsKnown(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case Any, Ubuntu, RHEL, COS, AmazonLinux, Talos:
		return true
	default:
		return false
	}
}

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

package resolver

import (
	"fmt"
	"slices"
	"strings"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// ReasonNamespaceNotAllowed marks a plan that renders a release into a
// namespace outside the installation's component namespaces.
const ReasonNamespaceNotAllowed = "NamespaceNotAllowed"

// CheckNamespaces refuses a plan whose releases target namespaces outside
// allowed, before anything reaches the engine. The engine's admission policy
// and project would refuse the write anyway; refusing the plan stalls at
// resolution and names every release instead of failing a rollout step.
func CheckNamespaces(p *plan.Document, allowed []string) error {
	var outside []string
	for i := range p.Releases {
		if r := &p.Releases[i]; !slices.Contains(allowed, r.TargetNamespace) {
			outside = append(outside, fmt.Sprintf("%s in %s", r.ID, r.TargetNamespace))
		}
	}
	if len(outside) == 0 {
		return nil
	}
	return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(
		"releases %s target namespaces outside the installation's component namespaces (namespaces.components)",
		strings.Join(outside, ", "))).WithReason(ReasonNamespaceNotAllowed)
}

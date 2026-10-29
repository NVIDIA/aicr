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
	"strings"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

func TestCheckNamespaces(t *testing.T) {
	t.Parallel()
	p := &plan.Document{Releases: []plan.Release{
		{ID: "cert-manager", TargetNamespace: "cert-manager"},
		{ID: "gpu-operator", TargetNamespace: "gpu-operator"},
		{ID: "nfd", TargetNamespace: "node-feature-discovery"},
	}}
	if err := CheckNamespaces(p, []string{"cert-manager", "gpu-operator", "node-feature-discovery"}); err != nil {
		t.Fatalf("allowed plan: %v", err)
	}
	err := CheckNamespaces(p, []string{"cert-manager"})
	if operrors.ReasonOf(err) != ReasonNamespaceNotAllowed || !strings.Contains(err.Error(), "gpu-operator in gpu-operator, nfd in node-feature-discovery") {
		t.Fatalf("CheckNamespaces() = %v", err)
	}
	if err = CheckNamespaces(p, nil); operrors.ReasonOf(err) != ReasonNamespaceNotAllowed {
		t.Fatalf("no allowed namespace: %v", err)
	}
}

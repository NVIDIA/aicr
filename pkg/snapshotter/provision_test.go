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

package snapshotter

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestProvisionAgentRoles_RejectsBeforeClusterAccess covers the fail-before-
// connect contract: every input ProvisionAgentRoles can reject without a
// cluster must be rejected before the Kubernetes client is built, so a bad
// value is reported as itself rather than masked by a kubeconfig error on a
// machine with no cluster configured.
//
// The cluster-side behavior (existence check, naming, idempotent
// create-or-update) is covered against a fake clientset in pkg/k8s/agent.
func TestProvisionAgentRoles_RejectsBeforeClusterAccess(t *testing.T) {
	tests := []struct {
		name   string
		config *AgentRolesConfig
	}{
		{name: "nil config", config: nil},
		{name: "empty namespace", config: &AgentRolesConfig{ServiceAccountName: "irsa-snapshotter"}},
		{name: "whitespace namespace", config: &AgentRolesConfig{Namespace: "  ", ServiceAccountName: "irsa-snapshotter"}},
		{name: "empty ServiceAccount name", config: &AgentRolesConfig{Namespace: "gpu-operator"}},
		{name: "whitespace ServiceAccount name", config: &AgentRolesConfig{Namespace: "gpu-operator", ServiceAccountName: " "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A kubeconfig path that cannot resolve: if the rejection ever
			// moved after client construction, this test would start
			// failing on the wrong error instead of passing silently.
			if tt.config != nil {
				tt.config.Kubeconfig = "/nonexistent/kubeconfig-that-must-not-be-read"
			}

			res, err := ProvisionAgentRoles(context.Background(), tt.config)
			if err == nil {
				t.Fatal("ProvisionAgentRoles() error = nil, want ErrCodeInvalidRequest")
			}
			if res != nil {
				t.Errorf("result = %+v, want nil", res)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

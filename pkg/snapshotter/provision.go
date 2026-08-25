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
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
)

// AgentRolesConfig selects the ServiceAccount that ProvisionAgentRoles
// grants the snapshot agent's permissions to, and the cluster it lives in.
type AgentRolesConfig struct {
	// Kubeconfig is an optional path override; empty uses default
	// discovery (KUBECONFIG, then ~/.kube/config, then in-cluster).
	Kubeconfig string

	// Namespace holds the ServiceAccount and receives the Role and
	// RoleBinding. Required.
	Namespace string

	// ServiceAccountName is the EXACT name of an already-existing
	// ServiceAccount. Required.
	ServiceAccountName string

	// DiscoverNetwork also grants the cluster-scoped MUTATING rules that
	// `aicr snapshot --discover-network` needs. Permanently, not for one
	// run's lifetime.
	DiscoverNetwork bool
}

// AgentRolesResult names what ProvisionAgentRoles created or updated, so a
// caller can report it without rebuilding the names.
//
// It is snapshotter-owned rather than pkg/k8s/agent's own ProvisionResult
// so callers presenting the outcome — the CLI among them — need no
// dependency on the Kubernetes-facing package.
type AgentRolesResult struct {
	Namespace          string
	ServiceAccountName string
	Role               string
	RoleBinding        string
	ClusterRole        string
	ClusterRoleBinding string

	// DiscoverNetwork echoes AgentRolesConfig.DiscoverNetwork: it is the
	// difference between a read-only grant and one carrying cluster-scoped
	// mutating rules, so anything reporting this result can say which was
	// provisioned.
	DiscoverNetwork bool
}

// ProvisionAgentRoles grants the snapshot agent's permissions to an
// existing, operator-supplied ServiceAccount so that ServiceAccount can be
// named exactly via AgentConfig.ServiceAccountName
// (`--service-account-name`) and keep its own identity — the IRSA or GKE
// Workload Identity annotations a run-scoped ServiceAccount cannot carry,
// because both providers pin trust to the ServiceAccount name.
//
// It provisions and returns; it deploys no Job and collects no snapshot.
// The objects it creates are PERMANENT: they carry no run-ID label, never
// enter a run's created-set, and no run's cleanup deletes them. Removing
// them is the operator's job.
//
// Idempotent — re-run it after an aicr upgrade to refresh the rules in
// place. Returns ErrCodeNotFound when the named ServiceAccount does not
// exist.
func ProvisionAgentRoles(ctx context.Context, config *AgentRolesConfig) (*AgentRolesResult, error) {
	if config == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "agent roles config is required")
	}
	// Reject what can be rejected without contacting the cluster, so a bad
	// value is never masked by a kubeconfig error.
	if strings.TrimSpace(config.Namespace) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"Namespace is required: it is where the ServiceAccount, Role and RoleBinding live")
	}
	if strings.TrimSpace(config.ServiceAccountName) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"ServiceAccountName is required: provisioning grants permissions to an existing ServiceAccount, it does not create one")
	}

	clientset, err := getKubeClient(config.Kubeconfig)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.AgentRBACProvisionTimeout)
	defer cancel()

	res, err := agent.ProvisionServiceAccountRoles(ctx, clientset, agent.ProvisionOptions{
		Namespace:          config.Namespace,
		ServiceAccountName: config.ServiceAccountName,
		DiscoverNetwork:    config.DiscoverNetwork,
	})
	if err != nil {
		return nil, err
	}
	return &AgentRolesResult{
		Namespace:          res.Namespace,
		ServiceAccountName: res.ServiceAccountName,
		Role:               res.Role,
		RoleBinding:        res.RoleBinding,
		ClusterRole:        res.ClusterRole,
		ClusterRoleBinding: res.ClusterRoleBinding,
		DiscoverNetwork:    res.DiscoverNetwork,
	}, nil
}

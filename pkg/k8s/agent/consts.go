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

package agent

// Kubernetes RBAC verbs and resource names used when building agent RBAC.
const (
	verbCreate = "create"
	verbList   = "list"
	verbGet    = "get"
	verbDelete = "delete"
	verbWatch  = "watch"
	verbUpdate = "update"
	verbPatch  = "patch"

	// Resource names, as they appear in an RBAC PolicyRule and in the
	// ResourceAttributes of an access review. Named so the RBAC this
	// package builds and the pre-flight gate that checks for it can never
	// spell the same resource two different ways.
	resourceCM                  = "configmaps"
	resourceServiceAccounts     = "serviceaccounts"
	resourceRoles               = "roles"
	resourceRoleBindings        = "rolebindings"
	resourceClusterRoles        = "clusterroles"
	resourceClusterRoleBindings = "clusterrolebindings"
	resourceJobs                = "jobs"
	resourcePods                = "pods"
	resourceNodes               = "nodes"

	// subresourceLog is the pods subresource the CLI reads to stream the
	// agent's output back to the operator's terminal.
	subresourceLog = "log"

	// batchAPIGroup is the API group the agent Job lives in.
	batchAPIGroup = "batch"

	slinkyAPIGroup           = "slinky.slurm.net"
	slinkyControllerResource = "controllers"
	slinkyNodeSetResource    = "nodesets"
	slinkyLoginSetResource   = "loginsets"
	slinkyRestAPIResource    = "restapis"
	slinkyAccountingResource = "accountings"

	mariaDBAPIGroup = "k8s.mariadb.com"
	mariaDBResource = "mariadbs"

	// rbacAPIGroup is the API group RoleRef / ClusterRoleRef values bind
	// against, and that PolicyRules use when permitting Role / RoleBinding
	// / ClusterRole / ClusterRoleBinding resources.
	rbacAPIGroup = "rbac.authorization.k8s.io"
)

// Attribute keys shared by this package's structured log lines and error
// contexts. Named for the same reason the ctxKey* constants in names.go are:
// one spelling reaches every consumer that parses them, and a literal
// repeated across files does not drift.
const (
	attrNamespace      = "namespace"
	attrName           = "name"
	attrRunID          = "runID"
	attrServiceAccount = "serviceAccount"
)

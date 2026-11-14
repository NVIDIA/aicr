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

package sweep

import (
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/validator/job"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

// Kinds the sweeper selects. The values are the metav1 Kind strings so a
// report reads like kubectl output.
const (
	kindNamespace          = "Namespace"
	kindPod                = "Pod"
	kindPodGroup           = "PodGroup"
	kindClusterRoleBinding = "ClusterRoleBinding"
	kindClusterRole        = "ClusterRole"
	kindServiceAccount     = "ServiceAccount"
	kindRole               = "Role"
	kindRoleBinding        = "RoleBinding"
	kindConfigMap          = "ConfigMap"
)

// podGroupGVR is the Run:ai (KAI) PodGroup resource the gang-scheduling check
// creates in a Project namespace. Absent on clusters without Run:ai, which
// Plan tolerates.
var podGroupGVR = schema.GroupVersionResource{
	Group: "scheduling.run.ai", Version: "v2alpha2", Resource: "podgroups",
}

// Server-side label selectors, one per family. A bare key is an "exists"
// requirement. Selection is deliberately narrower than "stamped by aicr":
// only the run-scoped shapes the sweeper can prove ownership of.
const (
	// testNamespaceSelector selects every per-run conformance test
	// namespace, including any a user stamped by hand — which is why the
	// name-and-token check in acceptTestNamespace decides candidacy.
	testNamespaceSelector = labels.GPUTestRun

	// runAIResidueSelector selects the gang-scheduling pods and PodGroups a
	// run leaves in a Run:ai Project namespace. Both labels are stamped
	// together by the check; requiring both keeps a stray hand-stamped object
	// out of scope.
	runAIResidueSelector = labels.GPUTestRun + "," + labels.GangSchedulingPhase

	// runJobSelector selects the validator (and snapshot-agent) Jobs whose
	// existence proves a run is still alive. Any Job carrying the run ID
	// counts, finished ones included — but a finished Job persists only for
	// a --no-cleanup run (until the TTL controller reaps it after
	// defaults.JobTTLAfterFinished): with cleanup on, the validator deletes
	// each Job right after its check, so Job existence is a point-in-time
	// signal and Execute adds the settle observation for the gap between a
	// run's checks.
	runJobSelector = labels.RunID

	// validatorRBACSelector selects the per-run validator ServiceAccount and
	// ClusterRoleBinding. They carry no run-id label; the run ID comes from
	// the name (job.ServiceAccountName / job.ClusterRoleBindingName).
	validatorRBACSelector = labels.Name + "=" + labels.ValueValidator +
		"," + labels.ManagedBy + "=" + labels.ValueAICR

	// dataConfigMapSelector selects the per-run data and CTRF ConfigMaps.
	// The run ID comes from the labels.RunID label, required below.
	dataConfigMapSelector = labels.Name + "=" + labels.ValueAICR +
		"," + labels.Component + "=" + labels.ValueValidation +
		"," + labels.ManagedBy + "=" + labels.ValueAICR

	// agentObjectSelector selects the snapshot agent's run-scoped objects.
	// The operator-owned agent RBAC rendered by `aicr snapshot
	// --add-roles-to-service-account` carries component=agent-rbac
	// (labels.ValueAgentRBAC), so this selector never matches it — the
	// exclusion the tools/cleanup predicates make explicit is inherent here.
	agentObjectSelector = labels.Name + "=" + labels.ValueAICR +
		"," + labels.Component + "=" + labels.ValueSnapshotAgent +
		"," + labels.ManagedBy + "=" + labels.ValueAICR
)

// token32 and token8 are the hex shapes the checks' random namespace tokens
// take: 16 bytes for the GPU and gang-scheduling runs, 4 for
// cluster-autoscaling.
var (
	token32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	token8  = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

// acceptTestNamespace reports whether a namespace named name carrying the
// labels.GPUTestRun value token is one the sweeper may delete: the name must
// be a known test-namespace prefix followed by exactly that token, in the hex
// shape the producing check generates. A bare label proves nothing — a user
// can stamp it on anything — so the name must reconstruct from it.
func acceptTestNamespace(name, token string) bool {
	for _, p := range []struct {
		prefix string
		shape  *regexp.Regexp
	}{
		{defaults.GPUTestNamespacePrefix, token32},
		{defaults.GangSchedulingTestNamespacePrefix, token32},
		{defaults.ClusterAutoscalingTestNamespacePrefix, token8},
	} {
		if strings.HasPrefix(name, p.prefix) {
			return name == p.prefix+token && p.shape.MatchString(token)
		}
	}
	return false
}

// validatorRunName extracts the run ID from a validator ServiceAccount or
// ClusterRoleBinding name. Both derive from the same prefix in
// pkg/validator/job, so one check serves either kind; ok is false for any
// name that merely carries the prefix with an empty suffix.
func validatorRunName(name string) (runID string, ok bool) {
	runID, found := strings.CutPrefix(name, job.ServiceAccountName(""))
	if !found || runID == "" {
		return "", false
	}
	return runID, true
}

// deletionRank orders deletion: the ClusterRoleBinding first, mirroring
// job.CleanupRBAC — revoking the binding closes the cluster-admin escalation
// window immediately, whereas an orphaned ServiceAccount carries no
// privileges. Namespaces go before the Run:ai residue so a swept test
// namespace takes its contents with it.
func deletionRank(kind string) int {
	switch kind {
	case kindClusterRoleBinding:
		return 0
	case kindClusterRole:
		return 1
	case kindServiceAccount:
		return 2
	case kindRole, kindRoleBinding:
		return 3
	case kindConfigMap:
		return 4
	case kindNamespace:
		return 5
	case kindPod, kindPodGroup:
		return 6
	default:
		return 7
	}
}

// reasonForeignNamespace is why a namespace carrying the ownership label is
// kept when its name does not reconstruct from the label value.
const reasonForeignNamespace = "labeled but not a validator test namespace"

// reasonTerminating is why an object whose deletion already started is kept.
const reasonTerminating = "deletion already in progress"

// terminatingReason summarizes a namespace stuck in deletion: how long it has
// been terminating and what is still holding it. The sweeper never strips
// finalizers — that is a human call, per namespace.
func terminatingReason(ts *metav1.Time, finalizers, conditions []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "deletion in progress since %s", ts.UTC().Format("2006-01-02T15:04:05Z"))
	if len(finalizers) > 0 {
		fmt.Fprintf(&b, "; finalizers: %s", strings.Join(finalizers, ", "))
	}
	if len(conditions) > 0 {
		fmt.Fprintf(&b, "; conditions: %s", strings.Join(conditions, ", "))
	}
	return b.String()
}

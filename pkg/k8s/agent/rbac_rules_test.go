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

import (
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestClusterRulesAreReadOnlyEvenWithDiscoverNetwork is the regression guard
// for aicr#3240: the snapshot agent's cluster-scoped grant must never carry a
// write verb or a subresource (pods/exec), whether or not --discover-network
// is set. A change that re-introduces either fails here before it reaches a
// cluster.
func TestClusterRulesAreReadOnlyEvenWithDiscoverNetwork(t *testing.T) {
	forbidden := map[string]bool{
		"create": true, "update": true, "patch": true, "delete": true,
		"deletecollection": true, "escalate": true, "bind": true, "impersonate": true, "*": true,
	}
	for _, discover := range []bool{false, true} {
		for _, r := range clusterRules(discover) {
			for _, res := range r.Resources {
				if strings.Contains(res, "/") {
					t.Errorf("discover=%v: subresource %q granted; the cluster grant must stay read-only", discover, res)
				}
			}
			for _, v := range r.Verbs {
				if forbidden[v] {
					t.Errorf("discover=%v: %v/%v grants %q; the cluster grant must stay read-only",
						discover, r.APIGroups, r.Resources, v)
				}
			}
		}
	}
}

// TestClusterRulesDiscoverNetworkDeltaIsExactlyNicDevices pins that opting
// into discovery changes the grant by precisely one read rule, appended after
// the baseline, so the baseline cannot drift under the flag.
func TestClusterRulesDiscoverNetworkDeltaIsExactlyNicDevices(t *testing.T) {
	base, disc := clusterRules(false), clusterRules(true)
	if len(disc) != len(base)+1 {
		t.Fatalf("discover rules = %d, want baseline %d + 1", len(disc), len(base))
	}
	if !reflect.DeepEqual(disc[:len(base)], base) {
		t.Fatal("discover grant does not start with the unchanged baseline rules")
	}
	want := rbacv1.PolicyRule{
		APIGroups: []string{"configuration.net.nvidia.com"},
		Resources: []string{"nicdevices"},
		Verbs:     []string{verbGet, verbList},
	}
	if got := disc[len(base)]; !reflect.DeepEqual(got, want) {
		t.Errorf("discover delta = %+v, want %+v", got, want)
	}
}

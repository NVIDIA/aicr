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

package aicr_test

import (
	"reflect"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	appconfig "github.com/NVIDIA/aicr/pkg/config"
)

// assertProjected fails when a field of the resolved type is neither carried by
// one of the facade types, nor renamed into one, nor explicitly declined.
//
// It fails CLOSED: adding a spec field without deciding its fate breaks the
// build rather than silently dropping a user's configuration.
//
// It checks NAME PRESENCE, not type equality. Several projections are
// deliberate transforms (Requests string -> corev1.ResourceList, NoCleanup ->
// inverted Cleanup, Privileged *bool -> defaulted bool). Enforcing type
// equality would flag all of them. Value correctness is the per-derivation
// tests' job.
func assertProjected(
	t *testing.T,
	resolved reflect.Type,
	facades []reflect.Type,
	renamed map[string]string,
	declined map[string]string,
) {

	t.Helper()

	has := func(name string) bool {
		for _, f := range facades {
			if _, ok := f.FieldByName(name); ok {
				return true
			}
		}
		return false
	}

	for i := 0; i < resolved.NumField(); i++ {
		name := resolved.Field(i).Name
		if reason, ok := declined[name]; ok {
			if reason == "" {
				t.Errorf("%s.%s is declined with an empty reason; state why",
					resolved.Name(), name)
			}
			continue
		}
		target := name
		if r, ok := renamed[name]; ok {
			target = r
		}
		if !has(target) {
			t.Errorf("%s.%s is not projected by any of %v, not renamed, and not "+
				"declined — decide its fate or the setting is silently dropped",
				resolved.Name(), name, facades)
		}
	}
}

// SnapshotResolved projects across AgentConfig and SnapshotOutputOptions: the
// former describes the collection Job, the latter delivery (#2542).
func TestSnapshotResolved_IsFullyProjected(t *testing.T) {
	t.Parallel()

	assertProjected(t,
		reflect.TypeOf(appconfig.SnapshotResolved{}),
		[]reflect.Type{
			reflect.TypeOf(aicr.AgentConfig{}),
			reflect.TypeOf(aicr.SnapshotOutputOptions{}),
		},
		map[string]string{
			"NoCleanup":      "Cleanup", // inverted; see SnapshotAgentConfig godoc
			"OutputPath":     "Path",
			"OutputFormat":   "Format",
			"OutputTemplate": "Template",
		},
		map[string]string{},
	)
}

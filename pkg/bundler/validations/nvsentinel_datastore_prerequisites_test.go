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

package validations

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckNVSentinelDatastorePrerequisites(t *testing.T) {
	t.Parallel()

	const component = "nvsentinel-mongodb"
	result := func(storage, percona string, overrides map[string]any) *recipe.RecipeResult {
		r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: component, Overrides: overrides}}}
		r.Metadata.DefaultStorageClassState = storage
		r.Metadata.PerconaOperatorState = percona
		return r
	}
	namedClass := map[string]any{"replsets": map[string]any{"rs0": map[string]any{
		"volumeSpec": map[string]any{"pvc": map[string]any{"storageClassName": "fast"}},
	}}}
	emptyClass := map[string]any{"replsets": map[string]any{"rs0": map[string]any{
		"volumeSpec": map[string]any{"pvc": map[string]any{"storageClassName": ""}},
	}}}
	setClass := func(class string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
			"nvsentinelmongodb": {nvsentinelMongoDBStorageClassPath: class},
		}))
	}
	present, absentPercona := recipe.DefaultStorageClassStatePresent, recipe.PerconaOperatorStateAbsent

	tests := []struct {
		name         string
		rr           *recipe.RecipeResult
		cfg          *config.Config
		wantWarnings []string
		wantErrs     []string
		wantCode     aicrerrors.ErrorCode
	}{
		{name: "nil recipe"},
		{name: "component absent", rr: &recipe.RecipeResult{}},
		{
			name: "component disabled",
			rr:   result(recipe.DefaultStorageClassStateAbsent, recipe.PerconaOperatorStateCRsDetected, map[string]any{"enabled": false}),
		},
		{name: "all clear", rr: result(present, absentPercona, nil)},
		{name: "AICR's own Percona cluster is silent", rr: result(present, recipe.PerconaOperatorStateAICROwned, nil)},
		{
			name:         "no evidence warns twice",
			rr:           result("", "", nil),
			wantWarnings: []string{"no metadata.defaultStorageClassState", "no metadata.perconaOperatorState"},
		},
		{
			name:         "multiple defaults warn",
			rr:           result(recipe.DefaultStorageClassStateMultiple, absentPercona, nil),
			wantWarnings: []string{"more than one default StorageClass"},
		},
		{
			name:     "no default StorageClass blocks",
			rr:       result(recipe.DefaultStorageClassStateAbsent, absentPercona, nil),
			wantErrs: []string{"found no default StorageClass"},
			wantCode: aicrerrors.ErrCodeConflict,
		},
		{
			name:     "inconclusive StorageClass evidence blocks",
			rr:       result(recipe.DefaultStorageClassStateUnknown, absentPercona, nil),
			wantErrs: []string{"permission to list storageclasses"},
			wantCode: aicrerrors.ErrCodeConflict,
		},
		{name: "recipe-named StorageClass waives absent", rr: result(recipe.DefaultStorageClassStateAbsent, absentPercona, namedClass)},
		{name: "--set StorageClass waives unknown", rr: result(recipe.DefaultStorageClassStateUnknown, absentPercona, nil), cfg: setClass("fast")},
		{
			name: "--storage-class waives absent",
			rr:   result(recipe.DefaultStorageClassStateAbsent, absentPercona, nil),
			cfg:  config.NewConfig(config.WithStorageClass("fast")),
		},
		{
			name: "explicit empty --set beats --storage-class",
			rr:   result(recipe.DefaultStorageClassStateAbsent, absentPercona, nil),
			cfg: config.NewConfig(config.WithStorageClass("fast"), config.WithValueOverrides(map[string]map[string]string{
				"nvsentinelmongodb": {nvsentinelMongoDBStorageClassPath: ""},
			})),
			wantErrs: []string{"found no default StorageClass"},
		},
		{
			name:     "empty recipe StorageClass does not waive",
			rr:       result(recipe.DefaultStorageClassStateAbsent, absentPercona, emptyClass),
			wantErrs: []string{"found no default StorageClass"},
		},
		{
			name:     "unrecognized StorageClass state",
			rr:       result("bogus", absentPercona, nil),
			wantErrs: []string{`defaultStorageClassState="bogus"`},
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:         "existing Percona API warns",
			rr:           result(present, recipe.PerconaOperatorStateAPIDetected, nil),
			wantWarnings: []string{"psmdb-operator owns those CRDs"},
		},
		{
			name:     "foreign PerconaServerMongoDB blocks",
			rr:       result(present, recipe.PerconaOperatorStateCRsDetected, nil),
			wantErrs: []string{"other than AICR's own"},
			wantCode: aicrerrors.ErrCodeConflict,
		},
		{
			name:     "foreign Percona operator blocks",
			rr:       result(present, recipe.PerconaOperatorStateOperatorDetected, nil),
			wantErrs: []string{"that AICR did not install"},
			wantCode: aicrerrors.ErrCodeConflict,
		},
		{
			name:     "inconclusive Percona evidence blocks",
			rr:       result(present, recipe.PerconaOperatorStateUnknown, nil),
			wantErrs: []string{"conflict evidence is inconclusive"},
			wantCode: aicrerrors.ErrCodeConflict,
		},
		{
			name:     "unrecognized Percona state",
			rr:       result(present, "bogus", nil),
			wantErrs: []string{`perconaOperatorState="bogus"`},
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:         "findings from both evidence sources accumulate",
			rr:           result(recipe.DefaultStorageClassStateMultiple, recipe.PerconaOperatorStateCRsDetected, nil),
			wantWarnings: []string{"more than one default StorageClass"},
			wantErrs:     []string{"other than AICR's own"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelDatastorePrerequisites(t.Context(), component, tt.rr, cfg, nil)
			assertDatastoreFindings(t, "warning", warnings, tt.wantWarnings)
			errStrings := make([]string, 0, len(errs))
			for _, err := range errs {
				errStrings = append(errStrings, err.Error())
				if tt.wantCode != "" && !stderrors.Is(err, aicrerrors.New(tt.wantCode, "")) {
					t.Errorf("error %q does not carry code %s", err, tt.wantCode)
				}
			}
			assertDatastoreFindings(t, "error", errStrings, tt.wantErrs)
		})
	}
}

func assertDatastoreFindings(t *testing.T, kind string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d %ss %q, want %d matching %q", len(got), kind, got, len(want), want)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("%s %d = %q, want it to contain %q", kind, i, got[i], w)
		}
	}
}

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

package uatbroker

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const validRegistryYAML = `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-0cbe491320188dfa6
    accelerator: h100
    gpu-count: 8
    cluster-config-path: tests/uat/aws/cluster-config.yaml
    test-config-dir: tests/uat/aws/tests
  - name: gcp-h100
    cloud: gcp
    reservation-id: projects/p/reservations/r
    accelerator: h100
    gpu-count: 8
    cluster-config-path: tests/uat/gcp/cluster-config.yaml
    test-config-dir: tests/uat/gcp/tests
`

func TestParseRegistry(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		// code is the expected pkg/errors code when wantErr is true.
		code errors.ErrorCode
	}{
		{name: "valid", yaml: validRegistryYAML},
		{
			name:    "empty document",
			yaml:    "reservations: []",
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// Zero-byte input takes the decoder's io.EOF path and must
			// surface the canonical "no reservations" validation error.
			name:    "zero-byte input",
			yaml:    "",
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name:    "malformed yaml",
			yaml:    "reservations: [oops",
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// KnownFields: a mistyped row key must fail the parse, not
			// silently leave the real field on its default (fail open).
			name: "mistyped row key fails closed",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    nightly-intnts: []
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "unknown top-level key fails closed",
			yaml: `
reservatons:
  - name: aws-h100
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "empty name",
			yaml: `
reservations:
  - name: ""
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "unknown cloud",
			yaml: `
reservations:
  - name: foo-h100
    cloud: foo
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// Azure is a recognized cloud (PR: Azure UAT support).
			name: "azure cloud ok",
			yaml: `
reservations:
  - name: azure-h100
    cloud: azure
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`,
			wantErr: false,
		},
		{
			// reservation-id is optional: quota-backed capacity (e.g. Azure
			// subscription quota) has no capacity-reservation identifier.
			name: "missing reservation-id ok",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`,
			wantErr: false,
		},
		{
			name: "missing cluster-config-path",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    test-config-dir: t
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "non-positive gpu-count",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 0
    cluster-config-path: c.yaml
    test-config-dir: t
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "duplicate name",
			yaml: `
reservations:
  - name: dup
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
  - name: dup
    cloud: gcp
    reservation-id: cr-y
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c2.yaml
    test-config-dir: t2
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// Empty/absent daytime-intent is valid — the reservation is simply
			// not in the daytime rotation.
			name: "empty daytime-intent ok",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`,
		},
		{
			name: "valid daytime-intent",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    daytime-intent: inference
`,
		},
		{
			name: "unknown daytime-intent",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    daytime-intent: serving
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// Empty/absent nightly-intents is valid — it defaults to [training].
			name: "empty nightly-intents ok",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
`,
		},
		{
			name: "valid nightly-intents both",
			yaml: `
reservations:
  - name: gcp-h100
    cloud: gcp
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    nightly-intents: [training, inference]
`,
		},
		{
			name: "unknown nightly-intent in list",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    nightly-intents: [training, serving]
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// A duplicate would double-run the same cell — reject it.
			name: "duplicate nightly-intent in list",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    nightly-intents: [training, training]
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// Two daytime reservations on one cloud would contend for the same
			// reservation (a cloud cannot hold both a held daytime cluster and the
			// nightly batch at once), so Validate must reject it.
			name: "two daytime reservations same cloud",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    daytime-intent: training
  - name: aws-b200
    cloud: aws
    reservation-id: cr-y
    accelerator: b200
    gpu-count: 8
    cluster-config-path: c2.yaml
    test-config-dir: t2
    daytime-intent: inference
`,
			wantErr: true,
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// Two daytime reservations across DIFFERENT clouds is fine — that is
			// exactly the launch topology (AWS training + GCP inference).
			name: "daytime reservations across clouds ok",
			yaml: `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    daytime-intent: training
  - name: gcp-h100
    cloud: gcp
    reservation-id: projects/p/reservations/r
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c2.yaml
    test-config-dir: t2
    daytime-intent: inference
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := ParseRegistry([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRegistry err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(tt.code, "")) {
					t.Errorf("error code = %v, want %v", err, tt.code)
				}
				return
			}
			if reg == nil {
				t.Fatal("ParseRegistry returned nil registry without error")
			}
		})
	}
}

func TestRegistryLookup(t *testing.T) {
	reg, err := ParseRegistry([]byte(validRegistryYAML))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}

	res, err := reg.Lookup("gcp-h100")
	if err != nil {
		t.Fatalf("Lookup(gcp-h100): %v", err)
	}
	if res.Cloud != CloudGCP || res.ReservationID != "projects/p/reservations/r" {
		t.Errorf("Lookup(gcp-h100) = %+v, want cloud=gcp id=projects/p/reservations/r", res)
	}

	if _, missErr := reg.Lookup("does-not-exist"); !stderrors.Is(missErr, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("Lookup(missing) error = %v, want ErrCodeNotFound", missErr)
	}

	// Mutating the returned reservation must not leak into the registry.
	res.Name = "mutated"
	again, lookupErr := reg.Lookup("gcp-h100")
	if lookupErr != nil {
		t.Fatalf("re-Lookup(gcp-h100): %v", lookupErr)
	}
	if again.Name != "gcp-h100" {
		t.Errorf("Lookup returned an aliased internal pointer; mutation leaked: %q", again.Name)
	}
}

func TestRegistryNamesOrder(t *testing.T) {
	reg, err := ParseRegistry([]byte(validRegistryYAML))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	got := reg.Names()
	want := []string{"aws-h100", "gcp-h100"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadRegistryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reservations.yaml")
	if err := os.WriteFile(path, []byte(validRegistryYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistryFile(path)
	if err != nil {
		t.Fatalf("LoadRegistryFile: %v", err)
	}
	if len(reg.Reservations) != 2 {
		t.Errorf("loaded %d reservations, want 2", len(reg.Reservations))
	}

	if _, err := LoadRegistryFile(filepath.Join(dir, "nope.yaml")); !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("LoadRegistryFile(missing) error = %v, want ErrCodeInvalidRequest", err)
	}
}

func TestDaytimeAssignments(t *testing.T) {
	// Only rows with a non-empty daytime-intent appear, in document order.
	const yaml = `
reservations:
  - name: aws-h100
    cloud: aws
    reservation-id: cr-x
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    daytime-intent: training
  - name: gcp-h100
    cloud: gcp
    reservation-id: projects/p/reservations/r
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c2.yaml
    test-config-dir: t2
    daytime-intent: inference
  - name: aws-b200
    cloud: aws
    reservation-id: cr-y
    accelerator: b200
    gpu-count: 8
    cluster-config-path: c3.yaml
    test-config-dir: t3
`
	reg, err := ParseRegistry([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	got := reg.DaytimeAssignments()
	want := []DaytimeAssignment{
		{Reservation: "aws-h100", Intent: IntentTraining},
		{Reservation: "gcp-h100", Intent: IntentInference},
	}
	if len(got) != len(want) {
		t.Fatalf("DaytimeAssignments() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DaytimeAssignments()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDaytimeAssignmentsNone(t *testing.T) {
	// A registry with no daytime-intent rows yields an empty (non-nil) slice —
	// the scheduler's matrix is then empty and it dispatches nothing.
	reg, err := ParseRegistry([]byte(validRegistryYAML))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if got := reg.DaytimeAssignments(); len(got) != 0 {
		t.Errorf("DaytimeAssignments() = %+v, want empty", got)
	}
}

// TestCommittedRegistryValid guards the actual checked-in registry: it must
// parse, validate, and carry the two launch reservations. A bad data edit
// fails here before it can break the broker workflows.
func TestCommittedRegistryValid(t *testing.T) {
	reg, err := LoadRegistryFile(filepath.Join("..", "..", "infra", "uat", "reservations.yaml"))
	if err != nil {
		t.Fatalf("committed reservations.yaml invalid: %v", err)
	}
	want := map[string]string{"aws-h100": CloudAWS, "gcp-h100": CloudGCP, "azure-h100": CloudAzure}
	for name, cloud := range want {
		res, err := reg.Lookup(name)
		if err != nil {
			t.Errorf("committed registry missing %q: %v", name, err)
			continue
		}
		if res.Cloud != cloud {
			t.Errorf("%q cloud = %q, want %q", name, res.Cloud, cloud)
		}
	}

	// The launch cloud→flavor split (#1281, DC8): AWS hosts training, GCP hosts
	// inference. A future re-split changes these values here deliberately.
	assignments := reg.DaytimeAssignments()
	gotIntent := make(map[string]string, len(assignments))
	for _, a := range assignments {
		gotIntent[a.Reservation] = a.Intent
	}
	wantIntent := map[string]string{"aws-h100": IntentTraining, "gcp-h100": IntentInference}
	for name, intent := range wantIntent {
		if gotIntent[name] != intent {
			t.Errorf("committed registry daytime-intent[%q] = %q, want %q", name, gotIntent[name], intent)
		}
	}
	// At most one daytime reservation per cloud: one reservation cannot host
	// both a held daytime cluster and the nightly batch at once.
	perCloud := make(map[string]int)
	for _, a := range assignments {
		res, lookupErr := reg.Lookup(a.Reservation)
		if lookupErr != nil {
			t.Fatalf("Lookup(%q): %v", a.Reservation, lookupErr)
		}
		perCloud[res.Cloud]++
	}
	for cloud, n := range perCloud {
		if n > 1 {
			t.Errorf("cloud %q has %d daytime reservations, want at most 1 (both flavors per cloud is out of scope at launch)", cloud, n)
		}
	}

	// The launch nightly intents (#1276, DC3): BOTH training and inference run
	// nightly on BOTH reservations, serialized through the shared lease. A
	// future change to the per-reservation intent set changes this deliberately.
	wantNightly := map[string][]string{
		"aws-h100": {IntentTraining, IntentInference},
		"gcp-h100": {IntentTraining, IntentInference},
		// azure-h100 is bring-up-phase: explicitly opted out of the nightly
		// batch (nightly-intents: []) until the pipeline has green manual
		// runs; flipped to [training] (then both) in the follow-up PR.
		"azure-h100": {},
	}
	for name, want := range wantNightly {
		res, lookupErr := reg.Lookup(name)
		if lookupErr != nil {
			t.Errorf("committed registry missing %q: %v", name, lookupErr)
			continue
		}
		got := res.NightlyIntentsOrDefault()
		if !slices.Equal(got, want) {
			t.Errorf("committed registry nightly-intents[%q] = %v, want %v", name, got, want)
		}
	}
}

// TestParseRegistryBareNullNightlyIntents locks the authoring edge documented
// on Reservation.NightlyIntents: a bare `nightly-intents:` (YAML null)
// decodes to nil — indistinguishable from an absent key — so the reservation
// resolves to the [training] default (opt-IN), and only an explicit `[]`
// opts out. KnownFields cannot catch this; if this test ever fails because
// the decode semantics changed, update the field's doc comment too.
func TestParseRegistryBareNullNightlyIntents(t *testing.T) {
	const doc = `
reservations:
  - name: azure-h100
    cloud: azure
    accelerator: h100
    gpu-count: 8
    cluster-config-path: c.yaml
    test-config-dir: t
    nightly-intents:
`
	reg, err := ParseRegistry([]byte(doc))
	if err != nil {
		t.Fatalf("ParseRegistry(bare-null nightly-intents) = %v, want nil error", err)
	}
	got := reg.Reservations[0].NightlyIntentsOrDefault()
	if len(got) != 1 || got[0] != IntentTraining {
		t.Errorf("NightlyIntentsOrDefault() = %v, want [%s] (bare null must behave as absent)", got, IntentTraining)
	}
}

func TestNightlyIntentsOrDefault(t *testing.T) {
	tests := []struct {
		name string
		set  []string
		want []string
	}{
		// Absent (nil) keeps the pre-DC3 training default.
		{"nil defaults to training", nil, []string{IntentTraining}},
		// Explicit empty list is a nightly OPT-OUT, not the default: a
		// quota-backed bring-up reservation must be manually dispatchable
		// without being swept into the nightly batch.
		{"explicit empty list opts out", []string{}, []string{}},
		{"explicit training only", []string{IntentTraining}, []string{IntentTraining}},
		{"both intents", []string{IntentTraining, IntentInference}, []string{IntentTraining, IntentInference}},
		{"inference only", []string{IntentInference}, []string{IntentInference}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Reservation{NightlyIntents: tt.set}
			if got := r.NightlyIntentsOrDefault(); !slices.Equal(got, tt.want) {
				t.Errorf("NightlyIntentsOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}

	// Opt-out must be distinguishable from absent at the caller: the broker
	// prints strings.Join(...) — empty for opt-out — while nil (absent)
	// resolves to "training". slices.Equal treats nil and []string{} as equal,
	// so assert the length explicitly.
	r := Reservation{NightlyIntents: []string{}}
	if got := r.NightlyIntentsOrDefault(); len(got) != 0 {
		t.Errorf("explicit empty NightlyIntents = %v, want empty (opt-out)", got)
	}
}

// TestNightlyIntentsOrDefaultCopies verifies the returned slice is a copy —
// mutating it must not corrupt the registry's backing array.
func TestNightlyIntentsOrDefaultCopies(t *testing.T) {
	r := Reservation{NightlyIntents: []string{IntentTraining, IntentInference}}
	got := r.NightlyIntentsOrDefault()
	got[0] = "mutated"
	if r.NightlyIntents[0] != IntentTraining {
		t.Errorf("mutating the returned slice corrupted the reservation: %v", r.NightlyIntents)
	}
}

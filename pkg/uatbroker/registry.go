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
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// maxRegistryBytes bounds the registry file read so an attacker-influenced
// path cannot OOM the process; the registry is a small hand-edited file.
const maxRegistryBytes int64 = 1 << 20 // 1 MiB

// ParseRegistry parses and validates a reservations.yaml document. Decoding
// is strict (KnownFields): a mistyped key like `nightly-intnts:` must fail
// the parse rather than silently leave the real field on its default and
// fail open (e.g. re-enrolling an opted-out row in the nightly batch).
func ParseRegistry(data []byte) (*Registry, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var reg Registry
	if err := dec.Decode(&reg); err != nil {
		if stderrors.Is(err, io.EOF) {
			// Empty document: report the canonical "no reservations"
			// validation error instead of a cryptic EOF.
			return nil, reg.Validate()
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse reservation registry", err)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// LoadRegistryFile reads, size-bounds, parses, and validates the
// reservation registry at path.
func LoadRegistryFile(path string) (*Registry, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied registry path (CLI flag), size-bounded below
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "open reservation registry "+path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read reservation registry "+path, err)
	}
	if int64(len(data)) > maxRegistryBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "reservation registry "+path+" exceeds size limit")
	}
	return ParseRegistry(data)
}

// Validate enforces registry invariants: at least one row; every row has the
// required fields (reservation-id is optional — quota-backed rows have none);
// cloud is recognized; gpu-count is positive; and names are unique (the name
// is the lease key, so a duplicate would make the lease ambiguous).
func (r *Registry) Validate() error {
	if len(r.Reservations) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "reservation registry has no reservations")
	}
	seen := make(map[string]bool, len(r.Reservations))
	// daytimeCloud tracks which reservation already claimed each cloud's daytime
	// slot. At most one reservation per cloud may opt into the daytime rotation
	// (see below).
	daytimeCloud := make(map[string]string, len(r.Reservations))
	for i := range r.Reservations {
		res := &r.Reservations[i]
		if strings.TrimSpace(res.Name) == "" {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("reservation[%d] has an empty name", i))
		}
		if seen[res.Name] {
			return errors.New(errors.ErrCodeInvalidRequest, "duplicate reservation name "+res.Name)
		}
		seen[res.Name] = true
		if !validClouds[res.Cloud] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("reservation %s has unknown cloud %q (want %s, %s, or %s)",
					res.Name, res.Cloud, CloudAWS, CloudGCP, CloudAzure))
		}
		// reservation-id is intentionally NOT required: quota-backed rows
		// (Azure subscription quota) have no capacity-reservation identifier.
		for _, f := range []struct{ key, val string }{
			{"accelerator", res.Accelerator},
			{"cluster-config-path", res.ClusterConfigPath},
			{"test-config-dir", res.TestConfigDir},
		} {
			if strings.TrimSpace(f.val) == "" {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s has an empty %s", res.Name, f.key))
			}
		}
		if res.GPUCount <= 0 {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("reservation %s has a non-positive gpu-count (%d)", res.Name, res.GPUCount))
		}
		// nightly-intents is optional (empty defaults to [training]), but every
		// listed value must be a recognized intent — a typo would otherwise
		// dispatch a nonexistent per-intent config in the nightly batch — and
		// must be unique, since a duplicate would double-run the same cell. There
		// is no one-per-cloud limit (unlike daytime-intent): every reservation
		// runs the nightly batch, and each may run any subset of the intents.
		seenIntent := make(map[string]bool, len(res.NightlyIntents))
		for _, intent := range res.NightlyIntents {
			if !validIntents[intent] {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s has unknown nightly-intent %q (want %s or %s)",
						res.Name, intent, IntentTraining, IntentInference))
			}
			if seenIntent[intent] {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s lists duplicate nightly-intent %q", res.Name, intent))
			}
			seenIntent[intent] = true
		}
		// nightly-intent-min-versions gates RELEASE cells per intent. Each key
		// must be an intent this reservation actually runs (a gate on an unrun
		// intent — including any gate on an explicitly opted-out row — is dead
		// config / a typo that would silently never apply) and each value must
		// parse as semver (else the gate is inert and every release cell runs
		// the intent, defeating the gate). Fail closed on both.
		runsIntent := make(map[string]bool, len(res.NightlyIntents))
		for _, intent := range res.NightlyIntentsOrDefault() {
			runsIntent[intent] = true
		}
		for intent, minVer := range res.NightlyIntentMinVersions {
			if !validIntents[intent] {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s has a nightly-intent-min-version for unknown intent %q (want %s or %s)",
						res.Name, intent, IntentTraining, IntentInference))
			}
			if !runsIntent[intent] {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s gates intent %q via nightly-intent-min-versions but does not run it nightly (add %q to nightly-intents or drop the gate)",
						res.Name, intent, intent))
			}
			if _, err := semver.NewVersion(minVer); err != nil {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s has an invalid nightly-intent-min-version %q for intent %q (want a semver tag like v0.18.0)",
						res.Name, minVer, intent))
			}
		}
		// daytime-intent is optional (empty = not in the daytime rotation), but
		// when set it must be a recognized intent — a typo would otherwise
		// silently drop the reservation from the daytime rotation or dispatch a
		// nonexistent per-intent config.
		if res.DaytimeIntent != "" && !validIntents[res.DaytimeIntent] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("reservation %s has unknown daytime-intent %q (want %s or %s, or empty to opt out)",
					res.Name, res.DaytimeIntent, IntentTraining, IntentInference))
		}
		// At most one daytime reservation per cloud: a single reservation cannot
		// host both a held daytime cluster and the nightly batch at once, so two
		// daytime reservations on one cloud would contend. Enforced here (not just
		// in a test on the committed file) so every caller of ParseRegistry /
		// LoadRegistryFile — future tooling, alternate registries — upholds the
		// invariant the lease/scheduler relies on.
		if res.DaytimeIntent != "" {
			if prev, ok := daytimeCloud[res.Cloud]; ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("cloud %s has more than one daytime-intent reservation (%s and %s); at most one is allowed",
						res.Cloud, prev, res.Name))
			}
			daytimeCloud[res.Cloud] = res.Name
		}
	}
	return nil
}

// DaytimeAssignments returns the daytime human-access rotation (#1281, DC8):
// one entry per reservation that opts in via a non-empty daytime-intent, in
// registry (document) order. Reservations with an empty daytime-intent are
// nightly-batch only and omitted.
func (r *Registry) DaytimeAssignments() []DaytimeAssignment {
	out := make([]DaytimeAssignment, 0, len(r.Reservations))
	for i := range r.Reservations {
		if r.Reservations[i].DaytimeIntent == "" {
			continue
		}
		out = append(out, DaytimeAssignment{
			Reservation: r.Reservations[i].Name,
			Intent:      r.Reservations[i].DaytimeIntent,
		})
	}
	return out
}

// Lookup returns the reservation row with the given name, or an
// ErrCodeNotFound error when no row matches.
func (r *Registry) Lookup(name string) (*Reservation, error) {
	for i := range r.Reservations {
		if r.Reservations[i].Name == name {
			// Return a copy so callers cannot mutate the registry's internal
			// slice (and bypass Validate) through the returned pointer.
			res := r.Reservations[i]
			return &res, nil
		}
	}
	return nil, errors.New(errors.ErrCodeNotFound, "reservation "+name+" not found in registry")
}

// Names returns the reservation names in registry (document) order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Reservations))
	for i := range r.Reservations {
		names = append(names, r.Reservations[i].Name)
	}
	return names
}

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

package upgrade

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

// Source is the subset of recipe.DataProvider this package needs.
type Source interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// Component names a registry entry that may reference an upgrade record.
type Component struct {
	// Name is the registry component name.
	Name string
	// File is ComponentConfig.Upgrades.File. Empty means no record.
	File string
	// PinnedVersion is defaultVersion (Helm) or defaultTag (Kustomize).
	PinnedVersion string
}

// Set holds loaded records keyed by component name.
//
// A Set and everything reachable from it is read-only by contract: consumers
// share the same pointers, and nothing re-runs Validate after a mutation.
type Set map[string]*ComponentUpgrades

// Load reads and decodes each component's record.
//
// It fails closed. An unreadable or unrecognized record returns an error naming
// what was found and what was expected; it is never skipped and never degraded
// to the unknown verdict, because "a record exists and I could not read it" is
// not "no record exists".
func Load(ctx context.Context, src Source, comps []Component) (Set, error) {
	set := make(Set)
	seen := make(map[string]bool, len(comps))
	for _, c := range comps {
		if seen[c.Name] {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q appears more than once; the later entry would silently discard the earlier record", c.Name))
		}
		seen[c.Name] = true
		if c.File == "" {
			continue
		}
		if src == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q references upgrades file %q but no source was given to read it from", c.Name, c.File))
		}
		readCtx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
		data, err := src.ReadFile(readCtx, c.File)
		cancel()
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
				"failed to read upgrades file for component %q from %q", c.Name, c.File), err)
		}
		u, err := decodeRecord(data, c)
		if err != nil {
			return nil, err
		}
		set[c.Name] = u
	}
	return set, nil
}

func decodeRecord(data []byte, c Component) (*ComponentUpgrades, error) {
	var u ComponentUpgrades
	// Strict: an unknown or misspelled field is an error, not silently
	// dropped. A typo'd `hooks:` or `verifedBy:` would otherwise weaken a
	// record with no rule firing. Matches pkg/testgrid and
	// pkg/evidence/allowlist, which already use KnownFields(true).
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&u); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse upgrades file %q for component %q", c.File, c.Name), err)
	}
	// A second document silently vanishes rather than firing any rule: "a
	// record exists and it was not looked at" is no more "no record exists"
	// than the apiVersion case below. A bare trailing `---` decodes cleanly
	// to an empty second document rather than reaching io.EOF, and it is
	// rejected the same as any other second document rather than
	// special-cased as harmless — telling an intentionally empty document
	// apart from a truncated one is exactly the ambiguity this package fails
	// closed on elsewhere.
	switch err := dec.Decode(new(ComponentUpgrades)); {
	case stderrors.Is(err, io.EOF):
		// The sole document was the last one, as required.
	case err == nil:
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s contains more than one YAML document; a %s file holds exactly one", c.File, ComponentUpgradesKind))
	default:
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"failed to parse %s past its first document", c.File), err)
	}
	// Header identity is established before the document's contents are
	// judged: an empty transitions list is only meaningful once the document
	// is known to be a ComponentUpgrades at all.
	if u.Kind != ComponentUpgradesKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s has kind %q, expected %q; use a ComponentUpgrades document compatible with this aicr release",
			c.File, u.Kind, ComponentUpgradesKind))
	}
	// ComponentUpgrades starts at its ADR-022 target rather than on the alpha
	// track, so there is no alpha version to accept here and later retire.
	if u.APIVersion != header.GroupVersionV1Beta1 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s has apiVersion %q, expected %q for %s; update the record header for this aicr release",
			c.File, u.APIVersion, header.GroupVersionV1Beta1, ComponentUpgradesKind))
	}
	if len(u.Transitions) == 0 && u.Replaces == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s declares neither transitions nor a replaces block; a record that asserts nothing must not read as well-formed",
			c.File))
	}
	if u.Component != c.Name {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s declares component %q but is referenced by registry entry %q",
			c.File, u.Component, c.Name))
	}
	for i := range u.Transitions {
		if err := checkTransitionReadable(c.File, i, &u.Transitions[i]); err != nil {
			return nil, err
		}
	}
	if u.Replaces != nil && !u.Replaces.Verdict.Authorable() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s replaces block has verdict %q; only safe, manual and blocked may be authored",
			c.File, u.Replaces.Verdict))
	}
	return &u, nil
}

// checkTransitionReadable enforces the verdict-independent requirements without
// which a transition cannot be used at all.
func checkTransitionReadable(path string, idx int, tr *Transition) error {
	where := fmt.Sprintf("%s transition %d", path, idx)
	if !tr.Verdict.Authorable() {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s has verdict %q; only safe, manual and blocked may be authored (unknown and unversioned are computed)",
			where, tr.Verdict))
	}
	if tr.Summary == "" {
		return errors.New(errors.ErrCodeInvalidRequest, where+" is missing summary")
	}
	if _, err := parseBounds(tr.From); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, where+" has an invalid from range", err)
	}
	if _, err := parseBounds(tr.To); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, where+" has an invalid to range", err)
	}
	return nil
}

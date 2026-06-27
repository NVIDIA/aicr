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

package verifier

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/allowlist"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// EvidenceDirName is the repo-relative root of the committed evidence tree.
const EvidenceDirName = "recipes/evidence"

// AllowlistFileName is the basename of the signer allowlist at the evidence
// root. It is the one file at the root that is not a recipe directory.
const AllowlistFileName = "allowlist.yaml"

// DiscoverPointers returns, sorted, the per-source pointer files for one
// recipe under root, i.e. the glob <root>/<recipe>/<source>/*.yaml (issue
// #1347 Option A). Each match is an immutable single-attestation V1 pointer;
// callers iterate over the set and aggregate across sources rather than
// assuming a single fixed-path file. A recipe with no committed evidence
// yields an empty slice and no error.
func DiscoverPointers(root, recipe string) ([]string, error) {
	if recipe == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe is required")
	}
	matches, err := filepath.Glob(filepath.Join(root, recipe, "*", "*.yaml"))
	if err != nil {
		// filepath.Glob only errors on a malformed pattern, which a literal
		// join cannot produce; guard anyway rather than swallow it.
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to glob evidence pointers", err)
	}
	sort.Strings(matches)
	return matches, nil
}

// TreeProblem is one path-ownership / allowlist violation found by
// CheckEvidenceTree. Path is the offending pointer file (relative to the
// process working directory, as walked).
type TreeProblem struct {
	Path    string
	Message string
}

func (p TreeProblem) String() string { return p.Path + ": " + p.Message }

// CheckEvidenceTree enforces the per-source pointer contract over every
// committed pointer under root, using the allowlist at allowlistPath. It is
// the anti-squat gate (issue #1401): a pointer is rejected unless
//
//   - it parses and validates as a single-attestation V1 pointer;
//   - its attestation carries a signer with identity + issuer;
//   - the <recipe> path segment equals the pointer's recipe;
//   - the <source> path segment equals SourceSlug(signer.issuer, signer.identity)
//     — so a party cannot write under another party's directory; and
//   - that verified signer is allowlisted as community or partner (first-party
//     ingests directly and must not commit per-run pointers).
//
// It returns the list of problems (empty when the tree is clean) plus a
// non-nil error only for an operational failure (unreadable allowlist, etc.),
// keeping policy violations distinct from infrastructure errors.
func CheckEvidenceTree(root, allowlistPath string) ([]TreeProblem, error) {
	al, err := allowlist.Load(allowlistPath)
	if err != nil {
		// allowlist.Load already returns coded errors (NotFound for a missing
		// file, InvalidRequest for a malformed one); preserve them rather than
		// flattening every failure to InvalidRequest.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "failed to load signer allowlist")
	}

	recipeDirs, err := os.ReadDir(root)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read evidence root", err)
	}

	var problems []TreeProblem
	for _, rd := range recipeDirs {
		if !rd.IsDir() {
			// Only allowlist.yaml is expected as a non-directory at the root.
			if rd.Name() != AllowlistFileName {
				problems = append(problems, TreeProblem{
					Path:    filepath.Join(root, rd.Name()),
					Message: "unexpected file at evidence root (pointers live under <recipe>/<source>/)",
				})
			}
			continue
		}
		problems = append(problems, checkRecipeDir(root, rd.Name(), al)...)
	}
	return problems, nil
}

// checkRecipeDir walks one <recipe>/ directory: every pointer must sit two
// levels deep, under a <source>/ subdirectory.
func checkRecipeDir(root, recipe string, al *allowlist.Allowlist) []TreeProblem {
	recipePath := filepath.Join(root, recipe)
	sourceDirs, err := os.ReadDir(recipePath)
	if err != nil {
		return []TreeProblem{{Path: recipePath, Message: "failed to read recipe directory: " + err.Error()}}
	}

	var problems []TreeProblem
	for _, sd := range sourceDirs {
		if !sd.IsDir() {
			problems = append(problems, TreeProblem{
				Path:    filepath.Join(recipePath, sd.Name()),
				Message: "pointer must live under <recipe>/<source>/, not directly in the recipe directory",
			})
			continue
		}
		problems = append(problems, checkSourceDir(recipePath, recipe, sd.Name(), al)...)
	}
	return problems
}

// checkSourceDir validates every *.yaml pointer in one <recipe>/<source>/
// directory against the contract.
func checkSourceDir(recipePath, recipe, source string, al *allowlist.Allowlist) []TreeProblem {
	sourcePath := filepath.Join(recipePath, source)
	files, err := os.ReadDir(sourcePath)
	if err != nil {
		return []TreeProblem{{Path: sourcePath, Message: "failed to read source directory: " + err.Error()}}
	}

	var problems []TreeProblem
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(sourcePath, f.Name())
		if msg := checkPointerFile(path, recipe, source, al); msg != "" {
			problems = append(problems, TreeProblem{Path: path, Message: msg})
		}
	}
	return problems
}

// checkPointerFile validates a single committed pointer file and returns a
// non-empty message describing the first violation, or "" when it is clean.
func checkPointerFile(path, recipe, source string, al *allowlist.Allowlist) string {
	ptr, err := LoadAndValidatePointer(path)
	if err != nil {
		return "invalid pointer: " + err.Error()
	}
	if ptr.Recipe != recipe {
		return "pointer.recipe " + ptr.Recipe + " does not match recipe directory " + recipe
	}
	signer := ptr.Attestations[0].Signer
	if signer == nil {
		return "committed pointer must be signed (attestation has no signer)"
	}
	wantSource, err := attestation.SourceSlug(signer.Issuer, signer.Identity)
	if err != nil {
		return "cannot derive source slug from signer: " + err.Error()
	}
	if wantSource != source {
		return "signer " + signer.Identity + " maps to source " + wantSource +
			" but the pointer is committed under " + source + " (path ownership / squat)"
	}
	class, _, ok := al.Classify(signer.Issuer, signer.Identity)
	if !ok {
		return "signer " + signer.Identity + " (issuer " + signer.Issuer +
			") is not in the allowlist; add a community/partner entry to recipes/evidence/allowlist.yaml"
	}
	if class == allowlist.ClassFirstParty {
		return "first-party signer must ingest directly, not commit a per-run pointer"
	}
	return ""
}

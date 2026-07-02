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

package project

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"gopkg.in/yaml.v3"
)

// k8sServerVersionConstraint is the recipe constraint whose value is
// surfaced as meta.json's k8sConstraint (display only).
const k8sServerVersionConstraint = "K8s.server.version"

// runIDTimeLayout formats the attestedAt timestamp into the default run
// identifier, e.g. attestedAt 2026-06-25T05:57:23Z -> "run-20260625T0557".
// Stable and deterministic: the same predicate always yields the same id.
const runIDTimeLayout = "20060102T1504"

// resultsDir is the top-level directory under the output root that holds
// the source-keyed tree. The consumer walks for meta.json beneath it.
const resultsDir = "results"

// In is the input to Synthesize. Every signer field must be the
// cryptographically *verified* value (Fulcio cert SAN + OIDC issuer +
// Rekor index), never an unverified pointer claim — the caller is
// responsible for having run verification first.
type In struct {
	// BundleDir is the unpacked, verified summary-bundle directory. Its
	// recipe.yaml supplies the coordinate and K8s constraint; its
	// ctrf/<phase>.json files are copied into the run directory.
	BundleDir string

	// Predicate is the verified predicate body (preferably from the
	// signed DSSE payload). Supplies attestedAt, aicrVersion, recipe
	// name, k8s version, and the bundle (manifest) digest.
	Predicate *attestation.Predicate

	// SignerIdentity and SignerIssuer are the verified OIDC claims. An
	// empty SignerIdentity means no verified signer and Synthesize
	// fails closed — unverified evidence must never reach the tree.
	SignerIdentity string
	SignerIssuer   string

	// RekorLogIndex is the verified transparency-log index, or nil when
	// no Rekor entry exists. Omitted from meta.json when nil.
	RekorLogIndex *int64

	// Class and Allowlisted are the trust verdict from Allowlist.Classify.
	Class       Class
	Allowlisted bool

	// EvidenceRef is the OCI reference of the signed bundle, recorded as
	// the drilldown link. Empty is allowed (e.g. a local-only ingest).
	EvidenceRef string

	// RunID overrides the run identifier. When empty, it is derived
	// deterministically from the predicate's attestedAt.
	RunID string

	// OutRoot is the root directory the source-keyed tree is written
	// under (the tree lives at <OutRoot>/results/...).
	OutRoot string
}

// Result reports what Synthesize wrote.
type Result struct {
	// RunDir is the absolute-or-relative path of the per-run directory
	// (…/results/<group>/<dashboard>/<tab>/<idHash>/<runId>).
	RunDir string

	// Coordinate is the recipe placement the meta.json records.
	Coordinate recipe.Coordinate

	// IDHash is the source-dedup key derived from the verified signer.
	IDHash string

	// Phases lists the ctrf/<phase>.json files copied, in canonical order.
	Phases []string
}

// recipeView is the minimal projection of the bundle's recipe.yaml
// (a canonicalized recipe.RecipeResult) needed to place the evidence.
type recipeView struct {
	Criteria    *recipe.Criteria    `yaml:"criteria"`
	Constraints []recipe.Constraint `yaml:"constraints"`
}

// Synthesize verifies the inputs, derives the recipe coordinate and
// signer id-hash, and writes the source-keyed run directory
// (meta.json plus the present ctrf/<phase>.json reports) under
// In.OutRoot. It is idempotent on identical input: re-ingesting the
// same bundle replaces the run directory in place rather than
// duplicating it.
//
// Synthesize never consults the network and performs no verification of
// its own — the caller must pass already-verified inputs.
func Synthesize(ctx context.Context, in In) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "synthesize canceled", err)
	}
	if in.Predicate == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "predicate is required")
	}
	if in.SignerIdentity == "" || in.SignerIssuer == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"no verified signer (identity/issuer) — refusing to ingest unverified evidence")
	}
	if in.OutRoot == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "output root is required")
	}

	view, err := readRecipeView(in.BundleDir)
	if err != nil {
		return nil, err
	}
	coord, err := recipe.CoordinateFor(view.Criteria)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "derive coordinate from bundle recipe", err)
	}

	recipeName := in.Predicate.Recipe.Name
	if recipeName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "predicate recipe.name is empty")
	}
	if !filepath.IsLocal(recipeName) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "predicate recipe.name is not a local path segment: "+recipeName)
	}

	runID := in.RunID
	if runID == "" {
		runID = "run-" + in.Predicate.AttestedAt.UTC().Format(runIDTimeLayout)
	}
	if !filepath.IsLocal(runID) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "run id is not a local path segment: "+runID)
	}

	idHash := SignerIDHash(in.SignerIssuer, in.SignerIdentity)

	meta := &Meta{
		SchemaVersion: MetaSchemaVersion,
		Coordinate:    Coordinate{Group: coord.Group, Dashboard: coord.Dashboard, Tab: coord.Tab},
		Recipe:        recipeName,
		Signer: Signer{
			IDHash:      idHash,
			Identity:    in.SignerIdentity,
			Issuer:      in.SignerIssuer,
			Class:       in.Class,
			Allowlisted: in.Allowlisted,
		},
		RunID:         runID,
		AICRVersion:   in.Predicate.AICRVersion,
		K8sVersion:    in.Predicate.Fingerprint.K8sVersion.Value,
		K8sConstraint: constraintValue(view.Constraints, k8sServerVersionConstraint),
		BundleDigest:  in.Predicate.Manifest.Digest,
		EvidenceRef:   in.EvidenceRef,
		RekorLogIndex: in.RekorLogIndex,
		AttestedAt:    in.Predicate.AttestedAt.UTC().Format(time.RFC3339),
	}

	// Path segments are all validated: coordinate segments reject "/"
	// (recipe.CoordinateFor), idHash is hex, recipeName/runID passed
	// filepath.IsLocal. The tree lives under <OutRoot>/results.
	runDir := filepath.Join(in.OutRoot, resultsDir, coord.Group, coord.Dashboard, coord.Tab, idHash, runID)

	// Atomic idempotent replace: stage the full tree in a sibling temp
	// directory, then swap it in with a single rename. Clearing runDir up
	// front (as a naive replace would) destroys the last good result
	// before its replacement exists, so a cancellation or I/O failure
	// mid-write would leave nothing behind. Staging + rename keeps the
	// previous result intact until the new tree is complete.
	parent := filepath.Dir(runDir)
	if err = os.MkdirAll(parent, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "create run parent dir", err)
	}
	staging, err := os.MkdirTemp(parent, ".staging-"+idHash+"-")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "create staging dir", err)
	}
	// Removed on any early return below; a no-op once the rename has
	// consumed it, so a successful run leaves no staging dir behind.
	defer func() { _ = os.RemoveAll(staging) }()
	if err = os.Chmod(staging, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "chmod staging dir", err)
	}

	phases, err := copyCTRFReports(ctx, in.BundleDir, staging)
	if err != nil {
		return nil, err
	}

	metaBytes, err := meta.MarshalDeterministic()
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(staging, MetaFilename), metaBytes, 0o600); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "write meta.json", err)
	}

	// Swap in the completed tree. The prior run dir is removed first
	// (os.Rename refuses a non-empty destination); this is the only
	// non-atomic step and holds no long-running I/O.
	if err = os.RemoveAll(runDir); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "clear prior run dir", err)
	}
	if err = os.Rename(staging, runDir); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "swap in run dir", err)
	}

	return &Result{RunDir: runDir, Coordinate: coord, IDHash: idHash, Phases: phases}, nil
}

// readRecipeView reads and parses the bundle's recipe.yaml into the
// minimal projection Synthesize needs. The read is size-bounded against
// a hostile or corrupt bundle.
func readRecipeView(bundleDir string) (*recipeView, error) {
	path := filepath.Join(bundleDir, attestation.RecipeFilename)
	if err := confineToBundle(bundleDir, path); err != nil {
		return nil, err
	}
	data, err := readBoundedFile(path, "bundle recipe.yaml", defaults.EvidenceMaxOutputBytes)
	if err != nil {
		return nil, err
	}

	var view recipeView
	if err := yaml.Unmarshal(data, &view); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse bundle recipe.yaml", err)
	}
	if view.Criteria == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle recipe.yaml has no criteria")
	}
	return &view, nil
}

// readBoundedFile reads path into memory, capped at max bytes. The +1
// probe on the LimitReader detects an at-or-over-cap file without
// buffering the whole oversized payload — a guard against an
// attacker-influenced bundle path (symlink, /proc, network mount) where
// os.ReadFile would allocate the entire file first. label names the file
// in errors. Mirrors verifier.readBoundedFile, duplicated here to keep
// this package free of the verifier's OCI/sigstore dependency tree.
func readBoundedFile(path, label string, max int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // bundle-local or operator-supplied path, validated by caller
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "open "+label, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+label, err)
	}
	if int64(len(data)) > max {
		return nil, errors.New(errors.ErrCodeInvalidRequest, label+" exceeds size limit")
	}
	return data, nil
}

// confineToBundle resolves path (following any symlinks in it or its
// parents) and fails closed unless it stays inside bundleDir. A summary
// bundle is signed content, but its packed tar can still embed symlinks;
// this guard confines every bundle-local read so a crafted entry cannot
// redirect a read or copy to a host file outside the verified bundle. The
// path must exist — callers probe for absence (os.Lstat) beforehand where
// a missing file is legitimate.
func confineToBundle(bundleDir, path string) error {
	realRoot, err := filepath.EvalSymlinks(bundleDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "resolve bundle dir", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "resolve bundle file "+path, err)
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || !filepath.IsLocal(rel) {
		return errors.New(errors.ErrCodeInvalidRequest, "bundle file escapes bundle dir: "+path)
	}
	return nil
}

// copyCTRFReports copies each present ctrf/<phase>.json from the bundle
// into the run directory, in canonical phase order. A phase the run did
// not produce is simply absent — it is skipped, never stubbed. Returns
// the list of copied phase names.
func copyCTRFReports(ctx context.Context, bundleDir, runDir string) ([]string, error) {
	ctrfOut := filepath.Join(runDir, "ctrf")
	var copied []string
	for _, phase := range attestation.AllPhases {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "copy ctrf canceled", err)
		}
		name := string(phase) + ".json"
		src := filepath.Join(bundleDir, "ctrf", name)
		// Existence probe without following the final-component symlink: a
		// phase the run did not produce is simply absent and skipped.
		if _, lerr := os.Lstat(src); lerr != nil {
			if os.IsNotExist(lerr) {
				continue
			}
			return nil, errors.Wrap(errors.ErrCodeInternal, "stat ctrf "+name, lerr)
		}
		// Confine the read to the bundle before resolving its type, so a
		// symlinked report (or symlinked ctrf dir) cannot redirect the copy
		// to a host file outside the verified bundle.
		if err := confineToBundle(bundleDir, src); err != nil {
			return nil, err
		}
		info, statErr := os.Stat(src)
		if statErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "stat ctrf "+name, statErr)
		}
		if info.IsDir() {
			continue
		}
		if len(copied) == 0 {
			if err := os.MkdirAll(ctrfOut, 0o755); err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, "create ctrf dir", err)
			}
		}
		if err := copyFileBounded(src, filepath.Join(ctrfOut, name)); err != nil {
			return nil, err
		}
		copied = append(copied, string(phase))
	}
	return copied, nil
}

// copyFileBounded copies src to dst, refusing a file larger than the
// evidence output cap.
func copyFileBounded(src, dst string) error {
	data, err := readBoundedFile(src, "ctrf report "+filepath.Base(src), defaults.EvidenceMaxOutputBytes)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write ctrf report", err)
	}
	return nil
}

// constraintValue returns the value of the first constraint named name,
// or "" when absent (k8sConstraint is display-only, so a missing
// constraint is not an error).
func constraintValue(constraints []recipe.Constraint, name string) string {
	for _, c := range constraints {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

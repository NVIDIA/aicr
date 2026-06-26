// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

// Command testgrid-publish converts an AICR evidence OCI bundle into a
// TestGrid-compatible GCS feed entry.
//
// It implements the TG2 publish path described in
// https://github.com/NVIDIA/aicr/issues/1267.
//
// Pipeline:
//
//	OCI bundle  ──ORAS pull──▶  bundle dir
//	                              │
//	                              ├── recipe.yaml      ──▶ CoordinateFor() ──▶ GCS path
//	                              └── ctrf/*.json      ──▶ jUnit XML
//	                                  (+ predicate.json for AttestedAt / digest)
//	                                                         │
//	                                         ┌──────────────┘
//	                                         ▼
//	                         gs://<bucket>/groups/<group>/<dashboard>/<tab>/<build-id>/
//	                             started.json
//	                             finished.json
//	                             artifacts/junit.xml
//
// Usage:
//
//	testgrid-publish --bundle <oci-ref> --bucket <gcs-bucket>
//	testgrid-publish --bundle ghcr.io/nvidia/aicr-evidence:sha256-abc123 \
//	                 --bucket aicr-testgrid-staging \
//	                 --source-class uat
package main

import (
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
)

func main() {
	var (
		bundleRef   string
		bundleDir   string
		bucket      string
		sourceClass string
		plainHTTP   bool
		insecureTLS bool
		dryRun      bool
	)
	flag.StringVar(&bundleRef, "bundle", "", "OCI reference of the evidence bundle")
	flag.StringVar(&bundleDir, "bundle-dir", "", "pre-materialized bundle directory (alternative to --bundle)")
	flag.StringVar(&bucket, "bucket", "", "GCS bucket to publish to (required)")
	flag.StringVar(&sourceClass, "source-class", sourceClassUAT, "bundle origin: "+sourceClassUAT+" or "+sourceClassCommunity)
	flag.BoolVar(&plainHTTP, "plain-http", false, "use plain HTTP for OCI registry (dev only)")
	flag.BoolVar(&insecureTLS, "insecure-tls", false, "skip TLS verification for OCI registry (dev only)")
	flag.BoolVar(&dryRun, "dry-run", false, "print output paths and started.json without writing to GCS")
	flag.Parse()

	if (bundleRef == "") == (bundleDir == "") {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "testgrid-publish: exactly one of --bundle or --bundle-dir is required")
		os.Exit(1)
	}
	if bucket == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "testgrid-publish: --bucket is required")
		os.Exit(1)
	}
	if sourceClass != sourceClassUAT && sourceClass != sourceClassCommunity {
		fmt.Fprintf(os.Stderr, "testgrid-publish: --source-class must be %q or %q\n",
			sourceClassUAT, sourceClassCommunity)
		os.Exit(1)
	}

	ctx := context.Background()
	if err := run(ctx, runConfig{
		bundleRef:   bundleRef,
		bundleDir:   bundleDir,
		bucket:      bucket,
		sourceClass: sourceClass,
		plainHTTP:   plainHTTP,
		insecureTLS: insecureTLS,
		dryRun:      dryRun,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "testgrid-publish:", err)
		os.Exit(1)
	}
}

type runConfig struct {
	bundleRef   string
	bundleDir   string // pre-materialized (alternative to bundleRef)
	bucket      string
	sourceClass string
	plainHTTP   bool
	insecureTLS bool
	dryRun      bool
}

func run(ctx context.Context, cfg runConfig) error {
	var dir, digest string
	var cleanup func()

	if cfg.bundleDir != "" {
		// Pre-materialized bundle directory — skip OCI pull.
		dir = cfg.bundleDir
		digest = "local"
		cleanup = func() {}
		slog.Info("using pre-materialized bundle", "dir", dir)
		// Auto-resolve a nested summary-bundle/ subdir: aicr validate writes
		// bundles as <output>/summary-bundle/, so --bundle-dir <output> is the
		// natural argument. Redirect silently if recipe.yaml is absent at the
		// top level but present one level down.
		if _, statErr := os.Stat(filepath.Join(dir, attestation.RecipeFilename)); os.IsNotExist(statErr) {
			candidate := filepath.Join(dir, attestation.SummaryBundleDirName)
			if _, statErr2 := os.Stat(filepath.Join(candidate, attestation.RecipeFilename)); statErr2 == nil {
				slog.Info("auto-resolved summary-bundle subdir", "dir", candidate)
				dir = candidate
			}
		}
	} else {
		// ── 1. Materialize the OCI bundle to a local temp directory ───────────
		slog.Info("pulling bundle", "ref", cfg.bundleRef)
		// Bound the pull to the same 2-minute budget used for GCS uploads.
		pullCtx, pullCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
		defer pullCancel()
		mat, err := verifier.MaterializeBundle(pullCtx,
			verifier.VerifyOptions{
				Input:       cfg.bundleRef,
				PlainHTTP:   cfg.plainHTTP,
				InsecureTLS: cfg.insecureTLS,
				// Allow unpinned tags so operators can test with :latest during
				// local dev. TG5 (the GitHub Actions integration) will enforce
				// digest-pinned refs at the workflow level — NVIDIA/aicr#1267.
				// Pinning here would break --bundle-dir smoke tests and early
				// adoption before TG5 ships.
				AllowUnpinnedTag: true,
			},
			verifier.InputFormOCI,
			nil, // pointer not used for direct OCI input
		)
		if err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeUnavailable, "pull failed")
		}
		dir = mat.BundleDir
		digest = mat.Digest
		cleanup = mat.Cleanup
		slog.Info("bundle materialized", "dir", dir, "digest", digest)
	}
	defer cleanup()

	// ── 2. Parse recipe.yaml → criteria → coordinate ─────────────────────────
	criteria, err := parseCriteria(dir)
	if err != nil {
		return err
	}

	coord, err := CoordinateFor(criteria)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "coordinate resolution failed", err)
	}

	// ── 3. Extract AttestedAt, aicr_version, and k8s_version from the predicate
	pred, predErr := loadPredicate(dir)
	if predErr != nil {
		// Only a missing predicate is a safe fallback — unsigned/local bundles
		// legitimately omit statement.intoto.json. A corrupt or unparseable
		// predicate means the bundle is broken; fail closed rather than
		// fabricating an attestation timestamp.
		if !stderrors.Is(predErr, errors.New(errors.ErrCodeNotFound, "")) {
			return predErr
		}
		slog.Warn("predicate not found, using current time", "error", predErr)
		pred = &attestation.Predicate{AttestedAt: time.Now().UTC()}
	}
	// Fingerprint carries the observed k8s server version; empty for local/unsigned bundles.
	criteria.K8sVersion = pred.Fingerprint.K8sVersion.Value

	// ── 4. Generate deterministic build-id ───────────────────────────────────
	bid := buildID(pred.AttestedAt, digest)

	// ── 5. Convert CTRF phases → jUnit XML ───────────────────────────────────
	junitXML, allPassed, err := convertCTRF(dir)
	if err != nil {
		return err
	}

	// ── 6. Build GCS payload ──────────────────────────────────────────────────
	gcsPrefix := fmt.Sprintf("groups/%s/%s", coord.Path(), bid)

	signerIdentity, signerIssuer := extractSigner(dir)

	started := startedJSON{
		Timestamp: pred.AttestedAt.Unix(),
		Metadata: map[string]string{
			metaKeyAICRVersion:    pred.AICRVersion,
			metaKeyK8sVersion:     criteria.K8sVersion,
			metaKeyK8sConstraint:  criteria.K8sConstraint,
			metaKeySignerIdentity: signerIdentity,
			metaKeySignerIssuer:   signerIssuer,
			metaKeySourceClass:    cfg.sourceClass,
			metaKeyEvidenceDigest: digest,
		},
	}

	finishedMeta := make(map[string]string, len(started.Metadata))
	for k, v := range started.Metadata {
		finishedMeta[k] = v
	}
	finished := finishedJSON{
		Timestamp: pred.AttestedAt.Unix(),
		Passed:    allPassed,
		Result:    resultString(allPassed),
		Metadata:  finishedMeta,
	}

	if cfg.dryRun {
		printDryRun(cfg.bucket, gcsPrefix, started, finished, junitXML)
		return nil
	}

	// ── 7. Write to GCS (ordered: started → junit → finished) ────────────────
	slog.Info("writing to GCS", "bucket", cfg.bucket, "prefix", gcsPrefix)
	// EvidenceBundlePushTimeout (2 min) is the right bound for three sequential
	// network uploads; EvidenceBundleBuildTimeout is "local I/O only".
	writeCtx, writeCancel := context.WithTimeout(ctx, defaults.EvidenceBundlePushTimeout)
	defer writeCancel()

	if err := writeGCS(writeCtx, cfg.bucket, gcsPrefix, started, finished, junitXML); err != nil {
		return err
	}

	slog.Info("published",
		"bucket", cfg.bucket,
		"prefix", gcsPrefix,
		"coord", coord.String(),
		"passed", allPassed,
	)
	return nil
}

// resultString returns "SUCCESS" or "FAILURE".
func resultString(passed bool) string {
	if passed {
		return "SUCCESS"
	}
	return "FAILURE"
}

// extractSigner reads signer identity and issuer from the bundle's
// attestation.intoto.jsonl if present; returns empty strings otherwise.
func extractSigner(bundleDir string) (identity, issuer string) {
	pointer, err := readPointerFromAttestation(bundleDir)
	if err != nil || pointer == nil || len(pointer.Attestations) == 0 {
		return "", ""
	}
	att := pointer.Attestations[0]
	if att.Signer == nil {
		return "", ""
	}
	return att.Signer.Identity, att.Signer.Issuer
}

// printDryRun prints the planned GCS paths and started.json to stdout.
func printDryRun(bucket, prefix string, started startedJSON, finished finishedJSON, junitXML []byte) {
	fmt.Printf("dry-run: would write to gs://%s/%s/\n", bucket, prefix)
	fmt.Printf("  started.json\n")
	fmt.Printf("  artifacts/junit.xml (%d bytes)\n", len(junitXML))
	fmt.Printf("  finished.json (passed=%v)\n", finished.Passed)
	fmt.Printf("\nstarted.json metadata:\n")
	keys := make([]string, 0, len(started.Metadata))
	for k := range started.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := started.Metadata[k]; v != "" {
			fmt.Printf("  %-20s = %s\n", k, v)
		} else {
			fmt.Printf("  %-20s = (missing)\n", k)
		}
	}
}

// readPointerFromAttestation reads signer information from the DSSE-wrapped
// attestation bundle. Currently a stub — always returns ErrCodeInternal.
// extractSigner swallows the error and emits empty signer metadata fields.
//
// TODO: implement DSSE envelope parsing to extract PointerSigner fields
// (identity, issuer) from the Sigstore signing certificate in
// attestation.AttestationFilename. Tracking issue: NVIDIA/aicr#1267.
// When implemented, parse failures must NOT be swallowed the same way —
// only a genuinely absent attestation file should fall back to empty strings.
func readPointerFromAttestation(_ string) (*attestation.Pointer, error) {
	return nil, errors.New(errors.ErrCodeInternal, "signer extraction not yet implemented")
}

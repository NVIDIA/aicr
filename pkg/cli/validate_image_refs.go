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

package cli

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// validateOutputsGuard keeps a validate --image-refs file clear of the run's
// other outputs: the --emit-attestation directory, which receives
// pointer.yaml and the summary-bundle/ tree, and the --output report file
// when it is a local file. Unlike the bundle tree, both are written with
// plain file operations and need not exist at preflight.
type validateOutputsGuard struct {
	attestationDir string
	// reportFile is empty when --output goes to stdout or a ConfigMap.
	reportFile string
}

// newValidateOutputsGuard resolves the guarded outputs to absolute paths.
func newValidateOutputsGuard(attestationDir, output string) (*validateOutputsGuard, error) {
	absDir, err := filepath.Abs(attestationDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve --emit-attestation directory", err)
	}
	g := &validateOutputsGuard{attestationDir: filepath.Clean(absDir)}
	report := strings.TrimSpace(output)
	if report == "" || report == serializer.StdoutURI || strings.HasPrefix(report, serializer.ConfigMapURIScheme) {
		return g, nil
	}
	absReport, err := filepath.Abs(report)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve --output file", err)
	}
	g.reportFile = filepath.Clean(absReport)
	return g, nil
}

// overlaps reports whether either path is the other or lies below it. A
// target that is an ancestor of a not-yet-created output would be turned
// into a directory when the run creates that output.
func overlaps(a, b string) bool {
	return pathsConflict(a, b) || pathsConflict(b, a)
}

func (g *validateOutputsGuard) checkPlannedPath(ctx context.Context, absTarget string) error {
	if err := cliFilesystemContextError(ctx, "image-reference target validation canceled"); err != nil {
		return err
	}
	if overlaps(absTarget, g.attestationDir) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target must be outside, and must not contain, the --emit-attestation directory")
	}
	if g.reportFile != "" && overlaps(absTarget, g.reportFile) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target must not be the --output report file")
	}
	return nil
}

func (g *validateOutputsGuard) checkResolvedPath(resolvedTarget string) error {
	resolvedDir, err := resolveExistingPrefix(g.attestationDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			"--emit-attestation directory cannot be resolved", err)
	}
	if overlaps(resolvedTarget, resolvedDir) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target aliases the --emit-attestation directory")
	}
	if g.reportFile == "" {
		return nil
	}
	resolvedReport, err := resolveExistingPrefix(g.reportFile)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "--output file cannot be resolved", err)
	}
	if overlaps(resolvedTarget, resolvedReport) {
		return errors.New(errors.ErrCodeInvalidRequest,
			"image-reference target aliases the --output report file")
	}
	return nil
}

func (g *validateOutputsGuard) preflightAliases(ctx context.Context, t *imageRefsTarget) error {
	return g.checkAliases(ctx, t)
}

// checkAliases walks the resolved attestation directory without following
// symlinks, and compares the target with the report file. An output that
// does not exist yet has nothing to alias.
func (g *validateOutputsGuard) checkAliases(ctx context.Context, t *imageRefsTarget) error {
	targetInfo, err := t.deps.rootLstat(t.root, t.baseName)
	switch {
	case os.IsNotExist(err):
		targetInfo = nil
	case err != nil:
		return errors.Wrap(errors.ErrCodeInternal, "failed to inspect image-reference target", err)
	}
	if targetInfo != nil && g.reportFile != "" {
		report, reportErr := t.deps.lstat(g.reportFile)
		switch {
		case reportErr == nil && t.deps.sameFile(targetInfo, report):
			return errors.New(errors.ErrCodeInvalidRequest,
				"image-reference target is hardlinked to the --output report file")
		case reportErr != nil && !os.IsNotExist(reportErr):
			return errors.Wrap(errors.ErrCodeInternal, "failed to inspect --output file", reportErr)
		}
	}
	root, err := resolveExistingPrefix(g.attestationDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "--emit-attestation directory cannot be resolved", err)
	}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root && os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return errors.Wrap(errors.ErrCodeInternal, "failed to inspect --emit-attestation directory", err)
		}
		if ctxErr := cliFilesystemContextError(ctx, "--emit-attestation directory scan canceled"); ctxErr != nil {
			return ctxErr
		}
		info, err := entry.Info()
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to inspect --emit-attestation entry", err)
		}
		switch {
		case info.IsDir() && t.deps.sameFile(t.parentInfo, info):
			return errors.New(errors.ErrCodeInvalidRequest,
				"image-reference target parent aliases the --emit-attestation directory")
		case info.Mode().IsRegular() && targetInfo != nil && t.deps.sameFile(targetInfo, info):
			return errors.New(errors.ErrCodeInvalidRequest,
				"image-reference target is hardlinked to a file in the --emit-attestation directory")
		}
		return nil
	})
	return errors.PropagateOrWrap(walkErr, errors.ErrCodeInternal, "failed to scan --emit-attestation directory")
}

// resolveExistingPrefix resolves symlinks in the longest existing prefix of
// absPath and re-appends the components that do not exist yet.
func resolveExistingPrefix(absPath string) (string, error) {
	existing := filepath.Clean(absPath)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return filepath.Clean(absPath), nil
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
}

// validateImageRefsUse rejects --image-refs on a run that will push no
// evidence bundle, before any cluster work, so the flag is never silently
// ignored. evidence is the run-mode-filtered recipe-evidence configuration.
func validateImageRefsUse(imageRefsPath string, cncfSubmission, noCluster bool, evidence *recipeEvidenceConfig) error {
	switch {
	case imageRefsPath == "":
		return nil
	case cncfSubmission:
		return errors.New(errors.ErrCodeInvalidRequest,
			"--image-refs cannot be combined with --cncf-submission: that mode pushes no evidence bundle")
	case noCluster:
		return errors.New(errors.ErrCodeInvalidRequest,
			"--image-refs cannot be combined with --no-cluster: an offline dry-run pushes no evidence bundle")
	case evidence == nil || evidence.Push == "":
		return errors.New(errors.ErrCodeInvalidRequest,
			"--image-refs requires --push (with --emit-attestation): it records the digest of the pushed evidence bundle")
	}
	return nil
}

// prepareEvidenceImageRefsTarget validates and retains the --image-refs
// target, and proves it can be written, before the validation run, so a bad
// path fails in seconds rather than after the checks and the push.
func prepareEvidenceImageRefsTarget(
	ctx context.Context,
	imageRefsPath, output string,
	evidence *recipeEvidenceConfig,
) (*imageRefsTarget, error) {

	return prepareEvidenceImageRefsTargetWithDependencies(ctx, imageRefsPath, output, evidence,
		defaultImageRefsTargetDependencies())
}

func prepareEvidenceImageRefsTargetWithDependencies(
	ctx context.Context,
	imageRefsPath, output string,
	evidence *recipeEvidenceConfig,
	deps imageRefsTargetDependencies,
) (*imageRefsTarget, error) {

	if evidence == nil {
		return nil, errors.New(errors.ErrCodeInternal, "image-reference target requires an evidence configuration")
	}
	guard, err := newValidateOutputsGuard(evidence.OutDir, output)
	if err != nil {
		return nil, err
	}
	preflightCtx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	target, err := prepareGuardedImageRefsTarget(preflightCtx, guard, imageRefsPath, deps)
	if err == nil {
		err = target.preflightWrite(preflightCtx)
	}
	err = authoritativeFilesystemContextError(preflightCtx, "image-reference target preflight canceled", err)
	if err != nil {
		return nil, joinInternalCleanup(err, target.close(), "image-reference target")
	}
	return target, nil
}

// writeEvidenceImageRefs writes the pushed evidence bundle's digest to the
// retained --image-refs target, in the same format `aicr bundle --image-refs`
// writes: the digest and a newline.
func writeEvidenceImageRefs(ctx context.Context, target *imageRefsTarget, emission *aicr.EvidenceEmission) error {
	if target == nil {
		return nil
	}
	if emission == nil || emission.PushedDigest == "" {
		return errors.New(errors.ErrCodeInternal,
			"evidence push reported no digest to write to --image-refs")
	}
	writeCtx, cancel := context.WithTimeout(ctx, defaults.OCIImageRefsWriteTimeout)
	defer cancel()
	err := target.write(writeCtx, []byte(emission.PushedDigest+"\n"))
	if err = authoritativeFilesystemContextError(writeCtx, "image-reference publication canceled", err); err != nil {
		return err
	}
	slog.Info("wrote image reference",
		"path", filepath.Join(target.parentPath, target.baseName), "ref", emission.PushedDigest)
	return nil
}

// prepareRecipeEvidence resolves the run's recipe-evidence configuration
// (flag over config, dropped in --no-cluster mode) and, when --image-refs is
// set, rejects a run that pushes nothing and retains the target. Both happen
// before any cluster work. The caller closes the returned target.
func prepareRecipeEvidence(
	ctx context.Context,
	cmd *cli.Command,
	cfg *aicr.Config,
	cncfSubmission, noCluster bool,
) (*recipeEvidenceConfig, *imageRefsTarget, error) {

	attOpts, _, err := cfg.EvidenceAttestationOptions()
	if err != nil {
		return nil, nil, err
	}
	evidence := evidenceConfigForRunMode(noCluster, buildRecipeEvidenceConfig(cmd, attOpts))
	imageRefsPath := cmd.String(flagImageRefs)
	if err = validateImageRefsUse(imageRefsPath, cncfSubmission, noCluster, evidence); err != nil {
		return nil, nil, err
	}
	if imageRefsPath == "" {
		return evidence, nil, nil
	}
	target, err := prepareEvidenceImageRefsTarget(ctx, imageRefsPath, cmd.String("output"), evidence)
	if err != nil {
		return nil, nil, err
	}
	return evidence, target, nil
}

func closeEvidenceImageRefs(target *imageRefsTarget) {
	if err := target.close(); err != nil {
		slog.Warn("failed to close image-reference target", "error", err)
	}
}

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

package bundleinfo

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// FileName is the on-disk name at the bundle root. Exported so consumers
// reference the same name.
//
// Not dot-prefixed on purpose: Helm's default ignore rules drop "."-prefixed
// files, so a hidden name would be silently absent from the packaged
// argocd-helm chart while recipe.yaml survives.
const FileName = "bundle-info.yaml"

// Write serializes info deterministically to dir/bundle-info.yaml and returns
// the byte count. Mode 0600 matches the bundler's recipe.yaml write.
//
// It overwrites info's APIVersion and Kind with the values this package emits,
// which is what keeps the header out of a caller's hands.
func Write(ctx context.Context, dir string, info *BundleInfo) (int64, error) {
	if info == nil {
		return 0, errors.New(errors.ErrCodeInvalidRequest, "bundle info is required")
	}
	if err := contextError(ctx); err != nil {
		return 0, err
	}

	info.APIVersion = header.StableGroupVersion
	info.Kind = string(header.KindBundleInfo)

	if err := validateRequiredFields(info); err != nil {
		return 0, err
	}
	if err := validateRelativePaths(info); err != nil {
		return 0, err
	}

	data, err := serializer.MarshalYAMLDeterministic(info)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to serialize bundle info")
	}
	// Checked on this side too, not just on read: an oversize record would
	// otherwise ship inside checksums.txt and the attestation subject, and
	// fail only once a consumer tried to read it back.
	if int64(len(data)) > defaults.MaxBundleInfoBytes {
		return 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s is %d bytes, over the %d-byte limit", FileName, len(data), defaults.MaxBundleInfoBytes))
	}

	path, joinErr := deployer.SafeJoin(dir, FileName)
	if joinErr != nil {
		return 0, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest, "unsafe bundle info path")
	}
	if err := writeNoFollow(path, data); err != nil {
		return 0, err
	}

	slog.Debug("wrote bundle info", "path", path, "deployer", info.Build.Deployer)
	return int64(len(data)), nil
}

// writeNoFollow writes data to path through a descriptor that refuses a
// symlink, then confirms the descriptor is a regular file.
//
// ValidateOutputRoot rejects a symlinked bundle-info.yaml, but it runs as a
// preflight: anything with write access to the output directory can plant one
// in the window before this write, and os.WriteFile follows the final link and
// truncates whatever it points at (CWE-59). O_NONBLOCK keeps a planted FIFO
// from parking the write on a reader that never arrives.
func writeNoFollow(path string, data []byte) (err error) {
	f, openErr := os.OpenFile( //nolint:gosec // path validated by SafeJoin
		path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0600)
	if openErr != nil {
		if stderrors.Is(openErr, syscall.ELOOP) {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				"refusing to follow file symlink "+path, openErr)
		}
		return errors.Wrap(errors.ErrCodeInternal, "failed to open bundle info for writing", openErr)
	}
	defer func() {
		// The handle is writable, so Close can surface a deferred flush error.
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close bundle info", closeErr)
		}
	}()

	opened, statErr := f.Stat()
	if statErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to inspect opened bundle info", statErr)
	}
	if !opened.Mode().IsRegular() {
		return errors.New(errors.ErrCodeInvalidRequest, "bundle info is not a regular file: "+path)
	}

	if _, writeErr := f.Write(data); writeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write bundle info", writeErr)
	}
	return nil
}

// contextError codes a dead context by its cause. A deadline is an
// environmental fault worth retrying and a cancellation is an instruction to
// stop, and errors.IsTransient splits on exactly that — so collapsing both
// onto ErrCodeTimeout can send a caller back into a retry loop after a
// deliberate abort.
func contextError(ctx context.Context) error {
	err := ctx.Err()
	switch {
	case err == nil:
		return nil
	case stderrors.Is(err, context.Canceled):
		return errors.Wrap(errors.ErrCodeCanceled, "context canceled", err)
	default:
		return errors.Wrap(errors.ErrCodeTimeout, "context deadline exceeded", err)
	}
}

// Read loads and validates dir/bundle-info.yaml.
//
// Fails closed in every direction: the filesystem entry, the size, the
// header, the required fields, any content trailing the first YAML document,
// and every path the record carries. A bundle produced before this artifact
// shipped has no file at all, and that returns ErrCodeNotFound naming the
// reason: a consumer must treat it as a real state and ask the operator for
// the deployer, never fall back to guessing one from the directory layout.
func Read(ctx context.Context, dir string) (*BundleInfo, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	path, joinErr := deployer.SafeJoin(dir, FileName)
	if joinErr != nil {
		return nil, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest, "unsafe bundle info path")
	}

	// SafeJoin is lexical: it only ever sees the constant FileName and never
	// touches the filesystem, so nothing before this point can tell that the
	// entry is a symlink or a device. O_NOFOLLOW and the regular-file check
	// are what keep an untrusted bundle from redirecting this read, matching
	// verifier.readBoundedFileContext and checksum's bundle opens.
	f, err := os.OpenFile( //nolint:gosec // path validated by SafeJoin
		path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if stderrors.Is(err, syscall.ELOOP) {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"refusing to follow file symlink "+path, err)
		}
		if os.IsNotExist(err) {
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("%s has no %s, so the deployer that built it was not recorded; "+
					"the bundle predates build-record stamping", dir, FileName))
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open bundle info", err)
	}
	defer func() { _ = f.Close() }()

	opened, err := f.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to inspect opened bundle info", err)
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle info is not a regular file: "+path)
	}

	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxBundleInfoBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read bundle info", err)
	}
	if int64(len(data)) > defaults.MaxBundleInfoBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s exceeds the %d-byte limit", FileName, defaults.MaxBundleInfoBytes))
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var info BundleInfo
	if err := dec.Decode(&info); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse bundle info", err)
	}

	// A second Decode must hit EOF: anything else, including a second
	// document that decodes cleanly, means content rides after the record
	// that every check below validates, so that content itself is never
	// checked at all.
	var trailing any
	if err := dec.Decode(&trailing); !stderrors.Is(err, io.EOF) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s contains a trailing YAML document after the bundle info record", FileName))
	}

	if info.Kind != string(header.KindBundleInfo) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s declares kind %q, want %q", FileName, info.Kind, header.KindBundleInfo))
	}
	if !header.IsSupportedBundleInfoAPIVersion(info.APIVersion) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s declares apiVersion %q, which this binary does not understand",
				FileName, info.APIVersion))
	}

	if err := validateRequiredFields(&info); err != nil {
		return nil, err
	}
	if err := validateRelativePaths(&info); err != nil {
		return nil, err
	}

	return &info, nil
}

// validateRequiredFields rejects a record that clears the kind, apiVersion and
// path checks while still saying nothing. Every field it demands is
// non-optional in the schema.
//
// Both Read and Write call it. Read is the side that matters: it parses files
// that arrive from OCI registries and GitOps clones, where a missing field
// means the record was hand-written or truncated in transit. Write is
// exported, so its call closes the gap that would otherwise let an external
// caller persist exactly the record Read then rejects.
//
// Releases are exempt from presence: a recipe that resolves to no components
// emits none, and an empty list is the honest record of that. A release that
// is present must still identify itself and where it landed.
func validateRequiredFields(info *BundleInfo) error {
	for _, f := range []struct{ field, value string }{
		{"build.deployer", info.Build.Deployer},
		{"build.recipe.path", info.Build.Recipe.Path},
		{"build.recipe.digest", info.Build.Recipe.Digest},
		{"layout.entrypoint", info.Layout.Entrypoint},
	} {
		if f.value == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s is missing %s", FileName, f.field))
		}
	}

	// Parsed rather than wrapped so the error names the field and the
	// accepted set; config.ParseDeployerType's own message names neither.
	if _, err := config.ParseDeployerType(info.Build.Deployer); err != nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s declares build.deployer %q, which is not one of %v",
				FileName, info.Build.Deployer, config.GetDeployerTypes()))
	}

	for i, r := range info.Layout.Releases {
		for _, f := range []struct{ field, value string }{
			{"name", r.Name},
			{"component", r.Component},
			{"path", r.Path},
		} {
			if f.value == "" {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s is missing layout.releases[%d].%s", FileName, i, f.field))
			}
		}
	}
	return nil
}

// validateRelativePaths rejects info when any path it carries points outside
// the bundle directory. Both Write and Read call it: Write keeps a malformed
// record from being produced, and Read is the side that matters, since the
// file it parses arrived from an OCI registry or a GitOps clone.
//
// Downstream tooling resolves these paths with filepath.Join(outDir, path).
// Join returns an absolute right-hand argument unchanged, and it Cleans the
// result, which collapses a leading "../" into an escape from outDir — so
// either shape would silently read or write outside the bundle instead of
// failing. filepath.IsLocal rejects both, plus Windows reserved names, and
// unlike a substring scan for ".." it accepts benign names like "foo..bak".
func validateRelativePaths(info *BundleInfo) error {
	check := func(field, value string) error {
		// IsLocal("") is false, but an unset optional path is not a traversal;
		// presence is each field's own contract, enforced elsewhere.
		if value != "" && !filepath.IsLocal(value) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s must be a relative path inside the bundle, got %q", field, value))
		}
		return nil
	}

	if err := check("build.recipe.path", info.Build.Recipe.Path); err != nil {
		return err
	}
	if err := check("layout.entrypoint", info.Layout.Entrypoint); err != nil {
		return err
	}
	if err := check("layout.provenance", info.Layout.Provenance); err != nil {
		return err
	}
	for i, r := range info.Layout.Releases {
		if err := check(fmt.Sprintf("layout.releases[%d].path", i), r.Path); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("layout.releases[%d].manifest", i), r.Manifest); err != nil {
			return err
		}
	}
	return nil
}

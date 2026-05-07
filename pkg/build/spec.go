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

package build

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	// ExpectedAPIVersion is the required apiVersion for build spec files.
	ExpectedAPIVersion = "aicr.nvidia.com/v1beta1"
	// ExpectedKind is the required kind for build spec files.
	ExpectedKind = "AICRRuntime"
)

// renameFn is the rename implementation used by WriteBack. It exists as a
// package-private variable so tests can stub it to exercise the rename
// failure cleanup path; production code always uses os.Rename.
var renameFn = os.Rename

// BuildSpec represents the top-level build specification file used by the
// runtime controller. It contains input configuration and output status.
type BuildSpec struct {
	APIVersion string          `yaml:"apiVersion,omitempty"`
	Kind       string          `yaml:"kind,omitempty"`
	Spec       BuildSpecConfig `yaml:"spec"`
	Status     BuildStatus     `yaml:"status,omitempty"`
}

// BuildSpecConfig holds the input configuration for a build operation.
type BuildSpecConfig struct {
	Recipe   string         `yaml:"recipe,omitempty"`
	Version  string         `yaml:"version,omitempty"`
	Target   string         `yaml:"target,omitempty"`
	Registry RegistryConfig `yaml:"registry"`
}

// RegistryConfig holds OCI registry connection details.
type RegistryConfig struct {
	Host        string `yaml:"host"`
	Repository  string `yaml:"repository"`
	InsecureTLS bool   `yaml:"insecureTLS,omitempty"`
}

// BuildStatus holds the output status written back after a build.
type BuildStatus struct {
	Images map[string]ImageStatus `yaml:"images,omitempty"`
}

// ImageStatus describes a single OCI image produced by the build pipeline.
type ImageStatus struct {
	Path       string `yaml:"path,omitempty"`
	Registry   string `yaml:"registry"`
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
	Digest     string `yaml:"digest,omitempty"`
}

// LoadSpec reads and parses a build spec file from disk. The input is bounded
// by defaults.MaxSpecFileBytes to prevent unbounded memory allocation from a
// corrupted or hostile file. YAML decoding rejects unknown fields to surface
// typos rather than silently dropping them.
func LoadSpec(ctx context.Context, path string) (*BuildSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled before reading spec", err)
	}

	f, err := os.Open(path)
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("spec file not found: %q", path), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to open spec file %q", path), err)
	}
	defer f.Close()

	// Read up to MaxSpecFileBytes+1 to detect oversize without loading more
	// than necessary. If the read returns exactly cap+1 bytes, the file is
	// over the limit.
	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxSpecFileBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read spec file %q", path), err)
	}
	if len(data) > defaults.MaxSpecFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("spec file exceeds maximum size of %d bytes: %q", defaults.MaxSpecFileBytes, path))
	}

	var spec BuildSpec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse spec file %q", path), err)
	}

	return &spec, nil
}

// Validate checks that required fields are present in the spec.
func (s *BuildSpec) Validate() error {
	if s.APIVersion != ExpectedAPIVersion {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("apiVersion must be %q, got %q", ExpectedAPIVersion, s.APIVersion))
	}

	if s.Kind != ExpectedKind {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("kind must be %q, got %q", ExpectedKind, s.Kind))
	}

	if s.Spec.Registry.Host == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "spec.registry.host is required")
	}

	if s.Spec.Registry.Repository == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "spec.registry.repository is required")
	}

	return nil
}

// WriteBack marshals the spec (including updated status) back to disk
// atomically. It writes to a sibling temp file, fsyncs, closes, and renames
// over the destination so a crash mid-write cannot leave the controller
// observing a truncated or partially-written spec.
func (s *BuildSpec) WriteBack(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeTimeout, "context cancelled before writing spec", err)
	}

	data, err := yaml.Marshal(s)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal spec", err)
	}

	// Use os.CreateTemp so concurrent WriteBack callers don't race on a
	// shared "<path>.tmp" name and clobber each other before either rename.
	// CreateTemp opens with mode 0o600, matching defaults.SpecFileMode.
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create temp spec file for %q", path), err)
	}
	tmp := f.Name()

	// On any error after this point, attempt to remove the temp file so we
	// don't leave debris next to the destination.
	var writeErr error
	if _, writeErr = f.Write(data); writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr == nil {
		writeErr = closeErr
	}

	if writeErr != nil {
		// tmp came from os.CreateTemp on the same dir we just wrote to;
		// removing it is the inverse of that creation, not a tainted-path op.
		if rmErr := os.Remove(tmp); rmErr != nil && !stderrors.Is(rmErr, fs.ErrNotExist) { //nolint:gosec // G703: tmp is from os.CreateTemp above
			slog.Warn("failed to remove temp spec file after write error",
				"path", tmp, "error", rmErr)
		}
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write spec file %q", path), writeErr)
	}

	if err := renameFn(tmp, path); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !stderrors.Is(rmErr, fs.ErrNotExist) { //nolint:gosec // G703: tmp is from os.CreateTemp above
			slog.Warn("failed to remove temp spec file after rename error",
				"path", tmp, "error", rmErr)
		}
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to rename temp spec file to %q", path), err)
	}

	// Sync the parent directory so the rename is persisted to disk. Without
	// this, a crash after the rename can lose the new dirent even though
	// the file's data was already fsynced. Best-effort: a failure here just
	// means the rename isn't yet durable on disk; the in-memory state is
	// already correct, so log a warning rather than reverting the write.
	if dirErr := syncDir(dir); dirErr != nil {
		slog.Warn("failed to fsync parent directory after spec rename",
			"dir", dir, "error", dirErr)
	}

	return nil
}

// syncDir opens the directory and calls Sync to persist directory metadata
// (e.g., the dirent created by Rename). On platforms where directories
// cannot be opened for sync (notably Windows), the call is a no-op.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // opening a directory for fsync is intentional
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to open directory for sync %q", dir), err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to sync directory %q", dir), syncErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to close directory %q", dir), closeErr)
	}
	return nil
}

// SetImageStatus sets the status for a named image (e.g., "charts", "apps", "app-of-apps").
func (s *BuildSpec) SetImageStatus(name string, status ImageStatus) {
	if s.Status.Images == nil {
		s.Status.Images = make(map[string]ImageStatus)
	}
	s.Status.Images[name] = status
}

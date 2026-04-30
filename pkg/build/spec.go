// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("spec file exceeds maximum size of %d bytes: %q", defaults.MaxSpecFileBytes, path),
			fmt.Errorf("read more than %d bytes", defaults.MaxSpecFileBytes))
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

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, defaults.SpecFileMode)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create temp spec file %q", tmp), err)
	}

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
		if rmErr := os.Remove(tmp); rmErr != nil && !stderrors.Is(rmErr, fs.ErrNotExist) {
			slog.Warn("failed to remove temp spec file after write error",
				"path", tmp, "error", rmErr)
		}
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write spec file %q", path), writeErr)
	}

	if err := os.Rename(tmp, path); err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !stderrors.Is(rmErr, fs.ErrNotExist) {
			slog.Warn("failed to remove temp spec file after rename error",
				"path", tmp, "error", rmErr)
		}
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to rename temp spec file to %q", path), err)
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

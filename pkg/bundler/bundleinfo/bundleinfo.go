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
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

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
func Write(ctx context.Context, dir string, info *BundleInfo) (int64, error) {
	if info == nil {
		return 0, errors.New(errors.ErrCodeInvalidRequest, "bundle info is required")
	}
	if err := ctx.Err(); err != nil {
		return 0, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
	}

	info.APIVersion = header.StableGroupVersion
	info.Kind = string(header.KindBundleInfo)

	if err := validateRelativePaths(info); err != nil {
		return 0, err
	}

	data, err := serializer.MarshalYAMLDeterministic(info)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to serialize bundle info")
	}

	path, joinErr := deployer.SafeJoin(dir, FileName)
	if joinErr != nil {
		return 0, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest, "unsafe bundle info path")
	}
	if err := os.WriteFile(path, data, 0600); err != nil { //nolint:gosec // path validated by SafeJoin
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to write bundle info", err)
	}

	slog.Debug("wrote bundle info", "path", path, "deployer", info.Build.Deployer)
	return int64(len(data)), nil
}

// Read loads and validates dir/bundle-info.yaml.
//
// Fails closed in every direction. A bundle produced before this artifact
// shipped has no file at all, and that returns ErrCodeNotFound naming the
// reason: a consumer must treat it as a real state and ask the operator for
// the deployer, never fall back to guessing one from the directory layout.
func Read(ctx context.Context, dir string) (*BundleInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
	}

	path, joinErr := deployer.SafeJoin(dir, FileName)
	if joinErr != nil {
		return nil, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest, "unsafe bundle info path")
	}

	f, err := os.Open(path) //nolint:gosec // path validated by SafeJoin
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("%s has no %s, so the deployer that built it was not recorded; "+
					"the bundle predates build-record stamping", dir, FileName))
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open bundle info", err)
	}
	defer func() { _ = f.Close() }()

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

	if info.Kind != string(header.KindBundleInfo) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s declares kind %q, want %q", FileName, info.Kind, header.KindBundleInfo))
	}
	if !header.IsSupportedAPIVersion(info.APIVersion) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s declares apiVersion %q, which this binary does not understand",
				FileName, info.APIVersion))
	}

	return &info, nil
}

// validateRelativePaths rejects info when any path it emits is absolute.
// bundle-info.yaml feeds downstream tooling that resolves these paths with
// filepath.Join(outDir, path); Join returns an absolute path argument
// unchanged, so an absolute value here would silently escape outDir instead
// of failing. This is the only place that can close that off, since every
// reader trusts the record once it parses.
func validateRelativePaths(info *BundleInfo) error {
	check := func(field, value string) error {
		if value != "" && filepath.IsAbs(value) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s must be a relative path, got %q", field, value))
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

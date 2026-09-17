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

	data, err := serializer.MarshalYAMLDeterministic(info)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to serialize bundle info")
	}

	path, joinErr := deployer.SafeJoin(dir, FileName)
	if joinErr != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "unsafe bundle info path", joinErr)
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
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "unsafe bundle info path", joinErr)
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

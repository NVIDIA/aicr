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

package snapshotter

import (
	"context"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// LoadFromFile reads a snapshot from path (local file, HTTP(S) URL, or cm://
// ConfigMap URI) and enforces apiVersion compatibility. It is equivalent to
// LoadFromFileWithKubeconfig with an empty kubeconfig.
func LoadFromFile(ctx context.Context, path string) (*Snapshot, error) {
	return LoadFromFileWithKubeconfig(ctx, path, "")
}

// LoadFromFileWithKubeconfig reads a snapshot from path using kubeconfig for
// cm:// resolution, then rejects a snapshot stamped with an apiVersion this
// build does not understand.
//
// An empty apiVersion is tolerated (older snapshots predate the field); a
// non-empty unknown value means the snapshot was produced by an incompatible
// aicr version, so we fail closed rather than risk a schema mismatch during
// validation.
func LoadFromFileWithKubeconfig(ctx context.Context, path, kubeconfig string) (*Snapshot, error) {
	snap, err := serializer.FromFileWithKubeconfigContext[Snapshot](ctx, path, kubeconfig)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("failed to load snapshot from %q", path))
	}

	if snap.APIVersion != "" && !header.IsSupportedAPIVersion(snap.APIVersion) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("snapshot file has apiVersion %q, which this aicr build does not support (expected %q); "+
				"recapture the snapshot with a matching aicr version",
				snap.APIVersion, header.GroupVersion))
	}

	return snap, nil
}

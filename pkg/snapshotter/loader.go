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
// cm:// resolution, then rejects a document that is not a snapshot this build
// can consume.
//
// Deserialization is non-strict, so any YAML mapping decodes into a Snapshot
// with zero-value fields. Without an identity gate a wrong file (typo'd path,
// an AICRConfig, arbitrary YAML) would decode into an empty Snapshot, derive
// criteria(any), and silently emit a fallback recipe with exit 0. We fail
// closed instead, in identity → version → content order:
//
//   - A non-empty kind other than Snapshot (e.g. AICRConfig) is the wrong
//     document type. An empty kind is tolerated because older snapshots predate
//     the field.
//   - A non-empty apiVersion this build does not understand means the snapshot
//     came from an incompatible aicr version, so we fail closed rather than
//     risk a schema mismatch during validation. An empty apiVersion is
//     tolerated for the same backward-compatibility reason.
//   - A document with no usable measurement — regardless of kind — cannot be
//     distinguished from empty cluster state and would still derive
//     criteria(any). A measurement is usable only if it is non-nil and carries
//     a type; fingerprinting ignores nil and typeless entries alike. This gate
//     backstops a correctly stamped but empty (kind: Snapshot, measurements: [])
//     file as well as slices of only nil (- null) or typeless (- {}) entries.
func LoadFromFileWithKubeconfig(ctx context.Context, path, kubeconfig string) (*Snapshot, error) {
	snap, err := serializer.FromFileWithKubeconfigContext[Snapshot](ctx, path, kubeconfig)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("failed to load snapshot from %q", path))
	}

	if snap.Kind != "" && snap.Kind != header.KindSnapshot {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file %q has kind %q, but a %q is required; "+
				"run \"aicr snapshot\" to capture cluster state first",
				path, snap.Kind, header.KindSnapshot))
	}

	if snap.APIVersion != "" && !header.IsSupportedAPIVersion(snap.APIVersion) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("snapshot file has apiVersion %q, which this aicr build does not support (expected %q or %q); "+
				"recapture the snapshot with a matching aicr version",
				snap.APIVersion, header.GroupVersion, header.GroupVersionV1))
	}

	usable := 0
	for _, m := range snap.Measurements {
		if m != nil && m.Type != "" {
			usable++
		}
	}
	if usable == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file %q contains no usable measurements and is not a valid snapshot; "+
				"run \"aicr snapshot\" to capture cluster state first", path))
	}

	return snap, nil
}

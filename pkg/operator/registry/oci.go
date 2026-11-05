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

package registry

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
)

// MirrorOCIChart copies a chart from an upstream OCI registry into
// target/<chart>. The upstream reference is the chart repository
// (oci://host/path/<chart>) and the version is its tag. The upstream
// manifest must be a Helm chart with exactly one chart layer; the layer is
// verified against its descriptor digest and recorded as the archive digest.
// A runtime-registry tag that already holds another digest for the same
// version is a conflict: the upstream content changed after it was mirrored.
func (c *Client) MirrorOCIChart(ctx context.Context, up UpstreamChart, target Repository) (*MirroredChart, error) {
	src, err := ParseRepository(up.Chart)
	if err != nil {
		return nil, invalid("upstream chart %q is not an OCI repository", up.Chart)
	}
	if up.Version == "" {
		return nil, invalid("upstream chart %s has no version", up.Chart)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(src)
	if err != nil {
		return nil, err
	}
	pulled, meta, err := pullChart(ctx, repo, src, up.Version)
	if err != nil {
		return nil, err
	}
	dst := target.Child(meta.Name)
	digest, err := c.pushChart(ctx, dst, pulled.Archive, meta, meta.Tag())
	if err != nil {
		return nil, err
	}
	return &MirroredChart{Repository: dst, Digest: digest, ArchiveDigest: pulled.ArchiveDigest, Archive: pulled.Archive}, nil
}

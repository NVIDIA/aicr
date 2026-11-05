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
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// maxManifestBytes bounds an OCI manifest. Real manifests are a few KiB.
const maxManifestBytes = 4 << 20

// fetchManifest returns the decoded manifest m that ref, a tag or digest,
// names in r, and its descriptor desc. It reads at most maxBytes and verifies
// the manifest against its digest.
func fetchManifest(ctx context.Context, repo *remote.Repository, r Repository, ref string, maxBytes int64) (desc ocispec.Descriptor, m ocispec.Manifest, err error) {
	at := r.String() + ":" + ref
	if digestPattern.MatchString(ref) {
		at = r.String() + "@" + ref
	}
	desc, data, err := oras.FetchBytes(ctx, repo, ref, oras.FetchBytesOptions{MaxBytes: maxBytes})
	if err != nil {
		return desc, ocispec.Manifest{}, classify(err, "fetch manifest "+at)
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return desc, ocispec.Manifest{}, invalid("%s is not an OCI manifest: %v", at, err)
	}
	return desc, m, nil
}

// blob is content a manifest references.
type blob struct {
	desc ocispec.Descriptor
	data []byte
}

// pushManifest pushes the blobs r lacks, then manifest tagged tag unless the
// tag already holds it, and returns the manifest digest. A tag holding
// another manifest is CONFLICT, and is not overwritten.
func pushManifest(ctx context.Context, repo *remote.Repository, r Repository, tag string, manifest []byte, blobs ...blob) (string, error) {
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	want := desc.Digest.String()
	tagged := false
	switch existing, err := repo.Resolve(ctx, tag); {
	case err == nil && existing.Digest.String() == want:
		tagged = true
	case err == nil:
		return "", operrors.New(operrors.ErrCodeConflict, fmt.Sprintf("%s:%s already holds %s, not %s", r, tag, existing.Digest, want))
	case operrors.CodeOf(classify(err, "")) != operrors.ErrCodeNotFound:
		return "", classify(err, fmt.Sprintf("resolve %s:%s", r, tag))
	}
	for _, b := range blobs {
		exists, err := repo.Exists(ctx, b.desc)
		if err != nil {
			return "", classify(err, fmt.Sprintf("check blob %s in %s", b.desc.Digest, r))
		}
		if exists {
			continue
		}
		if err = repo.Push(ctx, b.desc, bytes.NewReader(b.data)); err != nil {
			return "", classify(err, fmt.Sprintf("push blob %s to %s", b.desc.Digest, r))
		}
	}
	if tagged {
		return want, nil
	}
	if err := repo.PushReference(ctx, desc, bytes.NewReader(manifest), tag); err != nil {
		return "", classify(err, fmt.Sprintf("push manifest %s:%s", r, tag))
	}
	return want, nil
}

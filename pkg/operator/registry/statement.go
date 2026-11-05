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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// StatementMediaType is the layer media type of an in-toto statement.
const StatementMediaType = "application/vnd.in-toto+json"

// maxStatementBytes bounds a statement read back from the registry.
const maxStatementBytes = 1 << 20

// SigstoreBundleArtifactType is the artifact type of the Sigstore bundle AICR
// attaches to its evidence as a referrer when it signs with a key.
const SigstoreBundleArtifactType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// sigstoreBundlePrefix matches every Sigstore bundle version.
const sigstoreBundlePrefix = "application/vnd.dev.sigstore.bundle"

// maxReferrers bounds the referrers read for one subject: AICR's signature
// and the operator's binding, with room for re-runs.
const maxReferrers = 16

// maxReferrerPages bounds the referrer pages read for one subject. A page
// without referrers never reaches the maxReferrers check.
const maxReferrerPages = maxReferrers

// SigstoreBundles returns the Sigstore bundles attached to the manifest with
// digest subject as referrers: the signatures AICR attaches to its evidence,
// which the evidence bundle itself never carries. Other referrers, such as
// the operator's binding, are not signatures. Each referrer is judged by its
// own manifest, since registries differ in the artifact type they list, and
// each bundle is read bounded and verified against its digest.
func (c *Client) SigstoreBundles(ctx context.Context, r Repository, subject string) ([][]byte, error) {
	if !digestPattern.MatchString(subject) {
		return nil, invalid("signature subject %q is not a sha256 digest", subject)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return nil, err
	}
	repo.ReferrerListMaxPages = maxReferrerPages
	desc, err := repo.Resolve(ctx, subject)
	if err != nil {
		return nil, classify(err, fmt.Sprintf("resolve %s@%s", r, subject))
	}
	var referrers []ocispec.Descriptor
	var tooMany error
	err = repo.Referrers(ctx, desc, "", func(page []ocispec.Descriptor) error {
		referrers = append(referrers, page...)
		if len(referrers) > maxReferrers {
			// Stop paging: a registry that keeps returning pages would grow
			// this until the timeout.
			tooMany = invalid("%s@%s has more than %d referrers", r, subject, maxReferrers)
			return tooMany
		}
		return nil
	})
	if tooMany != nil {
		return nil, tooMany
	}
	if err != nil {
		return nil, classify(err, fmt.Sprintf("list referrers of %s@%s", r, subject))
	}
	var out [][]byte
	for _, ref := range referrers {
		_, m, ferr := fetchManifest(ctx, repo, r, ref.Digest.String(), maxManifestBytes)
		if ferr != nil {
			return nil, ferr
		}
		if !strings.HasPrefix(m.ArtifactType, sigstoreBundlePrefix) {
			continue
		}
		if len(m.Layers) != 1 || !strings.HasPrefix(m.Layers[0].MediaType, sigstoreBundlePrefix) {
			return nil, invalid("Sigstore bundle artifact %s of %s@%s must have exactly one bundle layer", ref.Digest, r, subject)
		}
		if m.Layers[0].Size > maxStatementBytes {
			return nil, invalid("Sigstore bundle %s is %d bytes", m.Layers[0].Digest, m.Layers[0].Size)
		}
		bundle, ferr := content.FetchAll(ctx, repo, m.Layers[0])
		if ferr != nil {
			return nil, classify(ferr, "fetch Sigstore bundle "+m.Layers[0].Digest.String())
		}
		out = append(out, bundle)
	}
	return out, nil
}

// Describe returns the descriptor of the manifest with digest dgst.
func (c *Client) Describe(ctx context.Context, r Repository, dgst string) (ocispec.Descriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	desc, err := repo.Resolve(ctx, dgst)
	if err != nil {
		return ocispec.Descriptor{}, classify(err, fmt.Sprintf("resolve %s@%s", r, dgst))
	}
	return desc, nil
}

// PushStatement pushes an in-toto statement as an OCI artifact of
// artifactType whose subject is the manifest it describes, so a registry
// with the referrers API lists it with its subject. The manifest carries no
// timestamps: the same statement pushes to the same digest, and an existing
// tag holding other content is a conflict.
func (c *Client) PushStatement(ctx context.Context, r Repository, tag, artifactType string, statement []byte,
	subject ocispec.Descriptor) (string, error) {

	layer := ocispec.Descriptor{MediaType: StatementMediaType, Digest: digest.FromBytes(statement), Size: int64(len(statement))}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{layer},
		Subject:      &subject,
	})
	if err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInternal, "encode statement manifest", err)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return "", err
	}
	return pushManifest(ctx, repo, r, tag, manifest, blob{ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data}, blob{layer, statement})
}

// PullStatement reads the statement an artifact pushed by PushStatement
// carries, verifying it against the manifest's layer digest.
func (c *Client) PullStatement(ctx context.Context, r Repository, dgst string) ([]byte, ocispec.Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return nil, ocispec.Manifest{}, err
	}
	_, m, err := fetchManifest(ctx, repo, r, dgst, maxStatementBytes)
	if err != nil {
		return nil, ocispec.Manifest{}, err
	}
	switch {
	case len(m.Layers) != 1 || m.Layers[0].MediaType != StatementMediaType:
		return nil, ocispec.Manifest{}, invalid("%s@%s is not a statement artifact", r, dgst)
	case m.Layers[0].Size > maxStatementBytes:
		return nil, ocispec.Manifest{}, invalid("statement of %s@%s is %d bytes", r, dgst, m.Layers[0].Size)
	}
	data, err := content.FetchAll(ctx, repo, m.Layers[0])
	if err != nil {
		return nil, ocispec.Manifest{}, classify(err, fmt.Sprintf("fetch statement of %s@%s", r, dgst))
	}
	return data, m, nil
}

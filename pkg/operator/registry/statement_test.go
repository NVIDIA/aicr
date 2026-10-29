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
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// pushSigstoreReferrer attaches a Sigstore bundle to subject the way AICR
// does: an artifact of the bundle's media type whose single layer is the
// bundle, with the evidence as its subject.
func pushSigstoreReferrer(t *testing.T, c *Client, r Repository, bundle []byte, subject ocispec.Descriptor) {
	t.Helper()
	ctx := context.Background()
	repo, err := c.repository(r)
	if err != nil {
		t.Fatal(err)
	}
	layer := ocispec.Descriptor{MediaType: SigstoreBundleArtifactType, Digest: digest.FromBytes(bundle), Size: int64(len(bundle))}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: SigstoreBundleArtifactType, Config: ocispec.DescriptorEmptyJSON,
		Layers: []ocispec.Descriptor{layer}, Subject: &subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct {
		desc ocispec.Descriptor
		data []byte
	}{{ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data}, {layer, bundle}} {
		if err = repo.Push(ctx, b.desc, bytes.NewReader(b.data)); err != nil {
			t.Fatal(err)
		}
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	if err = repo.Push(ctx, desc, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
}

// AICR attaches its evidence signature as a Sigstore bundle referrer of the
// evidence, never inside it. SigstoreBundles reads exactly those, from
// registries with and without the referrers API; another referrer, such as
// the operator's binding, is not a signature.
func TestSigstoreBundles(t *testing.T) {
	t.Parallel()
	for _, api := range []bool{false, true} {
		t.Run(fmt.Sprintf("referrers API %t", api), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			c, repo := testRegistryWith(t, api)
			evidence := repo.Child("evidence")
			subjectDigest, err := c.PushChart(ctx, evidence, chartArchive(t, "evidence", "1.0.0", ""))
			if err != nil {
				t.Fatal(err)
			}
			subject, err := c.Describe(ctx, evidence, subjectDigest)
			if err != nil {
				t.Fatal(err)
			}
			if got, gotErr := c.SigstoreBundles(ctx, evidence, subjectDigest); gotErr != nil || len(got) != 0 {
				t.Fatalf("unsigned evidence: SigstoreBundles() = %d, %v", len(got), gotErr)
			}
			signature := []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{}}`)
			pushSigstoreReferrer(t, c, evidence, signature, subject)
			if _, err = c.PushStatement(ctx, evidence, "run-1-binding", "application/vnd.test.binding+json", []byte(`{}`), subject); err != nil {
				t.Fatal(err)
			}
			got, err := c.SigstoreBundles(ctx, evidence, subjectDigest)
			if err != nil || len(got) != 1 || !bytes.Equal(got[0], signature) {
				t.Fatalf("SigstoreBundles() = %q, %v; want the one signature", got, err)
			}
			if _, err = c.SigstoreBundles(ctx, evidence, "latest"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("a tag as the subject: %v", err)
			}
		})
	}
}

// A statement pushes as an artifact whose subject is the manifest it
// describes, deterministically; it reads back verified, and a tag holding
// other content is never overwritten.
func TestPushStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c, repo := testRegistry(t)
	evidence := repo.Child("evidence")
	subjectDigest, err := c.PushChart(ctx, evidence, chartArchive(t, "evidence", "1.0.0", ""))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := c.Describe(ctx, evidence, subjectDigest)
	if err != nil || subject.Digest.String() != subjectDigest || subject.Size == 0 {
		t.Fatalf("Describe() = %+v, %v", subject, err)
	}
	statement := []byte(`{"_type":"https://in-toto.io/Statement/v1"}`)
	first, err := c.PushStatement(ctx, evidence, "run-1-binding", "application/vnd.test+json", statement, subject)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.PushStatement(ctx, evidence, "run-1-binding", "application/vnd.test+json", statement, subject)
	if err != nil || again != first {
		t.Fatalf("a repeated push = %s, %v; want %s", again, err, first)
	}
	data, manifest, err := c.PullStatement(ctx, evidence, first)
	if err != nil || string(data) != string(statement) || manifest.Subject == nil || manifest.Subject.Digest != subject.Digest ||
		manifest.ArtifactType != "application/vnd.test+json" {

		t.Fatalf("PullStatement() = %s %+v, %v", data, manifest, err)
	}
	if _, err = c.PushStatement(ctx, evidence, "run-1-binding", "application/vnd.test+json", []byte(`{}`), subject); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("overwriting a tag: %v", err)
	}
	if _, _, err = c.PullStatement(ctx, evidence, subjectDigest); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("pulling a chart as a statement: %v", err)
	}
}

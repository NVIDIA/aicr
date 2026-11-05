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

package registrytest

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

func repository(t *testing.T, host, name string) *remote.Repository {
	t.Helper()
	repo, err := remote.NewRepository(host + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	return repo
}

func push(t *testing.T, repo *remote.Repository, data []byte) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := repo.Push(t.Context(), desc, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return desc
}

// registries are the registries this package serves, which must behave alike.
var registries = map[string]func() http.Handler{
	"Distribution":       New,
	"with referrers API": NewWithReferrersAPI,
}

// forEachRegistry runs test against each of registries, served over plain
// HTTP at host.
func forEachRegistry(t *testing.T, test func(t *testing.T, host string)) {
	t.Helper()
	for name, newRegistry := range registries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(newRegistry())
			t.Cleanup(srv.Close)
			test(t, strings.TrimPrefix(srv.URL, "http://"))
		})
	}
}

// A blob pushed to one repository is unknown to every other.
func TestBlobsStayInTheirRepository(t *testing.T) {
	t.Parallel()
	forEachRegistry(t, testBlobsStayInTheirRepository)
}

func testBlobsStayInTheirRepository(t *testing.T, host string) {
	ctx := t.Context()
	a, b := repository(t, host, "a"), repository(t, host, "b")
	desc := push(t, a, []byte("layer"))

	if ok, err := a.Exists(ctx, desc); err != nil || !ok {
		t.Fatalf("a.Exists() = %v, %v; want the blob pushed to a", ok, err)
	}
	if data, err := content.FetchAll(ctx, a, desc); err != nil || string(data) != "layer" {
		t.Fatalf("a.Fetch() = %q, %v; want the blob pushed to a", data, err)
	}
	if ok, err := b.Exists(ctx, desc); err != nil || ok {
		t.Fatalf("b.Exists() = %v, %v; want a's blob unknown to b", ok, err)
	}
	if _, err := b.Fetch(ctx, desc); !errors.Is(err, errdef.ErrNotFound) {
		t.Fatalf("b.Fetch() = %v, want not found", err)
	}
	if err := a.Blobs().Delete(ctx, desc); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Exists(ctx, desc); err != nil || ok {
		t.Fatalf("a.Exists() after Delete = %v, %v", ok, err)
	}
}

// An image manifest is stored only once its repository holds its config and
// every layer.
func TestManifestNeedsItsBlobs(t *testing.T) {
	t.Parallel()
	forEachRegistry(t, testManifestNeedsItsBlobs)
}

func testManifestNeedsItsBlobs(t *testing.T, host string) {
	ctx := t.Context()
	a, b := repository(t, host, "a"), repository(t, host, "b")
	config := push(t, a, []byte("{}"))
	layer := push(t, a, []byte("layer"))
	push(t, b, []byte("{}"))
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: config, Layers: []ocispec.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}

	err = b.PushReference(ctx, desc, bytes.NewReader(manifest), "v1")
	var resp *errcode.ErrorResponse
	if !errors.As(err, &resp) || resp.StatusCode != http.StatusBadRequest || len(resp.Errors) != 1 || resp.Errors[0].Code != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("pushing to b, which lacks the layer = %v, want MANIFEST_BLOB_UNKNOWN", err)
	}
	if err = a.PushReference(ctx, desc, bytes.NewReader(manifest), "v1"); err != nil {
		t.Fatalf("pushing to a, which holds every blob = %v", err)
	}
}

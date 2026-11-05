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
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// NewWithReferrersAPI returns an in-memory registry that also serves the OCI
// 1.1 referrers API, which Distribution lacks. Like New, it keeps each
// repository's blobs apart and refuses an image manifest whose config or
// layers the repository does not hold.
func NewWithReferrersAPI() http.Handler {
	// go-containerregistry's registry shares blobs across repositories and
	// stores a manifest whatever blobs it names. blobs and the manifest check
	// below make it behave as Distribution does.
	blobs := &blobStore{blobs: map[string][]byte{}}
	next := ggcrregistry.New(
		ggcrregistry.Logger(log.New(io.Discard, "", 0)),
		ggcrregistry.WithBlobHandler(blobs),
		ggcrregistry.WithReferrersSupport(true),
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if repo, ok := repoName(r.URL.Path, "/blobs/"); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), repoKey{}, repo)))
			return
		}
		repo, ok := manifestPut(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		var m ocispec.Manifest
		if err = json.Unmarshal(body, &m); err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
			if len(d.URLs) == 0 && !blobs.has(repo, d.Digest.String()) {
				writeError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "blob unknown to registry: "+d.Digest.String())
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// manifestPut returns the repository r writes to, and ok reports whether r is
// an image manifest PUT.
func manifestPut(r *http.Request) (repo string, ok bool) {
	if r.Method != http.MethodPut || !types.MediaType(r.Header.Get("Content-Type")).IsImage() {
		return "", false
	}
	return repoName(r.URL.Path, "/manifests/")
}

// repoName returns the repository name in a /v2/<name><route>... urlPath, and
// ok reports whether urlPath has that form.
func repoName(urlPath, route string) (name string, ok bool) {
	name, ok = strings.CutPrefix(urlPath, "/v2/")
	// A digest, tag or upload ID never holds route, so the last match ends
	// the name.
	i := strings.LastIndex(name, route)
	if !ok || i <= 0 {
		return "", false
	}
	return name[:i], true
}

type repoKey struct{}

// repoOf returns the repository NewWithReferrersAPI read from the request
// path into ctx.
func repoOf(ctx context.Context) string {
	repo, _ := ctx.Value(repoKey{}).(string)
	return repo
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

// blobStore holds blobs by repository and digest. Its methods take the
// repository from ctx, since go-containerregistry passes "<name>/blobs" when
// an upload finishes.
type blobStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func key(repo, dgst string) string { return repo + "@" + dgst }

func (s *blobStore) has(repo, dgst string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[key(repo, dgst)]
	return ok
}

func (s *blobStore) Stat(ctx context.Context, _ string, h v1.Hash) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[key(repoOf(ctx), h.String())]
	if !ok {
		return 0, ggcrregistry.ErrNotFound
	}
	return int64(len(b)), nil
}

func (s *blobStore) Get(ctx context.Context, _ string, h v1.Hash) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[key(repoOf(ctx), h.String())]
	if !ok {
		return nil, ggcrregistry.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Put stores the blob once rc verifies it, and returns rc's error otherwise.
func (s *blobStore) Put(ctx context.Context, _ string, h v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[key(repoOf(ctx), h.String())] = b
	return nil
}

func (s *blobStore) Delete(ctx context.Context, _ string, h v1.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(repoOf(ctx), h.String())
	if _, ok := s.blobs[k]; !ok {
		return ggcrregistry.ErrNotFound
	}
	delete(s.blobs, k)
	return nil
}

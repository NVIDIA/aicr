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

package oci

import (
	"encoding/base64"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestRemoteRecipeArtifactRepositoryUsesORASDockerCredentials(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	manifestDigest := digest.FromBytes(manifest)
	username := "registry-user"
	password := "registry-secret"
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(username+":"+password))

	var authenticated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != wantAuthorization {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Store(true)
		w.Header().Set("Content-Type", ociv1.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", manifestDigest.String())
		w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
		if request.Method != http.MethodHead {
			_, _ = w.Write(manifest)
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)
	authValue := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	writeTestFile(t, filepath.Join(configDir, "config.json"), []byte(
		fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, authValue)))

	repository, err := newRemoteRecipeArtifactRepository(t.Context(), host+"/recipes")
	if err != nil {
		t.Fatalf("newRemoteRecipeArtifactRepository() error = %v", err)
	}
	defer repository.CloseIdleConnections()
	remoteRepository := repository.(*remoteRecipeArtifactRepository)
	remoteRepository.repository.PlainHTTP = true
	descriptor, err := repository.Resolve(t.Context(), "v1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if descriptor.Digest != manifestDigest {
		t.Errorf("resolved digest = %s, want %s", descriptor.Digest, manifestDigest)
	}
	if !authenticated.Load() {
		t.Error("registry request never used the Docker credential")
	}
}

func TestRemoteRecipeArtifactRepositoryMissingDockerConfigUsesAnonymousAuth(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	repository, err := newRemoteRecipeArtifactRepository(
		t.Context(), "registry.example.test/aicr/recipes")
	if err != nil {
		t.Fatalf("newRemoteRecipeArtifactRepository() error = %v", err)
	}
	defer repository.CloseIdleConnections()

	remoteRepository := repository.(*remoteRecipeArtifactRepository)
	authClient := remoteRepository.repository.Client.(*auth.Client)
	credential, err := authClient.Credential(t.Context(), "registry.example.test")
	if err != nil {
		t.Fatalf("Credential() error = %v", err)
	}
	if credential != auth.EmptyCredential {
		t.Errorf("Credential() = %#v, want empty credential", credential)
	}
}

func TestRemoteRecipeArtifactRepositoryRejectsMalformedDockerConfig(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)
	writeTestFile(t, filepath.Join(configDir, "config.json"), []byte(`{"auths":`))

	_, err := newRemoteRecipeArtifactRepository(
		t.Context(), "registry.example.test/aicr/recipes")
	if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("newRemoteRecipeArtifactRepository() error = %v, want ErrCodeInvalidRequest", err)
	}
}

func TestRemoteRecipeArtifactRepositoryRejectsMalformedRegistryCredential(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)
	writeTestFile(t, filepath.Join(configDir, "config.json"), []byte(
		`{"auths":{"registry.example.test":{"auth":"not-base64"}}}`))

	repository, err := newRemoteRecipeArtifactRepository(
		t.Context(), "registry.example.test/aicr/recipes")
	if err != nil {
		t.Fatalf("newRemoteRecipeArtifactRepository() error = %v", err)
	}
	defer repository.CloseIdleConnections()

	remoteRepository := repository.(*remoteRecipeArtifactRepository)
	authClient := remoteRepository.repository.Client.(*auth.Client)
	_, err = authClient.Credential(t.Context(), "registry.example.test")
	if !stderrors.Is(err, apperrors.New(apperrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("Credential() error = %v, want ErrCodeInvalidRequest", err)
	}
}

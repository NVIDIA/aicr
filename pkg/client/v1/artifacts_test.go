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

package aicr_test

import (
	"path/filepath"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

func TestClientArtifactInputs(t *testing.T) {
	tests := []struct {
		name    string
		options []aicr.Option
	}{
		{"missing explicit inventory", []aicr.Option{aicr.WithRecipeSource(aicr.EmbeddedSource()), aicr.WithArtifacts(aicr.ArtifactOptions{Inventory: filepath.Join(t.TempDir(), "missing.yaml")})}},
		{"OCI options", []aicr.Option{aicr.WithRecipeSource(aicr.OCISource("example.test/catalog", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")), aicr.WithArtifacts(aicr.ArtifactOptions{})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := aicr.NewClientContext(t.Context(), tt.options...)
			if client != nil {
				_ = client.Close()
				t.Fatal("unexpected client")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
	dir := providertest.WriteDir(t, map[string][]byte{"registry.yaml": []byte(providertest.EmptyRegistryYAML), "artifacts/bindings.yaml": []byte("malformed")})
	client, err := aicr.NewClientContext(t.Context(), aicr.WithRecipeSource(aicr.FilesystemSource(dir)))
	if err != nil {
		t.Fatalf("inactive bindings affected construction: %v", err)
	}
	if err := client.LoadCatalog(t.Context()); err != nil {
		t.Fatalf("inactive bindings affected catalog scan: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

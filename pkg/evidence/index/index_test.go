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

package index

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/evidence/allowlist"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestPointersYAMLMatchesTree(t *testing.T) {
	fromTree, err := buildFromTree(evidenceRoot(t))
	if err != nil {
		t.Fatalf("build index from recipes/evidence: %v", err)
	}
	fromEmbed, err := decodeDocument(pointersYAML)
	if err != nil {
		t.Fatalf("decode embedded index: %v", err)
	}
	if reflect.DeepEqual(fromTree, fromEmbed) {
		if len(fromTree.Recipes) == 0 {
			t.Fatal("evidence tree produced an empty index")
		}
		return
	}
	if os.Getenv("AICR_UPDATE_EVIDENCE_INDEX") == "1" {
		body, marshalErr := marshalIndex(fromTree)
		if marshalErr != nil {
			t.Fatalf("marshal index: %v", marshalErr)
		}
		if writeErr := os.WriteFile("pointers.yaml", body, 0o600); writeErr != nil {
			t.Fatalf("write pointers.yaml: %v", writeErr)
		}
		t.Fatal("rewrote pointers.yaml from recipes/evidence; run the test again so the embed picks it up")
	}
	t.Fatalf("embedded evidence index does not match recipes/evidence\n"+
		"regenerate with AICR_UPDATE_EVIDENCE_INDEX=1 go test ./pkg/evidence/index/ -run TestPointersYAMLMatchesTree -count=1\n"+
		"tree has %d recipes, embed has %d", len(fromTree.Recipes), len(fromEmbed.Recipes))
}

func TestCheck(t *testing.T) {
	evidenced := &recipe.RecipeResult{Criteria: &recipe.Criteria{
		Accelerator: "gb200", Service: "eks", OS: "ubuntu", Intent: "training",
	}}
	unattested := &recipe.RecipeResult{Criteria: &recipe.Criteria{
		Accelerator: "h100", Service: "eks", OS: "ubuntu", Intent: "training",
	}}
	profiled := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{
			Accelerator: "h100", Service: "aks", OS: "ubuntu", Intent: "training",
		},
		Metadata: recipe.RecipeResultMetadata{SelectedProfile: &recipe.SelectedProfile{
			Name: "gpuStack", Value: "azure-managed",
		}},
	}

	tests := []struct {
		name       string
		result     *recipe.RecipeResult
		leafSource string
		wantCode   errors.ErrorCode
		wantText   string
	}{
		{
			name:       "embedded recipe with a pointer",
			result:     evidenced,
			leafSource: recipe.CatalogSourceEmbedded,
		},
		{
			name:       "embedded recipe without a pointer",
			result:     unattested,
			leafSource: recipe.CatalogSourceEmbedded,
			wantCode:   errors.ErrCodeNotFound,
			wantText:   `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name:       "profile suffix is not covered by the base pointer",
			result:     profiled,
			leafSource: recipe.CatalogSourceEmbedded,
			wantCode:   errors.ErrCodeNotFound,
			wantText:   `recipe "h100-aks-ubuntu-training-gpustack-azure-managed" has no published evidence`,
		},
		{
			name:       "external leaf fails even when the id has a pointer",
			result:     evidenced,
			leafSource: recipe.CatalogSourceExternal,
			wantCode:   errors.ErrCodeNotFound,
			wantText:   "resolved leaf is not the embedded catalog recipe",
		},
		{
			name:       "unknown leaf source fails closed",
			result:     evidenced,
			leafSource: "",
			wantCode:   errors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.result, tt.leafSource)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				return
			}
			if errorstest.ReportedCode(err) != tt.wantCode {
				t.Fatalf("Check code = %v, want %s (%v)", errorstest.ReportedCode(err), tt.wantCode, err)
			}
			if !strings.Contains(err.Error(), HealthDocURL) {
				t.Fatalf("Check error = %v, want the recipe-health link", err)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("Check error = %v, want text %q", err, tt.wantText)
			}
		})
	}
}

func TestCheckNilRecipe(t *testing.T) {
	err := Check(nil, recipe.CatalogSourceEmbedded)
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Fatalf("Check(nil) = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}

func TestParseToggleAndEnv(t *testing.T) {
	t.Setenv(EnvVar, "")
	if EnabledFromEnv() {
		t.Fatal("empty AICR_REQUIRE_EVIDENCE enabled the check")
	}
	for _, raw := range []string{"1", "true", "YES", "on"} {
		t.Setenv(EnvVar, raw)
		if !EnabledFromEnv() {
			t.Fatalf("AICR_REQUIRE_EVIDENCE=%q did not enable the check", raw)
		}
	}
	for _, raw := range []string{"0", "false", "no", "off", "maybe"} {
		t.Setenv(EnvVar, raw)
		if EnabledFromEnv() {
			t.Fatalf("AICR_REQUIRE_EVIDENCE=%q enabled the check", raw)
		}
	}
	if _, ok := ParseToggle("maybe"); ok {
		t.Fatal("ParseToggle accepted an unrecognized value")
	}
}

func TestLeafSource(t *testing.T) {
	entries := []CatalogSource{{Name: "base-overlay", Source: recipe.CatalogSourceEmbedded}, {Name: "leaf", Source: recipe.CatalogSourceExternal}}
	if got := LeafSource([]string{"base-overlay", "leaf"}, entries); got != recipe.CatalogSourceExternal {
		t.Fatalf("LeafSource = %q, want the last overlay", got)
	}
	if got := LeafSource([]string{"missing"}, entries); got != "" {
		t.Fatalf("LeafSource(missing) = %q, want empty", got)
	}
	if got := LeafSource(nil, entries); got != "" {
		t.Fatalf("LeafSource(nil) = %q, want empty", got)
	}
}

func TestEmbeddedClassesAreAllowlisted(t *testing.T) {
	doc, err := decodeDocument(pointersYAML)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, recipeEntry := range doc.Recipes {
		for _, source := range recipeEntry.Sources {
			switch source.Class {
			case string(allowlist.ClassCommunity), string(allowlist.ClassPartner):
			default:
				t.Errorf("recipe %s source %s class = %q, want community or partner", recipeEntry.ID, source.Slug, source.Class)
			}
			if source.Class == string(allowlist.ClassFirstParty) {
				t.Errorf("recipe %s records a first-party pointer; first-party evidence has no per-run pointer", recipeEntry.ID)
			}
		}
	}
}

func evidenceRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "recipes", "evidence")
}

func buildFromTree(root string) (indexDocument, error) {
	al, err := allowlist.Load(filepath.Join(root, allowlistFile))
	if err != nil {
		return indexDocument{}, err
	}
	classes := map[string]string{}
	for _, entry := range al.Community {
		if entry.Source != "" {
			classes[entry.Source] = string(allowlist.ClassCommunity)
		}
	}
	for _, entry := range al.Partner {
		if entry.Source != "" {
			classes[entry.Source] = string(allowlist.ClassPartner)
		}
	}

	recipeDirs, err := os.ReadDir(root)
	if err != nil {
		return indexDocument{}, err
	}
	doc := indexDocument{SchemaVersion: 1}
	for _, recipeDir := range recipeDirs {
		if !recipeDir.IsDir() {
			if recipeDir.Name() == allowlistFile {
				continue
			}
			return indexDocument{}, errors.New(errors.ErrCodeInvalidRequest,
				"unexpected file at evidence root: "+recipeDir.Name())
		}
		sources, err := readSourcePointers(root, recipeDir.Name(), classes)
		if err != nil {
			return indexDocument{}, err
		}
		if len(sources) == 0 {
			continue
		}
		doc.Recipes = append(doc.Recipes, recipePointers{ID: recipeDir.Name(), Sources: sources})
	}
	sort.Slice(doc.Recipes, func(i, j int) bool { return doc.Recipes[i].ID < doc.Recipes[j].ID })
	return doc, nil
}

func readSourcePointers(root, recipeID string, classes map[string]string) ([]sourcePointer, error) {
	sourceDirs, err := os.ReadDir(filepath.Join(root, recipeID))
	if err != nil {
		return nil, err
	}
	var sources []sourcePointer
	for _, sourceDir := range sourceDirs {
		if !sourceDir.IsDir() {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"unexpected file under "+recipeID+": "+sourceDir.Name())
		}
		matches, err := filepath.Glob(filepath.Join(root, recipeID, sourceDir.Name(), "*.yaml"))
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				recipeID+"/"+sourceDir.Name()+" has no pointer file")
		}
		class, ok := classes[sourceDir.Name()]
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"source "+sourceDir.Name()+" under "+recipeID+" is not in the signer allowlist")
		}
		sources = append(sources, sourcePointer{Slug: sourceDir.Name(), Class: class})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Slug < sources[j].Slug })
	return sources, nil
}

func decodeDocument(raw []byte) (indexDocument, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc indexDocument
	if err := dec.Decode(&doc); err != nil {
		return indexDocument{}, err
	}
	return doc, nil
}

func marshalIndex(doc indexDocument) ([]byte, error) {
	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(indexHeader)
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

const allowlistFile = "allowlist.yaml"

const indexHeader = `# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Generated from recipes/evidence. Do not edit. Regenerate with
# AICR_UPDATE_EVIDENCE_INDEX=1 go test ./pkg/evidence/index/ -run TestPointersYAMLMatchesTree -count=1
`

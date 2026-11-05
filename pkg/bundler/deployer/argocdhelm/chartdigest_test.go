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

package argocdhelm

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/deployertest"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat/localformattest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestGenerate_VendorChartsVerifiesRegistryDigest proves this deployer hands
// the registry's helm.defaultDigest to the vendor step.
func TestGenerate_VendorChartsVerifiesRegistryDigest(t *testing.T) {
	archive := localformattest.StubArchive(localformat.Component{
		ChartName: deployertest.DigestedComponent, Version: deployertest.DigestChartVersion,
	})
	deployertest.RunVendorDigestCases(t, archive, func(t *testing.T, rr *recipe.RecipeResult) error {
		g := &Generator{
			RecipeResult:    rr,
			ComponentValues: map[string]map[string]any{deployertest.DigestedComponent: {}},
			Version:         "v0.0.0-test",
			RepoURL:         "https://github.com/example/gitops.git",
			VendorCharts:    true,
			Puller:          localformattest.StubPuller{RecordSHA256: deployertest.UnhashedRecordSHA256},
		}
		_, err := g.Generate(context.Background(), t.TempDir())
		return err
	})
}

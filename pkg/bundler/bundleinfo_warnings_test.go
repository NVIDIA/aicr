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

package bundler

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

// TestBundleDoesNotWriteWarningsYet holds the writer side of the readers-first
// rollout for bundleinfo.BundleInfo.Warnings (#3223): the strict reader in a
// release that predates the field rejects a record carrying it, so no bundle
// may write it until a release that reads it has shipped. The argocd run has
// no repository URL, so it does report a note, which must stay on the console.
func TestBundleDoesNotWriteWarningsYet(t *testing.T) {
	tests := []struct {
		name     string
		deployer config.DeployerType
		repoURL  string
		wantNote bool
	}{
		{name: "helm", deployer: config.DeployerHelm},
		{name: "argocd without a repository URL", deployer: config.DeployerArgoCD, wantNote: true},
		{name: "argocd-helm", deployer: config.DeployerArgoCDHelm, repoURL: "https://github.com/example/bundles.git"},
		{name: "flux", deployer: config.DeployerFlux, repoURL: "https://github.com/example/bundles.git"},
		{name: "helmfile", deployer: config.DeployerHelmfile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			b, err := New(WithConfig(config.NewConfig(
				config.WithDeployer(tt.deployer),
				config.WithRepoURL(tt.repoURL),
				config.WithIncludeChecksums(false),
			)))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			out, err := b.Make(context.Background(), closedWorldRecipeResult(), dir)
			if err != nil {
				t.Fatalf("Make: %v", err)
			}
			if tt.wantNote && len(out.Deployment.Notes) == 0 {
				t.Fatal("the run reported no note; this case proves nothing about notes")
			}

			raw, err := os.ReadFile(filepath.Join(dir, bundleinfo.FileName))
			if err != nil {
				t.Fatalf("read %s: %v", bundleinfo.FileName, err)
			}
			if bytes.Contains(raw, []byte("warnings:")) {
				t.Errorf("%s carries warnings, which a release that predates the field cannot read; "+
					"write them only after a tagged release that reads them (#3223):\n%s",
					bundleinfo.FileName, raw)
			}
		})
	}
}

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

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	evidenceindex "github.com/NVIDIA/aicr/pkg/evidence/index"
)

func TestRequireEvidenceAcceptsOnlyPublishedPointers(t *testing.T) {
	t.Setenv(evidenceindex.EnvVar, "")
	configOn := writeRequireEvidenceConfig(t, true)
	configOff := writeRequireEvidenceConfig(t, false)
	externalLeaf := writeExternalEvidenceCatalog(t, true)
	unrelated := writeExternalEvidenceCatalog(t, false)

	tests := []struct {
		name     string
		args     []string
		wantCode errors.ErrorCode
		wantText string
	}{
		{
			name: "default leaves an unattested recipe alone",
			args: []string{"recipe", "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"},
		},
		{
			name:     "flag rejects an unattested recipe",
			args:     []string{"recipe", "--require-evidence", "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"},
			wantCode: errors.ErrCodeNotFound,
			wantText: `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name:     "query cannot bypass the flag",
			args:     []string{"query", "--require-evidence", "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training", "--selector", "deploymentOrder"},
			wantCode: errors.ErrCodeNotFound,
			wantText: `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name: "flag accepts a recipe with a pointer",
			args: []string{"recipe", "--require-evidence", "--service", "eks", "--accelerator", "gb200", "--os", "ubuntu", "--intent", "training"},
		},
		{
			name: "query accepts a recipe with a pointer",
			args: []string{"query", "--require-evidence", "--service", "eks", "--accelerator", "gb200", "--os", "ubuntu", "--intent", "training", "--selector", "deploymentOrder"},
		},
		{
			name:     "config enables the check",
			args:     []string{"recipe", "--config", configOn, "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"},
			wantCode: errors.ErrCodeNotFound,
			wantText: `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name: "explicit false config does not enable the check",
			args: []string{"recipe", "--config", configOff, "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"},
		},
		{
			name:     "flag still wins when config is false",
			args:     []string{"recipe", "--require-evidence", "--config", configOff, "--service", "eks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"},
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:     "base pointer does not cover the default profile",
			args:     []string{"recipe", "--require-evidence", "--service", "aks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training"},
			wantCode: errors.ErrCodeNotFound,
			wantText: "gpustack-azure-managed",
		},
		{
			name: "profiled recipe with its own pointer resolves",
			args: []string{"recipe", "--require-evidence", "--service", "aks", "--accelerator", "h100", "--os", "ubuntu", "--intent", "training", "--platform", "kubeflow"},
		},
		{
			name:     "external leaf fails even when the id has a pointer",
			args:     []string{"recipe", "--require-evidence", "--data", externalLeaf, "--service", "eks", "--accelerator", "gb200", "--os", "ubuntu", "--intent", "training"},
			wantCode: errors.ErrCodeNotFound,
			wantText: "resolved with an external data source",
		},
		{
			name:     "external data fails when the leaf stays embedded",
			args:     []string{"recipe", "--require-evidence", "--data", unrelated, "--service", "eks", "--accelerator", "gb200", "--os", "ubuntu", "--intent", "training"},
			wantCode: errors.ErrCodeNotFound,
			wantText: "resolved with an external data source",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runRequireEvidence(t, tt.args)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("command error = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), evidenceindex.HealthDocURL) {
				t.Errorf("command error = %v, want the recipe-health link", err)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("command error = %v, want text %q", err, tt.wantText)
			}
		})
	}
}

func runRequireEvidence(t *testing.T, args []string) error {
	t.Helper()
	var output bytes.Buffer
	parent := &cli.Command{
		Name:      "aicr",
		Commands:  []*cli.Command{recipeCmd(), queryCmd()},
		Writer:    &output,
		ErrWriter: &output,
	}
	full := append([]string{"aicr"}, args...)
	if args[0] == "recipe" {
		full = append(full, "--output", filepath.Join(t.TempDir(), "recipe.yaml"))
	}
	return parent.Run(t.Context(), full)
}

func writeRequireEvidenceConfig(t *testing.T, enabled bool) string {
	t.Helper()
	value := "false"
	if enabled {
		value = "true"
	}
	path := filepath.Join(t.TempDir(), "aicr-config.yaml")
	body := "apiVersion: aicr.run/v1beta1\nkind: AICRConfig\nmetadata:\n  name: require-evidence-test\nspec:\n  recipe:\n    requireEvidence: " + value + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeExternalEvidenceCatalog writes a --data catalog. matching adds a leaf
// that bases gb200-eks-ubuntu-training with the same criteria, so resolution
// selects that external overlay. Otherwise the overlay cannot match that
// request and the embedded leaf remains. Both shapes use an external source,
// so --require-evidence rejects them.
func writeExternalEvidenceCatalog(t *testing.T, matching bool) string {
	t.Helper()
	dir := t.TempDir()
	registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents: []\n"
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(registry), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	overlays := filepath.Join(dir, "overlays")
	if err := os.MkdirAll(overlays, 0o755); err != nil {
		t.Fatalf("create overlays: %v", err)
	}
	overlay := unrelatedEvidenceOverlay
	if matching {
		overlay = externalEvidenceLeaf
	}
	if err := os.WriteFile(filepath.Join(overlays, "external.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	return dir
}

const externalEvidenceLeaf = `kind: RecipeMetadata
apiVersion: aicr.run/v1beta1
metadata:
  name: external-gb200-leaf
spec:
  base: gb200-eks-ubuntu-training
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training
  componentRefs: []
`

const unrelatedEvidenceOverlay = `kind: RecipeMetadata
apiVersion: aicr.run/v1beta1
metadata:
  name: unrelated-evidence-overlay
spec:
  criteria:
    service: gke
    accelerator: h100
    os: cos
    intent: inference
  componentRefs: []
`

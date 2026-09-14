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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestResolveRecipeForMirrorRejectsTCPXOInterfacesWithRecipe pins the guard:
// --recipe already carries the recorded mapping, so a mapping supplied via the
// flag OR the config document is a conflict, matching the --profile guard.
func TestResolveRecipeForMirrorRejectsTCPXOInterfacesWithRecipe(t *testing.T) {
	t.Parallel()

	mapping := recipe.FormatGKETCPXOInterfaces(recipe.GKETCPXOIntrospectionInterfaces())

	newCmd := func() *cli.Command {
		return &cli.Command{
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "recipe"},
				&cli.StringFlag{Name: flagGKETCPXOInterfaces},
			},
		}
	}

	t.Run("flag", func(t *testing.T) {
		t.Parallel()
		cmd := newCmd()
		if err := cmd.Run(context.Background(), []string{"mirror", "--recipe", "r.yaml",
			"--gke-tcpxo-interfaces", mapping}); err != nil {
			t.Fatalf("command parse: %v", err)
		}
		_, err := resolveRecipeForMirror(context.Background(), cmd, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "cannot be combined with --recipe") {
			t.Fatalf("resolveRecipeForMirror() error = %v, want the conflict rejection", err)
		}
	})

	t.Run("config document", func(t *testing.T) {
		t.Parallel()
		cfgPath := filepath.Join(t.TempDir(), "aicr-config.yaml")
		if err := os.WriteFile(cfgPath, []byte(`apiVersion: aicr.run/v1alpha2
kind: AICRConfig
metadata:
  name: test
spec:
  recipe:
    configuration:
      gke:
        tcpxoInterfaces:
          - {interfaceName: eth1, network: gpu-nic-0}
          - {interfaceName: eth2, network: gpu-nic-1}
          - {interfaceName: eth3, network: gpu-nic-2}
          - {interfaceName: eth4, network: gpu-nic-3}
          - {interfaceName: eth5, network: gpu-nic-4}
          - {interfaceName: eth6, network: gpu-nic-5}
          - {interfaceName: eth7, network: gpu-nic-6}
          - {interfaceName: eth8, network: gpu-nic-7}
`), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := aicr.LoadConfig(context.Background(), cfgPath)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		cmd := newCmd()
		if runErr := cmd.Run(context.Background(), []string{"mirror", "--recipe", "r.yaml"}); runErr != nil {
			t.Fatalf("command parse: %v", runErr)
		}
		_, err = resolveRecipeForMirror(context.Background(), cmd, cfg, nil)
		if err == nil || !strings.Contains(err.Error(), "cannot be combined with --recipe") {
			t.Fatalf("resolveRecipeForMirror() error = %v, want the conflict rejection", err)
		}
	})
}

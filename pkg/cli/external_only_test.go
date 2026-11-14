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
	"context"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	appconfig "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
	"github.com/urfave/cli/v3"
)

func TestExternalOnlySourceSelection(t *testing.T) {
	dir := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":        []byte(providertest.EmptyRegistryYAML),
		"overlays/base.yaml":   []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: base\nspec: {}\n"),
		"overlays/custom.yaml": []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: custom\nspec:\n  base: base\n  criteria:\n    service: external-service\n"),
	})
	tests := []struct {
		name       string
		args       []string
		configDir  string
		configOnly bool
		wantOnly   bool
		wantErr    bool
	}{
		{name: "flag", args: []string{"--data", dir, "--external-only"}, wantOnly: true},
		{name: "config", configDir: dir, configOnly: true, wantOnly: true},
		{name: "flag disables config", args: []string{"--external-only=false"}, configDir: dir, configOnly: true},
		{name: "flag directory with config mode", args: []string{"--data", dir}, configOnly: true, wantOnly: true},
		{name: "flag without directory", args: []string{"--external-only"}, wantErr: true},
		{name: "config without directory", configOnly: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := aicr.WrapConfig(&appconfig.AICRConfig{Spec: appconfig.Spec{Recipe: &appconfig.RecipeSpec{Data: tt.configDir, ExternalOnly: tt.configOnly}}})
			cmd := &cli.Command{Name: "test", Flags: []cli.Flag{dataFlag(), externalOnlyFlag()}, Writer: &bytes.Buffer{}, Action: func(ctx context.Context, cmd *cli.Command) error {
				client, err := recipeClientFromCmd(ctx, cmd, cfg)
				if err != nil {
					return err
				}
				defer func() { _ = client.Close() }()
				entries, err := client.ListCatalog(ctx, nil)
				if err != nil {
					return err
				}
				if tt.wantOnly && (len(entries) != 1 || entries[0].Name != "custom") {
					t.Fatalf("catalog leaked embedded entries: %+v", entries)
				}
				if !tt.wantOnly && len(entries) <= 1 {
					t.Fatalf("layered catalog missing embedded entries: %+v", entries)
				}
				return nil
			}}
			err := cmd.Run(t.Context(), append([]string{"test"}, tt.args...))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "requires --data") {
				t.Fatalf("missing directory error: %v", err)
			}
		})
	}
}

func TestExternalOnlyFlagOnDataCommands(t *testing.T) {
	var walk func(*cli.Command)
	walk = func(cmd *cli.Command) {
		hasData, hasExternal := false, false
		for _, flag := range cmd.Flags {
			for _, name := range flag.Names() {
				hasData = hasData || name == "data"
				hasExternal = hasExternal || name == "external-only"
			}
		}
		if hasData && !hasExternal {
			t.Errorf("%s has --data without --external-only", cmd.Name)
		}
		for _, child := range cmd.Commands {
			walk(child)
		}
	}
	walk(RootCommand())
}

func TestExternalOnlyQueryMissingBase(t *testing.T) {
	dir := providertest.WriteDir(t, map[string][]byte{"registry.yaml": []byte(providertest.EmptyRegistryYAML)})
	var output bytes.Buffer
	cmd := &cli.Command{Name: "aicr", Commands: []*cli.Command{queryCmd()}, Writer: &output}
	err := cmd.Run(t.Context(), []string{"aicr", "query", "--data", dir, "--external-only", "--service", "eks", "--selector", "criteria.service"})
	if err == nil || !strings.Contains(err.Error(), "base.yaml") {
		t.Fatalf("expected missing base.yaml, got %v", err)
	}
}

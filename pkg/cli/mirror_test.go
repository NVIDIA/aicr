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
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/mirror"
)

func TestMirrorCmd_HasListSubcommand(t *testing.T) {
	cmd := mirrorCmd()

	if cmd.Name != "mirror" {
		t.Errorf("Name = %q, want %q", cmd.Name, "mirror")
	}

	if cmd.Category != functionalCategoryName {
		t.Errorf("Category = %q, want %q", cmd.Category, functionalCategoryName)
	}

	if len(cmd.Commands) == 0 {
		t.Fatal("mirror command should have subcommands")
	}

	found := false
	for _, sub := range cmd.Commands {
		if sub.Name == "list" {
			found = true
			break
		}
	}
	if !found {
		t.Error("mirror command missing 'list' subcommand")
	}
}

func TestMirrorListCmd_Flags(t *testing.T) {
	cmd := mirrorListCmd()

	expectedFlags := []string{
		"recipe", "service", "accelerator", "intent", "os", "platform",
		"snapshot", "config", "data", "kubeconfig", "set", "format", "output",
	}
	for _, flagName := range expectedFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing flag: %s", flagName)
		}
	}

	// Check recipe alias.
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "recipe") && !hasFlag(flag, "r") {
			t.Error("--recipe flag missing -r alias")
		}
	}

	// Check format alias.
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "format") && !hasFlag(flag, "f") {
			t.Error("--format flag missing -f alias")
		}
	}

	// Check output alias.
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "output") && !hasFlag(flag, "o") {
			t.Error("--output flag missing -o alias")
		}
	}
}

func TestMirrorCmd_RegisteredInRoot(t *testing.T) {
	root := newRootCmd()

	found := false
	for _, cmd := range root.Commands {
		if cmd.Name == "mirror" {
			found = true
			break
		}
	}
	if !found {
		t.Error("mirror command not registered in root command")
	}
}

func TestMirrorListCmd_InvalidFormat(t *testing.T) {
	cmd := mirrorListCmd()
	app := &cli.Command{
		Name: "aicr",
		Commands: []*cli.Command{
			{
				Name:     "mirror",
				Commands: []*cli.Command{cmd},
			},
		},
	}

	err := app.Run(t.Context(), []string{
		"aicr", "mirror", "list",
		"--format", "invalid",
		"--service", "eks",
		"--accelerator", "h100",
		"--intent", "training",
		"--os", "ubuntu",
	})
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
	if !strings.Contains(err.Error(), "unknown output format") {
		t.Errorf("expected error about unknown format, got: %v", err)
	}
}

func TestWriteMirrorResult_JSON(t *testing.T) {
	var buf bytes.Buffer
	list := &mirror.MirrorList{
		Images: []string{"nginx:latest"},
	}

	if err := mirror.Render(&buf, list, mirror.FormatJSON); err != nil {
		t.Fatalf("mirror.Render(json) error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"nginx:latest"`) {
		t.Errorf("JSON output missing image, got: %s", output)
	}
}

func TestWriteMirrorResult_YAML(t *testing.T) {
	var buf bytes.Buffer
	list := &mirror.MirrorList{
		Images: []string{"nginx:latest"},
	}

	if err := mirror.Render(&buf, list, mirror.FormatYAML); err != nil {
		t.Fatalf("mirror.Render(yaml) error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "nginx:latest") {
		t.Errorf("YAML output missing image, got: %s", output)
	}
}

func TestWriteMirrorResult_Hauler(t *testing.T) {
	var buf bytes.Buffer
	list := &mirror.MirrorList{
		Images: []string{"nginx:latest"},
		Charts: []mirror.ChartRef{
			{Name: "test", Repository: "oci://example.com", Chart: "test", Version: "1.0"},
		},
	}

	if err := mirror.Render(&buf, list, mirror.FormatHauler); err != nil {
		t.Fatalf("mirror.Render(hauler) error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "kind: Images") {
		t.Errorf("Hauler output missing Images kind, got: %s", output)
	}
}

func TestWriteMirrorResult_Zarf(t *testing.T) {
	var buf bytes.Buffer
	list := &mirror.MirrorList{
		Images: []string{"nginx:latest"},
	}

	if err := mirror.Render(&buf, list, mirror.FormatZarf); err != nil {
		t.Fatalf("mirror.Render(zarf) error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "kind: ZarfPackageConfig") {
		t.Errorf("Zarf output missing kind, got: %s", output)
	}
}

func TestWriteMirrorResult_UnsupportedFormat(t *testing.T) {
	var buf bytes.Buffer
	list := &mirror.MirrorList{}

	err := mirror.Render(&buf, list, mirror.Format("bogus"))
	if err == nil {
		t.Fatal("expected error for unsupported format, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("expected unsupported format error, got: %v", err)
	}
}

func TestIsValidMirrorFormat(t *testing.T) {
	tests := []struct {
		format mirror.Format
		want   bool
	}{
		{mirror.FormatYAML, true},
		{mirror.FormatJSON, true},
		{mirror.FormatHauler, true},
		{mirror.FormatZarf, true},
		{mirror.Format("table"), false},
		{mirror.Format("invalid"), false},
		{mirror.Format(""), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			if got := isValidMirrorFormat(tt.format); got != tt.want {
				t.Errorf("isValidMirrorFormat(%q) = %v, want %v", tt.format, got, tt.want)
			}
		})
	}
}

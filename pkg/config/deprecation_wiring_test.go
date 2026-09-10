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

package config_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/header"
)

// TestLoadWarnsOnAlphaAPIVersion binds the AICRConfig loader to the deprecation
// channel. Without a test at this level the call site is deletable green: the
// unit tests in pkg/header prove WarnDeprecatedAPIVersion works, not that
// anything calls it. This one was written because the loader shipped documented
// as warning while doing nothing of the sort.
func TestLoadWarnsOnAlphaAPIVersion(t *testing.T) {
	// t.TempDir is unique per run, so the dedup subject (which embeds the path)
	// differs on every invocation and `go test -count=2` still observes a warning.
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-config.yaml")
	body := "kind: AICRConfig\napiVersion: " + header.GroupVersion + "\n" +
		"metadata:\n  name: legacy\nspec:\n  snapshot: {}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Release N+1 still reads alpha; only the warning is new.
	if _, err := config.Load(context.Background(), path); err != nil {
		t.Fatalf("alpha AICRConfig must still load until %s: %v", header.AlphaRemovedIn, err)
	}

	got := buf.String()
	if !strings.Contains(got, "legacy-config.yaml") {
		t.Errorf("warning does not name the config file: %q", got)
	}
	if !strings.Contains(got, header.AlphaRemovedIn) {
		t.Errorf("warning does not name the removal release %q: %q", header.AlphaRemovedIn, got)
	}
	if !strings.Contains(got, header.GroupVersionV1Beta1) {
		t.Errorf("warning does not name the authoring target %q: %q", header.GroupVersionV1Beta1, got)
	}
}

// TestLoadIsSilentOnTargetAPIVersion is the other half: a migrated config must
// not nag. A warning that fires on the value the user was just told to adopt
// teaches them to filter the channel out.
func TestLoadIsSilentOnTargetAPIVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "current-config.yaml")
	body := "kind: AICRConfig\napiVersion: " + header.GroupVersionV1Beta1 + "\n" +
		"metadata:\n  name: current\nspec:\n  snapshot: {}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := config.Load(context.Background(), path); err != nil {
		t.Fatalf("target AICRConfig must load: %v", err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("target apiVersion must not warn, got: %q", got)
	}
}

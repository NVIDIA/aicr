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

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	// Stub out chainsaw exec for the duration of the test.
	orig := runComponentFn
	defer func() { runComponentFn = orig }()

	t.Run("all pass", func(t *testing.T) {
		runComponentFn = func(_ context.Context, _ time.Duration, _, _, _ string) ComponentResult {
			return ComponentResult{Result: ResultPass}
		}
		bundle := map[string]string{
			"comp-a.yaml": "# stub a",
			"comp-b.yaml": "# stub b",
		}
		res, err := Evaluate(context.Background(), bundle, Options{Namespace: "ns", Timeout: time.Second})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.AllPass {
			t.Errorf("AllPass: got false, want true")
		}
		if len(res.Components) != 2 {
			t.Errorf("Components len: got %d, want 2", len(res.Components))
		}
		for _, name := range []string{"comp-a", "comp-b"} {
			if res.Components[name].Result != ResultPass {
				t.Errorf("Components[%q]: got %v, want Pass", name, res.Components[name])
			}
		}
	})

	t.Run("one fail flips AllPass", func(t *testing.T) {
		runComponentFn = func(_ context.Context, _ time.Duration, _, _, compDir string) ComponentResult {
			if strings.Contains(compDir, "bad") {
				return ComponentResult{Result: ResultFail, Message: "boom"}
			}
			return ComponentResult{Result: ResultPass}
		}
		bundle := map[string]string{
			"good.yaml": "# stub",
			"bad.yaml":  "# stub",
		}
		res, err := Evaluate(context.Background(), bundle, Options{Namespace: "ns", Timeout: time.Second})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.AllPass {
			t.Errorf("AllPass: got true, want false")
		}
		if res.Components["bad"].Result != ResultFail {
			t.Errorf("bad component: got %v, want Fail", res.Components["bad"])
		}
		if res.Components["good"].Result != ResultPass {
			t.Errorf("good component: got %v, want Pass", res.Components["good"])
		}
	})

	t.Run("component name strips .yaml suffix", func(t *testing.T) {
		var seenDirs []string
		runComponentFn = func(_ context.Context, _ time.Duration, _, _, compDir string) ComponentResult {
			seenDirs = append(seenDirs, filepath.Base(compDir))
			return ComponentResult{Result: ResultPass}
		}
		bundle := map[string]string{"prometheus.yaml": "# stub"}
		res, err := Evaluate(context.Background(), bundle, Options{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if _, ok := res.Components["prometheus"]; !ok {
			t.Errorf("expected component %q in result, got %v", "prometheus", res.Components)
		}
		if len(seenDirs) != 1 || seenDirs[0] != "prometheus" {
			t.Errorf("expected compDir basename 'prometheus', got %v", seenDirs)
		}
	})

	t.Run("ConfigPath is forwarded to component runner", func(t *testing.T) {
		var seenConfig []string
		runComponentFn = func(_ context.Context, _ time.Duration, _, configPath, _ string) ComponentResult {
			seenConfig = append(seenConfig, configPath)
			return ComponentResult{Result: ResultPass}
		}
		bundle := map[string]string{"comp.yaml": "# stub"}
		if _, err := Evaluate(context.Background(), bundle,
			Options{Namespace: "ns", ConfigPath: "/etc/chainsaw/config.yaml"}); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(seenConfig) != 1 || seenConfig[0] != "/etc/chainsaw/config.yaml" {
			t.Errorf("ConfigPath forwarded: got %v, want [/etc/chainsaw/config.yaml]", seenConfig)
		}
	})

	t.Run("empty bundle returns AllPass=true", func(t *testing.T) {
		runComponentFn = func(_ context.Context, _ time.Duration, _, _, _ string) ComponentResult {
			t.Fatalf("runComponentFn should not be called for empty bundle")
			return ComponentResult{}
		}
		res, err := Evaluate(context.Background(), map[string]string{}, Options{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.AllPass {
			t.Errorf("AllPass on empty bundle: got false, want true (vacuous)")
		}
		if len(res.Components) != 0 {
			t.Errorf("Components on empty bundle: got %d, want 0", len(res.Components))
		}
	})
}

func TestLoadBundleDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("aaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("bbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Non-yaml file should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Subdir should be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := LoadBundleDir(dir)
	if err != nil {
		t.Fatalf("LoadBundleDir: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len: got %d, want 2 (a.yaml + b.yaml); got map: %v", len(got), got)
	}
	if got["a.yaml"] != "aaa" {
		t.Errorf("a.yaml content: got %q, want %q", got["a.yaml"], "aaa")
	}
	if got["b.yaml"] != "bbb" {
		t.Errorf("b.yaml content: got %q, want %q", got["b.yaml"], "bbb")
	}

	t.Run("missing dir returns error", func(t *testing.T) {
		_, err := LoadBundleDir(filepath.Join(dir, "nope"))
		if err == nil {
			t.Errorf("expected error for missing dir")
		}
	})
}

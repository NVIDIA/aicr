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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	aicr "aicr.run/pkg/client/v1"
	appconfig "aicr.run/pkg/config"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe/providertest"
	"github.com/urfave/cli/v3"
)

const withRegistry = "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\nmetadata:\n  name: %s\ncomponents: []\n"

func withOverlay(name, base, service string) string {
	text := "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: " + name + "\nspec:\n"
	if base != "" {
		text += "  base: " + base + "\n"
	}
	if service != "" {
		text += "  criteria:\n    service: " + service + "\n"
	}
	if base == "" && service == "" {
		return strings.TrimSuffix(text, "\n") + " {}\n"
	}
	return text
}

func withCatalogs(t *testing.T) (alpha, beta, override string) {
	t.Helper()
	alpha = providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":      []byte(strings.Replace(withRegistry, "%s", "alpha", 1)),
		"overlays/base.yaml": []byte(withOverlay("base", "", "")),
		"overlays/a.yaml":    []byte(withOverlay("alpha-leaf", "base", "alpha-service")),
	})
	beta = providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":   []byte(strings.Replace(withRegistry, "%s", "beta", 1)),
		"overlays/b.yaml": []byte(withOverlay("beta-leaf", "alpha-leaf", "beta-service")),
	})
	override = providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":   []byte(providertest.EmptyRegistryYAML),
		"overlays/o.yaml": []byte(withOverlay("override-leaf", "base", "override-service")),
	})
	return alpha, beta, override
}

// catalogNames runs recipeClientFromCmd under a command that defines the
// catalog flags and returns the names ListCatalog reports.
func catalogNames(t *testing.T, cfg *aicr.Config, args ...string) ([]string, error) {
	t.Helper()
	var names []string
	cmd := &cli.Command{
		Name:   "test",
		Flags:  []cli.Flag{dataFlag(), externalOnlyFlag(), withFlag()},
		Writer: &bytes.Buffer{},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			client, err := recipeClientFromCmd(ctx, cmd, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			entries, err := client.ListCatalog(ctx, nil)
			if err != nil {
				return err
			}
			for _, e := range entries {
				names = append(names, e.Name)
			}
			slices.Sort(names)
			return nil
		},
	}
	err := cmd.Run(t.Context(), append([]string{"test"}, args...))
	return names, err
}

func TestWithFlagComposesCatalogs(t *testing.T) {
	alpha, beta, override := withCatalogs(t)

	t.Run("two directories, either order", func(t *testing.T) {
		forward, err := catalogNames(t, nil, "--with", alpha, "--with", beta)
		if err != nil {
			t.Fatal(err)
		}
		reverse, err := catalogNames(t, nil, "--with", beta, "--with", alpha)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"alpha-leaf", "beta-leaf"}
		if !slices.Equal(forward, want) || !slices.Equal(forward, reverse) {
			t.Errorf("catalog = %v / %v, want %v in either order", forward, reverse, want)
		}
	})

	t.Run("--data layers over the composition", func(t *testing.T) {
		names, err := catalogNames(t, nil, "--with", alpha, "--with", beta, "--data", override)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"alpha-leaf", "beta-leaf", "override-leaf"}; !slices.Equal(names, want) {
			t.Errorf("catalog = %v, want %v", names, want)
		}
	})

	t.Run("with nvidia", func(t *testing.T) {
		names, err := catalogNames(t, nil, "--with", "nvidia")
		if err != nil {
			t.Fatal(err)
		}
		if len(names) < 10 {
			t.Errorf("--with nvidia listed %d overlays, want the built-in catalog", len(names))
		}
	})

	t.Run("a partner overlay missing its base names it", func(t *testing.T) {
		_, err := catalogNames(t, nil, "--with", beta)
		if err == nil || !strings.Contains(err.Error(), `"alpha-leaf"`) {
			t.Fatalf("error = %v, want one naming the missing overlay alpha-leaf", err)
		}
	})

	t.Run("config catalogs, replaced by the flag", func(t *testing.T) {
		cfg := aicr.WrapConfig(&appconfig.AICRConfig{Spec: appconfig.Spec{Recipe: &appconfig.RecipeSpec{Catalogs: []string{alpha, beta}}}})
		names, err := catalogNames(t, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"alpha-leaf", "beta-leaf"}; !slices.Equal(names, want) {
			t.Errorf("config catalogs: %v, want %v", names, want)
		}
		names, err = catalogNames(t, cfg, "--with", alpha)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"alpha-leaf"}; !slices.Equal(names, want) {
			t.Errorf("--with must replace spec.recipe.catalogs: %v, want %v", names, want)
		}
	})

	t.Run("not with --external-only", func(t *testing.T) {
		_, err := catalogNames(t, nil, "--with", alpha, "--data", override, "--external-only")
		if err == nil || !strings.Contains(err.Error(), "cannot be combined with --external-only") {
			t.Fatalf("error = %v, want the --with/--external-only conflict", err)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		_, err := catalogNames(t, nil, "--with", " ")
		if err == nil || !strings.Contains(err.Error(), "empty value") {
			t.Fatalf("error = %v, want an empty-value error", err)
		}
	})
}

// TestWithFlagAbsentKeepsDataBehavior pins that without --with the built-in
// catalog and --data behave exactly as before.
func TestWithFlagAbsentKeepsDataBehavior(t *testing.T) {
	_, _, override := withCatalogs(t)
	names, err := catalogNames(t, nil, "--data", override)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "override-leaf") || len(names) < 10 {
		t.Errorf("--data alone must layer over the built-in catalog, got %d entries", len(names))
	}
	// --external-only needs a complete catalog, so it gets a directory with its
	// own base overlay.
	complete := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":      []byte(providertest.EmptyRegistryYAML),
		"overlays/base.yaml": []byte(withOverlay("base", "", "")),
		"overlays/only.yaml": []byte(withOverlay("only-leaf", "base", "only-service")),
	})
	names, err = catalogNames(t, nil, "--data", complete, "--external-only")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"only-leaf"}) {
		t.Errorf("--data --external-only = %v, want only the external catalog", names)
	}
}

func TestWithFlagOnCatalogCommands(t *testing.T) {
	root := RootCommand()
	var walk func(path string, cmd *cli.Command)
	walk = func(path string, cmd *cli.Command) {
		hasExternal, hasWith := false, false
		for _, flag := range cmd.Flags {
			for _, name := range flag.Names() {
				hasExternal = hasExternal || name == "external-only"
				hasWith = hasWith || name == "with"
			}
		}
		// Only `recipe validate` is exempt, by its full path: it statically
		// checks one directory and has no composition to build, so it has
		// --external-only but not --with. The top-level `validate` command
		// resolves recipes against a catalog and must take --with.
		if hasExternal && !hasWith && path != root.Name+" recipe validate" {
			t.Errorf("%s has --external-only without --with", path)
		}
		for _, child := range cmd.Commands {
			walk(path+" "+child.Name, child)
		}
	}
	walk(root.Name, root)
}

// TestRecipeValidateRejectsWith pins that --with, which recipe validate
// inherits from the recipe command, is refused rather than silently ignored.
func TestRecipeValidateRejectsWith(t *testing.T) {
	alpha, _, _ := withCatalogs(t)
	_, err := runFresh(t, "aicr", "recipe", "validate", "--with", alpha, "--data", alpha)
	if err == nil {
		t.Fatal("recipe validate accepted --with")
	}
	if !strings.Contains(err.Error(), "--with does not apply to recipe validate") {
		t.Errorf("error = %v, want the recipe validate rejection", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

// TestRecipeValidateRejectsConfiguredCatalogs pins the same rule for a
// composition that comes from the config: recipe validate never reads
// spec.recipe.catalogs, so it must refuse rather than validate something else
// and report success.
func TestRecipeValidateRejectsConfiguredCatalogs(t *testing.T) {
	alpha, _, _ := withCatalogs(t)
	doc := "apiVersion: " + appconfig.APIVersion + "\nkind: " + appconfig.Kind +
		"\nmetadata:\n  name: test\nspec:\n  recipe:\n    catalogs:\n      - " + alpha + "\n"
	cfgPath := filepath.Join(t.TempDir(), "aicr-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runFresh(t, "aicr", "recipe", "validate", "--config", cfgPath, "--data", alpha)
	if err == nil || !strings.Contains(err.Error(), "spec.recipe.catalogs does not apply to recipe validate") {
		t.Fatalf("error = %v, want the recipe validate rejection of spec.recipe.catalogs", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

// catalogRefs runs catalogRefsFromCmd under a command that defines --with.
func catalogRefs(t *testing.T, cfg *aicr.Config, args ...string) ([]string, error) {
	t.Helper()
	var refs []string
	cmd := &cli.Command{
		Name:   "test",
		Flags:  []cli.Flag{withFlag()},
		Writer: &bytes.Buffer{},
		Action: func(_ context.Context, cmd *cli.Command) error {
			var err error
			refs, err = catalogRefsFromCmd(cmd, cfg)
			return err
		},
	}
	err := cmd.Run(t.Context(), append([]string{"test"}, args...))
	return refs, err
}

func configWithCatalogs(catalogs ...string) *aicr.Config {
	return aicr.WrapConfig(&appconfig.AICRConfig{Spec: appconfig.Spec{Recipe: &appconfig.RecipeSpec{Catalogs: catalogs}}})
}

// TestCatalogRefsFromCmd pins where the catalog list comes from: repeated
// --with values win over spec.recipe.catalogs, and a configured entry is never
// split, so a directory whose path contains a comma can be named there.
func TestCatalogRefsFromCmd(t *testing.T) {
	t.Run("none set", func(t *testing.T) {
		refs, err := catalogRefs(t, nil)
		if err != nil || refs != nil {
			t.Fatalf("refs = %v, error = %v, want none", refs, err)
		}
	})
	t.Run("repeated flag", func(t *testing.T) {
		refs, err := catalogRefs(t, nil, "--with", "nvidia", "--with", "./partner")
		if err != nil || !slices.Equal(refs, []string{"nvidia", "./partner"}) {
			t.Fatalf("refs = %v, error = %v", refs, err)
		}
	})
	t.Run("the flag wins over the config", func(t *testing.T) {
		refs, err := catalogRefs(t, configWithCatalogs("./from-config"), "--with", "nvidia")
		if err != nil || !slices.Equal(refs, []string{"nvidia"}) {
			t.Fatalf("refs = %v, error = %v", refs, err)
		}
	})
	t.Run("config entries keep their commas", func(t *testing.T) {
		refs, err := catalogRefs(t, configWithCatalogs("nvidia", "./team,a/catalog"))
		if err != nil || !slices.Equal(refs, []string{"nvidia", "./team,a/catalog"}) {
			t.Fatalf("refs = %v, error = %v", refs, err)
		}
	})
	t.Run("empty value", func(t *testing.T) {
		_, err := catalogRefs(t, configWithCatalogs("nvidia", "  "))
		if err == nil || !strings.Contains(err.Error(), "empty value") {
			t.Fatalf("error = %v, want the empty-value rejection", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
}

// TestRejectAttestedComposition pins the --attest guard: a bundle built from a
// composition that includes a catalog directory is refused before signing,
// whether the catalogs came from --with or from spec.recipe.catalogs.
func TestRejectAttestedComposition(t *testing.T) {
	alpha, _, _ := withCatalogs(t)

	run := func(t *testing.T, cfg *aicr.Config, args ...string) error {
		t.Helper()
		cmd := &cli.Command{
			Name:   "test",
			Flags:  []cli.Flag{withFlag()},
			Writer: &bytes.Buffer{},
			Action: func(_ context.Context, cmd *cli.Command) error { return rejectAttestedComposition(cmd, cfg) },
		}
		return cmd.Run(t.Context(), append([]string{"test"}, args...))
	}

	for name, tt := range map[string]struct {
		cfg  *aicr.Config
		args []string
		want bool // rejected
	}{
		"no catalogs":                  {nil, nil, false},
		"the built-in catalog":         {nil, []string{"--with", "nvidia"}, false},
		"a directory catalog":          {nil, []string{"--with", alpha}, true},
		"built-in and directory":       {nil, []string{"--with", "nvidia", "--with", alpha}, true},
		"configured built-in catalog":  {configWithCatalogs("nvidia"), nil, false},
		"configured directory catalog": {configWithCatalogs("nvidia", alpha), nil, true},
		"flag replaces a configured directory": {
			configWithCatalogs(alpha), []string{"--with", "nvidia"}, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t, tt.cfg, tt.args...)
			if !tt.want {
				if err != nil {
					t.Fatalf("rejectAttestedComposition() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "--attest cannot sign") {
				t.Fatalf("error = %v, want the --attest rejection", err)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

// TestBundleAttestRejectsComposedDirectory drives the guard through the real
// bundle command, which must refuse before it builds or signs anything.
func TestBundleAttestRejectsComposedDirectory(t *testing.T) {
	alpha, _, _ := withCatalogs(t)
	dir := t.TempDir()
	recipePath := filepath.Join(dir, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: RecipeResult\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")

	_, err := runFresh(t, "aicr", "bundle", "--recipe", recipePath, "--attest", "--with", alpha, "--output", out)
	if err == nil || !strings.Contains(err.Error(), "--attest cannot sign") {
		t.Fatalf("error = %v, want the --attest rejection", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("the output directory was created for a rejected bundle")
	}
}

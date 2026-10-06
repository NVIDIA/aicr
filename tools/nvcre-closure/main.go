// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Command nvcre-closure derives the NVCRE workload runtime closure: the set of
// container images the certification workloads pull, which AICR's mirror
// discovery cannot see.
//
// NVCRE compiles its workload catalog into the manager binary with go:embed, so
// the images the benchmarks run never appear in rendered chart output. Mirror
// discovery extracts images from rendered YAML and therefore reports a single
// controller image while missing every workload image behind it. ADR-025 makes
// closing that gap a benchmark execution safety gate, scoped to the supported
// certification paths rather than the whole catalog.
//
// The closure is read from the upstream repository at the tag matching the
// pinned chart version. That is equivalent to extracting the embedded files
// from the released manager and is reproducible without decompiling a binary;
// the tag is the same input the release builds from.
//
// Usage:
//
//	go run ./tools/nvcre-closure -version v0.2.0 -out recipes/components/nvcre/workload-images.yaml
//	go run ./tools/nvcre-closure -version v0.2.0 -check   # non-zero when the committed file is stale
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	upstreamRepo = "https://github.com/NVIDIA/cluster-readiness-engine.git"
	entriesPath  = "pkg/catalog/entries"
	libPrefix    = "_lib"

	componentName = "nvcre"
	registryPath  = "recipes/registry.yaml"

	cloneTimeout   = 2 * time.Minute
	resolveTimeout = 30 * time.Second
)

// pinnedChartVersion reads the component's pinned chart version from the
// registry. Defaulting to it rather than to a literal is what makes the
// staleness check self-renewing: a `defaultVersion` bump re-derives against the
// new catalog and fails until the closure is regenerated, so the inventory
// cannot quietly describe a version AICR no longer ships.
func pinnedChartVersion(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative registry path
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeNotFound, "read registry", err)
	}
	var reg struct {
		Components []struct {
			Name string `yaml:"name"`
			Helm struct {
				DefaultVersion string `yaml:"defaultVersion"`
			} `yaml:"helm"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, "parse registry", err)
	}
	for _, c := range reg.Components {
		if c.Name != componentName {
			continue
		}
		if c.Helm.DefaultVersion == "" {
			return "", errors.New(errors.ErrCodeInvalidRequest,
				"component "+componentName+" has no pinned chart version")
		}
		return c.Helm.DefaultVersion, nil
	}
	return "", errors.New(errors.ErrCodeNotFound, "component "+componentName+" not found in registry")
}

// closure is the generated document. Field order is the emitted order.
type closure struct {
	Component     string         `yaml:"component"`
	SourceVersion string         `yaml:"sourceVersion"`
	Platform      string         `yaml:"platform"`
	Architecture  string         `yaml:"gpuArchitecture"`
	Entries       []string       `yaml:"entries"`
	Images        []closureImage `yaml:"images"`
	RuntimeFetch  []runtimeFetch `yaml:"runtimeFetches,omitempty"`
}

// closureImage pairs the catalog's tag reference with the digest it resolved to.
// Both are recorded: the digest is what a mirror must copy, and the tag is what
// the catalog says, so a drifted tag is visible rather than silently replaced.
type closureImage struct {
	Image  string `yaml:"image"`
	Digest string `yaml:"digest"`
}

func main() {
	var (
		version   string
		platform  string
		arch      string
		entryList string
		out       string
		check     bool
		catalog   string
	)
	flag.StringVar(&version, "version", "",
		"upstream tag to derive the closure from; defaults to the registry's pinned chart version")
	flag.StringVar(&platform, "platform", "aws", "platform selector to resolve the catalog against")
	flag.StringVar(&arch, "arch", "h100", "gpuArchitecture selector to resolve the catalog against")
	flag.StringVar(&entryList, "entries", "communication/nccl-all-reduce,training/nemotron5-8b",
		"comma-separated catalog entries in scope")
	flag.StringVar(&out, "out", "recipes/components/nvcre/workload-images.yaml", "path to write")
	flag.BoolVar(&check, "check", false, "compare against the committed file and fail when stale")
	flag.StringVar(&catalog, "catalog", "", "use an already-checked-out entries directory instead of cloning")
	flag.Parse()

	if version == "" {
		resolved, err := pinnedChartVersion(registryPath)
		if err != nil {
			slog.Error("resolve pinned chart version", "error", err)
			os.Exit(2)
		}
		version = resolved
		slog.Info("derived version from registry pin", "version", version)
	}

	if err := run(context.Background(), version, platform, arch,
		strings.Split(entryList, ","), out, catalog, check); err != nil {
		slog.Error("closure generation failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, version, platform, arch string, entries []string,
	out, catalog string, check bool,
) error {

	if catalog == "" {
		dir, cleanup, err := cloneEntries(ctx, version)
		if err != nil {
			return err
		}
		defer cleanup()
		catalog = dir
	}

	doc, err := derive(ctx, catalog, version, platform, arch, entries)
	if err != nil {
		return err
	}

	rendered, err := render(doc)
	if err != nil {
		return err
	}

	if check {
		committed, readErr := os.ReadFile(out) //nolint:gosec // operator-supplied path to a repo file
		if readErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "read committed closure", readErr)
		}
		if string(committed) != string(rendered) {
			return errors.New(errors.ErrCodeConflict,
				fmt.Sprintf("%s is stale; regenerate with `make nvcre-closure`", out))
		}
		slog.Info("closure is current", "path", out, "version", version)
		return nil
	}

	if err := os.WriteFile(out, rendered, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write closure", err)
	}
	slog.Info("closure written", "path", out, "images", len(doc.Images), "runtimeFetches", len(doc.RuntimeFetch))
	return nil
}

// cloneEntries checks out the catalog at tag into a temporary directory.
func cloneEntries(ctx context.Context, tag string) (string, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "nvcre-catalog-")
	if err != nil {
		return "", nil, errors.Wrap(errors.ErrCodeInternal, "create temp dir", err)
	}
	cleanup := func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			slog.Warn("remove temp catalog", "error", rmErr)
		}
	}

	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1",
		"--branch", tag, upstreamRepo, dir)
	if combined, runErr := cmd.CombinedOutput(); runErr != nil {
		cleanup()
		return "", nil, errors.WrapWithContext(errors.ErrCodeUnavailable, "clone upstream catalog", runErr,
			map[string]interface{}{"tag": tag, "output": string(combined)})
	}
	return filepath.Join(dir, entriesPath), cleanup, nil
}

// normalizeEntries returns the sorted, trimmed, non-empty entry scope. It runs
// before the scope is recorded rather than during resolution: the closure
// otherwise keeps the raw flag value, so `-entries "a, b,"` writes " b" and ""
// into the committed file and the leading space reorders it. Copying also keeps
// the sort off the caller's slice.
func normalizeEntries(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	sort.Strings(out)
	return out
}

// derive resolves the in-scope entries against the platform and architecture
// selectors and digest-resolves every image the resulting blocks reference.
func derive(ctx context.Context, entriesDir, version, platform, arch string,
	entries []string,
) (*closure, error) {

	scope := normalizeEntries(entries)
	doc := &closure{
		Component:     "nvcre",
		SourceVersion: version,
		Platform:      platform,
		Architecture:  arch,
		Entries:       scope,
	}

	var images []string
	var fetches []runtimeFetch
	for _, entry := range scope {
		imgs, fs, err := resolveEntry(entriesDir, entry, platform, arch)
		if err != nil {
			return nil, err
		}
		images = append(images, imgs...)
		fetches = append(fetches, fs...)
	}

	if len(images) == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"no images resolved; the catalog layout or selector semantics changed upstream")
	}

	for _, img := range sortedUnique(images) {
		digest, err := resolveDigest(ctx, img)
		if err != nil {
			return nil, err
		}
		doc.Images = append(doc.Images, closureImage{Image: img, Digest: digest})
	}
	doc.RuntimeFetch = dedupeFetches(fetches)
	return doc, nil
}

// resolveEntry reads one catalog entry and every `lib` fragment the applicable
// blocks pull in, returning the images and runtime fetches on that path.
func resolveEntry(entriesDir, entry, platform, arch string) ([]string, []runtimeFetch, error) {
	data, err := os.ReadFile(filepath.Join(entriesDir, entry+".yaml")) //nolint:gosec // bounded by entriesDir
	if err != nil {
		return nil, nil, errors.WrapWithContext(errors.ErrCodeNotFound, "read catalog entry", err,
			map[string]interface{}{"entry": entry})
	}

	var images []string
	var fetches []runtimeFetch
	for _, b := range splitBlocks(string(data)) {
		if !b.sel.matches(platform, arch) {
			continue
		}
		images = append(images, scanImages(b.body)...)
		fetches = append(fetches, scanRuntimeFetches(b.body)...)

		for _, ref := range b.libRefs {
			libImages, libFetches, libErr := readLib(entriesDir, ref)
			if libErr != nil {
				return nil, nil, libErr
			}
			images = append(images, libImages...)
			fetches = append(fetches, libFetches...)
		}
	}
	return images, fetches, nil
}

// readLib loads a shared fragment referenced by a `{{ lib "..." }}` directive.
// Fragments carry no selectors of their own: the block that pulls one in has
// already been matched.
func readLib(entriesDir, ref string) ([]string, []runtimeFetch, error) {
	path := filepath.Join(entriesDir, libPrefix, filepath.Clean(ref))
	rel, err := filepath.Rel(filepath.Join(entriesDir, libPrefix), path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("lib reference escapes the catalog: %q", ref))
	}
	data, err := os.ReadFile(path) //nolint:gosec // confined to entriesDir by the check above
	if err != nil {
		return nil, nil, errors.WrapWithContext(errors.ErrCodeNotFound, "read lib fragment", err,
			map[string]interface{}{"ref": ref})
	}
	return scanImages(string(data)), scanRuntimeFetches(string(data)), nil
}

// resolveDigest resolves a tag reference to the digest a mirror must copy.
func resolveDigest(ctx context.Context, image string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	ref, err := name.ParseReference(image)
	if err != nil {
		return "", errors.WrapWithContext(errors.ErrCodeInvalidRequest, "parse image reference", err,
			map[string]interface{}{"image": image})
	}
	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", errors.WrapWithContext(errors.ErrCodeUnavailable, "resolve image digest", err,
			map[string]interface{}{"image": image})
	}
	return desc.Digest.String(), nil
}

func dedupeFetches(in []runtimeFetch) []runtimeFetch {
	seen := map[string]struct{}{}
	out := make([]runtimeFetch, 0, len(in))
	for _, f := range in {
		key := f.URL + "@" + f.Ref
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

// render emits the document with the generated-file header. Marshaling goes
// through the deterministic serializer so a regeneration with no input change
// produces identical bytes and the -check comparison stays meaningful.
func render(doc *closure) ([]byte, error) {
	body, err := serializer.MarshalYAMLDeterministic(doc)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "marshal closure", err)
	}

	var b strings.Builder
	b.WriteString(generatedHeader)
	b.Write(body)
	return []byte(b.String()), nil
}

const generatedHeader = `# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

# GENERATED FILE - DO NOT EDIT. Regenerate with ` + "`make nvcre-closure`" + `.
#
# The NVCRE workload runtime closure: the images the certification workloads
# pull, which rendering the chart cannot reveal because the workload catalog is
# go:embed-compiled into the manager binary. Mirror tooling reads this file to
# learn what a disconnected install has to carry beyond the controller image.
#
# Scope follows ADR-025's benchmark execution safety gate: the supported
# certification paths only, not every dormant catalog entry. Re-derive whenever
# the chart pin moves, because the catalog ships inside the manager image.
`

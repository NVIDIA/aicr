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

// Command registry-inventory statically extracts every package/container
// registry AICR's builds, CI, and deploy paths reach out to, from the
// high-reliability *structured* sources (recipes/registry.yaml, .settings.yaml,
// .goreleaser.yaml/.ko.yaml, Dockerfiles, .github Actions `uses:` pins).
//
// It emits a normalized, deterministic inventory (YAML + Markdown) and can
// check the distinct host set against a committed allowlist so a newly
// introduced registry fails CI — the same drift-gate shape as the BOM's
// TestCommittedBOMVersionsMatchRegistry.
//
// It deliberately does NOT parse interpolated shell installers or render Helm
// charts; those surfaces are reported as uncovered (see sources.go) so the
// inventory never reads as more complete than it is. The rendered chart→image
// surface is owned by tools/bom (docs/user/container-images.md).
//
// Usage:
//
//	registry-inventory -repo-root . -out-dir dist/registry-inventory      # emit inventory
//	registry-inventory -repo-root . -list-hosts               # print distinct hosts
//	registry-inventory -repo-root . -check                    # fail on non-allowlisted host
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// Inventory is the full extraction result: sorted facts, the distinct host set,
// and the problems gathered along the way. SourceErrors (a required source that
// failed to read/parse → under-detection risk) are kept apart from soft
// Warnings so the gate can escalate on the former without failing on the latter.
type Inventory struct {
	Records          []Record `yaml:"records"`
	Hosts            []string `yaml:"hosts"`
	SourceErrors     []string `yaml:"sourceErrors,omitempty"`
	Warnings         []string `yaml:"warnings,omitempty"`
	UncoveredSources []string `yaml:"uncoveredSources"`
}

// Collect runs every structured-source extractor against repoRoot and returns a
// deduped, deterministically sorted inventory. It performs no network I/O — all
// inputs are files in the repo — so it is safe to call from a unit test.
func Collect(repoRoot string) *Inventory {
	passes := []func(string) extractResult{
		extractRegistry,
		extractSettings,
		extractBuildImages,
		extractDockerfiles,
		extractGitHubActions,
		extractShellInstallers,
	}
	var recs []Record
	var warnings, sourceErrors []string
	for _, p := range passes {
		res := p(repoRoot)
		recs = append(recs, res.records...)
		warnings = append(warnings, res.warnings...)
		sourceErrors = append(sourceErrors, res.sourceErrors...)
	}
	sortRecords(recs)
	recs = dedupRecords(recs)

	hostSet := map[string]struct{}{}
	for _, r := range recs {
		hostSet[r.Host] = struct{}{}
	}
	hosts := make([]string, 0, len(hostSet))
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	warnings = sortedUnique(warnings)
	sourceErrors = sortedUnique(sourceErrors)

	return &Inventory{
		Records:          recs,
		Hosts:            hosts,
		SourceErrors:     sourceErrors,
		Warnings:         warnings,
		UncoveredSources: uncoveredSources,
	}
}

// sortedUnique returns s sorted with exact duplicates removed.
func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return s
	}
	sort.Strings(s)
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	var (
		repoRoot  string
		outDir    string
		docOut    string
		listHosts bool
		check     bool
	)
	flag.StringVar(&repoRoot, "repo-root", ".", "path to the AICR repository root")
	flag.StringVar(&outDir, "out-dir", "dist/registry-inventory", "directory to write registry-inventory.{yaml,md}")
	flag.StringVar(&docOut, "doc-out", "", "write the committed host-level docs page to this path and exit")
	flag.BoolVar(&listHosts, "list-hosts", false, "print the distinct host set (one per line) and exit")
	flag.BoolVar(&check, "check", false, "fail if any extracted host is absent from the committed allowlist")
	flag.Parse()

	if err := run(repoRoot, outDir, docOut, listHosts, check); err != nil {
		fmt.Fprintln(os.Stderr, "registry-inventory:", err)
		os.Exit(1)
	}
}

func run(repoRoot, outDir, docOut string, listHosts, check bool) error {
	inv := Collect(repoRoot)

	if listHosts {
		for _, h := range inv.Hosts {
			fmt.Println(h)
		}
		return nil
	}

	if docOut != "" {
		if err := writeDoc(docOut, inv); err != nil {
			return err
		}
		fmt.Printf("registry-inventory: wrote %s (%d hosts)\n", docOut, len(inv.Hosts))
		return nil
	}

	if check {
		allowPath := filepath.Join(repoRoot, "tools", "registry-inventory", allowlistFile)
		unknown, unused, err := diffAllowlist(allowPath, inv.Hosts)
		if err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "load allowlist")
		}
		for _, h := range unused {
			fmt.Fprintf(os.Stderr, "note: allowlisted host no longer detected: %s\n", h)
		}
		// A required source that failed to read/parse contributed zero hosts —
		// the gate would otherwise pass green while blind to that source's
		// egress. Escalate on it (F14/F25/N1). Soft warnings (var-built URLs)
		// stay informational.
		for _, e := range inv.SourceErrors {
			fmt.Fprintf(os.Stderr, "SOURCE FAILED (dropped from inventory): %s\n", e)
		}
		for _, h := range unknown {
			fmt.Fprintf(os.Stderr, "NEW REGISTRY-EGRESS HOST (not in allowlist): %s\n", h)
		}
		if len(unknown) > 0 || len(inv.SourceErrors) > 0 {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("registry-check failed: %d unallowlisted host(s), %d source failure(s); "+
					"review and add hosts to %s if intended, or fix the failed source",
					len(unknown), len(inv.SourceErrors), allowlistFile))
		}
		fmt.Printf("registry-inventory: OK, %d hosts all allowlisted (%d soft warnings)\n",
			len(inv.Hosts), len(inv.Warnings))
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir out-dir", err)
	}
	if err := writeYAML(filepath.Join(outDir, "registry-inventory.yaml"), inv); err != nil {
		return err
	}
	if err := writeMarkdown(filepath.Join(outDir, "registry-inventory.md"), inv); err != nil {
		return err
	}
	fmt.Printf("registry-inventory: wrote %s (%d records, %d hosts, %d warnings)\n",
		outDir, len(inv.Records), len(inv.Hosts), len(inv.Warnings))
	return nil
}

func writeYAML(path string, inv *Inventory) error {
	data, err := yaml.Marshal(inv)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "marshal inventory", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // report artifact, not a secret
		return errors.Wrap(errors.ErrCodeInternal, "write "+path, err)
	}
	return nil
}

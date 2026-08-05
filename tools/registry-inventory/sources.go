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

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// extractResult is what each source pass returns. Two problem buckets are kept
// distinct on purpose:
//   - sourceErrors: a *required* source could not be read/parsed, so it
//     contributed zero records. This is a fail-open risk (under-detection with a
//     green gate), so the gate escalates on it.
//   - warnings: soft coverage notes (e.g. a shell download URL built from a
//     variable) that are expected and non-fatal.
type extractResult struct {
	records      []Record
	warnings     []string
	sourceErrors []string
}

// dirsToSkip are trees that either aren't AICR's egress surface (vendored deps)
// or are throwaway fixtures that would add noise to the real inventory.
var dirsToSkip = map[string]struct{}{
	".git": {}, "vendor": {}, "dist": {}, "node_modules": {},
	"testdata": {}, ".claude": {},
}

func rel(repoRoot, path string) string {
	if r, err := filepath.Rel(repoRoot, path); err == nil {
		return r
	}
	return path
}

// ---------------------------------------------------------------------------
// recipes/registry.yaml — declared Helm chart repositories (target cluster)
// ---------------------------------------------------------------------------

type regFile struct {
	Components []struct {
		Name string `yaml:"name"`
		Helm struct {
			DefaultRepository string `yaml:"defaultRepository"`
			DefaultChart      string `yaml:"defaultChart"`
			DefaultVersion    string `yaml:"defaultVersion"`
		} `yaml:"helm"`
	} `yaml:"components"`
}

// extractRegistry reads the chart repositories AICR resolves at bundle/deploy
// time. It does NOT enumerate the images those charts render to — that is the
// job of `tools/bom` (docs/user/container-images.md), which is deliberately not
// duplicated here.
func extractRegistry(repoRoot string) extractResult {
	path := filepath.Join(repoRoot, "recipes", "registry.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		return extractResult{sourceErrors: []string{fmt.Sprintf("registry.yaml: %v", err)}}
	}
	var rf regFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return extractResult{sourceErrors: []string{fmt.Sprintf("registry.yaml parse: %v", err)}}
	}
	src := rel(repoRoot, path)
	var recs []Record
	for _, c := range rf.Components {
		repo := strings.TrimSpace(c.Helm.DefaultRepository)
		if repo == "" {
			continue // manifest-only component; no chart pull
		}
		host := urlHost(repo)
		if host == "" {
			continue
		}
		pkg := PkgHelmChartHTTP
		if strings.HasPrefix(repo, "oci://") {
			pkg = PkgOCIHelmChart
		}
		pinType := PinNone
		if v := strings.TrimSpace(c.Helm.DefaultVersion); v != "" {
			pinType = PinTag
		}
		recs = append(recs, Record{
			Host:        host,
			PackageType: pkg,
			Direction:   DirPull,
			Consumer:    ConsumerTargetCluster,
			PinType:     pinType,
			Pin:         strings.TrimSpace(c.Helm.DefaultVersion),
			Detail:      c.Name,
			Source:      src,
		})
	}
	return extractResult{records: recs}
}

// ---------------------------------------------------------------------------
// .settings.yaml — pinned infra container images and chart repos
// ---------------------------------------------------------------------------

// extractSettings walks every scalar in .settings.yaml and records the ones
// that look like container image references or chart/registry URLs. The file is
// the declared single source of truth for tool/image pins, so a generic walk is
// more robust than hard-coding today's key names.
func extractSettings(repoRoot string) extractResult {
	path := filepath.Join(repoRoot, ".settings.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		return extractResult{sourceErrors: []string{fmt.Sprintf(".settings.yaml: %v", err)}}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return extractResult{sourceErrors: []string{fmt.Sprintf(".settings.yaml parse: %v", err)}}
	}
	src := rel(repoRoot, path)
	var recs []Record
	walkScalars(&root, func(key, val string) {
		val = strings.TrimSpace(val)
		switch {
		case strings.HasPrefix(val, "https://") || strings.HasPrefix(val, "oci://"):
			// Record the host of ANY https/oci scalar, not only chart-key ones —
			// a raw download/registry URL under any key is real egress (F15). An
			// https value here isn't necessarily a Helm chart repo, so it gets
			// the generic http type; only oci:// is labeled an OCI chart.
			host := urlHost(val)
			if host == "" {
				return
			}
			pkg := PkgHTTP
			if strings.HasPrefix(val, "oci://") {
				pkg = PkgOCIHelmChart
			}
			recs = append(recs, Record{
				Host: host, PackageType: pkg, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: PinNone, Detail: key, Source: src,
			})
		case looksLikeImageRef(key, val):
			pt, pin := imagePin(val)
			recs = append(recs, Record{
				Host: imageHost(val), PackageType: PkgContainerImage, Direction: DirPull,
				Consumer: ConsumerCIRunner, PinType: pt, Pin: pin, Detail: key, Source: src,
			})
		}
	})
	return extractResult{records: recs}
}

// looksLikeImageRef decides whether a .settings.yaml scalar is a container image
// reference. A digest or a tag on the final path segment is sufficient (this
// covers implicit-docker.io two-part images like `ministackorg/ministack:1.4.7`,
// F4). An untagged `host/path` scalar is only treated as an image when the KEY
// names one (e.g. `nvml_mock_image`) — otherwise a plain `gitlab.com/g/p` value
// would be misrecorded as an image and could falsely trip the gate (F5).
func looksLikeImageRef(key, val string) bool {
	if val == "" || strings.ContainsAny(val, " \t") || strings.Contains(val, "://") {
		return false
	}
	if strings.Contains(val, "@sha256:") {
		return true
	}
	if !strings.Contains(val, "/") {
		return false // bare token, no registry/org
	}
	lastSeg := val[strings.LastIndex(val, "/")+1:]
	if strings.Contains(lastSeg, ":") {
		return true // image:tag
	}
	return keyNamesImage(key)
}

func keyNamesImage(key string) bool {
	return strings.Contains(strings.ToLower(key), "image")
}

// walkScalars invokes fn(mappingKey, scalarValue) for every scalar leaf that is
// the value of a mapping key, resolving YAML aliases/anchors and merge keys so
// an anchored image pin is not missed (F8).
func walkScalars(n *yaml.Node, fn func(key, val string)) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walkScalars(c, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := resolveAlias(n.Content[i+1])
			switch v.Kind {
			case yaml.ScalarNode:
				fn(k.Value, v.Value)
			case yaml.SequenceNode:
				for _, item := range v.Content {
					item = resolveAlias(item)
					if item.Kind == yaml.ScalarNode {
						fn(k.Value, item.Value)
					} else {
						walkScalars(item, fn)
					}
				}
			case yaml.DocumentNode, yaml.MappingNode, yaml.AliasNode:
				walkScalars(v, fn)
			}
		}
	case yaml.ScalarNode, yaml.AliasNode:
		// leaf with no enclosing key; nothing to emit
	}
}

// resolveAlias follows an AliasNode to its anchored target (used for `*anchor`
// values and `<<: *defaults` merge keys).
func resolveAlias(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return n.Alias
	}
	return n
}

// ---------------------------------------------------------------------------
// .goreleaser.yaml / .ko.yaml — base images (pull) and ko push repos
// ---------------------------------------------------------------------------

var (
	baseImageRe   = regexp.MustCompile(`(?m)^\s*(?:base_image|defaultBaseImage):\s*(\S+)`)
	reposBlockRe  = regexp.MustCompile(`^(\s*)repositories:\s*$`)
	reposEntryRe  = regexp.MustCompile(`^(\s*)-\s+(\S+)`)
	requiredBuild = map[string]struct{}{".goreleaser.yaml": {}, ".ko.yaml": {}}
)

// extractBuildImages pulls the compiled-in base image and ko push destinations
// from the release config. base_image ships in the product; the kos
// `repositories:` entries are push targets.
func extractBuildImages(repoRoot string) extractResult {
	var recs []Record
	var sourceErrors []string
	for f := range requiredBuild {
		path := filepath.Join(repoRoot, f)
		data, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
		if err != nil {
			sourceErrors = append(sourceErrors, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		src := rel(repoRoot, path)
		for _, m := range baseImageRe.FindAllStringSubmatch(string(data), -1) {
			ref := strings.TrimSpace(m[1])
			pt, pin := imagePin(ref)
			recs = append(recs, Record{
				Host: imageHost(ref), PackageType: PkgContainerImage, Direction: DirPull,
				Consumer: ConsumerCompiledIn, PinType: pt, Pin: pin, Detail: "base_image", Source: src,
			})
		}
		// ko push repos are scanned only inside a `repositories:` block so a
		// stray dotted list item elsewhere can't be mistaken for a push target,
		// and the pin is derived from the ref rather than hardcoded (F6).
		for _, ref := range koPushRepos(data) {
			pt, pin := imagePin(ref)
			recs = append(recs, Record{
				Host: imageHost(ref), PackageType: PkgContainerImage, Direction: DirPush,
				Consumer: ConsumerCIRunner, PinType: pt, Pin: pin, Detail: ref, Source: src,
			})
		}
	}
	return extractResult{records: recs, sourceErrors: sourceErrors}
}

// koPushRepos returns the registry refs listed under a `repositories:` block
// (ko/goreleaser push targets), tracking indentation to stay inside the block.
func koPushRepos(data []byte) []string {
	var repos []string
	inBlock := false
	baseIndent := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := reposBlockRe.FindStringSubmatch(line); m != nil {
			inBlock = true
			baseIndent = len(m[1])
			continue
		}
		if !inBlock {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A list item at or below the key's indent is still a member of the
		// block — YAML allows `- item` aligned with the `repositories:` key.
		if m := reposEntryRe.FindStringSubmatch(line); m != nil && len(m[1]) >= baseIndent {
			repos = append(repos, strings.TrimSpace(m[2]))
			continue
		}
		// A non-entry line at or above the block indent ends the block.
		if indent := leadingSpaces(line); indent <= baseIndent {
			inBlock = false
		}
	}
	return repos
}

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// ---------------------------------------------------------------------------
// Dockerfiles — FROM base images
// ---------------------------------------------------------------------------

var (
	fromRe = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	argRe  = regexp.MustCompile(`(?i)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)(?:=(.*))?$`)
)

// extractDockerfiles walks the repo for Dockerfiles and records each FROM image.
// A file that cannot be read, or a FROM whose registry host cannot be resolved,
// is a sourceError (fail closed) rather than a soft warning — a dropped/opaque
// base image must not leave the gate green.
func extractDockerfiles(repoRoot string) extractResult {
	var recs []Record
	var sourceErrors []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// An unreadable dir could hide a Dockerfile — fail closed, matching
			// extractGitHubActions (don't let the two passes disagree).
			sourceErrors = append(sourceErrors, fmt.Sprintf("%s: %v", rel(repoRoot, path), werr))
			return nil
		}
		if d.IsDir() {
			if _, skip := dirsToSkip[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if base != "Dockerfile" && !strings.HasSuffix(base, ".Dockerfile") {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // path bounded by repo walk
		if rerr != nil {
			sourceErrors = append(sourceErrors, fmt.Sprintf("%s: %v", rel(repoRoot, path), rerr))
			return nil
		}
		r, se := dockerfileFroms(rel(repoRoot, path), data)
		recs = append(recs, r...)
		sourceErrors = append(sourceErrors, se...)
		return nil
	})
	if err != nil {
		sourceErrors = append(sourceErrors, fmt.Sprintf("walk dockerfiles: %v", err))
	}
	return extractResult{records: recs, sourceErrors: sourceErrors}
}

func dockerfileFroms(src string, data []byte) (recs []Record, sourceErrors []string) {
	// Single positional pass: an ARG applies only to the FROMs that follow it
	// (and a later re-declaration must not retroactively change an earlier FROM),
	// so ARG defaults are accumulated in-order as they are encountered.
	args := map[string]string{}
	stages := map[string]struct{}{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := argRe.FindStringSubmatch(line); m != nil {
			// A bare `ARG NAME` (no `=`) declares no default — leave references
			// unresolved so hostUnresolved fails closed rather than substituting "".
			if strings.Contains(m[0], "=") {
				args[m[1]] = strings.Trim(strings.TrimSpace(m[2]), `"'`)
			}
			continue
		}
		m := fromRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := substituteArgs(m[1], args)
		// Stage names are case-insensitive in Docker (F10).
		if alias := m[2]; alias != "" {
			stages[strings.ToLower(alias)] = struct{}{}
		}
		if ref == "scratch" {
			continue
		}
		if _, isStage := stages[strings.ToLower(ref)]; isStage {
			continue // FROM builder — an earlier stage, not a registry pull
		}
		// Fail closed on an unresolved/empty registry host: `FROM ${BASE}` with no
		// resolvable ARG default would otherwise be silently mislabeled docker.io
		// and hide the real host from the allowlist.
		if ref == "" || hostUnresolved(ref) {
			sourceErrors = append(sourceErrors, fmt.Sprintf(
				"%s: unresolved FROM host %q (interpolated ARG/variable — cannot gate)", src, m[1]))
			continue
		}
		pt, pin := imagePin(ref)
		recs = append(recs, Record{
			Host: imageHost(ref), PackageType: PkgContainerImage, Direction: DirPull,
			Consumer: ConsumerCIRunner, PinType: pt, Pin: pin, Detail: ref, Source: src,
		})
	}
	if err := sc.Err(); err != nil {
		sourceErrors = append(sourceErrors, fmt.Sprintf("%s: scan: %v", src, err))
	}
	return recs, sourceErrors
}

// dockerVarRe matches a ${NAME} or $NAME shell variable. The `\w+` is greedy, so
// `$REGISTRY` is captured whole — a shorter ARG like `$REG` can't consume its
// prefix (the bug a naive ReplaceAll pass has).
var dockerVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// substituteArgs replaces ${NAME}/$NAME tokens using collected ARG defaults.
// Unknown names are left intact so hostUnresolved fails closed on them.
func substituteArgs(ref string, args map[string]string) string {
	return dockerVarRe.ReplaceAllStringFunc(ref, func(tok string) string {
		m := dockerVarRe.FindStringSubmatch(tok)
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if v, ok := args[name]; ok {
			return v
		}
		return tok
	})
}

// hostUnresolved reports whether a FROM ref's registry-host segment still
// contains an unresolved shell variable (the tag may legitimately interpolate,
// e.g. `golang:${GO_VERSION}`, but the host must be concrete to be gated).
func hostUnresolved(ref string) bool {
	end := len(ref)
	if i := strings.IndexAny(ref, "/:"); i >= 0 {
		end = i
	}
	return strings.Contains(ref[:end], "$")
}

// ---------------------------------------------------------------------------
// .github — GitHub Actions `uses:` supply-chain surface
// ---------------------------------------------------------------------------

var usesRe = regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s+['"]?([^'"\s]+)['"]?`)

// extractGitHubActions walks .github for `uses:` refs. Marketplace actions
// collapse to the single logical host "github-actions" (per-owner threats are an
// acknowledged gap — see uncoveredSources); a `docker://` ref is a real
// container pull and is recorded as such (F12). Local (./) composite-action refs
// are skipped (no external egress).
func extractGitHubActions(repoRoot string) extractResult {
	ghDir := filepath.Join(repoRoot, ".github")
	// A missing/unreadable .github removes the entire Actions surface — fail
	// closed rather than pass green with zero action records.
	if info, err := os.Stat(ghDir); err != nil || !info.IsDir() {
		return extractResult{sourceErrors: []string{fmt.Sprintf(".github: not a readable directory: %v", err)}}
	}
	var recs []Record
	var sourceErrors []string
	err := filepath.WalkDir(ghDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			sourceErrors = append(sourceErrors, fmt.Sprintf("%s: %v", rel(repoRoot, path), werr))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // path bounded by .github walk
		if rerr != nil {
			sourceErrors = append(sourceErrors, fmt.Sprintf("%s: %v", rel(repoRoot, path), rerr))
			return nil
		}
		src := rel(repoRoot, path)
		for _, m := range usesRe.FindAllStringSubmatch(string(data), -1) {
			ref := m[1]
			switch {
			case strings.HasPrefix(ref, "./"):
				continue // local composite action, no egress
			case strings.HasPrefix(ref, "docker://"):
				img := strings.TrimPrefix(ref, "docker://")
				pt, pin := imagePin(img)
				recs = append(recs, Record{
					Host: imageHost(img), PackageType: PkgContainerImage, Direction: DirPull,
					Consumer: ConsumerCIRunner, PinType: pt, Pin: pin, Detail: img, Source: src,
				})
			default:
				owner, _, _ := strings.Cut(ref, "@")
				pt, pin := actionPin(ref)
				recs = append(recs, Record{
					Host: "github-actions", PackageType: PkgGitHubAction, Direction: DirPull,
					Consumer: ConsumerCIRunner, PinType: pt, Pin: pin, Detail: owner, Source: src,
				})
			}
		}
		return nil
	})
	if err != nil {
		sourceErrors = append(sourceErrors, fmt.Sprintf("walk .github: %v", err))
	}
	return extractResult{records: recs, sourceErrors: sourceErrors}
}

// uncoveredSources are egress surfaces this prototype does NOT statically gate.
// They are reported so the inventory never masquerades as complete — see the
// report's §1E/§5 and the "No silent caps" rule.
var uncoveredSources = []string{
	"tests/uat/lib/phases.sh (helm-diff plugin + workload images assembled from shell vars)",
	"kwok/scripts/*.sh (in-cluster registry/gitea/karpenter side-loads)",
	"pkg/**/*.go Sigstore endpoint constants (fulcio/rekor/tuf/oidc) — see pkg/defaults/sigstore.go",
	"GitHub workflow inline `image:` and docker/login-action registry inputs",
	"tools/setup-tools: best-effort only — URLs built from shell vars are reported as warnings, not records",
	"GitHub Actions per-OWNER trust: all `uses:` collapse to host `github-actions`, so a new third-party action owner is not gated (only SHA-pinning is enforced, by TestExternalActionsArePinned)",
}

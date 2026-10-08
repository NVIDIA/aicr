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
	"regexp"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Catalog entry files are Go templates, not YAML: `{{ lib "..." }}` directives
// sit at the start of a line where a mapping key is expected, so a YAML parser
// rejects them before any image reference can be read. The scan below is
// therefore line-based over whole blocks rather than a document walk.
var (
	libRefPattern   = regexp.MustCompile(`\{\{-?\s*lib\s+"([^"]+)"`)
	imageRefPattern = regexp.MustCompile(`^\s*(?:-\s+)?image:\s*["']?([^"'\s]+)["']?\s*$`)
	// Runtime fetches defeat digest pinning: the bytes arrive at pod start from
	// outside the image, so a mirrored registry is not sufficient to run the
	// path disconnected.
	// Matched in steps rather than as one pattern: requiring the branch flag
	// ahead of the URL dropped `git clone <url> <dir>` and
	// `git clone <url> -b <ref>` silently, and an unrecorded fetch makes the
	// closure claim a path runs disconnected when it does not.
	gitClonePattern = regexp.MustCompile(`git\s+clone\b[^|&;\n]*`)
	// Two URL patterns, tried in this order. The .git form is matched first and
	// non-greedily because the remote is often immediately followed by template
	// syntax with no separator (`…Megatron-LM.git{{ end }}"`), which a
	// token-greedy match would swallow. git also accepts an https remote with
	// no .git suffix, so the fallback takes the whole token, stopping at the
	// quote and brace characters that delimit it in catalog sources.
	cloneGitURLPattern = regexp.MustCompile(`https://\S+?\.git\b`)
	cloneAnyURLPattern = regexp.MustCompile(`https://[^\s"'{}]+`)
	cloneRefPattern    = regexp.MustCompile(`(?:-b|--branch)\s+(\S+)`)
	cloneVarPattern    = regexp.MustCompile(`\$\{?(\w+)\}?`)
	shellAssignment    = regexp.MustCompile(`(?m)^\s*(\w+)=(.*)$`)
	fetchPattern       = regexp.MustCompile(`\b(?:curl|wget)\s+[^|&;]*?(https://\S+)`)
)

// Constraint fields a `when:` clause can scope a block by.
const (
	fieldPlatform = "platform"
	fieldArch     = "arch"
)

// selector is the subset of a catalog `when:` clause that decides whether a
// block applies. A nil field matches anything, which is how the base
// `dependencies:` section and unconstrained overrides behave.
type selector struct {
	platformEquals string
	archEquals     string
	archIn         []string
	archNotIn      []string
}

// matches reports whether the block applies to platform and arch. An absent
// constraint matches, so a block that names only a platform applies to every
// accelerator on it.
func (s selector) matches(platform, arch string) bool {
	if s.platformEquals != "" && s.platformEquals != platform {
		return false
	}
	if s.archEquals != "" && s.archEquals != arch {
		return false
	}
	if len(s.archIn) > 0 && !slicesContains(s.archIn, arch) {
		return false
	}
	if len(s.archNotIn) > 0 && slicesContains(s.archNotIn, arch) {
		return false
	}
	return true
}

func slicesContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// block is one selectable unit of a catalog entry: either the base
// `dependencies:` section or a single `- when:` override.
type block struct {
	sel     selector
	body    string
	libRefs []string
}

// runtimeFetch is a network fetch performed at pod start rather than baked into
// an image.
type runtimeFetch struct {
	URL string `yaml:"url"`
	Ref string `yaml:"ref,omitempty"`
}

// splitBlocks divides a catalog entry into its base section and the override
// list. Overrides begin at a `- when:` sequence item in column zero; everything
// before the `overrides:` key is the base, which always applies.
func splitBlocks(entry string) []block {
	lines := strings.Split(entry, "\n")

	overridesAt := -1
	for i, l := range lines {
		if strings.TrimRight(l, " \t") == "overrides:" {
			overridesAt = i
			break
		}
	}

	var blocks []block
	if overridesAt == -1 {
		return []block{newBlock(selector{}, strings.Join(lines, "\n"))}
	}

	blocks = append(blocks, newBlock(selector{}, strings.Join(lines[:overridesAt], "\n")))

	start := -1
	for i := overridesAt + 1; i <= len(lines); i++ {
		isItem := i < len(lines) && strings.HasPrefix(lines[i], "- ")
		if isItem || i == len(lines) {
			if start != -1 {
				body := strings.Join(lines[start:i], "\n")
				blocks = append(blocks, newBlock(parseSelector(body), body))
			}
			start = i
		}
	}
	return blocks
}

func newBlock(sel selector, body string) block {
	b := block{sel: sel, body: body}
	for _, m := range libRefPattern.FindAllStringSubmatch(body, -1) {
		b.libRefs = append(b.libRefs, m[1])
	}
	return b
}

// parseSelector reads the `when:` clause of an override block. It is a hand
// parser rather than a YAML unmarshal because the surrounding block contains
// template directives that would fail to parse as a document.
func parseSelector(body string) selector {
	var s selector
	var field string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(raw, "- "))
		switch line {
		case "platform:":
			field = fieldPlatform
			continue
		case "gpuArchitecture:":
			field = fieldArch
			continue
		case "when:", "":
			continue
		}
		// Any key at the same depth as the constraints ends the `when:` clause.
		if !isConstraintKey(line) {
			if strings.HasSuffix(line, ":") {
				field = ""
			}
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch {
		case field == fieldPlatform && key == "equals":
			s.platformEquals = value
		case field == fieldArch && key == "equals":
			s.archEquals = value
		case field == fieldArch && key == "in":
			s.archIn = parseList(value)
		case field == fieldArch && key == "notIn":
			s.archNotIn = parseList(value)
		}
	}
	return s
}

// isConstraintKey reports whether line opens one of the comparison keys a
// `when:` clause uses, as opposed to a sibling key that ends the clause.
func isConstraintKey(line string) bool {
	return strings.HasPrefix(line, "equals:") ||
		strings.HasPrefix(line, "in:") ||
		strings.HasPrefix(line, "notIn:")
}

func parseList(v string) []string {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(v, "["), "]"))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// scanImages returns the image references appearing directly in body.
func scanImages(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if m := imageRefPattern.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// scanRuntimeFetches returns the network fetches body performs at pod start.
func scanRuntimeFetches(body string) ([]runtimeFetch, error) {
	clones := gitClonePattern.FindAllStringIndex(body, -1)
	downloads := fetchPattern.FindAllStringSubmatch(body, -1)

	out := make([]runtimeFetch, 0, len(clones)+len(downloads))
	for _, loc := range clones {
		c := body[loc[0]:loc[1]]
		// Only what precedes this clone can have assigned its variables. Given
		// the whole body, two clones in a block that reassigns the remote
		// between them both resolve to the last assignment, so the earlier
		// repository drops out of the closure — and silently, because
		// dedupeFetches then collapses the pair.
		url, err := cloneRemote(c, body[:loc[0]])
		if err != nil {
			return nil, err
		}
		fetch := runtimeFetch{URL: url}
		if m := cloneRefPattern.FindStringSubmatch(c); m != nil {
			fetch.Ref = m[1]
		}
		out = append(out, fetch)
	}
	for _, m := range downloads {
		out = append(out, runtimeFetch{URL: strings.TrimRight(m[1], `"'`)})
	}
	return out, nil
}

// cloneRemote resolves the remote a clone command fetches from. A literal URL
// in the command is read directly; NVCRE v0.6.0 moved it behind a shell
// variable assigned earlier in the same block, so a variable reference is
// followed to its assignment and the URL read from there — including out of
// the `{{ if .SourceRepo }}…{{ else }}<default>{{ end }}` template that makes
// the remote overridable, since the default is what a run fetches unless the
// caller overrides it.
//
// Only that default is ever recorded. The scan reads catalog templates, never
// a rendered object, so an operator's sourceRepo override cannot reach the
// committed closure — which matters because upstream documents that the
// override may carry credentials in the URL.
//
// An unresolvable remote is an error rather than an omission. Dropping the
// clone would leave the closure stating that the path needs no network at pod
// start, which is the single question it exists to answer, and a wrong answer
// there reads as an air-gap clearance the path has not earned.
func cloneRemote(clone, preceding string) (string, error) {
	if url := httpsRemote(clone); url != "" {
		return url, nil
	}

	// Last assignment wins, matching the shell: the map is built in source
	// order over the text before this clone.
	assigned := map[string]string{}
	for _, m := range shellAssignment.FindAllStringSubmatch(preceding, -1) {
		assigned[m[1]] = m[2]
	}
	for _, ref := range cloneVarPattern.FindAllStringSubmatch(clone, -1) {
		if url := httpsRemote(assigned[ref[1]]); url != "" {
			return url, nil
		}
	}

	return "", errors.WrapWithContext(errors.ErrCodeInvalidRequest,
		"cannot resolve the remote of a git clone performed at pod start; "+
			"the closure would otherwise omit it and imply the path runs disconnected", nil,
		map[string]interface{}{"clone": strings.TrimSpace(clone)})
}

// httpsRemote returns the https remote in s, preferring a .git-suffixed match.
func httpsRemote(s string) string {
	if url := cloneGitURLPattern.FindString(s); url != "" {
		return url
	}
	return cloneAnyURLPattern.FindString(s)
}

func sortedUnique(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

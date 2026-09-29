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

package uatbroker

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// maxCompatBytes bounds the compat file read; it is a small hand-edited file.
const maxCompatBytes int64 = 1 << 20 // 1 MiB

// Floor problem kinds reported by CheckFloors. Over-high and inconclusive rows
// must not be honored (the cell runs and shows its real color); an inert row
// skips nothing in the scheduled window and is informational only.
const (
	FloorProblemOverHigh     = "over-high"
	FloorProblemInconclusive = "inconclusive"
	FloorProblemInert        = "inert"
)

// Floor problem severities: an error rejects the row, a notice does not.
const (
	SeverityError  = "error"
	SeverityNotice = "notice"
)

// Floor is one row of the harness-compat floor file (tests/uat/compat.yaml):
// the minimum AICR release whose binary can pass main's tests/uat/<Lane>/**
// fixtures and harness for each listed intent.
type Floor struct {
	// Lane is a reservation cloud, which is also the tests/uat/<lane>/
	// fixture directory every reservation on that cloud shares.
	Lane       string   `yaml:"lane" json:"lane"`
	Intents    []string `yaml:"intents" json:"intents"`
	MinRelease string   `yaml:"min-release" json:"min-release"`
	Reason     string   `yaml:"reason" json:"reason"`
	// Line is the 1-based source line of the row's min-release key: the line
	// the workflow blames to find the floor-introducing commit, and the row
	// identifier --ignore-floor-lines and the --containing file refer to.
	// Zero for a Floor not produced by ParseCompat.
	Line int `yaml:"-" json:"line"`
}

// Compat is the parsed harness-compat floor file.
type Compat struct {
	Floors []Floor `yaml:"floors"`
}

// SkippedIntent records one release-cell intent dropped by a harness-compat
// floor, so the nightly planner can announce it instead of skipping silently.
type SkippedIntent struct {
	Intent string `json:"intent"`
	Floor  string `json:"floor"`
	Lane   string `json:"lane"`
	Reason string `json:"reason"`
}

// FloorProblem is one CheckFloors finding for a floor row.
type FloorProblem struct {
	Line     int      `json:"line"`
	Lane     string   `json:"lane"`
	Intents  []string `json:"intents"`
	Floor    string   `json:"floor"`
	Kind     string   `json:"kind"`
	Severity string   `json:"severity"`
	// Expected is the highest acceptable floor for an over-high row.
	Expected string `json:"expected,omitempty"`
	Message  string `json:"message"`
}

// ParseCompat strictly decodes (KnownFields) a compat document, records each
// row's min-release source line, and enforces the schema rules that need no
// registry. Callers must still run Validate against the registry.
func ParseCompat(data []byte) (*Compat, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Compat
	if err := dec.Decode(&c); err != nil {
		if stderrors.Is(err, io.EOF) {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "compat file is empty (declare `floors: []` for none)")
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse compat file", err)
	}
	if err := c.recordLines(data); err != nil {
		return nil, err
	}
	if err := c.validateSchema(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadCompatFile reads, size-bounds, and parses the compat file at path, then
// validates it against reg.
func LoadCompatFile(path string, reg *Registry) (*Compat, error) {
	data, err := readFileBounded(path, "compat file", maxCompatBytes)
	if err != nil {
		return nil, err
	}
	c, err := ParseCompat(data)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(reg); err != nil {
		return nil, err
	}
	return c, nil
}

// recordLines fills each Floor.Line from the document's node tree. The strict
// decode has already succeeded, so the tree shape is known to match.
func (c *Compat) recordLines(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "parse compat file", err)
	}
	if len(doc.Content) == 0 {
		return nil
	}
	floors := mappingValue(doc.Content[0], "floors")
	if floors == nil || floors.Kind != yaml.SequenceNode {
		return nil
	}
	for i, item := range floors.Content {
		if i >= len(c.Floors) {
			break
		}
		if v := mappingValue(item, "min-release"); v != nil {
			c.Floors[i].Line = v.Line
		}
	}
	return nil
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// validateSchema enforces the registry-independent rules: floors declared;
// per row a lane, a non-empty duplicate-free list of recognized intents, a
// stable v-prefixed semver min-release, and a non-blank reason; and each
// (lane, intent) pair at most once across rows, so no two floors compete.
func (c *Compat) validateSchema() error {
	if c.Floors == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "compat file has no floors key (declare `floors: []` for none)")
	}
	seenPair := make(map[string]int, len(c.Floors))
	for i := range c.Floors {
		f := &c.Floors[i]
		where := fmt.Sprintf("compat floor[%d] (line %d)", i, f.Line)
		if strings.TrimSpace(f.Lane) == "" {
			return errors.New(errors.ErrCodeInvalidRequest, where+" has an empty lane")
		}
		if len(f.Intents) == 0 {
			return errors.New(errors.ErrCodeInvalidRequest, where+" has no intents")
		}
		seenIntent := make(map[string]bool, len(f.Intents))
		for _, intent := range f.Intents {
			if !validIntents[intent] {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s has unknown intent %q (want %s or %s)", where, intent, IntentTraining, IntentInference))
			}
			if seenIntent[intent] {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s lists duplicate intent %q", where, intent))
			}
			seenIntent[intent] = true
			key := f.Lane + "/" + intent
			if prev, ok := seenPair[key]; ok {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s repeats %s already floored by floor[%d]; edit that row instead", where, key, prev))
			}
			seenPair[key] = i
		}
		if _, err := parseStableRelease(f.MinRelease); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, where+" has an invalid min-release", err)
		}
		if strings.TrimSpace(f.Reason) == "" {
			return errors.New(errors.ErrCodeInvalidRequest, where+" has an empty reason")
		}
	}
	return nil
}

// parseStableRelease parses a floor version: a v-prefixed semver with no
// pre-release segment (a floor names a release, never an rc).
func parseStableRelease(s string) (*semver.Version, error) {
	if !strings.HasPrefix(s, "v") {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("%q must carry the v prefix (e.g. v0.22.0)", s))
	}
	v, err := semver.StrictNewVersion(strings.TrimPrefix(s, "v"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("%q is not a semver release", s), err)
	}
	if v.Prerelease() != "" || v.Metadata() != "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("%q must be a stable release (no pre-release or build metadata)", s))
	}
	return v, nil
}

// Validate enforces the schema rules plus the registry rule: every lane must
// be a cloud at least one reservation in reg uses. The tests/uat/<lane>/
// directory check lives in the repository tests; this package has no FS root.
func (c *Compat) Validate(reg *Registry) error {
	if err := c.validateSchema(); err != nil {
		return err
	}
	if reg == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "compat validation needs a reservation registry")
	}
	clouds := make(map[string]bool, len(reg.Reservations))
	for i := range reg.Reservations {
		clouds[reg.Reservations[i].Cloud] = true
	}
	for i := range c.Floors {
		if !clouds[c.Floors[i].Lane] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("compat floor[%d] (line %d) lane %q is not the cloud of any reservation in the registry",
					i, c.Floors[i].Line, c.Floors[i].Lane))
		}
	}
	return nil
}

// FloorFor returns the floor gating intent on lane cloud. A nil Compat has
// no floors.
func (c *Compat) FloorFor(cloud, intent string) (Floor, bool) {
	if c == nil {
		return Floor{}, false
	}
	for i := range c.Floors {
		if c.Floors[i].Lane != cloud {
			continue
		}
		for _, in := range c.Floors[i].Intents {
			if in == intent {
				return c.Floors[i], true
			}
		}
	}
	return Floor{}, false
}

// WithoutLines returns a copy of c minus the rows whose min-release sits on
// one of lines — the rows the nightly planner refuses to honor because
// CheckFloors rejected them. A line matching no row is an ErrCodeInvalidRequest:
// a typo must not leave the rejected floor silently honored.
func (c *Compat) WithoutLines(lines []int) (*Compat, error) {
	drop := make(map[int]bool, len(lines))
	for _, l := range lines {
		drop[l] = true
	}
	out := &Compat{Floors: make([]Floor, 0, len(c.Floors))}
	matched := make(map[int]bool, len(lines))
	for i := range c.Floors {
		if drop[c.Floors[i].Line] {
			matched[c.Floors[i].Line] = true
			continue
		}
		out.Floors = append(out.Floors, c.Floors[i])
	}
	for _, l := range lines {
		if !matched[l] {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("line %d is not the min-release line of any compat floor", l))
		}
	}
	return out, nil
}

// CheckFloors proves no floor is over-high — the dangerous direction, since
// an over-high floor silently skips a release that would pass. Its git inputs
// are computed by the caller: tags is the raw `git tag -l 'v*'` list, and
// containing maps each floor's Line to the tags containing the commit that
// last touched that line (`git blame` + `git tag --contains`). A Line absent
// from containing means the git lookup failed; present with no tags means no
// tag contains the commit.
//
// A row is over-high when (a) a stable tag below the floor contains the
// commit (the lowest such tag is the correct floor), or (b) no stable tag
// contains it and the floor is neither an existing stable tag nor the next
// patch, minor, or major release after the latest stable tag. A row is
// inconclusive when tags holds no stable release (shallow checkout) or its
// Line is absent from containing. Under-low floors are deliberately not
// flagged: they surface as a red cell.
//
// When previousN >= 0, rows that are otherwise sound but skip nothing in the
// window of the previousN newest stable releases are reported as inert
// notices. Problems are returned in document order, at most one per row.
func CheckFloors(c *Compat, tags []string, containing map[int][]string, previousN int) []FloorProblem {
	out := []FloorProblem{}
	if c == nil {
		return out
	}
	stable := stableVersionsDescending(tags)
	stableSet := make(map[string]bool, len(stable))
	for _, v := range stable {
		stableSet[v.String()] = true
	}

	for i := range c.Floors {
		f := &c.Floors[i]
		p := FloorProblem{Line: f.Line, Lane: f.Lane, Intents: f.Intents, Floor: f.MinRelease}
		floorV, err := parseStableRelease(f.MinRelease)
		if err != nil {
			out = append(out, reject(p, FloorProblemInconclusive, "", "does not parse: "+err.Error()))
			continue
		}
		if len(stable) == 0 {
			out = append(out, reject(p, FloorProblemInconclusive, "",
				"cannot be checked: no stable release tags supplied (shallow checkout? fetch full tag history)"))
			continue
		}
		contTags, ok := containing[f.Line]
		if !ok {
			out = append(out, reject(p, FloorProblemInconclusive, "",
				fmt.Sprintf("cannot be checked: no tag-containment data for line %d (git blame / git tag --contains failed)", f.Line)))
			continue
		}

		if lowest := lowestStable(contTags); lowest != nil {
			if lowest.LessThan(floorV) {
				out = append(out, reject(p, FloorProblemOverHigh, lowest.Original(),
					fmt.Sprintf("is above %s, the lowest release that already contains the commit that set it", lowest.Original())))
				continue
			}
		} else if !stableSet[floorV.String()] {
			latest := stable[0]
			next := []semver.Version{latest.IncPatch(), latest.IncMinor(), latest.IncMajor()}
			isNext := false
			for j := range next {
				if next[j].Equal(floorV) {
					isNext = true
					break
				}
			}
			if !isNext {
				out = append(out, reject(p, FloorProblemOverHigh, "v"+next[1].String(),
					fmt.Sprintf("is neither a shipped release nor the next release after %s (want v%s, v%s, or v%s)",
						latest.Original(), next[0].String(), next[1].String(), next[2].String())))
				continue
			}
		}

		if previousN >= 0 && isInert(floorV, stable, previousN) {
			p.Kind, p.Severity = FloorProblemInert, SeverityNotice
			p.Message = fmt.Sprintf("%s/%s floor %s skips no release in the %d-release window; delete it once no window reaches below %s",
				f.Lane, strings.Join(f.Intents, ","), f.MinRelease, previousN, f.MinRelease)
			out = append(out, p)
		}
	}
	return out
}

// reject fills p as an error-severity problem.
func reject(p FloorProblem, kind, expected, why string) FloorProblem {
	p.Kind, p.Severity, p.Expected = kind, SeverityError, expected
	p.Message = fmt.Sprintf("%s/%s floor %s %s", p.Lane, strings.Join(p.Intents, ","), p.Floor, why)
	if expected != "" {
		p.Message += "; expected <= " + expected
	}
	return p
}

// isInert reports whether no release in the previousN newest stable
// versions (descending) is below floor.
func isInert(floor *semver.Version, stableDesc []*semver.Version, previousN int) bool {
	if previousN < len(stableDesc) {
		stableDesc = stableDesc[:previousN]
	}
	for _, v := range stableDesc {
		if v.LessThan(floor) {
			return false
		}
	}
	return true
}

// lowestStable returns the lowest stable semver among tags, or nil.
func lowestStable(tags []string) *semver.Version {
	stable := stableVersionsDescending(tags)
	if len(stable) == 0 {
		return nil
	}
	return stable[len(stable)-1]
}

// ParseContaining parses the tag-containment input CheckFloors consumes: one
// record per line, a floor's min-release line number followed by zero or more
// whitespace-separated tags containing the commit that last touched it
// (`<line>\t<tag>\t<tag>…`). A record with no tags means no tag contains the
// commit. Repeated line numbers are unioned; blank lines are ignored.
func ParseContaining(data []byte) (map[int][]string, error) {
	out := make(map[int][]string)
	for i, raw := range strings.Split(string(data), "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		line, err := strconv.Atoi(fields[0])
		if err != nil || line <= 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("containing record %d: %q is not a positive line number", i+1, fields[0]))
		}
		if out[line] == nil {
			out[line] = []string{} // present with no tags ≠ absent (lookup failed)
		}
		out[line] = append(out[line], fields[1:]...)
	}
	return out, nil
}

// Gate decides whether a manual dispatch of aicrVersion on (cloud, intent)
// meets its harness-compat floor. It returns a one-line verdict when allowed
// (at or above the floor, or no floor) and an ErrCodeInvalidRequest naming
// the floor and its reason when the release is below it.
func (c *Compat) Gate(cloud, intent, aicrVersion string) (string, error) {
	v, err := semver.NewVersion(aicrVersion)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("version %q is not semver", aicrVersion), err)
	}
	floor, ok := c.FloorFor(cloud, intent)
	if !ok {
		return fmt.Sprintf("ok: no harness-compat floor for %s/%s", cloud, intent), nil
	}
	floorV, err := semver.NewVersion(floor.MinRelease)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("floor %q is not semver", floor.MinRelease), err)
	}
	if v.LessThan(floorV) {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s/%s @ %s is below the harness-compat floor %s (tests/uat/compat.yaml line %d: %s)",
				cloud, intent, aicrVersion, floor.MinRelease, floor.Line, strings.Join(strings.Fields(floor.Reason), " ")))
	}
	return fmt.Sprintf("ok: %s/%s @ %s meets the harness-compat floor %s", cloud, intent, aicrVersion, floor.MinRelease), nil
}

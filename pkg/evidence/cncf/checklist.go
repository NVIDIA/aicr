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

package cncf

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Version is a CNCF AI Conformance checklist version, named after the
// Kubernetes minor it certifies ("v1.36").
type Version string

// Supported checklist versions, oldest first. LatestVersion is the default
// when a caller supplies no version and no Kubernetes version to derive one
// from. v1.38 is deliberately absent: its checklist is unpublished, and the
// announced KAR graduations (driver_runtime_management and
// workload_sandboxing to MUST) are not a schema.
const (
	Version135 Version = "v1.35"
	Version136 Version = "v1.36"
	Version137 Version = "v1.37"

	LatestVersion = Version137
)

// Level is an upstream requirement level.
type Level string

// Upstream requirement levels.
const (
	LevelMust   Level = "MUST"
	LevelShould Level = "SHOULD"
)

// Provenance of the embedded upstream checklists: the cncf/k8s-ai-conformance
// commit the copies under schemas/ were taken from, and the SHA-256 of each
// file. TestEmbeddedSchemasMatchPins fails when a copy changes without the
// pin moving, so a refresh is a deliberate, reviewed edit of both.
const upstreamSchemaCommit = "0bb5c89256ce2f748d992b0c2f0508a55316dc82"

var upstreamSchemaDigests = map[Version]string{
	Version135: "cf87915a9bcf977bb9c61298bc60afa0f062d1c3aad0a2d3dcefcdf1fa644693",
	Version136: "43d8b708956c69f8a87a426c8c883e0e845588db173d94d707209397dad06170",
	Version137: "8d56838e47e17e4ba6a5e013f16f6711a6f4180b8545cd908199231f43fa8f96",
}

//go:embed schemas/AIConformance-*.yaml
var schemaFS embed.FS

// Upstream spec categories, in document order.
const (
	categoryAccelerators            = "accelerators"
	categoryNetworking              = "networking"
	categorySchedulingOrchestration = "schedulingOrchestration"
	categoryObservability           = "observability"
	categorySecurity                = "security"
	categoryOperator                = "operator"
)

// Requirement is one row of an upstream checklist, with the upstream id,
// description and level preserved verbatim.
type Requirement struct {
	// ID is the upstream requirement identifier ("gang_scheduling").
	ID string
	// Category is the upstream spec section ("schedulingOrchestration").
	Category string
	// Description is the upstream requirement text.
	Description string
	// Level is MUST or SHOULD at this checklist version.
	Level Level
}

// Checklist is one published upstream checklist.
type Checklist struct {
	Version      Version
	Requirements []Requirement
}

// Requirement returns the row with the given upstream id, or nil.
func (c *Checklist) Requirement(id string) *Requirement {
	for i := range c.Requirements {
		if c.Requirements[i].ID == id {
			return &c.Requirements[i]
		}
	}
	return nil
}

// MustRequirements returns the MUST rows in upstream order.
func (c *Checklist) MustRequirements() []Requirement {
	out := make([]Requirement, 0, len(c.Requirements))
	for _, r := range c.Requirements {
		if r.Level == LevelMust {
			out = append(out, r)
		}
	}
	return out
}

// SupportedVersions returns the checklist versions this build renders,
// oldest first.
func SupportedVersions() []Version {
	return []Version{Version135, Version136, Version137}
}

// ParseVersion normalizes a user-supplied checklist version ("1.36",
// "v1.36") and refuses one this build does not carry.
func ParseVersion(s string) (Version, error) {
	v := Version("v" + strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if _, ok := upstreamSchemaDigests[v]; !ok {
		return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"unsupported CNCF AI Conformance checklist version %q; supported: %s",
			s, joinVersions(SupportedVersions())))
	}
	return v, nil
}

// kubernetesMinor matches a Kubernetes server version such as "v1.36.3",
// "1.36.3-eks-a1b2c3" or "v1.36".
var kubernetesMinor = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:[.\-+].*)?$`)

// VersionForKubernetes derives the checklist version from a Kubernetes server
// version string. ok is false when the string does not parse or names a minor
// this build carries no checklist for. It is an exact lookup only: a caller
// that wants the nearest carried checklist for an uncarried minor (below the
// oldest renders the oldest, above the newest renders the latest) uses
// NearestVersion, which is what evidence rendering does.
func VersionForKubernetes(serverVersion string) (v Version, ok bool) {
	m := kubernetesMinor.FindStringSubmatch(strings.TrimSpace(serverVersion))
	if m == nil {
		return "", false
	}
	v = Version("v" + m[1] + "." + m[2])
	_, ok = upstreamSchemaDigests[v]
	return v, ok
}

// NearestVersion selects the carried checklist for a Kubernetes server
// version string. exact is true when the cluster's minor has a carried
// checklist. A minor below the oldest carried checklist selects that oldest
// one (Version135), and a minor above the newest selects LatestVersion, in
// both cases with exact false so the caller can say so. Upstream publishes
// checklists this build does not carry (v1.33, v1.34), so "not carried" is
// the right reading of exact == false, not "not published".
//
// Errors:
//   - ErrCodeInvalidRequest when serverVersion is not a Kubernetes version.
func NearestVersion(serverVersion string) (v Version, exact bool, err error) {
	m := kubernetesMinor.FindStringSubmatch(strings.TrimSpace(serverVersion))
	if m == nil {
		return "", false, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%q is not a Kubernetes server version", serverVersion))
	}
	v = Version("v" + m[1] + "." + m[2])
	if _, ok := upstreamSchemaDigests[v]; ok {
		return v, true, nil
	}
	supported := SupportedVersions()
	if versionLess(v, supported[0]) {
		return supported[0], false, nil
	}
	return LatestVersion, false, nil
}

// versionLess orders checklist versions by major then minor.
func versionLess(a, b Version) bool {
	am := kubernetesMinor.FindStringSubmatch(string(a))
	bm := kubernetesMinor.FindStringSubmatch(string(b))
	if am == nil || bm == nil {
		return false
	}
	if am[1] != bm[1] {
		return len(am[1]) < len(bm[1]) || (len(am[1]) == len(bm[1]) && am[1] < bm[1])
	}
	return len(am[2]) < len(bm[2]) || (len(am[2]) == len(bm[2]) && am[2] < bm[2])
}

// ChecklistFor returns the parsed upstream checklist for v.
//
// Errors:
//   - ErrCodeInvalidRequest when v is not a supported version.
//   - ErrCodeInternal when the embedded copy does not parse (a build defect).
func ChecklistFor(v Version) (*Checklist, error) {
	if _, ok := upstreamSchemaDigests[v]; !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"unsupported CNCF AI Conformance checklist version %q; supported: %s",
			v, joinVersions(SupportedVersions())))
	}
	checklistsOnce.Do(loadChecklists)
	if checklistsErr != nil {
		return nil, checklistsErr
	}
	return checklists[v], nil
}

var (
	checklistsOnce sync.Once
	checklists     map[Version]*Checklist
	checklistsErr  error
)

// schemaFile mirrors the upstream document shape. The category fields are
// fixed, in upstream order, so a new category in a future checklist fails
// strict decoding instead of being silently dropped. Metadata is the vendor
// placeholder block; only kubernetesVersion is read.
type schemaFile struct {
	Metadata map[string]string `yaml:"metadata"`
	Spec     struct {
		Accelerators            []schemaRequirement `yaml:"accelerators"`
		Networking              []schemaRequirement `yaml:"networking"`
		SchedulingOrchestration []schemaRequirement `yaml:"schedulingOrchestration"`
		Observability           []schemaRequirement `yaml:"observability"`
		Security                []schemaRequirement `yaml:"security"`
		Operator                []schemaRequirement `yaml:"operator"`
	} `yaml:"spec"`
}

type schemaRequirement struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Level       string   `yaml:"level"`
	Status      string   `yaml:"status"`
	Evidence    []string `yaml:"evidence"`
	Notes       string   `yaml:"notes"`
}

func schemaPath(v Version) string {
	return "schemas/AIConformance-" + strings.TrimPrefix(string(v), "v") + ".yaml"
}

func loadChecklists() {
	out := make(map[Version]*Checklist, len(upstreamSchemaDigests))
	for _, v := range SupportedVersions() {
		data, err := schemaFS.ReadFile(schemaPath(v))
		if err != nil {
			checklistsErr = errors.Wrap(errors.ErrCodeInternal, "embedded CNCF checklist missing: "+schemaPath(v), err)
			return
		}
		c, err := parseChecklist(v, data, upstreamSchemaDigests[v])
		if err != nil {
			checklistsErr = err
			return
		}
		out[v] = c
	}
	checklists = out
}

// parseChecklist decodes one upstream checklist document for version v,
// refusing a digest other than wantDigest, a kubernetesVersion other than v,
// an unknown level, or an empty or duplicate id.
func parseChecklist(v Version, data []byte, wantDigest string) (*Checklist, error) {
	name := schemaPath(v)
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != wantDigest {
		return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"embedded CNCF checklist %s digest %s does not match pinned %s", name, got, wantDigest))
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f schemaFile
	if err := dec.Decode(&f); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "parse embedded CNCF checklist "+name, err)
	}
	if f.Metadata["kubernetesVersion"] != string(v) {
		return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"embedded CNCF checklist %s declares kubernetesVersion %q", name, f.Metadata["kubernetesVersion"]))
	}
	c := &Checklist{Version: v}
	seen := map[string]bool{}
	for _, cat := range []struct {
		name string
		rows []schemaRequirement
	}{
		{categoryAccelerators, f.Spec.Accelerators},
		{categoryNetworking, f.Spec.Networking},
		{categorySchedulingOrchestration, f.Spec.SchedulingOrchestration},
		{categoryObservability, f.Spec.Observability},
		{categorySecurity, f.Spec.Security},
		{categoryOperator, f.Spec.Operator},
	} {
		for _, r := range cat.rows {
			lvl := Level(r.Level)
			if lvl != LevelMust && lvl != LevelShould {
				return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"embedded CNCF checklist %s: requirement %q has level %q", name, r.ID, r.Level))
			}
			if r.ID == "" || seen[r.ID] {
				return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"embedded CNCF checklist %s: empty or duplicate requirement id %q", name, r.ID))
			}
			seen[r.ID] = true
			c.Requirements = append(c.Requirements, Requirement{
				ID: r.ID, Category: cat.name, Description: r.Description, Level: lvl,
			})
		}
	}
	if len(c.Requirements) == 0 {
		return nil, errors.New(errors.ErrCodeInternal, "embedded CNCF checklist "+name+" has no requirements")
	}
	return c, nil
}

func joinVersions(vs []Version) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}

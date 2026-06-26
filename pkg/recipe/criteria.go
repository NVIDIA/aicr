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

// Package recipe provides recipe building and matching functionality.
package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe/oskind"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"gopkg.in/yaml.v3"
)

// CriteriaAnyValue is the wildcard string used across every criteria
// dimension — recipes set a field to this literal (or leave it empty)
// to mean "this dimension is unconstrained." Each typed enum
// (CriteriaServiceAny, CriteriaAcceleratorAny, etc.) is the same
// string in its typed form; CriteriaAnyValue is the bare-string
// constant for matching logic that operates on stringified values
// (e.g., pkg/fingerprint.matchDim's three-way comparison).
const CriteriaAnyValue = "any"

// CriteriaServiceType represents the Kubernetes service/platform type for criteria.
type CriteriaServiceType string

// CriteriaServiceType constants for supported Kubernetes services.
const (
	CriteriaServiceAny  CriteriaServiceType = "any"
	CriteriaServiceEKS  CriteriaServiceType = "eks"
	CriteriaServiceGKE  CriteriaServiceType = "gke"
	CriteriaServiceAKS  CriteriaServiceType = "aks"
	CriteriaServiceOKE  CriteriaServiceType = "oke"
	CriteriaServiceKind CriteriaServiceType = "kind"
	CriteriaServiceLKE  CriteriaServiceType = "lke"
	CriteriaServiceBCM  CriteriaServiceType = "bcm"
	CriteriaServiceOCP  CriteriaServiceType = "ocp"
)

// ParseService parses a string into a CriteriaServiceType against this
// registry.
//
// The switch arms below are the canonical/aliased fast path for the
// embedded OSS catalog. Any value not recognized here falls through to
// the registry, which the data provider seeds from loaded overlays
// (embedded + `--data`). This lets internal/proprietary service values
// (e.g., undisclosed NCPs) be admitted at runtime via `--data` without a
// binary rebuild.
func (r *CriteriaRegistry) ParseService(s string) (CriteriaServiceType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CriteriaAnyValue, "self-managed", "self", "vanilla":
		return CriteriaServiceAny, nil
	case string(CriteriaServiceEKS):
		return CriteriaServiceEKS, nil
	case "gke":
		return CriteriaServiceGKE, nil
	case "aks":
		return CriteriaServiceAKS, nil
	case "oke":
		return CriteriaServiceOKE, nil
	case string(CriteriaServiceKind):
		return CriteriaServiceKind, nil
	case "lke":
		return CriteriaServiceLKE, nil
	case "bcm":
		return CriteriaServiceBCM, nil
	case "ocp", "openshift":
		return CriteriaServiceOCP, nil
	default:
		if r.Has(FieldService, s) {
			return CriteriaServiceType(normalizeCriteriaValue(s)), nil
		}
		return CriteriaServiceAny, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid service type: %s", s))
	}
}

// GetCriteriaServiceTypes returns the static OSS-embedded service types
// sorted alphabetically. This is the canonical OSS list and is stable
// across `--data` configurations; for the union of static + registry
// (including values contributed by `--data`), use AllCriteriaServiceTypes.
func GetCriteriaServiceTypes() []string {
	return []string{"aks", "bcm", "eks", "gke", "kind", "lke", "ocp", "oke"}
}

// AllServiceTypes returns the union of the static OSS list and values
// registered in this registry, sorted alphabetically.
func (r *CriteriaRegistry) AllServiceTypes() []string {
	return mergeCriteriaTypes(GetCriteriaServiceTypes(), r.Values(FieldService))
}

// CriteriaAcceleratorType represents the GPU/accelerator type.
type CriteriaAcceleratorType string

// CriteriaAcceleratorType constants for supported accelerators.
const (
	CriteriaAcceleratorAny        CriteriaAcceleratorType = "any"
	CriteriaAcceleratorH100       CriteriaAcceleratorType = "h100"
	CriteriaAcceleratorH200       CriteriaAcceleratorType = "h200"
	CriteriaAcceleratorGB200      CriteriaAcceleratorType = "gb200"
	CriteriaAcceleratorB200       CriteriaAcceleratorType = "b200"
	CriteriaAcceleratorA100       CriteriaAcceleratorType = "a100"
	CriteriaAcceleratorL40        CriteriaAcceleratorType = "l40"
	CriteriaAcceleratorRTXPro6000 CriteriaAcceleratorType = "rtx-pro-6000"
)

// ParseAccelerator parses a string into a CriteriaAcceleratorType against
// this registry. See (*CriteriaRegistry).ParseService for the
// registry-fallback contract that also applies here.
func (r *CriteriaRegistry) ParseAccelerator(s string) (CriteriaAcceleratorType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CriteriaAnyValue:
		return CriteriaAcceleratorAny, nil
	case "h100":
		return CriteriaAcceleratorH100, nil
	case "h200":
		return CriteriaAcceleratorH200, nil
	case "gb200":
		return CriteriaAcceleratorGB200, nil
	case "b200":
		return CriteriaAcceleratorB200, nil
	case "a100":
		return CriteriaAcceleratorA100, nil
	case "l40":
		return CriteriaAcceleratorL40, nil
	case "rtx-pro-6000":
		return CriteriaAcceleratorRTXPro6000, nil
	default:
		if r.Has(FieldAccelerator, s) {
			return CriteriaAcceleratorType(normalizeCriteriaValue(s)), nil
		}
		return CriteriaAcceleratorAny, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid accelerator type: %s", s))
	}
}

// GetCriteriaAcceleratorTypes returns the static OSS-embedded accelerator
// types sorted alphabetically. For the union of static + registry, use
// AllCriteriaAcceleratorTypes.
func GetCriteriaAcceleratorTypes() []string {
	return []string{"a100", "b200", "gb200", "h100", "h200", "l40", "rtx-pro-6000"}
}

// AllAcceleratorTypes returns the union of the static OSS list and values
// registered in this registry, sorted alphabetically.
func (r *CriteriaRegistry) AllAcceleratorTypes() []string {
	return mergeCriteriaTypes(GetCriteriaAcceleratorTypes(), r.Values(FieldAccelerator))
}

// CriteriaIntentType represents the workload intent.
type CriteriaIntentType string

// CriteriaIntentType constants for supported workload intents.
const (
	CriteriaIntentAny       CriteriaIntentType = "any"
	CriteriaIntentTraining  CriteriaIntentType = "training"
	CriteriaIntentInference CriteriaIntentType = "inference"
)

// ParseIntent parses a string into a CriteriaIntentType against this
// registry. See (*CriteriaRegistry).ParseService for the registry-fallback
// contract.
func (r *CriteriaRegistry) ParseIntent(s string) (CriteriaIntentType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CriteriaAnyValue:
		return CriteriaIntentAny, nil
	case "training":
		return CriteriaIntentTraining, nil
	case "inference":
		return CriteriaIntentInference, nil
	default:
		if r.Has(FieldIntent, s) {
			return CriteriaIntentType(normalizeCriteriaValue(s)), nil
		}
		return CriteriaIntentAny, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid intent type: %s", s))
	}
}

// GetCriteriaIntentTypes returns the static OSS-embedded intent types
// sorted alphabetically. For the union of static + registry, use
// AllCriteriaIntentTypes.
func GetCriteriaIntentTypes() []string {
	return []string{"inference", "training"}
}

// AllIntentTypes returns the union of the static OSS list and values
// registered in this registry, sorted alphabetically.
func (r *CriteriaRegistry) AllIntentTypes() []string {
	return mergeCriteriaTypes(GetCriteriaIntentTypes(), r.Values(FieldIntent))
}

// CriteriaOSType represents an operating system type.
type CriteriaOSType string

// CriteriaOSType constants for supported operating systems. Values come
// from pkg/recipe/oskind (the single source of truth for OS string values
// shared across pkg/recipe, pkg/collector, pkg/k8s/agent, and the CLI).
const (
	CriteriaOSAny         CriteriaOSType = oskind.Any
	CriteriaOSUbuntu      CriteriaOSType = oskind.Ubuntu
	CriteriaOSRHEL        CriteriaOSType = oskind.RHEL
	CriteriaOSCOS         CriteriaOSType = oskind.COS
	CriteriaOSAmazonLinux CriteriaOSType = oskind.AmazonLinux
	CriteriaOSTalos       CriteriaOSType = oskind.Talos
)

// ParseOS parses a string into a CriteriaOSType against this registry.
// See (*CriteriaRegistry).ParseService for the registry-fallback contract.
func (r *CriteriaRegistry) ParseOS(s string) (CriteriaOSType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CriteriaAnyValue:
		return CriteriaOSAny, nil
	case oskind.Ubuntu:
		return CriteriaOSUbuntu, nil
	case oskind.RHEL:
		return CriteriaOSRHEL, nil
	case oskind.COS:
		return CriteriaOSCOS, nil
	case oskind.AmazonLinux, "al2", "al2023":
		return CriteriaOSAmazonLinux, nil
	case oskind.Talos:
		return CriteriaOSTalos, nil
	default:
		if r.Has(FieldOS, s) {
			return CriteriaOSType(normalizeCriteriaValue(s)), nil
		}
		return CriteriaOSAny, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid os type: %s", s))
	}
}

// GetCriteriaOSTypes returns the static OSS-embedded OS types sorted
// alphabetically. Delegates to oskind.All so the list stays in sync
// with the canonical constants without duplication. For the union of
// static + registry, use AllCriteriaOSTypes.
func GetCriteriaOSTypes() []string {
	return oskind.All()
}

// AllOSTypes returns the union of the static OSS list and values registered
// in this registry, sorted alphabetically.
func (r *CriteriaRegistry) AllOSTypes() []string {
	return mergeCriteriaTypes(GetCriteriaOSTypes(), r.Values(FieldOS))
}

// CriteriaPlatformType represents a platform/framework type.
type CriteriaPlatformType string

// CriteriaPlatformType constants for supported platforms.
const (
	CriteriaPlatformAny      CriteriaPlatformType = "any"
	CriteriaPlatformDynamo   CriteriaPlatformType = "dynamo"
	CriteriaPlatformKubeflow CriteriaPlatformType = "kubeflow"
	CriteriaPlatformNIM      CriteriaPlatformType = "nim"
	CriteriaPlatformRunai    CriteriaPlatformType = "runai"
	CriteriaPlatformSlurm    CriteriaPlatformType = "slurm"
)

// ParsePlatform parses a string into a CriteriaPlatformType against this
// registry. See (*CriteriaRegistry).ParseService for the registry-fallback
// contract.
func (r *CriteriaRegistry) ParsePlatform(s string) (CriteriaPlatformType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CriteriaAnyValue:
		return CriteriaPlatformAny, nil
	case "dynamo":
		return CriteriaPlatformDynamo, nil
	case "kubeflow":
		return CriteriaPlatformKubeflow, nil
	case "nim":
		return CriteriaPlatformNIM, nil
	case "runai":
		return CriteriaPlatformRunai, nil
	case "slurm":
		return CriteriaPlatformSlurm, nil
	default:
		if r.Has(FieldPlatform, s) {
			return CriteriaPlatformType(normalizeCriteriaValue(s)), nil
		}
		return CriteriaPlatformAny, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid platform type: %s", s))
	}
}

// GetCriteriaPlatformTypes returns the static OSS-embedded platform
// types sorted alphabetically. For the union of static + registry, use
// AllCriteriaPlatformTypes.
func GetCriteriaPlatformTypes() []string {
	return []string{"dynamo", "kubeflow", "nim", "runai", "slurm"}
}

// AllPlatformTypes returns the union of the static OSS list and values
// registered in this registry, sorted alphabetically.
func (r *CriteriaRegistry) AllPlatformTypes() []string {
	return mergeCriteriaTypes(GetCriteriaPlatformTypes(), r.Values(FieldPlatform))
}

// mergeCriteriaTypes returns the deduplicated, alphabetically-sorted
// union of two value slices. Used by the AllCriteria*Types helpers to
// combine the embedded OSS list with registry-discovered values.
func mergeCriteriaTypes(staticTypes, registered []string) []string {
	if len(registered) == 0 {
		// Return a copy so callers cannot mutate the canonical static slice.
		out := make([]string, len(staticTypes))
		copy(out, staticTypes)
		return out
	}
	seen := make(map[string]struct{}, len(staticTypes)+len(registered))
	out := make([]string, 0, len(staticTypes)+len(registered))
	for _, v := range staticTypes {
		v = normalizeCriteriaValue(v)
		if _, dup := seen[v]; dup || v == "" {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range registered {
		v = normalizeCriteriaValue(v)
		if _, dup := seen[v]; dup || v == "" {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Criteria represents the input parameters for recipe matching.
// All fields are optional and default to "any" if not specified.
type Criteria struct {
	// Service is the Kubernetes service type (eks, gke, aks, oke, ocp, kind, lke, bcm).
	Service CriteriaServiceType `json:"service,omitempty" yaml:"service,omitempty"`

	// Accelerator is the GPU/accelerator type (h100, h200, gb200, b200, a100, l40, rtx-pro-6000).
	Accelerator CriteriaAcceleratorType `json:"accelerator,omitempty" yaml:"accelerator,omitempty"`

	// Intent is the workload intent (training, inference).
	Intent CriteriaIntentType `json:"intent,omitempty" yaml:"intent,omitempty"`

	// OS is the worker node operating system type.
	OS CriteriaOSType `json:"os,omitempty" yaml:"os,omitempty"`

	// Platform is the platform/framework type (dynamo, kubeflow, nim, runai, slurm).
	Platform CriteriaPlatformType `json:"platform,omitempty" yaml:"platform,omitempty"`

	// Nodes is the number of worker nodes (0 means any/unspecified).
	Nodes int `json:"nodes,omitempty" yaml:"nodes,omitempty"`
}

// NewCriteria creates a new Criteria with all fields set to "any".
func NewCriteria() *Criteria {
	return &Criteria{
		Service:     CriteriaServiceAny,
		Accelerator: CriteriaAcceleratorAny,
		Intent:      CriteriaIntentAny,
		OS:          CriteriaOSAny,
		Platform:    CriteriaPlatformAny,
		Nodes:       0,
	}
}

// Matches checks if this recipe criteria matches the given query criteria.
// Uses asymmetric matching:
//   - Query "any" (or empty) = ONLY matches recipes that are also "any"/empty for that field
//   - Recipe "any" (or empty) = wildcard (matches any query value for that field)
//   - Query specific + Recipe specific = must match exactly
//
// This ensures a generic query (e.g., accelerator=any) only matches generic recipes
// (e.g., accelerator=any), while a specific query (e.g., accelerator=gb200) can match
// both generic recipes and recipes with that specific value.
func (c *Criteria) Matches(other *Criteria) bool {
	if c == nil {
		return other == nil
	}
	if other == nil {
		return true
	}

	// Asymmetric matching for each field:
	// - If query (other) is "any"/empty → only match if recipe is also "any"/empty
	// - If recipe (c) is "any"/empty → match any query value (recipe is generic)
	// - Otherwise → must match exactly
	//
	// Note: Empty string ("") is treated as equivalent to "any" because when YAML is parsed,
	// omitted fields get the zero value ("") rather than the "any" constant.

	// Service matching
	if !MatchesCriteriaField(string(c.Service), string(other.Service)) {
		return false
	}

	// Accelerator matching
	if !MatchesCriteriaField(string(c.Accelerator), string(other.Accelerator)) {
		return false
	}

	// Intent matching
	if !MatchesCriteriaField(string(c.Intent), string(other.Intent)) {
		return false
	}

	// OS matching
	if !MatchesCriteriaField(string(c.OS), string(other.OS)) {
		return false
	}

	// Platform matching
	if !MatchesCriteriaField(string(c.Platform), string(other.Platform)) {
		return false
	}

	// Nodes: 0 means any - apply same asymmetric logic
	// Query 0 (any) → only match if recipe is also 0 (generic)
	// Recipe 0 (any) → match any query value
	if other.Nodes == 0 && c.Nodes != 0 {
		// Query is generic but recipe is specific - no match
		return false
	}
	if other.Nodes != 0 && c.Nodes != 0 && c.Nodes != other.Nodes {
		// Both specific but different values - no match
		return false
	}

	return true
}

// MatchesCriteriaField implements asymmetric matching for a single criteria field.
// Returns true if the recipe field matches the query field.
//
// Matching rules:
//   - Query is "any"/empty → only matches if recipe is also "any"/empty
//   - Recipe is "any"/empty → matches any query value (recipe is generic/wildcard)
//   - Otherwise → must match exactly
func MatchesCriteriaField(recipeValue, queryValue string) bool {
	recipeIsAny := recipeValue == CriteriaAnyValue || recipeValue == ""
	queryIsAny := queryValue == CriteriaAnyValue || queryValue == ""

	// If recipe is "any", it matches any query value (recipe is generic)
	if recipeIsAny {
		return true
	}

	// Recipe has a specific value
	// Query must also have that specific value (not "any")
	if queryIsAny {
		// Query is generic but recipe is specific - no match
		return false
	}

	// Both have specific values - must match exactly
	return recipeValue == queryValue
}

// Validate checks that all non-empty criteria fields contain valid values
// against a fresh ephemeral registry (only the hardcoded OSS fast-path
// values will validate). Use ValidateWithRegistry to honor `--data`
// overlay values.
func (c *Criteria) Validate() error {
	return c.ValidateWithRegistry(NewCriteriaRegistry())
}

// ValidateWithRegistry checks that all non-empty criteria fields contain
// valid values against reg. A nil reg falls back to a fresh ephemeral
// registry (only the hardcoded OSS fast-path values will validate).
func (c *Criteria) ValidateWithRegistry(reg *CriteriaRegistry) error {
	if reg == nil {
		reg = NewCriteriaRegistry()
	}
	if c.Service != "" {
		parsed, err := reg.ParseService(string(c.Service))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid service", err)
		}
		c.Service = parsed
	}
	if c.Accelerator != "" {
		parsed, err := reg.ParseAccelerator(string(c.Accelerator))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid accelerator", err)
		}
		c.Accelerator = parsed
	}
	if c.Intent != "" {
		parsed, err := reg.ParseIntent(string(c.Intent))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid intent", err)
		}
		c.Intent = parsed
	}
	if c.OS != "" {
		parsed, err := reg.ParseOS(string(c.OS))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid os", err)
		}
		c.OS = parsed
	}
	if c.Platform != "" {
		parsed, err := reg.ParsePlatform(string(c.Platform))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid platform", err)
		}
		c.Platform = parsed
	}
	return nil
}

// Specificity returns a score indicating how specific this criteria is.
// Higher scores mean more specific criteria (fewer "any" fields).
// Used for ordering overlay application - more specific overlays are applied later.
func (c *Criteria) Specificity() int {
	score := 0
	// Empty string is treated as equivalent to "any" because when YAML is parsed,
	// omitted fields get the zero value ("") rather than the "any" constant.
	// This is consistent with Matches() and MatchesCriteriaField().
	if c.Service != CriteriaServiceAny && c.Service != "" {
		score++
	}
	if c.Accelerator != CriteriaAcceleratorAny && c.Accelerator != "" {
		score++
	}
	if c.Intent != CriteriaIntentAny && c.Intent != "" {
		score++
	}
	if c.OS != CriteriaOSAny && c.OS != "" {
		score++
	}
	if c.Platform != CriteriaPlatformAny && c.Platform != "" {
		score++
	}
	if c.Nodes != 0 {
		score++
	}
	return score
}

// String returns a human-readable representation of the criteria.
func (c *Criteria) String() string {
	parts := []string{}
	if c.Service != CriteriaServiceAny {
		parts = append(parts, fmt.Sprintf("service=%s", c.Service))
	}
	if c.Accelerator != CriteriaAcceleratorAny {
		parts = append(parts, fmt.Sprintf("accelerator=%s", c.Accelerator))
	}
	if c.Intent != CriteriaIntentAny {
		parts = append(parts, fmt.Sprintf("intent=%s", c.Intent))
	}
	if c.OS != CriteriaOSAny {
		parts = append(parts, fmt.Sprintf("os=%s", c.OS))
	}
	if c.Platform != CriteriaPlatformAny {
		parts = append(parts, fmt.Sprintf("platform=%s", c.Platform))
	}
	if c.Nodes != 0 {
		parts = append(parts, fmt.Sprintf("nodes=%d", c.Nodes))
	}
	if len(parts) == 0 {
		return "criteria(any)"
	}
	return fmt.Sprintf("criteria(%s)", strings.Join(parts, ", "))
}

// RegistryCriteriaOption is a functional option for building a Criteria
// against an explicit *CriteriaRegistry. Unlike CriteriaOption (which
// closes over the package-global registry through the ParseCriteria*Type
// shims), a RegistryCriteriaOption resolves its enum value against the
// registry threaded in by BuildCriteriaWithRegistry, so a caller holding a
// per-provider registry (from GetCriteriaRegistryFor) builds and validates
// criteria against THAT provider's registered values.
type RegistryCriteriaOption func(reg *CriteriaRegistry, c *Criteria) error

// WithServiceRegistry sets the service type, resolving s against the
// registry threaded in by BuildCriteriaWithRegistry.
func WithServiceRegistry(s string) RegistryCriteriaOption {
	return func(reg *CriteriaRegistry, c *Criteria) error {
		st, err := reg.ParseService(s)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse service type", err)
		}
		c.Service = st
		return nil
	}
}

// WithAcceleratorRegistry sets the accelerator type, resolving s against the
// registry threaded in by BuildCriteriaWithRegistry.
func WithAcceleratorRegistry(s string) RegistryCriteriaOption {
	return func(reg *CriteriaRegistry, c *Criteria) error {
		at, err := reg.ParseAccelerator(s)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse accelerator type", err)
		}
		c.Accelerator = at
		return nil
	}
}

// WithIntentRegistry sets the intent type, resolving s against the registry
// threaded in by BuildCriteriaWithRegistry.
func WithIntentRegistry(s string) RegistryCriteriaOption {
	return func(reg *CriteriaRegistry, c *Criteria) error {
		it, err := reg.ParseIntent(s)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse intent type", err)
		}
		c.Intent = it
		return nil
	}
}

// WithOSRegistry sets the OS type, resolving s against the registry threaded
// in by BuildCriteriaWithRegistry.
func WithOSRegistry(s string) RegistryCriteriaOption {
	return func(reg *CriteriaRegistry, c *Criteria) error {
		ot, err := reg.ParseOS(s)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse OS type", err)
		}
		c.OS = ot
		return nil
	}
}

// WithPlatformRegistry sets the platform type, resolving s against the
// registry threaded in by BuildCriteriaWithRegistry.
func WithPlatformRegistry(s string) RegistryCriteriaOption {
	return func(reg *CriteriaRegistry, c *Criteria) error {
		pt, err := reg.ParsePlatform(s)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse platform type", err)
		}
		c.Platform = pt
		return nil
	}
}

// WithNodesRegistry sets the number of nodes. The registry is unused (node
// count is not a registry dimension) but the signature matches the other
// RegistryCriteriaOption builders so all fields compose uniformly through
// BuildCriteriaWithRegistry.
func WithNodesRegistry(n int) RegistryCriteriaOption {
	return func(_ *CriteriaRegistry, c *Criteria) error {
		if n < 0 {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid nodes count: %d (must be >= 0)", n))
		}
		c.Nodes = n
		return nil
	}
}

// BuildCriteriaWithRegistry creates a Criteria from registry-aware options,
// resolving each field against the supplied registry. This is the path
// per-provider callers (e.g., the CLI holding a registry from
// GetCriteriaRegistryFor) use to build and validate criteria against a
// specific provider's registered values rather than the package global.
//
// A nil reg falls back to a fresh ephemeral registry (NewCriteriaRegistry)
// so the call is still well-defined for callers that have not yet bound a
// provider — only the hardcoded OSS fast-path values will validate.
func BuildCriteriaWithRegistry(reg *CriteriaRegistry, opts ...RegistryCriteriaOption) (*Criteria, error) {
	if reg == nil {
		reg = NewCriteriaRegistry()
	}
	c := NewCriteria()
	for _, opt := range opts {
		if err := opt(reg, c); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to apply criteria option", err)
		}
	}
	return c, nil
}

// ParseCriteriaFromRequest parses recipe criteria from HTTP query parameters,
// validating each enum value against reg so non-OSS values contributed by a
// `--data` overlay are honored. A nil reg falls back to a fresh ephemeral
// registry (only the hardcoded OSS fast-path values will validate).
// All parameters are optional and default to "any" if not specified.
// Supported parameters: service, accelerator (alias: gpu), intent, os, platform, nodes.
func ParseCriteriaFromRequest(r *http.Request, reg *CriteriaRegistry) (*Criteria, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "request cannot be nil")
	}

	q := r.URL.Query()
	return ParseCriteriaFromValues(q, reg)
}

// ParseCriteriaFromValues parses recipe criteria from URL values,
// validating each enum value against reg (a nil reg falls back to a fresh
// ephemeral registry — only hardcoded OSS values will validate).
// All parameters are optional and default to "any" if not specified.
// Supported parameters: service, accelerator (alias: gpu), intent, os, platform, nodes.
func ParseCriteriaFromValues(values url.Values, reg *CriteriaRegistry) (*Criteria, error) {
	if reg == nil {
		reg = NewCriteriaRegistry()
	}
	c := NewCriteria()

	// Parse service
	if s := values.Get("service"); s != "" {
		st, err := reg.ParseService(s)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid service parameter", err)
		}
		c.Service = st
	}

	// Parse accelerator (also accept "gpu" as alias for backwards compatibility)
	accelParam := values.Get("accelerator")
	if accelParam == "" {
		accelParam = values.Get("gpu")
	}
	if accelParam != "" {
		at, err := reg.ParseAccelerator(accelParam)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid accelerator parameter", err)
		}
		c.Accelerator = at
	}

	// Parse intent
	if s := values.Get("intent"); s != "" {
		it, err := reg.ParseIntent(s)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid intent parameter", err)
		}
		c.Intent = it
	}

	// Parse OS
	if s := values.Get("os"); s != "" {
		ot, err := reg.ParseOS(s)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid os parameter", err)
		}
		c.OS = ot
	}

	// Parse platform
	if s := values.Get("platform"); s != "" {
		pt, err := reg.ParsePlatform(s)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid platform parameter", err)
		}
		c.Platform = pt
	}

	// Parse nodes count
	if s := values.Get("nodes"); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid nodes value: %s", s))
		}
		if n < 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid nodes count: %d (must be >= 0)", n))
		}
		c.Nodes = n
	}

	return c, nil
}

// RecipeCriteriaKind is the kind value for RecipeCriteria resources.
const RecipeCriteriaKind = "RecipeCriteria"

// RecipeCriteriaAPIVersion is the API version for RecipeCriteria resources.
// It aliases RecipeAPIVersion (ultimately header.GroupVersion) so every AICR
// artifact apiVersion has a single source of truth.
const RecipeCriteriaAPIVersion = RecipeAPIVersion

// RecipeCriteria represents a Kubernetes-style criteria resource.
// This is the format used in criteria files and API requests.
//
// Example:
//
//	kind: RecipeCriteria
//	apiVersion: aicr.nvidia.com/v1alpha1
//	metadata:
//	  name: gb200-eks-ubuntu-training
//	spec:
//	  service: eks
//	  os: ubuntu
//	  accelerator: gb200
//	  intent: training
type RecipeCriteria struct {
	// Kind is always "RecipeCriteria".
	Kind string `json:"kind" yaml:"kind"`

	// APIVersion is the API version (e.g., "aicr.nvidia.com/v1alpha1").
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`

	// Metadata contains the name and other metadata.
	Metadata struct {
		// Name is the unique identifier for this criteria set.
		Name string `json:"name" yaml:"name"`
	} `json:"metadata" yaml:"metadata"`

	// Spec contains the actual criteria specification.
	Spec *Criteria `json:"spec" yaml:"spec"`
}

// rawCriteriaSpec is an intermediate struct for parsing criteria spec with string enum values.
// This allows validation through Parse* functions before creating the typed Criteria.
type rawCriteriaSpec struct {
	Service     string `json:"service,omitempty" yaml:"service,omitempty"`
	Accelerator string `json:"accelerator,omitempty" yaml:"accelerator,omitempty"`
	Intent      string `json:"intent,omitempty" yaml:"intent,omitempty"`
	OS          string `json:"os,omitempty" yaml:"os,omitempty"`
	Platform    string `json:"platform,omitempty" yaml:"platform,omitempty"`
	Nodes       int    `json:"nodes,omitempty" yaml:"nodes,omitempty"`
}

// rawRecipeCriteria is for parsing RecipeCriteria with string enum values in spec.
type rawRecipeCriteria struct {
	Kind       string `json:"kind" yaml:"kind"`
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Metadata   struct {
		Name string `json:"name" yaml:"name"`
	} `json:"metadata" yaml:"metadata"`
	Spec rawCriteriaSpec `json:"spec" yaml:"spec"`
}

// validateAndConvertRawSpec validates raw string values and converts to typed
// Criteria, resolving each enum against reg. A nil reg falls back to a fresh
// ephemeral registry — only the hardcoded OSS fast-path values will validate.
func validateAndConvertRawSpec(raw *rawCriteriaSpec, reg *CriteriaRegistry) (*Criteria, error) {
	if reg == nil {
		reg = NewCriteriaRegistry()
	}
	c := NewCriteria()

	if raw.Service != "" {
		st, err := reg.ParseService(raw.Service)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid service in criteria spec", err)
		}
		c.Service = st
	}

	if raw.Accelerator != "" {
		at, err := reg.ParseAccelerator(raw.Accelerator)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid accelerator in criteria spec", err)
		}
		c.Accelerator = at
	}

	if raw.Intent != "" {
		it, err := reg.ParseIntent(raw.Intent)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid intent in criteria spec", err)
		}
		c.Intent = it
	}

	if raw.OS != "" {
		ot, err := reg.ParseOS(raw.OS)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid os in criteria spec", err)
		}
		c.OS = ot
	}

	if raw.Platform != "" {
		pt, err := reg.ParsePlatform(raw.Platform)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid platform in criteria spec", err)
		}
		c.Platform = pt
	}

	if raw.Nodes < 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid nodes count: %d (must be >= 0)", raw.Nodes))
	}
	c.Nodes = raw.Nodes

	return c, nil
}

// LoadCriteriaFromFile loads criteria from a YAML or JSON file.
// The file format is auto-detected from the file extension.
// All fields are optional and default to "any" if not specified.
//
// Example file (YAML):
//
//	kind: RecipeCriteria
//	apiVersion: aicr.nvidia.com/v1alpha1
//	metadata:
//	  name: gb200-eks-ubuntu-training
//	spec:
//	  service: eks
//	  os: ubuntu
//	  accelerator: gb200
//	  intent: training
func LoadCriteriaFromFile(path string, reg *CriteriaRegistry) (*Criteria, error) {
	raw, err := serializer.FromFile[rawRecipeCriteria](path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load criteria file", err)
	}

	// Validate kind and apiVersion
	if raw.Kind != "" && raw.Kind != RecipeCriteriaKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid kind %q, expected %q", raw.Kind, RecipeCriteriaKind))
	}
	if raw.APIVersion != "" && raw.APIVersion != RecipeCriteriaAPIVersion {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q, expected %q", raw.APIVersion, RecipeCriteriaAPIVersion))
	}

	return validateAndConvertRawSpec(&raw.Spec, reg)
}

// LoadCriteriaFromFileWithContext loads criteria from a YAML or JSON file with context support.
// The file format is auto-detected from the file extension.
// All fields are optional and default to "any" if not specified.
//
// For HTTP/HTTPS URLs, the context is used for timeout and cancellation.
// For local file paths, the context is currently not used but is accepted for API consistency.
//
// Example file (YAML):
//
//	kind: RecipeCriteria
//	apiVersion: aicr.nvidia.com/v1alpha1
//	metadata:
//	  name: gb200-eks-ubuntu-training
//	spec:
//	  service: eks
//	  os: ubuntu
//	  accelerator: gb200
//	  intent: training
func LoadCriteriaFromFileWithContext(ctx context.Context, path string, reg *CriteriaRegistry) (*Criteria, error) {
	// For HTTP URLs, we need to use context-aware download
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return loadCriteriaFromHTTPWithContext(ctx, path, reg)
	}

	// For local files, use the existing FromFile which doesn't need context.
	// FromFile returns coded errors (NotFound for missing path, InvalidRequest
	// for parse failures); preserve the inner code rather than re-wrapping.
	//nolint:contextcheck // Local file reads don't require context; HTTP paths use loadCriteriaFromHTTPWithContext
	raw, err := serializer.FromFile[rawRecipeCriteria](path)
	if err != nil {
		return nil, err
	}

	// Validate kind and apiVersion
	if raw.Kind != "" && raw.Kind != RecipeCriteriaKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid kind %q, expected %q", raw.Kind, RecipeCriteriaKind))
	}
	if raw.APIVersion != "" && raw.APIVersion != RecipeCriteriaAPIVersion {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q, expected %q", raw.APIVersion, RecipeCriteriaAPIVersion))
	}

	return validateAndConvertRawSpec(&raw.Spec, reg)
}

// loadCriteriaFromHTTPWithContext loads criteria from an HTTP/HTTPS URL with context support.
func loadCriteriaFromHTTPWithContext(ctx context.Context, url string, reg *CriteriaRegistry) (*Criteria, error) {
	httpReader := serializer.NewHTTPReader()
	data, err := httpReader.ReadWithContext(ctx, url)
	if err != nil {
		// ReadWithContext returns properly-coded errors: ErrCodeInvalidRequest
		// for oversized bodies, ErrCodeUnavailable for transport failures.
		// Preserve the inner code rather than overwriting it.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeUnavailable, "failed to read criteria from URL")
	}

	// Determine format from URL extension
	format := serializer.FormatFromPath(url)
	reader, err := serializer.NewReader(format, strings.NewReader(string(data)))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "failed to create reader for criteria data")
	}
	defer reader.Close()

	var raw rawRecipeCriteria
	if err := reader.Deserialize(&raw); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "failed to deserialize criteria")
	}

	// Validate kind and apiVersion
	if raw.Kind != "" && raw.Kind != RecipeCriteriaKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid kind %q, expected %q", raw.Kind, RecipeCriteriaKind))
	}
	if raw.APIVersion != "" && raw.APIVersion != RecipeCriteriaAPIVersion {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q, expected %q", raw.APIVersion, RecipeCriteriaAPIVersion))
	}

	return validateAndConvertRawSpec(&raw.Spec, reg)
}

// ParseCriteriaFromBody parses criteria from an io.Reader (HTTP request body).
// Supports JSON and YAML based on the Content-Type header.
// All fields are optional and default to "any" if not specified.
//
// Supported Content-Types:
//   - application/json
//   - application/x-yaml, application/yaml, text/yaml
//
// If Content-Type is empty or unrecognized, JSON is assumed.
//
// Example JSON body:
//
//	{
//	  "kind": "RecipeCriteria",
//	  "apiVersion": "aicr.nvidia.com/v1alpha1",
//	  "metadata": {"name": "my-criteria"},
//	  "spec": {"service": "eks", "accelerator": "h100"}
//	}
func ParseCriteriaFromBody(body io.Reader, contentType string, reg *CriteriaRegistry) (*Criteria, error) {
	if body == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "request body cannot be nil")
	}

	limited := io.LimitReader(body, defaults.MaxRecipePOSTBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read request body", err)
	}
	if int64(len(data)) > defaults.MaxRecipePOSTBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("request body exceeds %d bytes", defaults.MaxRecipePOSTBytes))
	}

	if len(data) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "request body is empty")
	}

	var raw rawRecipeCriteria

	// Determine format from Content-Type header
	ct := strings.ToLower(strings.TrimSpace(contentType))
	// Extract media type (strip charset and other params)
	if idx := strings.Index(ct, ";"); idx != -1 {
		ct = strings.TrimSpace(ct[:idx])
	}

	switch ct {
	case "application/x-yaml", "application/yaml", "text/yaml":
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse YAML body", err)
		}
	case "application/json", "":
		// Default to JSON for empty or unrecognized content type
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse JSON body", err)
		}
	default:
		// Try JSON first for unrecognized types
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported content type %q and failed to parse as JSON", contentType), err)
		}
	}

	// Validate kind and apiVersion
	if raw.Kind != "" && raw.Kind != RecipeCriteriaKind {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid kind %q, expected %q", raw.Kind, RecipeCriteriaKind))
	}
	if raw.APIVersion != "" && raw.APIVersion != RecipeCriteriaAPIVersion {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q, expected %q", raw.APIVersion, RecipeCriteriaAPIVersion))
	}

	return validateAndConvertRawSpec(&raw.Spec, reg)
}

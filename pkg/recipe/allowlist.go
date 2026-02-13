// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package recipe

import (
	"log/slog"
	"os"
	"slices"
	"strings"

	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
)

// Environment variable names for allowlist configuration.
const (
	EnvAllowedAccelerators = "Eidos_ALLOWED_ACCELERATORS"
	EnvAllowedServices     = "Eidos_ALLOWED_SERVICES"
	EnvAllowedIntents      = "Eidos_ALLOWED_INTENTS"
	EnvAllowedOSTypes      = "Eidos_ALLOWED_OS"
)

// AllowLists defines which criteria values are permitted for API requests.
// An empty or nil slice means all values are allowed for that criteria type.
// This is used by the API server to restrict which values can be requested,
// while the CLI always allows all values.
type AllowLists struct {
	// Accelerators is the list of allowed accelerator types (e.g., "h100", "l40").
	// If empty, all accelerator types are allowed.
	Accelerators []CriteriaAcceleratorType

	// Services is the list of allowed service types (e.g., "eks", "gke").
	// If empty, all service types are allowed.
	Services []CriteriaServiceType

	// Intents is the list of allowed intent types (e.g., "training", "inference").
	// If empty, all intent types are allowed.
	Intents []CriteriaIntentType

	// OSTypes is the list of allowed OS types (e.g., "ubuntu", "rhel").
	// If empty, all OS types are allowed.
	OSTypes []CriteriaOSType
}

// IsEmpty returns true if no allowlists are configured (all values allowed).
func (a *AllowLists) IsEmpty() bool {
	if a == nil {
		return true
	}
	return len(a.Accelerators) == 0 &&
		len(a.Services) == 0 &&
		len(a.Intents) == 0 &&
		len(a.OSTypes) == 0
}

// AcceleratorStrings returns the allowed accelerator types as strings.
func (a *AllowLists) AcceleratorStrings() []string {
	if a == nil {
		return nil
	}
	return typesToStrings(a.Accelerators)
}

// ServiceStrings returns the allowed service types as strings.
func (a *AllowLists) ServiceStrings() []string {
	if a == nil {
		return nil
	}
	return typesToStrings(a.Services)
}

// IntentStrings returns the allowed intent types as strings.
func (a *AllowLists) IntentStrings() []string {
	if a == nil {
		return nil
	}
	return typesToStrings(a.Intents)
}

// OSTypeStrings returns the allowed OS types as strings.
func (a *AllowLists) OSTypeStrings() []string {
	if a == nil {
		return nil
	}
	return typesToStrings(a.OSTypes)
}

// ValidateCriteria checks if the given criteria values are permitted by the allowlists.
// Returns nil if validation passes, or an error with details about what value is not allowed.
// The "any" value is always allowed regardless of the allowlist configuration.
func (a *AllowLists) ValidateCriteria(c *Criteria) error {
	if a == nil || c == nil {
		return nil
	}

	slog.Debug("evaluating criteria against allowlists",
		"criteria_accelerator", string(c.Accelerator),
		"criteria_service", string(c.Service),
		"criteria_intent", string(c.Intent),
		"criteria_os", string(c.OS),
		"allowed_accelerators", a.AcceleratorStrings(),
		"allowed_services", a.ServiceStrings(),
		"allowed_intents", a.IntentStrings(),
		"allowed_os_types", a.OSTypeStrings(),
	)

	// Check accelerator
	if len(a.Accelerators) > 0 && c.Accelerator != CriteriaAcceleratorAny {
		if !slices.Contains(a.Accelerators, c.Accelerator) {
			return eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"accelerator type not allowed",
				nil,
				map[string]any{
					"requested": string(c.Accelerator),
					"allowed":   typesToStrings(a.Accelerators),
				},
			)
		}
	}

	// Check service
	if len(a.Services) > 0 && c.Service != CriteriaServiceAny {
		if !slices.Contains(a.Services, c.Service) {
			return eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"service type not allowed",
				nil,
				map[string]any{
					"requested": string(c.Service),
					"allowed":   typesToStrings(a.Services),
				},
			)
		}
	}

	// Check intent
	if len(a.Intents) > 0 && c.Intent != CriteriaIntentAny {
		if !slices.Contains(a.Intents, c.Intent) {
			return eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"intent type not allowed",
				nil,
				map[string]any{
					"requested": string(c.Intent),
					"allowed":   typesToStrings(a.Intents),
				},
			)
		}
	}

	// Check OS
	if len(a.OSTypes) > 0 && c.OS != CriteriaOSAny {
		if !slices.Contains(a.OSTypes, c.OS) {
			return eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"OS type not allowed",
				nil,
				map[string]any{
					"requested": string(c.OS),
					"allowed":   typesToStrings(a.OSTypes),
				},
			)
		}
	}

	return nil
}

// ParseAllowListsFromEnv parses allowlist configuration from environment variables.
// Returns nil if no allowlist environment variables are set.
// Environment variables:
//   - Eidos_ALLOWED_ACCELERATORS: comma-separated list of accelerator types (e.g., "h100,l40")
//   - Eidos_ALLOWED_SERVICES: comma-separated list of service types (e.g., "eks,gke")
//   - Eidos_ALLOWED_INTENTS: comma-separated list of intent types (e.g., "training,inference")
//   - Eidos_ALLOWED_OS: comma-separated list of OS types (e.g., "ubuntu,rhel")
//
// Invalid values in the environment variables are skipped with a warning logged.
func ParseAllowListsFromEnv() (*AllowLists, error) {
	al := &AllowLists{}

	// Parse accelerators
	if v := os.Getenv(EnvAllowedAccelerators); v != "" {
		accelerators, err := parseTypeList(v, ParseCriteriaAcceleratorType, CriteriaAcceleratorAny)
		if err != nil {
			return nil, eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"invalid "+EnvAllowedAccelerators,
				err,
				map[string]any{"value": v},
			)
		}
		al.Accelerators = accelerators
	}

	// Parse services
	if v := os.Getenv(EnvAllowedServices); v != "" {
		services, err := parseTypeList(v, ParseCriteriaServiceType, CriteriaServiceAny)
		if err != nil {
			return nil, eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"invalid "+EnvAllowedServices,
				err,
				map[string]any{"value": v},
			)
		}
		al.Services = services
	}

	// Parse intents
	if v := os.Getenv(EnvAllowedIntents); v != "" {
		intents, err := parseTypeList(v, ParseCriteriaIntentType, CriteriaIntentAny)
		if err != nil {
			return nil, eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"invalid "+EnvAllowedIntents,
				err,
				map[string]any{"value": v},
			)
		}
		al.Intents = intents
	}

	// Parse OS types
	if v := os.Getenv(EnvAllowedOSTypes); v != "" {
		osTypes, err := parseTypeList(v, ParseCriteriaOSType, CriteriaOSAny)
		if err != nil {
			return nil, eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
				"invalid "+EnvAllowedOSTypes,
				err,
				map[string]any{"value": v},
			)
		}
		al.OSTypes = osTypes
	}

	// Return nil if no allowlists configured (empty struct means all allowed)
	if al.IsEmpty() {
		return nil, nil //nolint:nilnil // nil allowlist means all values allowed, not an error
	}

	return al, nil
}

func parseTypeList[T ~string](s string, parse func(string) (T, error), anyVal T) ([]T, error) {
	var result []T
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		parsed, err := parse(v)
		if err != nil {
			return nil, err
		}
		if parsed != anyVal {
			result = append(result, parsed)
		}
	}
	return result, nil
}

func typesToStrings[T ~string](types []T) []string {
	result := make([]string, len(types))
	for i, t := range types {
		result[i] = string(t)
	}
	return result
}

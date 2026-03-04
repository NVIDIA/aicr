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

package validations

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

var (
	registry     map[string]ValidationFunc
	registryOnce sync.Once
	registryMu   sync.RWMutex
)

// initRegistry initializes the validation function registry.
// Functions are auto-registered via init() functions in their respective files.
func initRegistry() {
	registry = make(map[string]ValidationFunc)
	// Functions are registered via init() functions in check files
	// This ensures automatic discovery when new validation functions are added
}

// getRegistry returns the validation function registry, initializing it if needed.
func getRegistry() map[string]ValidationFunc {
	registryOnce.Do(initRegistry)
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry
}

// Register adds a validation function to the registry.
// This allows components to register custom validation functions.
// It's also called from init() functions in check files for auto-registration.
func Register(name string, fn ValidationFunc) {
	registryOnce.Do(initRegistry)
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		slog.Warn("validation function already registered, overwriting",
			"name", name,
		)
	}
	registry[name] = fn
}

// Get returns a validation function by name.
// Returns nil if the function is not found.
func Get(name string) ValidationFunc {
	reg := getRegistry()
	return reg[name]
}

// GetAll returns all registered validation function names.
func GetAll() []string {
	reg := getRegistry()
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	return names
}

// RunValidations executes all validations for a component and returns warnings and errors.
// The optional message from the validation config is appended to each warning/error.
// Severity determines whether check results become warnings or errors.
func RunValidations(ctx context.Context, componentName string, validations []recipe.ComponentValidationConfig, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config) (warnings []string, errors []error) {
	for _, validation := range validations {
		if err := ctx.Err(); err != nil {
			return warnings, append(errors, aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, "context cancelled during validation", err))
		}

		fn := Get(validation.Function)
		if fn == nil {
			slog.Warn("unknown validation function",
				"component", componentName,
				"function", validation.Function,
			)
			continue
		}

		// Execute validation function
		checkWarnings, checkErrors := fn(ctx, componentName, recipeResult, bundlerConfig, validation.Conditions)

		// Process results based on severity
		// If severity is "error", convert warnings to errors
		// If severity is "warning", keep as warnings
		severity := strings.ToLower(validation.Severity)
		if severity == "error" {
			// Convert all check results to errors
			for _, warning := range checkWarnings {
				msg := warning
				if validation.Message != "" {
					msg = warning + ". " + validation.Message
				}
				errors = append(errors, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, msg))
			}
			for _, err := range checkErrors {
				if validation.Message != "" {
					errors = append(errors, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, validation.Message, err))
				} else {
					errors = append(errors, err)
				}
			}
		} else {
			// Default to warning severity
			for _, warning := range checkWarnings {
				if validation.Message != "" {
					warnings = append(warnings, fmt.Sprintf("%s. %s", warning, validation.Message))
				} else {
					warnings = append(warnings, warning)
				}
			}
			// Even if severity is warning, checkErrors should still be errors
			for _, err := range checkErrors {
				if validation.Message != "" {
					errors = append(errors, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, validation.Message, err))
				} else {
					errors = append(errors, err)
				}
			}
		}
	}

	return warnings, errors
}

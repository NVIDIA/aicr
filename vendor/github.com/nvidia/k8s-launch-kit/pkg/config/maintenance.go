// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	defaultMaxParallelOperations         int32 = 4
	defaultMaxUnavailable                int32 = 4
	defaultMaxNodeMaintenanceTimeSeconds int32 = 3600
	defaultMaxParallelUpgrades                 = 4
)

// IntOrPercent is a YAML scalar that accepts either an integer or a percentage
// string. It mirrors the Kubernetes int-or-string fields used by the
// Maintenance Operator and SR-IOV Operator CRDs while keeping template output
// scalar (for example, 4 or 25%) rather than exposing IntOrString internals.
type IntOrPercent intstr.IntOrString

// IntOrPercentFromInt32 creates an integer IntOrPercent value.
func IntOrPercentFromInt32(value int32) *IntOrPercent {
	result := IntOrPercent(intstr.FromInt32(value))
	return &result
}

// IntOrPercentFromString creates a string IntOrPercent value. Validation by
// NormalizeMaintenance requires the string to be a percentage in [1%, 100%].
func IntOrPercentFromString(value string) *IntOrPercent {
	result := IntOrPercent(intstr.FromString(value))
	return &result
}

// String returns the scalar representation used by Go templates.
func (value IntOrPercent) String() string {
	converted := intstr.IntOrString(value)
	return converted.String()
}

// MarshalYAML preserves the int-or-percentage value as a YAML scalar.
func (value IntOrPercent) MarshalYAML() (interface{}, error) {
	converted := intstr.IntOrString(value)
	switch converted.Type {
	case intstr.Int:
		return converted.IntVal, nil
	case intstr.String:
		return converted.StrVal, nil
	default:
		return nil, fmt.Errorf("invalid IntOrPercent type %d", converted.Type)
	}
}

// UnmarshalYAML accepts only integer and string YAML scalars. Semantic
// validation (non-negative integers and bounded percentages) is performed by
// NormalizeMaintenance because the minimum differs between fields.
func (value *IntOrPercent) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar interface{}
	if err := unmarshal(&scalar); err != nil {
		return err
	}

	switch typed := scalar.(type) {
	case int:
		if typed < math.MinInt32 || typed > math.MaxInt32 {
			return fmt.Errorf("integer %d is outside the supported int32 range", typed)
		}
		*value = IntOrPercent(intstr.FromInt32(int32(typed)))
	case string:
		*value = IntOrPercent(intstr.FromString(typed))
	default:
		return fmt.Errorf("must be an integer or percentage string, got %T", scalar)
	}

	return nil
}

// MaintenanceConfig controls the concurrency policies used while operators
// make disruptive changes to nodes. Pointer fields distinguish an omitted key
// (which receives a default) from an explicit zero (which is preserved).
type MaintenanceConfig struct {
	MaxParallelOperations         *IntOrPercent `yaml:"maxParallelOperations,omitempty"`
	MaxUnavailable                *IntOrPercent `yaml:"maxUnavailable,omitempty"`
	MaxNodeMaintenanceTimeSeconds *int32        `yaml:"maxNodeMaintenanceTimeSeconds,omitempty"`
	MaxParallelUpgrades           *int          `yaml:"maxParallelUpgrades,omitempty"`
}

// DefaultMaintenanceConfig returns a fresh maintenance configuration. The
// caller may mutate it without affecting subsequent callers.
func DefaultMaintenanceConfig() *MaintenanceConfig {
	maxNodeMaintenanceTimeSeconds := defaultMaxNodeMaintenanceTimeSeconds
	maxParallelUpgrades := defaultMaxParallelUpgrades
	return &MaintenanceConfig{
		MaxParallelOperations:         IntOrPercentFromInt32(defaultMaxParallelOperations),
		MaxUnavailable:                IntOrPercentFromInt32(defaultMaxUnavailable),
		MaxNodeMaintenanceTimeSeconds: &maxNodeMaintenanceTimeSeconds,
		MaxParallelUpgrades:           &maxParallelUpgrades,
	}
}

// NormalizeMaintenance fills omitted maintenance fields and validates the
// result. Call it for programmatically-created LaunchKitConfig values before a
// profile template dereferences Maintenance.
func NormalizeMaintenance(config *LaunchKitConfig) error {
	if config == nil {
		return fmt.Errorf("launch kit config must not be nil")
	}

	defaults := DefaultMaintenanceConfig()
	if config.Maintenance == nil {
		config.Maintenance = defaults
	} else {
		if config.Maintenance.MaxParallelOperations == nil {
			config.Maintenance.MaxParallelOperations = defaults.MaxParallelOperations
		}
		if config.Maintenance.MaxUnavailable == nil {
			config.Maintenance.MaxUnavailable = defaults.MaxUnavailable
		}
		if config.Maintenance.MaxNodeMaintenanceTimeSeconds == nil {
			config.Maintenance.MaxNodeMaintenanceTimeSeconds = defaults.MaxNodeMaintenanceTimeSeconds
		}
		if config.Maintenance.MaxParallelUpgrades == nil {
			config.Maintenance.MaxParallelUpgrades = defaults.MaxParallelUpgrades
		}
	}

	return validateMaintenance(config.Maintenance)
}

func validateMaintenance(config *MaintenanceConfig) error {
	if err := validateIntOrPercent(
		"maintenance.maxParallelOperations", config.MaxParallelOperations, false); err != nil {
		return err
	}
	if err := validateIntOrPercent("maintenance.maxUnavailable", config.MaxUnavailable, true); err != nil {
		return err
	}
	if *config.MaxNodeMaintenanceTimeSeconds < 0 {
		return fmt.Errorf("maintenance.maxNodeMaintenanceTimeSeconds must be >= 0, got %d",
			*config.MaxNodeMaintenanceTimeSeconds)
	}
	if *config.MaxParallelUpgrades < 0 {
		return fmt.Errorf("maintenance.maxParallelUpgrades must be >= 0, got %d",
			*config.MaxParallelUpgrades)
	}
	return nil
}

func validateIntOrPercent(field string, value *IntOrPercent, allowZero bool) error {
	if value == nil {
		return fmt.Errorf("%s must be set", field)
	}

	converted := intstr.IntOrString(*value)
	switch converted.Type {
	case intstr.Int:
		minimum := int32(1)
		if allowZero {
			minimum = 0
		}
		if converted.IntVal < minimum {
			if allowZero {
				return fmt.Errorf("%s must be >= 0, got %d", field, converted.IntVal)
			}
			return fmt.Errorf("%s must be > 0, got %d", field, converted.IntVal)
		}
		return nil

	case intstr.String:
		if !strings.HasSuffix(converted.StrVal, "%") {
			return fmt.Errorf("%s string value must be a percentage, got %q", field, converted.StrVal)
		}
		percentageText := strings.TrimSuffix(converted.StrVal, "%")
		percentage, err := strconv.Atoi(percentageText)
		if err != nil {
			return fmt.Errorf("%s has invalid percentage %q: %w", field, converted.StrVal, err)
		}
		if percentage < 1 || percentage > 100 {
			return fmt.Errorf("%s percentage must be between 1%% and 100%%, got %q", field, converted.StrVal)
		}
		return nil

	default:
		return fmt.Errorf("%s has invalid int-or-percent type %d", field, converted.Type)
	}
}

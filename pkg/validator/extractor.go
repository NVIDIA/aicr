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

package validator

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// ConstraintPath represents a parsed fully qualified constraint path.
// Format: {Type}.{Subtype}.{Key}
// Example: "K8s.server.version" -> Type="K8s", Subtype="server", Key="version"
type ConstraintPath struct {
	Type    measurement.Type
	Subtype string
	Key     string
}

// ParseConstraintPath parses a fully qualified constraint path.
// The path format is: {Type}.{Subtype}.{Key}
// The key portion may contain dots (e.g., "/proc/sys/kernel/osrelease").
func ParseConstraintPath(path string) (*ConstraintPath, error) {
	if path == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "constraint path cannot be empty")
	}

	// Split by dots, but we need at least 3 parts
	parts := strings.SplitN(path, ".", 3)
	if len(parts) < 3 {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid constraint path: expected format {Type}.{Subtype}.{Key}",
			map[string]any{"path": path})
	}

	// Parse and validate the measurement type
	measurementType, valid := measurement.ParseType(parts[0])
	if !valid {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"invalid measurement type in constraint path",
			map[string]any{"type": parts[0], "path": path, "validTypes": measurement.Types})
	}

	return &ConstraintPath{
		Type:    measurementType,
		Subtype: parts[1],
		Key:     parts[2], // Key is everything after the second dot (preserves dots in key)
	}, nil
}

// String returns the fully qualified path string.
func (cp *ConstraintPath) String() string {
	return fmt.Sprintf("%s.%s.%s", cp.Type, cp.Subtype, cp.Key)
}

// ExtractValue extracts the value at this path from a snapshot.
// Returns the value as a string, or an error if the path doesn't exist.
func (cp *ConstraintPath) ExtractValue(snap *snapshotter.Snapshot) (string, error) {
	if snap == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "snapshot is nil")
	}

	// Find the measurement with matching type
	var targetMeasurement *measurement.Measurement
	for _, m := range snap.Measurements {
		if m.Type == cp.Type {
			targetMeasurement = m
			break
		}
	}

	if targetMeasurement == nil {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"measurement type not found in snapshot",
			map[string]any{"type": cp.Type})
	}

	// Find the subtype
	var targetSubtype *measurement.Subtype
	for i := range targetMeasurement.Subtypes {
		if targetMeasurement.Subtypes[i].Name == cp.Subtype {
			targetSubtype = &targetMeasurement.Subtypes[i]
			break
		}
	}

	if targetSubtype == nil {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"subtype not found in measurement",
			map[string]any{"subtype": cp.Subtype, "type": cp.Type})
	}

	// Find the key in data
	reading, exists := targetSubtype.Data[cp.Key]
	if !exists {
		return "", errors.NewWithContext(errors.ErrCodeNotFound,
			"key not found in subtype",
			map[string]any{"key": cp.Key, "subtype": cp.Subtype, "type": cp.Type})
	}

	// Convert reading to string
	return reading.String(), nil
}

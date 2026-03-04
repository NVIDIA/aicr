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

package pod

import (
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ParseConfigMapURI parses a ConfigMap URI in the format "cm://namespace/name"
// and returns the namespace and name components.
//
// Returns error if URI format is invalid.
func ParseConfigMapURI(uri string) (namespace, name string, err error) {
	const prefix = "cm://"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", errors.NewWithContext(errors.ErrCodeInvalidRequest, "invalid configmap URI", map[string]interface{}{
			"uri":             uri,
			"expected_format": "cm://namespace/name",
		})
	}

	parts := strings.SplitN(strings.TrimPrefix(uri, prefix), "/", 2)
	if len(parts) != 2 {
		return "", "", errors.NewWithContext(errors.ErrCodeInvalidRequest, "invalid configmap URI format", map[string]interface{}{
			"uri":             uri,
			"expected_format": "cm://namespace/name",
		})
	}

	namespace = strings.TrimSpace(parts[0])
	name = strings.TrimSpace(parts[1])

	if namespace == "" || name == "" {
		return "", "", errors.NewWithContext(errors.ErrCodeInvalidRequest, "namespace and name cannot be empty", map[string]interface{}{
			"uri": uri,
		})
	}

	return namespace, name, nil
}

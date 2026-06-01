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

package recipe

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// QueryRequest represents a query API request body for POST.
type QueryRequest struct {
	Criteria *Criteria `json:"criteria" yaml:"criteria"`
	Selector string    `json:"selector" yaml:"selector"`
}

// ParseQueryRequestFromBody parses a QueryRequest from the request body,
// honoring the standard MaxRecipePOSTBytes bound. The content type selects
// JSON vs YAML.
func ParseQueryRequestFromBody(body io.Reader, contentType string) (*QueryRequest, error) {
	if body == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "request body cannot be nil")
	}

	limited := io.LimitReader(body, defaults.MaxRecipePOSTBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read request body", err)
	}
	if int64(len(data)) > defaults.MaxRecipePOSTBytes {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("request body exceeds %d bytes", defaults.MaxRecipePOSTBytes))
	}

	if len(data) == 0 {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "request body cannot be empty")
	}

	var req QueryRequest
	if strings.Contains(contentType, "json") {
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to parse JSON body", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &req); err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to parse YAML body", err)
		}
	}

	return &req, nil
}

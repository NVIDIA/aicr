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

package bundler

import (
	stderrors "errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestParseBundleConfig_Bundlers pins the query-param parsing of the
// `bundlers` positive component-name filter on POST /v1/bundle: comma
// delimited, whitespace trimmed, empty segments dropped, and an all-empty
// value rejected with ErrCodeInvalidRequest. See #1531.
func TestParseBundleConfig_Bundlers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		present  bool
		bundlers string // raw query value, used only when present
		expected []string
		wantErr  bool
	}{
		{
			name:     "absent param means no filter",
			expected: nil,
		},
		{
			name:     "single name",
			present:  true,
			bundlers: "gpu-operator",
			expected: []string{"gpu-operator"},
		},
		{
			name:     "multiple names with whitespace trimmed",
			present:  true,
			bundlers: "gpu-operator, network-operator ,cert-manager",
			expected: []string{"gpu-operator", "network-operator", "cert-manager"},
		},
		{
			name:     "empty segments dropped",
			present:  true,
			bundlers: "gpu-operator,,network-operator,",
			expected: []string{"gpu-operator", "network-operator"},
		},
		{
			name:     "all-empty value rejected",
			present:  true,
			bundlers: ",, ,",
			wantErr:  true,
		},
		{
			name:    "explicit empty value rejected",
			present: true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := "/v1/bundle"
			if tt.present {
				target += "?bundlers=" + url.QueryEscape(tt.bundlers)
			}
			r := httptest.NewRequest("POST", target, nil)

			cfg, err := ParseBundleConfig(r)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseBundleConfig() expected error, got nil")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("ParseBundleConfig() error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBundleConfig() error = %v", err)
			}
			if got := cfg.Bundlers(); !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("Bundlers() = %v, want %v", got, tt.expected)
			}
		})
	}
}

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

package recipe

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestParseCriteriaServiceType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    CriteriaServiceType
		wantErr bool
	}{
		{"empty", "", CriteriaServiceAny, false},
		{"any", "any", CriteriaServiceAny, false},
		{"eks", "eks", CriteriaServiceEKS, false},
		{"EKS uppercase", "EKS", CriteriaServiceEKS, false},
		{"gke", "gke", CriteriaServiceGKE, false},
		{"aks", "aks", CriteriaServiceAKS, false},
		{"oke", "oke", CriteriaServiceOKE, false},
		{"lke", "lke", CriteriaServiceLKE, false},
		{"LKE uppercase", "LKE", CriteriaServiceLKE, false},
		{"self-managed", "self-managed", CriteriaServiceAny, false},
		{"self", "self", CriteriaServiceAny, false},
		{"vanilla", "vanilla", CriteriaServiceAny, false},
		{"invalid", "invalid", CriteriaServiceAny, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCriteriaServiceType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCriteriaServiceType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseCriteriaServiceType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCriteriaAcceleratorType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    CriteriaAcceleratorType
		wantErr bool
	}{
		{"empty", "", CriteriaAcceleratorAny, false},
		{"any", "any", CriteriaAcceleratorAny, false},
		{"h100", "h100", CriteriaAcceleratorH100, false},
		{"H100 uppercase", "H100", CriteriaAcceleratorH100, false},
		{"gb200", "gb200", CriteriaAcceleratorGB200, false},
		{"b200", "b200", CriteriaAcceleratorB200, false},
		{"a100", "a100", CriteriaAcceleratorA100, false},
		{"l40", "l40", CriteriaAcceleratorL40, false},
		{"rtx-pro-6000", "rtx-pro-6000", CriteriaAcceleratorRTXPro6000, false},
		{"RTX-PRO-6000 uppercase", "RTX-PRO-6000", CriteriaAcceleratorRTXPro6000, false},
		{"invalid", "v100", CriteriaAcceleratorAny, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCriteriaAcceleratorType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCriteriaAcceleratorType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseCriteriaAcceleratorType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCriteriaIntentType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    CriteriaIntentType
		wantErr bool
	}{
		{"empty", "", CriteriaIntentAny, false},
		{"any", "any", CriteriaIntentAny, false},
		{"training", "training", CriteriaIntentTraining, false},
		{"inference", "inference", CriteriaIntentInference, false},
		{"invalid", "serving", CriteriaIntentAny, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCriteriaIntentType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCriteriaIntentType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseCriteriaIntentType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCriteriaMatches(t *testing.T) {
	tests := []struct {
		name     string
		criteria *Criteria
		other    *Criteria
		want     bool
	}{
		{
			name:     "nil other",
			criteria: NewCriteria(),
			other:    nil,
			want:     true,
		},
		{
			name:     "all any matches all any",
			criteria: NewCriteria(),
			other:    NewCriteria(),
			want:     true,
		},
		{
			name: "specific recipe does not match generic query",
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
			},
			other: NewCriteria(),
			want:  false, // Query "any" only matches generic recipes
		},
		{
			name:     "generic recipe matches specific query",
			criteria: NewCriteria(), // Recipe: all "any"
			other: &Criteria{
				Service: CriteriaServiceEKS,
			},
			want: true, // Recipe is generic, matches any query value
		},
		{
			name: "same service matches",
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
			},
			other: &Criteria{
				Service: CriteriaServiceEKS,
			},
			want: true,
		},
		{
			name: "different service does not match",
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
			},
			other: &Criteria{
				Service: CriteriaServiceGKE,
			},
			want: false,
		},
		{
			name: "partial match on multiple fields",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
			},
			other: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
			},
			want: true,
		},
		{
			name: "one field mismatch",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
			},
			other: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
			},
			want: false,
		},
		{
			name: "recipe with partial criteria matches query with more fields",
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
				// Accelerator is "any" (zero value)
			},
			other: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
			},
			want: true, // Recipe service=eks matches, accelerator is generic so matches any
		},
		{
			name: "recipe with more specific criteria does not match less specific query",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
			},
			other: &Criteria{
				Service: CriteriaServiceEKS,
				// Accelerator is "any"
			},
			want: false, // Query doesn't specify accelerator, but recipe requires gb200
		},
		{
			name: "platform any recipe matches specific platform query",
			criteria: &Criteria{
				Service:  CriteriaServiceEKS,
				Platform: CriteriaPlatformAny,
			},
			other: &Criteria{
				Service:  CriteriaServiceEKS,
				Platform: CriteriaPlatformKubeflow,
			},
			want: true, // Recipe platform is generic, matches any query value
		},
		{
			name: "specific platform recipe does not match any platform query",
			criteria: &Criteria{
				Service:  CriteriaServiceEKS,
				Platform: CriteriaPlatformKubeflow,
			},
			other: &Criteria{
				Service:  CriteriaServiceEKS,
				Platform: CriteriaPlatformAny,
			},
			want: false, // Query "any" only matches generic recipes
		},
		{
			name: "same platform matches",
			criteria: &Criteria{
				Service:  CriteriaServiceEKS,
				Platform: CriteriaPlatformKubeflow,
			},
			other: &Criteria{
				Service:  CriteriaServiceEKS,
				Platform: CriteriaPlatformKubeflow,
			},
			want: true,
		},
		{
			name: "full criteria with platform matches",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformKubeflow,
			},
			other: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformKubeflow,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.criteria.Matches(tt.other); got != tt.want {
				t.Errorf("Criteria.Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCriteriaSpecificity(t *testing.T) {
	tests := []struct {
		name     string
		criteria *Criteria
		want     int
	}{
		{
			name:     "all any",
			criteria: NewCriteria(),
			want:     0,
		},
		{
			name: "one field",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			want: 1,
		},
		{
			name: "three fields",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			want: 3,
		},
		{
			name: "all fields",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformKubeflow,
				Nodes:       100,
			},
			want: 6,
		},
		// Regression tests: YAML-parsed criteria have empty strings for omitted
		// fields, not "any". Specificity must treat "" as equivalent to "any".
		{
			name: "yaml-parsed one field - empty strings are zero specificity",
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
				// Remaining fields omitted — Go zero value is ""
			},
			want: 1,
		},
		{
			name: "yaml-parsed two fields - empty strings are zero specificity",
			criteria: &Criteria{
				Service: CriteriaServiceEKS,
				Intent:  CriteriaIntentTraining,
			},
			want: 2,
		},
		{
			name: "yaml-parsed three fields - empty strings are zero specificity",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
			},
			want: 3,
		},
		{
			name:     "all empty strings - zero specificity",
			criteria: &Criteria{},
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.criteria.Specificity(); got != tt.want {
				t.Errorf("Criteria.Specificity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildCriteria(t *testing.T) {
	tests := []struct {
		name    string
		opts    []CriteriaOption
		want    *Criteria
		wantErr bool
	}{
		{
			name: "no options",
			opts: nil,
			want: NewCriteria(),
		},
		{
			name: "with service",
			opts: []CriteriaOption{WithCriteriaService("eks")},
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
			},
		},
		{
			name: "with multiple options",
			opts: []CriteriaOption{
				WithCriteriaService("eks"),
				WithCriteriaAccelerator("h100"),
				WithCriteriaIntent("training"),
			},
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
			},
		},
		{
			name: "with platform",
			opts: []CriteriaOption{WithCriteriaPlatform("kubeflow")},
			want: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformKubeflow,
			},
		},
		{
			name:    "invalid service",
			opts:    []CriteriaOption{WithCriteriaService("invalid")},
			wantErr: true,
		},
		{
			name:    "invalid accelerator",
			opts:    []CriteriaOption{WithCriteriaAccelerator("v100")},
			wantErr: true,
		},
		{
			name:    "invalid platform",
			opts:    []CriteriaOption{WithCriteriaPlatform("invalid")},
			wantErr: true,
		},
		{
			name:    "negative nodes",
			opts:    []CriteriaOption{WithCriteriaNodes(-1)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildCriteria(tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Errorf("BuildCriteria() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got.Service != tt.want.Service ||
				got.Accelerator != tt.want.Accelerator ||
				got.Intent != tt.want.Intent ||
				got.Platform != tt.want.Platform {

				t.Errorf("BuildCriteria() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCriteriaFromValues(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    *Criteria
		wantErr bool
	}{
		{
			name:  "empty query defaults to any",
			query: "",
			want: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:  "all parameters",
			query: "service=eks&accelerator=h100&intent=training&os=ubuntu&platform=kubeflow&nodes=8",
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformKubeflow,
				Nodes:       8,
			},
			wantErr: false,
		},
		{
			name:  "gpu alias for accelerator",
			query: "gpu=gb200",
			want: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:  "accelerator takes precedence over gpu",
			query: "accelerator=h100&gpu=a100",
			want: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:  "platform parameter",
			query: "platform=kubeflow",
			want: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformKubeflow,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:    "invalid service",
			query:   "service=invalid",
			wantErr: true,
		},
		{
			name:    "invalid accelerator",
			query:   "accelerator=invalid",
			wantErr: true,
		},
		{
			name:    "invalid intent",
			query:   "intent=invalid",
			wantErr: true,
		},
		{
			name:    "invalid os",
			query:   "os=invalid",
			wantErr: true,
		},
		{
			name:    "invalid platform",
			query:   "platform=invalid",
			wantErr: true,
		},
		{
			name:    "invalid nodes - not a number",
			query:   "nodes=abc",
			wantErr: true,
		},
		{
			name:    "invalid nodes - negative",
			query:   "nodes=-1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := parseQuery(tt.query)

			got, err := ParseCriteriaFromValues(values)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCriteriaFromValues() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got.Service != tt.want.Service {
				t.Errorf("Service = %v, want %v", got.Service, tt.want.Service)
			}
			if got.Accelerator != tt.want.Accelerator {
				t.Errorf("Accelerator = %v, want %v", got.Accelerator, tt.want.Accelerator)
			}
			if got.Intent != tt.want.Intent {
				t.Errorf("Intent = %v, want %v", got.Intent, tt.want.Intent)
			}
			if got.OS != tt.want.OS {
				t.Errorf("OS = %v, want %v", got.OS, tt.want.OS)
			}
			if got.Platform != tt.want.Platform {
				t.Errorf("Platform = %v, want %v", got.Platform, tt.want.Platform)
			}
			if got.Nodes != tt.want.Nodes {
				t.Errorf("Nodes = %v, want %v", got.Nodes, tt.want.Nodes)
			}
		})
	}
}

func TestParseCriteriaFromRequest(t *testing.T) {
	t.Run("nil request returns error", func(t *testing.T) {
		_, err := ParseCriteriaFromRequest(nil)
		if err == nil {
			t.Error("expected error for nil request")
		}
	})

	t.Run("valid request", func(t *testing.T) {
		req := createTestRequest("service=gke&accelerator=a100")
		got, err := ParseCriteriaFromRequest(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Service != CriteriaServiceGKE {
			t.Errorf("Service = %v, want %v", got.Service, CriteriaServiceGKE)
		}
		if got.Accelerator != CriteriaAcceleratorA100 {
			t.Errorf("Accelerator = %v, want %v", got.Accelerator, CriteriaAcceleratorA100)
		}
	})
}

// parseQuery is a helper to parse URL query strings for testing.
func parseQuery(query string) map[string][]string {
	values := make(map[string][]string)
	if query == "" {
		return values
	}
	for _, pair := range splitQueryParams(query) {
		parts := splitQueryParam(pair)
		if len(parts) == 2 {
			values[parts[0]] = append(values[parts[0]], parts[1])
		}
	}
	return values
}

// splitQueryParams splits a query string on &.
func splitQueryParams(query string) []string {
	result := []string{}
	start := 0
	for i, c := range query {
		if c == '&' {
			if i > start {
				result = append(result, query[start:i])
			}
			start = i + 1
		}
	}
	if start < len(query) {
		result = append(result, query[start:])
	}
	return result
}

// splitQueryParam splits a query param on =.
func splitQueryParam(param string) []string {
	for i, c := range param {
		if c == '=' {
			return []string{param[:i], param[i+1:]}
		}
	}
	return []string{param}
}

// createTestRequest creates a test HTTP request with given query params.
func createTestRequest(query string) *http.Request {
	req := &http.Request{}
	if query != "" {
		req.URL = &url.URL{RawQuery: query}
	} else {
		req.URL = &url.URL{}
	}
	return req
}

func TestGetCriteriaServiceTypes(t *testing.T) {
	types := GetCriteriaServiceTypes()

	// Should return sorted list
	expected := []string{"aks", "eks", "gke", "kind", "lke", "oke"}
	if len(types) != len(expected) {
		t.Errorf("GetCriteriaServiceTypes() returned %d types, want %d", len(types), len(expected))
	}

	for i, exp := range expected {
		if types[i] != exp {
			t.Errorf("GetCriteriaServiceTypes()[%d] = %s, want %s", i, types[i], exp)
		}
	}

	// Verify each type can be parsed
	for _, st := range types {
		_, err := ParseCriteriaServiceType(st)
		if err != nil {
			t.Errorf("ParseCriteriaServiceType(%s) error = %v", st, err)
		}
	}
}

func TestGetCriteriaAcceleratorTypes(t *testing.T) {
	types := GetCriteriaAcceleratorTypes()

	// Should return sorted list
	expected := []string{"a100", "b200", "gb200", "h100", "l40", "rtx-pro-6000"}
	if len(types) != len(expected) {
		t.Errorf("GetCriteriaAcceleratorTypes() returned %d types, want %d", len(types), len(expected))
	}

	for i, exp := range expected {
		if types[i] != exp {
			t.Errorf("GetCriteriaAcceleratorTypes()[%d] = %s, want %s", i, types[i], exp)
		}
	}

	// Verify each type can be parsed
	for _, at := range types {
		_, err := ParseCriteriaAcceleratorType(at)
		if err != nil {
			t.Errorf("ParseCriteriaAcceleratorType(%s) error = %v", at, err)
		}
	}
}

func TestGetCriteriaIntentTypes(t *testing.T) {
	types := GetCriteriaIntentTypes()

	// Should return sorted list
	expected := []string{"inference", "training"}
	if len(types) != len(expected) {
		t.Errorf("GetCriteriaIntentTypes() returned %d types, want %d", len(types), len(expected))
	}

	for i, exp := range expected {
		if types[i] != exp {
			t.Errorf("GetCriteriaIntentTypes()[%d] = %s, want %s", i, types[i], exp)
		}
	}

	// Verify each type can be parsed
	for _, it := range types {
		_, err := ParseCriteriaIntentType(it)
		if err != nil {
			t.Errorf("ParseCriteriaIntentType(%s) error = %v", it, err)
		}
	}
}

func TestGetCriteriaOSTypes(t *testing.T) {
	types := GetCriteriaOSTypes()

	// Should return sorted list
	expected := []string{"amazonlinux", "cos", "rhel", "ubuntu"}
	if len(types) != len(expected) {
		t.Errorf("GetCriteriaOSTypes() returned %d types, want %d", len(types), len(expected))
	}

	for i, exp := range expected {
		if types[i] != exp {
			t.Errorf("GetCriteriaOSTypes()[%d] = %s, want %s", i, types[i], exp)
		}
	}

	// Verify each type can be parsed
	for _, ot := range types {
		_, err := ParseCriteriaOSType(ot)
		if err != nil {
			t.Errorf("ParseCriteriaOSType(%s) error = %v", ot, err)
		}
	}
}

func TestParseCriteriaPlatformType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    CriteriaPlatformType
		wantErr bool
	}{
		{"empty", "", CriteriaPlatformAny, false},
		{"any", "any", CriteriaPlatformAny, false},
		{"dynamo", "dynamo", CriteriaPlatformDynamo, false},
		{"Dynamo uppercase", "Dynamo", CriteriaPlatformDynamo, false},
		{"kubeflow", "kubeflow", CriteriaPlatformKubeflow, false},
		{"Kubeflow uppercase", "Kubeflow", CriteriaPlatformKubeflow, false},
		{"nim", "nim", CriteriaPlatformNIM, false},
		{"NIM uppercase", "NIM", CriteriaPlatformNIM, false},
		{"invalid", "invalid", CriteriaPlatformAny, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCriteriaPlatformType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCriteriaPlatformType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseCriteriaPlatformType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetCriteriaPlatformTypes(t *testing.T) {
	types := GetCriteriaPlatformTypes()

	// Should return sorted list
	expected := []string{"dynamo", "kubeflow", "nim"}
	if len(types) != len(expected) {
		t.Errorf("GetCriteriaPlatformTypes() returned %d types, want %d", len(types), len(expected))
	}

	for i, exp := range expected {
		if types[i] != exp {
			t.Errorf("GetCriteriaPlatformTypes()[%d] = %s, want %s", i, types[i], exp)
		}
	}

	// Verify each type can be parsed
	for _, pt := range types {
		_, err := ParseCriteriaPlatformType(pt)
		if err != nil {
			t.Errorf("ParseCriteriaPlatformType(%s) error = %v", pt, err)
		}
	}
}

func TestParseCriteriaOSType_AllAliases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  CriteriaOSType
	}{
		{"amazonlinux", "amazonlinux", CriteriaOSAmazonLinux},
		{"al2", "al2", CriteriaOSAmazonLinux},
		{"al2023", "al2023", CriteriaOSAmazonLinux},
		{"ubuntu", "ubuntu", CriteriaOSUbuntu},
		{"rhel", "rhel", CriteriaOSRHEL},
		{"cos", "cos", CriteriaOSCOS},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCriteriaOSType(tt.input)
			if err != nil {
				t.Errorf("ParseCriteriaOSType(%s) error = %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("ParseCriteriaOSType(%s) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadCriteriaFromFile(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		want     *Criteria
		wantErr  bool
	}{
		{
			name:     "valid YAML file with full structure",
			filename: "criteria.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: eks-h100-training
spec:
  service: eks
  accelerator: h100
  intent: training
  os: ubuntu
  nodes: 4
`,
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformAny,
				Nodes:       4,
			},
			wantErr: false,
		},
		{
			name:     "valid JSON file with full structure",
			filename: "criteria.json",
			content:  `{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","metadata":{"name":"gke-a100"},"spec":{"service":"gke","accelerator":"a100","intent":"inference"}}`,
			want: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorA100,
				Intent:      CriteriaIntentInference,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:     "partial fields - only spec.service",
			filename: "partial.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: aks-only
spec:
  service: aks`,
			want: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:     "empty file returns error",
			filename: "empty.yaml",
			content:  ``,
			wantErr:  true,
		},
		{
			name:     "empty spec defaults to any",
			filename: "empty_spec.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: empty
spec: {}`,
			want: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:     "missing kind and apiVersion still works",
			filename: "minimal.yaml",
			content: `spec:
  service: eks`,
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:     "invalid kind",
			filename: "invalid_kind.yaml",
			content: `kind: wrongKind
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  service: eks`,
			wantErr: true,
		},
		{
			name:     "invalid apiVersion",
			filename: "invalid_api.yaml",
			content: `kind: RecipeCriteria
apiVersion: wrong/v1
spec:
  service: eks`,
			wantErr: true,
		},
		{
			name:     "invalid service type",
			filename: "invalid_service.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  service: invalid`,
			wantErr: true,
		},
		{
			name:     "invalid accelerator type",
			filename: "invalid_accelerator.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  accelerator: v100`,
			wantErr: true,
		},
		{
			name:     "invalid intent type",
			filename: "invalid_intent.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  intent: serving`,
			wantErr: true,
		},
		{
			name:     "invalid OS type",
			filename: "invalid_os.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  os: windows`,
			wantErr: true,
		},
		{
			name:     "negative nodes count",
			filename: "negative_nodes.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  nodes: -5`,
			wantErr: true,
		},
		{
			name:     "valid YAML file with platform",
			filename: "criteria_with_platform.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: eks-h100-training-kubeflow
spec:
  service: eks
  accelerator: h100
  intent: training
  os: ubuntu
  platform: kubeflow
  nodes: 4
`,
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformKubeflow,
				Nodes:       4,
			},
			wantErr: false,
		},
		{
			name:     "invalid platform type",
			filename: "invalid_platform.yaml",
			content: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  platform: invalid-platform`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary file
			tmpDir := t.TempDir()
			filePath := tmpDir + "/" + tt.filename
			if err := writeTestFile(filePath, tt.content); err != nil {
				t.Fatalf("failed to create test file: %v", err)
			}

			got, err := LoadCriteriaFromFile(filePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadCriteriaFromFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got.Service != tt.want.Service {
				t.Errorf("Service = %v, want %v", got.Service, tt.want.Service)
			}
			if got.Accelerator != tt.want.Accelerator {
				t.Errorf("Accelerator = %v, want %v", got.Accelerator, tt.want.Accelerator)
			}
			if got.Intent != tt.want.Intent {
				t.Errorf("Intent = %v, want %v", got.Intent, tt.want.Intent)
			}
			if got.OS != tt.want.OS {
				t.Errorf("OS = %v, want %v", got.OS, tt.want.OS)
			}
			if got.Nodes != tt.want.Nodes {
				t.Errorf("Nodes = %v, want %v", got.Nodes, tt.want.Nodes)
			}
			if got.Platform != tt.want.Platform {
				t.Errorf("Platform = %v, want %v", got.Platform, tt.want.Platform)
			}
		})
	}
}

func TestLoadCriteriaFromFile_NotFound(t *testing.T) {
	_, err := LoadCriteriaFromFile("/nonexistent/path/criteria.yaml")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoadCriteriaFromFileWithContext(t *testing.T) {
	ctx := context.Background()

	t.Run("local file", func(t *testing.T) {
		// Create a temporary file with criteria
		content := `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: test-criteria
spec:
  service: eks
  accelerator: h100
  intent: training
`
		tmpFile, err := os.CreateTemp("", "criteria-*.yaml")
		if err != nil {
			t.Fatalf("failed to create temp file: %v", err)
		}
		defer os.Remove(tmpFile.Name())

		if _, writeErr := tmpFile.WriteString(content); writeErr != nil {
			t.Fatalf("failed to write temp file: %v", writeErr)
		}
		tmpFile.Close()

		// Test loading with context
		got, err := LoadCriteriaFromFileWithContext(ctx, tmpFile.Name())
		if err != nil {
			t.Fatalf("LoadCriteriaFromFileWithContext() error = %v", err)
		}

		if got.Service != CriteriaServiceEKS {
			t.Errorf("Service = %v, want %v", got.Service, CriteriaServiceEKS)
		}
		if got.Accelerator != CriteriaAcceleratorH100 {
			t.Errorf("Accelerator = %v, want %v", got.Accelerator, CriteriaAcceleratorH100)
		}
		if got.Intent != CriteriaIntentTraining {
			t.Errorf("Intent = %v, want %v", got.Intent, CriteriaIntentTraining)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := LoadCriteriaFromFileWithContext(ctx, "/nonexistent/path/criteria.yaml")
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		// Create a temp file for testing
		tmpFile, err := os.CreateTemp("", "criteria-*.yaml")
		if err != nil {
			t.Fatalf("failed to create temp file: %v", err)
		}
		defer os.Remove(tmpFile.Name())
		tmpFile.Close()

		// For local files, context cancellation doesn't currently affect the operation
		// but the API accepts context for consistency and future HTTP support
		_, err = LoadCriteriaFromFileWithContext(canceledCtx, tmpFile.Name())
		// For local files, this should succeed even with canceled context
		// because file I/O doesn't use context yet
		if err != nil {
			// If it fails, that's also acceptable - depends on implementation
			t.Logf("operation with canceled context: %v", err)
		}
	})
}

func TestParseCriteriaFromBody(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		want        *Criteria
		wantErr     bool
	}{
		{
			name:        "JSON body with full structure",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","metadata":{"name":"test"},"spec":{"service":"eks","accelerator":"h100","intent":"training"}}`,
			contentType: "application/json",
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name: "YAML body with application/x-yaml",
			body: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: test
spec:
  service: gke
  accelerator: gb200
  os: cos`,
			contentType: "application/x-yaml",
			want: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSCOS,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name: "YAML body with text/yaml",
			body: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  service: aks
  nodes: 8`,
			contentType: "text/yaml",
			want: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       8,
			},
			wantErr: false,
		},
		{
			name:        "empty content type defaults to JSON",
			body:        `{"spec":{"service":"oke"}}`,
			contentType: "",
			want: &Criteria{
				Service:     CriteriaServiceOKE,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:        "content type with charset",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","spec":{"service":"eks"}}`,
			contentType: "application/json; charset=utf-8",
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:        "empty body",
			body:        "",
			contentType: "application/json",
			wantErr:     true,
		},
		{
			name:        "invalid JSON",
			body:        `{invalid json}`,
			contentType: "application/json",
			wantErr:     true,
		},
		{
			name: "invalid YAML",
			body: `spec:
  service: [unclosed`,
			contentType: "application/x-yaml",
			wantErr:     true,
		},
		{
			name:        "invalid service in body",
			body:        `{"spec":{"service":"invalid"}}`,
			contentType: "application/json",
			wantErr:     true,
		},
		{
			name:        "invalid kind",
			body:        `{"kind":"wrongKind","spec":{"service":"eks"}}`,
			contentType: "application/json",
			wantErr:     true,
		},
		{
			name:        "invalid apiVersion",
			body:        `{"kind":"RecipeCriteria","apiVersion":"wrong/v1","spec":{"service":"eks"}}`,
			contentType: "application/json",
			wantErr:     true,
		},
		{
			name:        "unknown content type tries JSON",
			body:        `{"spec":{"service":"eks"}}`,
			contentType: "text/plain",
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:        "JSON body with platform kubeflow",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","spec":{"service":"eks","accelerator":"h100","platform":"kubeflow"}}`,
			contentType: "application/json",
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformKubeflow,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name: "YAML body with platform",
			body: `kind: RecipeCriteria
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  service: eks
  accelerator: h100
  intent: training
  platform: kubeflow`,
			contentType: "application/x-yaml",
			want: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformKubeflow,
				Nodes:       0,
			},
			wantErr: false,
		},
		{
			name:        "invalid platform in JSON body",
			body:        `{"spec":{"platform":"invalid-platform"}}`,
			contentType: "application/json",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := strings.NewReader(tt.body)
			got, err := ParseCriteriaFromBody(reader, tt.contentType)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCriteriaFromBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got.Service != tt.want.Service {
				t.Errorf("Service = %v, want %v", got.Service, tt.want.Service)
			}
			if got.Accelerator != tt.want.Accelerator {
				t.Errorf("Accelerator = %v, want %v", got.Accelerator, tt.want.Accelerator)
			}
			if got.Intent != tt.want.Intent {
				t.Errorf("Intent = %v, want %v", got.Intent, tt.want.Intent)
			}
			if got.OS != tt.want.OS {
				t.Errorf("OS = %v, want %v", got.OS, tt.want.OS)
			}
			if got.Platform != tt.want.Platform {
				t.Errorf("Platform = %v, want %v", got.Platform, tt.want.Platform)
			}
			if got.Nodes != tt.want.Nodes {
				t.Errorf("Nodes = %v, want %v", got.Nodes, tt.want.Nodes)
			}
		})
	}
}

func TestParseCriteriaFromBody_NilBody(t *testing.T) {
	_, err := ParseCriteriaFromBody(nil, "application/json")
	if err == nil {
		t.Error("expected error for nil body")
	}
}

func TestCriteria_String(t *testing.T) {
	tests := []struct {
		name     string
		criteria Criteria
		want     string
	}{
		{
			name:     "all defaults (any)",
			criteria: Criteria{Service: CriteriaServiceAny, Accelerator: CriteriaAcceleratorAny, Intent: CriteriaIntentAny, OS: CriteriaOSAny, Platform: CriteriaPlatformAny},
			want:     "criteria(any)",
		},
		{
			name:     "single field set",
			criteria: Criteria{Service: "eks", Accelerator: CriteriaAcceleratorAny, Intent: CriteriaIntentAny, OS: CriteriaOSAny, Platform: CriteriaPlatformAny},
			want:     "criteria(service=eks)",
		},
		{
			name:     "multiple fields set",
			criteria: Criteria{Service: "eks", Accelerator: "h100", Intent: "training", OS: CriteriaOSAny, Platform: CriteriaPlatformAny},
			want:     "criteria(service=eks, accelerator=h100, intent=training)",
		},
		{
			name:     "all fields set",
			criteria: Criteria{Service: "gke", Accelerator: "l40", Intent: "inference", OS: "ubuntu", Platform: "kubeflow", Nodes: 8},
			want:     "criteria(service=gke, accelerator=l40, intent=inference, os=ubuntu, platform=kubeflow, nodes=8)",
		},
		{
			name:     "only nodes set",
			criteria: Criteria{Service: CriteriaServiceAny, Accelerator: CriteriaAcceleratorAny, Intent: CriteriaIntentAny, OS: CriteriaOSAny, Platform: CriteriaPlatformAny, Nodes: 4},
			want:     "criteria(nodes=4)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.criteria.String()
			if got != tt.want {
				t.Errorf("Criteria.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithCriteriaOS(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOS  CriteriaOSType
		wantErr bool
	}{
		{"valid ubuntu", "ubuntu", "ubuntu", false},
		{"valid any", "any", CriteriaOSAny, false},
		{"invalid os", "windows", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria, err := BuildCriteria(WithCriteriaOS(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("WithCriteriaOS(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && criteria.OS != tt.wantOS {
				t.Errorf("OS = %v, want %v", criteria.OS, tt.wantOS)
			}
		})
	}
}

func TestWithCriteriaNodes(t *testing.T) {
	tests := []struct {
		name      string
		nodes     int
		wantNodes int
		wantErr   bool
	}{
		{"zero nodes", 0, 0, false},
		{"positive nodes", 8, 8, false},
		{"negative nodes", -1, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria, err := BuildCriteria(WithCriteriaNodes(tt.nodes))
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && criteria.Nodes != tt.wantNodes {
				t.Errorf("Nodes = %d, want %d", criteria.Nodes, tt.wantNodes)
			}
		})
	}
}

func TestCriteriaValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       *Criteria
		wantErr bool
	}{
		{
			name:    "valid criteria",
			c:       &Criteria{Service: "eks", Accelerator: "h100", Intent: "training"},
			wantErr: false,
		},
		{
			name:    "empty criteria is valid",
			c:       &Criteria{},
			wantErr: false,
		},
		{
			name:    "invalid service",
			c:       &Criteria{Service: "invalid-service"},
			wantErr: true,
		},
		{
			name:    "invalid accelerator",
			c:       &Criteria{Accelerator: "invalid-gpu"},
			wantErr: true,
		},
		{
			name:    "invalid intent",
			c:       &Criteria{Intent: "invalid-intent"},
			wantErr: true,
		},
		{
			name:    "invalid os",
			c:       &Criteria{OS: "windows"},
			wantErr: true,
		},
		{
			name:    "invalid platform",
			c:       &Criteria{Platform: "invalid-platform"},
			wantErr: true,
		},
		{
			name:    "normalizes case",
			c:       &Criteria{Service: "EKS", Accelerator: "H100"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// writeTestFile is a helper to create test files.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

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

package main

import "testing"

const sampleReport = `{
  "repositories": {
    "NVIDIA/aicr": {
      "packageFiles": {
        "custom.regex": [
          {
            "packageFile": "recipes/registry.yaml",
            "deps": [
              {
                "depName": "ghcr.io/nvidia/nvsentinel",
                "depType": "registry-chart",
                "datasource": "docker",
                "currentValue": "v1.20.0",
                "updates": [
                  {"newValue": "v1.20.3", "updateType": "patch"},
                  {"newValue": "v1.23.0", "updateType": "minor"}
                ]
              },
              {
                "depName": "cert-manager",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "v1.20.2",
                "updates": []
              },
              {
                "depName": "dynamo-platform",
                "depType": "registry-chart",
                "datasource": "helm",
                "currentValue": "1.4.2",
                "skipReason": "invalid-value",
                "warnings": [{"message": "Failed to look up helm package"}],
                "updates": []
              },
              {
                "depName": "golang.org/x/net",
                "depType": "require",
                "datasource": "go",
                "currentValue": "v0.1.0",
                "updates": [{"newValue": "v0.2.0", "updateType": "minor"}]
              }
            ]
          }
        ]
      }
    }
  }
}`

func TestParseRenovateReport(t *testing.T) {
	got, err := ParseRenovateReport([]byte(sampleReport))
	if err != nil {
		t.Fatalf("ParseRenovateReport: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d registry-chart deps, want 3 (non-registry depTypes must be dropped)", len(got))
	}

	tests := []struct {
		name                           string
		dep                            string
		wantLatest, wantType, wantProb string
	}{
		{"highest update wins", "ghcr.io/nvidia/nvsentinel", "v1.23.0", "minor", ""},
		{"no updates means current", "cert-manager", "", "", ""},
		{"skipReason surfaces as a problem", "dynamo-platform", "", "", "invalid-value: Failed to look up helm package"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, ok := got[tt.dep]
			if !ok {
				t.Fatalf("dep %q missing from result", tt.dep)
			}
			if l.Latest != tt.wantLatest || l.UpdateType != tt.wantType || l.Problem != tt.wantProb {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					l.Latest, l.UpdateType, l.Problem, tt.wantLatest, tt.wantType, tt.wantProb)
			}
		})
	}
}

func TestParseRenovateReportRejectsGarbage(t *testing.T) {
	if _, err := ParseRenovateReport([]byte(`{"repositories":`)); err == nil {
		t.Fatal("want error on truncated JSON, got nil")
	}
}

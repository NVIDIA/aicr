// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

import (
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestCoordinateFor(t *testing.T) {
	tests := []struct {
		name      string
		criteria  RecipeCriteria
		wantGroup string
		wantDash  string
		wantTab   string
		wantPath  string
		wantErr   bool
	}{
		// ── Golden cases matching issue #1267 / PR #1409 ─────────────────────
		{
			name:      "eks h100 ubuntu training bare intent",
			criteria:  RecipeCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training"},
			wantGroup: "eks",
			wantDash:  "h100-ubuntu",
			wantTab:   "training",
			wantPath:  "eks/h100-ubuntu/training",
		},
		{
			name:      "eks h100 ubuntu training kubeflow platform",
			criteria:  RecipeCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training", Platform: "kubeflow"},
			wantGroup: "eks",
			wantDash:  "h100-ubuntu",
			wantTab:   "training-kubeflow",
			wantPath:  "eks/h100-ubuntu/training-kubeflow",
		},
		{
			name:      "gke h100 cos inference nim",
			criteria:  RecipeCriteria{Service: "gke", Accelerator: "h100", OS: "cos", Intent: "inference", Platform: "nim"},
			wantGroup: "gke",
			wantDash:  "h100-cos",
			wantTab:   "inference-nim",
			wantPath:  "gke/h100-cos/inference-nim",
		},
		{
			name:      "aks h200 ubuntu training dynamo",
			criteria:  RecipeCriteria{Service: "aks", Accelerator: "h200", OS: "ubuntu", Intent: "training", Platform: "dynamo"},
			wantGroup: "aks",
			wantDash:  "h200-ubuntu",
			wantTab:   "training-dynamo",
			wantPath:  "aks/h200-ubuntu/training-dynamo",
		},
		{
			name:      "platform any treated as bare intent",
			criteria:  RecipeCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training", Platform: "any"},
			wantGroup: "eks",
			wantDash:  "h100-ubuntu",
			wantTab:   "training",
			wantPath:  "eks/h100-ubuntu/training",
		},
		// ── Error cases ────────────────────────────────────────────────────────
		{name: "empty service", criteria: RecipeCriteria{Accelerator: "h100", OS: "ubuntu", Intent: "training"}, wantErr: true},
		{name: "any service", criteria: RecipeCriteria{Service: "any", Accelerator: "h100", OS: "ubuntu", Intent: "training"}, wantErr: true},
		{name: "empty accelerator", criteria: RecipeCriteria{Service: "eks", OS: "ubuntu", Intent: "training"}, wantErr: true},
		{name: "empty os", criteria: RecipeCriteria{Service: "eks", Accelerator: "h100", Intent: "training"}, wantErr: true},
		{name: "empty intent", criteria: RecipeCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu"}, wantErr: true},
		{name: "any intent", criteria: RecipeCriteria{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "any"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coord, err := CoordinateFor(tt.criteria)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CoordinateFor() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
				}
				return
			}
			if coord.Group != tt.wantGroup {
				t.Errorf("Group = %q, want %q", coord.Group, tt.wantGroup)
			}
			if coord.Dashboard != tt.wantDash {
				t.Errorf("Dashboard = %q, want %q", coord.Dashboard, tt.wantDash)
			}
			if coord.Tab != tt.wantTab {
				t.Errorf("Tab = %q, want %q", coord.Tab, tt.wantTab)
			}
			if coord.Path() != tt.wantPath {
				t.Errorf("Path() = %q, want %q", coord.Path(), tt.wantPath)
			}
			if coord.String() != tt.wantPath {
				t.Errorf("String() = %q, want %q", coord.String(), tt.wantPath)
			}
		})
	}
}

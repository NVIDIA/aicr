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

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckTag(t *testing.T) {
	tests := []struct {
		name    string
		pinned  string
		tag     string
		wantErr bool
	}{
		{"match", "2.0.0", "2.0.0", false},
		{"runtime behind operator", "2.0.0", "1.0.0", true},
		{"runtime ahead of operator", "1.0.0", "2.0.0", true},
		{"v prefix is not normalized", "2.0.0", "v2.0.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTag(tt.pinned, tt.tag)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkTag(%q, %q) err = %v, wantErr %v", tt.pinned, tt.tag, err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
			for _, want := range []string{tt.pinned, tt.tag, "pkg/defaults/images.go"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

func TestPinnedChartVersionMissingComponent(t *testing.T) {
	_, err := pinnedChartVersion(nil)
	errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
}

// TestRuntimeImageTagMatchesRegistry runs the make check-dynamo-runtime-image
// check against the committed registry, and checks that each hand-maintained
// manifest that hard-codes the vllm-runtime image names the pinned tag.
func TestRuntimeImageTagMatchesRegistry(t *testing.T) {
	reg, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("load component registry: %v", err)
	}
	pinned, err := pinnedChartVersion(reg)
	if err != nil {
		t.Fatalf("pinnedChartVersion: %v", err)
	}
	if err := checkTag(pinned, defaults.DynamoRuntimeImageTag); err != nil {
		t.Fatal(err)
	}

	ref := defaults.DynamoImageRepository + "/vllm-runtime:" + pinned
	for _, path := range []string{
		"../../tests/manifests/dynamo-vllm-smoke-test.yaml",
		"../../tests/uat/lib/phases.sh",
		"../../demos/workloads/inference/vllm-agg.yaml",
		"../../pkg/evidence/cncf/scripts/manifests/dynamo-vllm-agg.yaml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		if !strings.Contains(string(data), ref) {
			t.Errorf("%s does not pin %s; bump it alongside the %s defaultVersion", path, ref, componentName)
		}
	}
}

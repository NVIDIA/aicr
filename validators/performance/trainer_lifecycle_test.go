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
	"strings"
	"testing"
)

func TestRewriteJobSetStagingImage(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantChanged bool
	}{
		{
			name:        "staging image with v0.11.0 tag is repointed",
			input:       "        image: us-central1-docker.pkg.dev/k8s-staging-images/jobset/jobset:v0.11.0\n",
			wantChanged: true,
		},
		{
			name:        "staging image with arbitrary tag is repointed (tag-agnostic)",
			input:       "image: us-central1-docker.pkg.dev/k8s-staging-images/jobset/jobset:v0.99.9",
			wantChanged: true,
		},
		{
			name:        "already-promoted image is left untouched",
			input:       "image: registry.k8s.io/jobset/jobset:v0.11.0",
			wantChanged: false,
		},
		{
			name:        "unrelated resource is left untouched",
			input:       "image: nvcr.io/nvidia/some-image:latest",
			wantChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(rewriteJobSetStagingImage([]byte(tt.input)))

			if strings.Contains(got, jobSetStagingImageRepo) {
				t.Errorf("output still references staging repo %q: %s", jobSetStagingImageRepo, got)
			}

			changed := got != tt.input
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v (output: %s)", changed, tt.wantChanged, got)
			}

			if tt.wantChanged && !strings.Contains(got, jobSetPromotedImageRepo) {
				t.Errorf("output does not reference promoted repo %q: %s", jobSetPromotedImageRepo, got)
			}
		})
	}
}

// TestRewriteJobSetStagingImage_PreservesTag verifies the rewrite is a repo-prefix swap
// that preserves the original tag.
func TestRewriteJobSetStagingImage_PreservesTag(t *testing.T) {
	in := "image: " + jobSetStagingImageRepo + ":v0.11.0"
	want := "image: " + jobSetPromotedImageRepo + ":v0.11.0"
	if got := string(rewriteJobSetStagingImage([]byte(in))); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

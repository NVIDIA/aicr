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

package aicr

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// The projection is the only place either axis can be lost: what it drops, the
// matcher never sees, and a discarded namespace reports a relocation as no
// change whatsoever.
func TestComponentIdentities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          *RecipeResult
		objectNames map[string]map[string]string
		want        map[string]upgrade.Identity
	}{
		{
			name: "nil recipe",
			in:   nil,
			want: nil,
		},
		{
			name: "helm component carries version and namespace",
			in: &RecipeResult{Components: []ComponentRef{
				{Name: "synthetic-alpha", Kind: "Helm", Version: "1.2.0", Namespace: "synthetic-ns"},
			}},
			want: map[string]upgrade.Identity{
				"synthetic-alpha": {Version: "1.2.0", Namespace: "synthetic-ns"},
			},
		},
		{
			name: "kustomize component falls back to its tag",
			in: &RecipeResult{Components: []ComponentRef{
				{Name: "synthetic-kustomize", Kind: "Kustomize", Tag: "v1.0.0", Namespace: "synthetic-ns"},
			}},
			want: map[string]upgrade.Identity{
				"synthetic-kustomize": {Version: "v1.0.0", Namespace: "synthetic-ns"},
			},
		},
		{
			name: "an unversioned component is carried, not dropped",
			in: &RecipeResult{Components: []ComponentRef{
				{Name: "synthetic-unpinned", Kind: "Helm", Namespace: "synthetic-ns"},
			}},
			want: map[string]upgrade.Identity{
				"synthetic-unpinned": {Namespace: "synthetic-ns"},
			},
		},
		{
			name: "object names attach to the component that pins them",
			in: &RecipeResult{Components: []ComponentRef{
				{Name: "synthetic-alpha", Kind: "Helm", Version: "1.2.0", Namespace: "synthetic-ns"},
				{Name: "synthetic-beta", Kind: "Helm", Version: "2.0.0", Namespace: "synthetic-ns"},
			}},
			objectNames: map[string]map[string]string{
				"synthetic-alpha": {"fullnameOverride": "alpha"},
			},
			want: map[string]upgrade.Identity{
				"synthetic-alpha": {
					Version:     "1.2.0",
					Namespace:   "synthetic-ns",
					ObjectNames: map[string]string{"fullnameOverride": "alpha"},
				},
				"synthetic-beta": {Version: "2.0.0", Namespace: "synthetic-ns"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := componentIdentities(tt.in, tt.objectNames)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("componentIdentities() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

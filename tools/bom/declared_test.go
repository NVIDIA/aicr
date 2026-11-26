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
	"context"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/helm/helmtest"
)

const declaredFixture = `component: %s
charts:
  - repository: https://charts.example.com
    chart: example/op
    sourceVersion: 1.0.0
    images:
      - image: example.com/op/initc:1.0.0
        source: INIT_IMAGE env of the operator
      - image: example.com/op/extra:1.0.0
        enabledBy: extra.enabled
        source: rendered only when extra.enabled is true
  - repository: oci://mirror.example.com/charts
    chart: op
    sourceVersion: 0.9.0
    images:
      - image: mirror.example.com/op/initc:0.9.0
        source: INIT_IMAGE env of the operator
manifests:
  - operatorImage: example.com/op/operator:v1
    images:
      - image: docker.io/library/alpine:3.10
        source: compiled-in default
`

// declaredTree returns a recipes tree whose component name has content as its
// images.yaml, or no images.yaml when content is empty.
func declaredTree(t *testing.T, name, content string) string {
	t.Helper()
	if content == "" {
		return writeRecipeTree(t, nil)
	}
	return writeRecipeTree(t, map[string]string{"components/" + name + "/images.yaml": content})
}

func TestReadDeclaredImages(t *testing.T) {
	helmComp := func(name string) component {
		return component{Name: name, Helm: helmCfg{DefaultRepository: "https://charts.example.com",
			DefaultChart: "example/op", DefaultVersion: "1.0.0"}}
	}
	fixture := func(name string) string { return strings.ReplaceAll(declaredFixture, "%s", name) }

	tests := []struct {
		name     string
		root     func(t *testing.T) string
		comp     component
		version  string
		rendered []string
		want     []string
		wantCode errors.ErrorCode // empty when no error is expected
		wantMsg  string
	}{
		{
			name: "helm entry at the surveyed version, gated images included",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("op")) },
			comp: helmComp("op"), version: "1.0.0",
			want: []string{"example.com/op/initc:1.0.0", "example.com/op/extra:1.0.0"},
		},
		{
			name: "variant version declared only for another publisher fails",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("op")) },
			comp: helmComp("op"), version: "0.9.0",
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "no entry for chart https://charts.example.com op@0.9.0",
		},
		{
			name: "variant version from an undeclared repository uses the only entry",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("op")) },
			comp: component{Name: "op", Helm: helmCfg{DefaultRepository: "oci://registry.internal/charts",
				DefaultChart: "op", DefaultVersion: "0.9.0"}}, version: "0.9.0",
			want: []string{"mirror.example.com/op/initc:0.9.0"},
		},
		{
			name: "helm version with no entry",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("op")) },
			comp: helmComp("op"), version: "2.0.0",
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `component "op": no entry for chart https://charts.example.com op@2.0.0`,
		},
		{
			name: "manifest entry by operator image",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("op")) },
			comp: component{Name: "op"}, rendered: []string{"example.com/op/operator:v1"},
			want: []string{"docker.io/library/alpine:3.10"},
		},
		{
			name: "manifest with a different operator image",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("op")) },
			comp: component{Name: "op"}, rendered: []string{"example.com/op/operator:v2"},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `component "op": no entry for an operator image`,
		},
		{
			name: "absent file for an optional component",
			root: func(t *testing.T) string { return declaredTree(t, "op", "") },
			comp: helmComp("op"), version: "1.0.0",
		},
		{
			name: "absent recipes root for an optional component",
			root: func(t *testing.T) string { return t.TempDir() },
			comp: helmComp("op"), version: "1.0.0",
		},
		{
			name: "absent file for a required component",
			root: func(t *testing.T) string { return declaredTree(t, "grove", "") },
			comp: helmComp("grove"), version: "1.0.0",
			wantCode: errors.ErrCodeNotFound, wantMsg: "required for this component",
		},
		{
			name: "absent recipes root for a required component",
			root: func(t *testing.T) string { return t.TempDir() },
			comp: helmComp("grove"), version: "1.0.0",
			wantCode: errors.ErrCodeNotFound, wantMsg: "required for this component",
		},
		{
			name: "file names another component",
			root: func(t *testing.T) string { return declaredTree(t, "op", fixture("other")) },
			comp: helmComp("op"), version: "1.0.0",
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `of component "op" name component "other"`,
		},
		{
			name: "malformed file",
			root: func(t *testing.T) string { return declaredTree(t, "op", "component: op\nbogus: true\n") },
			comp: helmComp("op"), version: "1.0.0",
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "parse declared images",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readDeclaredImages(tt.root(t), tt.comp, tt.version, tt.rendered)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("readDeclaredImages() = %v, want %s error", got, tt.wantCode)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("readDeclaredImages() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("readDeclaredImages() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSurveyComponentMergesDeclaredImages(t *testing.T) {
	root := declaredTree(t, "op", strings.ReplaceAll(declaredFixture, "%s", "op"))
	mock := &helmtest.MockRenderer{Rendered: map[string][]byte{"op": []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: op
spec:
  template:
    spec:
      containers:
      - name: op
        image: example.com/op/operator:1.0.0
      - name: dup
        image: example.com/op/initc:1.0.0
`)}}
	c := component{Name: "op", Helm: helmCfg{DefaultRepository: "https://charts.example.com",
		DefaultChart: "example/op", DefaultVersion: "1.0.0"}}

	res, err := surveyComponent(context.Background(), root, c, mock, false, catalogDriverTargets{})
	if err != nil {
		t.Fatalf("surveyComponent() error = %v", err)
	}
	want := []string{"example.com/op/extra:1.0.0", "example.com/op/initc:1.0.0", "example.com/op/operator:1.0.0"}
	if !slices.Equal(res.Images, want) {
		t.Errorf("images = %v, want %v (declared merged, sorted, deduplicated)", res.Images, want)
	}
}

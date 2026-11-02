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

package localformat_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// TestReleaseNames_MatchesWrite pins ReleaseNames to the folders Write
// actually emits, so the two cannot drift, and checks that ReleaseNames
// leaves the output directory untouched.
func TestReleaseNames_MatchesWrite(t *testing.T) {
	upstream := func(name string) localformat.Component {
		return localformat.Component{
			Name:       name,
			Namespace:  name,
			Repository: "https://example.invalid/charts",
			ChartName:  "example/" + name,
			Version:    "v1.0.0",
		}
	}
	manifestOnly := localformat.Component{Name: "bar", Namespace: "bar"}
	configMap := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: {{ .Release.Namespace }}\n")
	emptyRender := []byte("{{- if false }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: never\n{{- end }}\n")

	tests := []struct {
		name string
		opts localformat.Options
		want []string
	}{
		{
			name: "primary only",
			opts: localformat.Options{Components: []localformat.Component{upstream("foo")}},
			want: []string{"foo"},
		},
		{
			name: "pre, primary, post, and readiness in emission order",
			opts: localformat.Options{
				Components:             []localformat.Component{upstream("foo"), upstream("baz")},
				ComponentPreManifests:  map[string]map[string][]byte{"foo": {"pre.yaml": configMap}},
				ComponentPostManifests: map[string]map[string][]byte{"foo": {"post.yaml": configMap}},
				ComponentReadiness:     map[string]map[string][]byte{"foo": {"readiness.yaml": configMap}},
			},
			want: []string{"foo-pre", "foo", "foo-post", "foo-readiness", "baz"},
		},
		{
			name: "pre-manifest rendering no object injects no pre release",
			opts: localformat.Options{
				Components:            []localformat.Component{upstream("foo")},
				ComponentPreManifests: map[string]map[string][]byte{"foo": {"pre.yaml": emptyRender}},
			},
			want: []string{"foo"},
		},
		{
			name: "manifest-only component wraps post manifests in its primary",
			opts: localformat.Options{
				Components:             []localformat.Component{manifestOnly},
				ComponentPostManifests: map[string]map[string][]byte{"bar": {"post.yaml": configMap}},
			},
			want: []string{"bar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			planDir := filepath.Join(t.TempDir(), "plan")
			planOpts := tt.opts
			planOpts.OutputDir = planDir
			got, err := localformat.ReleaseNames(ctx, planOpts)
			if err != nil {
				t.Fatalf("ReleaseNames() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReleaseNames() = %v, want %v", got, tt.want)
			}
			if _, statErr := os.Stat(planDir); !os.IsNotExist(statErr) {
				t.Errorf("ReleaseNames() touched OutputDir: stat error = %v", statErr)
			}

			writeOpts := tt.opts
			writeOpts.OutputDir = t.TempDir()
			res, err := localformat.Write(ctx, writeOpts)
			if err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			written := make([]string, 0, len(res.Folders))
			for _, f := range res.Folders {
				written = append(written, f.Name)
			}
			if !reflect.DeepEqual(got, written) {
				t.Errorf("ReleaseNames() = %v, but Write emitted %v", got, written)
			}
		})
	}
}

// TestReleaseNames_DeadContext checks that a dead context is classified by
// cause: a user abort must report CANCELED, not a retryable TIMEOUT.
func TestReleaseNames_DeadContext(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		want errors.ErrorCode
	}{
		{"canceled", canceled, errors.ErrCodeCanceled},
		{"deadline exceeded", expired, errors.ErrCodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := localformat.ReleaseNames(tt.ctx, localformat.Options{})
			errorstest.WantReportedCode(t, err, tt.want)
		})
	}
}

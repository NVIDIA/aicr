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

package os

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCollectKModParam pins how allowlisted module parameters are read: the
// file's trimmed content keyed by its path, and no key at all when the module
// is not loaded, so a constraint on it reads unavailable rather than a value.
func TestCollectKModParam(t *testing.T) {
	tests := []struct {
		name    string
		content *string
		want    map[string]string
	}{
		{"exclusive", ptrTo("N\n"), map[string]string{"netns_mode": "N"}},
		{"shared", ptrTo("Y\n"), map[string]string{"netns_mode": "Y"}},
		{"module not loaded", nil, map[string]string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "netns_mode")
			if tt.content != nil {
				if err := os.WriteFile(path, []byte(*tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			original := kmodParamPaths
			kmodParamPaths = []string{path}
			t.Cleanup(func() { kmodParamPaths = original })

			subtype, err := (&Collector{}).collectKModParam(context.Background())
			if err != nil {
				t.Fatalf("collectKModParam() error = %v", err)
			}
			if subtype.Name != SubtypeKModParam {
				t.Errorf("subtype name = %q, want %q", subtype.Name, SubtypeKModParam)
			}
			if len(subtype.Data) != len(tt.want) {
				t.Fatalf("readings = %v, want %v", subtype.Data, tt.want)
			}
			for base, want := range tt.want {
				reading, ok := subtype.Data[filepath.Join(filepath.Dir(path), base)]
				if !ok {
					t.Fatalf("missing reading for %s in %v", base, subtype.Data)
				}
				if got := reading.Any(); got != want {
					t.Errorf("%s = %v, want %q", base, got, want)
				}
			}
		})
	}
}

// TestCollectKModParam_ReadError pins that a parameter that exists but cannot
// be read fails the subtype rather than reading as absent.
func TestCollectKModParam_ReadError(t *testing.T) {
	original := kmodParamPaths
	kmodParamPaths = []string{t.TempDir()}
	t.Cleanup(func() { kmodParamPaths = original })

	if _, err := (&Collector{}).collectKModParam(context.Background()); err == nil {
		t.Fatal("expected an error reading a directory as a module parameter")
	}
}

func TestCollectKModParam_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Collector{}).collectKModParam(ctx); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}

func ptrTo(s string) *string { return &s }

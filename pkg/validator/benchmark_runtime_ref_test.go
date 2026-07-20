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

package validator

import (
	"context"
	stderrors "errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// fakeDataProvider serves canned file content for the resolver test. A nil
// files map with readErr set simulates an unreadable/missing --data file.
type fakeDataProvider struct {
	files   map[string][]byte
	readErr error
	reads   []string // paths ReadFile was called with
}

func (f *fakeDataProvider) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	if f.readErr != nil {
		return nil, f.readErr
	}
	if b, ok := f.files[path]; ok {
		return b, nil
	}
	return nil, errors.New(errors.ErrCodeNotFound, "no such file: "+path)
}

func (f *fakeDataProvider) WalkDir(_ context.Context, _ string, _ fs.WalkDirFunc) error { return nil }
func (f *fakeDataProvider) Source(string) string                                        { return "fake" }

const testBenchmarkRuntime = `apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
spec:
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            spec:
              template:
                spec:
                  containers:
                    - name: node
`

// notATrainingRuntime is valid YAML but the wrong kind — used to exercise the
// orchestrator's resolve-time shape check.
const notATrainingRuntime = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nope\n"

func perfInput(cs ...recipe.Constraint) *v1.ValidationInput {
	return &v1.ValidationInput{
		Config: v1.ValidationConfig{
			Performance: &v1.ValidationPhase{Constraints: cs},
		},
	}
}

func ncclRuntimeCarrier(vi *v1.ValidationInput) (string, bool) {
	if vi.Config.Performance == nil {
		return "", false
	}
	c, ok := v1.FindConstraint(vi.Config.Performance.Constraints, v1.PerfConstraintNCCLBenchmarkRuntime)
	return c.Value, ok
}

func TestResolveBenchmarkRuntimeRef(t *testing.T) {
	const goodPath = "validators/performance/testdata/gb200/mycloud/runtime.yaml"

	refC := func(v string) recipe.Constraint {
		return recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntimeRef, Value: v}
	}

	tests := []struct {
		name        string
		input       *v1.ValidationInput
		dp          recipe.DataProvider
		wantCarrier string // expected injected nccl-benchmark-runtime value; "" = none injected
		wantErr     bool
		wantTimeout bool // expect a transient (ErrCodeTimeout) error rather than ErrCodeInvalidRequest
		wantErrSub  string
	}{
		{
			name:  "nil performance phase → no-op",
			input: &v1.ValidationInput{},
			dp:    &fakeDataProvider{},
		},
		{
			name:  "no ref constraint → no-op",
			input: perfInput(recipe.Constraint{Name: "nccl-all-reduce-bw", Value: ">= 450"}),
			dp:    &fakeDataProvider{},
		},
		{
			name:  "blank ref → no-op",
			input: perfInput(refC("   ")),
			dp:    &fakeDataProvider{},
		},
		{
			name:        "valid ref resolves file into carrier",
			input:       perfInput(refC("gb200/mycloud")),
			dp:          &fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}},
			wantCarrier: testBenchmarkRuntime,
		},
		{
			name: "valid ref drops a pre-existing blank carrier",
			input: perfInput(
				refC("gb200/mycloud"),
				recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntime, Value: ""},
			),
			dp:          &fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}},
			wantCarrier: testBenchmarkRuntime,
		},
		{
			name:       "malformed ref (one segment) fails closed",
			input:      perfInput(refC("gb200")),
			dp:         &fakeDataProvider{},
			wantErr:    true,
			wantErrSub: "{accelerator}/{service}",
		},
		{
			name:       "traversal ref fails closed",
			input:      perfInput(refC("../secrets")),
			dp:         &fakeDataProvider{},
			wantErr:    true,
			wantErrSub: "{accelerator}/{service}",
		},
		{
			name:       "missing file fails closed",
			input:      perfInput(refC("gb200/mycloud")),
			dp:         &fakeDataProvider{files: map[string][]byte{}},
			wantErr:    true,
			wantErrSub: "expected",
		},
		{
			name:        "transient provider error preserves timeout code",
			input:       perfInput(refC("gb200/mycloud")),
			dp:          &fakeDataProvider{readErr: errors.New(errors.ErrCodeTimeout, "context deadline exceeded")},
			wantErr:     true,
			wantTimeout: true,
		},
		{
			name:       "duplicate refs fail closed (blank must not hide non-blank)",
			input:      perfInput(refC(""), refC("gb200/mycloud")),
			dp:         &fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}},
			wantErr:    true,
			wantErrSub: "at most one",
		},
		{
			name: "duplicate carriers fail closed",
			input: perfInput(
				refC("gb200/mycloud"),
				recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntime, Value: ""},
				recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntime, Value: testBenchmarkRuntime},
			),
			dp:         &fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}},
			wantErr:    true,
			wantErrSub: "at most one",
		},
		{
			name:       "empty file fails closed",
			input:      perfInput(refC("gb200/mycloud")),
			dp:         &fakeDataProvider{files: map[string][]byte{goodPath: []byte("   \n")}},
			wantErr:    true,
			wantErrSub: "empty file",
		},
		{
			name:       "resolved file is not a TrainingRuntime fails closed",
			input:      perfInput(refC("gb200/mycloud")),
			dp:         &fakeDataProvider{files: map[string][]byte{goodPath: []byte(notATrainingRuntime)}},
			wantErr:    true,
			wantErrSub: "resolved to an invalid runtime",
		},
		{
			name: "ref alongside a profile fails closed",
			input: perfInput(
				refC("gb200/mycloud"),
				recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkProfile, Value: "gb200/eks"},
			),
			dp:         &fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}},
			wantErr:    true,
			wantErrSub: "mutually exclusive",
		},
		{
			name:       "ref set but no data provider fails closed",
			input:      perfInput(refC("gb200/mycloud")),
			dp:         nil,
			wantErr:    true,
			wantErrSub: "no --data source",
		},
		{
			name: "ref alongside inline carrier fails closed",
			input: perfInput(
				refC("gb200/mycloud"),
				recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntime, Value: testBenchmarkRuntime},
			),
			dp:         &fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}},
			wantErr:    true,
			wantErrSub: "both set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []Option{}
			if tt.dp != nil {
				opts = append(opts, WithDataProvider(tt.dp))
			}
			v := New(opts...)

			err := v.resolveBenchmarkRuntimeRef(context.Background(), tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				wantCode := errors.ErrCodeInvalidRequest
				if tt.wantTimeout {
					wantCode = errors.ErrCodeTimeout
				}
				if !stderrors.Is(err, errors.New(wantCode, "")) {
					t.Errorf("error code = %v, want %v", err, wantCode)
				}
				if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			got, ok := ncclRuntimeCarrier(tt.input)
			if tt.wantCarrier == "" {
				if ok {
					t.Errorf("carrier unexpectedly injected: %q", got)
				}
				return
			}
			if !ok || got != tt.wantCarrier {
				t.Errorf("carrier = %q (present=%v), want %q", got, ok, tt.wantCarrier)
			}
			// Exactly one carrier must remain — any pre-existing (blank) one is
			// dropped so first-match consumers see the resolved value.
			if n := v1.CountConstraint(tt.input.Config.Performance.Constraints, v1.PerfConstraintNCCLBenchmarkRuntime); n != 1 {
				t.Errorf("carrier count = %d, want exactly 1", n)
			}
		})
	}
}

// TestResolveBenchmarkRuntimeRefIdempotent proves a second resolution pass on
// the same input is a no-op: the first pass consumes the ref (drops it and
// appends the carrier), so the second finds no ref and does not re-trip the
// ref+inline guard.
func TestResolveBenchmarkRuntimeRefIdempotent(t *testing.T) {
	const goodPath = "validators/performance/testdata/gb200/mycloud/runtime.yaml"
	vi := perfInput(recipe.Constraint{Name: v1.PerfConstraintNCCLBenchmarkRuntimeRef, Value: "gb200/mycloud"})
	v := New(WithDataProvider(&fakeDataProvider{files: map[string][]byte{goodPath: []byte(testBenchmarkRuntime)}}))

	for i := 1; i <= 2; i++ {
		if err := v.resolveBenchmarkRuntimeRef(context.Background(), vi); err != nil {
			t.Fatalf("pass %d: unexpected error: %v", i, err)
		}
	}

	// Ref consumed, exactly one carrier present with the file content.
	if _, ok := v1.FindConstraint(vi.Config.Performance.Constraints, v1.PerfConstraintNCCLBenchmarkRuntimeRef); ok {
		t.Errorf("ref constraint should have been consumed after resolution")
	}
	carriers := 0
	for _, c := range vi.Config.Performance.Constraints {
		if c.Name == v1.PerfConstraintNCCLBenchmarkRuntime {
			carriers++
			if c.Value != testBenchmarkRuntime {
				t.Errorf("carrier value = %q, want the file content", c.Value)
			}
		}
	}
	if carriers != 1 {
		t.Errorf("carrier count = %d, want exactly 1", carriers)
	}
}

func TestBenchmarkRuntimeRefPath(t *testing.T) {
	tests := []struct {
		ref     string
		want    string
		wantErr bool
	}{
		{ref: "gb200/mycloud", want: "validators/performance/testdata/gb200/mycloud/runtime.yaml"},
		{ref: "  h100 / eks ", want: "validators/performance/testdata/h100/eks/runtime.yaml"},
		{ref: "gb200", wantErr: true},
		{ref: "gb200/eks/net", wantErr: true},
		{ref: "/eks", wantErr: true},
		{ref: "gb200/", wantErr: true},
		{ref: "../x", wantErr: true},
		{ref: "gb200/..", wantErr: true},
		{ref: ".", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := benchmarkRuntimeRefPath(tt.ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

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

package localformat

import (
	"context"
	stderrors "errors"
	"os"
	"os/exec"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestValidateForPull(t *testing.T) {
	tests := []struct {
		name    string
		c       Component
		wantErr bool
		code    string
	}{
		{"missing repo", Component{Name: "x"}, true, string(errors.ErrCodeInvalidRequest)},
		{"missing version", Component{Name: "x", Repository: "https://r"}, true, string(errors.ErrCodeInvalidRequest)},
		{"valid http", Component{Name: "x", Repository: "http://r", Version: "1"}, false, ""},
		{"valid https", Component{Name: "x", Repository: "https://r", Version: "1"}, false, ""},
		{"valid oci", Component{Name: "x", Repository: "oci://r/c", Version: "1", IsOCI: true}, false, ""},
		{"bare hostname rejected", Component{Name: "x", Repository: "r.example.com", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"file scheme rejected", Component{Name: "x", Repository: "file:///tmp", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"oci scheme without IsOCI rejected", Component{Name: "x", Repository: "oci://r/c", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"IsOCI without oci scheme rejected", Component{Name: "x", Repository: "https://r/c", Version: "1", IsOCI: true}, true, string(errors.ErrCodeInvalidRequest)},
		{"flag-looking chart name rejected", Component{Name: "x", ChartName: "--insecure-skip-tls-verify", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"flag-looking component name rejected", Component{Name: "--ca-file=evil", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"flag-looking version rejected", Component{Name: "x", Repository: "https://r", Version: "-rce"}, true, string(errors.ErrCodeInvalidRequest)},
		// Regex defense-in-depth: rejects values that don't lead with `-`
		// but contain shell-meta or whitespace which could matter if the
		// invocation path ever changes (env-var, shell wrapper, etc.).
		{"chart name with space rejected", Component{Name: "x", ChartName: "foo bar", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"chart name with shell meta rejected", Component{Name: "x", ChartName: "foo;rm", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		{"chart name with leading dot rejected", Component{Name: "x", ChartName: ".hidden", Repository: "https://r", Version: "1"}, true, string(errors.ErrCodeInvalidRequest)},
		// Legitimate inputs that the regex must NOT reject:
		{"chart name with hyphen accepted", Component{Name: "x", ChartName: "gpu-operator", Repository: "https://r", Version: "1.2.3"}, false, ""},
		{"chart name with dot accepted", Component{Name: "x", ChartName: "v8.21.runtime", Repository: "https://r", Version: "1"}, false, ""},
		{"semver version with build metadata accepted", Component{Name: "x", Repository: "https://r", Version: "1.2.3+build.7"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateForPull(tt.c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.code != "" {
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("expected StructuredError, got: %T (%v)", err, err)
				}
				if string(se.Code) != tt.code {
					t.Errorf("Code = %s, want %s", se.Code, tt.code)
				}
			}
		})
	}
}

func TestShouldVendor(t *testing.T) {
	tests := []struct {
		name string
		c    Component
		want bool
	}{
		{"helm component", Component{Repository: "https://r"}, true},
		{"oci component", Component{Repository: "oci://r/c", IsOCI: true}, true},
		{"manifest only", Component{Repository: ""}, false},
		{"kustomize tag", Component{Repository: "https://git/repo", Tag: "v1"}, false},
		{"kustomize path", Component{Repository: "https://git/repo", Path: "p"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldVendor(tt.c); got != tt.want {
				t.Errorf("shouldVendor(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

func TestClassifyHelmCLIError(t *testing.T) {
	c := Component{Name: "x", ChartName: "foo", Version: "1.0", Repository: "https://r"}
	tests := []struct {
		name   string
		runErr error
		stderr string
		want   errors.ErrorCode
	}{
		// Substring-fallback path (runErr is a generic "exit status 1"):
		{"binary missing (stderr fallback)", stderrors.New("exit status 1"), "exec: \"helm\": executable file not found in $PATH", errors.ErrCodeUnavailable},
		{"chart not found", stderrors.New("exit status 1"), `Error: chart "foo" version "1.0" not found`, errors.ErrCodeNotFound},
		{"http 404", stderrors.New("exit status 1"), "Error: failed to fetch https://r/foo-1.0.tgz: 404 Not Found", errors.ErrCodeNotFound},
		{"unauthorized", stderrors.New("exit status 1"), "Error: failed to authorize: unauthorized: authentication required", errors.ErrCodeUnauthorized},
		{"forbidden 403", stderrors.New("exit status 1"), "Error: 403 Forbidden", errors.ErrCodeUnauthorized},
		{"context deadline", stderrors.New("exit status 1"), "Error: context deadline exceeded", errors.ErrCodeTimeout},
		{"signal killed", stderrors.New("exit status 1"), "signal: killed", errors.ErrCodeTimeout},
		{"dns failure", stderrors.New("exit status 1"), "Error: dial tcp: lookup r: no such host", errors.ErrCodeUnavailable},
		{"connection refused", stderrors.New("exit status 1"), "Error: dial tcp: connection refused", errors.ErrCodeUnavailable},
		{"generic", stderrors.New("exit status 1"), "Error: something bizarre and unexpected", errors.ErrCodeInternal},

		// Typed-sentinel path: the classifier checks errors.Is BEFORE the
		// stderr substring fallback. Both sentinels must classify as
		// ErrCodeUnavailable (binary missing) regardless of stderr.
		{"binary missing (exec.ErrNotFound sentinel)", exec.ErrNotFound, "", errors.ErrCodeUnavailable},
		{"binary missing (os.ErrNotExist sentinel)", os.ErrNotExist, "", errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyHelmCLIError(c, tt.runErr, tt.stderr)
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("not a StructuredError: %v", err)
			}
			if se.Code != tt.want {
				t.Errorf("Code = %v, want %v\nrunErr: %v\nstderr: %s", se.Code, tt.want, tt.runErr, tt.stderr)
			}
		})
	}
}

func TestCLIChartPuller_NoBinary(t *testing.T) {
	// Point HelmBin at a path that definitely doesn't exist so the test
	// is hermetic regardless of whether `helm` is installed locally.
	p := &CLIChartPuller{HelmBin: "/nonexistent/aicr-test/helm"}
	c := Component{Name: "x", ChartName: "foo", Version: "1", Repository: "https://r"}

	tgz, _, _, err := p.Pull(context.Background(), c)
	if err == nil {
		t.Fatalf("expected error from missing helm binary; got %d bytes", len(tgz))
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("not a StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("Code = %v, want %v", se.Code, errors.ErrCodeUnavailable)
	}
}

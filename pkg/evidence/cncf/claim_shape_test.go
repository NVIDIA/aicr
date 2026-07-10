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

package cncf

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// runEmitClaim writes the embedded evidence script to a temp file and invokes
// its hidden `emit-claim` subcommand — added exactly for this test so the
// SHIPPED emitter function is exercised (not a copy) without contacting a
// cluster. Returns the emitted YAML and the command error.
func runEmitClaim(t *testing.T, version string) ([]byte, error) {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "collect-evidence.sh")
	if err := os.WriteFile(scriptPath, collectScript, 0o700); err != nil { //nolint:gosec // test script needs execute permission
		t.Fatalf("failed to write embedded script: %v", err)
	}
	//nolint:gosec // fixed binary name; args are test-table constants
	return exec.Command("bash", scriptPath, "emit-claim",
		version, "test-claim", "test-ns", "gpu.nvidia.com").Output()
}

// nestedMap walks a decoded YAML document through map keys, failing the test
// when an intermediate value is missing or not a map.
func nestedMap(t *testing.T, obj map[string]any, path ...string) map[string]any {
	t.Helper()
	current := obj
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("path %v: key %q missing or not a map (got %T)", path, key, current[key])
		}
		current = next
	}
	return current
}

// TestEmitResourceClaimShapes asserts the evidence script emits the
// version-correct ResourceClaim schema for every resource.k8s.io API version
// the collector supports, matching the vendored Kubernetes types
// (vendor/k8s.io/api/resource/{v1,v1beta2,v1beta1}/types.go):
//
//   - v1 and v1beta2 wrap the request in `exactly`
//     (spec.devices.requests[0].exactly.deviceClassName) and must NOT carry a
//     bare deviceClassName on the request;
//   - v1beta1 has no `exactly` wrapper — deviceClassName sits directly on the
//     request.
func TestEmitResourceClaimShapes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping script emitter test")
	}

	tests := []struct {
		name        string
		version     string
		wantExactly bool
	}{
		{"v1 uses exactly wrapper", "v1", true},
		{"v1beta2 uses exactly wrapper", "v1beta2", true},
		{"v1beta1 uses bare deviceClassName", "v1beta1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runEmitClaim(t, tt.version)
			if err != nil {
				t.Fatalf("emit-claim %s failed: %v (output: %s)", tt.version, err, out)
			}

			var claim map[string]any
			if err := yaml.Unmarshal(out, &claim); err != nil {
				t.Fatalf("emitted YAML does not parse: %v\n%s", err, out)
			}

			wantAPIVersion := "resource.k8s.io/" + tt.version
			if got := claim["apiVersion"]; got != wantAPIVersion {
				t.Errorf("apiVersion = %v, want %s", got, wantAPIVersion)
			}
			if got := claim["kind"]; got != "ResourceClaim" {
				t.Errorf("kind = %v, want ResourceClaim", got)
			}
			metadata := nestedMap(t, claim, "metadata")
			if got := metadata["name"]; got != "test-claim" {
				t.Errorf("metadata.name = %v, want test-claim", got)
			}
			if got := metadata["namespace"]; got != "test-ns" {
				t.Errorf("metadata.namespace = %v, want test-ns", got)
			}

			devices := nestedMap(t, claim, "spec", "devices")
			requests, ok := devices["requests"].([]any)
			if !ok || len(requests) != 1 {
				t.Fatalf("spec.devices.requests = %v, want exactly one request", devices["requests"])
			}
			request, ok := requests[0].(map[string]any)
			if !ok {
				t.Fatalf("spec.devices.requests[0] is %T, want map", requests[0])
			}
			if got := request["name"]; got != "gpu" {
				t.Errorf("request name = %v, want gpu", got)
			}

			// carrier is where deviceClassName/allocationMode/count must live:
			// the `exactly` wrapper for v1/v1beta2, the request itself for
			// v1beta1.
			carrier := request
			if tt.wantExactly {
				if _, bare := request["deviceClassName"]; bare {
					t.Errorf("%s request carries a bare deviceClassName — must be nested under exactly", tt.version)
				}
				exactly, ok := request["exactly"].(map[string]any)
				if !ok {
					t.Fatalf("%s request missing the exactly wrapper (got %T)", tt.version, request["exactly"])
				}
				carrier = exactly
			} else {
				if _, wrapped := request["exactly"]; wrapped {
					t.Errorf("%s request carries an exactly wrapper — v1beta1 has no such field", tt.version)
				}
			}
			if got := carrier["deviceClassName"]; got != "gpu.nvidia.com" {
				t.Errorf("deviceClassName = %v, want gpu.nvidia.com", got)
			}
			if got := carrier["allocationMode"]; got != "ExactCount" {
				t.Errorf("allocationMode = %v, want ExactCount", got)
			}
			if got := carrier["count"]; got != 1 {
				t.Errorf("count = %v (%T), want 1", got, got)
			}
		})
	}
}

// TestEmitResourceClaimRejectsUnknownVersion asserts the emitter fails closed
// on an unsupported resource.k8s.io version instead of guessing a shape.
func TestEmitResourceClaimRejectsUnknownVersion(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping script emitter test")
	}

	tests := []struct {
		name    string
		version string
	}{
		{"unknown version", "v2"},
		{"empty version", ""},
		{"retired alpha version", "v1alpha3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runEmitClaim(t, tt.version)
			if err == nil {
				t.Errorf("emit-claim %q succeeded, want failure (output: %s)", tt.version, out)
			}
		})
	}
}

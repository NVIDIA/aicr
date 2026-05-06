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

package config_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/config"
)

const validYAML = `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-training
spec:
  recipe:
    criteria:
      service: eks
      accelerator: gb200
      intent: training
      os: ubuntu
      platform: kubeflow
      nodes: 8
  bundle:
    deployment:
      deployer: argocd
      repo: https://example.git/charts
      set:
        - gpuoperator:driver.version=570.86.16
    scheduling:
      systemNodeSelector:
        role: system
      acceleratedNodeTolerations:
        - "nvidia.com/gpu=present:NoSchedule"
      nodes: 8
`

const validJSON = `{
  "kind": "AICRConfig",
  "apiVersion": "aicr.nvidia.com/v1alpha1",
  "metadata": {"name": "test"},
  "spec": {
    "recipe": {
      "criteria": {"service": "eks", "accelerator": "h100", "intent": "training"}
    }
  }
}`

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func TestLoad_File_YAML(t *testing.T) {
	path := writeTempFile(t, "config.yaml", validYAML)
	cfg, err := config.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Spec.Recipe == nil || cfg.Spec.Recipe.Criteria.Accelerator != "gb200" {
		t.Errorf("unexpected criteria: %+v", cfg.Spec.Recipe)
	}
	if cfg.Spec.Bundle == nil || cfg.Spec.Bundle.Deployment.Deployer != "argocd" {
		t.Errorf("unexpected bundle: %+v", cfg.Spec.Bundle)
	}
	if got := cfg.Spec.Bundle.Scheduling.SystemNodeSelector["role"]; got != "system" {
		t.Errorf("systemNodeSelector role = %q, want system", got)
	}
}

func TestLoad_File_JSON(t *testing.T) {
	path := writeTempFile(t, "config.json", validJSON)
	cfg, err := config.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Spec.Recipe.Criteria.Service != "eks" {
		t.Errorf("expected service eks, got %q", cfg.Spec.Recipe.Criteria.Service)
	}
}

func TestLoad_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(validYAML))
	}))
	t.Cleanup(srv.Close)

	cfg, err := config.Load(context.Background(), srv.URL+"/config.yaml")
	if err != nil {
		t.Fatalf("Load HTTP: %v", err)
	}
	if cfg.Spec.Recipe.Criteria.Accelerator != "gb200" {
		t.Errorf("unexpected accelerator: %q", cfg.Spec.Recipe.Criteria.Accelerator)
	}
}

func TestLoad_RejectsConfigMapURI(t *testing.T) {
	_, err := config.Load(context.Background(), "cm://gpu-operator/aicr-config")
	if err == nil {
		t.Fatal("expected error for cm:// URI, got nil")
	}
	if !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("error %q does not mention ConfigMap", err.Error())
	}
}

func TestLoad_RejectsFileURI(t *testing.T) {
	_, err := config.Load(context.Background(), "file:///etc/aicr/config.yaml")
	if err == nil {
		t.Fatal("expected error for file:// URI, got nil")
	}
	if !strings.Contains(err.Error(), "file://") {
		t.Errorf("error %q should mention file://", err.Error())
	}
}

func TestLoad_EmptySource(t *testing.T) {
	_, err := config.Load(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty source, got nil")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load(context.Background(), "/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTempFile(t, "config.yaml", "kind: AICRConfig\n  bad: indent")
	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

func TestLoad_FailsValidation(t *testing.T) {
	// Wrong kind should fail validation
	bad := strings.ReplaceAll(validYAML, "kind: AICRConfig", "kind: SomethingElse")
	path := writeTempFile(t, "config.yaml", bad)
	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid kind") {
		t.Errorf("expected 'invalid kind' in error, got %q", err.Error())
	}
}

func TestLoad_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := config.Load(ctx, writeTempFile(t, "config.yaml", validYAML))
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
}

func TestLoad_RejectsUnknownYAMLField(t *testing.T) {
	bad := `kind: AICRConfig
apiVersion: aicr.nvidia.com/v1alpha1
spec:
  recipe:
    criteria:
      service: eks
      bogusFieldThatDoesNotExist: oops
`
	path := writeTempFile(t, "config.yaml", bad)
	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for unknown YAML field, got nil")
	}
	if !strings.Contains(err.Error(), "bogusFieldThatDoesNotExist") {
		t.Errorf("error should reference unknown field, got %q", err.Error())
	}
}

func TestLoad_RejectsUnknownJSONField(t *testing.T) {
	bad := `{"kind":"AICRConfig","apiVersion":"aicr.nvidia.com/v1alpha1","spec":{"recipe":{"criteria":{"service":"eks","bogusFieldThatDoesNotExist":"oops"}}}}`
	path := writeTempFile(t, "config.json", bad)
	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for unknown JSON field, got nil")
	}
	if !strings.Contains(err.Error(), "bogusFieldThatDoesNotExist") {
		t.Errorf("error should reference unknown field, got %q", err.Error())
	}
}

// TestLoad_HTTPBodyLimit verifies the HTTP fetch path bounds response size
// against defaults.HTTPResponseBodyLimit.
func TestLoad_HTTPBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Write more than the body limit (1 MiB by default; emit 2 MiB).
		w.Header().Set("Content-Type", "application/yaml")
		// Valid prefix then padding so YAML parsing isn't the failure mode.
		_, _ = w.Write([]byte(validYAML))
		pad := make([]byte, 2*1024*1024)
		_, _ = w.Write(pad)
	}))
	t.Cleanup(srv.Close)
	_, err := config.Load(context.Background(), srv.URL+"/big.yaml")
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error should mention size limit, got %q", err.Error())
	}
}

func TestLoad_HTTPNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_, err := config.Load(context.Background(), srv.URL+"/missing.yaml")
	if err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}

// TestLoad_HTTPCanceledMidFetch verifies context cancellation propagates
// through the HTTP fetch (regression for the earlier ctx-not-threaded bug).
func TestLoad_HTTPCanceledMidFetch(t *testing.T) {
	// Server that hangs forever (until client disconnects).
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the request never completes.
	cancel()
	_, err := config.Load(ctx, srv.URL+"/hangs.yaml")
	if err == nil {
		t.Fatal("expected error for canceled HTTP fetch, got nil")
	}
}

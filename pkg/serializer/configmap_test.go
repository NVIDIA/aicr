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

package serializer

import (
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
)

func TestParseConfigMapURI(t *testing.T) {
	tests := []struct {
		name          string
		uri           string
		wantNamespace string
		wantName      string
		wantErr       bool
	}{
		{
			name:          "valid URI",
			uri:           "cm://gpu-operator/aicr-snapshot",
			wantNamespace: "gpu-operator",
			wantName:      "aicr-snapshot",
			wantErr:       false,
		},
		{
			name:          "valid URI with spaces",
			uri:           "cm://gpu-operator / aicr-snapshot ",
			wantNamespace: "gpu-operator",
			wantName:      "aicr-snapshot",
			wantErr:       false,
		},
		{
			name:          "valid URI with default namespace",
			uri:           "cm://default/snapshot",
			wantNamespace: "default",
			wantName:      "snapshot",
			wantErr:       false,
		},
		{
			name:    "missing scheme",
			uri:     "gpu-operator/aicr-snapshot",
			wantErr: true,
		},
		{
			name:    "wrong scheme",
			uri:     "http://gpu-operator/aicr-snapshot",
			wantErr: true,
		},
		{
			name:    "missing name",
			uri:     "cm://gpu-operator/",
			wantErr: true,
		},
		{
			name:    "missing namespace",
			uri:     "cm:///aicr-snapshot",
			wantErr: true,
		},
		{
			name:    "missing separator",
			uri:     "cm://gpu-operator",
			wantErr: true,
		},
		{
			name:    "empty URI",
			uri:     "",
			wantErr: true,
		},
		{
			name:    "only scheme",
			uri:     "cm://",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, name, err := pod.ParseConfigMapURI(tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("pod.ParseConfigMapURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if namespace != tt.wantNamespace {
					t.Errorf("pod.ParseConfigMapURI() namespace = %v, want %v", namespace, tt.wantNamespace)
				}
				if name != tt.wantName {
					t.Errorf("pod.ParseConfigMapURI() name = %v, want %v", name, tt.wantName)
				}
			}
		})
	}
}

func TestNewConfigMapWriter(t *testing.T) {
	tests := []struct {
		name       string
		namespace  string
		cmName     string
		format     Format
		wantFormat Format
	}{
		{
			name:       "valid JSON format",
			namespace:  "default",
			cmName:     "test",
			format:     FormatJSON,
			wantFormat: FormatJSON,
		},
		{
			name:       "valid YAML format",
			namespace:  "gpu-operator",
			cmName:     "snapshot",
			format:     FormatYAML,
			wantFormat: FormatYAML,
		},
		{
			name:       "unknown format defaults to JSON",
			namespace:  "default",
			cmName:     "test",
			format:     Format("unknown"),
			wantFormat: FormatJSON,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := NewConfigMapWriter(tt.namespace, tt.cmName, tt.format)
			if writer.namespace != tt.namespace {
				t.Errorf("NewConfigMapWriter() namespace = %v, want %v", writer.namespace, tt.namespace)
			}
			if writer.name != tt.cmName {
				t.Errorf("NewConfigMapWriter() name = %v, want %v", writer.name, tt.cmName)
			}
			if writer.format != tt.wantFormat {
				t.Errorf("NewConfigMapWriter() format = %v, want %v", writer.format, tt.wantFormat)
			}
			if writer.kubeconfig != "" {
				t.Errorf("NewConfigMapWriter() kubeconfig = %q, want \"\" (default discovery)", writer.kubeconfig)
			}
		})
	}
}

// TestNewConfigMapWriterWithKubeconfig verifies the kubeconfig path is stored
// on the writer so it can be used for client construction at Serialize() time.
func TestNewConfigMapWriterWithKubeconfig(t *testing.T) {
	tests := []struct {
		name       string
		kubeconfig string
	}{
		{"explicit kubeconfig path", "/tmp/custom-kubeconfig.yaml"},
		{"empty kubeconfig falls back to default discovery", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := NewConfigMapWriterWithKubeconfig("default", "test", tt.kubeconfig, FormatYAML)
			if writer.kubeconfig != tt.kubeconfig {
				t.Errorf("kubeconfig = %q, want %q", writer.kubeconfig, tt.kubeconfig)
			}
			if writer.format != FormatYAML {
				t.Errorf("format = %v, want FormatYAML", writer.format)
			}
		})
	}
}

// TestNewConfigMapWriter_PreservesFormatCoercion locks in the contract that the
// back-compat wrapper does not regress the IsUnknown -> JSON coercion when the
// underlying helper changes.
func TestNewConfigMapWriter_PreservesFormatCoercion(t *testing.T) {
	writer := NewConfigMapWriter("default", "test", Format("garbage"))
	if writer.format != FormatJSON {
		t.Errorf("format = %v, want FormatJSON (coerced from unknown)", writer.format)
	}
	if writer.kubeconfig != "" {
		t.Errorf("kubeconfig = %q, want empty (default discovery)", writer.kubeconfig)
	}
}

func TestConfigMapWriterSerializePreservesKubeconfigErrorCode(t *testing.T) {
	kubeconfig := writeInvalidKubeconfig(t)
	writer := NewConfigMapWriterWithKubeconfig("default", "test", kubeconfig, FormatYAML)

	err := writer.Serialize(t.Context(), map[string]string{"key": "value"})
	assertOutermostErrorCode(t, err, errors.ErrCodeInvalidRequest)
}

func writeInvalidKubeconfig(t *testing.T) string {
	t.Helper()

	kubeconfig := filepath.Join(t.TempDir(), "invalid-kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("invalid yaml content"), 0o600); err != nil {
		t.Fatalf("failed to write invalid kubeconfig: %v", err)
	}
	return kubeconfig
}

func assertOutermostErrorCode(t *testing.T, err error, want errors.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %s", want)
	}

	var structuredErr *errors.StructuredError
	if !stderrors.As(err, &structuredErr) {
		t.Fatalf("error = %v, want *errors.StructuredError", err)
	}
	if structuredErr.Code != want {
		t.Errorf("error code = %s, want %s", structuredErr.Code, want)
	}
}

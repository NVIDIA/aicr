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
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/k8s/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	accorev1 "k8s.io/client-go/applyconfigurations/core/v1"
)

// ConfigMapWriter writes serialized data to a Kubernetes ConfigMap.
// The ConfigMap is created if it doesn't exist, or updated if it does.
type ConfigMapWriter struct {
	namespace string
	name      string
	format    Format
}

// NewConfigMapWriter creates a new ConfigMapWriter that writes to the specified
// namespace and ConfigMap name in the given format.
func NewConfigMapWriter(namespace, name string, format Format) *ConfigMapWriter {
	if format.IsUnknown() {
		slog.Warn("unknown format, defaulting to JSON", "format", format)
		format = FormatJSON
	}
	return &ConfigMapWriter{
		namespace: namespace,
		name:      name,
		format:    format,
	}
}

// Serialize writes the snapshot data to a ConfigMap.
// The ConfigMap will have:
// - data.snapshot.{yaml|json}: The serialized snapshot content
// - data.format: The format used (yaml or json)
// - data.timestamp: ISO 8601 timestamp of when the snapshot was created
func (w *ConfigMapWriter) Serialize(ctx context.Context, snapshot any) error {
	// Create context with timeout for Kubernetes API operations
	// Use longer timeout to accommodate rate limiter after heavy API usage
	writeCtx, cancel := context.WithTimeout(ctx, defaults.ConfigMapWriteTimeout)
	defer cancel()

	client, config, err := client.GetKubeClient()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get kubernetes client", err)
	}

	// Log authentication context for audit
	authInfo := "default"
	switch {
	case config.AuthProvider != nil:
		authInfo = config.AuthProvider.Name
	case config.ExecProvider != nil:
		authInfo = "exec"
	case config.BearerToken != "":
		authInfo = "bearer-token"
	case config.CertData != nil:
		authInfo = "cert"
	}

	slog.Info("configmap operation",
		"namespace", w.namespace,
		"name", w.name,
		"auth_method", authInfo,
		"format", w.format)

	// Serialize snapshot to bytes using appropriate format
	var content []byte
	var extension string
	switch w.format {
	case FormatJSON:
		content, err = serializeJSON(snapshot)
		extension = "json"
	case FormatYAML:
		content, err = serializeYAML(snapshot)
		extension = "yaml"
	case FormatTable:
		content, err = serializeTable(snapshot)
		extension = "txt"
	default:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported format for ConfigMap: %s", w.format))
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot", err)
	}

	// Extract metadata from snapshot if it has a header
	var snapshotVersion string
	var snapshotKind string
	var snapshotTimestamp string

	// Try to extract header information if snapshot implements it
	if headerData, ok := snapshot.(interface {
		GetKind() header.Kind
		GetMetadata() map[string]string
	}); ok {
		snapshotKind = headerData.GetKind().String()
		metadata := headerData.GetMetadata()
		if v, exists := metadata["version"]; exists {
			snapshotVersion = v
		}
		if ts, exists := metadata["timestamp"]; exists {
			snapshotTimestamp = ts
		}
	}

	// Use defaults if not available from header
	if snapshotVersion == "" {
		snapshotVersion = "unknown"
	}
	if snapshotKind == "" {
		snapshotKind = header.KindSnapshot.String()
	}
	if snapshotTimestamp == "" {
		snapshotTimestamp = time.Now().UTC().Format(time.RFC3339)
	}

	// Create ConfigMap data
	dataKey := fmt.Sprintf("snapshot.%s", extension)
	configMapData := map[string]string{
		dataKey:     string(content),
		"format":    string(w.format),
		"timestamp": snapshotTimestamp,
	}

	// Build ConfigMap apply configuration for Server-Side Apply
	configMap := accorev1.ConfigMap(w.name, w.namespace).
		WithLabels(map[string]string{
			"app.kubernetes.io/name":      "aicr",
			"app.kubernetes.io/component": snapshotKind,
			"app.kubernetes.io/version":   snapshotVersion,
		}).
		WithData(configMapData)

	// Use Server-Side Apply for atomic create-or-update operation
	// This eliminates race conditions from the previous Get-then-Update pattern
	// Force allows taking ownership from previous field managers (aicr CLI vs agent)
	slog.Info("applying ConfigMap",
		"namespace", w.namespace,
		"name", w.name,
		"format", w.format)

	_, err = client.CoreV1().ConfigMaps(w.namespace).Apply(
		writeCtx,
		configMap,
		metav1.ApplyOptions{
			FieldManager: "aicr",
			Force:        true,
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply ConfigMap", err)
	}

	return nil
}

// Close is a no-op for ConfigMapWriter as there are no resources to release.
// This method exists to satisfy the Closer interface.
func (w *ConfigMapWriter) Close() error {
	return nil
}

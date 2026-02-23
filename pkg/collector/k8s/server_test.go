// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/stretchr/testify/assert"
)

func TestKubernetesCollector_Collect(t *testing.T) {
	t.Setenv("NODE_NAME", "test-node")

	ctx := context.TODO()
	collector := createTestCollector()

	m, err := collector.Collect(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, m)
	assert.Equal(t, measurement.TypeK8s, m.Type)
	// Should have 6 subtypes: server, image, policy, node, helm, and argocd
	assert.Len(t, m.Subtypes, 6)

	// Find the server subtype
	var serverSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == "server" {
			serverSubtype = &m.Subtypes[i]
			break
		}
	}
	if !assert.NotNil(t, serverSubtype, "Expected to find server subtype") {
		return
	}

	data := serverSubtype.Data
	if assert.Len(t, data, 3) {
		if reading, ok := data["version"]; assert.True(t, ok) {
			assert.Equal(t, "v1.28.0", reading.Any())
		}
		if reading, ok := data["platform"]; assert.True(t, ok) {
			assert.Equal(t, "linux/amd64", reading.Any())
		}
		if reading, ok := data["goVersion"]; assert.True(t, ok) {
			assert.Equal(t, "go1.20.7", reading.Any())
		}
	}
}

func TestKubernetesCollector_CollectWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	cancel() // Cancel immediately

	collector := createTestCollector()
	m, err := collector.Collect(ctx)

	assert.Error(t, err)
	assert.Nil(t, m)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestKubernetesCollector_CollectWithTimeout(t *testing.T) {
	// Create a context with very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Wait for context to timeout
	time.Sleep(10 * time.Millisecond)

	collector := createTestCollector()
	m, err := collector.Collect(ctx)

	// Should fail with deadline exceeded
	assert.Error(t, err)
	assert.Nil(t, m)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestKubernetesCollector_ErrorRecovery_NilClient(t *testing.T) {
	ctx := context.TODO()

	// Create collector without a valid client
	collector := &Collector{
		ClientSet:  nil,
		RestConfig: nil,
	}

	m, err := collector.Collect(ctx)

	// Should degrade gracefully when client is unavailable
	assert.NoError(t, err)
	assert.NotNil(t, m)
	assert.Equal(t, measurement.TypeK8s, m.Type)
	// All subtypes should be present but with empty data
	assert.Len(t, m.Subtypes, 6)
}

// Helper function defined in image_test.go
// Reused here to avoid duplication across test files

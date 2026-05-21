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

// Package helmtest provides test helpers for consumers of pkg/helm.
package helmtest

import (
	"context"
	"sync"

	"github.com/NVIDIA/aicr/pkg/helm"
)

// MockRenderer is a test double for helm.Renderer that returns canned
// YAML keyed by ChartInput.Name. It satisfies helm.Renderer so any
// package that needs to test chart rendering can inject it instead of
// requiring the helm binary on PATH.
type MockRenderer struct {
	// Rendered maps component name → rendered YAML bytes.
	Rendered map[string][]byte
	// Errs maps component name → error to return.
	Errs map[string]error

	// mu protects Inputs from concurrent Render calls.
	mu sync.Mutex
	// Inputs records every ChartInput passed to Render, in call order.
	Inputs []helm.ChartInput
}

// Render returns the canned response for the given input name.
func (m *MockRenderer) Render(_ context.Context, input helm.ChartInput) ([]byte, error) {
	m.mu.Lock()
	m.Inputs = append(m.Inputs, input)
	m.mu.Unlock()
	if err, ok := m.Errs[input.Name]; ok {
		return nil, err
	}
	if data, ok := m.Rendered[input.Name]; ok {
		return data, nil
	}
	return nil, nil
}

// BlockingRenderer blocks until the context is canceled, then returns
// the context error. Useful for verifying context cancellation propagation.
type BlockingRenderer struct{}

// Render blocks until ctx is done.
func (b *BlockingRenderer) Render(ctx context.Context, _ helm.ChartInput) ([]byte, error) {
	<-ctx.Done()
	return []byte{}, ctx.Err()
}

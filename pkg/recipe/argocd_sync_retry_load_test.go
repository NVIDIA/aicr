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

package recipe

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe/providertest"
)

// TestLoadRegistry_RejectsInvalidArgoCDSyncRetry pins that a --data registry
// is held to the argocd.syncRetry rules at load, not only by Validate: a zero
// limit would render a disabled retry, and an unknown whenComponents entry
// would silently leave the retry out.
func TestLoadRegistry_RejectsInvalidArgoCDSyncRetry(t *testing.T) {
	tests := []struct {
		name    string
		retry   *ArgoCDSyncRetryConfig
		wantErr string // empty: loads
	}{
		{name: "embedded declaration loads",
			retry: &ArgoCDSyncRetryConfig{Limit: 5, WhenComponents: []string{"grove"}}},
		{name: "zero limit",
			retry:   &ArgoCDSyncRetryConfig{Limit: 0, WhenComponents: []string{"grove"}},
			wantErr: "argocd.syncRetry.limit must be at least 1, got 0"},
		{name: "unknown whenComponents entry",
			retry:   &ArgoCDSyncRetryConfig{Limit: 5, WhenComponents: []string{"grove-typo"}},
			wantErr: `whenComponents names "grove-typo", which is not another registry component`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			embedded, err := GetComponentRegistry()
			if err != nil {
				t.Fatalf("load embedded registry: %v", err)
			}
			entry := *embedded.Get("dynamo-platform")
			entry.ArgoCD.SyncRetry = tt.retry
			external := ComponentRegistry{
				APIVersion: embedded.APIVersion, Kind: embedded.Kind,
				Components: []ComponentConfig{entry},
			}
			data, err := yaml.Marshal(&external)
			if err != nil {
				t.Fatalf("marshal registry: %v", err)
			}
			provider, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""),
				LayeredProviderConfig{ExternalDir: providertest.WriteDir(t, map[string][]byte{RegistryFileName: data})})
			if err != nil {
				t.Fatalf("layer registry: %v", err)
			}
			t.Cleanup(func() { EvictCachedRegistry(provider) })

			_, err = GetComponentRegistryFor(provider)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("GetComponentRegistryFor() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("GetComponentRegistryFor() error = %v, want one containing %q", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

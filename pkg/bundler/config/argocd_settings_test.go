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

package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func ptr[T any](v T) *T { return &v }

func TestResolveArgoDeployerOptions(t *testing.T) {
	tests := []struct {
		name      string
		legacy    map[string]string
		settings  ArgoCDSettings
		want      *ArgoDeployerOptions
		errSubstr string
	}{
		{
			name: "nothing set returns nil",
		},
		{
			name: "settings only",
			settings: ArgoCDSettings{
				NamePrefix:        ptr("tenant-a-"),
				DestinationServer: ptr("https://10.0.0.1:6443"),
				Project:           ptr("gpu-infra"),
				CascadeDelete:     ptr(true),
			},
			want: &ArgoDeployerOptions{
				NamePrefix:        "tenant-a-",
				DestinationServer: "https://10.0.0.1:6443",
				Project:           "gpu-infra",
				CascadeDelete:     true,
			},
		},
		{
			name:   "legacy only maps onto the same options",
			legacy: map[string]string{"project": "gpu-infra", "cascadeDelete": "true"},
			want:   &ArgoDeployerOptions{Project: "gpu-infra", CascadeDelete: true},
		},
		{
			name:     "disjoint legacy and settings merge",
			legacy:   map[string]string{"project": "gpu-infra"},
			settings: ArgoCDSettings{NamePrefix: ptr("tenant-a-")},
			want:     &ArgoDeployerOptions{Project: "gpu-infra", NamePrefix: "tenant-a-"},
		},
		{
			name:      "same option both ways is rejected",
			legacy:    map[string]string{"project": "a"},
			settings:  ArgoCDSettings{Project: ptr("b")},
			errSubstr: "set twice",
		},
		{
			// An explicit false is still a setting, so it conflicts too.
			name:      "explicit false conflicts with legacy",
			legacy:    map[string]string{"cascadeDelete": "true"},
			settings:  ArgoCDSettings{CascadeDelete: ptr(false)},
			errSubstr: "--argocd-cascade-delete",
		},
		{
			name:     "explicit false is honored",
			settings: ArgoCDSettings{CascadeDelete: ptr(false)},
			want:     &ArgoDeployerOptions{},
		},
		{
			name: "full sync retry",
			settings: ArgoCDSettings{
				SyncRetryLimit:              ptr(int64(20)),
				SyncRetryBackoffDuration:    ptr("30s"),
				SyncRetryBackoffFactor:      ptr(int64(2)),
				SyncRetryBackoffMaxDuration: ptr("5m"),
			},
			want: &ArgoDeployerOptions{SyncRetry: &SyncRetry{
				Limit: 20, BackoffDuration: "30s", BackoffFactor: 2, BackoffMaxDuration: "5m",
			}},
		},
		{
			name:     "explicit zero limit renders a retry",
			settings: ArgoCDSettings{SyncRetryLimit: ptr(int64(0))},
			want:     &ArgoDeployerOptions{SyncRetry: &SyncRetry{}},
		},
		{
			name:      "backoff without limit",
			settings:  ArgoCDSettings{SyncRetryBackoffDuration: ptr("30s")},
			errSubstr: "requires a sync retry limit",
		},
		{
			name:      "negative limit",
			settings:  ArgoCDSettings{SyncRetryLimit: ptr(int64(-1))},
			errSubstr: "must be >= 0",
		},
		{
			name:      "zero factor",
			settings:  ArgoCDSettings{SyncRetryLimit: ptr(int64(3)), SyncRetryBackoffFactor: ptr(int64(0))},
			errSubstr: "backoff factor 0",
		},
		{
			name:      "unparseable duration",
			settings:  ArgoCDSettings{SyncRetryLimit: ptr(int64(3)), SyncRetryBackoffDuration: ptr("soon")},
			errSubstr: "backoff duration",
		},
		{
			name:      "non-positive max duration",
			settings:  ArgoCDSettings{SyncRetryLimit: ptr(int64(3)), SyncRetryBackoffMaxDuration: ptr("0s")},
			errSubstr: "must be positive",
		},
		{
			name:      "explicit empty duration",
			settings:  ArgoCDSettings{SyncRetryLimit: ptr(int64(3)), SyncRetryBackoffDuration: ptr("")},
			errSubstr: "must not be empty",
		},
		{
			name:      "empty project",
			settings:  ArgoCDSettings{Project: ptr("")},
			errSubstr: "must not be empty",
		},
		{
			name:      "invalid name prefix",
			settings:  ArgoCDSettings{NamePrefix: ptr("Tenant")},
			errSubstr: "Argo CD name prefix",
		},
		{
			name:      "invalid destination server",
			settings:  ArgoCDSettings{DestinationServer: ptr("http://insecure")},
			errSubstr: "Argo CD destination server",
		},
		{
			// The legacy vocabulary does not grow: retry is a dedicated
			// setting only.
			name:      "legacy retry key is unknown",
			legacy:    map[string]string{"syncRetry.limit": "3"},
			errSubstr: "unknown deployer option",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveArgoDeployerOptions(tt.legacy, tt.settings)
			if tt.errSubstr != "" {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not contain %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestValidateArgoCDSource pins what one source is held to before layering:
// field values and duplicate spellings, but not backoff needing a limit,
// which a higher-precedence source may supply.
func TestValidateArgoCDSource(t *testing.T) {
	tests := []struct {
		name      string
		legacy    map[string]string
		settings  ArgoCDSettings
		errSubstr string
	}{
		{name: "backoff without limit passes", settings: ArgoCDSettings{SyncRetryBackoffFactor: ptr(int64(2))}},
		{name: "zero factor fails", settings: ArgoCDSettings{SyncRetryBackoffFactor: ptr(int64(0))}, errSubstr: "backoff factor 0"},
		{name: "duplicate spelling fails", legacy: map[string]string{"project": "a"},
			settings: ArgoCDSettings{Project: ptr("b")}, errSubstr: "set twice"},
		{name: "unknown legacy key fails", legacy: map[string]string{"bogus": "x"}, errSubstr: "unknown deployer option"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArgoCDSource(tt.legacy, tt.settings)
			if tt.errSubstr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not contain %q", err, tt.errSubstr)
			}
		})
	}
}

func TestArgoCDSettingsOverlay(t *testing.T) {
	base := ArgoCDSettings{
		Project:        ptr("from-config"),
		NamePrefix:     ptr("cfg-"),
		SyncRetryLimit: ptr(int64(5)),
	}
	top := ArgoCDSettings{
		Project:       ptr("from-cli"),
		CascadeDelete: ptr(false),
	}
	got := base.Overlay(top)
	want := ArgoCDSettings{
		Project:        ptr("from-cli"),
		NamePrefix:     ptr("cfg-"),
		CascadeDelete:  ptr(false),
		SyncRetryLimit: ptr(int64(5)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Overlay() = %+v, want %+v", got, want)
	}

	// The result must not alias either input.
	*got.Project = "mutated"
	*got.NamePrefix = "mutated"
	if *top.Project != "from-cli" || *base.NamePrefix != "cfg-" {
		t.Error("Overlay() result aliases an input pointer")
	}
}

func TestLayerArgoCDSettings(t *testing.T) {
	legacy := func(pairs ...string) []ComponentPath {
		var out []ComponentPath
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, ComponentPath{Component: DeployerOverrideKey, Path: pairs[i], Value: ptr(pairs[i+1])})
		}
		return out
	}
	component := ComponentPath{Component: "gpuoperator", Path: "driver.version", Value: ptr("580")}

	t.Run("flag beats config entry of the deprecated spelling", func(t *testing.T) {
		overrides := append(legacy("project", "cfg", "namePrefix", "cfg-"), component)
		gotOverrides, got := LayerArgoCDSettings(false, overrides,
			ArgoCDSettings{}, ArgoCDSettings{Project: ptr("cli")})
		want := append(legacy("namePrefix", "cfg-"), component)
		if !reflect.DeepEqual(gotOverrides, want) {
			t.Errorf("overrides = %+v, want %+v", gotOverrides, want)
		}
		if *got.Project != "cli" {
			t.Errorf("Project = %q, want cli", *got.Project)
		}
	})

	t.Run("deprecated --set beats config field", func(t *testing.T) {
		overrides := legacy("project", "cli")
		gotOverrides, got := LayerArgoCDSettings(true, overrides,
			ArgoCDSettings{Project: ptr("cfg"), NamePrefix: ptr("cfg-")}, ArgoCDSettings{})
		if !reflect.DeepEqual(gotOverrides, overrides) {
			t.Errorf("overrides = %+v, want them unchanged", gotOverrides)
		}
		if got.Project != nil || got.NamePrefix == nil || *got.NamePrefix != "cfg-" {
			t.Errorf("settings = %+v, want Project cleared and NamePrefix kept", got)
		}
	})

	t.Run("same-source duplicates survive for the bundler to reject", func(t *testing.T) {
		overrides := legacy("project", "a")
		gotOverrides, got := LayerArgoCDSettings(true, overrides,
			ArgoCDSettings{}, ArgoCDSettings{Project: ptr("b")})
		if _, err := ResolveArgoDeployerOptions(LegacyDeployerOverrides(gotOverrides), got); err == nil {
			t.Error("ResolveArgoDeployerOptions accepted both spellings from the CLI")
		}
	})
}

func TestConfigLegacyDeployerNotice(t *testing.T) {
	if _, ok := NewConfig().LegacyDeployerNotice(); ok {
		t.Error("a config without deployer: entries reported a notice")
	}
	cfg := NewConfig(WithValueOverrides(map[string]map[string]string{
		DeployerOverrideKey: {"project": "a", "cascadeDelete": "true"},
	}))
	n, ok := cfg.LegacyDeployerNotice()
	if !ok || n.Subject != "--set deployer:cascadeDelete" {
		t.Errorf("notice = %+v, %v; want the first key in sorted order", n, ok)
	}
}

func TestWithArgoCDSettingsCopies(t *testing.T) {
	project := "a"
	cfg := NewConfig(WithArgoCDSettings(ArgoCDSettings{Project: &project}))
	project = "b"
	got := cfg.ArgoCDSettings()
	if *got.Project != "a" {
		t.Errorf("stored Project = %q, want the value at option time", *got.Project)
	}
	*got.Project = "c"
	if *cfg.ArgoCDSettings().Project != "a" {
		t.Error("ArgoCDSettings() returned an aliased copy")
	}
}

func TestLegacyDeployerNotice(t *testing.T) {
	for _, key := range []string{"cascadeDelete", "destinationServer", "namePrefix", "project"} {
		n := LegacyDeployerNotice(key)
		if n.Replacement == "" {
			t.Errorf("%s: notice names no replacement", key)
		}
		if n.RemovedIn != "" {
			t.Errorf("%s: notice names removal %q, but none is scheduled", key, n.RemovedIn)
		}
		if !strings.Contains(n.URL(), "#"+legacyDeployerAnchor) {
			t.Errorf("%s: notice URL %q does not point at the entry", key, n.URL())
		}
	}
}

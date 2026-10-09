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

package validations

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckNVSentinelMongoDBCoherent(t *testing.T) {
	t.Parallel()

	const appDN = "CN=mongo-user-client,OU=DGXC,O=Nvidia,L=SantaClara,ST=California,C=US"
	readWrite := map[string]any{"name": "readWrite", "db": "HealthEventsDatabase"}
	ttlGrant := map[string]any{"name": "nvsentinelTTLIndex", "db": "admin"}
	appUser := func(roles ...any) []any {
		return []any{map[string]any{"name": appDN, "db": "$external", "roles": roles}}
	}
	ttlRole := func(actions ...any) []any {
		return []any{map[string]any{"role": "nvsentinelTTLIndex", "db": "admin", "privileges": []any{map[string]any{
			"resource": map[string]any{"db": "HealthEventsDatabase", "collection": ""},
			"actions":  actions,
		}}}}
	}
	good := func() map[string]any {
		return map[string]any{
			"crVersion": "1.21.2",
			"initImage": map[string]any{"tag": "1.21.2@sha256:abc"},
			"tls":       map[string]any{"mode": "requireTLS"},
			"sharding":  map[string]any{"enabled": false},
			"roles":     ttlRole("collMod"),
			"users":     appUser(readWrite, ttlGrant),
		}
	}
	operator := func(tag string, extra map[string]any) recipe.ComponentRef {
		o := map[string]any{"image": map[string]any{"tag": tag}, "watchAllNamespaces": false, "disableTelemetry": true}
		for k, v := range extra {
			o[k] = v
		}
		return recipe.ComponentRef{Name: "psmdb-operator", Overrides: o}
	}
	with := func(v map[string]any, key string, value any) map[string]any {
		v[key] = value
		return v
	}
	result := func(db map[string]any, refs ...recipe.ComponentRef) *recipe.RecipeResult {
		base := []recipe.ComponentRef{
			{Name: "nvsentinel-mongodb", Overrides: db},
			operator("1.21.2@sha256:def", nil),
			{Name: nvsentinelComponent},
		}
		if refs != nil {
			base = append(base[:1], refs...)
		}
		return &recipe.RecipeResult{ComponentRefs: base}
	}

	dynamic := func(key string, paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{key: paths}))
	}

	tests := []struct {
		name string
		rr   *recipe.RecipeResult
		cfg  *config.Config
		want []string
	}{
		{name: "nil recipe", rr: nil},
		{name: "component absent", rr: &recipe.RecipeResult{}},
		{name: "coherent", rr: result(good())},
		{
			name: "bundlers subset renders only the datastore",
			rr: func() *recipe.RecipeResult {
				full := result(good())
				filtered := &recipe.RecipeResult{ComponentRefs: full.ComponentRefs[:1]}
				return filtered.WithDeclaredComponents(full.ComponentRefs)
			}(),
		},
		{
			name: "bundlers subset with the operator declared but disabled",
			rr: func() *recipe.RecipeResult {
				full := result(good(), operator("1.21.2", map[string]any{"enabled": false}), recipe.ComponentRef{Name: nvsentinelComponent})
				filtered := &recipe.RecipeResult{ComponentRefs: full.ComponentRefs[:1]}
				return filtered.WithDeclaredComponents(full.ComponentRefs)
			}(),
			want: []string{"psmdb-operator is not deployed"},
		},
		{
			name: "no NVSentinel to consume it",
			rr:   result(good(), operator("1.21.2", nil)),
			want: []string{"NVSentinel is not deployed"},
		},
		{
			name: "NVSentinel disabled",
			rr: result(good(),
				operator("1.21.2", nil),
				recipe.ComponentRef{Name: nvsentinelComponent, Overrides: map[string]any{"enabled": false}}),
			want: []string{"NVSentinel is not deployed"},
		},
		{name: "TLS preferred, not required", rr: result(with(good(), "tls", map[string]any{"mode": "preferTLS"})), want: []string{"want requireTLS"}},
		{name: "sharding on", rr: result(with(good(), "sharding", map[string]any{"enabled": true})), want: []string{"sharding.enabled must be false"}},
		{name: "sharding unset", rr: result(with(good(), "sharding", map[string]any{})), want: []string{"sharding.enabled must be false"}},
		{name: "no app user", rr: result(with(good(), "users", []any{})), want: []string{"no $external user"}},
		{
			name: "app user in the wrong database",
			rr: result(with(good(), "users", []any{map[string]any{
				"name": appDN, "db": "admin", "roles": []any{readWrite, ttlGrant},
			}})),
			want: []string{"no $external user"},
		},
		{
			name: "app user without readWrite",
			rr:   result(with(good(), "users", appUser(ttlGrant))),
			want: []string{"lacks readWrite on HealthEventsDatabase"},
		},
		{
			name: "app user without the TTL role",
			rr:   result(with(good(), "users", appUser(readWrite))),
			want: []string{"lacks role nvsentinelTTLIndex"},
		},
		{
			name: "TTL role defined without collMod",
			rr:   result(with(good(), "roles", ttlRole("find"))),
			want: []string{"lacks role nvsentinelTTLIndex"},
		},
		{
			name: "TTL role not defined",
			rr:   result(with(good(), "roles", []any{})),
			want: []string{"lacks role nvsentinelTTLIndex"},
		},
		{
			name: "no psmdb-operator",
			rr:   result(good(), recipe.ComponentRef{Name: nvsentinelComponent}),
			want: []string{"psmdb-operator is not deployed"},
		},
		{
			name: "psmdb-operator disabled",
			rr: result(good(),
				operator("1.21.2", map[string]any{"enabled": false}),
				recipe.ComponentRef{Name: nvsentinelComponent}),
			want: []string{"psmdb-operator is not deployed"},
		},
		{
			name: "operator watches every namespace",
			rr: result(good(),
				operator("1.21.2", map[string]any{"watchAllNamespaces": true}),
				recipe.ComponentRef{Name: nvsentinelComponent}),
			want: []string{"watchAllNamespaces is true, want false"},
		},
		{
			name: "operator telemetry on",
			rr: result(good(),
				operator("1.21.2", map[string]any{"disableTelemetry": false}),
				recipe.ComponentRef{Name: nvsentinelComponent}),
			want: []string{"disableTelemetry is false, want true"},
		},
		{name: "crVersion a minor behind", rr: result(with(good(), "crVersion", "1.20.1")), want: []string{"crVersion \"1.20.1\""}},
		{name: "initImage a minor ahead", rr: result(with(good(), "initImage", map[string]any{"tag": "1.22.0"})), want: []string{"initImage.tag"}},
		{
			name: "operator tag unreadable",
			rr: result(good(),
				operator("latest", nil),
				recipe.ComponentRef{Name: nvsentinelComponent}),
			want: []string{"cannot read psmdb-operator's image.tag"},
		},
		{name: "dynamic tls.mode", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "tls.mode"), want: []string{"--dynamic nvsentinelmongodb:tls.mode"}},
		{name: "dynamic tls ancestor", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "tls"), want: []string{"--dynamic nvsentinelmongodb:tls targets tls.mode"}},
		{name: "dynamic users", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "users"), want: []string{"--dynamic nvsentinelmongodb:users"}},
		{name: "dynamic sharding", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "sharding.enabled"), want: []string{"--dynamic nvsentinelmongodb:sharding.enabled"}},
		{name: "dynamic crVersion", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "crVersion"), want: []string{"--dynamic nvsentinelmongodb:crVersion"}},
		{name: "dynamic initImage", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "initImage"), want: []string{"--dynamic nvsentinelmongodb:initImage"}},
		{name: "dynamic operator image.tag", rr: result(good()), cfg: dynamic("psmdboperator", "image.tag"), want: []string{"--dynamic psmdboperator:image.tag"}},
		{name: "dynamic roles", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "roles"), want: []string{"--dynamic nvsentinelmongodb:roles"}},
		{name: "dynamic operator watchAllNamespaces", rr: result(good()), cfg: dynamic("psmdboperator", "watchAllNamespaces"), want: []string{"--dynamic psmdboperator:watchAllNamespaces"}},
		{name: "dynamic operator disableTelemetry", rr: result(good()), cfg: dynamic("psmdboperator", "disableTelemetry"), want: []string{"--dynamic psmdboperator:disableTelemetry"}},
		{name: "dynamic unrelated path", rr: result(good()), cfg: dynamic("nvsentinelmongodb", "replsets.rs0.resources")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelMongoDBCoherent(t.Context(), "nvsentinel-mongodb", tt.rr, cfg, nil)
			got := append([]string{}, warnings...)
			for _, err := range errs {
				got = append(got, err.Error())
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d findings %q, want %d matching %q", len(got), got, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("finding %d = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

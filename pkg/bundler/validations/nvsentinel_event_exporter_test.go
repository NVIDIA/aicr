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

func TestCheckNVSentinelEventExporterCoherent(t *testing.T) {
	t.Parallel()

	// exporter is the event-exporter block the mixin sets plus every
	// per-deployment value an operator supplies.
	exporter := func() map[string]any {
		return map[string]any{
			"oidcSecretName": "event-exporter-oidc-secret",
			"exporter": map[string]any{
				"metadata": map[string]any{"cluster": "train-1", "environment": "staging"},
				"sink":     map[string]any{"endpoint": "https://events.corp.internal/v1/events", "insecureSkipVerify": false},
				"oidc": map[string]any{
					"tokenUrl": "https://idp.corp.internal/oauth2/token", "clientId": "nvsentinel", "scope": "events:write",
					"insecureSkipVerify": false,
				},
				"filter": map[string]any{"expression": "event.recommendedAction != 'NONE' || event.isHealthy == true"},
			},
		}
	}
	section := func(e map[string]any, path ...string) map[string]any {
		cur := e
		for _, p := range path {
			cur = cur[p].(map[string]any)
		}
		return cur
	}
	set := func(e map[string]any, value any, path ...string) map[string]any {
		section(e, path[:len(path)-1]...)[path[len(path)-1]] = value
		return e
	}
	unset := func(e map[string]any, path ...string) map[string]any {
		delete(section(e, path[:len(path)-1]...), path[len(path)-1])
		return e
	}
	recipeOf := func(e map[string]any) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
			Name: nvsentinelComponent,
			Overrides: map[string]any{
				"global": map[string]any{
					"eventExporter": map[string]any{"enabled": true},
					"datastore":     map[string]any{"provider": "mongodb", "connection": map[string]any{"database": "HealthEventsDatabase"}},
				},
				"event-exporter": e,
			},
		}}}
	}
	sets := func(kv map[string]string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{"nv-sentinel": kv}))
	}
	setJSON := func(specs ...string) *config.Config {
		paths, err := config.ParseValueOverridesJSON(specs)
		if err != nil {
			t.Fatalf("ParseValueOverridesJSON: %v", err)
		}
		return config.NewConfig(config.WithValueOverridesTypedPaths(paths))
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}

	const (
		wantNotSet      = "is not set, so the chart's placeholder would apply"
		wantNotString   = "not a string"
		wantPlaceholder = "a placeholder host"
		wantScheme      = "not an http or https URL"
		wantNoHost      = "a URL with no host"
		wantTOML        = "unescaped"
		wantBareKey     = "as a bare key"
		wantCluster     = "the chart's placeholder \"my-cluster\""
		wantSkipVerify  = "it must be the boolean false"
		wantSecret      = "not a valid Secret name"
		wantCleartext   = "plain http to a host outside the cluster"
		wantDynamic     = "--dynamic"
	)

	tests := []struct {
		name   string
		recipe *recipe.RecipeResult
		cfg    *config.Config
		want   []string
	}{
		{name: "exporter off", recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent}}}},
		{name: "every per-deployment value set", recipe: recipeOf(exporter())},
		{name: "plain http in-cluster sink",
			recipe: recipeOf(set(exporter(), "http://collector.observability.svc:8080/events", "exporter", "sink", "endpoint"))},
		{name: "sink endpoint missing", recipe: recipeOf(unset(exporter(), "exporter", "sink", "endpoint")),
			want: []string{"sink.endpoint " + wantNotSet}},
		{name: "sink endpoint empty", recipe: recipeOf(set(exporter(), " ", "exporter", "sink", "endpoint")),
			want: []string{"sink.endpoint " + wantNotSet}},
		{name: "sink endpoint the chart's placeholder",
			recipe: recipeOf(set(exporter(), "https://events.example.com/api/v1/events", "exporter", "sink", "endpoint")),
			want:   []string{wantPlaceholder}},
		{name: "sink endpoint on example.org",
			recipe: recipeOf(set(exporter(), "https://example.org/events", "exporter", "sink", "endpoint")),
			want:   []string{wantPlaceholder}},
		{name: "sink endpoint not a URL scheme it posts to",
			recipe: recipeOf(set(exporter(), "grpc://events.corp.internal:443", "exporter", "sink", "endpoint")),
			want:   []string{wantScheme}},
		{name: "sink endpoint without a host",
			recipe: recipeOf(set(exporter(), "https:///events", "exporter", "sink", "endpoint")),
			want:   []string{wantNoHost}},
		{name: "sink endpoint under the reserved .example TLD",
			recipe: recipeOf(set(exporter(), "https://events.corp.example/v1/events", "exporter", "sink", "endpoint")),
			want:   []string{wantPlaceholder}},
		{name: "token URL on the bare example host",
			recipe: recipeOf(set(exporter(), "https://example/oauth2/token", "exporter", "oidc", "tokenUrl")),
			want:   []string{wantPlaceholder}},
		{name: "plain http token URL outside the cluster",
			recipe: recipeOf(set(exporter(), "http://idp.corp.com/oauth2/token", "exporter", "oidc", "tokenUrl")),
			want:   []string{wantCleartext}},
		{name: "placeholder host with a trailing dot",
			recipe: recipeOf(set(exporter(), "https://events.corp.example./events", "exporter", "sink", "endpoint")),
			want:   []string{wantPlaceholder}},
		{name: "chart placeholder with a trailing dot",
			recipe: recipeOf(set(exporter(), "https://auth.example.com./oauth2/token", "exporter", "oidc", "tokenUrl")),
			want:   []string{wantPlaceholder}},
		{name: "plain http to a public IPv6 literal",
			recipe: recipeOf(set(exporter(), "http://[2606:4700:4700::1111]/token", "exporter", "oidc", "tokenUrl")),
			want:   []string{wantCleartext}},
		{name: "plain http to an IPv6 literal with a zone",
			recipe: recipeOf(set(exporter(), "http://[2606:4700:4700::1111%25eth0]/token", "exporter", "oidc", "tokenUrl")),
			want:   []string{wantCleartext}},
		{name: "plain http to a link-local IPv6 literal with a zone",
			recipe: recipeOf(set(exporter(), "http://[fe80::1%25en0]:8080/events", "exporter", "sink", "endpoint")),
			want:   []string{wantCleartext}},
		{name: "plain http to an IPv4 literal",
			recipe: recipeOf(set(exporter(), "http://203.0.113.7:8080/events", "exporter", "sink", "endpoint")),
			want:   []string{wantCleartext}},
		{name: "https to an IPv6 literal",
			recipe: recipeOf(set(exporter(), "https://[2606:4700:4700::1111]/token", "exporter", "oidc", "tokenUrl"))},
		{name: "plain http at the svc.cluster.local name with a trailing dot",
			recipe: recipeOf(set(exporter(), "http://collector.observability.svc.cluster.local./events", "exporter", "sink", "endpoint"))},
		{name: "host that is only a trailing dot",
			recipe: recipeOf(set(exporter(), "https://./events", "exporter", "sink", "endpoint")),
			want:   []string{wantNoHost}},
		{name: "plain http sink at a single-label Service name",
			recipe: recipeOf(set(exporter(), "http://collector:8080/events", "exporter", "sink", "endpoint"))},
		{name: "plain http token URL at the svc.cluster.local name",
			recipe: recipeOf(set(exporter(), "http://ee-mock.ee-mock.svc.cluster.local:8080/token", "exporter", "oidc", "tokenUrl"))},
		{name: "token URL the chart's placeholder",
			recipe: recipeOf(set(exporter(), "https://auth.example.com/oauth2/token", "exporter", "oidc", "tokenUrl")),
			want:   []string{wantPlaceholder}},
		{name: "client ID missing", recipe: recipeOf(unset(exporter(), "exporter", "oidc", "clientId")),
			want: []string{"oidc.clientId " + wantNotSet}},
		{name: "client ID from --set parsed as a number", recipe: recipeOf(exporter()),
			cfg:  sets(map[string]string{eventExporterClientIDPath: "12345"}),
			want: []string{wantNotString}},
		{name: "client ID from --set-json as a string", recipe: recipeOf(unset(exporter(), "exporter", "oidc", "clientId")),
			cfg: setJSON(`nv-sentinel:` + eventExporterClientIDPath + `="12345"`)},
		{name: "scope missing", recipe: recipeOf(unset(exporter(), "exporter", "oidc", "scope")),
			want: []string{"oidc.scope " + wantNotSet}},
		{name: "cluster missing", recipe: recipeOf(unset(exporter(), "exporter", "metadata", "cluster")),
			want: []string{"metadata.cluster " + wantNotSet}},
		{name: "cluster the chart's placeholder",
			recipe: recipeOf(set(exporter(), "my-cluster", "exporter", "metadata", "cluster")),
			want:   []string{wantCluster}},
		{name: "environment missing", recipe: recipeOf(unset(exporter(), "exporter", "metadata", "environment")),
			want: []string{"metadata.environment " + wantNotSet}},
		{name: "per-deployment values from --set",
			recipe: recipeOf(unset(unset(exporter(), "exporter", "sink", "endpoint"), "exporter", "metadata", "cluster")),
			cfg: sets(map[string]string{
				eventExporterSinkEndpointPath: "https://events.corp.internal/v1/events",
				eventExporterClusterPath:      "train-2",
			})},
		{name: "extra metadata entry", recipe: recipeOf(set(exporter(), "us-west-2", "exporter", "metadata", "region"))},
		{name: "extra metadata entry with a quote",
			recipe: recipeOf(set(exporter(), `us"west`, "exporter", "metadata", "region")),
			want:   []string{wantTOML}},
		{name: "extra metadata value with a control character",
			recipe: recipeOf(set(exporter(), "us\x01west", "exporter", "metadata", "region")),
			want:   []string{wantTOML}},
		{name: "extra metadata value with a tab", recipe: recipeOf(set(exporter(), "us\twest", "exporter", "metadata", "region"))},
		{name: "extra metadata key that is not a TOML bare key",
			recipe: recipeOf(set(exporter(), "x", "exporter", "metadata", "team name")),
			want:   []string{wantBareKey}},
		{name: "cluster with a line break", recipe: recipeOf(set(exporter(), "train\n1", "exporter", "metadata", "cluster")),
			want: []string{wantTOML}},
		{name: "endpoint with a backslash",
			recipe: recipeOf(set(exporter(), `https://events.corp.internal/a\b`, "exporter", "sink", "endpoint")),
			want:   []string{wantTOML}},
		{name: "sink certificate unchecked", recipe: recipeOf(set(exporter(), true, "exporter", "sink", "insecureSkipVerify")),
			want: []string{wantSkipVerify}},
		{name: "OIDC certificate check as a string",
			recipe: recipeOf(set(exporter(), "false", "exporter", "oidc", "insecureSkipVerify")),
			want:   []string{`insecureSkipVerify is "false"; ` + wantSkipVerify}},
		{name: "secret name invalid", recipe: recipeOf(set(exporter(), "Event_Exporter", "oidcSecretName")),
			want: []string{wantSecret}},
		{name: "secret name left to the chart", recipe: recipeOf(unset(exporter(), "oidcSecretName")),
			want: []string{"oidcSecretName " + wantNotSet}},
		{name: "secret name from --set parsed as a number", recipe: recipeOf(exporter()),
			cfg:  sets(map[string]string{eventExporterSecretNamePath: "12345"}),
			want: []string{"oidcSecretName is 12345, " + wantNotString}},
		{name: "no per-deployment values", recipe: func() *recipe.RecipeResult {
			e := exporter()
			x := section(e, "exporter")
			delete(x, "metadata")
			delete(section(e, "exporter", "sink"), "endpoint")
			x["oidc"] = map[string]any{"insecureSkipVerify": false}
			return recipeOf(e)
		}(), want: []string{
			"sink.endpoint " + wantNotSet, "oidc.tokenUrl " + wantNotSet, "oidc.clientId " + wantNotSet,
			"oidc.scope " + wantNotSet, "metadata.environment " + wantNotSet, "metadata.cluster " + wantNotSet,
		}},
		{name: "per-deployment value dynamic", recipe: recipeOf(exporter()), cfg: dynamic(eventExporterSinkEndpointPath),
			want: []string{wantDynamic}},
		{name: "toggle dynamic while off", recipe: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent}}},
			cfg: dynamic(eventExporterEnabledPath), want: []string{wantDynamic}},
		{name: "nvsentinel disabled", recipe: func() *recipe.RecipeResult {
			r := recipeOf(unset(exporter(), "exporter", "sink", "endpoint"))
			r.ComponentRefs[0].Overrides["enabled"] = false
			return r
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelEventExporterCoherent(t.Context(), nvsentinelComponent, tt.recipe, cfg, nil)
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

func TestCheckNVSentinelEventExporterCoherentNilRecipe(t *testing.T) {
	t.Parallel()
	if w, e := CheckNVSentinelEventExporterCoherent(t.Context(), nvsentinelComponent, nil, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("nil recipe: got %v %v, want nothing", w, e)
	}
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	if w, e := CheckNVSentinelEventExporterCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("no nvsentinel: got %v %v, want nothing", w, e)
	}
}

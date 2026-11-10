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

func TestCheckNVSentinelCSPHealthMonitorCoherent(t *testing.T) {
	t.Parallel()

	// monitor is the csp-health-monitor block for cloud with every
	// per-cluster value an operator supplies set.
	monitor := func(cloud string) map[string]any {
		toml := map[string]any{"clusterName": "train-1", "triggerQuarantineWorkflowTimeLimitMinutes": 30}
		switch cloud {
		case "aws":
			toml["aws"] = map[string]any{"accountId": "012345678901", "region": "us-east-1"}
		case "gcp":
			toml["gcp"] = map[string]any{
				"targetProjectId":       "gpu-prod",
				"gcpServiceAccountName": "nvsentinel-csp",
				"logFilter":             `protoPayload.methodName="compute.instances.upcomingMaintenance"`,
			}
		}
		return map[string]any{"cspName": cloud, "processingStrategy": "STORE_ONLY", "configToml": toml}
	}
	recipeOf := func(service recipe.CriteriaServiceType, csp map[string]any) *recipe.RecipeResult {
		r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
			Name: nvsentinelComponent,
			Overrides: map[string]any{
				"global":             map[string]any{"cspHealthMonitor": map[string]any{"enabled": true}},
				"csp-health-monitor": csp,
			},
		}}}
		if service != "" {
			r.Criteria = &recipe.Criteria{Service: service}
		}
		return r
	}
	toml := func(csp map[string]any) map[string]any { return csp["configToml"].(map[string]any) }
	with := func(m map[string]any, key string, value any) map[string]any {
		m[key] = value
		return m
	}
	without := func(m map[string]any, key string) map[string]any {
		delete(m, key)
		return m
	}
	awsWith := func(key string, value any) map[string]any {
		csp := monitor("aws")
		toml(csp)["aws"].(map[string]any)[key] = value
		return csp
	}
	gcpWith := func(key string, value any) map[string]any {
		csp := monitor("gcp")
		toml(csp)["gcp"].(map[string]any)[key] = value
		return csp
	}
	clusterNamed := func(cloud, name string) map[string]any {
		csp := monitor(cloud)
		toml(csp)["clusterName"] = name
		return csp
	}
	sets := func(kv map[string]string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{"nv-sentinel": kv}))
	}
	// setJSON parses specs as the CLI parses --set-json.
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
		wantUnsupported = "only with aws on eks and gcp on gke"
		wantMismatch    = "but the recipe's service is"
		wantStrategy    = "but AICR supports only STORE_ONLY"
		wantClusterName = "configToml.clusterName is empty"
		wantClusterNum  = "configToml.clusterName is 12345, not a string"
		wantAccount     = "aws.accountId is empty"
		wantAccountNum  = "--set parses an all-digit value as a number"
		wantAccountFmt  = "not a 12-digit AWS account ID"
		wantRegion      = "aws.region is empty"
		wantRegionAPI   = "AWS Health API has endpoints only in"
		wantRoleName    = "is not a valid IAM role"
		wantRoleType    = "aws.iamRoleName is 42, not a string"
		wantRegionNum   = "aws.region is 1, not a string"
		wantClusterPath = "a / there would be read as an IAM path"
		wantNotWhole    = "want a whole number below 1000000"
		wantBelowMin    = "below the monitor's minimum"
		wantUseAWS      = "compose nvsentinel-csp-health-monitor-aws instead"
		wantNoMonitor   = "AICR has no CSP Health Monitor for it"
		wantGCPNum      = "gcp.targetProjectId is 123456, not a string"
		wantGCPEmail    = "not a service account ID"
		wantProject     = "gcp.targetProjectId is empty"
		wantGCPAccount  = "gcp.gcpServiceAccountName is empty"
		wantLogFilter   = "gcp.logFilter is empty"
		wantDynamic     = "--dynamic"
	)

	tests := []struct {
		name   string
		recipe *recipe.RecipeResult
		cfg    *config.Config
		want   []string
	}{
		{name: "monitor off", recipe: &recipe.RecipeResult{
			Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceEKS},
			ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent}},
		}},
		{name: "aws on eks", recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("aws"))},
		{name: "gcp on gke", recipe: recipeOf(recipe.CriteriaServiceGKE, monitor("gcp"))},
		{name: "aws on gke", recipe: recipeOf(recipe.CriteriaServiceGKE, monitor("aws")), want: []string{wantMismatch}},
		{name: "gcp on eks", recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("gcp")), want: []string{wantUseAWS}},
		{name: "aws on aks", recipe: recipeOf(recipe.CriteriaServiceAKS, monitor("aws")), want: []string{wantNoMonitor}},
		{name: "gcp on oke", recipe: recipeOf(recipe.CriteriaServiceOKE, monitor("gcp")), want: []string{wantMismatch}},
		{name: "aws on kind", recipe: recipeOf(recipe.CriteriaServiceKind, monitor("aws")), want: []string{wantMismatch}},
		{name: "aws with no service", recipe: recipeOf("", monitor("aws")), want: []string{wantMismatch}},
		{name: "lambda", recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "cspName", "lambda")),
			want: []string{wantUnsupported}},
		{name: "cspName not a string", recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "cspName", 42)),
			want: []string{"cspName is 42, but"}},
		{name: "processingStrategy not a string",
			recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "processingStrategy", true)),
			want:   []string{"processingStrategy is true, but AICR supports only STORE_ONLY"}},
		{name: "cspName unset", recipe: recipeOf(recipe.CriteriaServiceEKS, without(monitor("aws"), "cspName")),
			want: []string{wantUnsupported}},
		{name: "processingStrategy left to the chart",
			recipe: recipeOf(recipe.CriteriaServiceEKS, without(monitor("aws"), "processingStrategy")),
			want:   []string{wantStrategy}},
		{name: "processingStrategy EXECUTE_REMEDIATION on aws",
			recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "processingStrategy", "EXECUTE_REMEDIATION")),
			want:   []string{wantStrategy}},
		{name: "processingStrategy EXECUTE_REMEDIATION on gcp",
			recipe: recipeOf(recipe.CriteriaServiceGKE, with(monitor("gcp"), "processingStrategy", "EXECUTE_REMEDIATION")),
			want:   []string{wantStrategy}},
		{name: "EXECUTE_REMEDIATION by --set over the aws mixin's STORE_ONLY",
			recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("aws")),
			cfg:    sets(map[string]string{cspMonitorProcessingStrategyPath: "EXECUTE_REMEDIATION"}),
			want:   []string{wantStrategy}},
		{name: "EXECUTE_REMEDIATION by --set over the gcp mixin's STORE_ONLY",
			recipe: recipeOf(recipe.CriteriaServiceGKE, monitor("gcp")),
			cfg:    sets(map[string]string{cspMonitorProcessingStrategyPath: "EXECUTE_REMEDIATION"}),
			want:   []string{wantStrategy}},
		{name: "EXECUTE_REMEDIATION by --set-json",
			recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("aws")),
			cfg:    setJSON(`nv-sentinel:` + cspMonitorProcessingStrategyPath + `="EXECUTE_REMEDIATION"`),
			want:   []string{wantStrategy}},
		{name: "processingStrategy UNSPECIFIED", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "processingStrategy", "UNSPECIFIED")), want: []string{wantStrategy}},
		{name: "processingStrategy misspelled",
			recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "processingStrategy", "store_only")),
			want:   []string{wantStrategy}},
		{name: "clusterName empty", recipe: recipeOf(recipe.CriteriaServiceEKS, clusterNamed("aws", "")),
			want: []string{wantClusterName}},
		{name: "clusterName null", recipe: func() *recipe.RecipeResult {
			csp := monitor("aws")
			toml(csp)["clusterName"] = nil
			return recipeOf(recipe.CriteriaServiceEKS, csp)
		}(), want: []string{wantClusterName}},
		{name: "gcp project null", recipe: recipeOf(recipe.CriteriaServiceGKE, gcpWith("targetProjectId", nil)),
			want: []string{wantProject}},
		{name: "clusterName blank", recipe: recipeOf(recipe.CriteriaServiceGKE, clusterNamed("gcp", "  ")),
			want: []string{wantClusterName}},
		{name: "clusterName from --set parsed as a number",
			recipe: recipeOf(recipe.CriteriaServiceEKS, clusterNamed("aws", "")),
			cfg:    sets(map[string]string{cspMonitorClusterNamePath: "12345"}),
			want:   []string{wantClusterNum}},
		{name: "clusterName from --set",
			recipe: recipeOf(recipe.CriteriaServiceEKS, clusterNamed("aws", "")),
			cfg:    sets(map[string]string{cspMonitorClusterNamePath: "train-2"})},
		{name: "accountId missing", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("accountId", nil)),
			want: []string{wantAccount}},
		{name: "accountId given as a number", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("accountId", int64(123456789012))),
			want: []string{wantAccountNum}},
		{name: "accountId given as a boolean", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("accountId", true)),
			want: []string{"aws.accountId is true, not a string"}},
		{name: "accountId from --set is parsed as a number",
			recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("aws")),
			cfg:    sets(map[string]string{cspMonitorAWSAccountIDPath: "012345678901"}),
			want:   []string{wantAccountNum}},
		{name: "accountId from --set-json as a string",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("accountId", nil)),
			cfg:    setJSON(`nv-sentinel:` + cspMonitorAWSAccountIDPath + `="012345678901"`)},
		{name: "accountId short", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("accountId", "12345678901")),
			want: []string{wantAccountFmt}},
		{name: "region missing", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("region", "")),
			want: []string{wantRegion}},
		{name: "region us-east-2", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("region", "us-east-2"))},
		{name: "region not a string", recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("region", int64(1))),
			want: []string{wantRegionNum}},
		{name: "region without a Health API endpoint",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("region", "us-west-2")), want: []string{wantRegionAPI}},
		{name: "clusterName with a / feeding the derived role name",
			recipe: recipeOf(recipe.CriteriaServiceEKS, clusterNamed("aws", "team/gpu")), want: []string{wantClusterPath}},
		{name: "clusterName with a / beside an explicit iamRoleName", recipe: func() *recipe.RecipeResult {
			csp := clusterNamed("aws", "team/gpu")
			toml(csp)["aws"].(map[string]any)["iamRoleName"] = "gpu-csp"
			return recipeOf(recipe.CriteriaServiceEKS, csp)
		}()},
		{name: "derived role name at IAM's limit",
			recipe: recipeOf(recipe.CriteriaServiceEKS, clusterNamed("aws", strings.Repeat("c", 19)))},
		{name: "derived role name over IAM's limit",
			recipe: recipeOf(recipe.CriteriaServiceEKS, clusterNamed("aws", strings.Repeat("c", 20))),
			want:   []string{wantRoleName}},
		{name: "iamRoleName for a long cluster name", recipe: func() *recipe.RecipeResult {
			csp := clusterNamed("aws", strings.Repeat("c", 40))
			toml(csp)["aws"].(map[string]any)["iamRoleName"] = "nvsentinel-csp"
			return recipeOf(recipe.CriteriaServiceEKS, csp)
		}()},
		{name: "iamRoleName over IAM's limit",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", strings.Repeat("r", 65))),
			want:   []string{wantRoleName}},
		{name: "iamRoleName with a space",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "bad role")), want: []string{wantRoleName}},
		{name: "iamRoleName given as the role ARN",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "arn:aws:iam::012345678901:role/r")),
			want:   []string{wantRoleName}},
		{name: "iamRoleName not a string",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", int64(42))), want: []string{wantRoleType}},
		{name: "iamRoleName behind a path",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "teams/gpu/"+strings.Repeat("r", 64)))},
		{name: "iamRoleName behind a path over IAM's limit",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "teams/"+strings.Repeat("r", 65))),
			want:   []string{wantRoleName}},
		{name: "iamRoleName with an empty path segment, which IAM's path pattern allows",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "teams//r"))},
		{name: "iamRoleName whose path holds punctuation IAM allows in a path",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "teams:gpu!a~b/csp-monitor"))},
		{name: "iamRoleName with a leading /",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "/teams/csp")), want: []string{wantRoleName}},
		{name: "iamRoleName that is only / and a name",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "/csp")), want: []string{wantRoleName}},
		{name: "iamRoleName with no name after the path",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "teams/")), want: []string{wantRoleName}},
		{name: "iamRoleName whose name holds a path-only character",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "teams/csp:monitor")), want: []string{wantRoleName}},
		{name: "iamRoleName whose path holds a space",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "my teams/csp")), want: []string{wantRoleName}},
		{name: "iamRoleName path at IAM's limit",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", strings.Repeat("p", 510)+"/csp"))},
		{name: "iamRoleName path over IAM's limit",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", strings.Repeat("p", 511)+"/csp")),
			want:   []string{wantRoleName}},
		{name: "iamRoleName with IAM's other characters",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("iamRoleName", "team+csp=a,b.c@d_e-f"))},
		{name: "lead time negative", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "triggerQuarantineWorkflowTimeLimitMinutes", -1))),
			want: []string{wantBelowMin}},
		{name: "lead time with a unit", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "triggerQuarantineWorkflowTimeLimitMinutes", "45m"))),
			want: []string{wantNotWhole}},
		{name: "lead time rendered in exponent form", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "triggerQuarantineWorkflowTimeLimitMinutes", float64(1e6)))),
			want: []string{wantNotWhole}},
		{name: "readiness timeout over the exponent bound as an int64", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "nodeReadinessTimeoutMinutes", int64(3_000_000_000)))),
			want: []string{wantNotWhole}},
		{name: "readiness timeout just below the exponent bound", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "nodeReadinessTimeoutMinutes", float64(999_999))))},
		{name: "lead time 0 takes the monitor's default", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "triggerQuarantineWorkflowTimeLimitMinutes", 0)))},
		{name: "event poll below the monitor's 10 seconds", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "maintenanceEventPollIntervalSeconds", 5))),
			want: []string{wantBelowMin}},
		{name: "event poll 0 takes the monitor's default", recipe: recipeOf(recipe.CriteriaServiceEKS,
			with(monitor("aws"), "configToml", with(toml(monitor("aws")), "maintenanceEventPollIntervalSeconds", 0)))},
		{name: "aws poll below 30 seconds",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("pollingIntervalSeconds", float64(15))),
			want:   []string{wantBelowMin}},
		{name: "aws poll at 30 seconds",
			recipe: recipeOf(recipe.CriteriaServiceEKS, awsWith("pollingIntervalSeconds", float64(30)))},
		{name: "gcp poll fractional",
			recipe: recipeOf(recipe.CriteriaServiceGKE, gcpWith("apiPollingIntervalSeconds", 45.5)),
			want:   []string{wantNotWhole}},
		{name: "aws poll minimum ignored on gcp",
			recipe: recipeOf(recipe.CriteriaServiceGKE, with(monitor("gcp"), "configToml",
				with(toml(monitor("gcp")), "aws", map[string]any{"pollingIntervalSeconds": 1})))},
		{name: "lambda poll on aws still decoded",
			recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "configToml",
				with(toml(monitor("aws")), "lambda", map[string]any{"pollingIntervalSeconds": 1.5}))),
			want: []string{wantNotWhole}},
		{name: "lambda poll with a unit on gcp",
			recipe: recipeOf(recipe.CriteriaServiceGKE, with(monitor("gcp"), "configToml",
				with(toml(monitor("gcp")), "lambda", map[string]any{"pollingIntervalSeconds": "45s"}))),
			want: []string{wantNotWhole}},
		{name: "lambda poll minimum ignored on aws",
			recipe: recipeOf(recipe.CriteriaServiceEKS, with(monitor("aws"), "configToml",
				with(toml(monitor("aws")), "lambda", map[string]any{"pollingIntervalSeconds": 10})))},
		{name: "aws poll on gcp still decoded",
			recipe: recipeOf(recipe.CriteriaServiceGKE, with(monitor("gcp"), "configToml",
				with(toml(monitor("gcp")), "aws", map[string]any{"pollingIntervalSeconds": 1.5}))),
			want: []string{wantNotWhole}},
		{name: "gcp project missing", recipe: recipeOf(recipe.CriteriaServiceGKE, gcpWith("targetProjectId", "")),
			want: []string{wantProject}},
		{name: "gcp service account missing", recipe: recipeOf(recipe.CriteriaServiceGKE, gcpWith("gcpServiceAccountName", "")),
			want: []string{wantGCPAccount}},
		{name: "gcp service account given as its email", recipe: recipeOf(recipe.CriteriaServiceGKE,
			gcpWith("gcpServiceAccountName", "nvsentinel-csp@gpu-prod.iam.gserviceaccount.com")),
			want: []string{wantGCPEmail}},
		{name: "gcp project not a string",
			recipe: recipeOf(recipe.CriteriaServiceGKE, gcpWith("targetProjectId", int64(123456))), want: []string{wantGCPNum}},
		{name: "gcp log filter missing", recipe: recipeOf(recipe.CriteriaServiceGKE, gcpWith("logFilter", "")),
			want: []string{wantLogFilter}},
		{name: "aws with no per-cluster values", recipe: func() *recipe.RecipeResult {
			csp := monitor("aws")
			csp["configToml"] = map[string]any{"triggerQuarantineWorkflowTimeLimitMinutes": 30}
			return recipeOf(recipe.CriteriaServiceEKS, csp)
		}(), want: []string{wantClusterName, wantAccount, wantRegion}},
		{name: "gcp with no per-cluster values", recipe: func() *recipe.RecipeResult {
			csp := monitor("gcp")
			csp["configToml"] = map[string]any{"triggerQuarantineWorkflowTimeLimitMinutes": 30}
			return recipeOf(recipe.CriteriaServiceGKE, csp)
		}(), want: []string{wantClusterName, wantProject, wantGCPAccount, wantLogFilter}},
		{name: "per-cluster value dynamic", recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("aws")),
			cfg: dynamic(cspMonitorClusterNamePath), want: []string{wantDynamic}},
		{name: "cspName dynamic", recipe: recipeOf(recipe.CriteriaServiceEKS, monitor("aws")),
			cfg: dynamic(cspMonitorNamePath), want: []string{wantDynamic}},
		{name: "toggle dynamic while off", recipe: &recipe.RecipeResult{
			Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceEKS},
			ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent}},
		}, cfg: dynamic(cspMonitorEnabledPath), want: []string{wantDynamic}},
		{name: "nvsentinel disabled", recipe: func() *recipe.RecipeResult {
			r := recipeOf(recipe.CriteriaServiceAKS, monitor("aws"))
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
			warnings, errs := CheckNVSentinelCSPHealthMonitorCoherent(t.Context(), nvsentinelComponent, tt.recipe, cfg, nil)
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

func TestCheckNVSentinelCSPHealthMonitorCoherentNilRecipe(t *testing.T) {
	t.Parallel()
	if w, e := CheckNVSentinelCSPHealthMonitorCoherent(t.Context(), nvsentinelComponent, nil, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("nil recipe: got %v %v, want nothing", w, e)
	}
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	if w, e := CheckNVSentinelCSPHealthMonitorCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("no nvsentinel: got %v %v, want nothing", w, e)
	}
}

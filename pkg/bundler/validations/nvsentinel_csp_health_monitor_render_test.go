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
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// The workload name, and the images docs/user/container-images.md discloses
// for the mixins.
const (
	cspHealthMonitorName     = "csp-health-monitor"
	cspHealthMonitorImage    = "ghcr.io/nvidia/nvsentinel/csp-health-monitor:v1.28.0"
	maintenanceNotifierImage = "ghcr.io/nvidia/nvsentinel/maintenance-notifier:v1.28.0"
)

// cspMonitorClusterValues are the per-cluster values an operator passes on
// the bundle command, by cloud.
var cspMonitorClusterValues = map[string]map[string]any{
	"aws": {"clusterName": "train-1", "aws": map[string]any{"accountId": "012345678901", "region": "us-east-1"}},
	"gcp": {"clusterName": "train-1", "gcp": map[string]any{
		"targetProjectId":       "gpu-prod",
		"gcpServiceAccountName": "nvsentinel-csp",
		"logFilter":             `logName="projects/gpu-prod/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"`,
	}},
}

// cspMonitorMixinOverrides returns a copy of the named mixin's nvsentinel
// overrides with cloud's per-cluster values added.
func cspMonitorMixinOverrides(t *testing.T, store *recipe.MetadataStore, mixin, cloud string) map[string]any {
	t.Helper()
	m, ok := store.Mixins[mixin]
	if !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/", mixin)
	}
	overrides := map[string]any{}
	for _, c := range m.Spec.ComponentRefs {
		if c.Name == nvsentinelComponent {
			mergeValuesForRender(overrides, c.Overrides)
		}
	}
	mergeValuesForRender(overrides, map[string]any{
		cspHealthMonitorName: map[string]any{"configToml": cspMonitorClusterValues[cloud]},
	})
	return overrides
}

// TestCSPHealthMonitorProviderMatchesService composes each CSP Health
// Monitor mixin onto every shipped leaf, with every per-cluster value
// supplied, and requires the bundle gate to accept it only where the
// mixin's cloud runs the recipe's service: no recipe can render a provider
// that does not match it.
func TestCSPHealthMonitorProviderMatchesService(t *testing.T) {
	t.Parallel()
	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	mixins := map[string]struct {
		cloud   string
		service recipe.CriteriaServiceType
	}{
		"nvsentinel-csp-health-monitor-aws": {"aws", recipe.CriteriaServiceEKS},
		"nvsentinel-csp-health-monitor-gcp": {"gcp", recipe.CriteriaServiceGKE},
	}
	accepted := map[string]int{}
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		for mixin, want := range mixins {
			r := &recipe.RecipeResult{
				Criteria: overlay.Spec.Criteria,
				ComponentRefs: []recipe.ComponentRef{{
					Name: nvsentinelComponent, Overrides: cspMonitorMixinOverrides(t, store, mixin, want.cloud),
				}},
			}
			_, errs := CheckNVSentinelCSPHealthMonitorCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil)
			matches := overlay.Spec.Criteria.Service == want.service
			switch {
			case matches && len(errs) > 0:
				t.Errorf("%s on %s (service %s) is rejected: %v", mixin, name, overlay.Spec.Criteria.Service, errs)
			case !matches && len(errs) == 0:
				t.Errorf("%s on %s (service %q) is accepted; only %s runs its maintenance API",
					mixin, name, overlay.Spec.Criteria.Service, want.service)
			case matches:
				accepted[mixin]++
			}
		}
	}
	for mixin := range mixins {
		if accepted[mixin] == 0 {
			t.Errorf("%s is accepted on no shipped leaf -- the walker or the gate is broken", mixin)
		}
	}
}

// TestNVSentinelCSPHealthMonitorChartRender renders the pinned chart with each
// CSP mixin and asserts the images, STORE_ONLY, the config and each cloud's
// workload identity annotation. The lead time equals the chart default, so
// TestMixinNVSentinelCSPHealthMonitor_Composes pins it on the mixin instead.
// -short skips it (network).
func TestNVSentinelCSPHealthMonitorChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	for _, tc := range []struct {
		mixin, cloud, annotation, wantIdentity string
	}{
		{"nvsentinel-csp-health-monitor-aws", "aws", "eks.amazonaws.com/role-arn",
			"arn:aws:iam::012345678901:role/train-1-nvsentinel-health-monitor-assume-role-policy"},
		{"nvsentinel-csp-health-monitor-gcp", "gcp", "iam.gke.io/gcp-service-account",
			"nvsentinel-csp@gpu-prod.iam.gserviceaccount.com"},
	} {
		t.Run(tc.mixin, func(t *testing.T) {
			values := nvsentinelRenderedValues(t, store, "nvsentinel-remediation", tc.mixin)
			mergeValuesForRender(values, map[string]any{
				cspHealthMonitorName: map[string]any{"configToml": cspMonitorClusterValues[tc.cloud]},
			})
			out := renderNVSentinel(t, values)

			containers := podContainers(t, renderedObject(t, out, "Deployment", cspHealthMonitorName))
			images := make([]string, 0, len(containers))
			for _, c := range containers {
				image, _ := c["image"].(string)
				images = append(images, image)
			}
			slices.Sort(images)
			if want := []string{cspHealthMonitorImage, maintenanceNotifierImage}; !slices.Equal(images, want) {
				t.Errorf("csp-health-monitor images = %v, want %v", images, want)
			}
			if args := stringList(containers["maintenance-notifier"]["args"]); !slices.Contains(args, "--processing-strategy=STORE_ONLY") {
				t.Errorf("maintenance-notifier args %v do not carry --processing-strategy=STORE_ONLY", args)
			}

			configMap := renderedObject(t, out, "ConfigMap", cspHealthMonitorName)
			data, _ := configMap["data"].(map[string]any)
			toml, _ := data["config.toml"].(string)
			for _, want := range []string{
				`clusterName = "train-1"`,
				"[" + tc.cloud + "]\nenabled = true",
			} {
				if !strings.Contains(toml, want) {
					t.Errorf("config.toml does not contain %q:\n%s", want, toml)
				}
			}

			sa := renderedObject(t, out, "ServiceAccount", cspHealthMonitorName)
			meta, _ := sa["metadata"].(map[string]any)
			annotations, _ := meta["annotations"].(map[string]any)
			if got := annotations[tc.annotation]; got != tc.wantIdentity {
				t.Errorf("ServiceAccount %s = %v, want %s", tc.annotation, got, tc.wantIdentity)
			}
		})
	}
}

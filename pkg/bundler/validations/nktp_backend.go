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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nktpLGTMEnabledPath = "otel-lgtm.enabled"

// nktpLGTMEndpoints are the endpoints of NKTP's five backend exporters, the
// three gateways, the watchdog, and the watchdog's sidecar, whose chart
// default is otel-lgtm (chart 0.1.1-rc.3). The other collectors export to
// these.
var nktpLGTMEndpoints = []string{
	"gateway_metrics.otelcol.config.exporters.otlp.endpoint",
	"gateway_logs.otelcol.config.exporters.otlp.endpoint",
	"gateway_traces.otelcol.config.exporters.otlp.endpoint",
	"watchdog.otelcol.config.exporters.otlp.endpoint",
	"watchdog.sidecar.otelcol.config.exporters.otlp.endpoint",
}

// CheckNKTPExportersHaveBackend blocks a bundle that disables NKTP's bundled
// otel-lgtm backend while an exporter endpoint is unset, so the chart default
// still points it at otel-lgtm. Such a release deploys cleanly and reports
// healthy while every exporter retries into a missing Service and drops what
// it receives.
//
// A null endpoint counts as unset. An endpoint covered by a --dynamic path
// counts as set, since the operator supplies it at install time. A --dynamic
// otel-lgtm toggle counts as disabled, since it can be flipped then.
func CheckNKTPExportersHaveBackend(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	ref := recipeResult.GetComponentRef(componentName)
	if ref == nil {
		return nil, nil
	}
	keys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(ref, bundlerConfig, keys) {
		return nil, nil
	}
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, keys,
		"NKTP exporter backend")
	if err != nil {
		return nil, []error{err}
	}

	toggleDynamic := len(dynamicPathIntersections(bundlerConfig, keys, []string{nktpLGTMEnabledPath})) > 0
	if nktpLGTMEnabled(values) && !toggleDynamic {
		return nil, nil
	}

	var unset []string
	for _, path := range nktpLGTMEndpoints {
		if len(dynamicPathIntersections(bundlerConfig, keys, []string{path})) > 0 {
			continue
		}
		if v, ok := component.GetValueByPath(values, path); !ok || v == nil {
			unset = append(unset, path)
		}
	}
	if len(unset) == 0 {
		return nil, nil
	}
	state := "otel-lgtm is disabled"
	if toggleDynamic {
		state = nktpLGTMEnabledPath + " is deferred to install time with --dynamic, where it can be disabled"
	}
	return nil, []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
		"component %q: %s, but %d exporter endpoint(s) are unset and default to the otel-lgtm Service: %s. "+
			"Point each at your backend with --set %s:<path>=<host:port>, or keep otel-lgtm enabled",
		componentName, state, len(unset), strings.Join(unset, ", "), componentName))}
}

// nktpLGTMEnabled reports whether otel-lgtm renders. An absent toggle is the
// chart default, enabled; a null otel-lgtm section deletes that default, and
// the chart's `if` on a missing key renders nothing.
func nktpLGTMEnabled(values map[string]any) bool {
	raw, present := values["otel-lgtm"]
	if !present {
		return true
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	enabled, present := section["enabled"]
	if !present {
		return true
	}
	return helmTruthy(enabled)
}

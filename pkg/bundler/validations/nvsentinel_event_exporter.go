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
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Value paths CheckNVSentinelEventExporterCoherent reads under nvsentinel, the
// chart's placeholders it rejects, and the characters the chart's TOML
// interpolation cannot carry.
const (
	eventExporterEnabledPath          = "global.eventExporter.enabled"
	eventExporterPath                 = "event-exporter"
	eventExporterSecretNamePath       = "event-exporter.oidcSecretName"
	eventExporterSinkEndpointPath     = "event-exporter.exporter.sink.endpoint"
	eventExporterSinkSkipVerifyPath   = "event-exporter.exporter.sink.insecureSkipVerify"
	eventExporterTokenURLPath         = "event-exporter.exporter.oidc.tokenUrl"
	eventExporterClientIDPath         = "event-exporter.exporter.oidc.clientId"
	eventExporterScopePath            = "event-exporter.exporter.oidc.scope"
	eventExporterOIDCSkipVerifyPath   = "event-exporter.exporter.oidc.insecureSkipVerify"
	eventExporterMetadataPath         = "event-exporter.exporter.metadata"
	eventExporterClusterPath          = "event-exporter.exporter.metadata.cluster"
	eventExporterEnvironmentPath      = "event-exporter.exporter.metadata.environment"
	eventExporterChartClusterDefault  = "my-cluster"
	eventExporterPlaceholderDomain    = "example.com"
	eventExporterPlaceholderDomainOrg = "example.org"
	eventExporterPlaceholderDomainNet = "example.net"
	eventExporterPlaceholderTLD       = "example"
)

// CheckNVSentinelEventExporterCoherent blocks a bundle in which NVSentinel's
// Event Exporter cannot deliver anything. The chart ships placeholder
// endpoints and renders whatever it is given, so every case below deploys
// cleanly and then retries into nothing. It fails when:
//
//   - sink.endpoint or oidc.tokenUrl is not an http or https URL with a
//     host, names an example.com, .org or .net host, as the chart's
//     placeholders do, or a host under the reserved .example TLD, or uses
//     plain http to a host outside the cluster, which would send the client
//     credentials and bearer token unencrypted;
//   - oidc.clientId, oidc.scope, metadata.cluster or metadata.environment is
//     not set by AICR's values, so the chart's placeholder would apply
//     (metadata.cluster may not be the chart's "my-cluster" either);
//   - any of those, or another metadata value, holds a double quote,
//     backslash or control character other than tab, or a metadata key is not
//     a TOML bare key: the chart writes them into its TOML config without
//     escaping, and the exporter refuses the broken config;
//   - sink or OIDC insecureSkipVerify is anything but false, since events
//     and the client credentials leave the cluster;
//   - oidcSecretName is not a valid Secret name.
//
// The datastore it reads is required by
// CheckNVSentinelRemediationPipelineCoherent. It is a no-op unless the
// exporter is enabled, and then rejects --dynamic on any event-exporter
// value.
func CheckNVSentinelEventExporterCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	sentinelKeys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys,
		"NVSentinel Event Exporter")
	if err != nil {
		return nil, []error{err}
	}

	enabled := nvsentinelSubchartRenders(values, "eventExporter")
	guarded := []string{eventExporterEnabledPath}
	if enabled {
		guarded = append(guarded, eventExporterPath)
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, guarded,
		"decides whether the Event Exporter runs, where it delivers and how it authenticates"); len(msgs) > 0 {
		for _, msg := range msgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return msgs, nil
	}
	if !enabled {
		return nil, nil
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}

	for _, path := range []string{eventExporterSinkEndpointPath, eventExporterTokenURLPath} {
		if v, ok := eventExporterString(values, path, true, fail); ok {
			if why := eventExporterURLProblem(v); why != "" {
				fail("%s is %q, %s -- set it with --set nv-sentinel:%s=<URL>", path, v, why, path)
			}
		}
	}
	for _, path := range []string{eventExporterClientIDPath, eventExporterScopePath, eventExporterEnvironmentPath} {
		eventExporterString(values, path, true, fail)
	}
	if cluster, ok := eventExporterString(values, eventExporterClusterPath, true, fail); ok && cluster == eventExporterChartClusterDefault {
		fail("%s is the chart's placeholder %q -- set it to this cluster's name with --set nv-sentinel:%s=<cluster>",
			eventExporterClusterPath, cluster, eventExporterClusterPath)
	}
	eventExporterMetadataViolations(values, fail)

	for _, path := range []string{eventExporterSinkSkipVerifyPath, eventExporterOIDCSkipVerifyPath} {
		if raw, present := component.GetValueByPath(values, path); present && raw != false {
			fail("%s is %#v; it must be the boolean false: health events and the OIDC client credentials leave "+
				"the cluster, so the endpoint's certificate must be verified", path, raw)
		}
	}

	if secret, ok := eventExporterString(values, eventExporterSecretNamePath, true, fail); ok {
		if problems := validation.IsDNS1123Subdomain(secret); len(problems) > 0 {
			fail("%s is %q, not a valid Secret name (%s) -- the Deployment mounts it and does not start without it",
				eventExporterSecretNamePath, secret, strings.Join(problems, "; "))
		}
	}

	return nil, errs
}

// eventExporterString returns the string AICR's values set at path, failing
// when it is not a string or holds a character the chart's TOML
// interpolation cannot carry, and, when required, when it is absent or empty.
func eventExporterString(values map[string]any, path string, required bool, fail func(string, ...any)) (string, bool) {
	raw, present := component.GetValueByPath(values, path)
	v, isString := raw.(string)
	switch {
	case !required && (!present || raw == nil):
		return "", false
	case !present || raw == nil || (isString && strings.TrimSpace(v) == "" && required):
		fail("%s is not set, so the chart's placeholder would apply and the exporter would deliver nowhere "+
			"usable. It is per deployment, so no recipe sets it: --set nv-sentinel:%s=<value>", path, path)
		return "", false
	case !isString:
		fail("%s is %v, not a string: --set parses a numeric or boolean value as one -- pass it as a string: "+
			"--set-json 'nv-sentinel:%s=\"<value>\"'", path, raw, path)
		return "", false
	case strings.ContainsFunc(v, eventExporterTOMLUnsafe):
		fail("%s holds a double quote, backslash or control character, which the chart writes into its TOML "+
			"config unescaped, so the exporter refuses to start", path)
		return "", false
	}
	return v, true
}

// eventExporterMetadataViolations checks the metadata entries beyond cluster
// and environment, which the chart also writes into its TOML config.
func eventExporterMetadataViolations(values map[string]any, fail func(string, ...any)) {
	raw, _ := component.GetValueByPath(values, eventExporterMetadataPath)
	metadata, _ := raw.(map[string]any)
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		if key != "cluster" && key != "environment" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !eventExporterBareKey(key) {
			fail("%s has the key %q, which the chart writes into its TOML config as a bare key; use only "+
				"letters, digits, '_' and '-'", eventExporterMetadataPath, key)
			continue
		}
		eventExporterString(values, eventExporterMetadataPath+"."+key, false, fail)
	}
}

// eventExporterTOMLUnsafe reports whether r cannot appear unescaped in a TOML
// basic string.
func eventExporterTOMLUnsafe(r rune) bool {
	return r == '"' || r == '\\' || (unicode.IsControl(r) && r != '\t')
}

// eventExporterBareKey reports whether key is a TOML bare key.
func eventExporterBareKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// eventExporterURLProblem describes why v cannot be a sink or token URL, or
// returns "".
func eventExporterURLProblem(v string) string {
	u, err := url.Parse(v)
	switch {
	case err != nil:
		return "not a URL"
	case u.Scheme != "http" && u.Scheme != "https":
		return "not an http or https URL"
	}
	// A fully qualified name may end in a dot; it names the same host.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "a URL with no host"
	}
	for _, domain := range []string{eventExporterPlaceholderDomain, eventExporterPlaceholderDomainOrg,
		eventExporterPlaceholderDomainNet, eventExporterPlaceholderTLD} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return "a placeholder host, like the chart's own"
		}
	}
	if u.Scheme == "http" && !eventExporterClusterLocalHost(host) {
		return "plain http to a host outside the cluster, which would send the client credentials and bearer " +
			"token unencrypted -- use https"
	}
	return ""
}

// eventExporterClusterLocalHost reports whether host resolves inside the
// cluster: a single-label Service name, <service>.<namespace>.svc, or the
// fully qualified svc.cluster.local form. An IP literal is never treated as
// one, since it can be any address; a colon marks IPv6, including the zoned
// form net.ParseIP rejects, and never appears in a DNS name.
func eventExporterClusterLocalHost(host string) bool {
	if strings.Contains(host, ":") || net.ParseIP(host) != nil {
		return false
	}
	return !strings.Contains(host, ".") || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

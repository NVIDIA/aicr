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
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Values CheckNVSentinelCSPHealthMonitorCoherent reads and compares against.
const (
	cspMonitorEnabledPath            = "global.cspHealthMonitor.enabled"
	cspMonitorPath                   = "csp-health-monitor"
	cspMonitorNamePath               = "csp-health-monitor.cspName"
	cspMonitorProcessingStrategyPath = "csp-health-monitor.processingStrategy"
	cspMonitorClusterNamePath        = "csp-health-monitor.configToml.clusterName"
	cspMonitorAWSAccountIDPath       = "csp-health-monitor.configToml.aws.accountId"
	cspMonitorAWSRegionPath          = "csp-health-monitor.configToml.aws.region"
	cspMonitorAWSRoleNamePath        = "csp-health-monitor.configToml.aws.iamRoleName"
	cspMonitorGCPProjectPath         = "csp-health-monitor.configToml.gcp.targetProjectId"
	cspMonitorGCPServiceAccountPath  = "csp-health-monitor.configToml.gcp.gcpServiceAccountName"
	cspMonitorGCPLogFilterPath       = "csp-health-monitor.configToml.gcp.logFilter"
	cspMonitorAWSDefaultRoleSuffix   = "-nvsentinel-health-monitor-assume-role-policy"
	cspMonitorAWSRoleNameMaxLen      = 64
	cspMonitorAWSRolePathMaxLen      = 510
	cspMonitorMixinPrefix            = "nvsentinel-csp-health-monitor-"
	cspMonitorAWS                    = "aws"
	cspMonitorGCP                    = "gcp"
	cspMonitorLambda                 = "lambda"
	cspMonitorStoreOnly              = "STORE_ONLY"
	cspMonitorMaxWholeNumber         = 1e6
)

// cspMonitorIntervals are the timing values the chart writes into the
// monitor's TOML unquoted, with the minimum the monitor accepts at startup
// (health-monitors/csp-health-monitor/pkg/config/config.go, NVSentinel
// v1.28.0). zeroIsDefault marks the ones the monitor replaces with its
// default when 0. aws, gcp and lambda scope the minimum to that provider's
// poller; the monitor decodes all three sections whatever cspName is, so each
// must still be a whole number.
var cspMonitorIntervals = []struct {
	path, provider string
	min            int64
	zeroIsDefault  bool
}{
	{path: "csp-health-monitor.configToml.maintenanceEventPollIntervalSeconds", min: 10, zeroIsDefault: true},
	{path: "csp-health-monitor.configToml.triggerQuarantineWorkflowTimeLimitMinutes", min: 1, zeroIsDefault: true},
	{path: "csp-health-monitor.configToml.postMaintenanceHealthyDelayMinutes", min: 1, zeroIsDefault: true},
	{path: "csp-health-monitor.configToml.nodeReadinessTimeoutMinutes", min: 1, zeroIsDefault: true},
	{path: "csp-health-monitor.configToml.aws.pollingIntervalSeconds", provider: cspMonitorAWS, min: 30},
	{path: "csp-health-monitor.configToml.gcp.apiPollingIntervalSeconds", provider: cspMonitorGCP, min: 30},
	{path: "csp-health-monitor.configToml.lambda.pollingIntervalSeconds", provider: cspMonitorLambda, min: 30},
}

// cspMonitorAWSHealthRegions are the regions the AWS Health API has an
// endpoint in, in the commercial partition. NVSentinel v1.28.0 uses
// aws.region both as the client's region and as the event filter, so a
// cluster elsewhere can neither reach the API nor read its own region's
// events.
var cspMonitorAWSHealthRegions = []string{"us-east-1", "us-east-2"}

// cspMonitorProviderServices maps each provider AICR supports to the one
// service whose maintenance API it polls. Upstream also has lambda, which no
// AICR service runs on.
var cspMonitorProviderServices = map[string]recipe.CriteriaServiceType{
	cspMonitorAWS: recipe.CriteriaServiceEKS,
	cspMonitorGCP: recipe.CriteriaServiceGKE,
}

// Shapes of the identity values the chart builds the cloud identity
// annotation from. iamRoleName is an optional IAM path and a role name, which
// the chart's role/<iamRoleName> ARN carries: awsRoleNamePattern is IAM's
// RoleName (up to cspMonitorAWSRoleNameMaxLen), and awsRolePathPattern IAM's
// Path with its leading and trailing / dropped (up to
// cspMonitorAWSRolePathMaxLen), per IAM's CreateRole reference.
// gcpServiceAccountIDPattern is the part of a service account email before
// the @, which the chart joins with @<targetProjectId>.iam.gserviceaccount.com.
var (
	awsAccountIDPattern        = regexp.MustCompile(`^[0-9]{12}$`)
	awsRoleNamePattern         = regexp.MustCompile(`^[\w+=,.@-]+$`)
	awsRolePathPattern         = regexp.MustCompile(`^[\x21-\x2E\x30-\x7E][\x21-\x7E]*$`)
	gcpServiceAccountIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
)

// CheckNVSentinelCSPHealthMonitorCoherent blocks a bundle in which NVSentinel's
// CSP Health Monitor cannot poll its cloud's maintenance API. The chart
// renders it from whatever it is given, so every case below deploys cleanly.
// It fails when:
//
//   - cspName is not aws or gcp, or does not match the recipe's service (aws
//     on eks, gcp on gke): on any other service there is no API it can read;
//   - processingStrategy is not STORE_ONLY. At NVSentinel v1.28.0 a
//     maintenance event that fault-quarantine acts on can release a node
//     another notice still holds: fault-quarantine keys the quarantine by
//     agent, check, node and instance, among other fields, never by notice, so when two notices
//     share an instance, or a notice stored under STORE_ONLY ends after a
//     switch, the first to end removes the hold. Unset, the chart's
//     EXECUTE_REMEDIATION would apply;
//   - configToml.clusterName is not a non-empty string, which the monitor
//     refuses at startup;
//   - a timing value is not a whole number, or is below the monitor's
//     minimum (cspMonitorIntervals), which crash-loops it; 0 takes the
//     default where zeroIsDefault, and the other provider's poll interval
//     need only be a whole number;
//   - on aws, accountId is not a 12-digit string, region is not one the AWS
//     Health API serves (cspMonitorAWSHealthRegions), or the IAM role the
//     IRSA annotation names (iamRoleName, else <clusterName> plus a
//     45-character suffix, where a / in clusterName is rejected) is not a
//     valid IAM role name;
//   - on gcp, targetProjectId is empty, gcpServiceAccountName is not a
//     service account ID (the chart appends the domain), or logFilter is
//     empty, which the poller ANDs into every query.
//
// The datastore the monitor stores events in is required by
// CheckNVSentinelRemediationPipelineCoherent. It is a no-op unless the
// monitor is enabled, and rejects --dynamic on any path it verifies.
func CheckNVSentinelCSPHealthMonitorCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
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
		"NVSentinel CSP Health Monitor")
	if err != nil {
		return nil, []error{err}
	}

	enabled := nvsentinelSubchartRenders(values, "cspHealthMonitor")
	guarded := []string{cspMonitorEnabledPath}
	if enabled {
		guarded = append(guarded, cspMonitorPath)
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, guarded,
		"decides whether the CSP Health Monitor runs, which cloud it polls and how it authenticates"); len(msgs) > 0 {
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

	var service recipe.CriteriaServiceType
	if recipeResult.Criteria != nil {
		service = recipeResult.Criteria.Service
	}
	cspName, _, _ := resolvedStringValue(values, cspMonitorNamePath)
	want, known := cspMonitorProviderServices[cspName]
	switch {
	case !known:
		raw, _ := component.GetValueByPath(values, cspMonitorNamePath)
		fail("%s is %#v, but AICR supports the CSP Health Monitor only with aws on eks and gcp on gke -- compose "+
			"%saws or %sgcp", cspMonitorNamePath, raw, cspMonitorMixinPrefix, cspMonitorMixinPrefix)
		return nil, errs
	case service != want:
		hint := "AICR has no CSP Health Monitor for it, so do not compose " + cspMonitorMixinPrefix + cspName
		for provider, providerService := range cspMonitorProviderServices {
			if providerService == service {
				hint = "compose " + cspMonitorMixinPrefix + provider + " instead"
			}
		}
		fail("%s is %s, which AICR supports only on %s, but the recipe's service is %q, so the monitor "+
			"would read another cloud's events -- %s", cspMonitorNamePath, cspName, want, service, hint)
		return nil, errs
	}

	if strategy, _, _ := resolvedStringValue(values, cspMonitorProcessingStrategyPath); strategy != cspMonitorStoreOnly {
		raw, _ := component.GetValueByPath(values, cspMonitorProcessingStrategyPath)
		fail("%s is %#v, but AICR supports only %s: at NVSentinel v1.28.0 a maintenance event fault-quarantine "+
			"acts on can release a node another notice still holds (two notices for one instance, or a notice "+
			"stored under %s ending after a switch), uncordoning it during active maintenance -- keep the %s%s "+
			"mixin's %s", cspMonitorProcessingStrategyPath, raw, cspMonitorStoreOnly, cspMonitorStoreOnly,
			cspMonitorMixinPrefix, cspName, cspMonitorStoreOnly)
	}

	clusterName, _ := cspMonitorRequiredString(values, cspMonitorClusterNamePath,
		"it names this cluster in maintenance events and the monitor refuses to start without it", fail)
	cspMonitorIntervalViolations(values, cspName, fail)

	switch cspName {
	case cspMonitorAWS:
		cspMonitorAWSViolations(values, clusterName, fail)
	case cspMonitorGCP:
		cspMonitorRequiredString(values, cspMonitorGCPProjectPath,
			"the ServiceAccount gets no Workload Identity annotation and the monitor cannot read Cloud Logging", fail)
		if account, ok := cspMonitorRequiredString(values, cspMonitorGCPServiceAccountPath,
			"the ServiceAccount gets no Workload Identity annotation and the monitor cannot read Cloud Logging",
			fail); ok && !gcpServiceAccountIDPattern.MatchString(account) {
			fail("%s is %q, not a service account ID: the chart appends @<targetProjectId>.iam.gserviceaccount.com, "+
				"so the Workload Identity annotation names no account -- pass only the part before the @",
				cspMonitorGCPServiceAccountPath, account)
		}
		cspMonitorRequiredString(values, cspMonitorGCPLogFilterPath,
			"the monitor ANDs it into every Cloud Logging query, so no poll succeeds -- use the Compute Engine "+
				"maintenance filter in docs/user/component-catalog.md#cloud-maintenance-events", fail)
	}
	return nil, errs
}

// cspMonitorRequiredString returns the non-empty string at path, or fails
// with why it is needed.
func cspMonitorRequiredString(values map[string]any, path, why string, fail func(string, ...any)) (string, bool) {
	v, _, valid := resolvedStringValue(values, path)
	raw, _ := component.GetValueByPath(values, path)
	switch {
	case !valid && raw != nil:
		fail("%s is %v, not a string: --set parses an all-digit or boolean value as a number or boolean -- pass it "+
			"as a string: "+
			"--set-json 'nv-sentinel:%s=\"<value>\"'", path, raw, path)
	case raw == nil || strings.TrimSpace(v) == "":
		fail("%s is empty: %s. It is per cluster, so no recipe sets it: --set nv-sentinel:%s=<value>", path, why, path)
	default:
		return v, true
	}
	return "", false
}

// cspMonitorIntervalViolations checks the timing values present in values.
func cspMonitorIntervalViolations(values map[string]any, cspName string, fail func(string, ...any)) {
	for _, interval := range cspMonitorIntervals {
		raw, present := component.GetValueByPath(values, interval.path)
		if !present {
			continue
		}
		n, whole := cspMonitorWholeNumber(raw)
		switch {
		case !whole:
			fail("%s is %#v, want a whole number below %d: the chart writes it into the monitor's config "+
				"unquoted, and the monitor refuses anything else at startup", interval.path, raw,
				int64(cspMonitorMaxWholeNumber))
		case interval.provider != "" && interval.provider != cspName:
		case n >= interval.min || (interval.zeroIsDefault && n == 0):
		default:
			fail("%s is %d, below the monitor's minimum of %d, so it refuses to start", interval.path, n,
				interval.min)
		}
	}
}

// cspMonitorWholeNumber returns raw as an integer when the chart would render
// it as one: Helm holds numbers as float64 and prints cspMonitorMaxWholeNumber
// and above in exponent form, which TOML reads as a float.
func cspMonitorWholeNumber(raw any) (int64, bool) {
	var v float64
	switch n := raw.(type) {
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	case float64:
		v = n
	default:
		return 0, false
	}
	if v != math.Trunc(v) || math.Abs(v) >= cspMonitorMaxWholeNumber {
		return 0, false
	}
	return int64(v), true
}

// cspMonitorAWSViolations checks the values the IRSA annotation and the AWS
// Health client are built from.
func cspMonitorAWSViolations(values map[string]any, clusterName string, fail func(string, ...any)) {
	raw, present := component.GetValueByPath(values, cspMonitorAWSAccountIDPath)
	accountID, isString := raw.(string)
	switch {
	case !present || raw == nil || (isString && accountID == ""):
		fail("%s is empty, so the ServiceAccount gets no IRSA role and the monitor cannot call the AWS Health "+
			"API -- --set-json 'nv-sentinel:%s=\"<12-digit account>\"'", cspMonitorAWSAccountIDPath,
			cspMonitorAWSAccountIDPath)
	case !isString:
		fail("%s is %#v, not a string: --set parses an all-digit value as a number, which drops a leading zero and "+
			"reaches the chart as a float, so the IRSA role ARN names the wrong account -- pass it as a string: "+
			"--set-json 'nv-sentinel:%s=\"<12-digit account>\"'", cspMonitorAWSAccountIDPath, raw,
			cspMonitorAWSAccountIDPath)
	case !awsAccountIDPattern.MatchString(accountID):
		fail("%s is %q, not a 12-digit AWS account ID", cspMonitorAWSAccountIDPath, accountID)
	}

	if region, ok := cspMonitorRequiredString(values, cspMonitorAWSRegionPath,
		"the monitor asks the AWS Health API only for events in this region", fail); ok &&
		!slices.Contains(cspMonitorAWSHealthRegions, region) {

		regions := strings.Join(cspMonitorAWSHealthRegions, " or ")
		fail("%s is %q, but the AWS Health API has endpoints only in %s (commercial partition), and NVSentinel uses "+
			"this one value both to reach the API and to filter events by region, so a cluster in any other region "+
			"cannot be monitored -- the aws mixin supports only clusters in %s", cspMonitorAWSRegionPath, region,
			regions, regions)
	}

	roleRaw, rolePresent := component.GetValueByPath(values, cspMonitorAWSRoleNamePath)
	roleName, roleIsString := roleRaw.(string)
	source := cspMonitorAWSRoleNamePath
	switch {
	case rolePresent && roleRaw != nil && !roleIsString:
		fail("%s is %v, not a string -- pass it as a string: --set-json 'nv-sentinel:%s=\"<role>\"'",
			cspMonitorAWSRoleNamePath, roleRaw, cspMonitorAWSRoleNamePath)
		return
	case roleName == "":
		if clusterName == "" {
			return
		}
		if strings.Contains(clusterName, "/") {
			fail("%s is %q: the chart builds the default IRSA role name from it, and a / there would be read as "+
				"an IAM path -- use the cluster's name, or set %s", cspMonitorClusterNamePath, clusterName,
				cspMonitorAWSRoleNamePath)
			return
		}
		roleName, source = clusterName+cspMonitorAWSDefaultRoleSuffix, cspMonitorClusterNamePath
	}
	name, validPath := roleName, true
	if i := strings.LastIndex(roleName, "/"); i >= 0 {
		path := roleName[:i]
		name = roleName[i+1:]
		validPath = len(path) <= cspMonitorAWSRolePathMaxLen && awsRolePathPattern.MatchString(path)
	}
	validName := len(name) <= cspMonitorAWSRoleNameMaxLen && awsRoleNamePattern.MatchString(name)
	if strings.HasPrefix(roleName, "arn:") {
		validPath = false
	}
	if !validName || !validPath {
		fail("the IRSA role name %q, from %s, is not a valid IAM role: its name, after the last /, must be 1 to %d "+
			"letters, digits and +=,.@_-, and a path before it printable ASCII with no leading /, at most %d "+
			"characters, so the ServiceAccount's role ARN names no role -- set %s to the role's path and name, "+
			"not its ARN: --set nv-sentinel:%s=<value>", roleName, source, cspMonitorAWSRoleNameMaxLen,
			cspMonitorAWSRolePathMaxLen, cspMonitorAWSRoleNamePath, cspMonitorAWSRoleNamePath)
	}
}

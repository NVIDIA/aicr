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
	"path"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Value paths CheckNVSentinelLogCollectorCoherent reads, under nvsentinel and
// the enabled GPU Operator, and the names its findings use.
const (
	logCollectorPath                = "fault-remediation.logCollector"
	logCollectorEnabledPath         = "fault-remediation.logCollector.enabled"
	logCollectorUploadURLPath       = "fault-remediation.logCollector.uploadURL"
	logCollectorNamespacesPath      = "fault-remediation.logCollector.gpuOperatorNamespaces"
	logCollectorMockModePath        = "fault-remediation.logCollector.env.MOCK_MODE"
	logCollectorEnvUploadURLPath    = "fault-remediation.logCollector.env.UPLOAD_URL_BASE"
	logCollectorEnvNamespacePath    = "fault-remediation.logCollector.env.GPU_OPERATOR_NAMESPACE"
	fileServerPath                  = "global.inclusterFileServer"
	fileServerEnabledPath           = "global.inclusterFileServer.enabled"
	fileServerNamePath              = "incluster-file-server.fullnameOverride"
	fileServerPortPath              = "incluster-file-server.service.port"
	logCollectorMixinName           = "nvsentinel-log-collector"
	logCollectorFileServerName      = "nvsentinel-incluster-file-server"
	gpuOperatorDriverEnabledPath    = "driver.enabled"
	gpuOperatorDriverInstallDirPath = "hostPaths.driverInstallDir"
	logCollectorDefaultOperatorNS   = "gpu-operator"
)

// CheckNVSentinelLogCollectorCoherent blocks a bundle in which NVSentinel's
// log collector cannot capture or upload anything. The collector and the
// in-cluster file server it uploads to are separate subcharts, and neither
// checks the other or what the collector needs from the node, so every case
// below deploys cleanly. It fails when:
//
//   - only one of fault-remediation.logCollector.enabled and
//     global.inclusterFileServer.enabled is on: the collector has nowhere to
//     upload, or the file server and its volume hold nothing;
//   - fault-remediation does not run, or runs in dry run or with a dryRun
//     that is not a boolean: fault-remediation launches the collector and
//     skips it in dry run, so it never captures. A logCollector.enabled that
//     is not a boolean is rejected too: the chart also renders it into
//     fault-remediation's --enable-log-collector flag, which parses strictly;
//   - the file server's name is not the mixin's fullnameOverride: unset, its
//     Service follows the Helm release name, which differs by deployer (Flux
//     names the release <namespace>-<name>); renamed, the nvsentinel health
//     check, which asserts the rollout by that name, passes a failed one. Or
//     the upload URL is not that Service;
//   - the collector cannot reach nvidia-bug-report. It runs the script in the
//     GPU Operator's driver container, which it looks up in the single
//     namespace gpuOperatorNamespaces names, or from GKE's managed driver
//     install, so a recipe whose node image carries the driver otherwise (AKS,
//     OKE, Kind, EKS on Amazon Linux) fails every collection.
//
// The Job template renders logCollector.env after the variables it sets from
// uploadURL and gpuOperatorNamespaces, so an env.UPLOAD_URL_BASE or
// env.GPU_OPERATOR_NAMESPACE entry replaces them; the rules read the entry when
// one is set, and an empty namespace as the collector's gpu-operator fallback.
//
// The collector's MOCK_MODE uploads a synthetic report without a driver,
// which the Kind e2e uses, so it skips the last rule. The collector enters it
// only on the exact string "true", so that is all this reads as mock.
//
// It is a no-op unless either setting is on, and rejects --dynamic on any path
// it verifies.
func CheckNVSentinelLogCollectorCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	provider := recipeResult.DataProvider()
	sentinelKeys := componentOverrideKeys(componentName, provider)
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys,
		"NVSentinel log collector")
	if err != nil {
		return nil, []error{err}
	}

	collectorRaw, _ := component.GetValueByPath(values, logCollectorEnabledPath)
	collector := helmTruthy(collectorRaw)
	fileServer := nvsentinelSubchartRenders(values, "inclusterFileServer")
	mockMode, _ := logCollectorEnvValue(values, logCollectorMockModePath)
	mock := mockMode == "true"
	gpuOp := enabledGPUOperator(recipeResult, bundlerConfig)

	if msgs := logCollectorDynamicViolations(bundlerConfig, componentName, sentinelKeys, collector, fileServer, mock,
		gpuOp); len(msgs) > 0 {
		for _, msg := range msgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return msgs, nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}
	if _, isBool := collectorRaw.(bool); collectorRaw != nil && !isBool {
		fail("%s is %v, not a boolean, and fault-remediation parses it strictly as --enable-log-collector -- "+
			"set it to true or false", logCollectorEnabledPath, collectorRaw)
		return nil, errs
	}
	if !collector && !fileServer {
		return nil, nil
	}

	if !collector {
		fail("%s is on without %s, so the file server and its volume hold nothing -- compose the %s mixin, "+
			"which turns on both", fileServerEnabledPath, logCollectorEnabledPath, logCollectorMixinName)
		return nil, errs
	}
	if !fileServer {
		fail("%s is on without %s, so every collection fails to upload and is lost -- compose the %s mixin, "+
			"which turns on both", logCollectorEnabledPath, fileServerEnabledPath, logCollectorMixinName)
	}

	remediation := nvsentinelSubchartRenders(values, "faultRemediation")
	dryRun, dryRunWellFormed := nvsentinelDryRun(values)
	switch {
	case !remediation:
		fail("%s is on but fault-remediation, which launches the collector, does not run -- compose %s with "+
			"nvsentinel-remediation", logCollectorEnabledPath, logCollectorMixinName)
	case !dryRunWellFormed:
		fail("%s is not a boolean, so whether fault-remediation runs the collector cannot be verified -- set it "+
			"to false", nvsentinelDryRunPath)
	case dryRun:
		fail("%s is on but fault-remediation runs in dry run (%s=true), which skips the collector -- compose %s "+
			"with nvsentinel-remediation, not nvsentinel-observe", logCollectorEnabledPath, nvsentinelDryRunPath,
			logCollectorMixinName)
	}

	if fileServer {
		name, _, _ := resolvedStringValue(values, fileServerNamePath)
		switch name {
		case "":
			fail("%s is unset, so the file server's Service is named after the Helm release, which differs by "+
				"deployer (Flux names it <namespace>-<name>), and %s cannot name it -- compose the %s mixin, which "+
				"pins it", fileServerNamePath, logCollectorUploadURLPath, logCollectorMixinName)
		case logCollectorFileServerName:
			host := fmt.Sprintf("%s.%s.svc.cluster.local", name, componentNamespace(recipeResult, componentName))
			// A null or 80 port is the chart's default, which the URL may omit.
			if port, ok := component.GetValueByPath(values, fileServerPortPath); ok && port != nil && fmt.Sprint(port) != "80" {
				host += ":" + fmt.Sprint(port)
			}
			want := "http://" + host + "/upload"
			urlPath := logCollectorUploadURLPath
			got, _, valid := resolvedStringValue(values, urlPath)
			if env, ok := logCollectorEnvValue(values, logCollectorEnvUploadURLPath); ok {
				urlPath, got, valid = logCollectorEnvUploadURLPath, env, true
			}
			if !valid || strings.Replace(strings.TrimSuffix(got, "/"), ".svc.cluster.local:80/", ".svc.cluster.local/", 1) != want {
				fail("%s is %q, not the in-cluster file server's Service, so collections upload elsewhere and "+
					"the file server holds nothing -- set it to %q", urlPath, got, want)
			}
		default:
			fail("%s is %q, but the nvsentinel health check asserts the file server's rollout as %q, so a failed "+
				"rollout passes validation -- leave it at %q, as the %s mixin sets it", fileServerNamePath, name,
				logCollectorFileServerName, logCollectorFileServerName, logCollectorMixinName)
		}
	}

	if !mock {
		errs = append(errs, logCollectorDriverViolations(ctx, recipeResult, bundlerConfig, componentName, values,
			gpuOp)...)
	}
	return nil, errs
}

// gpuOperatorRef names the first enabled GPU Operator variant in a recipe.
type gpuOperatorRef struct {
	name, namespace string
	keys            []string
}

// enabledGPUOperator returns the first enabled GPU Operator variant, or nil.
// OCP recipes carry a disabled gpu-operator beside the gpu-operator-ocp they
// deploy, so the first enabled one is the one that runs.
func enabledGPUOperator(recipeResult *recipe.RecipeResult, bundlerConfig *config.Config) *gpuOperatorRef {
	view := declaredUnionView(recipeResult)
	for _, name := range gpuOperatorComponentNames {
		ref := view.GetComponentRef(name)
		if ref == nil {
			continue
		}
		keys := componentOverrideKeys(name, recipeResult.DataProvider())
		if !componentDisabled(ref, bundlerConfig, keys) {
			return &gpuOperatorRef{name: name, namespace: componentNamespace(view, name), keys: keys}
		}
	}
	return nil
}

// logCollectorDriverViolations checks that the collector can run
// nvidia-bug-report: in the GPU Operator's driver container, which it finds by
// label in the namespace gpuOperatorNamespaces names (despite the name, the
// collector passes the whole value to kubectl -n), or from GKE's managed
// driver install, which it reads from the host.
func logCollectorDriverViolations(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, componentName string, values map[string]any, gpuOp *gpuOperatorRef) []error {
	unsupported := func(why string) []error {
		return []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: %s is on but %s, so every collection fails and no bug report is captured. "+
				"The collector runs nvidia-bug-report in the GPU Operator's driver container, or from GKE's "+
				"managed driver install -- let the GPU Operator install the driver (on AKS and OKE, "+
				"--profile gpuStack=operator-managed), or do not compose %s", componentName,
				logCollectorEnabledPath, why, logCollectorMixinName))}
	}
	if gpuOp == nil {
		return unsupported("the recipe deploys no GPU Operator")
	}
	gpuOpValues, err := effectiveComponentValues(ctx, declaredUnionView(recipeResult), bundlerConfig, gpuOp.name,
		gpuOp.keys, "NVSentinel log collector")
	if err != nil {
		return []error{err}
	}
	driverToggle, problem := ownershipToggle(gpuOpValues, "driver")
	if problem != "" {
		// CheckDriverOwnershipCoherence rejects the toggle itself.
		return nil
	}
	if driverToggle != nil && !*driverToggle {
		installDir := operatorContainerDriverRoot
		if dir, ok, _ := resolvedStringValue(gpuOpValues, gpuOperatorDriverInstallDirPath); ok && dir != "" {
			installDir = path.Clean(dir)
		}
		if installDir == gkeManagedDriverRootPath {
			return nil
		}
		return unsupported(fmt.Sprintf("%s leaves the driver to the node image (%s=false) and runs no driver "+
			"container", gpuOp.name, gpuOperatorDriverEnabledPath))
	}

	nsPath, namespace := logCollectorNamespacesPath, logCollectorDefaultOperatorNS
	ns, set, valid := resolvedStringValue(values, logCollectorNamespacesPath)
	if env, ok := logCollectorEnvValue(values, logCollectorEnvNamespacePath); ok {
		// The entry replaces gpuOperatorNamespaces outright, so an empty one
		// takes the collector's own fallback rather than that value.
		nsPath, ns, set, valid = logCollectorEnvNamespacePath, env, true, true
	}
	if !valid {
		// The chart quotes whatever it is given, so a list renders as the
		// literal "[gpu-operator]" and names no namespace.
		raw, _ := component.GetValueByPath(values, logCollectorNamespacesPath)
		return []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: %s is %v, not a string, so the collector searches a namespace that does "+
				"not exist and every collection fails -- set it to %q", componentName, logCollectorNamespacesPath,
				raw, gpuOp.namespace))}
	}
	if set && ns != "" {
		namespace = ns
	}
	if namespace != gpuOp.namespace {
		return []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: %s deploys to namespace %q, but %s is %q, the one namespace the collector "+
				"searches for the driver container, so every collection fails -- set it to %q",
				componentName, gpuOp.name, gpuOp.namespace, nsPath, namespace, gpuOp.namespace))}
	}
	return nil
}

// logCollectorEnvValue returns the logCollector.env entry at envPath as the
// Job template renders it: any scalar is quoted into a string, and null into
// the empty string.
func logCollectorEnvValue(values map[string]any, envPath string) (string, bool) {
	raw, ok := component.GetValueByPath(values, envPath)
	if !ok {
		return "", false
	}
	if raw == nil {
		return "", true
	}
	return fmt.Sprint(raw), true
}

// logCollectorDynamicViolations returns a violation for each --dynamic path a
// rule of CheckNVSentinelLogCollectorCoherent reads. The two toggles always
// matter; the rest only once either is on.
func logCollectorDynamicViolations(bundlerConfig *config.Config, componentName string, sentinelKeys []string, collector, fileServer, mock bool, gpuOp *gpuOperatorRef) []string {
	const reason = "decides whether NVSentinel's log collector runs, uploads to the in-cluster file server " +
		"and can reach the driver"
	guarded := []string{logCollectorEnabledPath, fileServerEnabledPath}
	if collector || fileServer {
		guarded = append(guarded, logCollectorPath, fileServerPath, fileServerNamePath, fileServerPortPath,
			nvsentinelFaultRemediationEnabledPath, nvsentinelDryRunPath)
	}
	msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, guarded, reason)
	if collector && !mock && gpuOp != nil {
		msgs = append(msgs, nvsentinelDynamicGuardViolations(bundlerConfig, gpuOp.name, gpuOp.keys,
			[]string{gpuOperatorDriverEnabledPath, gpuOperatorDriverInstallDirPath}, reason)...)
	}
	return msgs
}

// fileServerStorageClassPath names the file server's StorageClass; it is in
// nvsentinel's storageClassPaths, so --storage-class reaches it.
const fileServerStorageClassPath = "incluster-file-server.persistence.storageClassName"

// fileServerClaim is the file server's classless report volume.
var fileServerClaim = storageClassClaim{
	path:        fileServerStorageClassPath,
	overrideKey: "nv-sentinel",
	volumes:     "the file server's volume",
	pending: "the file server's volume, which names no StorageClass, would stay Pending, the file server " +
		"would not start, and every bug report upload would fail",
	purpose: "NVSentinel file server storage",
}

// CheckNVSentinelFileServerStorageClass applies the snapshot's default
// StorageClass evidence to the in-cluster file server's report volume, the
// rule CheckNVSentinelDatastorePrerequisites applies to the datastore's.
// Unless --storage-class or incluster-file-server.persistence.storageClassName
// names a class, it is a blocking error when the snapshot found no default
// StorageClass or could not tell, and a warning when the recipe carries no
// evidence or the cluster has more than one default. A no-op unless the file
// server renders with persistence on. It is registered apart from
// CheckNVSentinelLogCollectorCoherent because its warnings must stay
// non-blocking, which that gate's severity does not allow.
func CheckNVSentinelFileServerStorageClass(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
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
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, keys, fileServerClaim.purpose)
	if err != nil {
		return nil, []error{err}
	}
	if !nvsentinelSubchartRenders(values, "inclusterFileServer") {
		return nil, nil
	}
	// The chart renders the claim only under persistence.enabled, which
	// defaults to true.
	if enabled, set := component.GetValueByPath(values, "incluster-file-server.persistence.enabled"); set && !helmTruthy(enabled) {
		return nil, nil
	}
	warning, findingErr := storageClassFinding(ctx, componentName, recipeResult, bundlerConfig, keys, fileServerClaim)
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if findingErr != nil {
		return warnings, []error{findingErr}
	}
	return warnings, nil
}

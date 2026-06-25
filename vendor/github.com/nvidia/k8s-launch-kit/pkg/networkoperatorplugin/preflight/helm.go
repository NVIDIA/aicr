// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"errors"
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
)

// CheckHelmChartVersion compares the chart version reported by the live
// network-operator helm release against the version derived from the
// configured release line (Inputs.ExpectedChartVersion, no "v" prefix).
//
// Soft-skipped (Result.Skipped=true) when the expected version is empty
// (no release pinned), when no helm release exists in the operator
// namespace, or when the action.Configuration cannot be built. Other
// failures are surfaced via Result.Reason without panicking the caller.
func CheckHelmChartVersion(ctx context.Context, in Inputs) Result {
	_ = ctx
	r := Result{
		Name: "Helm chart version",
		Code: CodeHelmChartVersion,
	}
	if in.ExpectedChartVersion == "" {
		r.Skipped = true
		r.Reason = "no expected chart version (no --network-operator-release pinned)"
		return r
	}
	if in.RestConfig == nil {
		r.Skipped = true
		r.Reason = "no kube REST config available for helm chart-version check"
		return r
	}

	namespace := in.OperatorNamespace
	if namespace == "" {
		namespace = helmclient.DefaultNamespace
	}
	releaseName := in.HelmReleaseName
	if releaseName == "" {
		releaseName = helmclient.DefaultReleaseName
	}

	actionCfg, err := helmclient.NewActionConfig(in.RestConfig, namespace, helmclient.StorageDriver)
	if err != nil {
		r.Skipped = true
		r.Reason = fmt.Sprintf("helm action config init failed: %v", err)
		return r
	}
	rel, err := action.NewGet(actionCfg).Run(releaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			r.Skipped = true
			r.Reason = fmt.Sprintf("no helm release %q in namespace %s — run `l8k deploy` first", releaseName, namespace)
			return r
		}
		r.Skipped = true
		r.Reason = fmt.Sprintf("helm get release failed: %v", err)
		return r
	}
	if rel.Chart == nil || rel.Chart.Metadata == nil {
		r.Skipped = true
		r.Reason = "deployed helm release has no chart metadata"
		return r
	}
	deployedChartVersion := rel.Chart.Metadata.Version
	if deployedChartVersion == in.ExpectedChartVersion {
		r.Reason = fmt.Sprintf("chart version matches (%s)", in.ExpectedChartVersion)
		return r
	}
	r.Reason = fmt.Sprintf("chart version drift: release %q in %s", releaseName, namespace)
	r.Mismatches = []Mismatch{{
		Path:     "chart.version",
		Expected: in.ExpectedChartVersion,
		Actual:   deployedChartVersion,
		Detail:   fmt.Sprintf("selectedRelease=%s appVersion=%s", in.SelectedRelease, rel.Chart.Metadata.AppVersion),
	}}
	return r
}

// CheckHelmValues compares the deployed release's user-supplied values
// against the generated values.yaml. Same comparison logic
// (DeepEqualValues) the Phase 0 install/upgrade conflict gate uses.
//
// Soft-skipped when Inputs.GeneratedValuesYAML is empty (chart managed
// out of band) or no release exists in the target namespace. The
// comparison ignores merged chart defaults — only user-supplied values
// are diffed (action.GetValues with AllValues=false), matching what
// `l8k deploy` would actually re-install.
func CheckHelmValues(ctx context.Context, in Inputs) Result {
	_ = ctx
	r := Result{
		Name: "Helm values",
		Code: CodeHelmValues,
	}
	if len(in.GeneratedValuesYAML) == 0 {
		r.Skipped = true
		r.Reason = "no values.yaml in deployment dir — chart managed out of band"
		return r
	}
	if in.RestConfig == nil {
		r.Skipped = true
		r.Reason = "no kube REST config available for helm values check"
		return r
	}

	namespace := in.OperatorNamespace
	if namespace == "" {
		namespace = helmclient.DefaultNamespace
	}
	releaseName := in.HelmReleaseName
	if releaseName == "" {
		releaseName = helmclient.DefaultReleaseName
	}

	actionCfg, err := helmclient.NewActionConfig(in.RestConfig, namespace, helmclient.StorageDriver)
	if err != nil {
		r.Skipped = true
		r.Reason = fmt.Sprintf("helm action config init failed: %v", err)
		return r
	}
	getValues := action.NewGetValues(actionCfg)
	getValues.AllValues = false
	deployed, err := getValues.Run(releaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			r.Skipped = true
			r.Reason = fmt.Sprintf("no helm release %q in namespace %s — run `l8k deploy` first", releaseName, namespace)
			return r
		}
		r.Skipped = true
		r.Reason = fmt.Sprintf("helm get values failed: %v", err)
		return r
	}

	generated, err := helmclient.UnmarshalValues(in.GeneratedValuesYAML)
	if err != nil {
		r.Skipped = true
		r.Reason = fmt.Sprintf("parse generated values.yaml: %v", err)
		return r
	}
	diffs := DeepEqualValues(deployed, generated)
	if len(diffs) == 0 {
		r.Reason = fmt.Sprintf("helm values match (release %q in %s)", releaseName, namespace)
		return r
	}
	r.Reason = fmt.Sprintf("%d helm value(s) diverge from generated values.yaml", len(diffs))
	r.Mismatches = diffs
	return r
}

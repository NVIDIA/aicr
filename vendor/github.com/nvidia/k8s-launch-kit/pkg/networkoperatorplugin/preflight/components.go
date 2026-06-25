// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// CheckNCPComponentVersions cross-references the version-bearing fields in
// the live NicClusterPolicy + NicNodePolicy CRs against the expected
// component / DOCA versions supplied by the caller (looked up from the
// embedded release catalog). One Mismatch per divergent field.
//
// Soft-skipped when:
//   - Inputs.ExpectedComponentVersion is empty (no release pinned),
//   - the cluster has no NicClusterPolicy (nothing to compare against), or
//   - listing NicClusterPolicy fails.
//
// Missing sections are not flagged — a section that isn't rendered (e.g.
// nicConfigurationOperator disabled, no NNPs in 26.1) is simply skipped.
func CheckNCPComponentVersions(ctx context.Context, in Inputs) Result {
	r := Result{
		Name: "NicClusterPolicy / NicNodePolicy component versions",
		Code: CodeNCPComponentVersions,
	}
	if in.ExpectedComponentVersion == "" {
		r.Skipped = true
		r.Reason = "no expected componentVersion (no --network-operator-release pinned)"
		return r
	}
	if in.KubeClient == nil {
		r.Skipped = true
		r.Reason = "no kube client available for NCP/NNP version check"
		return r
	}

	ncpList := &unstructured.UnstructuredList{}
	ncpList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "mellanox.com",
		Version: "v1alpha1",
		Kind:    "NicClusterPolicyList",
	})
	if err := in.KubeClient.List(ctx, ncpList); err != nil {
		r.Skipped = true
		r.Reason = fmt.Sprintf("list NicClusterPolicy: %v", err)
		return r
	}
	if len(ncpList.Items) == 0 {
		r.Skipped = true
		r.Reason = "no NicClusterPolicy in cluster — run `l8k deploy` first"
		return r
	}

	ncpPaths := []componentVersionPath{
		{Path: []string{"spec", "nicConfigurationOperator", "operator", "version"}, Section: "nicConfigurationOperator.operator", Kind: "component"},
		{Path: []string{"spec", "nicConfigurationOperator", "configurationDaemon", "version"}, Section: "nicConfigurationOperator.configurationDaemon", Kind: "component"},
		{Path: []string{"spec", "nvIpam", "version"}, Section: "nvIpam", Kind: "component"},
		{Path: []string{"spec", "secondaryNetwork", "cniPlugins", "version"}, Section: "secondaryNetwork.cniPlugins", Kind: "component"},
		{Path: []string{"spec", "secondaryNetwork", "multus", "version"}, Section: "secondaryNetwork.multus", Kind: "component"},
		{Path: []string{"spec", "ofedDriver", "version"}, Section: "ofedDriver", Kind: "doca"},
	}
	for i := range ncpList.Items {
		ncp := &ncpList.Items[i]
		source := fmt.Sprintf("NicClusterPolicy/%s", ncp.GetName())
		for _, p := range ncpPaths {
			if m := compareVersionAtPath(ncp, source, p, in.ExpectedComponentVersion, in.ExpectedDOCAVersion); m != nil {
				r.Mismatches = append(r.Mismatches, *m)
			}
		}
	}

	// NicNodePolicy: zero or more (one per group in 26.4+, none in 26.1).
	nnpList := &unstructured.UnstructuredList{}
	nnpList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "mellanox.com",
		Version: "v1alpha1",
		Kind:    "NicNodePolicyList",
	})
	if err := in.KubeClient.List(ctx, nnpList); err != nil {
		// Tolerate the list error — the cluster might not have the
		// NicNodePolicy CRD at all (older release). Skip silently.
		log.Log.V(1).Info("List NicNodePolicy failed during component-version check", "error", err.Error())
	}
	nnpPaths := []componentVersionPath{
		{Path: []string{"spec", "ofedDriver", "version"}, Section: "ofedDriver", Kind: "doca"},
		{Path: []string{"spec", "rdmaSharedDevicePlugin", "version"}, Section: "rdmaSharedDevicePlugin", Kind: "component"},
		{Path: []string{"spec", "sriovDevicePlugin", "version"}, Section: "sriovDevicePlugin", Kind: "component"},
	}
	for i := range nnpList.Items {
		nnp := &nnpList.Items[i]
		source := fmt.Sprintf("NicNodePolicy/%s", nnp.GetName())
		for _, p := range nnpPaths {
			if m := compareVersionAtPath(nnp, source, p, in.ExpectedComponentVersion, in.ExpectedDOCAVersion); m != nil {
				r.Mismatches = append(r.Mismatches, *m)
			}
		}
	}

	if len(r.Mismatches) == 0 {
		r.Reason = "component versions match (expected component=" + in.ExpectedComponentVersion + ", doca=" + in.ExpectedDOCAVersion + ")"
	} else {
		r.Reason = fmt.Sprintf("%d component version(s) diverge from selectedRelease catalog", len(r.Mismatches))
	}
	return r
}

// componentVersionPath identifies one version-bearing section to inspect
// inside an NCP or NNP. Kind selects which expected value the comparison
// uses ("component" or "doca").
type componentVersionPath struct {
	Path    []string
	Section string
	Kind    string
}

// compareVersionAtPath reads obj.<path>.version and returns a Mismatch
// when the rendered value differs from expected. Returns nil when the
// path is missing (section not rendered) or when the value matches.
//
// Used by the NCP and NNP loops above to keep the per-Kind iteration
// declarative.
func compareVersionAtPath(obj *unstructured.Unstructured, source string, p componentVersionPath, expectedComponent, expectedDoca string) *Mismatch {
	actual, found, _ := unstructured.NestedString(obj.Object, p.Path...)
	if !found || actual == "" {
		return nil
	}
	expected := expectedComponent
	detail := "componentVersion"
	if p.Kind == "doca" {
		expected = expectedDoca
		detail = "docaDriver.version"
	}
	if actual == expected {
		return nil
	}
	return &Mismatch{
		Path:     fmt.Sprintf("%s.%s", source, p.Section),
		Expected: expected,
		Actual:   actual,
		Detail:   detail,
	}
}


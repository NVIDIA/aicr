// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RunAll runs every preflight check in a deterministic order and returns
// the slice of Result. Order matters for two reasons:
//
//  1. The aggregated structured error returned by the deploy gate lists
//     mismatches in the same order users see them in the HTML report.
//  2. The chart-version check ordering before the values check makes
//     "version drifted, values match" the first reason a user sees —
//     which is usually the more important signal.
//
// All checks are read-only. Use Remediate() to actually fix mismatches.
func RunAll(ctx context.Context, in Inputs) []Result {
	return []Result{
		CheckHelmChartVersion(ctx, in),
		CheckHelmValues(ctx, in),
		CheckNCPComponentVersions(ctx, in),
		CheckStrayCRs(ctx, in),
	}
}

// AnyFailed reports whether any non-skipped check in results found at
// least one mismatch. Used by deploy to decide whether to fail / remediate.
func AnyFailed(results []Result) bool {
	for _, r := range results {
		if r.Failed() {
			return true
		}
	}
	return false
}

// FailedCodes returns the Code of every check in results that found at
// least one mismatch. Used by structured JSON consumers; user-facing
// strings should prefer FailedNames so they don't see SHOUTY_SNAKE_CASE
// codes.
func FailedCodes(results []Result) []string {
	var out []string
	for _, r := range results {
		if r.Failed() {
			out = append(out, r.Code)
		}
	}
	return out
}

// FailedNames returns the human-readable Name of every check in results
// that found at least one mismatch. Used by deploy's aggregate error
// message and validate's verdict reasons so users see "Conflicting
// Network Operator resources" instead of "CONFLICTING_RESOURCES".
func FailedNames(results []Result) []string {
	var out []string
	for _, r := range results {
		if r.Failed() {
			out = append(out, r.Name)
		}
	}
	return out
}

// RemediationOptions toggles the side-effects of Remediate.
type RemediationOptions struct {
	// DryRun, when true, logs what would happen without making any
	// cluster mutations.
	DryRun bool
}

// Remediate applies the side-effects needed to converge the cluster to the
// expected state for each failed check. Only stray-CR deletion is performed
// here:
//
//   - HELM_CHART_VERSION / HELM_VALUES are remediated by the Phase 0 helm
//     install/upgrade path, not this function.
//   - NCP_COMPONENT_VERSIONS is remediated by the Phase 1 NCP/NNP apply
//     (SSA + ForceOwnership rewrites every field l8k owns).
//   - STRAY_CRS has no other natural remediation — apply phases don't touch
//     objects we don't render, so we delete each stray here.
//
// Returns the first deletion error encountered, or nil if every stray was
// deleted (or DryRun=true).
func Remediate(ctx context.Context, in Inputs, results []Result, opts RemediationOptions) error {
	if in.KubeClient == nil {
		return fmt.Errorf("remediate: no kube client supplied")
	}
	for _, r := range results {
		if r.Code != CodeStrayCRs || !r.Failed() {
			continue
		}
		for _, m := range r.Mismatches {
			ref, ok := parseStrayPath(m)
			if !ok {
				log.Log.V(1).Info("Remediate: skipping malformed stray mismatch", "path", m.Path)
				continue
			}
			if opts.DryRun {
				log.Log.Info("Remediate (dry-run): would delete stray CR",
					"gvk", ref.GVK, "namespace", ref.Namespace, "name", ref.Name)
				continue
			}
			if err := deleteOne(ctx, in.KubeClient, ref); err != nil {
				return fmt.Errorf("delete stray %s: %w", m.Path, err)
			}
			log.Log.Info("Remediate: deleted stray CR",
				"gvk", ref.GVK, "namespace", ref.Namespace, "name", ref.Name)
		}
	}
	return nil
}

// parseStrayPath reconstructs an ObjectRef from a Mismatch produced by
// CheckStrayCRs. The Mismatch.Detail field carries the GVK string and
// Path carries Kind/[Namespace/]Name — re-parsing both keeps the
// remediation independent of any private field on the Result.
func parseStrayPath(m Mismatch) (ObjectRef, bool) {
	// Detail is "<group>/<version>/<kind>; <scope>" — parse the GVK.
	// We can't trust Path's Kind alone because two managedKinds share
	// the same Kind name in different API groups (none today, but the
	// hardcoded list could grow into that).
	gvk, ok := parseGVKFromDetail(m.Detail)
	if !ok {
		return ObjectRef{}, false
	}
	parts := strings.SplitN(m.Path, "/", 3)
	switch len(parts) {
	case 2:
		// "Kind/Name" — cluster-scoped.
		return ObjectRef{GVK: gvk, Name: parts[1]}, true
	case 3:
		// "Kind/Namespace/Name" — namespaced.
		return ObjectRef{GVK: gvk, Namespace: parts[1], Name: parts[2]}, true
	default:
		return ObjectRef{}, false
	}
}

// parseGVKFromDetail extracts the GroupVersionKind from a Mismatch.Detail
// string of the form "<group>/<version>/<kind>; <scope>". Returns
// (gvk, true) on success.
func parseGVKFromDetail(detail string) (schema.GroupVersionKind, bool) {
	head := detail
	if i := strings.Index(detail, ";"); i >= 0 {
		head = detail[:i]
	}
	parts := strings.SplitN(strings.TrimSpace(head), "/", 3)
	if len(parts) != 3 {
		return schema.GroupVersionKind{}, false
	}
	return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}, true
}

// deleteOne issues a single Delete on the cluster. Background propagation
// (the default) — the operator's garbage-collector chains handle any
// downstream sweeps. NotFound is tolerated so a stray that was already
// deleted by a concurrent caller doesn't fail remediation.
func deleteOne(ctx context.Context, c client.Client, ref ObjectRef) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ref.GVK)
	obj.SetName(ref.Name)
	if ref.Namespace != "" {
		obj.SetNamespace(ref.Namespace)
	}
	if err := c.Delete(ctx, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		return err
	}
	return nil
}


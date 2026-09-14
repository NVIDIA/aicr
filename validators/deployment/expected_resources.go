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

package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/aicr/pkg/chainsaw"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	nodewrightCustomizationsComponent = "nodewright-customizations"
	// gcpDriverInstallerComponent is the values-gated GKE COS driver
	// installer (issue #1716); rendered only under gpuStack=bundle-installer.
	gcpDriverInstallerComponent = "gcp-driver-installer"
	draDriverComponent          = "nvidia-dra-driver-gpu"
	networkOperatorComponent    = "network-operator"

	// draKubeletPluginSuffix is the chart-template-defined name suffix for
	// the NVIDIA DRA driver's kubelet-plugin DaemonSet. The upstream chart
	// renders its DaemonSet name as "<fullname>-kubelet-plugin", where
	// "<fullname>" is controlled by chart values. Discovering by suffix is
	// deployer-neutral: it reads only a live Kubernetes object name shape,
	// makes no assumption about release identity or the deployer that
	// installed the chart.
	draKubeletPluginSuffix = "-kubelet-plugin"

	nodewrightCompleteState = "complete"

	// runtimeRequiredTaintKey / runtimeRequiredTaintValue identify the
	// workload-gate taint the nodewright (skyhook) operator manages for Skyhook
	// CRs with runtimeRequired: true (see tuning.yaml `runtimeRequired: true`).
	//
	// Why gate on this taint and not status.status: a GPU node joins carrying
	// this NoSchedule taint, and the operator removes it once *all*
	// runtime-required Skyhooks targeting that node are complete *on that node*
	// (per-node, not per-package). Unlike status.status — an aggregate over
	// (packages × matching nodes) that re-opens to in_progress on every package
	// reboot and each newly-joined node — the taint is applied once and removed
	// once as the monotone terminal step, so "taint absent" is a durable
	// "done, won't reboot again" signal rather than a probabilistic settling
	// heuristic (see issue #1775). Note the operator re-applies the taint across
	// reboots only when configured with REAPPLY_ON_REBOOT/reapplyOnReboot=true
	// (the gke-cos and bcm overlays); on those the taint flaps like the status
	// and the stability window rides through it, so gating on the taint is never
	// weaker than gating on the status.
	//
	// Values match the skyhook chart's default
	// controllerManager.manager.env.runtimeRequiredTaint
	// (skyhook.nvidia.com=runtime-required:NoSchedule), which AICR ships
	// unchanged and the UAT GPU node pools pre-taint with verbatim
	// (tests/uat/aws/cluster-config.yaml).
	runtimeRequiredTaintKey   = "skyhook.nvidia.com"
	runtimeRequiredTaintValue = "runtime-required"

	// nicClusterPolicyManifestMarker identifies a NicClusterPolicy manifest
	// (nic-cluster-policy-aks.yaml, nic-cluster-policy-oke-{gb200,l40s}.yaml
	// under recipes/components/network-operator/manifests/) among a
	// network-operator ComponentRef's ManifestFiles. Its presence means the
	// recipe stands up an RDMA fabric (an RDMA-shared or SR-IOV device plugin),
	// so a GPU node not yet advertising the fabric resource is "still
	// converging", not "no fabric". OCP wires a different component
	// (network-operator-ocp) and manifest name and so does not match; kind/talos
	// enable network-operator without this manifest and are likewise (correctly)
	// not gated. The polled resource name is derived per recipe from the matched
	// manifest itself (rdmaFabricResource), so the gate always waits for exactly
	// what this recipe's fabric advertises; the RDMA node label lives in
	// validators/helper (PCIMellanoxPresentLabel).
	nicClusterPolicyManifestMarker = "nic-cluster-policy"
)

var (
	nodewrightGVR = schema.GroupVersionResource{
		Group: "skyhook.nvidia.com", Version: "v1alpha1", Resource: "skyhooks",
	}

	// GPU readiness poll tunables shared by verifyNodewrightReady and
	// verifyDRAKubeletPluginReady. Package-level (not inline constants) so tests
	// can shrink them via TestMain — set once before any test runs and never
	// mutated after, so they stay race-free under t.Parallel. Production seeds
	// them from pkg/defaults.
	gpuReadinessPollInterval    = defaults.GPUReadinessPollInterval
	gpuReadinessStabilityWindow = defaults.GPUReadinessStabilityWindow
	gpuReadinessTimeout         = defaults.GPUReadinessTimeout
)

// pollUntilStable repeatedly calls probe until it reports healthy (nil error)
// continuously for gpuReadinessStabilityWindow, or the budget elapses. It
// absorbs the non-monotonic flaps a GPU-node reboot introduces (see pkg/defaults
// GPUReadiness*): a single unhealthy sample no longer fails the deployment
// phase. On timeout it returns an ErrCodeTimeout error wrapping the last
// unhealthy state so the gate log and operators see *why*; if the signal became
// healthy but never held it for the full window before the budget ran out, the
// ErrCodeTimeout reports the stability-window miss. onStable prints the success
// line(s).
//
// probe MUST be a single-pass, side-effect-free readiness check that returns
// nil when healthy. The parent check budget (ctx.Ctx) caps the poll even when
// gpuReadinessTimeout is larger, so the surrounding chainsaw asserts still run.
func pollUntilStable(ctx *validators.Context, label string, probe func() error, onStable func()) error {
	deadline := time.Now().Add(gpuReadinessTimeout)
	if ctxDeadline, ok := ctx.Ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	// timedOut classifies both exit paths as ErrCodeTimeout — deadline-expired
	// convergence is a timeout, not an internal failure. It preserves the last
	// observed unhealthy state (cause) so the gate log still shows *why*; when the
	// signal became healthy but never held it for the full window, cause is nil
	// and it reports the stability-window miss.
	timedOut := func(cause error) error {
		if cause == nil {
			return errors.New(errors.ErrCodeTimeout,
				fmt.Sprintf("%s became healthy but did not hold it for the %s stability window within %s (reboot still settling)",
					label, gpuReadinessStabilityWindow, gpuReadinessTimeout))
		}
		return errors.Wrap(errors.ErrCodeTimeout,
			fmt.Sprintf("%s not ready within %s", label, gpuReadinessTimeout), cause)
	}

	var stableSince time.Time
	var lastErr error
	for {
		lastErr = probe()
		if lastErr == nil {
			if stableSince.IsZero() {
				stableSince = time.Now()
			}
			if time.Since(stableSince) >= gpuReadinessStabilityWindow {
				if onStable != nil {
					onStable()
				}
				return nil
			}
		} else {
			// Any regression (a reboot re-opened the unhealthy state) restarts
			// the dwell.
			stableSince = time.Time{}
		}

		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Ctx.Done():
			// Parent check budget (not gpuReadinessTimeout) is the binding
			// constraint here. When the signal was healthy but hadn't yet held
			// the window, say so distinctly — timedOut(nil) would misattribute
			// it to the 8m poll budget in gate logs.
			if lastErr == nil {
				return errors.Wrap(errors.ErrCodeTimeout,
					fmt.Sprintf("%s became healthy but the parent check budget was exhausted before it held the %s stability window",
						label, gpuReadinessStabilityWindow), ctx.Ctx.Err())
			}
			return timedOut(lastErr)
		case <-time.After(gpuReadinessPollInterval):
		}
	}

	return timedOut(lastErr)
}

// checkExpectedResources verifies that all expected Kubernetes resources declared
// in the validation's componentRefs exist and are healthy in the live cluster.
func checkExpectedResources(ctx *validators.Context) error {
	if ctx.ValidationInput == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "validation is not available")
	}
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	var chainsawAsserts []chainsaw.ComponentAssert
	var failures []string
	// firstStructuredErr captures the first structured error surfaced by
	// chainsaw.Run results (e.g., ErrCodeInvalidRequest from
	// ValidateTestReadOnly when a registry assert violates the read-only
	// allowlist). Without this, the function would flatten such errors
	// into the generic ErrCodeNotFound "expected resource check failed"
	// summary at the bottom, losing the actionable classification. Per
	// PR #1235 review.
	var firstStructuredErr error
	enabledRefs := enabledComponentRefs(ctx.ValidationInput.ComponentRefs)

	failures = append(failures, verifyNamespacesActive(ctx, enabledRefs)...)

	// When both ExpectedResources and HealthCheckAsserts are populated on
	// the same ref, both paths execute. ExpectedResources is verified
	// here via helper.VerifyResource; HealthCheckAsserts is queued for
	// the chainsaw runner below. Output is source-tagged
	// [expectedResources] / [chainsaw] so operators can disambiguate
	// when both report on the same component. The previous
	// mutual-exclusion gate (`len(ExpectedResources) == 0`) was dropped
	// in #1220: the registry-declared assertFile is the deeper
	// readiness signal and should always run alongside the overlay-
	// declared resource list. The transitional hydration skip in
	// pkg/recipe (added in #1234) was reverted in lockstep.
	for _, ref := range enabledRefs {
		// Honor cancellation between components so a canceled run stops
		// before issuing more API calls — per repo CLAUDE.md "Always
		// check ctx.Done() in long-running operations and loops".
		select {
		case <-ctx.Ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout,
				"deployment validation canceled during expected-resources iteration",
				ctx.Ctx.Err())
		default:
		}
		if ref.HealthCheckAsserts != "" {
			// The registry-declared static assert cannot see value gates, so on a
			// component whose effective values suppress the Skyhook CR the assert
			// targets (e.g. tuningEnabled=false on a single-package tuning
			// manifest) it would fail on a deliberately-untuned cluster. Skip it
			// in that case, mirroring the render-aware Go readiness check. Only
			// nodewright-customizations is subject to this; a render/read error
			// propagates rather than silently skipping. See #1844.
			suppressed, reason, suppressErr := gatedHealthCheckSuppressed(ctx.Ctx, ref)
			if suppressErr != nil {
				return suppressErr
			}
			if suppressed {
				fmt.Printf("  [chainsaw] %s: skipped — %s\n", ref.Name, reason)
			} else {
				chainsawAsserts = append(chainsawAsserts, chainsaw.ComponentAssert{
					Name:       ref.Name,
					AssertYAML: ref.HealthCheckAsserts,
				})
			}
		}
		for _, er := range ref.ExpectedResources {
			if err := helper.VerifyResource(ctx.Ctx, ctx.Clientset, er); err != nil {
				failures = append(failures, fmt.Sprintf("[expectedResources] %s %s/%s (%s): %s",
					er.Kind, er.Namespace, er.Name, ref.Name, err.Error()))
			} else {
				fmt.Printf("  [expectedResources] %s %s/%s: healthy\n", er.Kind, er.Namespace, er.Name)
			}
		}
	}

	gpuFailures, gpuStructuredErr := verifyGPUReadinessSignals(ctx, enabledRefs)
	failures = append(failures, gpuFailures...)
	// firstStructuredErr is guaranteed nil here (the chainsaw block
	// below is the only other producer and hasn't run yet); we can
	// assign unconditionally. The chainsaw block downstream checks
	// firstStructuredErr == nil before its own assignment so the GPU
	// error wins when both produce one.
	if gpuStructuredErr != nil {
		firstStructuredErr = gpuStructuredErr
	}

	if len(chainsawAsserts) > 0 {
		// Bail out before paying chainsaw startup cost if the caller
		// already canceled. chainsaw.Run honors ctx mid-flight too,
		// but a short-circuit here skips fetcher construction and
		// log noise on a doomed run.
		select {
		case <-ctx.Ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout,
				"deployment validation canceled before chainsaw dispatch",
				ctx.Ctx.Err())
		default:
		}
		slog.Info("running health check assertions", "components", len(chainsawAsserts))
		fetcher, fetcherErr := buildResourceFetcher(ctx)
		if fetcherErr != nil {
			return fetcherErr
		}
		results := chainsaw.Run(ctx.Ctx, chainsawAsserts, defaults.ChainsawAssertTimeout, fetcher)
		for _, r := range results {
			if r.Passed {
				fmt.Printf("  [chainsaw] %s: health check passed\n", r.Component)
			} else {
				msg := fmt.Sprintf("[chainsaw] %s: health check failed", r.Component)
				if r.Output != "" {
					msg += fmt.Sprintf(":\n%s", r.Output)
				}
				if r.Error != nil {
					msg += fmt.Sprintf("\nerror: %v", r.Error)
					// Capture the first structured error so we can
					// preserve its code (e.g., ErrCodeInvalidRequest)
					// when returning to the catalog layer. Subsequent
					// structured errors still surface in the human-
					// readable failures list above.
					if firstStructuredErr == nil {
						if _, ok := stderrors.AsType[*errors.StructuredError](r.Error); ok {
							firstStructuredErr = r.Error
						}
					}
				}
				failures = append(failures, msg)
			}
		}
	}

	if len(failures) > 0 {
		fmt.Println("Failed resources:")
		for _, f := range failures {
			fmt.Printf("  %s\n", f)
		}
		// Prefer the first structured error (e.g.,
		// ErrCodeInvalidRequest from a registry assert that violated
		// the read-only allowlist) over the generic ErrCodeNotFound
		// summary so downstream catalog/CLI surfaces classify the
		// failure correctly. The human-readable failures list is still
		// printed above for operator visibility.
		if firstStructuredErr != nil {
			return firstStructuredErr
		}
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("expected resource check failed: %d issue(s):\n  %s",
				len(failures), strings.Join(failures, "\n  ")))
	}

	fmt.Println("All deployment resources and required readiness signals are healthy")
	return nil
}

func enabledComponentRefs(refs []recipe.ComponentRef) []recipe.ComponentRef {
	enabled := make([]recipe.ComponentRef, 0, len(refs))
	for _, ref := range refs {
		if ref.IsEnabled() {
			enabled = append(enabled, ref)
		}
	}
	return enabled
}

func verifyNamespacesActive(ctx *validators.Context, refs []recipe.ComponentRef) []string {
	var failures []string
	seen := make(map[string]bool, len(refs))

	for _, ref := range refs {
		if ref.Namespace == "" || seen[ref.Namespace] {
			continue
		}
		seen[ref.Namespace] = true

		verifyCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
		ns, err := ctx.Clientset.CoreV1().Namespaces().Get(verifyCtx, ref.Namespace, metav1.GetOptions{})
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("namespace %s: %v", ref.Namespace, err))
			continue
		}
		if ns.Status.Phase != corev1.NamespaceActive {
			failures = append(failures, fmt.Sprintf("namespace %s: phase=%s (want %s)", ref.Namespace, ns.Status.Phase, corev1.NamespaceActive))
			continue
		}

		fmt.Printf("  Namespace %s: Active\n", ref.Namespace)
	}

	return failures
}

// verifyGPUReadinessSignals runs the two Go-resident deep checks
// introduced by issue #611. Returns the human-readable failure strings
// plus the first *errors.StructuredError encountered across all checks
// so the caller can propagate the original error code (e.g.,
// ErrCodeInternal from a discovery/RBAC failure) instead of flattening
// it into the generic ErrCodeNotFound summary — per PR #1235 review.
//
// Migration disposition (per #1220 plan):
//
//   - clusterPolicyReady: removed (#1495). Now sole-sourced by the
//     Chainsaw `validate-cluster-policy-ready` check in
//     recipes/checks/gpu-operator/health-check.yaml, which polls the
//     same ClusterPolicy status.state for ~5m (vs the former one-shot
//     Go check that caused spurious failures on fresh gpu-operator installs).
//   - verifyNodewrightReady (formerly skyhookReady): stays in Go. Names
//     are derived from the recipe's own ManifestFiles at validate-time
//     (see expectedNodewrightNames), not from a stable label, so static
//     Chainsaw YAML cannot express the dynamic-name selector.
//   - verifyDRAKubeletPluginReady: stays in Go. The chart's full DaemonSet
//     name is release-derived; expressing the same check in Chainsaw
//     requires a chart-shape label upstream nvidia-dra-driver-gpu does
//     not currently apply. Encoding a release-derived full name would
//     violate the deployer-neutrality constraint (no
//     app.kubernetes.io/instance dependence — see #660 issue body).
func verifyGPUReadinessSignals(ctx *validators.Context, refs []recipe.ComponentRef) ([]string, error) {
	var failures []string
	var firstStructured error
	capture := func(err error) {
		if err == nil {
			return
		}
		failures = append(failures, err.Error())
		if firstStructured == nil {
			if _, ok := stderrors.AsType[*errors.StructuredError](err); ok {
				firstStructured = err
			}
		}
	}

	if ref, ok := findEnabledComponent(refs, nodewrightCustomizationsComponent); ok {
		capture(verifyNodewrightReady(ctx, ref))
	}

	if ref, ok := findEnabledComponent(refs, draDriverComponent); ok {
		capture(verifyDRAKubeletPluginReady(ctx, ref.Namespace))
	}

	if ref, ok := findEnabledComponent(refs, networkOperatorComponent); ok && recipeDeclaresRDMAFabric(ref) {
		// The polled resource is derived from the recipe's own NicClusterPolicy
		// manifest (rdma/hca_shared_devices_a on AKS, nvidia.com/mlnxnics on OKE)
		// so the gate waits for exactly what this recipe's fabric advertises. A
		// derivation failure fails the gate closed — never "skip the fabric".
		if fabricResource, ferr := rdmaFabricResource(ctx.Ctx, ref); ferr != nil {
			capture(ferr)
		} else {
			capture(verifyRDMAFabricReady(ctx, fabricResource))
		}
	}

	return failures, firstStructured
}

func findEnabledComponent(refs []recipe.ComponentRef, name string) (recipe.ComponentRef, bool) {
	for _, ref := range refs {
		if ref.Name == name {
			return ref, true
		}
	}
	return recipe.ComponentRef{}, false
}

// verifyNodewrightReady checks that the specific Nodewright CR(s) this recipe
// declares are present and have reached status.status == "complete".
//
// Deployer-neutrality stance: no Helm API calls, no reads of release
// metadata, no dependence on release-scoped labels. The set of Nodewright CRs
// to verify is derived from the recipe's own ComponentRef.ManifestFiles —
// the validator reads those manifests from the embedded data provider and
// extracts each Nodewright resource's metadata.name. At runtime it then looks
// those exact names up on the cluster via the Kubernetes API. Unrelated
// Nodewright CRs on the cluster (stale from previous deploys, or from other
// tenants) are explicitly ignored.
func verifyNodewrightReady(ctx *validators.Context, ref recipe.ComponentRef) error {
	expectedNames, err := expectedNodewrightNames(ref)
	if err != nil {
		return err
	}
	if len(expectedNames) == 0 {
		if len(ref.ManifestFiles) == 0 {
			// The recipe enabled nodewright-customizations but declared no
			// Nodewright manifests, so we cannot prove readiness. Fail closed
			// rather than silently pass — a genuine recipe misconfiguration the
			// user should see.
			return errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("no Nodewright CR names could be extracted from component %s manifestFiles=%v",
					ref.Name, ref.ManifestFiles))
		}
		// Manifests are declared but the effective values suppress every Skyhook
		// CR (e.g. tuningEnabled=false on a single-package tuning manifest, which
		// gates out the whole tuning CR). There is nothing to verify — the CR is
		// intentionally absent. This is NOT fail-open: expectedNodewrightNames
		// renders with the effective values, so any CR those values keep would
		// still be listed and asserted. See #1844.
		fmt.Printf("  Nodewright: all Skyhook CRs suppressed by effective values (manifestFiles=%v); nothing to verify\n",
			ref.ManifestFiles)
		return nil
	}

	// Discovery-gate the CRD before attempting Get by name: CRD not
	// registered → skip per #607; any other discovery error (RBAC, 5xx,
	// timeout) → fail closed so a transient discovery failure cannot mask
	// readiness.
	gv := nodewrightGVR.GroupVersion().String()
	_, discErr := ctx.Clientset.Discovery().ServerResourcesForGroupVersion(gv)
	switch {
	case discErr == nil:
		// fall through to per-CR checks
	case apierrors.IsNotFound(discErr):
		fmt.Printf("  Nodewright: %s not registered, skipping\n", gv)
		return nil
	default:
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to discover %s resources (is the API server reachable and RBAC in order?)", gv), discErr)
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	// Poll two signals until both hold continuously for the stability window, or
	// the budget elapses:
	//
	//  1. Every expected Skyhook CR reports status.status == "complete".
	//  2. No node still carries the runtime-required NoSchedule taint the
	//     operator removes as its monotone terminal step.
	//
	// status.status alone is non-monotonic during tuning: a reboot (or a
	// newly-joined GPU node) re-opens it to in_progress, and — worse — it can
	// momentarily read "complete" in the lull between two package reboots while
	// tuning is still in flight, which is exactly how the gate certified a
	// cluster ready and then had tuning re-open post-gate (issue #1775). Adding
	// the taint gate closes that hole: during such a lull the runtime-required
	// taint is still present, so the probe stays unhealthy until tuning is truly
	// done on every node. Polling rides through the reboot flaps rather than
	// failing the deployment phase on a transient in_progress / re-taint. See
	// pkg/defaults GPUReadiness* for sizing.
	return pollUntilStable(ctx,
		fmt.Sprintf("%d expected Nodewright(s) + runtime-required taint clearance", len(expectedNames)),
		func() error {
			// The Skyhook status Gets and the node-list taint scan are
			// independent read-only calls, so fan them out (per repo CLAUDE.md
			// "Sequential calls to N independent read-only K8s APIs → fan-out
			// with errgroup") rather than paying both round-trips serially every
			// poll iteration.
			var statusFailures, taintFailures []string
			var taintErr error
			g := new(errgroup.Group)
			g.Go(func() error {
				statusFailures = nodewrightStatusFailures(ctx, dynClient, expectedNames)
				return nil
			})
			g.Go(func() error {
				taintFailures, taintErr = runtimeRequiredTaintFailures(ctx)
				return nil
			})
			_ = g.Wait()

			if taintErr != nil {
				// A transient node-list failure (e.g. an apiserver hiccup while
				// a GPU node reboots) must not be read as "taint absent". Return
				// it so the poll resets the dwell and retries — fail closed.
				return taintErr
			}
			failures := make([]string, 0, len(statusFailures)+len(taintFailures))
			failures = append(failures, statusFailures...)
			failures = append(failures, taintFailures...)
			if len(failures) == 0 {
				return nil
			}
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%d Nodewright readiness signal(s) not settled:\n  %s",
					len(failures), strings.Join(failures, "\n  ")))
		},
		func() {
			for _, name := range expectedNames {
				fmt.Printf("  Nodewright %s: %s (stable ≥%s)\n", name, nodewrightCompleteState, gpuReadinessStabilityWindow)
			}
			fmt.Printf("  Nodewright runtime-required taint (%s=%s:%s): cleared from all nodes (stable ≥%s)\n",
				runtimeRequiredTaintKey, runtimeRequiredTaintValue, corev1.TaintEffectNoSchedule, gpuReadinessStabilityWindow)
		})
}

// nodewrightStatusFailures does one pass over the expected Skyhook CRs and
// returns a human-readable failure string for each that is missing, unreadable,
// or not yet status.status == "complete". An empty slice means all are complete.
//
// The per-name Gets are independent read-only calls, so they fan out
// concurrently (errgroup) and each keeps its own ResourceVerificationTimeout;
// results are written to a fixed-index slice to preserve deterministic order.
func nodewrightStatusFailures(ctx *validators.Context, dynClient dynamic.Interface, expectedNames []string) []string {
	results := make([]string, len(expectedNames))
	g, gctx := errgroup.WithContext(ctx.Ctx)
	for i, name := range expectedNames {
		g.Go(func() error {
			verifyCtx, cancel := context.WithTimeout(gctx, defaults.ResourceVerificationTimeout)
			defer cancel()
			results[i] = nodewrightStatusFailure(verifyCtx, dynClient, name)
			return nil
		})
	}
	// Goroutines never return an error (failures are recorded per-index), so Wait
	// only blocks until every Get completes.
	_ = g.Wait()

	failures := make([]string, 0, len(results))
	for _, r := range results {
		if r != "" {
			failures = append(failures, r)
		}
	}
	return failures
}

// nodewrightStatusFailure checks one Skyhook CR and returns a failure string, or
// "" when it is present and status.status == "complete".
func nodewrightStatusFailure(verifyCtx context.Context, dynClient dynamic.Interface, name string) string {
	sk, getErr := dynClient.Resource(nodewrightGVR).Get(verifyCtx, name, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return fmt.Sprintf("Nodewright %s: not found (recipe declared it but the cluster has no such CR)", name)
		}
		return fmt.Sprintf("Nodewright %s: failed to get: %v", name, getErr)
	}
	// Reject a CR that is on its way out even when it still reports complete.
	// Nodewright uses a deletion finalizer, so an expected Skyhook can sit
	// Terminating for a while with status.status untouched. Accepting it would
	// report readiness on the strength of state that is about to disappear —
	// the same false-PASS direction the Chainsaw executor already guards
	// against by skipping ghosts on positive assertions (#2041). The nameless
	// assert in the component health check cannot cover this: it is satisfied
	// by any live complete Skyhook, including a stale or unrelated one, so this
	// per-name check is the only gate that binds liveness to the CR the recipe
	// actually declared.
	if sk.GetDeletionTimestamp() != nil {
		return fmt.Sprintf("Nodewright %s: terminating (deletionTimestamp set)", name)
	}
	status, found, statusErr := unstructured.NestedString(sk.Object, "status", "status")
	if statusErr != nil {
		return fmt.Sprintf("Nodewright %s: failed to read status.status: %v", name, statusErr)
	}
	if !found {
		return fmt.Sprintf("Nodewright %s: missing status.status", name)
	}
	if status != nodewrightCompleteState {
		return fmt.Sprintf("Nodewright %s: status=%s (want %s)", name, status, nodewrightCompleteState)
	}
	return ""
}

// runtimeRequiredTaintFailures lists cluster nodes and returns a failure string
// for each that still carries the nodewright (skyhook) runtime-required
// NoSchedule taint — the durable "tuning not yet complete on this node" signal
// (see runtimeRequiredTaintKey). An empty slice means the taint is cleared from
// every node (or was never applied, e.g. a Skyhook without runtimeRequired:
// true), so this gate is a no-op when the recipe does not opt into the feature.
//
// A List error (transient apiserver failure, RBAC gap) is returned so the
// caller fails closed: "could not list nodes" must never be read as "taint
// absent". The error rides through the poll's dwell reset like any other
// unhealthy sample.
func runtimeRequiredTaintFailures(ctx *validators.Context) ([]string, error) {
	listCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()

	nodes, err := ctx.Clientset.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to list nodes for the nodewright runtime-required taint gate", err)
	}

	var failures []string
	for i := range nodes.Items {
		// Honor cancellation while walking a potentially large node list, per
		// repo CLAUDE.md "Always check ctx.Done() in long-running operations".
		select {
		case <-listCtx.Done():
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"canceled while scanning nodes for the nodewright runtime-required taint gate", listCtx.Err())
		default:
		}
		node := &nodes.Items[i]
		for j := range node.Spec.Taints {
			if isRuntimeRequiredTaint(&node.Spec.Taints[j]) {
				failures = append(failures, fmt.Sprintf(
					"node %s: still carries the runtime-required taint %s=%s:%s (nodewright tuning not complete on this node)",
					node.Name, runtimeRequiredTaintKey, runtimeRequiredTaintValue, corev1.TaintEffectNoSchedule))
				break
			}
		}
	}
	return failures, nil
}

// isRuntimeRequiredTaint reports whether t is the nodewright (skyhook)
// runtime-required workload-gate taint. It matches on key+value and requires the
// NoSchedule effect so an unrelated taint that happens to share the key cannot
// mask an in-flight tuning.
func isRuntimeRequiredTaint(t *corev1.Taint) bool {
	return t.Key == runtimeRequiredTaintKey &&
		t.Value == runtimeRequiredTaintValue &&
		t.Effect == corev1.TaintEffectNoSchedule
}

// gatedHealthCheckSuppressed dispatches the render-aware static-assert
// suppression for the small set of values-gated components whose registry
// health check targets objects the effective values may legitimately
// suppress. Every other component's assert queues unconditionally.
// Fail-closed throughout: a render or read error propagates so a broken
// template is never mistaken for "nothing to assert".
func gatedHealthCheckSuppressed(goCtx context.Context, ref recipe.ComponentRef) (bool, string, error) {
	switch ref.Name {
	case nodewrightCustomizationsComponent:
		//nolint:contextcheck // pre-existing ctx-less chain (expectedNodewrightNames); threading ctx through it is tracked separately from this dispatch.
		suppressed, err := nodewrightHealthCheckSuppressed(ref)
		return suppressed, "effective values suppress the tuning Skyhook CR (see #1844)", err
	case gcpDriverInstallerComponent:
		suppressed, err := emptyRenderHealthCheckSuppressed(goCtx, ref)
		return suppressed, "effective values gate the component off (installer.enabled=false); it renders no objects", err
	default:
		return false, "", nil
	}
}

// emptyRenderHealthCheckSuppressed reports whether the component's manifests
// render zero Kubernetes objects under its effective values — the shape of a
// values-gated component (ADR-015: the component set is constant across
// profile values; a non-selected value renders an empty release). A static
// health-check assert cannot see value gates and would fail on a healthy
// cluster where the render is deliberately empty.
func emptyRenderHealthCheckSuppressed(goCtx context.Context, ref recipe.ComponentRef) (bool, error) {
	if len(ref.ManifestFiles) == 0 {
		// Nothing to render — leave the assert in place so its own failure
		// surfaces the problem.
		return false, nil
	}
	values, err := recipe.GetComponentValuesWithContext(goCtx, nil, &ref)
	if err != nil {
		return false, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to resolve effective values for component %s", ref.Name), err)
	}
	chartName := ref.Chart
	if chartName == "" {
		chartName = ref.Name
	}
	renderInput := manifest.RenderInput{
		ComponentName: ref.Name,
		Namespace:     ref.Namespace,
		ChartName:     chartName,
		ChartVersion:  ref.Version,
		Values:        values,
	}
	for _, path := range ref.ManifestFiles {
		// Preserve the validator cancellation contract: reads and renders in
		// this loop must stop once the deployment phase is canceled.
		select {
		case <-goCtx.Done():
			return false, errors.Wrap(errors.ErrCodeTimeout,
				"deployment validation canceled during gated health-check evaluation", goCtx.Err())
		default:
		}
		content, err := recipe.GetManifestContentWithContext(goCtx, nil, path)
		if err != nil {
			return false, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to load manifest %s for component %s", path, ref.Name), err)
		}
		rendered, rerr := manifest.Render(content, renderInput)
		if rerr != nil {
			// Fail closed: a render error must not be read as "renders nothing".
			return false, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to render manifest %s for component %s with effective values", path, ref.Name), rerr)
		}
		if renderedYAMLHasObjects(string(rendered)) {
			return false, nil
		}
	}
	return true, nil
}

// renderedYAMLHasObjects reports whether rendered YAML contains at least one
// non-empty document (comment-only and whitespace-only documents count as
// empty), mirroring the bundler's empty-release detection.
func renderedYAMLHasObjects(rendered string) bool {
	for _, doc := range strings.Split(rendered, "\n---") {
		for _, line := range strings.Split(doc, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && trimmed != "---" {
				return true
			}
		}
	}
	return false
}

// nodewrightHealthCheckSuppressed reports whether the registry-declared static
// health-check assert for the nodewright-customizations component targets a
// Skyhook CR that the component's effective values gate off. The static assert
// (recipes/checks/nodewright-customizations/health-check.yaml) asserts the
// tuning Skyhook reaches status.status: complete and cannot see value gates, so
// it must be skipped when the CR is intentionally absent — otherwise it fails on
// a deliberately-untuned cluster (issue #1844).
//
// It reuses the same render-aware extraction the Go readiness check uses: with
// manifests declared but a zero rendered CR set, every Skyhook CR is gated off.
// Only the nodewright-customizations component is subject to this; every other
// component's assert queues unconditionally. Fail-closed: a render error
// propagates so a broken template is never mistaken for "nothing to assert".
func nodewrightHealthCheckSuppressed(ref recipe.ComponentRef) (bool, error) {
	if ref.Name != nodewrightCustomizationsComponent {
		return false, nil
	}
	if len(ref.ManifestFiles) == 0 {
		// No manifests to render — leave the assert in place so its own failure
		// (or the Go check's misconfiguration error) surfaces the problem.
		return false, nil
	}
	names, err := expectedNodewrightNames(ref)
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}

// expectedNodewrightNames derives the set of Nodewright CR names that this
// component is expected to deploy, by reading each ManifestFile through the
// recipe data provider, rendering it with the component's effective Helm
// values, and extracting the metadata.name of every Nodewright resource in the
// rendered output.
//
// Rendering (not raw-template scanning) is what makes the check value-aware: a
// CR that the effective values gate off — e.g. tuningEnabled=false suppressing
// the whole tuning Skyhook on a single-package tuning manifest
// (tuning-gke.yaml / tuning-generic.yaml) — drops out of the render and is
// therefore not asserted on a deliberately-untuned cluster (issue #1844). This
// stays fail-closed: the render reflects the effective values, so any CR those
// values keep still appears here and is verified. A CR that is *expected* but
// missing on the cluster still fails — only a CR the values deliberately
// suppress is tolerated.
//
// Values are resolved from the ref alone (base values.yaml → ValuesFile →
// inline Overrides) via the embedded data provider, mirroring how the bundler
// renders these same manifests. Manifest reads use the package-global provider,
// matching the pre-existing behavior of this check.
func expectedNodewrightNames(ref recipe.ComponentRef) ([]string, error) {
	values, err := recipe.GetComponentValues(&ref)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to resolve effective values for component %s", ref.Name), err)
	}

	chartName := ref.Chart
	if chartName == "" {
		chartName = ref.Name
	}
	renderInput := manifest.RenderInput{
		ComponentName: ref.Name,
		Namespace:     ref.Namespace,
		ChartName:     chartName,
		ChartVersion:  ref.Version,
		Values:        values,
	}

	seen := make(map[string]bool)
	var names []string
	for _, path := range ref.ManifestFiles {
		content, err := recipe.GetManifestContent(path)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to load manifest %s for component %s", path, ref.Name), err)
		}
		rendered, rerr := manifest.Render(content, renderInput)
		if rerr != nil {
			// Fail closed: a render error must not be read as "no CRs to verify".
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to render manifest %s for component %s with effective values", path, ref.Name), rerr)
		}
		for _, name := range extractNodewrightNamesFromManifest(rendered) {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// nodewrightKindRE and nodewrightMetadataNameRE are narrow extractors for Nodewright
// CR names out of a manifest that has been Helm-rendered by expectedNodewrightNames
// (so value-gated CRs are already absent). Rendered output is concrete YAML, but
// these line-oriented patterns are retained over a full YAML parse because the
// rendered documents can still carry Helm-hook annotations and blank optional
// blocks, and the templated-name guard below stays as defense in depth in case a
// name ever fails to render to a literal.
//
// These patterns make three chart-shape assumptions that hold across every
// manifest AICR ships today (tuning, no-op, tuning-gke in
// recipes/components/nodewright-customizations/manifests/):
//   - "kind: Skyhook" sits at column 0.
//   - The metadata.name of each Nodewright is a literal string (not templated)
//     at exactly 2-space indent under a top-level "metadata:" block.
//   - Document separators use a bare "---" on its own line.
//
// If those shapes change, the helper's direct unit tests fail loudly.
var (
	nodewrightKindRE         = regexp.MustCompile(`(?m)^kind:\s*Skyhook\s*$`)
	nodewrightDocSeparatorRE = regexp.MustCompile(`(?m)^---\s*$`)
	nodewrightMetadataNameRE = regexp.MustCompile(`(?m)^  name:\s+(\S+)\s*$`)
)

// extractNodewrightNamesFromManifest returns the metadata.name of every Nodewright
// CR declared in a (possibly Helm-templated) manifest file. Names that are
// themselves templated (e.g. "{{ .Chart.Name }}") are skipped — the
// validator cannot evaluate them, and a templated name is never what a
// concrete AICR recipe declares today.
func extractNodewrightNamesFromManifest(content []byte) []string {
	var names []string
	for _, doc := range nodewrightDocSeparatorRE.Split(string(content), -1) {
		if !nodewrightKindRE.MatchString(doc) {
			continue
		}
		m := nodewrightMetadataNameRE.FindStringSubmatch(doc)
		if m == nil {
			continue
		}
		name := strings.Trim(m[1], `"'`)
		if strings.Contains(name, "{{") {
			continue
		}
		names = append(names, name)
	}
	return names
}

// verifyDRAKubeletPluginReady locates the kubelet-plugin DaemonSet by
// Kubernetes object shape — not by Helm release identity — and gates on pod
// readiness.
//
// Deployer-neutrality stance: no Helm API calls, no reads of release
// metadata, no dependence on release-scoped labels like
// app.kubernetes.io/instance. The check lists DaemonSets in the component's
// namespace and selects the one whose name ends in the chart's hard-coded
// role suffix "-kubelet-plugin". This is a *chart-shape* assumption (the
// upstream nvidia-dra-driver-gpu chart names that DaemonSet
// "<fullname>-kubelet-plugin" regardless of how fullname resolves), not a
// deployer assumption. If the upstream chart ever renames the component,
// this constant moves with it.
func verifyDRAKubeletPluginReady(ctx *validators.Context, namespace string) error {
	// Upfront structural gate (mirrors verifyNodewrightReady's CRD discovery
	// gate): fail fast on an AMBIGUOUS suffix match. More than one DaemonSet
	// carrying the "-kubelet-plugin" role suffix is a deterministic
	// misconfiguration (a stale DaemonSet from a prior deploy under a different
	// fullname, or two charts) that retrying for the full poll budget cannot
	// resolve — so surface it immediately instead of after GPUReadinessTimeout.
	// Zero-match and not-yet-ready status stay in the polled path below: the
	// DaemonSet's pods churn to 0/0 across a GPU-node reboot, which the dwell is
	// there to ride through.
	matches, _, err := listDRAKubeletPluginDaemonSets(ctx, namespace)
	if err != nil {
		return err
	}
	if len(matches) > 1 {
		return ambiguousDRAKubeletPluginError(namespace, matches)
	}

	// Poll until the kubelet-plugin DaemonSet is fully rolled out continuously
	// for the stability window, or the budget elapses. See pkg/defaults
	// GPUReadiness* for sizing.
	var healthyName string
	return pollUntilStable(ctx,
		fmt.Sprintf("DRA kubelet-plugin DaemonSet in namespace %s", namespace),
		func() error {
			name, probeErr := draKubeletPluginProbe(ctx, namespace)
			healthyName = name
			return probeErr
		},
		func() {
			fmt.Printf("  DaemonSet %s/%s: healthy (stable ≥%s)\n", namespace, healthyName, gpuReadinessStabilityWindow)
		})
}

// listDRAKubeletPluginDaemonSets lists DaemonSets in the namespace and returns
// those whose name carries the chart's "-kubelet-plugin" role suffix, plus the
// names of every DaemonSet seen (for the not-found diagnostic).
func listDRAKubeletPluginDaemonSets(ctx *validators.Context, namespace string) ([]appsv1.DaemonSet, []string, error) {
	verifyCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()

	dsList, err := ctx.Clientset.AppsV1().DaemonSets(namespace).List(verifyCtx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to list DaemonSets in namespace %s", namespace), err)
	}

	var matches []appsv1.DaemonSet
	var seenNames []string
	for _, ds := range dsList.Items {
		seenNames = append(seenNames, ds.Name)
		if strings.HasSuffix(ds.Name, draKubeletPluginSuffix) {
			matches = append(matches, ds)
		}
	}
	return matches, seenNames, nil
}

// ambiguousDRAKubeletPluginError reports more than one DaemonSet matching the
// kubelet-plugin role suffix — a deterministic misconfiguration, not a transient.
func ambiguousDRAKubeletPluginError(namespace string, matches []appsv1.DaemonSet) error {
	matchedNames := make([]string, 0, len(matches))
	for _, ds := range matches {
		matchedNames = append(matchedNames, ds.Name)
	}
	return errors.New(errors.ErrCodeInternal,
		fmt.Sprintf("ambiguous: %d DaemonSets in namespace %s match kubelet-plugin role suffix %q: %s",
			len(matches), namespace, draKubeletPluginSuffix, formatNames(matchedNames)))
}

// draKubeletPluginProbe does one readiness pass: it locates the kubelet-plugin
// DaemonSet by name suffix and reports nil (plus the DaemonSet name) when it is
// fully rolled out, or an error describing the unhealthy/missing state. The
// ambiguous (>1 match) case is caught fail-fast upstream in
// verifyDRAKubeletPluginReady; the guard here only fires if a second matching
// DaemonSet appears mid-poll.
func draKubeletPluginProbe(ctx *validators.Context, namespace string) (string, error) {
	matches, seenNames, err := listDRAKubeletPluginDaemonSets(ctx, namespace)
	if err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no kubelet-plugin DaemonSet (name suffix %q) found in namespace %s (DaemonSets in namespace: %s)",
				draKubeletPluginSuffix, namespace, formatNames(seenNames)))
	case 1:
		// proceed
	default:
		return "", ambiguousDRAKubeletPluginError(namespace, matches)
	}

	ds := matches[0]
	if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady == 0 {
		return ds.Name, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DaemonSet %s/%s: no ready kubelet-plugin pods scheduled (%d/%d pods ready)",
				namespace, ds.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}
	if ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
		return ds.Name, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DaemonSet %s/%s: not healthy: %d/%d pods ready",
				namespace, ds.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}

	return ds.Name, nil
}

// recipeDeclaresRDMAFabric reports whether a network-operator ComponentRef
// stands up an RDMA fabric on this cluster — i.e. it declares a
// NicClusterPolicy manifest that creates an RDMA device plugin
// (nicClusterPolicyManifestMarker). When true, a GPU node that does not yet
// advertise the manifest-derived fabric resource is "still converging", so
// verifyRDMAFabricReady waits for it; when false (kind's single-node nvkind,
// talos' namespace-only ref) there is no shared fabric to gate on and the
// check is skipped.
//
// Sibling predicate: pkg/bundler/readiness.go's
// recipeAttachesNicClusterPolicy encodes the same "does this recipe stand
// up an NCP?" question for the bundler's readiness-gate emission, but
// scans manifest content across every ComponentRef's Pre+ManifestFiles
// via line-anchored regexes rather than a filename-substring check on a
// single ref. Package layering blocks direct reuse, so the two functions
// have deliberately different names and must be kept in sync when a
// future overlay changes how an NCP is attached (a new marker filename,
// an attachment via PreManifestFiles, a differently-scoped ref). Update
// both — and their cross-reference comments — together.
func recipeDeclaresRDMAFabric(ref recipe.ComponentRef) bool {
	for _, f := range ref.ManifestFiles {
		if strings.Contains(f, nicClusterPolicyManifestMarker) {
			return true
		}
	}
	return false
}

// verifyRDMAFabricReady blocks the deployment gate until the network operator's
// shared RDMA device (fabricResource, derived from the recipe's own
// NicClusterPolicy manifest by rdmaFabricResource) is allocatable in a uniform,
// positive count across every Mellanox RDMA-capable GPU node, held continuously for the
// stability window.
//
// Why span the whole RDMA cohort rather than one node: NCCL all-reduce and any
// other all-to-all fabric test participate every node together, so a single node
// whose MOFED / rdma-shared-device-plugin has not finished rolling out degrades
// the whole run. The network operator rolls MOFED out per node and one node can
// lag another by many minutes (issue #1862), during which the shared resource is
// present on only a subset of nodes. Gating on uniform allocatable presence stops
// the downstream NCCL check's uniformFabricResourceCount from failing closed on a
// transient, self-healing partial rollout, mirroring the DRA kubelet-plugin and
// Nodewright "stable ≥window" treatment above.
//
// Why *this* node set: the gate validates schedulable GPU nodes that carry the
// NicClusterPolicy's own nodeAffinity label (helper.PCIMellanoxPresentLabel) —
// exactly the cohort the fabric can land on and the NCCL check runs on. A GPU
// node in a non-RDMA (non-Mellanox) pool never advertises the resource; including
// it would wedge the gate on a node the workload excludes.
//
// Cordoned RDMA-capable nodes: like check-nvidia-smi (#1668/#1936), a cordoned
// Mellanox RDMA GPU node is excluded from the *validated* cohort (the NCCL
// workload will not land on it) but is NOT silently dropped. It is enumerated via
// helper.FindGpuNodes, disclosed explicitly as "skipped (cordoned)" in stdout,
// counted in nodesTotal, and the coverage is emitted through validators.EmitExtra
// so it survives the default redaction policy into the signed bundle (#1951/#1952) —
// a cordoned node narrowing the fabric cohort can no longer hide behind a
// stdout-only line the publisher strips.
func verifyRDMAFabricReady(ctx *validators.Context, fabricResource string) error {
	// Production emit seam: publish the structured coverage as an EmitExtra
	// sentinel. verifyRDMAFabricReadyEmit injects it so tests can record the eager
	// floor and terminal disclosures without capturing the EmitExtra stdout
	// transport (which lives in the validators package).
	return verifyRDMAFabricReadyEmit(ctx, fabricResource, func(validated, total int) {
		emitExtraOrWarn(rdmaFabricCoverageExtra(validated, total))
	})
}

// verifyRDMAFabricReadyEmit is verifyRDMAFabricReady with the structured
// coverage emit injected. See verifyRDMAFabricReady for the gate contract.
func verifyRDMAFabricReadyEmit(ctx *validators.Context, fabricResource string, emitCoverage func(validated, total int)) error {
	var coverage rdmaFabricCoverage
	// emittedEarly gates the eager disclosure floor to exactly one emit.
	var emittedEarly bool
	// onStable is nil: the success line and the *terminal* coverage disclosure
	// are printed once at the single seam below (rdmaFabricProbeCoverage runs every
	// poll iteration, so emitting the human enumeration there would repeat it on
	// each tick — the settled disclosure must land exactly once, at the final
	// outcome).
	err := pollUntilStable(ctx,
		fmt.Sprintf("RDMA shared-device fabric (%s) across RDMA GPU nodes", fabricResource),
		func() error {
			cov, probeErr := rdmaFabricProbeCoverage(ctx, fabricResource)
			coverage = cov
			// Eager disclosure floor: emit the structured coverage once, on the
			// first observation that actually enumerated an RDMA-candidate node,
			// so a cordoned node narrowing the cohort survives even if the Job's
			// activeDeadlineSeconds SIGKILLs the process mid-poll before the
			// terminal emit runs. The catalog timeout feeds both the Job deadline
			// and this poll budget with no margin (pkg/validator/v1/job_plan.go),
			// so an exhausted never-ready poll (every RDMA node cordoned for
			// maintenance, or a rollout slower than the budget) can be killed at
			// the deadline with no terminal emit. parseExtraSentinels keeps the
			// LAST valid sentinel, so a clean exit's terminal emit wins and a
			// deadline kill leaves this floor as the disclosure of record.
			// validated=0: nothing is certified mid-poll. Only the structured
			// Extra is emitted eagerly (not the stdout enumeration) — the Extra is
			// the piece that survives redaction into the signed bundle (#1951/
			// #1952), and duplicating stdout would spam divergent counts. The
			// broader no-margin kill race predates this gate and is tracked
			// separately; this closes only the gate's own every-terminal-outcome
			// coverage contract.
			if !emittedEarly && cov.total() > 0 {
				emittedEarly = true
				emitCoverage(0, cov.total())
			}
			return probeErr
		},
		nil)

	// Single terminal disclosure — printed/emitted exactly once after the poll
	// settles, on BOTH the ready and the fail-closed path, reflecting the final
	// observation. validated is the schedulable cohort size only when the gate
	// certified it uniform+ready; a fail-closed exit (transient List error, no
	// cohort observed, partial rollout, skew, or timeout) reports 0 validated so
	// a narrowed-scope failure is never conflated with a full pass.
	validated := 0
	if err == nil {
		validated = coverage.schedulable
	}
	printLines(coverage.enumerationLines()...)
	printLines(coverage.coverageLine(validated))
	// nodesValidated/nodesTotal are reused verbatim from the existing
	// ctrfExtraAllowlist (see pkg/evidence/redact): their semantics fit exactly —
	// validated = schedulable RDMA nodes with uniform allocatable fabric, total =
	// all RDMA-candidate nodes incl cordoned. No new key or skipReason enum is
	// minted (the RDMA gate never "skips" — it fails closed), so the redaction
	// PolicyVersion stays v2.
	emitCoverage(validated, coverage.total())

	if err == nil {
		fmt.Printf("  RDMA fabric (%s): allocatable (uniform) on all %d schedulable RDMA GPU node(s) (stable ≥%s)\n",
			fabricResource, coverage.schedulable, gpuReadinessStabilityWindow)
	}
	return err
}

// rdmaFabricCoverage partitions the Mellanox RDMA-capable GPU nodes the fabric
// gate discloses: the schedulable nodes it actually validates and the cordoned
// RDMA-capable nodes it must reveal (never silently omit from the total). It
// exists so the disclosure text and the coverage counts are a pure, independently
// testable function of the partition rather than interleaved fmt.Printf calls —
// the #1668/#1936 node-scope disclosure pattern applied to the RDMA gate (#1952).
type rdmaFabricCoverage struct {
	schedulable int      // schedulable Mellanox RDMA-capable GPU nodes in the gated cohort
	cordoned    []string // cordoned Mellanox RDMA-capable GPU nodes: excluded from the cohort but disclosed
}

// total is every RDMA-candidate node the gate saw — the schedulable cohort plus
// the cordoned nodes it excluded but must still count (nodesTotal).
func (c rdmaFabricCoverage) total() int { return c.schedulable + len(c.cordoned) }

// enumerationLines renders the RDMA-candidate listing: the total/schedulable/
// cordoned counts, and each cordoned node explicitly marked "skipped (cordoned)"
// rather than omitted from the total. Node names appear ONLY here (stdout),
// never in the structured Extra.
func (c rdmaFabricCoverage) enumerationLines() []string {
	total := c.total()
	if total == 0 {
		return []string{"Found 0 Mellanox RDMA-capable GPU node(s)."}
	}
	lines := make([]string, 0, 1+len(c.cordoned))
	lines = append(lines, fmt.Sprintf(
		"Found %d Mellanox RDMA-capable GPU node(s), %d schedulable, %d cordoned:",
		total, c.schedulable, len(c.cordoned)))
	for _, name := range c.cordoned {
		lines = append(lines, fmt.Sprintf("  %s: skipped (cordoned)", name))
	}
	return lines
}

// coverageLine renders the nodesValidated disclosure for the RDMA gate. The
// "RESULT: " prefix is the validator runtime's convention (pkg/validator/
// validator.go resultSummaryPrefix) for echoing a stdout line into live CLI
// output; it is not guaranteed to survive redaction, which is why the same
// counts are also emitted structurally via EmitExtra.
func (c rdmaFabricCoverage) coverageLine(validated int) string {
	if len(c.cordoned) == 0 {
		return fmt.Sprintf("RESULT: nodesValidated: %d/%d", validated, c.total())
	}
	return fmt.Sprintf("RESULT: nodesValidated: %d/%d (%d cordoned, skipped)",
		validated, c.total(), len(c.cordoned))
}

// rdmaFabricCoverageExtra builds the structured coverage disclosure carried
// through the redaction boundary: how many schedulable RDMA nodes the gate
// certified (validated) out of every RDMA-candidate node incl. cordoned (total).
// Values are counts only — never node names or IPs (those live in the stdout
// enumeration lines). The keys mirror check-nvidia-smi's coverage Extra and the
// existing ctrfExtraAllowlist entries.
func rdmaFabricCoverageExtra(validated, total int) map[string]string {
	return map[string]string{
		"nodesValidated": strconv.Itoa(validated),
		"nodesTotal":     strconv.Itoa(total),
	}
}

// rdmaFabricProbeCoverage does one readiness pass over the Mellanox RDMA-capable
// GPU nodes. It enumerates every GPU node via helper.FindGpuNodes (NOT
// FindSchedulableGpuNodes) so cordoned RDMA nodes stay VISIBLE in the coverage,
// then validates only the schedulable cohort: nodes carrying the NicClusterPolicy
// nodeAffinity label helper.PCIMellanoxPresentLabel. It returns nil — plus the
// coverage partition — only when every schedulable such node advertises
// fabricResource in a uniform, positive count. It fails closed on a
// List error and when no schedulable RDMA GPU node is observed yet: "could not
// observe the fabric" must never read as "fabric ready". The returned error rides
// the poll's dwell reset like any other unhealthy sample; the coverage is
// returned alongside every error so the terminal disclosure can still name the
// cordoned nodes it saw.
func rdmaFabricProbeCoverage(ctx *validators.Context, fabricResource string) (rdmaFabricCoverage, error) {
	listCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()

	gpuNodes, err := helper.FindGpuNodes(listCtx, ctx.Clientset)
	if err != nil {
		// FindGpuNodes may return a coded *errors.StructuredError (ErrCodeTimeout if
		// cancellation interrupts its own node scan, before this function's loop).
		// PropagateOrWrap preserves that code, wrapping only a plain error with
		// ErrCodeInternal + gate context.
		return rdmaFabricCoverage{}, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to list nodes for the RDMA fabric readiness gate")
	}

	fabric := corev1.ResourceName(fabricResource)
	type rdmaNode struct {
		name  string
		count int64
	}
	var cohort []rdmaNode
	var coverage rdmaFabricCoverage
	for i := range gpuNodes {
		// Honor cancellation while walking a potentially large node list, per
		// repo CLAUDE.md "Always check ctx.Done() in long-running operations".
		select {
		case <-listCtx.Done():
			// Return the coverage accumulated so far, not an empty partition:
			// the function's contract (and every other error path below) hands
			// back the cordoned nodes already seen so the terminal disclosure can
			// still name them. Set schedulable from the cohort scanned before the
			// cancellation so a partially-walked cohort count is not lost.
			coverage.schedulable = len(cohort)
			return coverage, errors.Wrap(errors.ErrCodeTimeout,
				"canceled while scanning nodes for the RDMA fabric readiness gate", listCtx.Err())
		default:
		}
		node := &gpuNodes[i].Node
		// Only Mellanox RDMA-capable GPU nodes are fabric candidates; a non-RDMA
		// GPU node never advertises the shared resource.
		if node.Labels[helper.PCIMellanoxPresentLabel] != "true" {
			continue
		}
		// A cordoned RDMA-capable node is excluded from the validated cohort (the
		// NCCL workload will not land on it) but is disclosed, not dropped — the
		// spuriously-narrowed pass #1668/#1936 fixed, applied here (#1952).
		if gpuNodes[i].Cordoned {
			coverage.cordoned = append(coverage.cordoned, node.Name)
			continue
		}
		var count int64
		if q, ok := node.Status.Allocatable[fabric]; ok {
			count = q.Value()
		}
		cohort = append(cohort, rdmaNode{name: node.Name, count: count})
	}
	coverage.schedulable = len(cohort)

	if len(cohort) == 0 {
		return coverage, errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("RDMA fabric gate: no schedulable Mellanox RDMA-capable GPU nodes observed yet (label %s=true)",
				helper.PCIMellanoxPresentLabel))
	}

	// Not-ready nodes: the fabric resource is absent or zero — MOFED /
	// rdma-shared-device-plugin has not finished rolling out on that node.
	var notReady []string
	for _, n := range cohort {
		if n.count <= 0 {
			notReady = append(notReady, n.name)
		}
	}
	if len(notReady) > 0 {
		return coverage, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s not yet allocatable on %d of %d RDMA GPU node(s): %s "+
				"(network operator MOFED / RDMA device plugin still rolling out)",
				fabricResource, len(notReady), len(cohort), formatNames(notReady)))
	}

	// All present and positive: require a uniform count, matching the NCCL
	// consumer's uniformFabricResourceCount. A skew (e.g. 1000 vs 500) means the
	// fabric is still settling and the NCCL check would reject it as non-uniform.
	want := cohort[0].count
	var skew []string
	for _, n := range cohort {
		if n.count != want {
			skew = append(skew, fmt.Sprintf("%s=%d", n.name, n.count))
		}
	}
	if len(skew) > 0 {
		return coverage, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%s allocatable count is non-uniform across %d RDMA GPU node(s) (want all == %d): %s",
				fabricResource, len(cohort), want, formatNames(skew)))
	}
	return coverage, nil
}

func formatNames(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	return "[" + strings.Join(names, ", ") + "]"
}

func buildResourceFetcher(ctx *validators.Context) (chainsaw.ResourceFetcher, error) {
	if ctx.RESTConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
	}

	// Mapper AND partial-discovery-probe wiring is shared with the readiness
	// gate (cmd/gate) so the two consumers of the in-process executor cannot
	// drift; only the dynamic client stays local, because ctx.DynamicClient is
	// an injection seam for tests. Going through NewClusterFetcherWithClient
	// (rather than assembling a mapper and calling NewClusterFetcher) is what
	// gives the validator the same fail-closed no-match classification the gate
	// gets: without the probe, a kind whose API group failed discovery reads as
	// "absent" and satisfies every negative assertion.
	//
	// Invariant: a namespaced check MUST set metadata.namespace on its
	// resource block. Unlike the gate — which has one release namespace and
	// wraps its fetcher to default to it — the validator evaluates components
	// spread across namespaces and has no single sensible default, so an
	// omitted namespace Lists across ALL namespaces.
	return chainsaw.NewClusterFetcherWithClient(dynClient, ctx.RESTConfig)
}

func getDynamicClient(ctx *validators.Context) (dynamic.Interface, error) {
	if ctx.DynamicClient != nil {
		return ctx.DynamicClient, nil
	}
	if ctx.RESTConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}

	// Reached only when a caller assembled the Context by hand: LoadContext
	// always populates DynamicClient (validators/context.go), so in production
	// the branch above returns first. Kept because the deployment validator's
	// tests build a Context directly, and building through pkg/chainsaw gives
	// that client the same request bound the shared RESTMapper carries.
	dynClient, err := chainsaw.NewDynamicClientForConfig(ctx.RESTConfig)
	if err != nil {
		return nil, err
	}
	ctx.DynamicClient = dynClient
	return dynClient, nil
}

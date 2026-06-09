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
	stderrors "errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/chainsaw"
	"github.com/NVIDIA/aicr/validators/helper"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
)

const (
	gpuOperatorComponent              = "gpu-operator"
	nodewrightCustomizationsComponent = "nodewright-customizations"
	draDriverComponent                = "nvidia-dra-driver-gpu"
	clusterPolicyName                 = "cluster-policy"

	// draKubeletPluginSuffix is the chart-template-defined name suffix for
	// the NVIDIA DRA driver's kubelet-plugin DaemonSet. The upstream chart
	// renders its DaemonSet name as "<fullname>-kubelet-plugin", where
	// "<fullname>" is controlled by chart values. Discovering by suffix is
	// deployer-neutral: it reads only a live Kubernetes object name shape,
	// makes no assumption about release identity or the deployer that
	// installed the chart.
	draKubeletPluginSuffix = "-kubelet-plugin"

	clusterPolicyReadyState = "ready"
	nodewrightCompleteState = "complete"
)

var (
	clusterPolicyGVR = schema.GroupVersionResource{
		Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies",
	}
	nodewrightGVR = schema.GroupVersionResource{
		Group: "skyhook.nvidia.com", Version: "v1alpha1", Resource: "skyhooks",
	}
)

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
			chainsawAsserts = append(chainsawAsserts, chainsaw.ComponentAssert{
				Name:       ref.Name,
				AssertYAML: ref.HealthCheckAsserts,
			})
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
						var se *errors.StructuredError
						if stderrors.As(r.Error, &se) {
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

// verifyGPUReadinessSignals runs the three Go-resident deep checks
// introduced by issue #611. Returns the human-readable failure strings
// plus the first *errors.StructuredError encountered across all three
// helpers so the caller can propagate the original error code (e.g.,
// ErrCodeInternal from a discovery/RBAC failure) instead of flattening
// it into the generic ErrCodeNotFound summary — per PR #1235 review.
//
// Migration disposition (per #1220 plan):
//
//   - clusterPolicyReady: candidate for migration to registry-declared
//     Chainsaw YAML in recipes/checks/gpu-operator/health-check.yaml.
//     Deferred to follow-up because the existing expected_resources_test.go
//     suite mocks ClusterPolicy state extensively, and the migration must
//     prove assertion equivalence rather than just code equivalence.
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
			var se *errors.StructuredError
			if stderrors.As(err, &se) {
				firstStructured = err
			}
		}
	}

	if ref, ok := findEnabledComponent(refs, nodewrightCustomizationsComponent); ok {
		capture(verifyNodewrightReady(ctx, ref))
	}

	if _, ok := findEnabledComponent(refs, gpuOperatorComponent); ok {
		capture(verifyClusterPolicyReady(ctx))
	}

	if ref, ok := findEnabledComponent(refs, draDriverComponent); ok {
		capture(verifyDRAKubeletPluginReady(ctx, ref.Namespace))
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
		// The recipe enabled nodewright-customizations but declared no Nodewright
		// manifests, so we cannot prove readiness. Fail closed rather than
		// silently pass — treating this as a recipe misconfiguration that the
		// user should see.
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no Nodewright CR names could be extracted from component %s manifestFiles=%v",
				ref.Name, ref.ManifestFiles))
	}

	// Discovery-gate the CRD before attempting Get by name. Matches the
	// verifyClusterPolicyReady pattern: CRD not registered → skip per #607;
	// any other discovery error (RBAC, 5xx, timeout) → fail closed so a
	// transient discovery failure cannot mask readiness.
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

	var failures []string
	for _, name := range expectedNames {
		verifyCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
		sk, getErr := dynClient.Resource(nodewrightGVR).Get(verifyCtx, name, metav1.GetOptions{})
		cancel()
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				failures = append(failures, fmt.Sprintf("Nodewright %s: not found (recipe declared it but the cluster has no such CR)", name))
				continue
			}
			failures = append(failures, fmt.Sprintf("Nodewright %s: failed to get: %v", name, getErr))
			continue
		}
		status, found, statusErr := unstructured.NestedString(sk.Object, "status", "status")
		if statusErr != nil {
			failures = append(failures, fmt.Sprintf("Nodewright %s: failed to read status.status: %v", name, statusErr))
			continue
		}
		if !found {
			failures = append(failures, fmt.Sprintf("Nodewright %s: missing status.status", name))
			continue
		}
		if status != nodewrightCompleteState {
			failures = append(failures, fmt.Sprintf("Nodewright %s: status=%s (want %s)", name, status, nodewrightCompleteState))
			continue
		}
		fmt.Printf("  Nodewright %s: %s\n", name, nodewrightCompleteState)
	}

	if len(failures) > 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%d of %d expected Nodewright(s) not ready:\n  %s",
				len(failures), len(expectedNames), strings.Join(failures, "\n  ")))
	}
	return nil
}

// expectedNodewrightNames derives the set of Nodewright CR names that this
// component is expected to deploy, by reading each ManifestFile through the
// recipe data provider and extracting the metadata.name of every Nodewright
// resource declared in those files.
func expectedNodewrightNames(ref recipe.ComponentRef) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for _, path := range ref.ManifestFiles {
		content, err := recipe.GetManifestContent(path)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to load manifest %s for component %s", path, ref.Name), err)
		}
		for _, name := range extractNodewrightNamesFromManifest(content) {
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
// CR names out of a manifest file that may contain Helm template directives
// ({{ ... }}). A full YAML parse is not an option: templated lines are not
// valid YAML on their own, and evaluating Helm templates at validate time
// would require chart values the validator does not have.
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

func verifyClusterPolicyReady(ctx *validators.Context) error {
	// Use discovery to distinguish two cases the dynamic-client Get conflates:
	//   1. CRD not registered ("CustomResourceDefinition clusterpolicies
	//      does not exist") — the recipe declares gpu-operator but the
	//      operator chart is not installed yet. Skip per #607.
	//   2. CRD registered but the cluster-policy CR is absent — gpu-operator
	//      is installed but its singleton CR was never created or has been
	//      manually deleted. The operator cannot reconcile the GPU stack in
	//      that state, so this is a real misconfiguration — fail.
	// A bare Get() that returns IsNotFound cannot tell these apart. Explicit
	// discovery lookup does.
	//
	// Critically, only skip on IsNotFound from discovery. Anything else
	// (403 from RBAC, 5xx from an overloaded API server, network timeout) is
	// a real signal that we cannot prove readiness, and silently passing would
	// hide a broken cluster. Fail closed on those.
	gv := clusterPolicyGVR.GroupVersion().String()
	_, discErr := ctx.Clientset.Discovery().ServerResourcesForGroupVersion(gv)
	switch {
	case discErr == nil:
		// fall through to the CR check
	case apierrors.IsNotFound(discErr):
		fmt.Printf("  ClusterPolicy: %s not registered, skipping\n", gv)
		return nil
	default:
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to discover %s resources (is the API server reachable and RBAC in order?)", gv), discErr)
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	verifyCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	clusterPolicy, err := dynClient.Resource(clusterPolicyGVR).Get(verifyCtx, clusterPolicyName, metav1.GetOptions{})
	cancel()
	if err != nil {
		// CRD is registered (we just checked). Any Get error here — including
		// IsNotFound on the CR itself — signals that gpu-operator is installed
		// but not reconciled. Surface it rather than silently skipping.
		return errors.Wrap(errors.ErrCodeNotFound,
			"failed to get ClusterPolicy cluster-policy (gpu-operator installed but CR missing?)", err)
	}

	state, found, stateErr := unstructured.NestedString(clusterPolicy.Object, "status", "state")
	if stateErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read ClusterPolicy status.state", stateErr)
	}
	if !found {
		return errors.New(errors.ErrCodeInternal, "ClusterPolicy status.state not found")
	}
	if state != clusterPolicyReadyState {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("ClusterPolicy state=%s (want %s)", state, clusterPolicyReadyState))
	}

	fmt.Printf("  ClusterPolicy %s: %s\n", clusterPolicyName, clusterPolicyReadyState)
	return nil
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
	verifyCtx, cancel := ctx.Timeout(defaults.ResourceVerificationTimeout)
	defer cancel()

	dsList, err := ctx.Clientset.AppsV1().DaemonSets(namespace).List(verifyCtx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
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

	switch len(matches) {
	case 0:
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no kubelet-plugin DaemonSet (name suffix %q) found in namespace %s (DaemonSets in namespace: %s)",
				draKubeletPluginSuffix, namespace, formatNames(seenNames)))
	case 1:
		// proceed
	default:
		matchedNames := make([]string, 0, len(matches))
		for _, ds := range matches {
			matchedNames = append(matchedNames, ds.Name)
		}
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("ambiguous: %d DaemonSets in namespace %s match kubelet-plugin role suffix %q: %s",
				len(matches), namespace, draKubeletPluginSuffix, formatNames(matchedNames)))
	}

	ds := matches[0]
	if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady == 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DaemonSet %s/%s: no ready kubelet-plugin pods scheduled (%d/%d pods ready)",
				namespace, ds.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}
	if ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DaemonSet %s/%s: not healthy: %d/%d pods ready",
				namespace, ds.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}

	fmt.Printf("  DaemonSet %s/%s: healthy\n", namespace, ds.Name)
	return nil
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

	discoveryClient, err := kubernetes.NewForConfig(ctx.RESTConfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create discovery client", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(discoveryClient.Discovery()),
	)

	return chainsaw.NewClusterFetcher(dynClient, mapper), nil
}

func getDynamicClient(ctx *validators.Context) (dynamic.Interface, error) {
	if ctx.DynamicClient != nil {
		return ctx.DynamicClient, nil
	}
	if ctx.RESTConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}

	dynClient, err := dynamic.NewForConfig(ctx.RESTConfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}
	ctx.DynamicClient = dynClient
	return dynClient, nil
}

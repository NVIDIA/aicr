// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/helmclient"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	yaml "sigs.k8s.io/yaml"
)

// ValidationResult captures the cluster state of one manifest discovered
// under the deployment-files directory. State is the canonical
// classification produced by the crstate registry; Found/Missing are
// derived helpers preserved so the existing text/JSON callers don't
// have to change their summary logic on day one (success/in-progress
// → Found, not-deployed → Missing, error → ERROR).
type ValidationResult struct {
	Kind       string
	APIVersion string
	Name       string
	Namespace  string
	SourceFile string

	// State is the four-way classification (not-deployed / error /
	// in-progress / success). Reason/Details carry the validator's
	// per-Kind human-readable summary and per-companion-CR breakdown
	// (e.g. per-node SriovNetworkNodeState states).
	State   crstate.CRState
	Reason  string
	Details map[string]string

	// LiveYAML is the cluster's view of the object, marshalled
	// back to YAML for the validation report's expandable "Live YAML"
	// dropdown. Empty when the object isn't present
	// (StateNotDeployed) or when the post-validate fetch failed.
	// Managed-fields / status are kept; we want the operator to
	// see what the controller actually wrote.
	LiveYAML string `json:"-"`

	// Legacy derived flags — keep for backwards-compatible JSON
	// consumers. Found is true for StateSuccess and StateInProgress
	// (the object exists in the cluster, even if it's still
	// reconciling). Missing is true only for StateNotDeployed.
	// Anything else (StateError) is rendered as a third "ERROR"
	// status by emitValidationReport.
	Found   bool
	Missing bool
	// Detail is a short human-readable summary equivalent to Reason —
	// retained as the field old JSON consumers parse.
	Detail string
}

// HelmReleaseInfo carries the subset of fields we care about from a
// Helm release Secret.
type HelmReleaseInfo struct {
	Name         string
	Namespace    string
	ChartName    string
	ChartVersion string
	AppVersion   string
	Revision     int
	Status       string
}

// VersionCheck captures the comparison between the Network Operator
// Helm release deployed in the cluster and the release line declared
// in the user's cluster-config.yaml.
type VersionCheck struct {
	// Skipped is true when the check could not be performed (no
	// selectedRelease in user-config, or no Helm release Secret found).
	Skipped       bool
	Reason        string
	SelectedRelease string // from user-config (e.g. "26.4")
	ExpectedVersion string // from embedded release catalog (e.g. "v26.4.0-beta.6")
	DeployedRelease *HelmReleaseInfo
	Match         bool
}

const (
	helmReleaseSecretType   = "helm.sh/release.v1"
	helmReleaseSecretPrefix = "sh.helm.release.v1."
)

// ComponentVersionCheck holds the result of cross-referencing each
// version-bearing field in the live NicClusterPolicy + NicNodePolicy
// CRs against the expected versions for the user's selectedRelease.
//
// The Helm-chart appVersion check (CheckHelmReleaseVersion) tells us
// whether the *operator* matches the release line; this check tells
// us whether the *resources the operator manages* (ofedDriver,
// nvIpam, multus, device plugins, …) carry the catalog-pinned image
// tags. Out-of-band kubectl edits, partial upgrades, or
// hand-rolled chart values are the usual reasons for divergence.
type ComponentVersionCheck struct {
	// Skipped is true when the check couldn't run (no
	// selectedRelease in user-config, no NCP in the cluster, etc.).
	Skipped bool
	Reason  string
	// ExpectedComponent is the catalog's
	// networkOperator.componentVersion (e.g.
	// "network-operator-v26.4.0-beta.9"). Used as the expected
	// value for every operator/device-plugin component.
	ExpectedComponent string
	// ExpectedDOCA is the catalog's docaDriver.version (e.g.
	// "doca3.4.0-26.04-0.8.4.0-0"). Used as the expected value for
	// every ofedDriver block.
	ExpectedDOCA string
	// Components is the per-section result, one entry per
	// version-bearing block found in the cluster.
	Components []ComponentVersionResult
	// AllMatch is true when every component matched. False when at
	// least one mismatched OR when Components is empty (no CRs in
	// the cluster — that's already its own signal).
	AllMatch bool
}

// ComponentVersionResult is one row of the ComponentVersionCheck.
type ComponentVersionResult struct {
	Source   string // "NicClusterPolicy/nic-cluster-policy", "NicNodePolicy/<name>"
	Section  string // "ofedDriver", "nvIpam", "rdmaSharedDevicePlugin", …
	Expected string
	Actual   string
	Match    bool
	// Kind is "component" or "doca" — labels which catalog field
	// supplied the expected value, so a report can group results
	// or explain mismatches.
	Kind string
}

// ValidateManifests reads YAML manifests from manifestDir (skipping example
// workloads) and classifies each object in the cluster via the crstate
// registry. Returns a slice of per-object results.
//
// Unlike the old "Get and check NotFound" path, this routes every
// manifest through the same per-Kind validator the deploy state machine
// uses, so SriovNetworkNodePolicy's silent-failure cross-check, the
// NicConfigurationTemplate's condition-Reason classification, and
// NicClusterPolicy's appliedStates breakdown all surface here too.
func ValidateManifests(ctx context.Context, c client.Client, manifestDir string) ([]ValidationResult, error) {
	entries, err := os.ReadDir(manifestDir)
	if err != nil {
		return nil, fmt.Errorf("read manifest dir %s: %w", manifestDir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if isExampleManifest(e.Name()) {
			log.Log.V(1).Info("Skipping example manifest", "file", e.Name())
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	registry := crstate.NewDefault()
	var results []ValidationResult
	for _, name := range files {
		full := filepath.Join(manifestDir, name)
		content, err := os.ReadFile(full)
		if err != nil {
			return results, fmt.Errorf("read %s: %w", full, err)
		}
		for _, doc := range splitYAMLDocuments(string(content)) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
				log.Log.V(1).Info("Skipping unparseable manifest doc", "file", name, "error", err.Error())
				continue
			}
			if obj.GetKind() == "" || obj.GetName() == "" {
				continue
			}
			r := ValidationResult{
				Kind:       obj.GetKind(),
				APIVersion: obj.GetAPIVersion(),
				Name:       obj.GetName(),
				Namespace:  obj.GetNamespace(),
				SourceFile: name,
			}

			gv, gvErr := schema.ParseGroupVersion(obj.GetAPIVersion())
			if gvErr != nil {
				r.State = crstate.StateError
				r.Reason = fmt.Sprintf("invalid apiVersion %q: %v", obj.GetAPIVersion(), gvErr)
				r.Detail = r.Reason
				results = append(results, r)
				continue
			}
			// Ensure GVK on the manifest object so the registry can
			// dispatch to the right validator.
			obj.SetGroupVersionKind(gv.WithKind(obj.GetKind()))

			res, vErr := registry.Validate(ctx, c, obj)
			if vErr != nil {
				// Transport error — treat as error state. Use the
				// Reason carried by the Result (registry sets it
				// even on err) so callers still get a useful
				// summary.
				r.State = crstate.StateError
				r.Reason = res.Reason
				if r.Reason == "" {
					r.Reason = vErr.Error()
				}
			} else {
				r.State = res.State
				r.Reason = res.Reason
			}
			r.Details = res.Details
			// Best-effort capture of the live object for the
			// HTML report's "Live YAML" dropdown. Skip when the
			// validator says the object isn't deployed — there's
			// nothing in the cluster to fetch.
			if r.State != crstate.StateNotDeployed {
				r.LiveYAML = fetchLiveYAML(ctx, c, obj)
			}
			applyLegacyFlags(&r)
			results = append(results, r)
		}
	}
	return results, nil
}

// applyLegacyFlags fills in the backwards-compatible Found / Missing /
// Detail fields from the canonical State + Reason.
func applyLegacyFlags(r *ValidationResult) {
	r.Detail = r.Reason
	switch r.State {
	case crstate.StateSuccess, crstate.StateInProgress:
		r.Found = true
		r.Missing = false
	case crstate.StateNotDeployed:
		r.Found = false
		r.Missing = true
		if r.Reason == "" {
			r.Detail = "not found in cluster"
			r.Reason = r.Detail
		}
	case crstate.StateError:
		r.Found = false
		r.Missing = false
	}
}

// CheckComponentVersions reads the live NicClusterPolicy and every
// NicNodePolicy in the operator namespace, extracts each component
// section's `version:` field, and compares it against the catalog
// entry for `selectedRelease`. Returns a ComponentVersionCheck whose
// Components slice has one entry per version-bearing block found,
// each tagged with Match / Expected / Actual so a CLI or HTML report
// can render the per-component status.
//
// The check soft-skips (Skipped=true, populated Reason) when:
//   - selectedRelease is empty
//   - selectedRelease isn't in the embedded catalog
//   - the cluster has no NicClusterPolicy at all (nothing to check)
func CheckComponentVersions(ctx context.Context, c client.Client, namespace, selectedRelease string) (*ComponentVersionCheck, error) {
	if selectedRelease == "" {
		return &ComponentVersionCheck{Skipped: true, Reason: "no networkOperator.selectedRelease in user-config"}, nil
	}
	rel, ok := LookupRelease(selectedRelease)
	if !ok {
		return &ComponentVersionCheck{Skipped: true, Reason: fmt.Sprintf("selectedRelease %q is not in the embedded catalog", selectedRelease)}, nil
	}

	cv := &ComponentVersionCheck{
		ExpectedComponent: rel.NetworkOperator.ComponentVersion,
		ExpectedDOCA:      rel.DOCADriver.Version,
	}

	// NicClusterPolicy: there's exactly one per cluster (the deploy
	// path enforces this) — list rather than Get-by-name so we
	// don't have to guess the name.
	ncpList := &unstructured.UnstructuredList{}
	ncpList.SetGroupVersionKind(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicClusterPolicyList"})
	if err := c.List(ctx, ncpList); err != nil {
		return &ComponentVersionCheck{Skipped: true, Reason: fmt.Sprintf("list NicClusterPolicy: %v", err)}, nil
	}
	if len(ncpList.Items) == 0 {
		return &ComponentVersionCheck{Skipped: true, Reason: "no NicClusterPolicy in cluster — run `l8k deploy` first"}, nil
	}

	for i := range ncpList.Items {
		ncp := &ncpList.Items[i]
		source := fmt.Sprintf("NicClusterPolicy/%s", ncp.GetName())
		// NCP sections that carry .version. Walk them
		// deterministically rather than ranging over spec —
		// missing sections (e.g. nicConfigurationOperator not
		// enabled) are simply skipped, not reported as empty
		// mismatches.
		for _, section := range []ncpVersionPath{
			{Path: []string{"spec", "nicConfigurationOperator", "operator", "version"}, Section: "nicConfigurationOperator.operator", Kind: "component"},
			{Path: []string{"spec", "nicConfigurationOperator", "configurationDaemon", "version"}, Section: "nicConfigurationOperator.configurationDaemon", Kind: "component"},
			{Path: []string{"spec", "nvIpam", "version"}, Section: "nvIpam", Kind: "component"},
			{Path: []string{"spec", "secondaryNetwork", "cniPlugins", "version"}, Section: "secondaryNetwork.cniPlugins", Kind: "component"},
			{Path: []string{"spec", "secondaryNetwork", "multus", "version"}, Section: "secondaryNetwork.multus", Kind: "component"},
			{Path: []string{"spec", "ofedDriver", "version"}, Section: "ofedDriver", Kind: "doca"},
		} {
			r := compareVersion(ncp, source, section, cv.ExpectedComponent, cv.ExpectedDOCA)
			if r != nil {
				cv.Components = append(cv.Components, *r)
			}
		}
	}

	// NicNodePolicy: zero or more per cluster (one per node group
	// in the 26.4+ model). 26.1 has no NNPs.
	nnpList := &unstructured.UnstructuredList{}
	nnpList.SetGroupVersionKind(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicNodePolicyList"})
	if err := c.List(ctx, nnpList); err != nil {
		// Tolerate the list error — the cluster might not have the
		// NicNodePolicy CRD at all (older release). Just don't add
		// any NNP rows.
		log.Log.V(1).Info("List NicNodePolicy failed during component-version check", "error", err.Error())
	}
	for i := range nnpList.Items {
		nnp := &nnpList.Items[i]
		source := fmt.Sprintf("NicNodePolicy/%s", nnp.GetName())
		for _, section := range []ncpVersionPath{
			{Path: []string{"spec", "ofedDriver", "version"}, Section: "ofedDriver", Kind: "doca"},
			{Path: []string{"spec", "rdmaSharedDevicePlugin", "version"}, Section: "rdmaSharedDevicePlugin", Kind: "component"},
			{Path: []string{"spec", "sriovDevicePlugin", "version"}, Section: "sriovDevicePlugin", Kind: "component"},
		} {
			r := compareVersion(nnp, source, section, cv.ExpectedComponent, cv.ExpectedDOCA)
			if r != nil {
				cv.Components = append(cv.Components, *r)
			}
		}
	}

	cv.AllMatch = len(cv.Components) > 0
	for _, r := range cv.Components {
		if !r.Match {
			cv.AllMatch = false
			break
		}
	}
	return cv, nil
}

// ncpVersionPath identifies one version-bearing section to inspect.
type ncpVersionPath struct {
	Path    []string
	Section string
	Kind    string // "component" or "doca"
}

// compareVersion reads the version field at section.Path on obj and
// returns a ComponentVersionResult when the section is present.
// Returns nil when the path is missing — that means the section
// wasn't rendered (e.g. nicConfigurationOperator disabled), not a
// mismatch.
func compareVersion(obj *unstructured.Unstructured, source string, section ncpVersionPath, expectedComponent, expectedDoca string) *ComponentVersionResult {
	actual, found, _ := unstructured.NestedString(obj.Object, section.Path...)
	if !found || actual == "" {
		return nil
	}
	expected := expectedComponent
	if section.Kind == "doca" {
		expected = expectedDoca
	}
	return &ComponentVersionResult{
		Source:   source,
		Section:  section.Section,
		Expected: expected,
		Actual:   actual,
		Match:    actual == expected,
		Kind:     section.Kind,
	}
}

// fetchLiveYAML grabs the live object for the manifest by GVK +
// namespace/name and serializes it back to YAML. Returns "" on any
// fetch / marshal failure — the report just hides the dropdown.
//
// Side-effect of routing through the controller-runtime client: we
// rely on the cluster honouring `kubectl get -o yaml` semantics, so
// the YAML is faithful to what `kubectl` would show, including the
// status subresource the operator wrote.
func fetchLiveYAML(ctx context.Context, c client.Client, manifest *unstructured.Unstructured) string {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(manifest.GroupVersionKind())
	key := types.NamespacedName{Namespace: manifest.GetNamespace(), Name: manifest.GetName()}
	if err := c.Get(ctx, key, live); err != nil {
		log.Log.V(1).Info("fetchLiveYAML get failed", "kind", manifest.GetKind(), "name", manifest.GetName(), "error", err.Error())
		return ""
	}
	// Drop verbose metadata that just adds noise to the dropdown
	// — managedFields can be 80% of the document on an active CR.
	live.SetManagedFields(nil)
	data, err := yaml.Marshal(live.Object)
	if err != nil {
		log.Log.V(1).Info("fetchLiveYAML marshal failed", "kind", manifest.GetKind(), "name", manifest.GetName(), "error", err.Error())
		return ""
	}
	return string(data)
}

// IsExampleManifest reports whether the given filename matches the
// example-workload naming pattern (case-insensitive substring
// "example"). Files matching this pattern are deployed by
// `l8k validate --connectivity` for the ping matrix and skipped by
// every other code path. Exported so the connectivity package can
// find the example DS to apply.
func IsExampleManifest(name string) bool {
	return isExampleManifest(name)
}

// isExampleManifest treats files matching *example* (e.g.
// 50-example-daemonset.yaml) as test/demo workloads outside the
// network-operator surface to validate.
func isExampleManifest(name string) bool {
	return strings.Contains(strings.ToLower(name), "example")
}

// CheckHelmReleaseVersion compares the Network Operator Helm release
// installed in `namespace` against the version expected by `selectedRelease`
// looked up in the embedded catalog.
//
// When `selectedRelease` is empty, or no matching Helm release Secret is
// found, the returned VersionCheck has Skipped=true with a Reason.
func CheckHelmReleaseVersion(ctx context.Context, c client.Client, namespace, selectedRelease string) (*VersionCheck, error) {
	if selectedRelease == "" {
		return &VersionCheck{Skipped: true, Reason: "no networkOperator.selectedRelease in user-config"}, nil
	}

	rel, ok := LookupRelease(selectedRelease)
	if !ok {
		return &VersionCheck{
			Skipped:         true,
			Reason:          fmt.Sprintf("selectedRelease %q is not in the embedded catalog", selectedRelease),
			SelectedRelease: selectedRelease,
		}, nil
	}

	deployed, err := findNetworkOperatorHelmRelease(ctx, c, namespace)
	if err != nil {
		return &VersionCheck{
			Skipped:         true,
			Reason:          fmt.Sprintf("Helm release lookup failed: %v", err),
			SelectedRelease: selectedRelease,
			ExpectedVersion: rel.NetworkOperator.Version,
		}, nil
	}

	return &VersionCheck{
		SelectedRelease: selectedRelease,
		ExpectedVersion: rel.NetworkOperator.Version,
		DeployedRelease: deployed,
		Match:           deployed.AppVersion == rel.NetworkOperator.Version,
	}, nil
}

// HelmValuesCheck captures the comparison between the user-supplied values
// of the deployed network-operator helm release and the values l8k would
// install now from the freshly rendered values.yaml. Surfaced in the
// validate HTML report under "Network Operator release" and gates exit
// code 4 when AllMatch=false (same as version-mismatch).
//
// The check is intentionally narrow: it compares user-supplied values
// (action.GetValues without --all), not the merged chart defaults, so the
// validator only flags drift that re-running `l8k deploy` would actually
// converge.
type HelmValuesCheck struct {
	// Skipped is true when the check could not be performed: no
	// selectedRelease in user-config, no values.yaml on disk, no release
	// in the target namespace, or a transient kube-API failure.
	Skipped bool
	Reason  string
	// Namespace and ReleaseName describe what the check examined.
	Namespace   string
	ReleaseName string
	// Diff lists the paths that differ between the deployed values and
	// the generated values. Empty when AllMatch=true.
	Diff []ValueDiff
	// AllMatch is true when the deployed values are deep-equal to the
	// generated values. Combined with Skipped=false, this is the
	// "validate green" condition for the helm-values check.
	AllMatch bool
}

// ValueDiff records a single differing path between two helm values trees.
// Used by the validate flow's HTML report to render a per-path table. The
// underlying comparison logic lives in pkg/networkoperatorplugin/preflight
// (DeepEqualValues); ValueDiff is the projection consumed by report.html.tmpl.
type ValueDiff struct {
	Path      string
	Deployed  interface{}
	Generated interface{}
}

// CheckHelmReleaseValues compares the user-supplied values of the deployed
// network-operator release in `namespace` against `generatedValuesYAML` (the
// rendered values.yaml from `l8k generate`). Same equality logic as the
// deploy-time conflict check in InstallOrUpgrade, so the validator and
// deploy can't disagree about whether the deployed release matches what
// `l8k deploy` would install now.
//
// When `generatedValuesYAML` is empty, restConfig is nil, or no release
// exists in the target namespace, returns Skipped=true with a Reason.
func CheckHelmReleaseValues(ctx context.Context, restConfig *rest.Config, namespace string, generatedValuesYAML []byte) (*HelmValuesCheck, error) {
	if restConfig == nil {
		return &HelmValuesCheck{Skipped: true, Reason: "no kube REST config available for helm values check"}, nil
	}
	if len(generatedValuesYAML) == 0 {
		return &HelmValuesCheck{Skipped: true, Reason: "no values.yaml in deployment dir — chart managed out of band"}, nil
	}
	if namespace == "" {
		namespace = "nvidia-network-operator"
	}

	actionCfg, err := helmclient.NewActionConfig(restConfig, namespace, helmclient.StorageDriver)
	if err != nil {
		return &HelmValuesCheck{
			Skipped:   true,
			Reason:    fmt.Sprintf("helm action config init failed: %v", err),
			Namespace: namespace,
		}, nil
	}

	// GetValues without --all returns only the user-supplied values,
	// matching what helm.go writes during install. Comparing against the
	// merged chart defaults would be noisy (sub-chart defaults change
	// release-over-release) and out of scope for this check.
	getValues := action.NewGetValues(actionCfg)
	getValues.AllValues = false
	deployed, err := getValues.Run(helmclient.DefaultReleaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return &HelmValuesCheck{
				Skipped:     true,
				Reason:      fmt.Sprintf("no helm release %q in namespace %s — run `l8k deploy` first", helmclient.DefaultReleaseName, namespace),
				Namespace:   namespace,
				ReleaseName: helmclient.DefaultReleaseName,
			}, nil
		}
		return &HelmValuesCheck{
			Skipped:     true,
			Reason:      fmt.Sprintf("helm get values failed: %v", err),
			Namespace:   namespace,
			ReleaseName: helmclient.DefaultReleaseName,
		}, nil
	}

	generated, err := helmclient.UnmarshalValues(generatedValuesYAML)
	if err != nil {
		return &HelmValuesCheck{
			Skipped:     true,
			Reason:      fmt.Sprintf("parse generated values.yaml: %v", err),
			Namespace:   namespace,
			ReleaseName: helmclient.DefaultReleaseName,
		}, nil
	}

	// Use the shared preflight diff so deploy + validate can't disagree.
	mismatches := preflight.DeepEqualValues(deployed, generated)
	out := &HelmValuesCheck{
		Namespace:   namespace,
		ReleaseName: helmclient.DefaultReleaseName,
		AllMatch:    len(mismatches) == 0,
	}
	for _, m := range mismatches {
		out.Diff = append(out.Diff, ValueDiff{
			Path:      m.Path,
			Deployed:  m.Actual,
			Generated: m.Expected,
		})
	}
	return out, nil
}

func findNetworkOperatorHelmRelease(ctx context.Context, c client.Client, namespace string) (*HelmReleaseInfo, error) {
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list secrets in %s: %w", namespace, err)
	}
	var matched []corev1.Secret
	for _, s := range secrets.Items {
		if string(s.Type) != helmReleaseSecretType {
			continue
		}
		if !strings.HasPrefix(s.Name, helmReleaseSecretPrefix) {
			continue
		}
		releaseName := helmReleaseNameFromSecret(s.Name)
		if !strings.Contains(strings.ToLower(releaseName), "network-operator") {
			continue
		}
		matched = append(matched, s)
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no Helm release Secret containing 'network-operator' in namespace %s", namespace)
	}
	sort.Slice(matched, func(i, j int) bool {
		return helmRevisionFromName(matched[i].Name) > helmRevisionFromName(matched[j].Name)
	})
	latest := matched[0]
	info, err := decodeHelmRelease(latest.Data["release"])
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", latest.Name, err)
	}
	return info, nil
}

// helmReleaseNameFromSecret extracts the release name from a Helm release
// secret name like "sh.helm.release.v1.<release>.v<N>".
func helmReleaseNameFromSecret(secretName string) string {
	rest := strings.TrimPrefix(secretName, helmReleaseSecretPrefix)
	dot := strings.LastIndex(rest, ".v")
	if dot < 0 {
		return rest
	}
	return rest[:dot]
}

func helmRevisionFromName(secretName string) int {
	rest := strings.TrimPrefix(secretName, helmReleaseSecretPrefix)
	dot := strings.LastIndex(rest, ".v")
	if dot < 0 {
		return 0
	}
	rev, err := strconv.Atoi(rest[dot+2:])
	if err != nil {
		return 0
	}
	return rev
}

// decodeHelmRelease parses the "release" key from a Helm release Secret.
// The value is base64-encoded gzip(JSON).
func decodeHelmRelease(b []byte) (*HelmReleaseInfo, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty release data")
	}
	decoded, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	plain, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	var rel struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status string `json:"status"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name       string `json:"name"`
				Version    string `json:"version"`
				AppVersion string `json:"appVersion"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(plain, &rel); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &HelmReleaseInfo{
		Name:         rel.Name,
		Namespace:    rel.Namespace,
		ChartName:    rel.Chart.Metadata.Name,
		ChartVersion: rel.Chart.Metadata.Version,
		AppVersion:   rel.Chart.Metadata.AppVersion,
		Revision:     rel.Version,
		Status:       rel.Info.Status,
	}, nil
}

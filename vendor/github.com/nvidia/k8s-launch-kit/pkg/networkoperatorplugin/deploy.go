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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	pkgerrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	yaml "sigs.k8s.io/yaml"
)

// helmValuesFile is the on-disk filename `l8k generate` writes per profile
// (when a 00-values.yaml template is present). `ApplyManifestsFromDir`
// looks for this exact filename in the deployment directory to drive the
// Phase 0 helm install. The filename matches the helm convention so users
// can also `helm install -f values.yaml` by hand if needed.
const helmValuesFile = "values.yaml"

// defaultHelmInstallTimeout caps the helm install/upgrade wait when the
// caller doesn't supply a budget via DeployOptions.HelmTimeout. Kept on the
// higher side because the upstream chart's Wait covers the operator pod
// rollout, which on slow networks can take a few minutes.
const defaultHelmInstallTimeout = 10 * time.Minute

// DeployOptions bundles the knobs that `ApplyManifestsFromDir` needs beyond
// the manifest directory itself. Keeping them in one struct keeps the
// callsite readable as Phase 0 (helm install) and the existing four phases
// grow more parameters over time.
type DeployOptions struct {
	// DryRun threads through to server-side dry-run for apply and to
	// action.Install.DryRun / action.Upgrade.DryRun for helm.
	DryRun bool
	// OverwriteExisting, when true, promotes the helm install path to
	// `helm upgrade --install` when a release already exists in the target
	// namespace with different user-supplied values. When false (default),
	// a value-conflict surfaces as an error pointing at this flag.
	OverwriteExisting bool
	// RestConfig is required for Phase 0. When nil, Phase 0 is skipped
	// (backward-compatible: lets users keep managing the chart out-of-band).
	RestConfig *rest.Config
	// NetworkOperator carries the helm-install metadata: chart version
	// (from Version, "v" prefix stripped), repo URL, and namespace. When
	// nil or HelmRepoURL is empty, Phase 0 is skipped.
	NetworkOperator *config.NetworkOperatorConfig
	// DOCAVersion is the catalog's docaDriver.version, used by the
	// preflight component-versions check to compare against the
	// ofedDriver section of NicClusterPolicy / NicNodePolicy. Empty when
	// no release is pinned; the component check soft-skips its DOCA rows
	// in that case.
	DOCAVersion string
	// HelmTimeout caps the helm install/upgrade Wait. Zero means use
	// defaultHelmInstallTimeout. The deploy-wide timeout (set on ctx by
	// the caller) is the absolute ceiling for the entire run.
	HelmTimeout time.Duration
}

// deployPollInterval is the cadence of the state-machine deploy loop's
// polling between Validate calls. Matches the historical 3-second wait
// helper cadence so logs feel familiar.
const deployPollInterval = 3 * time.Second

// appliedManifest pairs an applied "other" manifest with the
// information phase 4 needs to gate on controller observation:
// awaitObservationAfterRV holds the resourceVersion the server
// returned from our Patch when the apply bumped .metadata.generation
// (spec changed). The verify loop refuses to trust the live status
// until the live resourceVersion has advanced past that value —
// otherwise a re-deploy with new config would see the controller's
// stale `status: ready` from the previous reconcile and declare
// success before the operator had even noticed the new spec.
type appliedManifest struct {
	obj                     *unstructured.Unstructured
	awaitObservationAfterRV string
}

// DeployProfile is a thin wrapper that preserves the existing plugin call
// shape (profile arg unused). Delegates to ApplyManifestsFromDir, supplying
// the Phase 0 helm-install metadata from the plugin's own fields (populated
// by the launcher after ApplyOptionsToConfig has settled the config).
func (p *NetworkOperatorPlugin) DeployProfile(ctx context.Context, profile *profiles.Profile, kubeClient client.Client, manifestsDir string) error {
	_ = profile
	return ApplyManifestsFromDir(ctx, kubeClient, manifestsDir, DeployOptions{
		DryRun:            p.DryRun,
		OverwriteExisting: p.OverwriteExisting,
		RestConfig:        p.RESTConfig,
		NetworkOperator:   p.NetworkOperator,
		DOCAVersion:       p.DOCAVersion,
	})
}

// ApplyManifestsFromDir reads Kubernetes manifests from manifestsDir and
// applies them to the cluster in five phases:
//
//  0. Helm install — if `values.yaml` is present in manifestsDir AND opts
//     supplies a NetworkOperator config with HelmRepoURL, install or
//     upgrade the network-operator helm release into opts.NetworkOperator.
//     Namespace via the Helm Go SDK. Skipped silently when values.yaml is
//     absent, or when opts.RestConfig / opts.NetworkOperator are nil —
//     backward compatible with users managing the chart out of band.
//     A value-conflict against an existing release surfaces as an error
//     pointing at --overwrite-existing.
//  1. NicClusterPolicy — apply, then wait until the registry reports
//     success or error. NCP is upstream of every per-node component and
//     gates the rest of the deploy.
//  2. NicNodePolicy — apply each NNP and wait until it reports success
//     or error before moving on (preserves historical sequential
//     behavior since NNP-per-group manifests carry orthogonal node
//     selectors but downstream device plugins depend on each landing).
//  3. Remaining manifests — apply ALL in one pass without waiting.
//     Networks, IP pools, OVS configs, example DaemonSets and the
//     SR-IOV / Spectrum-X CRs all reconcile concurrently in the cluster,
//     so launching them back-to-back lets the wall-clock budget overlap.
//  4. Verify — poll the registry for every manifest applied in phase 3
//     until each reaches a terminal state (success/error) or the deploy
//     context is cancelled. Skipped in dry-run mode.
//
// Per-manifest deadlines have been removed: the only timeout that applies
// is whatever the caller threads into ctx (typically wrapped via
// context.WithTimeout for a maintenance-window-sized budget). When ctx
// has no deadline, the deploy waits indefinitely for reconciliation —
// which is the right default for SR-IOV configuration on large clusters,
// where a single policy can easily exceed any small per-manifest budget.
//
// When opts.DryRun is true the apply path uses server-side dry-run
// (client.DryRunAll) so the cluster validates manifests without
// persisting them; phase 4 is skipped entirely. Phase 0's helm install
// also runs in dry-run mode (action.Install.DryRun).
func ApplyManifestsFromDir(ctx context.Context, kubeClient client.Client, manifestsDir string, opts DeployOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}

	uiOutput := ui.FromContext(ctx)

	// Phase 0 — helm install/upgrade the network-operator chart when
	// values.yaml is present and the caller supplied the helm-install
	// metadata. Skipped silently otherwise so users managing the chart
	// out of band can keep using the standalone `l8k deploy`.
	if err := runHelmInstallPhase(ctx, manifestsDir, opts, uiOutput); err != nil {
		return err
	}

	// Phase 0.5 — preflight checks (chart version, values, stray CRs,
	// NCP component versions). Surfaces every mismatch in one pass.
	// Without --overwrite-existing: fail fast with all failures listed.
	// With it: log + remediate strays (helm + NCP drift are resolved by
	// the Phase 0 install and the Phase 1 SSA apply respectively).
	if err := runPreflightPhase(ctx, kubeClient, manifestsDir, opts, uiOutput); err != nil {
		return err
	}

	// Read & triage manifest docs from the deployment directory.
	nicDoc, nnpDocs, otherDocs, err := readManifestDir(manifestsDir)
	if err != nil {
		return err
	}

	dryRun := opts.DryRun

	// Pre-decode NCP / NNP so any YAML error surfaces before we start
	// touching the cluster. The "other" docs are decoded lazily inside
	// phase 3 so a Pod retry can re-use the same Unstructured object.
	var ncpObj *unstructured.Unstructured
	if len(nicDoc) != 0 {
		ncpObj, err = decodeUnstructured(nicDoc)
		if err != nil {
			return fmt.Errorf("decode NicClusterPolicy: %w", err)
		}
	}
	nnpObjs := make([]*unstructured.Unstructured, 0, len(nnpDocs))
	for i, b := range nnpDocs {
		obj, err := decodeUnstructured(b)
		if err != nil {
			return fmt.Errorf("decode NicNodePolicy manifest %d: %w", i+1, err)
		}
		nnpObjs = append(nnpObjs, obj)
	}

	registry := crstate.NewDefault()

	// Compute total phase count for the headers — skip empty phases so
	// users don't see "Phase 2/4" when there are no NNPs.
	phases := computePhases(ncpObj, nnpObjs, otherDocs, dryRun)
	uiOutput.Info("Deploying %d manifest(s): %d NicClusterPolicy, %d NicNodePolicy, %d other%s",
		phases.totalManifests, btoi(ncpObj != nil), len(nnpObjs), len(otherDocs), phases.dryRunSuffix)
	if deadline, ok := ctx.Deadline(); ok {
		uiOutput.Info("Deploy budget: %s (until %s)", time.Until(deadline).Round(time.Second), deadline.Format(time.RFC3339))
	} else {
		uiOutput.Info("Deploy budget: unbounded (no --deploy-timeout set)")
	}

	// Phase 1 — NicClusterPolicy.
	if ncpObj != nil {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — NicClusterPolicy", phases.next(), phases.total))
		if err := applyAndWait(ctx, kubeClient, registry, ncpObj, dryRun, manifestLabel(ncpObj, 0, 0)); err != nil {
			return err
		}
	}

	// Phase 2 — NicNodePolicies (sequential apply + wait per policy).
	if len(nnpObjs) > 0 {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — NicNodePolicies (%d)", phases.next(), phases.total, len(nnpObjs)))
		for i, obj := range nnpObjs {
			if err := applyAndWait(ctx, kubeClient, registry, obj, dryRun, manifestLabel(obj, i+1, len(nnpObjs))); err != nil {
				return err
			}
		}
	}

	// Phase 3 — apply remaining manifests, no per-manifest wait.
	// Per manifest we capture (a) the pre-apply generation so the
	// verify phase knows whether the spec actually changed, and
	// (b) the resourceVersion the server returned from our Patch
	// — phase 4 then uses the same observation gate as phase 1/2
	// (don't trust status until live RV moves past the apply's
	// RV when the spec was new).
	appliedOthers := make([]appliedManifest, 0, len(otherDocs))
	if len(otherDocs) > 0 {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — Applying %d additional manifest(s)", phases.next(), phases.total, len(otherDocs)))
		for i, b := range otherDocs {
			obj, err := decodeUnstructured(b)
			if err != nil {
				return fmt.Errorf("decode manifest: %w", err)
			}
			label := manifestLabel(obj, i+1, len(otherDocs))

			// Pre-apply Get: capture the existing object's
			// generation so we can tell post-apply whether
			// the spec changed. Failure = brand-new object;
			// preApplyGen stays 0 and post-apply gen >= 1
			// will trip the spec-changed branch.
			var preApplyGen int64
			pre := &unstructured.Unstructured{}
			pre.SetGroupVersionKind(obj.GroupVersionKind())
			if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, pre); err == nil {
				preApplyGen = pre.GetGeneration()
			}

			uiOutput.Info("Applying %s", label)
			log.Log.Info("Applying manifest",
				"kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(),
				"index", i+1, "total", len(otherDocs))
			if err := applyUnstructuredWithRetry(ctx, kubeClient, obj, dryRun); err != nil {
				uiOutput.Error("Failed to apply %s: %v", label, err)
				return err
			}

			am := appliedManifest{obj: obj}
			if obj.GetGeneration() > preApplyGen && crstate.NeedsObservationGate(obj.GroupVersionKind()) {
				am.awaitObservationAfterRV = obj.GetResourceVersion()
			}
			appliedOthers = append(appliedOthers, am)
		}
		uiOutput.Success("Applied %d additional manifest(s)", len(appliedOthers))
	}

	// Phase 4 — verify remaining manifests reach a terminal state.
	// Dry-run skips this; nothing was actually persisted.
	if dryRun {
		uiOutput.Info("Dry-run mode: skipping reconciliation verification")
		return nil
	}
	if len(appliedOthers) > 0 {
		uiOutput.Section(fmt.Sprintf("Phase %d/%d — Verifying %d manifest(s) reconcile", phases.next(), phases.total, len(appliedOthers)))
		for i, am := range appliedOthers {
			if err := pollUntilTerminal(ctx, kubeClient, registry, am.obj,
				manifestLabel(am.obj, i+1, len(appliedOthers)), am.awaitObservationAfterRV); err != nil {
				return err
			}
		}
		uiOutput.Success("All %d additional manifest(s) reconciled", len(appliedOthers))
	}

	return nil
}

// readManifestDir reads every YAML doc under manifestsDir (non-recursive)
// and triages them into the three deploy buckets. Files matching the
// example-manifest naming pattern (see isExampleManifest) are skipped —
// they're test fixtures consumed by `l8k validate --connectivity` to
// stand up a temporary ping-matrix DaemonSet, not part of the actual
// network-operator surface that `l8k deploy` should apply.
func readManifestDir(manifestsDir string) (nicDoc []byte, nnpDocs [][]byte, otherDocs [][]byte, err error) {
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return nil, nil, nil, err
	}
	filePaths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if isExampleManifest(e.Name()) {
			log.Log.V(1).Info("Skipping example manifest at deploy time", "file", e.Name())
			continue
		}
		// values.yaml is consumed by Phase 0 (helm install) — not a K8s
		// manifest, must not flow into Phase 1/2/3 apply.
		if e.Name() == helmValuesFile {
			log.Log.V(1).Info("Skipping helm values file at apply phase", "file", e.Name())
			continue
		}
		filePaths = append(filePaths, filepath.Join(manifestsDir, e.Name()))
	}
	sort.Strings(filePaths)

	for _, p := range filePaths {
		content, rErr := os.ReadFile(p)
		if rErr != nil {
			return nil, nil, nil, rErr
		}
		for _, doc := range splitYAMLDocuments(string(content)) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			b := []byte(doc)
			switch {
			case containsNicClusterPolicyKind(b):
				if len(nicDoc) != 0 {
					return nil, nil, nil, fmt.Errorf("multiple NicClusterPolicy manifests found; only one is allowed")
				}
				nicDoc = b
			case containsNicNodePolicyKind(b):
				nnpDocs = append(nnpDocs, b)
			default:
				otherDocs = append(otherDocs, b)
			}
		}
	}
	return nicDoc, nnpDocs, otherDocs, nil
}

// decodeUnstructured parses a YAML document into an Unstructured object
// and ensures its GroupVersionKind is set so server-side apply works.
func decodeUnstructured(doc []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(doc, obj); err != nil {
		return nil, err
	}
	if apiv, kind := obj.GetAPIVersion(), obj.GetKind(); apiv != "" && kind != "" {
		gv, err := schema.ParseGroupVersion(apiv)
		if err == nil {
			obj.SetGroupVersionKind(gv.WithKind(kind))
		}
	}
	return obj, nil
}

// applyAndWait applies obj and then polls until the registry reports a
// terminal state, re-applying once if the object goes missing mid-flight.
// Used for NCP and per-NNP in phases 1 and 2.
//
// Captures the object's pre-apply generation and the resourceVersion
// the server returned from the Patch. When the spec changed (generation
// bumped), the poll loop won't trust the validator's verdict until the
// controller has written *something* post-apply (live RV differs from
// the apply-time RV) — without this gate the controller's stale
// `status.state: ready` from the previous deploy would make the state
// machine declare success before reconciliation of the new spec had
// even started. Network Operator's NicClusterPolicyStatus carries no
// `observedGeneration`, so this resourceVersion-bump heuristic is the
// closest signal we have that the controller has reacted.
func applyAndWait(ctx context.Context, c client.Client, registry *crstate.Registry, obj *unstructured.Unstructured, dryRun bool, label string) error {
	uiOutput := ui.FromContext(ctx)

	// Pre-apply observation: capture the object's current
	// generation so we can detect whether the apply changed the
	// spec or was idempotent. Failure to read = treat as
	// "first-time apply" (preApplyGen=0).
	var preApplyGen int64
	pre := &unstructured.Unstructured{}
	pre.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, pre); err == nil {
		preApplyGen = pre.GetGeneration()
	}

	progress := uiOutput.StartProgress(fmt.Sprintf("Applying %s", label))
	log.Log.Info("Applying manifest", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
	if err := applyUnstructured(ctx, c, obj, dryRun); err != nil {
		progress.Fail(fmt.Sprintf("Failed to apply %s: %v", label, err))
		return err
	}
	progress.Success(fmt.Sprintf("Applied %s", label))

	if dryRun {
		return nil
	}

	// applyUnstructured does an SSA Patch that returns the
	// server-decided object on `obj`. Its current resourceVersion
	// is "the version right after our apply". If the spec changed
	// AND this Kind's validator reads .status from the CR itself
	// (vs from companion CRs), any subsequent live RV != this
	// value means the controller has written status since.
	awaitObservationAfterRV := ""
	if obj.GetGeneration() > preApplyGen && crstate.NeedsObservationGate(obj.GroupVersionKind()) {
		awaitObservationAfterRV = obj.GetResourceVersion()
		log.Log.V(1).Info("Spec changed; gating poll on controller observation",
			"kind", obj.GetKind(), "name", obj.GetName(),
			"preApplyGeneration", preApplyGen,
			"appliedGeneration", obj.GetGeneration(),
			"appliedResourceVersion", awaitObservationAfterRV)
	}
	return pollUntilTerminal(ctx, c, registry, obj, label, awaitObservationAfterRV)
}

// pollUntilTerminal polls the registry's Validator for obj until it
// reports StateSuccess or StateError. not-deployed transitions trigger a
// single re-apply (object vanished between apply and poll); the only
// exit condition besides terminal state is ctx.Done().
//
// awaitObservationAfterRV is the resourceVersion the server returned
// from the apply Patch. When non-empty, the poll loop refuses to act
// on any terminal verdict until the live object's resourceVersion
// has advanced past this value — that's the closest signal we have
// (without controller observedGeneration support) that the controller
// has written status since the apply landed. Avoids the deploy state
// machine declaring success on the previous reconcile's stale
// status.state=ready before the operator has even noticed the new
// spec.
//
// A spinner shows "Waiting for <label> to reconcile" so operators have
// a visible heartbeat. Reason transitions are emitted as discrete
// uiOutput.Info() lines — the StandardOutput now shares a mutex with
// the spinner goroutine and clears the spinner row before printing, so
// log lines land cleanly *above* the spinner instead of being glued
// onto the same row. The reason is deduped against the previous tick
// so a noisy 3-second polling loop only emits a fresh line when
// something actually changed.
func pollUntilTerminal(ctx context.Context, c client.Client, registry *crstate.Registry, obj *unstructured.Unstructured, label, awaitObservationAfterRV string) error {
	uiOutput := ui.FromContext(ctx)
	progress := uiOutput.StartProgress(fmt.Sprintf("Waiting for %s to reconcile", label))
	log.Log.Info("Waiting for manifest to reconcile", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())

	ticker := time.NewTicker(deployPollInterval)
	defer ticker.Stop()

	var lastReason string
	reportProgress := func(reason string) {
		if reason == lastReason || reason == "" {
			return
		}
		lastReason = reason
		// History line — scrollback record of every distinct
		// state transition. Always emitted.
		uiOutput.Info("  %s: %s", label, reason)
		log.Log.V(1).Info("manifest in-progress", "kind", obj.GetKind(), "name", obj.GetName(), "reason", reason)
		// Spinner-label update — TTY only. The spinner's paint
		// truncates the line to terminal width so long reasons
		// (e.g. "ready: 11/12; pending: state-OFED, …") render
		// on a single, in-place-redrawn line above which the
		// history scrolls. In non-TTY mode, progress.Update would
		// print a *duplicate* of the Info line above, so we skip
		// it here and rely on the history line alone.
		if uiOutput.IsTTY() {
			progress.Update(fmt.Sprintf("%s: %s", label, reason))
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			progress.Fail(fmt.Sprintf("Cancelled or timed out while waiting for %s", label))
			return err
		}

		// Observation gate: when the spec changed during apply,
		// hold off on trusting the validator until the controller
		// has written status post-apply. Detection: the live
		// resourceVersion differs from the one Patch returned.
		// Once we've seen the controller write, clear the gate
		// and proceed with normal polling for the rest of the
		// reconciliation window.
		if awaitObservationAfterRV != "" {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(obj.GroupVersionKind())
			err := c.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, live)
			switch {
			case err != nil:
				// Get failure — let the validator handle it
				// uniformly below.
			case live.GetResourceVersion() == awaitObservationAfterRV:
				reportProgress("waiting for controller to observe new spec")
				select {
				case <-ctx.Done():
					progress.Fail(fmt.Sprintf("Cancelled or timed out while waiting for %s", label))
					return ctx.Err()
				case <-ticker.C:
				}
				continue
			default:
				log.Log.V(1).Info("Controller observed apply; clearing gate",
					"kind", obj.GetKind(), "name", obj.GetName(),
					"appliedResourceVersion", awaitObservationAfterRV,
					"liveResourceVersion", live.GetResourceVersion())
				awaitObservationAfterRV = ""
			}
		}

		res, err := registry.Validate(ctx, c, obj)
		if err != nil {
			log.Log.V(1).Info("Validate transient failure",
				"kind", obj.GetKind(), "name", obj.GetName(), "error", err.Error())
			reportProgress(fmt.Sprintf("transient validate error: %v", err))
		} else {
			switch res.State {
			case crstate.StateSuccess:
				progress.Success(fmt.Sprintf("%s reconciled (%s)", label, res.Reason))
				log.Log.Info("Manifest reconciled",
					"kind", obj.GetKind(), "name", obj.GetName(), "reason", res.Reason)
				return nil
			case crstate.StateError:
				progress.Fail(fmt.Sprintf("%s error: %s", label, res.Reason))
				log.Log.Error(nil, "Manifest reported error",
					"kind", obj.GetKind(), "name", obj.GetName(), "reason", res.Reason)
				return fmt.Errorf("%s/%s: %s", obj.GetKind(), obj.GetName(), res.Reason)
			case crstate.StateNotDeployed:
				// Object vanished between apply and poll (admission
				// webhook race, manual kubectl delete). Re-apply once
				// and continue polling.
				uiOutput.Warning("%s went missing mid-flight; re-applying", label)
				log.Log.Info("Manifest reported not-deployed; re-applying",
					"kind", obj.GetKind(), "name", obj.GetName())
				if err := applyUnstructured(ctx, c, obj, false); err != nil {
					progress.Fail(fmt.Sprintf("Re-apply of %s failed: %v", label, err))
					return err
				}
				reportProgress("re-applied after disappearance")
			case crstate.StateInProgress:
				reportProgress(res.Reason)
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail(fmt.Sprintf("Cancelled or timed out while waiting for %s", label))
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// applyUnstructuredWithRetry wraps applyUnstructured with the legacy
// Pod-specific retry path (up to 3 attempts, 30s apart). Non-Pod kinds
// surface the first apply error unmodified.
func applyUnstructuredWithRetry(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dryRun bool) error {
	uiOutput := ui.FromContext(ctx)
	err := applyUnstructured(ctx, c, obj, dryRun)
	if err == nil || !strings.EqualFold(obj.GetKind(), "Pod") {
		return err
	}
	const maxAttempts = 3
	for attempt := 2; attempt <= maxAttempts && err != nil; attempt++ {
		uiOutput.Warning("    Retrying %s/%s (%d/%d)...", obj.GetKind(), obj.GetName(), attempt, maxAttempts)
		log.Log.Info("Pod apply failed, retrying",
			"name", obj.GetName(), "attempt", attempt, "delay", "30s", "error", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
		err = applyUnstructured(ctx, c, obj, dryRun)
	}
	return err
}

// phaseCounter tracks the active phase number for human-readable
// "Phase X/Y" headers, skipping phases with no manifests.
type phaseCounter struct {
	total          int
	current        int
	totalManifests int
	dryRunSuffix   string
}

func (p *phaseCounter) next() int {
	p.current++
	return p.current
}

func computePhases(ncp *unstructured.Unstructured, nnps []*unstructured.Unstructured, others [][]byte, dryRun bool) *phaseCounter {
	pc := &phaseCounter{}
	if ncp != nil {
		pc.total++
		pc.totalManifests++
	}
	if len(nnps) > 0 {
		pc.total++
		pc.totalManifests += len(nnps)
	}
	if len(others) > 0 {
		pc.total++ // apply phase
		pc.totalManifests += len(others)
		if !dryRun {
			pc.total++ // verify phase
		}
	}
	if dryRun {
		pc.dryRunSuffix = " (dry-run)"
	}
	return pc
}

// manifestLabel formats a manifest identifier for log lines. When n > 0,
// "[i/n]" is appended so users can see batch progress at a glance.
func manifestLabel(obj *unstructured.Unstructured, i, n int) string {
	kindName := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())
	if ns := obj.GetNamespace(); ns != "" {
		kindName = fmt.Sprintf("%s/%s in %s", obj.GetKind(), obj.GetName(), ns)
	}
	if n > 0 {
		return fmt.Sprintf("%s [%d/%d]", kindName, i, n)
	}
	return kindName
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func containsNicClusterPolicyKind(b []byte) bool {
	return sniffKind(b) == "NicClusterPolicy"
}

func containsNicNodePolicyKind(b []byte) bool {
	return sniffKind(b) == "NicNodePolicy"
}

// sniffKind extracts the Kind field from a YAML document without full parsing.
func sniffKind(b []byte) string {
	type metaOnly struct {
		Kind string `yaml:"kind"`
	}
	var mo metaOnly
	if err := yaml.Unmarshal(b, &mo); err != nil {
		return ""
	}
	return mo.Kind
}

func applyUnstructured(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dryRun bool) error {
	// kubectl-style server-side apply. dryRun appends client.DryRunAll so the
	// cluster validates the object without persisting it.
	opts := []client.PatchOption{client.FieldOwner("l8k"), client.ForceOwnership}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	return c.Patch(ctx, obj, client.Apply, opts...)
}

// splitYAMLDocuments splits a YAML stream by lines that start with '---' (doc separators)
func splitYAMLDocuments(s string) []string {
	var docs []string
	var cur []string
	lines := strings.Split(s, "\n")
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "---") {
			if len(cur) > 0 {
				docs = append(docs, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, ln)
	}
	if len(cur) > 0 {
		docs = append(docs, strings.Join(cur, "\n"))
	}
	return docs
}

// runHelmInstallPhase performs Phase 0: install (or upgrade) the
// network-operator helm chart from values.yaml in the deployment dir. The
// phase is a no-op when:
//   - values.yaml is absent (user manages the chart out of band),
//   - opts.RestConfig is nil (no cluster wiring available),
//   - opts.NetworkOperator is nil or HelmRepoURL/Version are empty (no
//     catalog metadata to drive the install).
//
// A value-conflict against an existing release surfaces as a
// DeploymentError pointing at --overwrite-existing.
func runHelmInstallPhase(ctx context.Context, manifestsDir string, opts DeployOptions, uiOutput ui.Output) error {
	valuesPath := filepath.Join(manifestsDir, helmValuesFile)
	valuesYAML, err := os.ReadFile(valuesPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Log.V(1).Info("No helm values file found; skipping operator install",
				"path", valuesPath)
			return nil
		}
		return fmt.Errorf("read helm values file %s: %w", valuesPath, err)
	}

	if opts.RestConfig == nil || opts.NetworkOperator == nil ||
		opts.NetworkOperator.HelmRepoURL == "" || opts.NetworkOperator.Version == "" {
		uiOutput.Warning("values.yaml is present but helm-install metadata is missing — skipping operator install.")
		uiOutput.Info("To install the network-operator chart, re-run with `l8k generate --deploy` (or pass --network-operator-release).")
		log.Log.Info("Skipping helm install phase",
			"hasRestConfig", opts.RestConfig != nil,
			"hasNetworkOperator", opts.NetworkOperator != nil)
		return nil
	}

	uiOutput.Section("Phase 0 — Helm install (network-operator chart)")
	uiOutput.Info("Installing network-operator chart from %s (version %s)",
		opts.NetworkOperator.HelmRepoURL, strings.TrimPrefix(opts.NetworkOperator.Version, "v"))

	timeout := opts.HelmTimeout
	if timeout == 0 {
		timeout = defaultHelmInstallTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	err = InstallOrUpgrade(ctx, opts.RestConfig, opts.NetworkOperator, valuesYAML, opts.OverwriteExisting, timeout, opts.DryRun)
	if err == nil {
		if opts.DryRun {
			uiOutput.Success("Dry-run: helm install would create network-operator release in namespace %s",
				opts.NetworkOperator.Namespace)
		} else {
			uiOutput.Success("network-operator release ready in namespace %s", opts.NetworkOperator.Namespace)
		}
		return nil
	}
	if errors.Is(err, ErrReleaseStuckInPendingState) {
		return pkgerrors.NewDeploymentError(
			fmt.Sprintf("helm release %q in namespace %s is stuck in a pending state (a previous install/upgrade/rollback was interrupted)",
				"network-operator", opts.NetworkOperator.Namespace),
			nil,
			fmt.Sprintf("Unstick the release before re-running deploy. If a previous deployed revision exists: `helm rollback network-operator -n %[1]s`. Otherwise: `helm uninstall network-operator -n %[1]s`. Both commands require the helm CLI.",
				opts.NetworkOperator.Namespace),
		)
	}
	if errors.Is(err, ErrReleaseExistsWithDifferentValues) {
		// nil cause: the sentinel's message duplicates the user-facing
		// Message text we just wrote, and StructuredError.Error()
		// would append it as redundant tail noise. Callers don't
		// `errors.Is` against the sentinel through this wrap.
		return pkgerrors.NewDeploymentError(
			fmt.Sprintf("helm release %q in namespace %s has different values than the rendered values.yaml",
				"network-operator", opts.NetworkOperator.Namespace),
			nil,
			"Re-run with --overwrite-existing to upgrade the release to the new values, or align cluster-config.yaml with the deployed release.",
		)
	}
	if errors.Is(err, ErrReleaseExistsWithDifferentChartVersion) {
		return pkgerrors.NewDeploymentError(
			fmt.Sprintf("helm release %q in namespace %s is at a different chart version than %s",
				"network-operator", opts.NetworkOperator.Namespace, strings.TrimPrefix(opts.NetworkOperator.Version, "v")),
			nil,
			"Re-run with --overwrite-existing to upgrade the chart, or set --network-operator-release to the currently deployed version.",
		)
	}
	return err
}

// runPreflightPhase runs the preflight checks (chart version, values, NCP
// component versions, stray CRs) and either fails fast or remediates strays.
//
//   - Without --overwrite-existing, any non-skipped failure aborts the deploy
//     with a structured error listing every failed check. The user sees ALL
//     mismatches in one pass instead of fixing them one re-run at a time.
//   - With --overwrite-existing, the helm phase (already run above) takes
//     care of chart-version + values drift, the Phase 1 NCP/NNP apply with
//     ForceOwnership replaces component versions, and we delete every
//     stray CR here so the cluster state matches what l8k just rendered.
//
// Phase 0.5 is a no-op when no preflight check is actionable — typically a
// standalone `l8k deploy` with no l8k-config.yaml and no helm release in
// the namespace yet.
func runPreflightPhase(ctx context.Context, kubeClient client.Client, manifestsDir string, opts DeployOptions, uiOutput ui.Output) error {
	in, err := buildPreflightInputs(kubeClient, manifestsDir, opts)
	if err != nil {
		return err
	}

	results := preflight.RunAll(ctx, in)
	failedCount := 0
	for _, r := range results {
		switch {
		case r.Skipped:
			log.Log.V(1).Info("preflight check skipped", "code", r.Code, "reason", r.Reason)
		case r.Failed():
			failedCount++
			log.Log.Info("preflight check failed", "code", r.Code, "reason", r.Reason, "mismatches", len(r.Mismatches))
		default:
			log.Log.V(1).Info("preflight check passed", "code", r.Code)
		}
	}
	if failedCount == 0 {
		return nil
	}

	uiOutput.Section(fmt.Sprintf("Phase 0.5 — Preflight checks (%d issue(s) found)", failedCount))
	for _, r := range results {
		if !r.Failed() {
			continue
		}
		uiOutput.Warning("  %s — %s", r.Name, r.Reason)
		for _, m := range r.Mismatches {
			uiOutput.Info("    • %s", m.String())
		}
	}

	if !opts.OverwriteExisting {
		return pkgerrors.NewDeploymentError(
			fmt.Sprintf("preflight found %d issue(s): %s",
				failedCount, strings.Join(preflight.FailedNames(results), "; ")),
			nil,
			"Re-run with --overwrite-existing to converge: upgrade the helm release, delete the conflicting Network Operator resources, and rewrite mismatched NicClusterPolicy fields. Or address each issue individually and re-run.",
		)
	}

	// Remediate strays. Chart-version / values drift was already handled
	// by Phase 0's `helm upgrade --install`; NCP component versions get
	// rewritten by Phase 1's SSA + ForceOwnership.
	if err := preflight.Remediate(ctx, in, results, preflight.RemediationOptions{DryRun: opts.DryRun}); err != nil {
		return fmt.Errorf("preflight remediation: %w", err)
	}
	uiOutput.Success("Preflight remediation applied")
	return nil
}

// buildPreflightInputs assembles the Inputs the four checks need from the
// deploy-level opts + manifestsDir. Unresolvable fields are left empty —
// individual checks soft-skip when an input is missing.
func buildPreflightInputs(kubeClient client.Client, manifestsDir string, opts DeployOptions) (preflight.Inputs, error) {
	in := preflight.Inputs{
		KubeClient: kubeClient,
		RestConfig: opts.RestConfig,
	}
	if opts.NetworkOperator != nil {
		in.OperatorNamespace = opts.NetworkOperator.Namespace
		in.SelectedRelease = opts.NetworkOperator.SelectedRelease
		in.ExpectedAppVersion = opts.NetworkOperator.Version
		in.ExpectedComponentVersion = opts.NetworkOperator.ComponentVersion
		in.ExpectedChartVersion = strings.TrimPrefix(opts.NetworkOperator.Version, "v")
	}
	if opts.DOCAVersion != "" {
		in.ExpectedDOCAVersion = opts.DOCAVersion
	}

	// Best-effort: read values.yaml (helm checks soft-skip if absent).
	if b, err := os.ReadFile(filepath.Join(manifestsDir, helmValuesFile)); err == nil {
		in.GeneratedValuesYAML = b
	}

	// Best-effort: scan manifests dir for rendered object refs (stray
	// check needs this — an unreadable dir is a hard error since the
	// rest of deploy wouldn't survive it either).
	refs, err := preflight.ScanGeneratedManifests(manifestsDir)
	if err != nil {
		return in, fmt.Errorf("scan generated manifests for preflight: %w", err)
	}
	in.GeneratedManifests = refs
	return in, nil
}


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

package job

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"time"

	v1 "github.com/NVIDIA/aicr/pkg/api/validator/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

// Deployer manages the lifecycle of a single validator Job.
type Deployer struct {
	clientset             kubernetes.Interface
	factory               informers.SharedInformerFactory
	namespace             string
	runID                 string
	cliVersion            string // CLI version — forwarded to validator containers via AICR_CLI_VERSION for inner-image resolution
	cliCommit             string // CLI commit SHA — forwarded via AICR_CLI_COMMIT for dev-build image resolution
	entry                 catalog.ValidatorEntry
	jobName               string // Unique name generated client-side (set by DeployJob)
	imagePullSecrets      []string
	tolerations           []corev1.Toleration
	nodeSelector          map[string]string     // passed through to inner workloads via AICR_NODE_SELECTOR env var
	imageRegistryOverride string                // overrides the image registry prefix for validator containers
	imageTagOverride      string                // overrides the resolved image tag for validator containers
	componentRefs         []recipe.ComponentRef // resolved recipe components, used for dependencyAffinity resolution
}

// NewDeployer creates a Deployer for a single validator catalog entry.
// The factory must be a namespace-scoped SharedInformerFactory started by the caller.
// cliVersion is the CLI's own version string; empty is acceptable for dev builds
// and is forwarded to the validator container via the AICR_CLI_VERSION env var so
// the validator can resolve images it references outside the catalog (e.g. the
// AIPerf benchmark image used by inference-perf) using the same rewriting
// rules as catalog.Load. cliCommit is the git commit SHA, forwarded via
// AICR_CLI_COMMIT for SHA-based image tag resolution in dev builds.
func NewDeployer(
	clientset kubernetes.Interface,
	factory informers.SharedInformerFactory,
	namespace, runID, cliVersion, cliCommit string,
	entry catalog.ValidatorEntry,
	imagePullSecrets []string,
	tolerations []corev1.Toleration,
	nodeSelector map[string]string,
	imageRegistryOverride string,
	imageTagOverride string,
	componentRefs []recipe.ComponentRef,
) *Deployer {

	return &Deployer{
		clientset:             clientset,
		factory:               factory,
		namespace:             namespace,
		runID:                 runID,
		cliVersion:            cliVersion,
		cliCommit:             cliCommit,
		entry:                 entry,
		imagePullSecrets:      imagePullSecrets,
		tolerations:           tolerations,
		nodeSelector:          nodeSelector,
		imageRegistryOverride: imageRegistryOverride,
		imageTagOverride:      imageTagOverride,
		componentRefs:         componentRefs,
	}
}

// JobName returns the Kubernetes Job name assigned by the API server.
// Empty until DeployJob is called.
func (d *Deployer) JobName() string {
	return d.jobName
}

// DeployJob creates the validator Job using server-side apply.
// A unique name is generated and the Job is applied with the aicr-validator field manager.
func (d *Deployer) DeployJob(ctx context.Context) error {
	// Build JobPlan from deployer configuration
	plan, err := v1.BuildJobPlan(
		d.entry,
		d.runID,
		d.namespace,
		d.cliVersion,
		d.cliCommit,
		ServiceAccountName(d.runID),
		d.imagePullSecrets,
		d.tolerations,
		d.nodeSelector,
		d.imageRegistryOverride,
		d.imageTagOverride,
		d.componentRefs,
	)
	if err != nil {
		return err
	}

	// Use the job name from the plan
	d.jobName = plan.JobName

	// Best-effort: warn if any dependencyAffinity selector matches zero
	// pods at deploy time. Catches silent label drift on dependency-chart
	// bumps (e.g., kube-prometheus-stack relabels its Prometheus pods).
	// Logging only — we don't block deploy, because the scheduler will
	// surface a hard match miss as Pending if the affinity is `required`.
	if plan.Affinity != nil && plan.Affinity.PodAffinity != nil {
		for _, w := range scanMissingPodAffinityDeps(ctx, d.clientset, plan.Affinity.PodAffinity) {
			attrs := []any{
				"validator", d.entry.Name,
				"namespace", w.Namespace,
				"selector", w.Selector,
				"reason", w.Reason,
			}
			if w.Err != nil {
				attrs = append(attrs, "error", w.Err)
			}
			slog.Warn(w.Message, attrs...)
		}
	}

	// Render Job ApplyConfiguration from plan
	jobApply := v1.RenderPlanToApplyConfig(plan, plan.JobName)

	// Apply the Job with server-side apply
	applied, err := d.clientset.BatchV1().Jobs(d.namespace).Apply(
		ctx, jobApply, metav1.ApplyOptions{FieldManager: labels.ValueAICR, Force: true})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to apply Job %s", plan.JobName), err)
	}

	d.jobName = applied.Name

	slog.Debug("validator Job applied",
		"job", d.jobName,
		"validator", d.entry.Name,
		"namespace", d.namespace)

	return nil
}

// Stable reason codes for affinityScanWarning, suitable for log filtering
// and metric labels. Kept short and ASCII so they round-trip through any
// downstream sink without escaping.
const (
	affinityScanReasonZeroMatch         = "zero-match"
	affinityScanReasonLookupFailed      = "lookup-failed"
	affinityScanReasonMalformedSelector = "malformed-selector"
)

// affinityScanWarning is a structured record of a single dependencyAffinity
// scan finding. Returned by scanMissingPodAffinityDeps so callers can emit
// stable-message slog records with queryable attrs rather than embedding
// namespace/selector inside the log message text.
type affinityScanWarning struct {
	// Message is the human-readable slog message. Stable across invocations
	// so log aggregators can group by msg.
	Message string
	// Namespace is the namespace that was Listed. Empty when the warning is
	// not namespace-scoped (e.g., a malformed selector skipped before any
	// List call).
	Namespace string
	// Selector is the best-effort string form of the term's LabelSelector.
	// Always populated (may be empty when the selector itself was malformed
	// before formatting succeeded).
	Selector string
	// Reason is one of the affinityScanReason* constants.
	Reason string
	// Err is the underlying error for lookup-failed / malformed-selector
	// reasons; nil for zero-match.
	Err error
}

// scanMissingPodAffinityDeps lists pods for each PodAffinityTerm in pa and
// returns one affinityScanWarning per term whose selector matched zero pods
// in the listed namespace at deploy time, plus warnings for malformed
// selectors and List errors. The returned slice is nil when every term
// matches at least one pod or when pa has no terms.
//
// This is a defensive check against silent dependency-chart label drift —
// if e.g. kube-prometheus-stack changes its pod labels in a future chart
// release, the affinity term becomes a no-op (preferred) or blocks scheduling
// (required) without an actionable error. The warning is intended for log
// grep / triage. We do NOT fail closed because:
//   - preferred affinity is best-effort by design; the orchestrator schedules
//     wherever the scheduler picks if no match exists,
//   - required affinity already surfaces a mismatch as Pending via the
//     scheduler, which is more authoritative than this point-in-time list.
//
// Each List uses a short per-namespace timeout (defaults.PodAffinitySelectorLookupTimeout)
// so a slow or unreachable apiserver doesn't delay Job deploy.
//
// Intentional coverage gaps (silent skips):
//   - term.NamespaceSelector is not resolved. AICR's BuildOrchestratorAffinity
//     always populates Namespaces from the resolved component ref's namespace,
//     so this case is unreachable from the catalog path. External callers
//     populating NamespaceSelector instead of Namespaces will get no scan
//     coverage for those terms.
//   - term.LabelSelector == nil is skipped without warning. Same reason —
//     AICR always sets it; this guard is defense-in-depth against a malformed
//     externally-built PodAffinity reaching the scanner.
func scanMissingPodAffinityDeps(ctx context.Context, client kubernetes.Interface, pa *corev1.PodAffinity) []affinityScanWarning {
	if pa == nil {
		return nil
	}

	terms := make([]corev1.PodAffinityTerm, 0,
		len(pa.RequiredDuringSchedulingIgnoredDuringExecution)+
			len(pa.PreferredDuringSchedulingIgnoredDuringExecution))
	terms = append(terms, pa.RequiredDuringSchedulingIgnoredDuringExecution...)
	for _, w := range pa.PreferredDuringSchedulingIgnoredDuringExecution {
		terms = append(terms, w.PodAffinityTerm)
	}

	var warnings []affinityScanWarning
	for _, term := range terms {
		// Stop early on cancellation. Continuing here would only emit a
		// "selector lookup failed; error=context canceled" warning per
		// remaining (term, namespace) pair, turning one cancellation into
		// N misleading apiserver-flake warnings.
		if ctx.Err() != nil {
			return warnings
		}
		// See coverage-gap notes on the function doc: terms with nil
		// LabelSelector or empty Namespaces are intentionally not scanned.
		if term.LabelSelector == nil || len(term.Namespaces) == 0 {
			continue
		}
		// Use LabelSelectorAsSelector (not FormatLabelSelector, which is a
		// display helper that returns "<error>" / "<none>" sentinel strings
		// the apiserver then rejects as malformed). This produces a string
		// that round-trips through labels.Parse and is accepted as
		// ListOptions.LabelSelector.
		sel, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			warnings = append(warnings, affinityScanWarning{
				Message: "dependencyAffinity selector is malformed; skipping lookup",
				Reason:  affinityScanReasonMalformedSelector,
				Err:     err,
			})
			continue
		}
		selector := sel.String()
		for _, ns := range term.Namespaces {
			if ctx.Err() != nil {
				return warnings
			}
			listCtx, cancel := context.WithTimeout(ctx, defaults.PodAffinitySelectorLookupTimeout)
			pods, err := client.CoreV1().Pods(ns).List(listCtx, metav1.ListOptions{
				LabelSelector: selector,
				Limit:         1,
			})
			cancel()
			if err != nil {
				warnings = append(warnings, affinityScanWarning{
					Message:   "dependencyAffinity selector lookup failed; affinity may not behave as intended",
					Namespace: ns,
					Selector:  selector,
					Reason:    affinityScanReasonLookupFailed,
					Err:       err,
				})
				continue
			}
			if len(pods.Items) == 0 {
				warnings = append(warnings, affinityScanWarning{
					Message:   "dependencyAffinity selector matched zero pods at deploy time; affinity term will be a no-op until matching pods appear",
					Namespace: ns,
					Selector:  selector,
					Reason:    affinityScanReasonZeroMatch,
				})
			}
		}
	}
	return warnings
}

// CleanupJob deletes the validator Job with foreground propagation
// (waits for pod deletion).
func (d *Deployer) CleanupJob(ctx context.Context) error {
	if d.jobName == "" {
		return nil
	}
	propagation := metav1.DeletePropagationForeground
	err := d.clientset.BatchV1().Jobs(d.namespace).Delete(ctx, d.jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	return k8s.IgnoreNotFound(err)
}

// WaitForCompletion watches the Job until it reaches a terminal state
// (Complete or Failed). Returns nil for both — the caller uses ExtractResult
// to determine pass/fail/skip from the exit code.
//
// Returns error only for infrastructure failures (watch error, timeout).
// Job failure (exit != 0) is NOT an error return — that decision lives here
// in the validator orchestrator, not in the shared pod.WaitForJobTerminal
// helper, which intentionally treats both Complete and Failed Jobs as
// legitimate completions and lets the caller classify them.
func (d *Deployer) WaitForCompletion(ctx context.Context, timeout time.Duration) error {
	waitTimeout := timeout + defaults.ValidatorWaitBuffer
	// pod.WaitForJobTerminal already returns structured errors with proper
	// codes (ErrCodeTimeout, ErrCodeUnavailable, ErrCodeInternal). Propagate
	// as-is so callers can distinguish retryable from terminal failures.
	if _, err := pod.WaitForJobTerminal(ctx, d.clientset, d.namespace, d.jobName, waitTimeout); err != nil {
		return err
	}
	return nil
}

// WaitForPodTermination watches the Job's pod until it reaches a terminal
// state. Prevents RBAC cleanup from racing with in-progress pod operations.
//
// Returns the underlying error from pod.WaitForTermination so callers can
// decide log severity. A nil error means the pod is gone or terminal; a
// non-nil error means the wait was abandoned (timeout, watch failure, or
// repeated watch closures) and the cleanup may race with an in-progress pod.
func (d *Deployer) WaitForPodTermination(ctx context.Context) error {
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		// Pod-not-found is the expected steady state once the Job's TTL
		// controller or foreground-propagation delete has already run.
		// Anything else (RBAC, transient API failure, timeout) must
		// propagate so the caller can decide whether to retry or escalate.
		var sErr *errors.StructuredError
		if stderrors.As(err, &sErr) && sErr.Code == errors.ErrCodeNotFound {
			slog.Debug("no pod found, skipping termination wait", "job", d.jobName)
			return nil
		}
		return err
	}

	if jobPod.Status.Phase == corev1.PodSucceeded || jobPod.Status.Phase == corev1.PodFailed {
		return nil
	}

	slog.Debug("waiting for pod termination", "pod", jobPod.Name)
	return pod.WaitForTermination(ctx, d.clientset, d.namespace, jobPod.Name)
}

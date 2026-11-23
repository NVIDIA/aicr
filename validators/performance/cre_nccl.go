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
	"fmt"
	"log/slog"
	"strings"

	"aicr.run/pkg/defaults"
	aicrErrors "aicr.run/pkg/errors"
	k8spod "aicr.run/pkg/k8s/pod"
	"aicr.run/pkg/recipe"
	"aicr.run/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// creNCCLCheckNames are the CRE-driven NCCL checks. They share one
// implementation and differ only in the qualification entry each reads.
var creNCCLCheckNames = []string{checkNameCRENCCLAllReduceBW, checkNameCRENCCLAllReduceBWNET}

func checkCRENCCLAllReduceBW(ctx *validators.Context) error {
	return checkCRENCCLAllReduce(ctx, checkNameCRENCCLAllReduceBW)
}

func checkCRENCCLAllReduceBWNET(ctx *validators.Context) error {
	return checkCRENCCLAllReduce(ctx, checkNameCRENCCLAllReduceBWNET)
}

func checkCRENCCLAllReduce(ctx *validators.Context, checkName string) error {
	constraint, found := findPerformanceConstraint(ctx, checkName)
	if !found {
		return validators.SkipWithReason(validators.SkipReasonNoConstraint, fmt.Sprintf("no %s constraint in recipe", checkName))
	}
	actual, passed, err := validateCRENcclAllReduceBw(ctx, checkName, constraint)
	return classifyNCCLAllReduceBWResult(checkName, constraint, actual, passed, err)
}

func validateCRENcclAllReduceBw(
	ctx *validators.Context,
	checkName string,
	constraint recipe.Constraint,
) (actual string, passed bool, err error) {

	if ctx.ValidationInput == nil {
		return ncclSkip(skipMsgNCCLNoInput, validators.SkipReasonNoValidationInput)
	}
	service := ctx.ValidationInput.Criteria.Service
	accelerator := ctx.ValidationInput.Criteria.Accelerator
	// Before the qualification lookup, so a constraint the check cannot honor
	// is reported rather than hidden behind a skip on an unqualified
	// combination. It needs no cluster access, and the docs promise it fails.
	threshold, err := creThreshold(checkName, constraint.Value)
	if err != nil {
		return "", false, err
	}

	entry, qualified := lookupCREQualification(checkName, service, accelerator)
	if !qualified {
		return ncclSkip(fmt.Sprintf("skipped - %s is not qualified on %s × %s", checkName, service, accelerator),
			validators.SkipReasonUnsupportedTarget)
	}

	gpuConfig, err := determineGPUConfig(ctx, service, accelerator, ctx.NodeSelector)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine GPU configuration", err)
	}
	if len(gpuConfig.Nodes) < 2 {
		return skipMsgNCCLFewNodes, true, nil
	}

	dyn := ctx.DynamicClient
	if dyn == nil {
		return "", false, aicrErrors.New(aicrErrors.ErrCodeInternal, "dynamic client is required to create a Certification")
	}

	objName, err := uniqueCREResourceName(creNCCLRunName)
	if err != nil {
		return "", false, err
	}
	// Built before anything is created, so a refused footprint returns without
	// a Certification to clean up.
	obj, err := buildCRECertification(ctx.Namespace, objName, gpuConfig, entry)
	if err != nil {
		return "", false, err
	}

	if reclaimErr := reclaimStaleCRECertifications(ctx.Ctx, dyn, ctx.Clientset, ctx.Namespace); reclaimErr != nil {
		return "", false, reclaimErr
	}

	defer func() {
		actual, passed, err = creNCCLCleanupResult(checkName, actual, passed, err,
			teardownCRECertification(context.Background(), dyn, ctx.Clientset, ctx.Namespace, objName))
	}()

	if createErr := createUnstructured(ctx.Ctx, dyn, certificationGVR, ctx.Namespace, obj); createErr != nil {
		return "", false, createErr
	}

	run, err := waitForCertificationTerminal(ctx.Ctx, dyn, ctx.Namespace, objName)
	if err != nil {
		return "", false, creErrorWithPartialReport(err, run)
	}
	if unstructuredConditionTrue(run, "Failed") {
		// Teardown deletes the Certification, so CRE's reason is only
		// recoverable from the error this returns.
		return "", false, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("CRE Certification failed: %s", creTerminalConditionSummary(run)))
	}

	workflowName, err := certificationWorkflowName(run, entry.Domain, entry.Variant)
	if err != nil {
		return "", false, err
	}
	return judgeCRENCCLRun(ctx, dyn, workflowName, run.GetCreationTimestamp(), threshold)
}

// judgeCRENCCLRun reads a completed run's final measurements, asserts the
// transport that carried it, and judges its peak bus bandwidth against
// threshold.
func judgeCRENCCLRun(
	ctx *validators.Context,
	dyn dynamic.Interface,
	workflowName string,
	createdAt metav1.Time,
	threshold float64,
) (actual string, passed bool, err error) {

	reading, err := readCREBandwidth(ctx.Ctx, dyn, ctx.Namespace, workflowName, createdAt)
	if err != nil {
		return "", false, err
	}
	emitCREBandwidthExtras(reading.MaxBusGBps, threshold)

	logs, logErr := creLauncherLogs(ctx, workflowName, createdAt)
	evidence, err := verifyCRETransport(logs, logErr, reading.Transports)
	if err != nil {
		return "", false, err
	}
	fmt.Printf("NCCL transport asserted from %s: %s\n", evidence.Source, strings.Join(evidence.Transports, ", "))

	actual, passed = creNCCLVerdict(reading.MaxBusGBps, threshold)
	return actual, passed, nil
}

// creTransportEvidence is the transport a run's assertion accepted and the
// record it was read from.
type creTransportEvidence struct {
	Source     string
	Transports []string
}

const (
	creTransportSourceLauncherLog = "launcher log"
	creTransportSourceMeasurement = "BandwidthMeasurement status.transport"
)

// verifyCRETransport asserts that a network transport, not Socket, carried the
// run. The launcher log's "Using network" banner is the authority when it is
// there. It is printed once at initialization, so kubelet rotating a long
// launcher log, as the TCPXO teardown output does on GKE, can drop it from the
// one segment the logs API returns. That only reaches this assertion when
// rotation keeps the results table: when the table goes too, CRE completes the
// measurement NoDataCollected and readCREBandwidth fails first (#3590).
//
// When the banner is missing, or the log cannot be read, the assertion falls
// back to status.transport, which CRE records from the same banner only while
// the nccl-bandwidth LogProfile defines networkTransport; a profile without
// that pattern leaves nothing recorded, and the run fails as it would without
// the fallback. Nothing recorded is a failure, and on the fallback a recorded
// Socket is always a failure.
func verifyCRETransport(logs string, logErr error, recorded []string) (creTransportEvidence, error) {
	if logErr == nil {
		if m := ncclUsingNetRe.FindStringSubmatch(logs); m != nil {
			if err := verifyTransportFromLogs(logs, variantNET); err != nil {
				return creTransportEvidence{}, err
			}
			return creTransportEvidence{Source: creTransportSourceLauncherLog, Transports: []string{m[1]}}, nil
		}
	}
	if len(recorded) == 0 {
		if logErr != nil {
			return creTransportEvidence{}, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
				"CRE launcher logs required for transport assertion", logErr)
		}
		return creTransportEvidence{}, verifyTransportFromLogs(logs, variantNET)
	}
	for _, name := range recorded {
		if name == "Socket" {
			return creTransportEvidence{}, aicrErrors.New(aicrErrors.ErrCodeInternal,
				"NET variant selected but CRE recorded 'Using network Socket' — "+
					"provider plugin (e.g. AWS Libfabric) did not load")
		}
	}
	if logErr != nil {
		slog.Warn("CRE launcher log unreadable; transport asserted from BandwidthMeasurement status",
			"error", logErr, "transport", recorded)
	} else {
		slog.Info("CRE launcher log has no network banner; transport asserted from BandwidthMeasurement status",
			"transport", recorded)
	}
	return creTransportEvidence{Source: creTransportSourceMeasurement, Transports: recorded}, nil
}

// creNCCLVerdict judges bw with the verdict the TrainJob check uses, so one
// recipe threshold means the same under both backends.
func creNCCLVerdict(bw, threshold float64) (actual string, passed bool) {
	return ncclBandwidthVerdict(bw, threshold, "")
}

// emitCREBandwidthExtras publishes the result extras the TrainJob check
// publishes for bandwidth. It runs before the transport assertion, so a run
// that falls back to Socket still records what it measured against what.
func emitCREBandwidthExtras(bw, threshold float64) {
	validators.EmitExtraOrWarn(ncclThresholdExtra(threshold))
	validators.EmitExtraOrWarn(ncclBandwidthExtra(bw, threshold))
}

// creNCCLCleanupResult folds a teardown failure into the check's result.
//
// A leak must not hide behind a passing bandwidth reading, nor behind a check
// that already failed for its own reason: either way the nodes may still be
// busy and the operator has to be told which objects survived.
// creCleanupFailure keeps the primary error's code and its Unwrap chain, so
// only the message grows.
//
// Split out of the deferred call so a test can reach the case where both the
// check and its teardown failed.
func creNCCLCleanupResult(checkName, actual string, passed bool, primary, delErr error) (string, bool, error) {
	if delErr == nil {
		return actual, passed, primary
	}
	return "", false, creCleanupFailure(checkName, primary, delErr)
}

func creLauncherLogs(ctx *validators.Context, workflowName string, createdAt metav1.Time) (string, error) {
	listCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	pods, err := ctx.Clientset.CoreV1().Pods(ctx.Namespace).List(listCtx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf(
			"jobset.sigs.k8s.io/jobset-name=%s-job-workload,jobset.sigs.k8s.io/replicatedjob-name=launcher",
			workflowName,
		),
	})
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list CRE launcher pods", err)
	}
	if len(pods.Items) == 0 {
		return "", aicrErrors.New(aicrErrors.ErrCodeNotFound, "no CRE launcher pods found for transport assertion")
	}
	pod := youngestLivePodSince(pods.Items, createdAt)
	if pod == nil {
		return "", aicrErrors.New(aicrErrors.ErrCodeNotFound, "no live CRE launcher pods found for transport assertion")
	}
	return k8spod.GetPodLogs(listCtx, ctx.Clientset, ctx.Namespace, pod.Name, nodeJobName)
}

func youngestLivePodSince(pods []corev1.Pod, createdAt metav1.Time) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed ||
			p.CreationTimestamp.Time.Before(createdAt.Time) {

			continue
		}

		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}

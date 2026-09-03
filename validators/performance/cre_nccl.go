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
	"strconv"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	k8spod "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func checkCRENCCLAllReduceBW(ctx *validators.Context) error {
	constraint, found := findPerformanceConstraint(ctx, checkNameCRENCCLAllReduceBW)
	if !found {
		return validators.Skip(fmt.Sprintf("no %s constraint in recipe", checkNameCRENCCLAllReduceBW))
	}
	actual, passed, err := validateCRENcclAllReduceBw(ctx, constraint)
	return classifyNCCLAllReduceBWResult(checkNameCRENCCLAllReduceBW, constraint, actual, passed, err)
}

func validateCRENcclAllReduceBw(
	ctx *validators.Context,
	constraint recipe.Constraint,
) (actual string, passed bool, err error) {

	if ctx.ValidationInput == nil {
		return skipMsgNCCLNoInput, true, nil
	}
	service := ctx.ValidationInput.Criteria.Service
	accelerator := ctx.ValidationInput.Criteria.Accelerator
	if service != recipe.CriteriaServiceEKS || accelerator != recipe.CriteriaAcceleratorH100 {
		return fmt.Sprintf("skipped - CRE NCCL currently supports only eks × h100, got %s × %s", service, accelerator), true, nil
	}

	threshold, err := parseThreshold(constraint.Value)
	if err != nil {
		return "", false, err
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
	obj := buildCRENCCLCertification(ctx.Namespace, objName, gpuConfig)

	if deleteErr := deleteCRECertification(ctx.Ctx, dyn, ctx.Namespace, objName); deleteErr != nil {
		return "", false, deleteErr
	}

	defer func() {
		delErr := teardownCRECertification(context.Background(), dyn, ctx.Clientset, ctx.Namespace, objName)
		// A leak must not hide behind a passing bandwidth reading, so drop the
		// measurement along with the pass when cleanup is what failed.
		if leaked := creCleanupFailure(checkNameCRENCCLAllReduceBW, err, delErr); leaked != nil && err == nil {
			actual, passed, err = "", false, leaked
		}
	}()

	if createErr := createUnstructured(ctx.Ctx, dyn, certificationGVR, ctx.Namespace, obj); createErr != nil {
		return "", false, createErr
	}

	run, err := waitForCertificationTerminal(ctx.Ctx, dyn, ctx.Namespace, objName)
	if err != nil {
		return "", false, err
	}
	if unstructuredConditionTrue(run, "Failed") {
		return "", false, aicrErrors.New(aicrErrors.ErrCodeInternal, "CRE Certification failed")
	}

	workflowName, err := certificationWorkflowName(run, creNCCLDomain, creNCCLVariant)
	if err != nil {
		return "", false, err
	}
	bw, err := listMaxBusBandwidth(ctx.Ctx, dyn, ctx.Namespace, workflowName, run.GetCreationTimestamp())
	if err != nil {
		return "", false, err
	}

	logs, logErr := creLauncherLogs(ctx, workflowName, run.GetCreationTimestamp())
	if logErr != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "CRE launcher logs required for transport assertion", logErr)
	}
	if err := verifyTransportFromLogs(logs, variantNET); err != nil {
		return "", false, err
	}

	actual = strconv.FormatFloat(bw, 'f', 2, 64)
	return actual, bw >= threshold, nil
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

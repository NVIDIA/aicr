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
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func checkCRETrainingGoodput(ctx *validators.Context) (err error) {
	constraint, found := findPerformanceConstraint(ctx, checkNameCRETrainingGoodput)
	if !found {
		return validators.Skip(fmt.Sprintf("no %s constraint in recipe", checkNameCRETrainingGoodput))
	}
	if ctx.ValidationInput == nil {
		return validators.Skip("no validation input")
	}
	if ctx.ValidationInput.Criteria.Service != recipe.CriteriaServiceEKS ||
		ctx.ValidationInput.Criteria.Accelerator != recipe.CriteriaAcceleratorH100 {

		return validators.Skip(fmt.Sprintf(
			"%s currently supports only eks × h100, got %s × %s",
			checkNameCRETrainingGoodput,
			ctx.ValidationInput.Criteria.Service,
			ctx.ValidationInput.Criteria.Accelerator,
		))
	}

	threshold, err := parseThreshold(constraint.Value)
	if err != nil {
		return err
	}

	gpuConfig, err := determineGPUConfig(
		ctx,
		ctx.ValidationInput.Criteria.Service,
		ctx.ValidationInput.Criteria.Accelerator,
		ctx.NodeSelector,
	)
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine GPU configuration", err)
	}
	if len(gpuConfig.Nodes) < 2 {
		return aicrErrors.New(aicrErrors.ErrCodeNotFound,
			fmt.Sprintf("recipe declares %s but the cluster has fewer than 2 GPU nodes", checkNameCRETrainingGoodput))
	}
	if ctx.DynamicClient == nil {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, "dynamic client is required to create a Certification")
	}

	objName, err := uniqueCREResourceName(creTrainingRunName)
	if err != nil {
		return err
	}
	obj := buildCRETrainingCertification(ctx.Namespace, objName, gpuConfig)
	if deleteErr := deleteCRECertification(ctx.Ctx, ctx.DynamicClient, ctx.Namespace, objName); deleteErr != nil {
		return deleteErr
	}
	defer func() {
		err = creCleanupFailure(checkNameCRETrainingGoodput, err, teardownCRECertification(
			context.Background(),
			ctx.DynamicClient,
			ctx.Clientset,
			ctx.Namespace,
			objName,
		))
	}()

	if createErr := createUnstructured(ctx.Ctx, ctx.DynamicClient, certificationGVR, ctx.Namespace, obj); createErr != nil {
		return createErr
	}
	run, err := waitForCertificationTerminal(ctx.Ctx, ctx.DynamicClient, ctx.Namespace, objName)
	if err != nil {
		return err
	}
	if unstructuredConditionTrue(run, "Failed") {
		summary := creTerminalConditionSummary(run)
		slog.Error("CRE training Certification failed", "certification", objName, "summary", summary)
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("CRE training Certification failed: %s", summary))
	}

	workflowName, err := certificationWorkflowName(run, creTrainingDomain, creTrainingVariant)
	if err != nil {
		return err
	}
	status, err := getGoodputStatus(
		ctx.Ctx,
		ctx.DynamicClient,
		ctx.Namespace,
		workflowName,
		run.GetCreationTimestamp(),
	)
	if err != nil {
		return err
	}
	ratio, err := parseGoodputRatio(status)
	if err != nil {
		return err
	}

	actual := strconv.FormatFloat(ratio, 'f', 4, 64)
	fmt.Printf("CRE NeMo training goodput ratio: %s\n", actual)
	fmt.Printf("Constraint: %s → %v\n", constraint.Value, ratio >= threshold)
	for _, metric := range []string{
		"avgTFLOPSPerGPU",
		"avgStepTimeSec",
		"interruptionCount",
		"lostWorkTimeSec",
	} {
		if value, ok := status[metric]; ok {
			fmt.Printf("%s: %v\n", metric, value)
		}
	}

	if ratio < threshold {
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("goodput ratio %s does not satisfy constraint %q", actual, constraint.Value))
	}
	return nil
}

const creTrainingRunName = "aicr-cre-nemo"

var goodputMeasurementGVR = schema.GroupVersionResource{
	Group: creAPIGroup, Version: versionV1alpha1, Resource: "goodputmeasurements",
}

func creTerminalConditionSummary(obj *unstructured.Unstructured) string {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "no status.conditions"
	}
	var parts []string
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := fmt.Sprint(m["type"])
		if unstructuredString(m["status"]) != conditionStatusTrue {
			continue
		}
		reason := unstructuredString(m["reason"])
		msg := unstructuredString(m["message"])
		switch {
		case reason != "" && msg != "":
			parts = append(parts, fmt.Sprintf("%s (%s): %s", typ, reason, msg))
		case reason != "":
			parts = append(parts, fmt.Sprintf("%s (%s)", typ, reason))
		case msg != "":
			parts = append(parts, fmt.Sprintf("%s: %s", typ, msg))
		default:
			parts = append(parts, typ)
		}
	}
	if len(parts) == 0 {
		return "Failed with empty condition reason/message"
	}
	return strings.Join(parts, "; ")
}

func unstructuredString(v any) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return strings.TrimSpace(s)
}

func parseGoodputRatio(status map[string]any) (float64, error) {
	raw, ok := status["result"]
	if !ok || raw == nil {
		return 0, aicrErrors.New(aicrErrors.ErrCodeNotFound, "GoodputMeasurement status.result is empty")
	}
	switch value := raw.(type) {
	case string:
		ratio, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid goodput result", err)
		}
		return ratio, nil
	case float64:
		return value, nil
	default:
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported goodput result type %T", raw))
	}
}

func getGoodputStatus(
	ctx context.Context,
	client dynamic.Interface,
	namespace, workflowName string,
	createdAt metav1.Time,
) (map[string]any, error) {

	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	list, err := client.Resource(goodputMeasurementGVR).Namespace(namespace).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list GoodputMeasurements", err)
	}
	for i := range list.Items {
		if !measurementBelongsToRun(&list.Items[i], workflowName, createdAt) {
			continue
		}
		status, found, nestedErr := unstructured.NestedMap(list.Items[i].Object, "status")
		if nestedErr != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read GoodputMeasurement status", nestedErr)
		}
		if found {
			return status, nil
		}
	}
	return nil, aicrErrors.New(aicrErrors.ErrCodeNotFound, "no GoodputMeasurement status for CRE training Certification")
}

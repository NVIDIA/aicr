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
	"path/filepath"
	"testing"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// webhookDenialRaw builds the Kubeflow Trainer validating webhook's raw
// rejection of a TrainJob whose referenced TrainingRuntime is not yet visible to
// the webhook's informer cache — the StatusError the API server returns from
// Create, before createUnstructured wraps it.
func webhookDenialRaw(withPhrase bool) *apierrors.StatusError {
	detail := `TrainingRuntime.trainer.kubeflow.org "` + ncclTrainingRuntimeName + `" not found`
	if withPhrase {
		detail += ": specified trainingRuntime must be created before the TrainJob is created"
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "trainer.kubeflow.org", Kind: "TrainJob"},
		ncclTrainJobName,
		field.ErrorList{field.Invalid(field.NewPath("spec", "runtimeRef"), ncclTrainingRuntimeName, detail)},
	)
}

// webhookDenial wraps webhookDenialRaw the way createUnstructured wraps Create
// errors, so detection is exercised through the same wrap chain as production.
func webhookDenial(withPhrase bool) error {
	return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create resource", webhookDenialRaw(withPhrase))
}

func TestIsTrainingRuntimeNotYetVisible(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"webhook cache-lag phrase", webhookDenial(true), true},
		{"invalid + runtime not found (no phrase)", webhookDenial(false), true},
		{"generic internal error", aicrErrors.New(aicrErrors.ErrCodeInternal, "boom"), false},
		{"timeout", aicrErrors.New(aicrErrors.ErrCodeTimeout, "deadline exceeded"), false},
		{
			"unrelated NotFound",
			aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create resource",
				apierrors.NewNotFound(schema.GroupResource{Group: "trainer.kubeflow.org", Resource: "trainjobs"}, ncclTrainJobName)),
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTrainingRuntimeNotYetVisible(tt.err); got != tt.want {
				t.Errorf("isTrainingRuntimeNotYetVisible() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeTrainJobClient returns a fake dynamic client whose TrainJob create is
// denied by the webhook for the first denyCount attempts, then admitted.
func fakeTrainJobClient(denyCount *int) *dynamicfake.FakeDynamicClient {
	c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), ncclGVRListKinds)
	c.PrependReactor("create", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if *denyCount > 0 {
			*denyCount--
			return true, nil, webhookDenialRaw(true)
		}
		// Admit: let the default tracker create the object.
		return false, nil, nil
	})
	return c
}

func TestApplyTrainJobWithRetry_RetriesUntilAdmitted(t *testing.T) {
	deny := 2
	client := fakeTrainJobClient(&deny)
	data := map[string]string{"NAMESPACE": "aicr-validation", "WORKER_COUNT": "2"}

	err := applyTrainJobWithRetry(context.Background(), client, "aicr-validation",
		filepath.Join("testdata", "trainjob.yaml"), data)
	if err != nil {
		t.Fatalf("expected TrainJob to be admitted after retries, got %v", err)
	}
	if deny != 0 {
		t.Errorf("expected all %d denials to be consumed, %d remaining", 2, deny)
	}
}

func TestApplyTrainJobWithRetry_PropagatesNonRaceError(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), ncclGVRListKinds)
	sentinel := apierrors.NewForbidden(
		schema.GroupResource{Group: "trainer.kubeflow.org", Resource: "trainjobs"}, ncclTrainJobName,
		stderrors.New("not authorized"))
	client.PrependReactor("create", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, sentinel
	})
	data := map[string]string{"NAMESPACE": "aicr-validation", "WORKER_COUNT": "2"}

	err := applyTrainJobWithRetry(context.Background(), client, "aicr-validation",
		filepath.Join("testdata", "trainjob.yaml"), data)
	if err == nil {
		t.Fatal("expected non-race error to propagate, got nil")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("expected the Forbidden error to propagate unmasked, got %v", err)
	}
}

func TestApplyTrainJobWithRetry_TimeoutClassifiedWhenBudgetExpiresMidCreate(t *testing.T) {
	// Simulates the retry budget elapsing while createUnstructured is in flight:
	// the aborted create returns a non-race error, but because retryCtx is
	// already done the result must be classified as ErrCodeTimeout rather than
	// leaking that incidental error (CodeRabbit finding on the non-race return).
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), ncclGVRListKinds)
	client.PrependReactor("create", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(stderrors.New("apiserver hiccup"))
	})
	data := map[string]string{"NAMESPACE": "aicr-validation", "WORKER_COUNT": "2"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // budget already exhausted before the first create returns

	err := applyTrainJobWithRetry(ctx, client, "aicr-validation",
		filepath.Join("testdata", "trainjob.yaml"), data)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
		t.Errorf("expected ErrCodeTimeout when the retry budget expired, got %v", err)
	}
}

func TestApplyTrainJobWithRetry_TimesOutWhenWebhookNeverCatchesUp(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), ncclGVRListKinds)
	client.PrependReactor("create", "trainjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, webhookDenialRaw(true)
	})
	data := map[string]string{"NAMESPACE": "aicr-validation", "WORKER_COUNT": "2"}

	// Bound the parent context tightly so the retry loop gives up quickly
	// rather than waiting out TrainJobAdmissionRetryTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := applyTrainJobWithRetry(ctx, client, "aicr-validation",
		filepath.Join("testdata", "trainjob.yaml"), data)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
		t.Errorf("expected ErrCodeTimeout, got %v", err)
	}
}

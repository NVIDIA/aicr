// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package helper

import (
	"context"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// VerifyResource checks that a single expected resource exists and is healthy.
// Supports Deployment, DaemonSet, StatefulSet, Service, ConfigMap, Secret.
func VerifyResource(ctx context.Context, clientset kubernetes.Interface, er recipe.ExpectedResource) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.ResourceVerificationTimeout)
	defer cancel()

	switch er.Kind {
	case "Deployment":
		deploy, err := clientset.AppsV1().Deployments(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}
		expected := int32(1)
		if deploy.Spec.Replicas != nil {
			expected = *deploy.Spec.Replicas
		}
		if deploy.Status.AvailableReplicas < expected {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("not healthy: %d/%d replicas available",
					deploy.Status.AvailableReplicas, expected))
		}

	case "DaemonSet":
		ds, err := clientset.AppsV1().DaemonSets(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}
		if ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("not healthy: %d/%d pods ready",
					ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
		}

	case "StatefulSet":
		ss, err := clientset.AppsV1().StatefulSets(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}
		expected := int32(1)
		if ss.Spec.Replicas != nil {
			expected = *ss.Spec.Replicas
		}
		if ss.Status.ReadyReplicas < expected {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("not healthy: %d/%d replicas ready",
					ss.Status.ReadyReplicas, expected))
		}

	case "Service":
		_, err := clientset.CoreV1().Services(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}

	case "ConfigMap":
		_, err := clientset.CoreV1().ConfigMaps(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}

	case "Secret":
		_, err := clientset.CoreV1().Secrets(er.Namespace).Get(ctx, er.Name, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "not found", err)
		}

	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported resource kind %q", er.Kind))
	}

	return nil
}

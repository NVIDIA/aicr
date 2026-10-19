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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// imagePullFailureReasons are the container Waiting reasons for an image the
// kubelet could not pull.
var imagePullFailureReasons = map[string]bool{
	"ErrImagePull":     true,
	"ImagePullBackOff": true,
	"InvalidImageName": true,
}

// imagePullTimeoutError explains a wait that timed out because a pod could not
// pull its image. When waitErr is a timeout and a pod that selector matches in
// namespace is still failing to pull, it returns a timeout error naming the pod
// and image, wrapping waitErr. Otherwise it returns nil. An empty selector
// checks every pod in namespace.
func imagePullTimeoutError(ctx context.Context, client kubernetes.Interface, namespace, selector string, waitErr error) error {
	if !stderrors.Is(waitErr, errors.New(errors.ErrCodeTimeout, "")) {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	pods, err := client.CoreV1().Pods(namespace).List(listCtx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		slog.Warn("cannot list pods to diagnose wait timeout",
			"namespace", namespace, "selector", selector, "error", err)
		return nil
	}
	for i := range pods.Items {
		if msg := imagePullFailure(&pods.Items[i]); msg != "" {
			return errors.Wrap(errors.ErrCodeTimeout, msg, waitErr)
		}
	}
	return nil
}

// imagePullFailure describes the first init or regular container of p that is
// failing to pull its image, or returns "" when none is. A pod being deleted is
// ignored.
func imagePullFailure(p *v1.Pod) string {
	if p.DeletionTimestamp != nil {
		return ""
	}
	statuses := append(append([]v1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		waiting := cs.State.Waiting
		if waiting == nil || !imagePullFailureReasons[waiting.Reason] {
			continue
		}
		return fmt.Sprintf(
			"pod %s/%s cannot pull image %q for container %q (%s: %s). "+
				"Check that the image exists at the configured registry path and that "+
				"--image-pull-secret names a Secret with access to it",
			p.Namespace, p.Name, cs.Image, cs.Name, waiting.Reason, waiting.Message)
	}
	return ""
}

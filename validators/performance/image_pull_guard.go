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
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// imagePullFailureReasons are the container Waiting reasons that mark an image
// the kubelet keeps failing to pull. ErrImagePull is absent because the kubelet
// reports it on the first failed attempt, including transient registry errors.
var imagePullFailureReasons = map[string]bool{
	"ImagePullBackOff": true,
	"InvalidImageName": true,
}

// waitFailFastOnImagePull runs wait while watching the pods that selector
// matches in namespace. When one of them cannot pull an image, it cancels wait
// and returns an error naming the pod and image. Otherwise it returns wait's
// result. An empty selector watches every pod in namespace.
func waitFailFastOnImagePull(ctx context.Context, client kubernetes.Interface, namespace, selector string, wait func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var guardErr error
	guardDone := make(chan struct{})
	go func() {
		defer close(guardDone)
		if err := watchForImagePullFailure(ctx, client, namespace, selector); err != nil {
			guardErr = err
			cancel()
		}
	}()

	waitErr := wait(ctx)
	cancel()
	<-guardDone
	if waitErr != nil && guardErr != nil {
		return guardErr
	}
	return waitErr
}

// watchForImagePullFailure returns an error once a pod matching selector has a
// container that cannot pull its image, and nil when ctx ends. A List or Watch
// call that fails leaves the caller's own timeout in charge, so it logs and
// returns nil. A watch that closes is re-established from a fresh List.
func watchForImagePullFailure(ctx context.Context, client kubernetes.Interface, namespace, selector string) error {
	pods := client.CoreV1().Pods(namespace)
	for {
		listCtx, listCancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
		list, err := pods.List(listCtx, metav1.ListOptions{LabelSelector: selector})
		listCancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("image pull check stopped, cannot list pods",
					"namespace", namespace, "selector", selector, "error", err)
			}
			return nil
		}
		for i := range list.Items {
			if pullErr := imagePullError(&list.Items[i]); pullErr != nil {
				return pullErr
			}
		}

		watcher, err := pods.Watch(ctx, metav1.ListOptions{
			LabelSelector:   selector,
			ResourceVersion: list.ResourceVersion,
		})
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("image pull check stopped, cannot watch pods",
					"namespace", namespace, "selector", selector, "error", err)
			}
			return nil
		}
		pullErr := drainPodWatch(ctx, watcher)
		watcher.Stop()
		if pullErr != nil {
			return pullErr
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(defaults.K8sJobWatchResumeBackoff):
		}
	}
}

// drainPodWatch checks each pod event until ctx ends, the watch closes, or the
// watch reports an error event.
func drainPodWatch(ctx context.Context, watcher watch.Interface) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok || event.Type == watch.Error {
				return nil
			}
			p, isPod := event.Object.(*v1.Pod)
			if !isPod || event.Type == watch.Deleted {
				continue
			}
			if pullErr := imagePullError(p); pullErr != nil {
				return pullErr
			}
		}
	}
}

// imagePullError reports the first init or regular container of p that is
// waiting on an image it cannot pull. A pod being deleted is ignored.
func imagePullError(p *v1.Pod) error {
	if p.DeletionTimestamp != nil {
		return nil
	}
	statuses := append(append([]v1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		waiting := cs.State.Waiting
		if waiting == nil || !imagePullFailureReasons[waiting.Reason] {
			continue
		}
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"pod %s/%s cannot pull image %q for container %q (%s: %s). "+
				"Check that the image exists at the configured registry path and that "+
				"--image-pull-secret names a Secret with access to it",
			p.Namespace, p.Name, cs.Image, cs.Name, waiting.Reason, waiting.Message))
	}
	return nil
}

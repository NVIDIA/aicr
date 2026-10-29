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

package controller

import (
	"context"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/evidence"
)

// OperatorImage reads the operator's own image from its pod, for evidence:
// the reference the pod spec names and its digest. A reference
// pinned by digest is its own digest, which the kubelet resolved by content;
// a tag is resolved through the image ID the kubelet reports, which for an
// image it loaded rather than pulled can name a loader's wrapper. The first
// complete answer is kept for the process's life.
type OperatorImage struct {
	Reader    client.Reader
	Namespace string
	Pod       string
	Container string

	mu    sync.Mutex
	image *evidence.Image
}

// Get returns the operator's image and digest.
func (o *OperatorImage) Get(ctx context.Context) (evidence.Image, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.image != nil {
		return *o.image, nil
	}
	pod := &corev1.Pod{}
	if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Pod}, pod); err != nil {
		return evidence.Image{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read the operator pod "+o.Namespace+"/"+o.Pod)
	}
	image := evidence.Image{}
	for _, c := range pod.Spec.Containers {
		if c.Name == o.Container {
			image.Image = c.Image
			image.Digest = digestOfImageID(c.Image)
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == o.Container && image.Digest == "" {
			image.Digest = digestOfImageID(s.ImageID)
		}
	}
	if image.Image == "" || image.Digest == "" {
		return evidence.Image{}, operrors.New(operrors.ErrCodeUnavailable,
			"the operator pod "+o.Namespace+"/"+o.Pod+" records no image digest for container "+o.Container+" yet")
	}
	o.image = &image
	return image, nil
}

// digestOfImageID extracts the digest from a container status image ID:
// repository@sha256:..., or a bare sha256:... for a side-loaded image.
func digestOfImageID(id string) string {
	if _, d, ok := strings.Cut(id, "@"); ok {
		id = d
	}
	if strings.HasPrefix(id, "sha256:") && len(id) == len("sha256:")+64 {
		return id
	}
	return ""
}

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

package chainsaw

import (
	"context"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// clusterFetcher implements ResourceFetcher using a dynamic Kubernetes client.
type clusterFetcher struct {
	client dynamic.Interface
	mapper meta.RESTMapper
}

// NewClusterFetcher creates a ResourceFetcher that queries a live Kubernetes cluster.
func NewClusterFetcher(client dynamic.Interface, mapper meta.RESTMapper) ResourceFetcher {
	return &clusterFetcher{client: client, mapper: mapper}
}

func (f *clusterFetcher) Fetch(ctx context.Context, apiVersion, kind, namespace, name string) (map[string]interface{}, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q", apiVersion), err)
	}

	gvk := gv.WithKind(kind)
	mapping, err := f.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("no REST mapping for %s", gvk), err)
	}

	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = f.client.Resource(mapping.Resource).Namespace(namespace)
	} else {
		resource = f.client.Resource(mapping.Resource)
	}

	obj, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Preserve the distinction between a true 404 and any other
		// API failure. Negative health checks (chainsaw `error:`
		// blocks) treat NotFound as the happy path and must fail
		// closed on transient errors (timeouts, 5xx, forbidden) —
		// otherwise a flaky apiserver silently passes a check that
		// should have caught the forbidden shape.
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("%s %s/%s not found", kind, namespace, name), err)
		}
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to get %s %s/%s", kind, namespace, name), err)
	}

	return obj.UnstructuredContent(), nil
}

// List enumerates resources of the given kind in the given namespace,
// optionally narrowed by label match. labels is a string→string map
// converted to the canonical "k=v,k=v" Kubernetes label selector
// format. An empty labels map yields no selector (all resources of the
// kind in the namespace).
//
// Returns an empty slice (not error) when no resources match; callers
// distinguish "no matches" from "list failed".
func (f *clusterFetcher) List(ctx context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]interface{}, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q", apiVersion), err)
	}

	gvk := gv.WithKind(kind)
	mapping, err := f.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("no REST mapping for %s", gvk), err)
	}

	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = f.client.Resource(mapping.Resource).Namespace(namespace)
	} else {
		resource = f.client.Resource(mapping.Resource)
	}

	opts := metav1.ListOptions{}
	if len(labels) > 0 {
		parts := make([]string, 0, len(labels))
		for k, v := range labels {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		opts.LabelSelector = strings.Join(parts, ",")
	}

	list, err := resource.List(ctx, opts)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to list %s in namespace %q", gvk, namespace), err)
	}

	out := make([]map[string]interface{}, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].UnstructuredContent())
	}
	return out, nil
}

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

	"github.com/NVIDIA/aicr/pkg/errors"
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
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("%s %s/%s not found", kind, namespace, name), err)
	}

	return obj.UnstructuredContent(), nil
}

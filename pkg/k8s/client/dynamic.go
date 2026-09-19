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

package client

import (
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// BoundedConfig returns a copy of restConfig with an explicit request timeout
// when the caller left one unset. Discovery reaches the apiserver through the
// context-free DiscoveryInterface, so nothing else bounds those calls —
// client-go's own 32s discovery default is the only backstop, and it does not
// apply to the dynamic client at all. Copying keeps the caller's config
// untouched.
func BoundedConfig(restConfig *rest.Config) *rest.Config {
	cfg := rest.CopyConfig(restConfig)
	if cfg.Timeout == 0 {
		cfg.Timeout = defaults.K8sClientRequestTimeout
	}

	return cfg
}

// NewDynamicClientForConfig builds an unstructured client for restConfig,
// bounded by BoundedConfig so a per-request hang cannot outlive the caller.
//
// This is the construction site for dynamic clients; callers that want one
// pre-wired into a resource fetcher want chainsaw.NewClusterFetcherForConfig
// instead.
func NewDynamicClientForConfig(restConfig *rest.Config) (dynamic.Interface, error) {
	if restConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	dynClient, err := dynamic.NewForConfig(BoundedConfig(restConfig))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}

	return dynClient, nil
}

// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package kubeclient

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netop "github.com/Mellanox/network-operator/api/v1alpha1"
	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// New builds a controller-runtime client using the provided kubeconfig path
// and registers required schemes. It also returns the underlying REST config
// for use with client-go APIs (e.g., pod exec).
func New(kubeconfigPath string) (client.Client, *rest.Config, error) {
	// Build REST config from kubeconfig path
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}

	// Prepare scheme and client
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = apiextv1.AddToScheme(scheme)
	_ = netop.AddToScheme(scheme)
	_ = nicop.AddToScheme(scheme)

	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, err
	}

	return c, restCfg, nil
}

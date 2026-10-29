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

// Package v1alpha1 contains the operator.aicr.run/v1alpha1 API: ClusterRuntime
// and RuntimeRollout (docs/operator/api.md). v1alpha1 carries no conversion
// guarantee; a breaking change ships with a documented manual migration.
//
// +kubebuilder:object:generate=true
// +groupName=operator.aicr.run
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the operator API.
	GroupVersion = schema.GroupVersion{Group: "operator.aicr.run", Version: "v1alpha1"}

	// SchemeBuilder registers the API types with a scheme. It depends only on
	// apimachinery, so the API package stays cheap to import.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the API types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ClusterRuntime{}, &ClusterRuntimeList{},
		&RuntimeRollout{}, &RuntimeRolloutList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

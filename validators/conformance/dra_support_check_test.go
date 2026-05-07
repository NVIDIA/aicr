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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestCheckDRASupport_SkipsWhenNoDRAAPI(t *testing.T) {
	t.Parallel()

	client := k8sfake.NewClientset()
	fakeDisc := client.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDisc.Resources = []*metav1.APIResourceList{}

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected skip when DRA API is not available")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected skip message about DRA API, got: %v", err)
	}
}

func TestCheckDRASupport_V1GatePassesThenChecksDriver(t *testing.T) {
	t.Parallel()

	client := k8sfake.NewClientset()
	fakeDisc := client.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDisc.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "resource.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "resourceclaims", Kind: "ResourceClaim", Namespaced: true},
				{Name: "resourceslices", Kind: "ResourceSlice", Namespaced: false},
			},
		},
	}

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}

	err := CheckDRASupport(ctx)
	if err == nil {
		t.Fatal("expected error (no DRA driver), but should NOT be a skip about DRA API")
	}
	// The check should proceed past the v1 API gate and fail on missing driver,
	// not skip due to missing API.
	if strings.Contains(err.Error(), "resource.k8s.io") && strings.Contains(err.Error(), "not available") {
		t.Fatalf("check incorrectly skipped at API gate on v1-capable cluster: %v", err)
	}
	// Should hit the driver-not-found skip.
	if !strings.Contains(err.Error(), "DRA driver not found") {
		t.Fatalf("expected driver-not-found skip, got: %v", err)
	}
}

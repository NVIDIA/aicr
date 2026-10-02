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
	"context"
	stderrors "errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/NVIDIA/aicr/pkg/errors"
)

type fakeGroups struct{ err error }

func (f fakeGroups) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, nil, f.err
}

func groupFailure(group string) *discovery.ErrGroupDiscoveryFailed {
	return &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: group, Version: "v1"}: stderrors.New("service unavailable"),
	}}
}

func TestIsGenuineNoMatch(t *testing.T) {
	noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "example.io", Kind: "Widget"}}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bare no-match", noMatch, true},
		{"no-match wrapped with a group failure", fmt.Errorf("%w: %w", groupFailure("example.io"), noMatch), false},
		{"group failure alone", groupFailure("example.io"), false},
		{"unrelated error", stderrors.New("i/o timeout"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGenuineNoMatch(tt.err); got != tt.want {
				t.Errorf("IsGenuineNoMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGroupDiscoveryFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		groups  GroupResourceLister
		wantErr bool
	}{
		{"every group enumerated", context.Background(), fakeGroups{}, false},
		{"another group failed", context.Background(), fakeGroups{err: groupFailure("metrics.k8s.io")}, false},
		{"this group failed", context.Background(), fakeGroups{err: groupFailure("example.io")}, true},
		{"discovery failed outright", context.Background(), fakeGroups{err: stderrors.New("i/o timeout")}, true},
		{"context already ended", canceled, fakeGroups{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := GroupDiscoveryFailure(tt.ctx, tt.groups, "example.io")
			if (err != nil) != tt.wantErr {
				t.Fatalf("GroupDiscoveryFailure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
				t.Errorf("GroupDiscoveryFailure() error = %v, want %s", err, errors.ErrCodeUnavailable)
			}
		})
	}
}

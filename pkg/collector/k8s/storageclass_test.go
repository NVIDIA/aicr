// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestCollectDefaultStorageClass(t *testing.T) {
	t.Parallel()

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"},
		"",
		stderrors.New("forbidden"),
	)

	tests := []struct {
		name        string
		classes     []runtime.Object
		listError   error
		want        map[string]any
		wantMissing []string
	}{
		{
			name:        "no StorageClasses is absent",
			want:        map[string]any{storageClassKeyCollectionState: storageClassStateAbsent, storageClassKeyDefaultCount: 0},
			wantMissing: []string{storageClassKeyDefaultClasses},
		},
		{
			name: "non-default classes only is absent",
			classes: []runtime.Object{
				newStorageClass("gp2", nil),
				newStorageClass("gp3", map[string]string{defaultStorageClassAnnotation: "false"}),
			},
			want:        map[string]any{storageClassKeyCollectionState: storageClassStateAbsent, storageClassKeyDefaultCount: 0},
			wantMissing: []string{storageClassKeyDefaultClasses},
		},
		{
			name: "one default is present",
			classes: []runtime.Object{
				newStorageClass("gp2", nil),
				newStorageClass("gp3", map[string]string{defaultStorageClassAnnotation: "true"}),
			},
			want: map[string]any{
				storageClassKeyCollectionState: storageClassStatePresent,
				storageClassKeyDefaultCount:    1,
				storageClassKeyDefaultClasses:  "gp3",
			},
		},
		{
			name:    "beta annotation counts as default",
			classes: []runtime.Object{newStorageClass("standard", map[string]string{defaultStorageClassBetaAnnotation: "true"})},
			want: map[string]any{
				storageClassKeyCollectionState: storageClassStatePresent,
				storageClassKeyDefaultCount:    1,
				storageClassKeyDefaultClasses:  "standard",
			},
		},
		{
			name: "two defaults is multiple, names sorted",
			classes: []runtime.Object{
				newStorageClass("zeta", map[string]string{defaultStorageClassAnnotation: "true"}),
				newStorageClass("alpha", map[string]string{defaultStorageClassAnnotation: "true"}),
			},
			want: map[string]any{
				storageClassKeyCollectionState: storageClassStateMultiple,
				storageClassKeyDefaultCount:    2,
				storageClassKeyDefaultClasses:  "alpha,zeta",
			},
		},
		{
			name:        "list forbidden is unknown",
			listError:   forbidden,
			want:        map[string]any{storageClassKeyCollectionState: storageClassStateUnknown},
			wantMissing: []string{storageClassKeyDefaultCount, storageClassKeyDefaultClasses},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clientset := fakeclient.NewClientset(tt.classes...)
			if tt.listError != nil {
				clientset.PrependReactor("list", "storageclasses",
					func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, tt.listError
					})
			}
			subtype := (&Collector{ClientSet: clientset}).collectDefaultStorageClass(context.Background())

			assert.Equal(t, SubtypeDefaultStorageClass, subtype.Name)
			for key, want := range tt.want {
				reading, ok := subtype.Data[key]
				if !ok {
					t.Errorf("missing key %q", key)
					continue
				}
				assert.Equal(t, want, reading.Any(), "key %q", key)
			}
			for _, key := range tt.wantMissing {
				assert.NotContains(t, subtype.Data, key)
			}
		})
	}
}

func TestCollectDefaultStorageClass_Pagination(t *testing.T) {
	t.Parallel()

	clientset := fakeclient.NewClientset()
	var continues []string
	clientset.PrependReactor("list", "storageclasses",
		func(action ktesting.Action) (bool, runtime.Object, error) {
			cont := action.(ktesting.ListActionImpl).GetListOptions().Continue
			continues = append(continues, cont)
			if cont == "" {
				list := &storagev1.StorageClassList{
					Items: []storagev1.StorageClass{*newStorageClass("a", map[string]string{defaultStorageClassAnnotation: "true"})},
				}
				list.Continue = "page-2"
				return true, list, nil
			}
			return true, &storagev1.StorageClassList{
				Items: []storagev1.StorageClass{*newStorageClass("b", map[string]string{defaultStorageClassAnnotation: "true"})},
			}, nil
		})

	subtype := (&Collector{ClientSet: clientset}).collectDefaultStorageClass(context.Background())

	assert.Equal(t, []string{"", "page-2"}, continues)
	assert.Equal(t, storageClassStateMultiple, subtype.Data[storageClassKeyCollectionState].Any())
	assert.Equal(t, "a,b", subtype.Data[storageClassKeyDefaultClasses].Any())
}

func TestCollectDefaultStorageClass_FailureModesAreUnknown(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		collector *Collector
	}{
		{name: "canceled context", ctx: canceled, collector: &Collector{ClientSet: fakeclient.NewClientset()}},
		{name: "no client", ctx: context.Background(), collector: &Collector{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subtype := tt.collector.collectDefaultStorageClass(tt.ctx)
			assert.Equal(t, storageClassStateUnknown, subtype.Data[storageClassKeyCollectionState].Any())
			assert.Len(t, subtype.Data, 1)
		})
	}
}

func newStorageClass(name string, annotations map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Annotations: annotations},
		Provisioner: "example.com/provisioner",
	}
}

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

package k8s

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAPIErrorCode(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()

	unauthorized := k8serrors.NewUnauthorized("token has expired")
	networkErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	forbidden := k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", stderrors.New("denied"))

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want errors.ErrorCode
	}{
		{"canceled context outranks the API error", canceled, unauthorized, errors.ErrCodeCanceled},
		{"expired deadline outranks the API error", expired, networkErr, errors.ErrCodeTimeout},
		{"unauthorized", context.Background(), unauthorized, errors.ErrCodeUnauthorized},
		{"network failure", context.Background(), networkErr, errors.ErrCodeUnavailable},
		{"anything else", context.Background(), forbidden, errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := APIErrorCode(tt.ctx, tt.err); got != tt.want {
				t.Errorf("APIErrorCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIgnoreNotFound(t *testing.T) {
	gr := schema.GroupResource{Group: "test", Resource: "things"}

	tests := []struct {
		name    string
		err     error
		wantNil bool
	}{
		{"nil error", nil, true},
		{"not found error", k8serrors.NewNotFound(gr, "thing1"), true},
		{"other error", fmt.Errorf("something else"), false},
		{"already exists error", k8serrors.NewAlreadyExists(gr, "thing1"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IgnoreNotFound(tt.err)
			if (got == nil) != tt.wantNil {
				t.Errorf("IgnoreNotFound() = %v, wantNil %v", got, tt.wantNil)
			}
		})
	}
}

func TestIgnoreAlreadyExists(t *testing.T) {
	gr := schema.GroupResource{Group: "test", Resource: "things"}

	tests := []struct {
		name    string
		err     error
		wantNil bool
	}{
		{"nil error", nil, true},
		{"already exists error", k8serrors.NewAlreadyExists(gr, "thing1"), true},
		{"other error", fmt.Errorf("something else"), false},
		{"not found error", k8serrors.NewNotFound(gr, "thing1"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IgnoreAlreadyExists(tt.err)
			if (got == nil) != tt.wantNil {
				t.Errorf("IgnoreAlreadyExists() = %v, wantNil %v", got, tt.wantNil)
			}
		})
	}
}

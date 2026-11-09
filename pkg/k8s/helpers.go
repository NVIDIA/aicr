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

	"github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// IgnoreNotFound returns nil if the error is "not found", otherwise returns the error.
// Used to make resource deletion idempotent.
func IgnoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// IgnoreAlreadyExists returns nil if the error is "already exists", otherwise returns the error.
// Used to make resource creation idempotent.
func IgnoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// APIErrorCode classifies an unexpected apiserver error: a dead context is a
// cancellation or a timeout, rejected credentials are unauthorized, a network
// failure is retryable, and anything else is internal.
func APIErrorCode(ctx context.Context, err error) errors.ErrorCode {
	switch {
	case stderrors.Is(ctx.Err(), context.Canceled):
		return errors.ErrCodeCanceled
	case ctx.Err() != nil:
		return errors.ErrCodeTimeout
	case apierrors.IsUnauthorized(err):
		return errors.ErrCodeUnauthorized
	case errors.IsNetworkError(err):
		return errors.ErrCodeUnavailable
	default:
		return errors.ErrCodeInternal
	}
}

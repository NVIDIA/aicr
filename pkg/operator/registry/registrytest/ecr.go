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

package registrytest

import (
	"context"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// ECR answers GetAuthorizationToken with Token, valid until Expires. Special
// cases are:
//   - With Release set, a call answers only once Release is closed, whatever
//     its context.
//   - A call fails while Err is set.
//   - The first Fail calls fail, as in an STS or ECR outage.
type ECR struct {
	// Token is the authorization token ECR would base64-encode,
	// "AWS:<password>".
	Token   string
	Expires time.Time
	Err     error
	Fail    int32
	Release chan struct{}

	calls atomic.Int32
}

// Calls returns how many times GetAuthorizationToken has been called.
func (f *ECR) Calls() int32 { return f.calls.Load() }

func (f *ECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	n := f.calls.Add(1)
	if f.Release != nil {
		<-f.Release
	}
	if f.Err != nil {
		return nil, f.Err
	}
	if n <= f.Fail {
		return nil, errors.New("sts: connection reset by peer")
	}
	token := base64.StdEncoding.EncodeToString([]byte(f.Token))
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []ecrtypes.AuthorizationData{{AuthorizationToken: &token, ExpiresAt: &f.Expires}}}, nil
}

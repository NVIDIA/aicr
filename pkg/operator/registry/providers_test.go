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

package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

const secretToken = "SECRET-TOKEN-VALUE"

type fakeECR struct {
	calls   int
	expires time.Time
	err     error
	token   string
}

func (f *fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	token := base64.StdEncoding.EncodeToString([]byte(f.token))
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []types.AuthorizationData{{AuthorizationToken: &token, ExpiresAt: &f.expires}}}, nil
}

// Tokens are cached until they near expiry, scoped to the runtime registry
// host, and never appear in errors.
func TestECRProvider(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	api := &fakeECR{token: "AWS:" + secretToken, expires: now.Add(12 * time.Hour)}
	clock := now
	p := &ECRProvider{Client: api, Host: "123456789012.dkr.ecr.us-west-2.amazonaws.com", Now: func() time.Time { return clock }}
	ctx := context.Background()

	c, err := p.Credential(ctx, p.Host)
	if err != nil || c.Username != "AWS" || c.Password != secretToken {
		t.Fatalf("Credential() = %+v, %v", c, err)
	}
	if other, _ := p.Credential(ctx, "ghcr.io"); other.Password != "" {
		t.Fatal("an upstream host received the runtime registry's token")
	}
	clock = now.Add(5 * time.Hour)
	if _, err = p.Token(ctx); err != nil || api.calls != 1 {
		t.Fatalf("a token with 7h left was not reused (%d calls)", api.calls)
	}
	clock = now.Add(7 * time.Hour)
	api.expires = clock.Add(12 * time.Hour)
	if tok, _ := p.Token(ctx); api.calls != 2 || !tok.Expires.Equal(api.expires) {
		t.Fatalf("a token with 5h left was reused (%d calls)", api.calls)
	}
	jc, err := p.ForJob(ctx)
	if err != nil || jc.SecretName != "" || !strings.Contains(string(jc.DockerConfigJSON), base64.StdEncoding.EncodeToString([]byte("AWS:"+secretToken))) {
		t.Fatalf("ForJob() = %s, %v", jc.DockerConfigJSON, err)
	}

	for name, bad := range map[string]*fakeECR{
		"api error":       {err: errors.New("AccessDeniedException: not authorized")},
		"malformed token": {token: secretToken, expires: now.Add(time.Hour * 12)},
	} {
		fresh := &ECRProvider{Client: bad, Host: p.Host, Now: func() time.Time { return now }}
		_, err := fresh.Credential(ctx, p.Host)
		if operrors.ReasonOf(err) != ReasonCredentialsUnavailable || strings.Contains(err.Error(), secretToken) {
			t.Errorf("%s: Credential() = %v", name, err)
		}
	}
}

func TestECRRegion(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{
		"123456789012.dkr.ecr.us-west-2.amazonaws.com":      "us-west-2",
		"123456789012.dkr.ecr-fips.us-east-1.amazonaws.com": "us-east-1",
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn":  "cn-north-1",
	} {
		if got, err := ECRRegion(host); err != nil || got != want {
			t.Errorf("ECRRegion(%s) = %s, %v", host, got, err)
		}
	}
	if _, err := ECRRegion("registry.example.com"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("ECRRegion(non-ECR) = %v", err)
	}
}

// The static provider reads the administrator's Secret by name, mounts it
// for Jobs as it is, and is anonymous without one; errors never carry the
// Secret's content.
func TestStaticProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cfg, _ := DockerConfigJSON("registry.local:5000", "aicr", secretToken)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "aicr-system", Name: "registry-auth"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: cfg},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	p := &StaticProvider{Reader: reader, Namespace: "aicr-system", SecretName: "registry-auth"}
	if c, err := p.Credential(ctx, "registry.local:5000"); err != nil || c.Username != "aicr" || c.Password != secretToken {
		t.Fatalf("Credential() = %+v, %v", c, err)
	}
	if jc, err := p.ForJob(ctx); err != nil || jc.SecretName != "registry-auth" || jc.DockerConfigJSON != nil {
		t.Fatalf("ForJob() = %+v, %v", jc, err)
	}

	anonymous := &StaticProvider{}
	if c, err := anonymous.Credential(ctx, "registry.local:5000"); err != nil || c.Password != "" {
		t.Fatalf("anonymous Credential() = %+v, %v", c, err)
	}
	if jc, _ := anonymous.ForJob(ctx); string(jc.DockerConfigJSON) != `{"auths":{}}` {
		t.Fatalf("anonymous ForJob() = %s", jc.DockerConfigJSON)
	}

	missing := &StaticProvider{Reader: reader, Namespace: "aicr-system", SecretName: "absent"}
	if _, err := missing.Credential(ctx, "registry.local:5000"); operrors.CodeOf(err) != operrors.ErrCodeUnavailable ||
		operrors.ReasonOf(err) != ReasonCredentialsUnavailable {

		t.Fatalf("missing Secret = %v", err)
	}
	secret.Data[corev1.DockerConfigJsonKey] = []byte("not-json " + secretToken)
	broken := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	_, err := (&StaticProvider{Reader: broken, Namespace: "aicr-system", SecretName: "registry-auth"}).Credential(ctx, "registry.local:5000")
	if err == nil || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("broken Secret = %v", err)
	}
}

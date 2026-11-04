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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry/registrytest"
)

const secretToken = "SECRET-TOKEN-VALUE"

// Tokens are cached until they near expiry, scoped to the runtime registry
// host, and never appear in errors.
func TestECRProvider(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	api := &registrytest.ECR{Token: "AWS:" + secretToken, Expires: now.Add(12 * time.Hour)}
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
	tok, err := p.FreshToken(ctx)
	if err != nil || tok.Password != secretToken || api.Calls() != 1 {
		t.Fatalf("FreshToken() with 12h left = %+v, %v after %d calls; want the cached token", tok, err, api.Calls())
	}
	clock = now.Add(5 * time.Hour)
	if _, err = p.Token(ctx); err != nil || api.Calls() != 1 {
		t.Fatalf("a token with 7h left was not reused (%d calls)", api.Calls())
	}
	clock = now.Add(7 * time.Hour)
	api.Expires = clock.Add(12 * time.Hour)
	if tok, _ := p.Token(ctx); !tok.Expires.Equal(now.Add(12 * time.Hour)) {
		t.Fatalf("a token with 5h left was not served while its renewal runs: %+v", tok)
	}
	if tok, _ := p.FreshToken(ctx); api.Calls() != 2 || !tok.Expires.Equal(api.Expires) {
		t.Fatalf("a token with 5h left was not renewed (%d calls)", api.Calls())
	}
	jc, err := p.ForJob(ctx)
	if err != nil || jc.SecretName != "" || !strings.Contains(string(jc.DockerConfigJSON), base64.StdEncoding.EncodeToString([]byte("AWS:"+secretToken))) {
		t.Fatalf("ForJob() = %s, %v", jc.DockerConfigJSON, err)
	}

	for name, bad := range map[string]*registrytest.ECR{
		"api error":       {Err: errors.New("AccessDeniedException: not authorized")},
		"malformed token": {Token: secretToken, Expires: now.Add(time.Hour * 12)},
	} {
		fresh := &ECRProvider{Client: bad, Host: p.Host, Now: func() time.Time { return now }}
		_, err := fresh.Credential(ctx, p.Host)
		if operrors.ReasonOf(err) != ReasonCredentialsUnavailable || strings.Contains(err.Error(), secretToken) {
			t.Errorf("%s: Credential() = %v", name, err)
		}
	}
}

// A hung mint holds no caller past its own deadline, and concurrent callers
// share one mint, which completes for later callers once ECR answers.
func TestECRProviderHungMint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	api := &registrytest.ECR{Token: "AWS:" + secretToken, Expires: now.Add(12 * time.Hour), Release: make(chan struct{})}
	p := &ECRProvider{Client: api, Host: "123456789012.dkr.ecr.us-west-2.amazonaws.com", Now: func() time.Time { return now }}

	errs := make(chan error, 3)
	for range cap(errs) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err := p.Credential(ctx, p.Host)
			errs <- err
		}()
	}
	for range cap(errs) {
		if err := <-errs; operrors.CodeOf(err) != operrors.ErrCodeTimeout || operrors.ReasonOf(err) != ReasonCredentialsUnavailable {
			t.Errorf("Credential() during a hung mint = %v, want TIMEOUT", err)
		}
	}
	if n := api.Calls(); n != 1 {
		t.Errorf("%d concurrent mints, want 1 shared", n)
	}
	close(api.Release)
	settle(p)
	if tok, err := p.FreshToken(context.Background()); err != nil || tok.Password != secretToken || api.Calls() != 1 {
		t.Fatalf("FreshToken() after ECR answered = %+v, %v with %d mints", tok, err, api.Calls())
	}
}

// A failed mint keeps the cached token in use while it has more than
// RegistryTokenMinServeValidity left, delays the next background mint by
// RegistryTokenMintRetryBackoff, and is recorded until a mint succeeds.
// Past that, the failure is the caller's.
func TestECRProviderRidesOutAFailedMint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	clock := now
	api := &registrytest.ECR{Token: "AWS:" + secretToken, Expires: now.Add(12 * time.Hour)}
	p := &ECRProvider{Client: api, Host: "123456789012.dkr.ecr.us-west-2.amazonaws.com", Now: func() time.Time { return clock }}
	ctx := context.Background()
	if p.ServesCachedToken() {
		t.Fatal("ServesCachedToken() before any mint = true")
	}
	if _, err := p.Token(ctx); err != nil || p.MintFailure() != nil {
		t.Fatalf("Token() = %v, failure %+v", err, p.MintFailure())
	}

	clock, api.Err = now.Add(7*time.Hour), errors.New("ThrottlingException: rate exceeded")
	if _, err := p.FreshToken(ctx); operrors.ReasonOf(err) != ReasonCredentialsUnavailable {
		t.Fatalf("FreshToken() with a failing mint = %v", err)
	}
	if f := p.MintFailure(); f == nil || !f.Since.Equal(clock) || !strings.Contains(f.Err.Error(), "ThrottlingException") || p.MintFailures() != 1 {
		t.Fatalf("MintFailure() = %+v, %d failures", f, p.MintFailures())
	}
	calls := api.Calls()
	if c, err := p.Credential(ctx, p.Host); err != nil || c.Password != secretToken || !p.ServesCachedToken() {
		t.Fatalf("Credential() with 5h left and a failing mint = %+v, %v; want the cached token", c, err)
	}
	settle(p)
	if api.Calls() != calls {
		t.Fatalf("Credential() within RegistryTokenMintRetryBackoff of a failed mint minted again (%d calls, want %d)", api.Calls(), calls)
	}
	clock = clock.Add(defaults.RegistryTokenMintRetryBackoff)
	if c, err := p.Credential(ctx, p.Host); err != nil || c.Password != secretToken {
		t.Fatalf("Credential() past RegistryTokenMintRetryBackoff = %+v, %v; want the cached token", c, err)
	}
	settle(p)
	if api.Calls() != calls+1 {
		t.Fatalf("Credential() past RegistryTokenMintRetryBackoff started %d mints, want 1", api.Calls()-calls)
	}

	clock = now.Add(11*time.Hour + 30*time.Minute)
	if _, err := p.Token(ctx); operrors.ReasonOf(err) != ReasonCredentialsUnavailable || p.ServesCachedToken() {
		t.Fatalf("Token() with 30m left and a failing mint = %v", err)
	}
	if f := p.MintFailure(); f == nil || !f.Since.Equal(now.Add(7*time.Hour)) {
		t.Fatalf("MintFailure() = %+v, want the first failure kept", f)
	}

	api.Err = nil
	if _, err := p.Token(ctx); err != nil || p.MintFailure() != nil {
		t.Fatalf("Token() after ECR recovered = %v, failure %+v", err, p.MintFailure())
	}
}

// settle waits out p's mint in flight, if one is.
func settle(p *ECRProvider) { <-p.mints.DoChan("", func() (any, error) { return nil, nil }) } //nolint:nilnil // the result is unused

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

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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Credential providers (registry.auth.provider).
const (
	ProviderStatic = "static"
	ProviderAWS    = "aws"
)

// ReasonCredentialsUnavailable marks a failure to obtain registry credentials.
const ReasonCredentialsUnavailable = "RegistryCredentialsUnavailable"

// JobCredentials says how a pushing AICR Job authenticates: by mounting an
// administrator's Secret in its namespace, or with a docker config the
// operator writes into a Secret the Job owns.
type JobCredentials struct {
	// SecretName names an existing docker config Secret in the Job namespace.
	SecretName string
	// DockerConfigJSON is the content of a per-run Secret when SecretName is empty.
	DockerConfigJSON []byte
}

// Provider supplies credentials for the runtime registry. Implementations
// never put a credential in an error, a log or a status.
type Provider interface {
	// Credential resolves the operator's own credential for a registry host;
	// other hosts (upstream chart registries) are anonymous.
	Credential(ctx context.Context, host string) (auth.Credential, error)
	// ForJob returns the credentials a pushing AICR Job uses.
	ForJob(ctx context.Context) (*JobCredentials, error)
}

// emptyDockerConfig authenticates to nothing: an anonymous registry.
var emptyDockerConfig = []byte(`{"auths":{}}`)

// StaticProvider uses an administrator-provided docker config Secret with
// the same name in the operator, Job and validation namespaces; the operator
// only reads it and never writes credentials. An empty SecretName is an
// anonymous registry.
type StaticProvider struct {
	Reader     client.Reader
	Namespace  string
	SecretName string
}

// Credential reads the Secret by name through the uncached reader. A
// missing or unreadable Secret is unavailable, never NotFound: callers read
// NotFound as a missing artifact, and the administrator restoring the
// Secret must resume the work.
func (p *StaticProvider) Credential(ctx context.Context, host string) (auth.Credential, error) {
	if p.SecretName == "" {
		return auth.EmptyCredential, nil
	}
	secret := &corev1.Secret{}
	if err := p.Reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.SecretName}, secret); err != nil {
		what := "read"
		if apierrors.IsNotFound(err) {
			what = "find"
		}
		return auth.EmptyCredential, operrors.New(operrors.ErrCodeUnavailable,
			fmt.Sprintf("cannot %s registry credentials Secret %s/%s", what, p.Namespace, p.SecretName)).WithReason(ReasonCredentialsUnavailable)
	}
	creds, err := DockerConfigCredentials(secret.Data[corev1.DockerConfigJsonKey])
	if err != nil {
		return auth.EmptyCredential, err
	}
	return creds(ctx, host)
}

// ForJob points pushing Jobs at the administrator's Secret, or at an empty
// per-run docker config for an anonymous registry.
func (p *StaticProvider) ForJob(context.Context) (*JobCredentials, error) { //nolint:unparam // implements Provider; the ECR provider can fail
	if p.SecretName != "" {
		return &JobCredentials{SecretName: p.SecretName}, nil
	}
	return &JobCredentials{DockerConfigJSON: emptyDockerConfig}, nil
}

// ECRTokens is the part of the ECR API the provider uses.
type ECRTokens interface {
	GetAuthorizationToken(ctx context.Context, in *ecr.GetAuthorizationTokenInput, opts ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// ecrHost matches a private ECR registry host and captures its region.
var ecrHost = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?$`)

// ECRRegion returns the region of a private ECR registry host.
func ECRRegion(host string) (string, error) {
	m := ecrHost.FindStringSubmatch(host)
	if m == nil {
		return "", invalid("registry host %q is not a private Amazon ECR registry", host)
	}
	return m[1], nil
}

// Token is a registry token and its expiry.
type Token struct {
	Username string
	Password string
	Expires  time.Time
}

// ECRProvider mints ECR authorization tokens with the operator's AWS
// identity (EKS Pod Identity or IRSA) and caches each until it nears expiry.
type ECRProvider struct {
	Client ECRTokens
	// Host is the runtime registry host.
	Host string
	Now  func() time.Time

	mu    sync.Mutex
	token *Token
}

// Token returns a token with at least defaults.RegistryTokenMinValidity
// left, minting a new one when the cached token is older.
func (p *ECRProvider) Token(ctx context.Context) (*Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != nil && p.token.Expires.Sub(p.Now()) > defaults.RegistryTokenMinValidity {
		return p.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	out, err := p.Client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		// The SDK's error names the operation and status, never the token.
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "mint an ECR authorization token").
			WithReason(ReasonCredentialsUnavailable)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil || out.AuthorizationData[0].ExpiresAt == nil {
		return nil, operrors.New(operrors.ErrCodeUnavailable, "ECR returned no authorization token").WithReason(ReasonCredentialsUnavailable)
	}
	raw, err := base64.StdEncoding.DecodeString(*out.AuthorizationData[0].AuthorizationToken)
	user, pass, ok := strings.Cut(string(raw), ":")
	if err != nil || !ok {
		return nil, operrors.New(operrors.ErrCodeUnavailable, "ECR returned a malformed authorization token").WithReason(ReasonCredentialsUnavailable)
	}
	p.token = &Token{Username: user, Password: pass, Expires: *out.AuthorizationData[0].ExpiresAt}
	return p.token, nil
}

// Credential returns the ECR token for the runtime registry host.
func (p *ECRProvider) Credential(ctx context.Context, host string) (auth.Credential, error) {
	if host != p.Host {
		return auth.EmptyCredential, nil
	}
	t, err := p.Token(ctx)
	if err != nil {
		return auth.EmptyCredential, err
	}
	return auth.Credential{Username: t.Username, Password: t.Password}, nil
}

// ForJob returns a fresh docker config for a per-run, Job-owned Secret.
func (p *ECRProvider) ForJob(ctx context.Context) (*JobCredentials, error) {
	t, err := p.Token(ctx)
	if err != nil {
		return nil, err
	}
	data, err := DockerConfigJSON(p.Host, t.Username, t.Password)
	if err != nil {
		return nil, err
	}
	return &JobCredentials{DockerConfigJSON: data}, nil
}

// DockerConfigJSON renders a docker config authenticating to one host.
func DockerConfigJSON(host, username, password string) ([]byte, error) {
	cfg := map[string]any{"auths": map[string]any{
		host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(username + ":" + password))},
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, operrors.New(operrors.ErrCodeInternal, "encode docker config")
	}
	return data, nil
}

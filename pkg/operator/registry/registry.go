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
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// URIScheme prefixes repository references in plans and engine objects.
const URIScheme = "oci://"

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Repository is an OCI repository: a registry host and a path within it.
type Repository struct {
	Host string
	Path string
}

// ParseRepository parses "oci://host[:port]/path" or "host[:port]/path". A
// tag or digest is not part of a repository.
func ParseRepository(uri string) (Repository, error) {
	ref := strings.TrimPrefix(uri, URIScheme)
	host, path, ok := strings.Cut(ref, "/")
	switch {
	case !ok || host == "" || path == "":
		return Repository{}, invalid("repository %q is not host/path", uri)
	case strings.ContainsAny(path, "@:") || strings.HasSuffix(path, "/"):
		return Repository{}, invalid("repository %q carries a tag, digest or trailing slash", uri)
	}
	return Repository{Host: host, Path: path}, nil
}

// Child returns the repository at path below r.
func (r Repository) Child(path string) Repository {
	return Repository{Host: r.Host, Path: r.Path + "/" + path}
}

// String renders host/path.
func (r Repository) String() string { return r.Host + "/" + r.Path }

// URI renders oci://host/path, the form plans and engine objects record.
func (r Repository) URI() string { return URIScheme + r.String() }

// Options configure a Client.
type Options struct {
	// PlainHTTPHosts are the registry hosts reached over HTTP instead of
	// HTTPS: only a runtime registry inside a test cluster. Every other host,
	// upstream chart registries included, is reached over HTTPS.
	PlainHTTPHosts []string
	// Credentials resolves the credential for a registry host. Nil means
	// anonymous access.
	Credentials auth.CredentialFunc
	// HTTPClient carries requests to registries and upstream chart
	// repositories. Nil uses a client that retries throttled and failed
	// requests with bounded exponential backoff, within RegistryTimeout.
	HTTPClient *http.Client
}

// Client reads and writes OCI artifacts in the runtime registry and fetches
// upstream charts. It holds no per-call state and is safe for concurrent use.
type Client struct {
	opts Options
	http *http.Client
}

// New returns a Client.
func New(opts Options) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: retry.NewTransport(http.DefaultTransport.(*http.Transport).Clone()), Timeout: defaults.RegistryTimeout}
	}
	return &Client{opts: opts, http: hc}
}

func (c *Client) repository(r Repository) (*remote.Repository, error) {
	repo, err := remote.NewRepository(r.String())
	if err != nil {
		return nil, invalid("repository %s: %v", r, err)
	}
	repo.PlainHTTP = slices.Contains(c.opts.PlainHTTPHosts, r.Host)
	repo.Client = &auth.Client{
		Client:     c.http,
		Cache:      auth.NewCache(),
		Credential: c.opts.Credentials,
	}
	return repo, nil
}

// Resolve returns the manifest digest a tag currently points at.
func (c *Client) Resolve(ctx context.Context, r Repository, tag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return "", err
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return "", classify(err, fmt.Sprintf("resolve %s:%s", r, tag))
	}
	return desc.Digest.String(), nil
}

// classify maps registry errors onto operator codes: a missing artifact is
// NOT_FOUND, an authorization failure UNAUTHORIZED, a throttled request
// RATE_LIMIT_EXCEEDED, cancellation and deadlines keep their codes, and
// anything else is SERVICE_UNAVAILABLE so the caller retries with backoff.
func classify(err error, message string) error {
	var se *operrors.StructuredError
	if errors.As(err, &se) {
		return err
	}
	if errors.Is(err, errdef.ErrNotFound) {
		return operrors.Wrap(operrors.ErrCodeNotFound, message, err)
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return operrors.Wrap(operrors.ErrCodeNotFound, message, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return operrors.Wrap(operrors.ErrCodeUnauthorized, message, err)
		case http.StatusTooManyRequests:
			return operrors.Wrap(operrors.ErrCodeRateLimitExceeded, message, err)
		}
	}
	return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, message)
}

func invalid(format string, args ...any) error {
	return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(format, args...))
}

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
	"encoding/json"
	"fmt"
	"io"
	"time"

	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// The one representation a MetadataListClient asks for and accepts.
const (
	partialObjectMetadataListKind   = "PartialObjectMetadataList"
	partialObjectMetadataListAccept = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
)

// MetadataLister lists one resource across all namespaces as a meta.k8s.io/v1
// PartialObjectMetadataList.
type MetadataLister interface {
	List(ctx context.Context, gvr schema.GroupVersionResource,
		opts metav1.ListOptions) (*metav1.PartialObjectMetadataList, error)
}

// MetadataListClient is a MetadataLister that accepts nothing but a
// PartialObjectMetadataList. Its Accept header carries no plain
// application/json fallback, so an apiserver that cannot serve the list as
// metadata answers 406 rather than sending every object in full, spec and
// Secret data included.
type MetadataListClient struct {
	client        *rest.RESTClient
	limit         int64
	retries       int
	retryInterval time.Duration
}

// NewMetadataListClientForConfig returns a MetadataListClient for restConfig,
// with the request timeout BoundedConfig applies. A nil restConfig is an
// ErrCodeInvalidRequest error.
func NewMetadataListClientForConfig(restConfig *rest.Config) (*MetadataListClient, error) {
	if restConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	cfg := BoundedConfig(restConfig)
	cfg.AcceptContentTypes = partialObjectMetadataListAccept
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.NegotiatedSerializer = metainternalversionscheme.Codecs.WithoutConversion()
	// Required by RESTClientFor but never sent, since List builds an absolute
	// path.
	cfg.GroupVersion = &schema.GroupVersion{}
	cfg.APIPath = "/this-value-should-never-be-sent"
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	restClient, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create metadata list client", err)
	}

	return &MetadataListClient{
		client:        restClient,
		limit:         defaults.MetadataListResponseLimit,
		retries:       defaults.MetadataListRetries,
		retryInterval: defaults.MetadataListRetryInterval,
	}, nil
}

// List lists gvr across all namespaces. It retries a request lost to a
// connection reset, a probable EOF or a lost HTTP/2 connection up to
// defaults.MetadataListRetries times. The error codes are:
//   - ErrCodeInvalidRequest if gvr has no version or resource
//   - ErrCodeUnavailable if the response is not a meta.k8s.io/v1
//     PartialObjectMetadataList, a 406 included, or exceeds
//     defaults.MetadataListResponseLimit
//   - ErrCodeCanceled or ErrCodeTimeout if ctx ends first
func (c *MetadataListClient) List(ctx context.Context, gvr schema.GroupVersionResource,
	opts metav1.ListOptions) (*metav1.PartialObjectMetadataList, error) {

	if gvr.Version == "" || gvr.Resource == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("cannot list metadata for %q, which names no version or resource", gvr.String()))
	}
	errCtx := map[string]any{"resource": gvr.String()}

	body, err := c.stream(ctx, gvr, opts)
	if err != nil {
		return nil, listErr(ctx, err, fmt.Sprintf("failed to list %s metadata", gvr.String()), errCtx)
	}
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(io.LimitReader(body, c.limit+1))
	if err != nil {
		return nil, listErr(ctx, err, fmt.Sprintf("failed to read the %s metadata list", gvr.String()), errCtx)
	}
	if int64(len(data)) > c.limit {
		return nil, errors.NewWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the %s metadata list exceeds %d bytes", gvr.String(), c.limit), errCtx)
	}

	// The error never quotes data, which may be a full object list.
	var list metav1.PartialObjectMetadataList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the apiserver answered a %s metadata list with a body that is not JSON", gvr.String()),
			err, errCtx)
	}
	if list.APIVersion != metav1.SchemeGroupVersion.String() || list.Kind != partialObjectMetadataListKind {
		return nil, errors.NewWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the apiserver answered a %s metadata list with kind %q in %q, not %s in %s",
				gvr.String(), list.Kind, list.APIVersion, partialObjectMetadataListKind,
				metav1.SchemeGroupVersion.String()),
			errCtx)
	}

	return &list, nil
}

// stream returns the body of a List of gvr, retrying a dropped connection. It
// returns ctx.Err() if ctx ends between attempts.
func (c *MetadataListClient) stream(ctx context.Context, gvr schema.GroupVersionResource,
	opts metav1.ListOptions) (io.ReadCloser, error) {

	for attempt := 0; ; attempt++ {
		body, err := c.client.Get().
			AbsPath(resourcePath(gvr)...).
			SpecificallyVersionedParams(&opts, metav1.ParameterCodec, metav1.SchemeGroupVersion).
			Stream(ctx)
		if err == nil || attempt >= c.retries || !retryableTransportErr(err) {
			return body, err
		}

		timer := time.NewTimer(c.retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// retryableTransportErr reports whether err is a connection reset, a probable
// EOF or a lost HTTP/2 connection.
func retryableTransportErr(err error) bool {
	// The errors client-go's rest.Request.Do retries a GET on.
	return utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err) || utilnet.IsHTTP2ConnectionLost(err)
}

// listErr classifies a failed request or read. A dead ctx keeps its
// cancellation or timeout, and anything else is ErrCodeUnavailable.
func listErr(ctx context.Context, err error, message string, errCtx map[string]any) error {
	if ctx.Err() != nil {
		return errors.WrapCtxErrWithContext(err, errors.ErrCodeTimeout, message, errCtx)
	}

	return errors.WrapWithContext(errors.ErrCodeUnavailable, message, err, errCtx)
}

// resourcePath is the cluster-wide collection path for gvr.
func resourcePath(gvr schema.GroupVersionResource) []string {
	if gvr.Group == "" {
		return []string{"api", gvr.Version, gvr.Resource}
	}

	return []string{"apis", gvr.Group, gvr.Version, gvr.Resource}
}

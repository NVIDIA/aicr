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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// UpstreamChart names a pinned chart in an upstream Helm repository.
type UpstreamChart struct {
	// Repository is the upstream repository URL; HTTPS outside tests.
	Repository string
	Chart      string
	Version    string
}

// MirroredChart is an upstream chart copied into the runtime registry.
type MirroredChart struct {
	// Repository is the runtime registry repository the chart was pushed to.
	Repository Repository
	// Digest is the chart's manifest digest in that repository.
	Digest string
	// ArchiveDigest is the verified upstream archive digest.
	ArchiveDigest string
	// Archive is the chart archive, for inspection by the planner.
	Archive []byte
}

// MirrorHTTPChart copies a chart from an HTTP(S) Helm repository into
// target/<chart>. The archive's SHA-256 must equal the digest the repository
// index records for that version; a mismatch is a conflict, because it means
// the upstream content changed after it was indexed. Upstreams in
// OCI registries are not mirrored here. The index and the archive are
// fetched over HTTPS, or over HTTP from a host in PlainHTTPHosts. The
// archive may be on another host than the index: chart-releaser
// repositories keep theirs on github.com.
func (c *Client) MirrorHTTPChart(ctx context.Context, up UpstreamChart, target Repository) (*MirroredChart, error) {
	base, err := url.Parse(up.Repository)
	if err != nil || base.Host == "" || !c.allowedScheme(base) {
		return nil, invalid("upstream repository %q must be an HTTPS URL, or HTTP to a host configured for plain HTTP", up.Repository)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()

	entry, err := c.indexEntry(ctx, base, up)
	if err != nil {
		return nil, err
	}
	// Index URLs may be relative to the repository, which is a directory.
	dir := *base
	if !strings.HasSuffix(dir.Path, "/") {
		dir.Path += "/"
	}
	archiveURL, err := dir.Parse(entry.URLs[0])
	if err != nil || !c.allowedScheme(archiveURL) {
		return nil, invalid("chart %s %s has an unusable URL %q", up.Chart, up.Version, entry.URLs[0])
	}
	archive, err := c.get(ctx, archiveURL.String(), defaults.MaxChartArchiveBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != entry.Digest {
		return nil, operrors.New(operrors.ErrCodeConflict, fmt.Sprintf(
			"chart %s %s archive digest %s does not match the index digest %s", up.Chart, up.Version, got, entry.Digest))
	}
	meta, err := ReadChartMeta(archive)
	if err != nil {
		return nil, err
	}
	if meta.Name != up.Chart || meta.Version != up.Version {
		return nil, invalid("archive holds chart %s %s, want %s %s", meta.Name, meta.Version, up.Chart, up.Version)
	}
	repo := target.Child(up.Chart)
	digest, err := c.PushChart(ctx, repo, archive)
	if err != nil {
		return nil, err
	}
	return &MirroredChart{Repository: repo, Digest: digest, ArchiveDigest: "sha256:" + entry.Digest, Archive: archive}, nil
}

// allowedScheme reports whether a chart URL may be fetched: over HTTPS, or
// over HTTP from a host configured for it.
func (c *Client) allowedScheme(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && slices.Contains(c.opts.PlainHTTPHosts, u.Host))
}

type indexEntry struct {
	Version string   `yaml:"version"`
	Digest  string   `yaml:"digest"`
	URLs    []string `yaml:"urls"`
}

// indexEntry fetches the repository index and returns the pinned version's
// entry, which must carry a digest and a URL.
func (c *Client) indexEntry(ctx context.Context, base *url.URL, up UpstreamChart) (*indexEntry, error) {
	indexURL := *base
	indexURL.Path = strings.TrimSuffix(indexURL.Path, "/") + "/index.yaml"
	raw, err := c.get(ctx, indexURL.String(), defaults.MaxChartIndexBytes)
	if err != nil {
		return nil, err
	}
	var index struct {
		Entries map[string][]indexEntry `yaml:"entries"`
	}
	if err = yaml.Unmarshal(raw, &index); err != nil {
		return nil, invalid("index of %s is not valid YAML: %v", up.Repository, err)
	}
	for i := range index.Entries[up.Chart] {
		e := &index.Entries[up.Chart][i]
		if e.Version != up.Version {
			continue
		}
		if len(e.Digest) != sha256.Size*2 || len(e.URLs) == 0 {
			return nil, invalid("index entry for %s %s has no digest or URL", up.Chart, up.Version)
		}
		return e, nil
	}
	return nil, operrors.New(operrors.ErrCodeNotFound,
		fmt.Sprintf("chart %s %s is not in the index of %s", up.Chart, up.Version, up.Repository))
}

// get fetches a URL with a size bound.
func (c *Client) get(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, invalid("request %s: %v", rawURL, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "fetch "+rawURL)
	}
	defer resp.Body.Close()
	if !c.allowedScheme(resp.Request.URL) {
		return nil, invalid("fetch %s: redirected to %s, which is neither HTTPS nor a plain-HTTP host", rawURL, resp.Request.URL.Redacted())
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, operrors.New(operrors.ErrCodeNotFound, fmt.Sprintf("fetch %s: %s", rawURL, resp.Status))
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, operrors.New(operrors.ErrCodeRateLimitExceeded, fmt.Sprintf("fetch %s: %s", rawURL, resp.Status))
	case resp.StatusCode != http.StatusOK:
		return nil, operrors.New(operrors.ErrCodeUnavailable, fmt.Sprintf("fetch %s: %s", rawURL, resp.Status))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read "+rawURL)
	}
	if int64(len(data)) > maxBytes {
		return nil, invalid("%s is larger than %d bytes", rawURL, maxBytes)
	}
	return data, nil
}

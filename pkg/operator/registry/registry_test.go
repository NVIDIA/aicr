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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// testRegistry serves an in-memory OCI registry over plain HTTP.
func testRegistry(t *testing.T) (*Client, Repository) {
	t.Helper()
	return testRegistryWith(t, false)
}

// testRegistryWith serves an in-memory OCI registry over plain HTTP, with or
// without the OCI 1.1 referrers API; without it, clients fall back to the
// referrers tag schema.
func testRegistryWith(t *testing.T, referrersAPI bool) (*Client, Repository) {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)), ggcrregistry.WithReferrersSupport(referrersAPI)))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return New(Options{PlainHTTPHosts: []string{host}}), Repository{Host: host, Path: "aicr/cluster"}
}

type tarEntry struct {
	name string
	body string
	flag byte
}

func targz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.flag
		if flag == 0 {
			flag = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: flag, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Unix(0, 0)}
		if flag != tar.TypeReg {
			h.Size = 0
			h.Linkname = "/etc/passwd"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func chartArchive(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	return targz(t, []tarEntry{
		{name: name + "/Chart.yaml", body: fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version)},
		{name: name + "/templates/cm.yaml", body: "kind: ConfigMap" + extra},
		{name: name + "/charts/sub/Chart.yaml", body: "apiVersion: v2\nname: sub\nversion: 0.0.1\n"},
	})
}

func TestParseRepository(t *testing.T) {
	t.Parallel()
	r, err := ParseRepository("oci://registry.local:5000/aicr/cluster")
	if err != nil || r.Host != "registry.local:5000" || r.Path != "aicr/cluster" {
		t.Fatalf("ParseRepository() = %+v, %v", r, err)
	}
	if got := r.Child("charts/x").URI(); got != "oci://registry.local:5000/aicr/cluster/charts/x" {
		t.Errorf("Child().URI() = %s", got)
	}
	for _, bad := range []string{"", "oci://host", "host/", "host/repo:tag", "host/repo@sha256:x", "host/repo/"} {
		if _, err := ParseRepository(bad); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("ParseRepository(%q) = %v, want INVALID_REQUEST", bad, err)
		}
	}
}

// The same archive always yields the same manifest digest; a different
// archive for an existing version is a conflict, never an overwrite.
func TestPushChartIsDeterministicAndNeverOverwrites(t *testing.T) {
	t.Parallel()
	c, repo := testRegistry(t)
	ctx := context.Background()
	archive := chartArchive(t, "demo", "1.2.3+build", "")
	target := repo.Child("charts/demo")

	first, err := c.PushChart(ctx, target, archive)
	if err != nil {
		t.Fatalf("PushChart() = %v", err)
	}
	again, err := c.PushChart(ctx, target, archive)
	if err != nil || again != first {
		t.Fatalf("PushChart() again = %s, %v; want %s", again, err, first)
	}
	resolved, err := c.Resolve(ctx, target, "1.2.3_build")
	if err != nil || resolved != first {
		t.Fatalf("Resolve() = %s, %v; want %s", resolved, err, first)
	}
	_, err = c.PushChart(ctx, target, chartArchive(t, "demo", "1.2.3+build", "\n# changed"))
	if operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("PushChart(changed) = %v, want CONFLICT", err)
	}
	if _, err = c.Resolve(ctx, target, "9.9.9"); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Fatalf("Resolve(missing) = %v, want NOT_FOUND", err)
	}
}

func TestReadChartMetaRejects(t *testing.T) {
	t.Parallel()
	tests := map[string][]byte{
		"not gzip":     []byte("plain"),
		"no Chart":     targz(t, []tarEntry{{name: "x/values.yaml", body: "a: 1"}}),
		"no version":   targz(t, []tarEntry{{name: "x/Chart.yaml", body: "name: x"}}),
		"invalid yaml": targz(t, []tarEntry{{name: "x/Chart.yaml", body: "name: [x"}}),
	}
	for name, archive := range tests {
		if _, err := ReadChartMeta(archive); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: ReadChartMeta() = %v, want INVALID_REQUEST", name, err)
		}
	}
}

// pushBundle pushes an AICR-shaped bundle artifact and returns its digest.
func pushBundle(t *testing.T, c *Client, r Repository, artifactType string, layer []byte) string {
	t.Helper()
	ctx := context.Background()
	repo, err := c.repository(r)
	if err != nil {
		t.Fatal(err)
	}
	empty := []byte("{}")
	config := ocispec.Descriptor{MediaType: ocispec.MediaTypeEmptyJSON, Digest: digest.FromBytes(empty), Size: 2}
	layerDesc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromBytes(layer), Size: int64(len(layer))}
	for _, b := range []struct {
		d    ocispec.Descriptor
		data []byte
	}{{config, empty}, {layerDesc, layer}} {
		if err = repo.Push(ctx, b.d, bytes.NewReader(b.data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			t.Fatal(err)
		}
	}
	manifest, _ := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType, Config: config, Layers: []ocispec.Descriptor{layerDesc},
	})
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest))}
	if err = repo.PushReference(ctx, desc, bytes.NewReader(manifest), "run-0123456789ab"); err != nil {
		t.Fatal(err)
	}
	return desc.Digest.String()
}

func TestPullBundle(t *testing.T) {
	t.Parallel()
	c, repo := testRegistry(t)
	ctx := context.Background()
	layer := targz(t, []tarEntry{
		{name: "./bundle-info.yaml", body: "kind: BundleInfo"},
		{name: "001-cert-manager/", flag: tar.TypeDir},
		{name: "001-cert-manager/values.yaml", body: "a: 1"},
	})
	d := pushBundle(t, c, repo, BundleArtifactType, layer)
	b, err := c.PullBundle(ctx, repo, d)
	if err != nil {
		t.Fatalf("PullBundle() = %v", err)
	}
	if string(b.Files["bundle-info.yaml"]) != "kind: BundleInfo" || b.Digest != d {
		t.Fatalf("bundle = %v", b.Paths())
	}
	if got := b.Dir("001-cert-manager"); len(got) != 1 || string(got["values.yaml"]) != "a: 1" {
		t.Fatalf("Dir() = %v", got)
	}
	if tag, err := c.Resolve(ctx, repo, "run-0123456789ab"); err != nil || tag != d {
		t.Fatalf("Resolve(run tag) = %s, %v", tag, err)
	}
}

func TestPullBundleRejects(t *testing.T) {
	t.Parallel()
	c, repo := testRegistry(t)
	ctx := context.Background()
	tests := []struct {
		name         string
		artifactType string
		layer        []byte
	}{
		{"not an AICR bundle", "application/vnd.cncf.helm.config.v1+json", targz(t, []tarEntry{{name: "a", body: "x"}})},
		{"symlink", BundleArtifactType, targz(t, []tarEntry{{name: "link", flag: tar.TypeSymlink}})},
		{"escaping path", BundleArtifactType, targz(t, []tarEntry{{name: "../escape", body: "x"}})},
		{"absolute path", BundleArtifactType, targz(t, []tarEntry{{name: "/etc/x", body: "x"}})},
		{"duplicate entry", BundleArtifactType, targz(t, []tarEntry{{name: "a", body: "x"}, {name: "./a", body: "y"}})},
		{"not gzip", BundleArtifactType, []byte("plain")},
	}
	for _, tt := range tests {
		d := pushBundle(t, c, repo, tt.artifactType, tt.layer)
		if _, err := c.PullBundle(ctx, repo, d); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: PullBundle() = %v, want INVALID_REQUEST", tt.name, err)
		}
	}
	if _, err := c.PullBundle(ctx, repo, "latest"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("PullBundle(tag) = %v, want INVALID_REQUEST", err)
	}
	missing := "sha256:" + strings.Repeat("0", 64)
	if _, err := c.PullBundle(ctx, repo, missing); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Errorf("PullBundle(missing) = %v, want NOT_FOUND", err)
	}
}

// upstream serves a Helm repository index and archives over HTTP.
func upstream(t *testing.T, archive []byte, indexDigest, url string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/charts/index.yaml", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n  - version: 2.0.0\n    digest: %s\n    urls: [x.tgz]\n  - version: 1.0.0\n    digest: %s\n    urls: [%s]\n",
			strings.Repeat("a", 64), indexDigest, url)
	})
	mux.HandleFunc("/charts/demo-1.0.0.tgz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/charts"
}

func TestMirrorHTTPChart(t *testing.T) {
	t.Parallel()
	c, repo := testRegistry(t)
	ctx := context.Background()
	archive := chartArchive(t, "demo", "1.0.0", "")
	sum := sha256.Sum256(archive)
	base := upstream(t, archive, hex.EncodeToString(sum[:]), "demo-1.0.0.tgz")

	m, err := c.MirrorHTTPChart(ctx, UpstreamChart{Repository: base, Chart: "demo", Version: "1.0.0"}, repo.Child("charts"))
	if err != nil {
		t.Fatalf("MirrorHTTPChart() = %v", err)
	}
	if m.Repository.Path != "aicr/cluster/charts/demo" || m.ArchiveDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("mirrored = %+v", m)
	}
	if got, _ := c.Resolve(ctx, m.Repository, "1.0.0"); got != m.Digest {
		t.Fatalf("registry digest = %s, want %s", got, m.Digest)
	}

	tampered := upstream(t, archive, strings.Repeat("b", 64), "demo-1.0.0.tgz")
	if _, err = c.MirrorHTTPChart(ctx, UpstreamChart{Repository: tampered, Chart: "demo", Version: "1.0.0"}, repo.Child("charts")); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("tampered archive = %v, want CONFLICT", err)
	}
	if _, err = c.MirrorHTTPChart(ctx, UpstreamChart{Repository: base, Chart: "demo", Version: "3.0.0"}, repo.Child("charts")); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Fatalf("missing version = %v, want NOT_FOUND", err)
	}
	if _, err = c.MirrorHTTPChart(ctx, UpstreamChart{Repository: "oci://ghcr.io/x", Chart: "demo", Version: "1.0.0"}, repo); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("OCI upstream = %v, want INVALID_REQUEST", err)
	}
	wrongName := chartArchive(t, "other", "1.0.0", "")
	wsum := sha256.Sum256(wrongName)
	mismatch := upstream(t, wrongName, hex.EncodeToString(wsum[:]), "demo-1.0.0.tgz")
	if _, err = c.MirrorHTTPChart(ctx, UpstreamChart{Repository: mismatch, Chart: "demo", Version: "1.0.0"}, repo.Child("charts")); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("archive of another chart = %v, want INVALID_REQUEST", err)
	}
}

func TestDockerConfigCredentials(t *testing.T) {
	t.Parallel()
	cfg := fmt.Sprintf(`{"auths":{"https://reg.example.com/v1/":{"auth":%q},"other:5000":{"username":"u","password":"p"}}}`,
		base64.StdEncoding.EncodeToString([]byte("AWS:token")))
	creds, err := DockerConfigCredentials([]byte(cfg))
	if err != nil {
		t.Fatalf("DockerConfigCredentials() = %v", err)
	}
	ctx := context.Background()
	if c, _ := creds(ctx, "reg.example.com"); c.Username != "AWS" || c.Password != "token" {
		t.Errorf("reg.example.com = %+v", c)
	}
	if c, _ := creds(ctx, "other:5000"); c.Username != "u" || c.Password != "p" {
		t.Errorf("other:5000 = %+v", c)
	}
	if c, _ := creds(ctx, "unknown"); c.Username != "" {
		t.Errorf("unknown host = %+v, want anonymous", c)
	}
	for _, bad := range []string{`nope`, `{"auths":{"h":{"auth":"%%%"}}}`, `{"auths":{"h":{"auth":"bm9jb2xvbg=="}}}`} {
		_, err := DockerConfigCredentials([]byte(bad))
		if operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest || strings.Contains(fmt.Sprint(err), "nocolon") {
			t.Errorf("DockerConfigCredentials(%s) = %v", bad, err)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	resp := func(code int) error { return &errcode.ErrorResponse{StatusCode: code} }
	tests := []struct {
		err  error
		want operrors.ErrorCode
	}{
		{errdef.ErrNotFound, operrors.ErrCodeNotFound},
		{resp(http.StatusNotFound), operrors.ErrCodeNotFound},
		{resp(http.StatusUnauthorized), operrors.ErrCodeUnauthorized},
		{resp(http.StatusForbidden), operrors.ErrCodeUnauthorized},
		{resp(http.StatusTooManyRequests), operrors.ErrCodeRateLimitExceeded},
		{resp(http.StatusBadGateway), operrors.ErrCodeUnavailable},
		{context.DeadlineExceeded, operrors.ErrCodeTimeout},
		{operrors.New(operrors.ErrCodeConflict, "kept"), operrors.ErrCodeConflict},
	}
	for _, tt := range tests {
		if got := operrors.CodeOf(classify(tt.err, "op")); got != tt.want {
			t.Errorf("classify(%v) = %s, want %s", tt.err, got, tt.want)
		}
	}
}

// An OCI upstream chart is verified against its layer digest and mirrored
// under the runtime registry; content that is not a single Helm chart, or a
// runtime tag that already holds other content, is refused.
func TestMirrorOCIChart(t *testing.T) {
	t.Parallel()
	_, runtime := testRegistry(t)
	_, upstreamRepo := testRegistry(t)
	c := New(Options{PlainHTTPHosts: []string{runtime.Host, upstreamRepo.Host}})
	ctx := context.Background()
	archive := chartArchive(t, "demo", "v1.0.0", "")
	upstream := upstreamRepo.Child("charts/demo")
	if _, err := c.PushChart(ctx, upstream, archive); err != nil {
		t.Fatal(err)
	}
	up := UpstreamChart{Chart: upstream.URI(), Version: "v1.0.0"}
	m, err := c.MirrorOCIChart(ctx, up, runtime.Child("charts"))
	if err != nil {
		t.Fatalf("MirrorOCIChart() = %v", err)
	}
	sum := sha256.Sum256(archive)
	if m.Repository.Path != "aicr/cluster/charts/demo" || m.ArchiveDigest != "sha256:"+hex.EncodeToString(sum[:]) || !bytes.Equal(m.Archive, archive) {
		t.Fatalf("mirrored = %+v", m)
	}
	again, err := c.MirrorOCIChart(ctx, up, runtime.Child("charts"))
	if err != nil || again.Digest != m.Digest {
		t.Fatalf("second mirror = %v, %v", again, err)
	}

	// The same version upstream now holds other content: never overwritten.
	if _, err = c.PushChart(ctx, upstreamRepo.Child("moved/demo"), chartArchive(t, "demo", "v1.0.0", "\n# changed")); err != nil {
		t.Fatal(err)
	}
	moved := UpstreamChart{Chart: upstreamRepo.Child("moved/demo").URI(), Version: "v1.0.0"}
	if _, err = c.MirrorOCIChart(ctx, moved, runtime.Child("charts")); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("changed upstream content = %v, want CONFLICT", err)
	}

	pushBundle(t, c, upstreamRepo.Child("bundle"), BundleArtifactType, targz(t, []tarEntry{{name: "a", body: "x"}}))
	for name, ref := range map[string]UpstreamChart{
		"not a chart":     {Chart: upstreamRepo.Child("bundle").URI(), Version: "run-0123456789ab"},
		"missing version": {Chart: upstream.URI(), Version: "v9.9.9"},
		"no version":      {Chart: upstream.URI()},
		"not OCI":         {Chart: "https://charts.example.com/demo", Version: "1"},
	} {
		_, mirrorErr := c.MirrorOCIChart(ctx, ref, runtime.Child("charts"))
		if code := operrors.CodeOf(mirrorErr); code != operrors.ErrCodeInvalidRequest && code != operrors.ErrCodeNotFound {
			t.Errorf("%s: MirrorOCIChart() = %v", name, mirrorErr)
		}
	}
	// The chart in the layer must be the chart the reference names.
	if _, err = c.PushChart(ctx, upstreamRepo.Child("charts/other"), chartArchive(t, "demo", "v2.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.MirrorOCIChart(ctx, UpstreamChart{Chart: upstreamRepo.Child("charts/other").URI(), Version: "v2.0.0"}, runtime); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("misnamed chart = %v, want INVALID_REQUEST", err)
	}
}

func wrapperFilesFor(templates string) map[string][]byte {
	return map[string][]byte{
		"Chart.yaml":                   []byte("apiVersion: v2\nname: gpu-operator-readiness\nversion: \"1.0.0\"\n"),
		"values.yaml":                  []byte("a: 1\n"),
		"cluster-values.yaml":          []byte("b: 2\n"),
		"install.sh":                   []byte("#!/bin/sh\n"),
		"templates/readiness.yaml":     []byte(templates),
		"templates/sub/configmap.yaml": []byte("kind: ConfigMap\n"),
	}
}

// Packaging the same files always yields the same bytes; deployment inputs
// are not chart content; different content gets a different tag.
func TestPackageWrapperIsDeterministic(t *testing.T) {
	t.Parallel()
	first, meta, err := PackageWrapper(wrapperFilesFor("kind: Job\n"))
	if err != nil {
		t.Fatalf("PackageWrapper() = %v", err)
	}
	for range 5 {
		again, _, againErr := PackageWrapper(wrapperFilesFor("kind: Job\n"))
		if againErr != nil || !bytes.Equal(again, first) {
			t.Fatal("repeated packaging produced different bytes")
		}
	}
	if meta.Name != "gpu-operator-readiness" || meta.Version != "1.0.0" {
		t.Fatalf("meta = %+v", meta)
	}
	gz, _ := gzip.NewReader(bytes.NewReader(first))
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, nextErr := tr.Next()
		if nextErr != nil {
			break
		}
		names = append(names, h.Name)
		if !h.ModTime.Equal(time.Unix(0, 0)) || h.Uid != 0 || h.Mode != 0o644 {
			t.Errorf("entry %s is not normalized: %+v", h.Name, h)
		}
	}
	want := []string{"gpu-operator-readiness/Chart.yaml", "gpu-operator-readiness/templates/readiness.yaml",
		"gpu-operator-readiness/templates/sub/configmap.yaml", "gpu-operator-readiness/values.yaml"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v\nwant %v", names, want)
	}
	if _, _, err = PackageWrapper(map[string][]byte{"values.yaml": nil}); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("no Chart.yaml = %v", err)
	}
	if _, _, err = PackageWrapper(map[string][]byte{"Chart.yaml": []byte("name: ../x\nversion: 1\n")}); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("unsafe name = %v", err)
	}

	c, repo := testRegistry(t)
	ctx := context.Background()
	target := repo.Child("wrappers/gpu-operator-readiness")
	a, err := c.PushWrapper(ctx, target, wrapperFilesFor("kind: Job\n"))
	if err != nil {
		t.Fatalf("PushWrapper() = %v", err)
	}
	b, err := c.PushWrapper(ctx, target, wrapperFilesFor("kind: Job\n"))
	if err != nil || b.Digest != a.Digest {
		t.Fatalf("repeated PushWrapper() = %v, %v", b, err)
	}
	changed, err := c.PushWrapper(ctx, target, wrapperFilesFor("kind: Job\nmetadata: {}\n"))
	if err != nil || changed.Digest == a.Digest {
		t.Fatalf("changed content = %v, %v; want a new digest", changed, err)
	}
}

// Plain HTTP applies only to the hosts configured for it: an upstream
// registry is always reached over HTTPS.
func TestPlainHTTPIsPerHost(t *testing.T) {
	t.Parallel()
	_, plain := testRegistry(t)
	c := New(Options{PlainHTTPHosts: []string{"registry.local:5000"}})
	_, err := c.Resolve(context.Background(), plain, "v1")
	if err == nil || !strings.Contains(err.Error(), "HTTP response to HTTPS client") {
		t.Fatalf("Resolve() over HTTPS = %v", err)
	}
}

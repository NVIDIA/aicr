// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// pushManifest stores a Helm-shaped chart manifest under repo:v1.0.0 in an
// in-memory registry. The registry does not check that referenced blobs exist,
// which is all this test needs: ociPublished reads only the manifest.
func pushManifest(t *testing.T, host, repo, annotations string) {
	t.Helper()
	body := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
	  "config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:` + strings.Repeat("a", 64) + `","size":1},
	  "layers":[],"annotations":` + annotations + `}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		"http://"+host+"/v2/"+repo+"/manifests/v1.0.0", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push manifest: status %d", resp.StatusCode)
	}
}

func TestOCIPublished(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	pushManifest(t, host, "charts/dated",
		`{"`+ocispec.AnnotationCreated+`":"2026-10-05T07:15:03-04:00"}`)
	pushManifest(t, host, "charts/undated", `{"org.opencontainers.image.title":"undated"}`)
	pushManifest(t, host, "charts/garbled", `{"`+ocispec.AnnotationCreated+`":"last tuesday"}`)
	pushManifest(t, host, "charts/reproducible", `{"`+ocispec.AnnotationCreated+`":"1970-01-01T00:00:00Z"}`)

	tests := []struct {
		name     string
		image    string
		want     time.Time
		wantCode errors.ErrorCode
	}{
		// nvsentinel's chart records a local offset; it must normalize to UTC
		// so the artifact does not mix zones.
		{"created annotation, normalized to UTC", host + "/charts/dated:v1.0.0",
			time.Date(2026, 10, 5, 11, 15, 3, 0, time.UTC), ""},
		{"no created annotation", host + "/charts/undated:v1.0.0", time.Time{}, errors.ErrCodeNotFound},
		{"unparseable created annotation", host + "/charts/garbled:v1.0.0", time.Time{}, errors.ErrCodeInvalidRequest},
		// Dated 1970, a pin would make the screen a silent no-op, and
		// candidates would all be withheld.
		{"reproducible-build epoch is no date", host + "/charts/reproducible:v1.0.0", time.Time{}, errors.ErrCodeNotFound},
		{"unknown tag", host + "/charts/dated:v9.9.9", time.Time{}, errors.ErrCodeUnavailable},
		{"malformed reference", "UPPER/Case:tag", time.Time{}, errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ociPublished(context.Background(), tt.image)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("want %s error, got %v", tt.wantCode, got)
				}
				if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
					t.Errorf("error %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("ociPublished: %v", err)
			}
			if !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Errorf("got %v, want %v in UTC", got, tt.want)
			}
		})
	}
}

// fakeRegistry answers publish-date lookups from a table and records what was
// asked, so a test can assert both what FillPublished wrote and that it never
// reached for a date it did not need.
type fakeRegistry struct {
	mu    sync.Mutex
	dates map[string]time.Time
	asked []string
}

func (f *fakeRegistry) published(_ context.Context, image string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, image)
	if ts, ok := f.dates[image]; ok {
		return ts, nil
	}
	return time.Time{}, errors.New(errors.ErrCodeUnavailable, "registry unreachable")
}

func TestFillPublished(t *testing.T) {
	const kai = "ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler"
	helmDate := mustTime(t, "2026-05-01T17:59:10Z")
	reg := &fakeRegistry{dates: map[string]time.Time{
		kai + ":v0.16.9": mustTime(t, "2026-08-16T10:29:50Z"),
		kai + ":v0.20.1": mustTime(t, "2025-11-30T13:27:49Z"),
		// v0.16.11 missing: its lookup fails.
	}}
	lookups := map[string]Lookup{
		kai: {DepName: kai, Datasource: "docker", Current: "v0.16.9", Candidates: []Candidate{
			{Version: "v0.16.11", UpdateType: "patch"},
			{Version: "v0.20.1", UpdateType: "minor"},
		}},
		// Renovate dates helm deps itself, and the dates it supplied must
		// survive untouched. One it left undated stays undated: a helm chart
		// name is not an OCI reference, so there is nothing to ask.
		"kube-prometheus-stack": {DepName: "kube-prometheus-stack", Datasource: "helm", Current: "84.4.0",
			CurrentPublished: helmDate,
			Candidates: []Candidate{
				{Version: "84.5.0", UpdateType: "minor", Published: helmDate},
				{Version: "92.2.0", UpdateType: "major"},
			}},
		// A current docker pin has nothing to screen.
		"ghcr.io/nvidia/nvsentinel": {DepName: "ghcr.io/nvidia/nvsentinel", Datasource: "docker", Current: "v1.28.0"},
		// An unresolved pin is never screened either.
		"ghcr.io/broken/chart": {DepName: "ghcr.io/broken/chart", Datasource: "docker", Current: "v1.0.0",
			Problem: "lookup failed", Candidates: []Candidate{{Version: "v1.1.0", UpdateType: "minor"}}},
	}

	FillPublished(context.Background(), lookups, reg.published)

	l := lookups[kai]
	if !l.CurrentPublished.Equal(mustTime(t, "2026-08-16T10:29:50Z")) {
		t.Errorf("CurrentPublished = %v", l.CurrentPublished)
	}
	if !l.Candidates[0].Published.IsZero() {
		t.Errorf("v0.16.11 Published = %v, want zero after a failed lookup", l.Candidates[0].Published)
	}
	if !l.Candidates[1].Published.Equal(mustTime(t, "2025-11-30T13:27:49Z")) {
		t.Errorf("v0.20.1 Published = %v", l.Candidates[1].Published)
	}
	if got := lookups["kube-prometheus-stack"]; !got.CurrentPublished.Equal(helmDate) || !got.Candidates[0].Published.Equal(helmDate) {
		t.Errorf("helm dates changed: %+v", got)
	}
	if len(reg.asked) != 3 {
		t.Errorf("asked the registry %d times (%v), want 3: only the screened docker dep's versions", len(reg.asked), reg.asked)
	}
	for _, image := range reg.asked {
		if !strings.HasPrefix(image, kai+":") {
			t.Errorf("asked for %q, which needs no publish date", image)
		}
	}
}

// An expired budget must leave dates unset -- which screen reports as
// unscreened -- rather than block or fabricate one.
func TestFillPublishedStopsWhenContextIsDone(t *testing.T) {
	reg := &fakeRegistry{dates: map[string]time.Time{"ghcr.io/x/chart:v1.0.0": time.Now()}}
	lookups := map[string]Lookup{"ghcr.io/x/chart": {DepName: "ghcr.io/x/chart", Datasource: "docker",
		Current: "v1.0.0", Candidates: []Candidate{{Version: "v1.1.0", UpdateType: "minor"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	FillPublished(ctx, lookups, reg.published)

	if len(reg.asked) != 0 {
		t.Errorf("asked the registry %v after the context was done", reg.asked)
	}
	if !lookups["ghcr.io/x/chart"].CurrentPublished.IsZero() {
		t.Error("a date was set without a lookup")
	}
}

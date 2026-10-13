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
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	// publishedBudget bounds every registry lookup in one run. The workflow
	// job has 20 minutes and Renovate spends most of them; whatever is still
	// undated when this expires is reported as unscreened.
	publishedBudget = 3 * time.Minute

	publishedFetchTimeout = 30 * time.Second

	publishedConcurrency = 4
)

// publishedFunc reports when the chart at image, a tag reference, was pushed.
type publishedFunc func(ctx context.Context, image string) (time.Time, error)

// ociPublished reads a chart's push time from its manifest's
// org.opencontainers.image.created annotation, which `helm push` records.
// Renovate's docker datasource reports no release dates, and every OCI chart
// the registry tracks carries this annotation. The lookup is anonymous: the
// charts are public, and the default keychain would run the caller's docker
// credential helpers for no gain.
func ociPublished(ctx context.Context, image string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, publishedFetchTimeout)
	defer cancel()

	fields := map[string]interface{}{"image": image}
	ref, err := name.ParseReference(image)
	if err != nil {
		return time.Time{}, errors.WrapWithContext(errors.ErrCodeInvalidRequest, "parse chart reference", err, fields)
	}
	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous))
	if err != nil {
		return time.Time{}, errors.WrapWithContext(errors.ErrCodeUnavailable, "fetch chart manifest", err, fields)
	}
	var manifest struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err = json.Unmarshal(desc.Manifest, &manifest); err != nil {
		return time.Time{}, errors.WrapWithContext(errors.ErrCodeInvalidRequest, "decode chart manifest", err, fields)
	}
	created, ok := manifest.Annotations[ocispec.AnnotationCreated]
	if !ok {
		return time.Time{}, errors.NewWithContext(errors.ErrCodeNotFound,
			"chart manifest has no "+ocispec.AnnotationCreated+" annotation", fields)
	}
	ts, err := time.Parse(time.RFC3339, created)
	if err != nil {
		fields["value"] = created
		return time.Time{}, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			"parse "+ocispec.AnnotationCreated+" annotation", err, fields)
	}
	// A reproducible build with SOURCE_DATE_EPOCH=0 stamps the Unix epoch,
	// which says nothing about when the chart was published.
	if !ts.After(time.Unix(0, 0)) {
		return time.Time{}, errors.NewWithContext(errors.ErrCodeNotFound,
			ocispec.AnnotationCreated+" annotation is the Unix epoch, a reproducible-build placeholder", fields)
	}
	return ts.UTC(), nil
}

// FillPublished dates the current version and every candidate of each docker
// dep that has candidates and no Problem, using fetch. Helm deps arrive dated
// from Renovate. A failed lookup leaves the date zero, which screen reports as
// unscreened; it is logged here and never fails the report, because one
// registry hiccup should cost one row's screen, not the weekly digest.
func FillPublished(ctx context.Context, lookups map[string]Lookup, fetch publishedFunc) {
	type job struct {
		dep       string
		candidate int // -1 for the current version
		image     string
		published time.Time
	}
	var jobs []*job
	for dep, l := range lookups {
		if l.Datasource != "docker" || l.Problem != "" || len(l.Candidates) == 0 {
			continue
		}
		if l.CurrentPublished.IsZero() {
			jobs = append(jobs, &job{dep: dep, candidate: -1, image: dep + ":" + l.Current})
		}
		for i, c := range l.Candidates {
			if c.Published.IsZero() {
				jobs = append(jobs, &job{dep: dep, candidate: i, image: dep + ":" + c.Version})
			}
		}
	}

	var g errgroup.Group
	g.SetLimit(publishedConcurrency)
	for _, j := range jobs {
		if ctx.Err() != nil {
			slog.Warn("publish-date budget exhausted; remaining charts are unscreened", "error", ctx.Err())
			break
		}
		g.Go(func() error {
			ts, err := fetch(ctx, j.image)
			if err != nil {
				slog.Warn("chart publish date unavailable", "image", j.image, "error", err)
				return nil
			}
			j.published = ts
			return nil
		})
	}
	_ = g.Wait() // every job reports its own failure and returns nil

	for _, j := range jobs {
		if j.published.IsZero() {
			continue
		}
		l := lookups[j.dep]
		if j.candidate < 0 {
			l.CurrentPublished = j.published
		} else {
			l.Candidates[j.candidate].Published = j.published
		}
		lookups[j.dep] = l
	}
}

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

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// pushTimeout bounds publishing a candidate.
const pushTimeout = 10 * time.Minute

// Helm's OCI chart media types, so `helm pull` and `helm install` read the
// published chart.
const (
	helmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	helmChartMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// push publishes a candidate as built, never rebuilt: the operator image
// index from its OCI layout to the repository candidate.json names, tagged
// with the candidate version, and the chart archive as a Helm OCI chart under
// chart-registry/<chart name>, tagged with the chart version. A tag that
// already names other content is refused, since a version names one
// candidate. Credentials come from CANDIDATE_REGISTRY_USERNAME and
// CANDIDATE_REGISTRY_PASSWORD. It prints what it published as key=value
// lines.
func push(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	dir := fs.String("dir", "dist/candidate", "Candidate directory.")
	chartRegistry := fs.String("chart-registry", "ghcr.io/nvidia/charts", "Registry path the chart is published under.")
	plainHTTP := fs.Bool("plain-http", false, "Push over plain HTTP; test registries only.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(*dir, "candidate.json"))
	if err != nil {
		return err
	}
	m := &Manifest{}
	if err = json.Unmarshal(data, m); err != nil {
		return fmt.Errorf("parse candidate.json: %w", err)
	}
	if m.Version == "" || m.Operator.Repository == "" || m.Operator.Digest == "" || m.Chart.Name == "" {
		return errors.New("candidate.json names no version, image or chart")
	}
	archive, err := os.ReadFile(filepath.Join(*dir, "chart", m.Chart.Name))
	if err != nil {
		return err
	}
	if sha256Of(archive) != m.Chart.Digest {
		return fmt.Errorf("chart %s is not the candidate's %s", m.Chart.Name, m.Chart.Digest)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	images, err := oci.NewFromFS(ctx, os.DirFS(filepath.Join(*dir, "image")))
	if err != nil {
		return fmt.Errorf("open the candidate's image layout: %w", err)
	}
	image, err := images.Resolve(ctx, m.Operator.Digest)
	if err != nil {
		return fmt.Errorf("the candidate's image layout: %w", err)
	}
	imageRepo, err := repository(m.Operator.Repository, *plainHTTP)
	if err != nil {
		return err
	}
	if err = publish(ctx, images, image, imageRepo, m.Version); err != nil {
		return err
	}

	charts := memory.New()
	name, version, chart, err := packChart(ctx, charts, archive, m.Source.CommitDate)
	if err != nil {
		return err
	}
	chartRepo, err := repository(*chartRegistry+"/"+name, *plainHTTP)
	if err != nil {
		return err
	}
	// Helm tags a chart with its version, "+" written as "_".
	if err = publish(ctx, charts, chart, chartRepo, strings.ReplaceAll(version, "+", "_")); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "image-repository=%s\nimage-digest=%s\nchart-repository=%s\nchart-version=%s\nchart-digest=%s\n",
		m.Operator.Repository, m.Operator.Digest, chartRepo.Reference.Registry+"/"+chartRepo.Reference.Repository, version, chart.Digest)
	return err
}

// repository is a remote repository using the candidate registry credentials.
func repository(ref string, plainHTTP bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("repository %s: %w", ref, err)
	}
	repo.PlainHTTP = plainHTTP
	repo.Client = &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.NewCache(),
		Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: os.Getenv("CANDIDATE_REGISTRY_USERNAME"), Password: os.Getenv("CANDIDATE_REGISTRY_PASSWORD"),
		}),
	}
	return repo, nil
}

// publish copies desc and everything it references from src to repo and tags
// it, then confirms the tag resolves to desc. Publishing the same content
// again is a no-op; a tag that already names other content is refused.
func publish(ctx context.Context, src content.ReadOnlyStorage, desc ocispec.Descriptor, repo *remote.Repository, tag string) error {
	ref := repo.Reference.Registry + "/" + repo.Reference.Repository + ":" + tag
	existing, err := repo.Resolve(ctx, tag)
	switch {
	case err == nil && existing.Digest != desc.Digest:
		return fmt.Errorf("%s already names %s, not this candidate's %s; a version names one candidate", ref, existing.Digest, desc.Digest)
	case err != nil && !errors.Is(err, errdef.ErrNotFound):
		return fmt.Errorf("resolve %s: %w", ref, err)
	}
	if err = oras.CopyGraph(ctx, src, repo, desc, oras.DefaultCopyGraphOptions); err != nil {
		return fmt.Errorf("push %s@%s: %w", ref, desc.Digest, err)
	}
	if err = repo.Tag(ctx, desc, tag); err != nil {
		return fmt.Errorf("tag %s: %w", ref, err)
	}
	tagged, err := repo.Resolve(ctx, tag)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", ref, err)
	}
	if tagged.Digest != desc.Digest {
		return fmt.Errorf("%s is %s, not the candidate's %s", ref, tagged.Digest, desc.Digest)
	}
	return nil
}

// packChart stores a chart archive in dst as a Helm OCI chart: Chart.yaml as
// JSON for the config and the archive, unchanged, as the one layer. The
// creation annotation is the candidate's commit date, so the same candidate
// always packs to the same digest.
func packChart(ctx context.Context, dst content.Pusher, archive []byte, created string) (name, version string, desc ocispec.Descriptor, err error) {
	meta, err := chartMetadata(archive)
	if err != nil {
		return "", "", desc, err
	}
	name, _ = meta["name"].(string)
	version, _ = meta["version"].(string)
	if name == "" || version == "" {
		return "", "", desc, errors.New("the chart's Chart.yaml names no name or version")
	}
	config, err := json.Marshal(meta)
	if err != nil {
		return "", "", desc, err
	}
	configDesc, err := oras.PushBytes(ctx, dst, helmConfigMediaType, config)
	if err != nil {
		return "", "", desc, err
	}
	layer, err := oras.PushBytes(ctx, dst, helmChartMediaType, archive)
	if err != nil {
		return "", "", desc, err
	}
	desc, err = oras.PackManifest(ctx, dst, oras.PackManifestVersion1_1, "", oras.PackManifestOptions{
		ConfigDescriptor: &configDesc,
		Layers:           []ocispec.Descriptor{layer},
		ManifestAnnotations: map[string]string{
			ocispec.AnnotationTitle:   name,
			ocispec.AnnotationVersion: version,
			ocispec.AnnotationCreated: created,
		},
	})
	return name, version, desc, err
}

// chartMetadata reads the top-level Chart.yaml from a chart archive.
func chartMetadata(archive []byte) (map[string]any, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("read the chart archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the chart archive has no Chart.yaml")
		}
		if err != nil {
			return nil, fmt.Errorf("read the chart archive: %w", err)
		}
		if dir, file := path.Split(h.Name); file != "Chart.yaml" || strings.Count(dir, "/") != 1 {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		meta := map[string]any{}
		if err = yaml.Unmarshal(data, &meta); err != nil {
			return nil, fmt.Errorf("parse Chart.yaml: %w", err)
		}
		return meta, nil
	}
}

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
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Manifest is a candidate's identity: everything qualification binds and
// a release must promote unchanged. Digest covers every other
// field, so any changed byte or input is a different candidate.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Version       string            `json:"version"`
	Source        Source            `json:"source"`
	Toolchain     Toolchain         `json:"toolchain"`
	Operator      Image             `json:"operator"`
	Chart         File              `json:"chart"`
	Plugin        map[string]string `json:"plugin"`
	SBOMs         map[string]string `json:"sboms"`
	AICR          AICR              `json:"aicr"`
	// Registry is the in-cluster registry image the chart defaults to.
	Registry Image `json:"registry"`
	// Inputs are the digests of the files that pin everything else.
	Inputs map[string]string `json:"inputs"`
	Digest string            `json:"digest,omitempty"`
}

// Source is the commit a candidate was built from.
type Source struct {
	Commit     string `json:"commit"`
	CommitDate string `json:"commitDate"`
}

// Toolchain is what built the candidate.
type Toolchain struct {
	Go        string `json:"go"`
	Ko        string `json:"ko"`
	BaseImage string `json:"baseImage"`
}

// Image is an image index and its platform images.
type Image struct {
	Repository string            `json:"repository"`
	Digest     string            `json:"digest"`
	Platforms  map[string]string `json:"platforms"`
}

// File is a file and its digest.
type File struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// AICR is the AICR release a candidate runs and is qualified with.
type AICR struct {
	Version         string   `json:"version"`
	Image           string   `json:"image"`
	ValidatorImages []string `json:"validatorImages"`
}

// Ref is the operator image reference, pinned by digest.
func (o Image) Ref() string { return o.Repository + "@" + o.Digest }

func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// seal sets the manifest digest over every other field.
func (m *Manifest) seal() error {
	m.Digest = ""
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	m.Digest = sha256Of(data)
	return nil
}

// ociIndex is the part of an OCI index the candidate reads.
type ociIndex struct {
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  *struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant,omitempty"`
		} `json:"platform,omitempty"`
	} `json:"manifests"`
}

// readImage reads the image index ko wrote to an OCI layout.
func readImage(layout, repository string) (Image, error) {
	top, err := readIndex(filepath.Join(layout, "index.json"))
	if err != nil {
		return Image{}, err
	}
	if len(top.Manifests) != 1 || !strings.HasSuffix(top.Manifests[0].MediaType, "image.index.v1+json") {
		return Image{}, fmt.Errorf("%s holds %d entries; want the one image index", layout, len(top.Manifests))
	}
	img := Image{Repository: repository, Digest: top.Manifests[0].Digest, Platforms: map[string]string{}}
	index, err := readIndex(blobPath(layout, img.Digest))
	if err != nil {
		return Image{}, err
	}
	for _, m := range index.Manifests {
		if p := m.Platform; p != nil {
			name := p.OS + "/" + p.Architecture
			if p.Variant != "" {
				name += "/" + p.Variant
			}
			img.Platforms[name] = m.Digest
		}
	}
	if len(img.Platforms) == 0 {
		return Image{}, fmt.Errorf("image index %s names no platform", img.Digest)
	}
	return img, nil
}

func readIndex(file string) (*ociIndex, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	idx := &ociIndex{}
	if err = json.Unmarshal(data, idx); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return idx, nil
}

func blobPath(layout, digest string) string {
	return filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// extractBinary writes the operator binary of one platform image, from its
// last layer, so a release build can be run as the candidate ships it.
func extractBinary(layout, platformDigest, binary, out string) error {
	data, err := os.ReadFile(blobPath(layout, platformDigest))
	if err != nil {
		return err
	}
	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse platform manifest %s: %w", platformDigest, err)
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("platform manifest %s has no layers", platformDigest)
	}
	f, err := os.Open(blobPath(layout, manifest.Layers[len(manifest.Layers)-1].Digest))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			return fmt.Errorf("no %s in the image's last layer", binary)
		}
		if nextErr != nil {
			return nextErr
		}
		if strings.TrimPrefix(h.Name, "/") != strings.TrimPrefix(binary, "/") {
			continue
		}
		w, createErr := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if createErr != nil {
			return createErr
		}
		if _, err = io.Copy(w, io.LimitReader(tr, 512<<20)); err != nil {
			_ = w.Close()
			return err
		}
		return w.Close()
	}
}

// hashDir maps every regular file in dir to its digest; a missing dir is
// empty.
func hashDir(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			return nil, readErr
		}
		out[e.Name()] = sha256Of(data)
	}
	return out, nil
}

// settings are the pins the candidate records from .settings.yaml.
type settings struct {
	Operator struct {
		AICRVersion         string            `yaml:"aicr_version"`
		AICRImage           string            `yaml:"aicr_image"`
		AICRValidatorImages map[string]string `yaml:"aicr_validator_images"`
	} `yaml:"operator"`
	BuildTools struct {
		Ko string `yaml:"ko"`
	} `yaml:"build_tools"`
}

func readSettings(file string) (*settings, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	s := &settings{}
	if err = yaml.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if s.Operator.AICRVersion == "" || !strings.Contains(s.Operator.AICRImage, "@sha256:") || len(s.Operator.AICRValidatorImages) == 0 || s.BuildTools.Ko == "" {
		return nil, fmt.Errorf("%s lacks the AICR version, image, validator images or ko pin", file)
	}
	return s, nil
}

// validators lists the validator pins in a stable order.
func (s *settings) validators() []string {
	out := make([]string, 0, len(s.Operator.AICRValidatorImages))
	for _, ref := range s.Operator.AICRValidatorImages {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// baseImage reads ko's pinned base image.
func baseImage(koYAML string) (string, error) {
	data, err := os.ReadFile(koYAML)
	if err != nil {
		return "", err
	}
	var ko struct {
		DefaultBaseImage string `yaml:"defaultBaseImage"`
	}
	if err = yaml.Unmarshal(data, &ko); err != nil {
		return "", err
	}
	if !strings.Contains(ko.DefaultBaseImage, "@sha256:") {
		return "", fmt.Errorf("%s base image %q is not pinned by digest", koYAML, ko.DefaultBaseImage)
	}
	return ko.DefaultBaseImage, nil
}

// values renders the candidate's deployment inputs: the images and AICR
// release it was qualified with, for `helm install -f`.
func values(m *Manifest) ([]byte, error) {
	const image = "image"
	doc := map[string]any{
		aicrValues: map[string]any{
			"version": m.AICR.Version, image: m.AICR.Image, "validatorImages": m.AICR.ValidatorImages,
		},
		operatorValues: map[string]any{image: m.Operator.Ref()},
		registryValues: map[string]any{"inCluster": map[string]any{image: m.Registry.Ref()}},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# Deployment inputs of candidate %s (%s), generated by tools/operator-release.\n", m.Version, m.Digest)
	return append([]byte(header), body...), nil
}

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

package bom

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"

	"aicr.run/pkg/errors"
)

// DeclaredImagesFile is the name of the file, beside a component's values in
// recipes/components/<name>/, that declares the images its operator launches.
const DeclaredImagesFile = "images.yaml"

const declaredRereadHint = "Read the images its operator launches at that version and add an entry, " +
	"in a --data overlay when the overlay moves the pin"

// DeclaredImages lists the images a component deploys that rendering its chart
// or manifests does not show. These are images an operator launches, injects or
// reads from an env value, and images its chart renders only behind a value the
// component's default values leave off.
//
// Each entry is recorded against the chart version, or the vendored operator
// image, it was read from, so a pin bump has no entry until someone re-reads
// the operator's images.
type DeclaredImages struct {
	Component string             `yaml:"component"`
	Charts    []DeclaredChart    `yaml:"charts,omitempty"`
	Manifests []DeclaredManifest `yaml:"manifests,omitempty"`
}

// DeclaredChart lists the declared images of one chart version.
type DeclaredChart struct {
	Repository    string          `yaml:"repository"`
	Chart         string          `yaml:"chart"`
	SourceVersion string          `yaml:"sourceVersion"`
	Images        []DeclaredImage `yaml:"images"`
}

// DeclaredManifest lists the declared images of a manifest-only component,
// identified by the operator image its vendored manifests deploy.
type DeclaredManifest struct {
	OperatorImage string          `yaml:"operatorImage"`
	Images        []DeclaredImage `yaml:"images"`
}

// DeclaredImage is one declared image reference.
//
// EnabledBy, when set, is the dot-separated values path that must be true in
// the component's resolved values for the image to be deployed. Only chart
// entries may set it. Source says where the reference comes from, such as the
// env var the operator reads.
type DeclaredImage struct {
	Image     string `yaml:"image"`
	EnabledBy string `yaml:"enabledBy,omitempty"`
	Source    string `yaml:"source"`
}

// ParseDeclaredImages decodes and validates the images.yaml document of the
// named component. It returns an ErrCodeInvalidRequest error for an unknown
// field, a missing required field, a second YAML document, a component other
// than component, a duplicate entry, enabledBy on a manifest entry, or an
// image that is not a valid reference with a tag or digest.
func ParseDeclaredImages(component string, data []byte) (*DeclaredImages, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var d DeclaredImages
	if err := dec.Decode(&d); err != nil {
		if stderrors.Is(err, io.EOF) {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "declared images document is empty")
		}
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse declared images", err)
	}
	var extra any
	if err := dec.Decode(&extra); !stderrors.Is(err, io.EOF) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "declared images file holds more than one YAML document")
	}
	if d.Component != component {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"declared images of component %q name component %q", component, d.Component))
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *DeclaredImages) validate() error {
	if len(d.Charts) == 0 && len(d.Manifests) == 0 {
		return d.invalid("declares no charts or manifests")
	}
	seen := map[string]struct{}{}
	for _, c := range d.Charts {
		if c.Repository == "" || c.Chart == "" || c.SourceVersion == "" {
			return d.invalid("chart entry needs repository, chart and sourceVersion")
		}
		key := fmt.Sprintf("chart %s %s@%s", strings.TrimSuffix(c.Repository, "/"), path.Base(c.Chart), c.SourceVersion)
		if err := d.checkEntry(seen, key, c.Images); err != nil {
			return err
		}
	}
	for _, m := range d.Manifests {
		if err := validImageRef(m.OperatorImage); err != nil {
			return d.invalid(fmt.Sprintf("manifest entry operatorImage %q: %v", m.OperatorImage, err))
		}
		key := "manifest " + m.OperatorImage
		if err := d.checkEntry(seen, key, m.Images); err != nil {
			return err
		}
		for _, i := range m.Images {
			if i.EnabledBy != "" {
				return d.invalid(fmt.Sprintf("%s: image %q sets enabledBy, which only chart entries support", key, i.Image))
			}
		}
	}
	return nil
}

func (d *DeclaredImages) checkEntry(seen map[string]struct{}, key string, images []DeclaredImage) error {
	if _, dup := seen[key]; dup {
		return d.invalid("duplicate entry for " + key)
	}
	seen[key] = struct{}{}
	if len(images) == 0 {
		return d.invalid(key + " lists no images")
	}
	refs := map[string]struct{}{}
	for _, i := range images {
		if err := validImageRef(i.Image); err != nil {
			return d.invalid(fmt.Sprintf("%s: image %q: %v", key, i.Image, err))
		}
		if i.Source == "" {
			return d.invalid(fmt.Sprintf("%s: image %q has no source", key, i.Image))
		}
		if _, dup := refs[i.Image]; dup {
			return d.invalid(fmt.Sprintf("%s lists image %q twice", key, i.Image))
		}
		refs[i.Image] = struct{}{}
	}
	return nil
}

func (d *DeclaredImages) invalid(msg string) error {
	return errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("declared images of component %q: %s", d.Component, msg))
}

// validImageRef rejects a reference that does not parse or that names neither
// a tag nor a digest, which a runtime would resolve to a moving latest tag.
func validImageRef(ref string) error {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return err
	}
	_, tagged := named.(reference.Tagged)
	_, digested := named.(reference.Digested)
	if !tagged && !digested {
		return stderrors.New("needs a tag or digest")
	}
	return nil
}

// Chart returns the entry for exactly the chart repository, chart and
// version, or nil if d has none.
func (d *DeclaredImages) Chart(repository, chart, version string) *DeclaredChart {
	for i := range d.Charts {
		c := &d.Charts[i]
		if c.Repository == repository && c.Chart == chart && c.SourceVersion == version {
			return c
		}
	}
	return nil
}

// ChartFor returns the entry for a deployed chart. Charts match by the last
// path segment of chart and by version, and the entry from repository wins.
// When repository has no entry for the chart at any version, as for a chart
// mirrored to an internal repository, the only entry from another repository
// with that chart and version applies. It returns an ErrCodeInvalidRequest
// error when no entry applies, or when several do.
func (d *DeclaredImages) ChartFor(repository, chart, version string) (*DeclaredChart, error) {
	var matches []*DeclaredChart
	repositoryKnown := false
	for i := range d.Charts {
		c := &d.Charts[i]
		if path.Base(c.Chart) != path.Base(chart) {
			continue
		}
		if strings.TrimSuffix(c.Repository, "/") == strings.TrimSuffix(repository, "/") {
			if c.SourceVersion == version {
				return c, nil
			}
			repositoryKnown = true
		} else if c.SourceVersion == version {
			matches = append(matches, c)
		}
	}
	deployed := fmt.Sprintf("chart %s %s@%s", repository, path.Base(chart), version)
	if repositoryKnown {
		matches = nil
	}
	switch len(matches) {
	case 0:
		return nil, d.invalid("no entry for " + deployed + ". " + declaredRereadHint)
	case 1:
		return matches[0], nil
	}
	return nil, d.invalid(fmt.Sprintf("%s matches entries from %d other repositories. Add an entry for %s",
		deployed, len(matches), repository))
}

// ManifestFor returns the entry whose operatorImage is in images. It returns
// an ErrCodeInvalidRequest error when none is.
func (d *DeclaredImages) ManifestFor(images []string) (*DeclaredManifest, error) {
	for i := range d.Manifests {
		if slices.Contains(images, d.Manifests[i].OperatorImage) {
			return &d.Manifests[i], nil
		}
	}
	return nil, d.invalid(fmt.Sprintf("no entry for an operator image its manifests deploy (%v). %s",
		images, declaredRereadHint))
}

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

// Command declared-images-diff compares the images a cluster runs, as
// recorded by k8s-aibom, with the images `aicr mirror list` reports for the
// cluster's recipe, and prints each running image the list does not cover.
// It finds images an images.yaml declaration missed.
//
// A running image with a digest is covered by a listed image with the same
// repository and digest, or with the same repository and tag and no digest.
// A running image without a digest is covered by a listed image with the same
// repository and tag.
//
// It runs offline on exported files. It exits 1 when a running image is not
// covered, and 2 when an input cannot be read, a BOM is not held inline, or
// the inputs hold no running container image.
//
// Usage:
//
//	aicr mirror list --recipe recipe.yaml --format json > mirror.json
//	kubectl get aiboms -A -o json > aiboms.json
//	go run ./tools/declared-images-diff -mirror-list mirror.json -aibom aiboms.json
//
// Each -aibom file is either one CycloneDX JSON document or a Kubernetes list
// of AIBOM resources, whose status.bomDocument.inline.data holds the document.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
)

// imageReferenceProperty is the CycloneDX property k8s-aibom sets on each
// container component to the image reference from the pod spec.
const imageReferenceProperty = "image.reference"

func main() {
	var (
		mirrorList string
		aiboms     []string
	)
	flag.StringVar(&mirrorList, "mirror-list", "", "`aicr mirror list` output, YAML or JSON")
	flag.Func("aibom", "CycloneDX document or AIBOM list exported from the cluster, repeatable", func(v string) error {
		aiboms = append(aiboms, v)
		return nil
	})
	flag.Parse()

	uncovered, err := run(mirrorList, aiboms)
	if err != nil {
		slog.Error("compare runtime images", "error", err)
		os.Exit(2)
	}
	for _, i := range uncovered {
		fmt.Println(i)
	}
	if len(uncovered) > 0 {
		slog.Error("running images not covered by the mirror list", "count", len(uncovered))
		os.Exit(1)
	}
	slog.Info("every running image is covered by the mirror list")
}

// run returns the sorted running images in the aibom files that the mirror
// list does not cover.
func run(mirrorListPath string, aibomPaths []string) ([]string, error) {
	if mirrorListPath == "" || len(aibomPaths) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "-mirror-list and at least one -aibom are required")
	}
	data, err := readBounded(mirrorListPath)
	if err != nil {
		return nil, err
	}
	covered, err := mirrorKeys(data)
	if err != nil {
		return nil, err
	}

	seen := 0
	uncovered := map[string]struct{}{}
	for _, p := range aibomPaths {
		data, err := readBounded(p)
		if err != nil {
			return nil, err
		}
		running, err := runningImages(data)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "read "+p)
		}
		seen += len(running)
		for _, ref := range running {
			ok, err := covered.covers(ref)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("running image %q in %s", ref, p), err)
			}
			if !ok {
				uncovered[ref] = struct{}{}
			}
		}
	}
	if seen == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"the -aibom inputs hold no running container image, so there is nothing to compare")
	}
	return slices.Sorted(maps.Keys(uncovered)), nil
}

// readBounded reads path, rejecting a file larger than defaults.MaxBOMBytes.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "open "+path, err)
	}
	defer f.Close() // read-only handle
	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxBOMBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+path, err)
	}
	if int64(len(data)) > defaults.MaxBOMBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s exceeds %d bytes", path, defaults.MaxBOMBytes))
	}
	return data, nil
}

// imageKeys indexes the images of a mirror list by normalized
// repository:tag and repository@digest.
type imageKeys struct {
	tags    map[string]struct{}
	tagOnly map[string]struct{}
	digests map[string]struct{}
}

// covers reports whether a listed image covers the running reference ref.
func (k imageKeys) covers(ref string) (bool, error) {
	tag, digest, err := refKeys(ref)
	if err != nil {
		return false, err
	}
	if digest != "" {
		_, byDigest := k.digests[digest]
		_, byTag := k.tagOnly[tag]
		return byDigest || (tag != "" && byTag), nil
	}
	_, byTag := k.tags[tag]
	return byTag, nil
}

// mirrorKeys returns the match keys of every image in an `aicr mirror list`
// document.
func mirrorKeys(data []byte) (imageKeys, error) {
	var list struct {
		Images []string `yaml:"images"`
	}
	keys := imageKeys{tags: map[string]struct{}{}, tagOnly: map[string]struct{}{}, digests: map[string]struct{}{}}
	if err := yaml.Unmarshal(data, &list); err != nil {
		return keys, errors.Wrap(errors.ErrCodeInvalidRequest, "parse mirror list", err)
	}
	if len(list.Images) == 0 {
		return keys, errors.New(errors.ErrCodeInvalidRequest, "mirror list has no images")
	}
	for _, ref := range list.Images {
		tag, digest, err := refKeys(ref)
		if err != nil {
			return keys, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("mirror list image %q", ref), err)
		}
		if tag != "" {
			keys.tags[tag] = struct{}{}
			if digest == "" {
				keys.tagOnly[tag] = struct{}{}
			}
		}
		if digest != "" {
			keys.digests[digest] = struct{}{}
		}
	}
	return keys, nil
}

// refKeys returns the normalized repository:tag and repository@digest forms
// of ref, each empty when ref has no tag or digest. A reference with neither
// resolves to the latest tag, as a container runtime does.
func refKeys(ref string) (tag, digest string, err error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", "", err
	}
	if t, ok := named.(reference.Tagged); ok {
		tag = named.Name() + ":" + t.Tag()
	}
	if d, ok := named.(reference.Digested); ok {
		digest = named.Name() + "@" + d.Digest().String()
	}
	if tag == "" && digest == "" {
		tag = named.Name() + ":latest"
	}
	return tag, digest, nil
}

// runningImages returns the container image references in data, a CycloneDX
// document or a list of AIBOM resources.
func runningImages(data []byte) ([]string, error) {
	var probe struct {
		BOMFormat string            `json:"bomFormat"`
		Items     []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse JSON", err)
	}
	if probe.BOMFormat == cdx.BOMFormat {
		return bomImages(data)
	}
	if probe.Items == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "neither a CycloneDX document nor a list of AIBOMs")
	}

	var images []string
	for _, raw := range probe.Items {
		var item struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status struct {
				BOMDocument *struct {
					Inline *struct {
						Data []byte `json:"data"`
					} `json:"inline"`
				} `json:"bomDocument"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse AIBOM", err)
		}
		doc := item.Status.BOMDocument
		if doc == nil || doc.Inline == nil || len(doc.Inline.Data) == 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"AIBOM %s/%s holds no inline BOM (truncated, external or not generated). "+
					"Export that document and pass it with -aibom", item.Metadata.Namespace, item.Metadata.Name))
		}
		found, err := bomImages(doc.Inline.Data)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("AIBOM %s/%s", item.Metadata.Namespace, item.Metadata.Name))
		}
		images = append(images, found...)
	}
	return images, nil
}

// bomImages returns the image reference of every container component in a
// CycloneDX JSON document, nested components included.
func bomImages(data []byte) ([]string, error) {
	var b cdx.BOM
	if err := cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatJSON).Decode(&b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode CycloneDX", err)
	}
	var images []string
	var walk func(cs *[]cdx.Component) error
	walk = func(cs *[]cdx.Component) error {
		if cs == nil {
			return nil
		}
		for _, c := range *cs {
			if c.Type == cdx.ComponentTypeContainer {
				ref := containerRef(c)
				if ref == "" {
					return errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("container component %q has no image reference", c.BOMRef))
				}
				images = append(images, ref)
			}
			if err := walk(c.Components); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(b.Components); err != nil {
		return nil, err
	}
	return images, nil
}

// containerRef returns the image.reference property of c, or else its name
// and version joined as name:version.
func containerRef(c cdx.Component) string {
	if c.Properties != nil {
		for _, p := range *c.Properties {
			if p.Name == imageReferenceProperty && p.Value != "" {
				return p.Value
			}
		}
	}
	if c.Name != "" && c.Version != "" {
		return c.Name + ":" + c.Version
	}
	return ""
}

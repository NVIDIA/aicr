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
	"context"
	stderrors "errors"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	imageReadTimeout = 10 * time.Minute
	maxCRDFileBytes  = 16 << 20
	imageKey         = "image"
)

// imageCRDSource names a container that applies CRDs from its own image, and
// the directory in that image that holds them.
type imageCRDSource struct {
	Container string
	Dir       string
}

// imageCRDSources lists the components whose operator applies CRDs from its
// own image at runtime, so the chart does not render them.
var imageCRDSources = map[string]imageCRDSource{
	"dynamo-platform":  {Container: "crd-apply", Dir: "opt/dynamo-operator/crds"},
	"network-operator": {Container: "network-operator", Dir: "manifests/state-nv-ipam-cni"},
}

// imageReader returns the names of the CRDs stored under dir in image.
type imageReader func(ctx context.Context, image, dir string) ([]string, error)

// containerImage returns the image of the container or init container named
// container in rendered. It returns an error unless exactly one distinct,
// non-empty image is found.
func containerImage(rendered []byte, container string) (string, error) {
	var images []string
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), decodeBufferSize)
	for {
		var doc struct {
			Spec struct {
				Template corev1.PodTemplateSpec `json:"template"`
			} `json:"spec"`
		}
		err := dec.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInvalidRequest, "decode rendered chart", err)
		}
		pod := doc.Spec.Template.Spec
		for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
			if c.Name == container && !slices.Contains(images, c.Image) {
				images = append(images, c.Image)
			}
		}
	}
	if len(images) != 1 || images[0] == "" {
		return "", errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			"expected exactly one image for the CRD container", nil,
			map[string]any{"container": container, "images": images})
	}
	return images[0], nil
}

// readImageCRDs returns the names of the CRDs stored under dir in the
// linux/amd64 variant of image.
func readImageCRDs(ctx context.Context, image, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, imageReadTimeout)
	defer cancel()

	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest, "parse image reference", err,
			map[string]any{imageKey: image})
	}
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeUnavailable, "fetch image", err,
			map[string]any{imageKey: image})
	}
	rc := mutate.Extract(img)
	defer rc.Close()
	names, err := crdsInTar(rc, dir)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeUnavailable, "read CRDs from image "+image)
	}
	return names, nil
}

// crdsInTar returns the sorted names of the CRDs defined in the .yaml and .yml
// files directly under dir in the tar stream r. Documents that do not parse as
// YAML are skipped. A file larger than maxCRDFileBytes is an error.
func crdsInTar(r io.Reader, dir string) ([]string, error) {
	dir = strings.Trim(dir, "/")
	names := sets.New[string]()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "read image filesystem", err)
		}
		file := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		ext := path.Ext(file)
		if hdr.Typeflag != tar.TypeReg || path.Dir(file) != dir || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxCRDFileBytes+1))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "read "+file, err)
		}
		if len(data) > maxCRDFileBytes {
			return nil, errors.New(errors.ErrCodeInvalidRequest, file+" exceeds the CRD file size limit")
		}
		addCRDNames(names, data)
	}
	return sets.List(names), nil
}

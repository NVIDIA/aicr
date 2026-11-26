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

// Package imagefs reads the YAML files an operator image carries, for tools
// that check repository data against what the image ships.
package imagefs

import (
	"archive/tar"
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

	"aicr.run/pkg/errors"
)

// MaxFileBytes is the largest file ReadYAML reads.
const MaxFileBytes = 16 << 20

const readTimeout = 10 * time.Minute

// ReadYAML returns the regular .yaml and .yml files directly under each of
// dirs in the linux/amd64 variant of image, keyed by path without a leading
// slash. A fetch or stream failure is an ErrCodeUnavailable error. A malformed
// reference, a file larger than MaxFileBytes, or a symlink or hard link with
// one of those extensions, whose content would go unread, is an
// ErrCodeInvalidRequest error.
func ReadYAML(ctx context.Context, image string, dirs ...string) (map[string][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	errCtx := map[string]any{"image": image}
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest, "parse image reference", err, errCtx)
	}
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeUnavailable, "fetch image", err, errCtx)
	}
	rc := mutate.Extract(img)
	defer rc.Close()
	return yamlInTar(rc, dirs)
}

// yamlInTar returns the .yaml and .yml files directly under each of dirs in
// the tar stream r, as ReadYAML describes.
func yamlInTar(r io.Reader, dirs []string) (map[string][]byte, error) {
	clean := make([]string, 0, len(dirs))
	for _, d := range dirs {
		clean = append(clean, strings.TrimPrefix(path.Clean("/"+d), "/"))
	}
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if stderrors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "read image filesystem", err)
		}
		file := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if ext := path.Ext(file); (ext != ".yaml" && ext != ".yml") || !slices.Contains(clean, path.Dir(file)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			return nil, errors.New(errors.ErrCodeInvalidRequest, file+" is a link, so its content would go unread")
		case tar.TypeReg:
		default:
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "read "+file, err)
		}
		if len(data) > MaxFileBytes {
			return nil, errors.New(errors.ErrCodeInvalidRequest, file+" exceeds the image file size limit")
		}
		files[file] = data
	}
}

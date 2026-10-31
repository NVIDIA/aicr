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

package oci

import (
	"context"
	stderrors "errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	oras "oras.land/oras-go/v2"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

// WriteLayout packages cfg.SourceDir exactly as PackageAndPush does and
// writes the artifact as an OCI image layout at cfg.Reference instead of
// pushing it, so its manifest digest is the one PackageAndPush would push.
// cfg.Reference must be an oci-layout reference with a tag, and its
// directory must not exist; on failure no layout is left behind.
func WriteLayout(ctx context.Context, cfg OutputConfig) (*PackageAndPushResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIBundlePublishTimeout)
	defer cancel()
	parent, name, err := layoutTarget(cfg.Reference)
	if err != nil {
		return nil, err
	}
	op, err := packageGenericOperation(ctx, PackageOptions{
		SourceDir:   cfg.SourceDir,
		OutputDir:   cfg.OutputDir,
		SourceFiles: cfg.SourceFiles,
		SubDir:      cfg.SubDir,
		Tag:         cfg.Reference.Tag,
		Annotations: cfg.manifestAnnotations(),
		layoutOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	return releaseLayout(ctx, op, cfg.Reference, parent, name)
}

// WriteHelmChartLayout packages a Helm chart exactly as
// PackageAndPushHelmChart does and writes it as an OCI image layout at
// opts.Reference instead of pushing it. The chart name comes from
// Chart.yaml. The same target rules as WriteLayout apply.
func WriteHelmChartLayout(ctx context.Context, opts HelmChartOptions) (*PackageAndPushResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIBundlePublishTimeout)
	defer cancel()
	parent, name, err := layoutTarget(opts.Reference)
	if err != nil {
		return nil, err
	}
	op, err := packageHelmOperation(ctx, opts)
	if err != nil {
		return nil, err
	}
	return releaseLayout(ctx, op, opts.Reference, parent, name)
}

// CheckLayoutTarget reports whether ref is a writable layout target: an
// oci-layout reference with a valid tag whose directory does not exist
// (ErrCodeConflict otherwise). WriteLayout repeats the check; calling it
// first lets a caller fail before generating content.
func CheckLayoutTarget(ref *Reference) error {
	_, _, err := layoutTarget(ref)
	return err
}

// layoutTarget validates ref and returns the parent directory and base name
// of its layout directory. An existing target fails before any packaging.
func layoutTarget(ref *Reference) (string, string, error) {
	if ref == nil || !ref.IsLayout {
		return "", "", apperrors.New(apperrors.ErrCodeInvalidRequest, "OCI layout reference is required")
	}
	if err := validateDistributionTag(ref.Tag); err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(ref.LocalPath)
	if err != nil {
		return "", "", apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"failed to resolve OCI layout path: "+ref.LocalPath, err)
	}
	name := filepath.Base(abs)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return "", "", apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI layout path must name a new directory: "+ref.LocalPath)
	}
	if _, statErr := os.Lstat(abs); statErr == nil {
		return "", "", apperrors.New(apperrors.ErrCodeConflict, "OCI layout target already exists: "+abs)
	} else if !stderrors.Is(statErr, fs.ErrNotExist) {
		return "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to inspect OCI layout target: "+abs, statErr)
	}
	return filepath.Dir(abs), name, nil
}

// releaseLayout copies the packaged graph into a new layout beside the
// target, renames it into place, and removes op's workspace layout. Copying
// keeps the workspace on its own filesystem while the final rename stays
// within the target's parent.
func releaseLayout(
	ctx context.Context,
	op *packageOperation,
	ref *Reference,
	parent, name string,
) (retResult *PackageAndPushResult, retErr error) {

	defer func() {
		if closeErr := op.Close(); closeErr != nil {
			if retErr == nil {
				retResult = nil
				retErr = closeErr
			} else {
				slog.Warn("failed to clean OCI package operation after primary error", "error", closeErr)
			}
		}
	}()
	localCtx, localCancel := context.WithTimeout(ctx, defaults.OCILocalPackageTimeout)
	defer localCancel()

	layout, err := newOwnedLayout(localCtx, parent)
	if err != nil {
		return nil, err
	}
	released := false
	defer func() {
		if released {
			return
		}
		if closeErr := layout.Close(); closeErr != nil {
			if retErr == nil {
				retResult = nil
				retErr = closeErr
			} else {
				slog.Warn("failed to remove OCI layout after primary error", "error", closeErr)
			}
		}
	}()
	store, err := newRootOCIStore(localCtx, layout)
	if err != nil {
		return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal, "failed to create OCI layout store")
	}
	if err = op.layout.validate(); err != nil {
		return nil, err
	}
	if err = oras.CopyGraph(localCtx, op.store, store, op.manifest, oras.CopyGraphOptions{}); err != nil {
		return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal, "failed to copy OCI artifact into layout")
	}
	if err = store.Tag(localCtx, op.manifest, ref.Tag); err != nil {
		return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal, "failed to tag OCI layout manifest")
	}
	// The store's ingest directory is its own scratch space, not layout content.
	if err = layout.child.Remove(rootStoreIngestDir); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to remove OCI layout ingest directory", err)
	}
	released = true
	path, err := layout.releaseAs(name)
	if err != nil {
		return nil, err
	}
	slog.Info("OCI artifact written to layout", "path", path, "tag", ref.Tag, "digest", op.result.Digest)
	return &PackageAndPushResult{
		Digest:    op.result.Digest,
		MediaType: op.result.MediaType,
		Size:      op.result.Size,
		Reference: ref.String(),
		StorePath: path,
	}, nil
}

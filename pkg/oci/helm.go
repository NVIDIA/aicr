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
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const (
	helmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	helmLayerMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// chartYAML is the bounded registry-visible subset of Chart.yaml. The source
// file itself is preserved byte-for-byte in the chart layer.
type chartYAML struct {
	APIVersion  string `yaml:"apiVersion" json:"apiVersion"`
	Name        string `yaml:"name" json:"name"`
	Version     string `yaml:"version" json:"version"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Type        string `yaml:"type,omitempty" json:"type,omitempty"`
	AppVersion  string `yaml:"appVersion,omitempty" json:"appVersion,omitempty"`
}

// HelmChartOptions configures immutable Helm OCI publication.
type HelmChartOptions struct {
	SourceDir   string
	OutputDir   string
	SourceFiles []string
	Reference   *Reference
	PlainHTTP   bool
	InsecureTLS bool
	Version     string
}

// PackageAndPushHelmChart creates a Helm-compatible OCI artifact without
// modifying Chart.yaml or any other caller source byte.
func PackageAndPushHelmChart(ctx context.Context, opts HelmChartOptions) (*PackageAndPushResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIBundlePublishTimeout)
	defer cancel()
	return packageAndPushHelmChartWithDependencies(ctx, opts, defaultHelmChartDependencies())
}

type helmChartDependencies struct {
	packageOperation func(context.Context, HelmChartOptions) (*packageOperation, error)
	pushOperation    func(context.Context, *packageOperation, PushOptions) (*PushResult, error)
	beforePush       func(*packageOperation) error
}

func defaultHelmChartDependencies() helmChartDependencies {
	return helmChartDependencies{
		packageOperation: packageHelmOperation,
		pushOperation:    pushPackageOperation,
	}
}

func packageAndPushHelmChartWithDependencies(
	ctx context.Context,
	opts HelmChartOptions,
	deps helmChartDependencies,
) (retResult *PackageAndPushResult, retErr error) {

	if err := validateHelmChartOptions(opts); err != nil {
		return nil, err
	}
	if err := contextError(ctx, "Helm OCI publication canceled"); err != nil {
		return nil, err
	}
	op, packageErr := deps.packageOperation(ctx, opts)
	if packageErr != nil {
		return nil, packageErr
	}
	released := false
	defer func() {
		if released {
			return
		}
		if closeErr := op.Close(); closeErr != nil {
			if retErr == nil {
				retResult = nil
				retErr = closeErr
			} else {
				slog.Warn("failed to clean Helm package operation after primary error",
					"error", closeErr, "primary", retErr)
			}
		}
	}()
	if deps.beforePush != nil {
		if err := deps.beforePush(op); err != nil {
			return nil, apperrors.PropagateOrWrap(
				err, apperrors.ErrCodeInternal, "Helm package-to-push check failed")
		}
	}
	if err := op.layout.validate(); err != nil {
		return nil, err
	}
	pushResult, pushErr := deps.pushOperation(ctx, op, PushOptions{
		Registry:    opts.Reference.Registry,
		Repository:  opts.Reference.Repository,
		Tag:         opts.Reference.Tag,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
	})
	if pushErr != nil {
		return nil, pushErr
	}
	if pushResult == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "Helm OCI push returned no result")
	}
	packageResult, releaseErr := op.release()
	if releaseErr != nil {
		return nil, releaseErr
	}
	released = true
	return &PackageAndPushResult{
		Digest:    pushResult.Digest,
		MediaType: pushResult.MediaType,
		Size:      pushResult.Size,
		Reference: pushResult.Reference,
		StorePath: packageResult.StorePath,
	}, nil
}

type helmPackageDependencies struct {
	prepareSource func(context.Context, string, string, []string) (*preparedSource, error)
	loadChart     func(context.Context, *preparedSource, string) (*chartYAML, error)
	newLayout     func(context.Context, string) (*ownedLayout, error)
	beforeLayout  func() error
	buildArchive  func(context.Context, *preparedSource, *ownedLayout, string) (string, ociv1.Descriptor, error)
	newStore      func(context.Context, *ownedLayout) (localOCIStore, error)
	beforeStore   func() error
	afterStore    func() error
	pushFileBlob  func(context.Context, localOCIStore, *ownedLayout, ociv1.Descriptor, string) error
	removeArchive func(*ownedLayout, string) error
	stageManifest func(context.Context, localOCIStore, *chartYAML, ociv1.Descriptor, []byte, string) (ociv1.Descriptor, error)
}

func defaultHelmPackageDependencies() helmPackageDependencies {
	generic := defaultGenericPackageDependencies()
	return helmPackageDependencies{
		prepareSource: func(
			ctx context.Context,
			sourceDir, outputDir string,
			sourceFiles []string,
		) (*preparedSource, error) {

			return preparePackageSource(ctx, sourceDir, outputDir, "", sourceFiles)
		},
		loadChart:    loadChartYAML,
		newLayout:    newOwnedLayout,
		buildArchive: buildHelmChartTGZ,
		newStore: func(ctx context.Context, layout *ownedLayout) (localOCIStore, error) {
			return newRootOCIStore(ctx, layout)
		},
		pushFileBlob:  pushFileBlob,
		removeArchive: generic.removeArchive,
		stageManifest: stageHelmOCIManifest,
	}
}

func packageHelmOperation(ctx context.Context, opts HelmChartOptions) (*packageOperation, error) {
	return packageHelmOperationWithDependencies(ctx, opts, defaultHelmPackageDependencies())
}

func packageHelmOperationWithDependencies(
	ctx context.Context,
	opts HelmChartOptions,
	deps helmPackageDependencies,
) (_ *packageOperation, retErr error) {

	if err := validateHelmChartOptions(opts); err != nil {
		return nil, err
	}
	stageCtx, stageCancel := context.WithTimeout(ctx, defaults.OCISourceStageTimeout)
	prepared, prepareErr := deps.prepareSource(stageCtx, opts.SourceDir, opts.OutputDir, opts.SourceFiles)
	stageCancel()
	if prepareErr != nil {
		return nil, prepareErr
	}
	defer func() {
		if closeErr := prepared.Close(); closeErr != nil {
			if retErr == nil {
				retErr = closeErr
			} else {
				slog.Warn("failed to close prepared Helm source after primary error",
					"error", closeErr, "primary", retErr)
			}
		}
	}()

	localCtx, localCancel := context.WithTimeout(ctx, defaults.OCILocalPackageTimeout)
	defer localCancel()
	meta, loadErr := deps.loadChart(localCtx, prepared, opts.Reference.Tag)
	if loadErr != nil {
		return nil, loadErr
	}
	if path.Base(opts.Reference.Repository) != meta.Name {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("OCI repository basename %q must match Chart.yaml name %q",
				path.Base(opts.Reference.Repository), meta.Name))
	}
	if err := runPackageHook(deps.beforeLayout, "Helm OCI layout creation precondition failed"); err != nil {
		return nil, err
	}
	if err := prepared.validate(); err != nil {
		return nil, err
	}
	layout, layoutErr := deps.newLayout(localCtx, opts.OutputDir)
	if layoutErr != nil {
		return nil, layoutErr
	}
	keepLayout := false
	defer func() {
		if keepLayout {
			return
		}
		if closeErr := layout.Close(); closeErr != nil {
			if retErr == nil {
				retErr = closeErr
			} else {
				slog.Warn("failed to remove Helm OCI layout after primary error",
					"error", closeErr, "primary", retErr)
			}
		}
	}()

	archiveName, archiveDescriptor, archiveErr := deps.buildArchive(
		localCtx, prepared, layout, meta.Name)
	if archiveErr != nil {
		return nil, archiveErr
	}
	if err := prepared.validate(); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if err := runPackageHook(deps.beforeStore, "Helm OCI store open precondition failed"); err != nil {
		return nil, err
	}
	store, storeErr := deps.newStore(localCtx, layout)
	if storeErr != nil {
		return nil, apperrors.PropagateOrWrap(
			storeErr, apperrors.ErrCodeInternal, "failed to create Helm OCI layout store")
	}
	if err := runPackageHook(deps.afterStore, "Helm OCI store open postcondition failed"); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if err := deps.pushFileBlob(localCtx, store, layout, archiveDescriptor, archiveName); err != nil {
		return nil, apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to push Helm chart archive into OCI layout")
	}
	if err := deps.removeArchive(layout, archiveName); err != nil {
		return nil, apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to remove staged Helm chart archive")
	}
	configBlob, marshalErr := json.Marshal(meta)
	if marshalErr != nil {
		return nil, apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to marshal Helm chart config", marshalErr)
	}
	manifest, manifestErr := deps.stageManifest(
		localCtx, store, meta, archiveDescriptor, configBlob, opts.Version)
	if manifestErr != nil {
		return nil, manifestErr
	}
	if err := store.Tag(localCtx, manifest, opts.Reference.Tag); err != nil {
		return nil, apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to tag Helm OCI manifest")
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if err := prepared.Close(); err != nil {
		return nil, err
	}
	manifest = immutableDescriptor(manifest)
	refString := fmt.Sprintf("%s/%s:%s", stripProtocol(opts.Reference.Registry),
		opts.Reference.Repository, opts.Reference.Tag)
	op := &packageOperation{
		layout:   layout,
		store:    store,
		manifest: manifest,
		result: PackageResult{
			Descriptor: manifest,
			Digest:     manifest.Digest.String(),
			MediaType:  manifest.MediaType,
			Size:       manifest.Size,
			Reference:  refString,
			StorePath:  layout.Path(),
		},
	}
	keepLayout = true
	return op, nil
}

func validateHelmChartOptions(opts HelmChartOptions) error {
	if opts.Reference == nil || !opts.Reference.IsOCI {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI reference is required for PackageAndPushHelmChart")
	}
	if opts.Reference.Tag == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required for Helm OCI push")
	}
	if _, err := HelmChartVersionFromTag(opts.Reference.Tag); err != nil {
		return err
	}
	if err := validateRegistryReference(opts.Reference.Registry, opts.Reference.Repository); err != nil {
		return err
	}
	if opts.SourceFiles != nil && !containsSourceFile(opts.SourceFiles, "Chart.yaml") {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"SourceFiles must explicitly include Chart.yaml")
	}
	return nil
}

func containsSourceFile(files []string, expected string) bool {
	for _, file := range files {
		if file == expected {
			return true
		}
	}
	return false
}

func loadChartYAML(
	ctx context.Context,
	source *preparedSource,
	expectedTag string,
) (*chartYAML, error) {

	if source == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "prepared Helm source is required")
	}
	if !containsSourceFile(source.relativeFiles(), "Chart.yaml") {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"selected Helm chart files do not include Chart.yaml")
	}
	if err := source.validate(); err != nil {
		return nil, err
	}
	file, openErr := source.open(ctx, "Chart.yaml")
	if openErr != nil {
		return nil, openErr
	}
	reader := newContextReadCloser(ctx, file)
	info, statErr := file.Stat()
	if statErr != nil {
		closeErr := reader.Close()
		return nil, wrapContextualIOFailure(
			ctx, stderrors.Join(statErr, closeErr),
			apperrors.ErrCodeInternal, "failed to inspect and close retained Chart.yaml")
	}
	if info.Size() > defaults.MaxChartYAMLBytes {
		primary := apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Chart.yaml exceeds %d bytes", defaults.MaxChartYAMLBytes))
		if closeErr := reader.Close(); closeErr != nil {
			slog.Warn("failed to close oversized retained Chart.yaml after primary error",
				"error", closeErr, "primary", primary)
		}
		return nil, primary
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, defaults.MaxChartYAMLBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, wrapContextualIOFailure(
			ctx, stderrors.Join(readErr, closeErr),
			apperrors.ErrCodeInternal, "failed to read and verify retained Chart.yaml")
	}
	if err := source.validate(); err != nil {
		return nil, err
	}
	var meta chartYAML
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "failed to parse Chart.yaml", err)
	}
	if meta.Name == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "Chart.yaml: name is required")
	}
	switch meta.APIVersion {
	case "v1", "v2":
	default:
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Chart.yaml: apiVersion must be v1 or v2; got %q", meta.APIVersion))
	}
	if err := validateChartName(meta.Name); err != nil {
		return nil, err
	}
	if meta.Version == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "Chart.yaml: version is required")
	}
	encoded, encodeErr := HelmTagFromChartVersion(meta.Version)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if encoded != expectedTag {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("Chart.yaml version %q encodes to tag %q, expected %q",
				meta.Version, encoded, expectedTag))
	}
	return &meta, nil
}

func validateChartName(name string) error {
	if name == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "chart name is required")
	}
	if !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\\x00") {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("chart name %q must be one safe path segment", name))
	}
	return nil
}

func buildHelmChartTGZ(
	ctx context.Context,
	source *preparedSource,
	layout *ownedLayout,
	chartName string,
) (string, ociv1.Descriptor, error) {

	if err := validateChartName(chartName); err != nil {
		return "", ociv1.Descriptor{}, err
	}
	name, descriptor, err := buildDeterministicTarGzip(
		ctx, source, layout, archiveOptions{Prefix: chartName})
	if err != nil {
		return "", ociv1.Descriptor{}, err
	}
	descriptor.MediaType = helmLayerMediaType
	descriptor.Annotations = nil
	return name, descriptor, nil
}

func stageHelmOCIManifest(
	ctx context.Context,
	store localOCIStore,
	meta *chartYAML,
	chartDescriptor ociv1.Descriptor,
	configBlob []byte,
	aicrVersion string,
) (ociv1.Descriptor, error) {

	configDescriptor := content.NewDescriptorFromBytes(helmConfigMediaType, configBlob)
	configReader := newContextReadCloser(ctx, io.NopCloser(bytes.NewReader(configBlob)))
	pushErr := store.Push(ctx, configDescriptor, configReader)
	closeErr := configReader.Close()
	if pushErr != nil {
		return ociv1.Descriptor{}, apperrors.PropagateOrWrap(
			pushErr, apperrors.ErrCodeInternal, "failed to add Helm chart config to OCI store")
	}
	if closeErr != nil {
		return ociv1.Descriptor{}, apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to close Helm chart config reader", closeErr)
	}

	annotations := map[string]string{
		ociv1.AnnotationCreated:           reproducibleTimestamp,
		ociv1.AnnotationTitle:             meta.Name,
		ociv1.AnnotationVersion:           meta.Version,
		"org.opencontainers.image.vendor": "NVIDIA",
	}
	if meta.Description != "" {
		annotations[ociv1.AnnotationDescription] = meta.Description
	}
	if aicrVersion != "" {
		annotations["com.nvidia.aicr.version"] = aicrVersion
	}
	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_0,
		helmConfigMediaType, oras.PackManifestOptions{
			Layers:              []ociv1.Descriptor{chartDescriptor},
			ConfigDescriptor:    &configDescriptor,
			ManifestAnnotations: annotations,
		})
	if err != nil {
		return ociv1.Descriptor{}, apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to pack Helm OCI manifest")
	}
	return manifest, nil
}

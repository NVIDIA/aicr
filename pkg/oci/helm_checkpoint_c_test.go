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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

var checkpointCHelmFiles = []string{
	"Chart.yaml",
	"templates/_helpers.tpl",
	"templates/deployment.yaml",
	"values.yaml",
}

func writeCheckpointCHelmChart(t *testing.T, version string) string {
	t.Helper()
	const chartName = "aicr-bundle"
	dir := writeHelmChartFixture(t, chartName)
	chart := "apiVersion: v2\nname: " + chartName + "\nversion: " + version +
		"\ndescription: AICR test chart\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chart), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

func prepareCheckpointCHelmSource(t *testing.T, source string, files []string) *preparedSource {
	t.Helper()
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), source, t.TempDir(), "", files)
	if err != nil {
		t.Fatalf("preparePackageSource() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Errorf("prepared.Close() error = %v", closeErr)
		}
	})
	return prepared
}

func snapshotCheckpointCHelmBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte, len(checkpointCHelmFiles))
	for _, rel := range checkpointCHelmFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		result[rel] = data
	}
	return result
}

func assertCheckpointCHelmBytes(t *testing.T, root string, want map[string][]byte) {
	t.Helper()
	for rel, expected := range want {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(data, expected) {
			t.Errorf("%s changed: got %q want %q", rel, data, expected)
		}
	}
}

func TestLoadChartYAMLRetainedSourceIsImmutable(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3+build.5")
	wantSource := snapshotCheckpointCHelmBytes(t, source)
	prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)
	wantStaged := snapshotCheckpointCHelmBytes(t, prepared.dir)

	meta, err := loadChartYAML(context.Background(), prepared, "1.2.3_build.5")
	if err != nil {
		t.Fatalf("loadChartYAML() error = %v", err)
	}
	if meta.Name != "aicr-bundle" || meta.Version != "1.2.3+build.5" {
		t.Fatalf("chart metadata = %+v", meta)
	}
	assertCheckpointCHelmBytes(t, source, wantSource)
	assertCheckpointCHelmBytes(t, prepared.dir, wantStaged)
}

func TestLoadChartYAMLRejectsVersionMismatchWithoutMutation(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3+build.5")
	want := snapshotCheckpointCHelmBytes(t, source)
	prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)

	meta, err := loadChartYAML(context.Background(), prepared, "1.2.4")
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if meta != nil {
		t.Fatalf("loadChartYAML() metadata = %+v on mismatch", meta)
	}
	assertCheckpointCHelmBytes(t, source, want)
}

func TestLoadChartYAMLRequiresExplicitSelection(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	prepared := prepareCheckpointCHelmSource(t, source, []string{"values.yaml"})

	meta, err := loadChartYAML(context.Background(), prepared, "1.2.3")
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if meta != nil {
		t.Fatalf("loadChartYAML() metadata = %+v without selected Chart.yaml", meta)
	}
}

func TestLoadChartYAMLRejectsMissingOrUnsupportedAPIVersion(t *testing.T) {
	for _, tt := range []struct {
		name       string
		apiVersion string
	}{
		{name: "missing"},
		{name: "unsupported", apiVersion: "v3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := writeCheckpointCHelmChart(t, "1.2.3")
			chart := "name: aicr-bundle\nversion: 1.2.3\n"
			if tt.apiVersion != "" {
				chart = "apiVersion: " + tt.apiVersion + "\n" + chart
			}
			if err := os.WriteFile(filepath.Join(source, "Chart.yaml"), []byte(chart), 0o640); err != nil {
				t.Fatal(err)
			}
			want := snapshotCheckpointCHelmBytes(t, source)
			prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)
			meta, err := loadChartYAML(context.Background(), prepared, "1.2.3")
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if meta != nil {
				t.Fatalf("loadChartYAML() metadata = %+v for apiVersion %q", meta, tt.apiVersion)
			}
			assertCheckpointCHelmBytes(t, source, want)
		})
	}
}

func TestLoadChartYAMLRejectsStagedPathSwapBeforeRead(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)
	moved := prepared.dir + "-moved"
	if err := os.Rename(prepared.dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(prepared.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("apiVersion: v2\nname: replacement\nversion: 1.2.3\n")
	if err := os.WriteFile(filepath.Join(prepared.dir, "Chart.yaml"), replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	meta, err := loadChartYAML(context.Background(), prepared, "1.2.3")
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if meta != nil {
		t.Fatalf("loadChartYAML() metadata = %+v after path swap", meta)
	}
	got, readErr := os.ReadFile(filepath.Join(prepared.dir, "Chart.yaml"))
	if readErr != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement Chart.yaml changed or consumed: bytes=%q err=%v", got, readErr)
	}

	if removeErr := os.RemoveAll(prepared.dir); removeErr != nil {
		t.Fatal(removeErr)
	}
	if renameErr := os.Rename(moved, prepared.dir); renameErr != nil {
		t.Fatal(renameErr)
	}
}

func TestLoadChartYAMLRejectsStagedFileCorruption(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)
	corrupt, readErr := os.ReadFile(filepath.Join(prepared.dir, "Chart.yaml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	nameOffset := bytes.Index(corrupt, []byte("aicr-bundle"))
	if nameOffset < 0 {
		t.Fatalf("Chart.yaml has no expected name: %q", corrupt)
	}
	corrupt[nameOffset] = 'x'
	if err := os.WriteFile(filepath.Join(prepared.dir, "Chart.yaml"), corrupt, 0o640); err != nil {
		t.Fatal(err)
	}
	meta, err := loadChartYAML(context.Background(), prepared, "1.2.3")
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if meta != nil {
		t.Fatalf("loadChartYAML() metadata = %+v after staged-file corruption", meta)
	}
}

func TestLoadChartYAMLOversizePreservesInvalidRequestBeforeCheckedClose(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	chart := []byte("apiVersion: v2\nname: aicr-bundle\nversion: 1.2.3\ndescription: " +
		strings.Repeat("a", int(defaults.MaxChartYAMLBytes)) + "\n")
	if err := os.WriteFile(filepath.Join(source, "Chart.yaml"), chart, 0o640); err != nil {
		t.Fatal(err)
	}
	prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)
	corrupt := append([]byte(nil), chart...)
	corrupt[0] = 0
	if err := os.WriteFile(filepath.Join(prepared.dir, "Chart.yaml"), corrupt, 0o640); err != nil {
		t.Fatal(err)
	}
	meta, err := loadChartYAML(context.Background(), prepared, "1.2.3")
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if meta != nil {
		t.Fatalf("loadChartYAML() metadata = %+v after oversized staged-file corruption", meta)
	}
}

func TestBuildHelmChartTGZStreamsSelectedFilesAndDescriptor(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3+build.5")
	prepared := prepareCheckpointCHelmSource(t, source, checkpointCHelmFiles)
	layout := newTestOwnedLayout(t, t.TempDir())

	archiveName, descriptor, err := buildHelmChartTGZ(
		context.Background(), prepared, layout, "aicr-bundle")
	if err != nil {
		t.Fatalf("buildHelmChartTGZ() error = %v", err)
	}
	archivePath := filepath.Join(layout.Path(), archiveName)
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.MediaType != helmLayerMediaType ||
		descriptor.Size != int64(len(data)) || descriptor.Digest != digest.FromBytes(data) {

		t.Fatalf("descriptor = %+v for archive size=%d digest=%s", descriptor, len(data), digest.FromBytes(data))
	}
	files, entries := readArchiveFiles(t, archivePath)
	wantEntries := []string{
		"aicr-bundle/",
		"aicr-bundle/Chart.yaml",
		"aicr-bundle/templates/",
		"aicr-bundle/templates/_helpers.tpl",
		"aicr-bundle/templates/deployment.yaml",
		"aicr-bundle/values.yaml",
	}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("archive entries = %v, want %v", entries, wantEntries)
	}
	if len(files) != len(checkpointCHelmFiles) {
		t.Fatalf("archive files = %v, want exactly %d selected files", files, len(checkpointCHelmFiles))
	}
}

func TestPackageAndPushHelmChartRejectsRepositoryBasenameBeforeLocalIO(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	safeOCITempRoot(t)
	deps := defaultHelmPackageDependencies()
	var layoutCalls atomic.Int32
	deps.newLayout = func(context.Context, string) (*ownedLayout, error) {
		layoutCalls.Add(1)
		return nil, stderrors.New("local layout must not be created")
	}

	op, err := packageHelmOperationWithDependencies(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   t.TempDir(),
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: "ghcr.io", Repository: "test/not-the-chart", Tag: "1.2.3", IsOCI: true,
		},
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
	if op != nil || layoutCalls.Load() != 0 {
		t.Fatalf("mismatch reached local I/O: op=%+v layoutCalls=%d", op, layoutCalls.Load())
	}
}

func TestPackageAndPushHelmChartPublishesRawTagWithoutMutatingSource(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3+build.5")
	want := snapshotCheckpointCHelmBytes(t, source)
	registry := newFakeOCIRegistry(t)
	t.Cleanup(registry.Close)

	result, err := PackageAndPushHelmChart(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   t.TempDir(),
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry:   strings.TrimPrefix(registry.URL, "http://"),
			Repository: "test/aicr-bundle",
			Tag:        "1.2.3_build.5",
			IsOCI:      true,
		},
		PlainHTTP: true,
		Version:   "v0.9.0-test",
	})
	if err != nil {
		t.Fatalf("PackageAndPushHelmChart() error = %v", err)
	}
	if result == nil || !strings.HasSuffix(result.Reference, ":1.2.3_build.5") {
		t.Fatalf("result = %+v, want raw Distribution tag", result)
	}
	if _, statErr := os.Stat(result.StorePath); statErr != nil {
		t.Fatalf("released Helm layout missing after complete remote push: %v", statErr)
	}
	assertCheckpointCHelmBytes(t, source, want)
}

func TestPackageAndPushHelmChartRetainsFrozenManifestAndStore(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	safeOCITempRoot(t)
	op, err := packageHelmOperation(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   t.TempDir(),
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3", IsOCI: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.Close() })
	want := op.manifest
	if op.layout == nil || op.store == nil || reflect.DeepEqual(want, ociv1.Descriptor{}) {
		t.Fatalf("incomplete Helm operation: %+v", op)
	}
	if err := op.store.Tag(context.Background(), want, "9.9.9"); err != nil {
		t.Fatalf("retag local store: %v", err)
	}

	target := newTestPublicationTarget()
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
	result, pushErr := pushPackageOperationWithDependencies(context.Background(), op, PushOptions{
		Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3",
	}, deps)
	if pushErr != nil {
		t.Fatalf("pushPackageOperationWithDependencies() error = %v", pushErr)
	}
	if result.Digest != want.Digest.String() || !content.Equal(target.tags["1.2.3"], want) {
		t.Fatalf("push used mutable local tag: result=%+v tagged=%+v want=%+v", result, target.tags["1.2.3"], want)
	}
}

func TestPackageAndPushHelmChartTempInsideSourceFailsBeforeLocalIO(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	output := t.TempDir()
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	want := snapshotCheckpointCHelmBytes(t, source)
	t.Setenv("TMPDIR", source)
	deps := defaultHelmPackageDependencies()
	var layoutCalls atomic.Int32
	deps.newLayout = func(context.Context, string) (*ownedLayout, error) {
		layoutCalls.Add(1)
		return nil, stderrors.New("unexpected local layout creation")
	}
	op, err := packageHelmOperationWithDependencies(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   output,
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3", IsOCI: true,
		},
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if op != nil || layoutCalls.Load() != 0 {
		t.Fatalf("unsafe temp reached local I/O: op=%+v layoutCalls=%d", op, layoutCalls.Load())
	}
	assertCheckpointCHelmBytes(t, source, want)
	assertDirectoryEmpty(t, output)
}

func TestPackageAndPushHelmChartLocalPhaseCancellationReturnsTimeout(t *testing.T) {
	for _, phase := range []string{"chart read", "archive", "store open", "local push"} {
		t.Run(phase, func(t *testing.T) {
			source := writeCheckpointCHelmChart(t, "1.2.3")
			output := t.TempDir()
			safeOCITempRoot(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deps := defaultHelmPackageDependencies()
			switch phase {
			case "chart read":
				deps.loadChart = func(callCtx context.Context, _ *preparedSource, _ string) (*chartYAML, error) {
					cancel()
					return nil, contextError(callCtx, "chart read canceled")
				}
			case "archive":
				deps.buildArchive = func(callCtx context.Context, _ *preparedSource, _ *ownedLayout, _ string) (string, ociv1.Descriptor, error) {
					cancel()
					return "", ociv1.Descriptor{}, contextError(callCtx, "archive canceled")
				}
			case "store open":
				deps.newStore = func(callCtx context.Context, _ *ownedLayout) (localOCIStore, error) {
					cancel()
					return nil, contextError(callCtx, "store open canceled")
				}
			case "local push":
				deps.pushFileBlob = func(callCtx context.Context, _ localOCIStore, _ *ownedLayout, _ ociv1.Descriptor, _ string) error {
					cancel()
					return contextError(callCtx, "local push canceled")
				}
			}
			op, err := packageHelmOperationWithDependencies(ctx, HelmChartOptions{
				SourceDir:   source,
				OutputDir:   output,
				SourceFiles: checkpointCHelmFiles,
				Reference: &Reference{
					Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3", IsOCI: true,
				},
			}, deps)
			assertErrorCode(t, err, apperrors.ErrCodeTimeout)
			if op != nil {
				t.Fatalf("canceled %s returned operation", phase)
			}
			assertDirectoryEmpty(t, output)
		})
	}
}

func TestPackageAndPushHelmChartRemoteFailureRemovesOwnedLayout(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	output := t.TempDir()
	safeOCITempRoot(t)
	deps := defaultHelmChartDependencies()
	var layoutPath string
	deps.packageOperation = func(ctx context.Context, opts HelmChartOptions) (*packageOperation, error) {
		op, err := packageHelmOperation(ctx, opts)
		if op != nil {
			layoutPath = op.layout.Path()
		}
		return op, err
	}
	deps.pushOperation = func(context.Context, *packageOperation, PushOptions) (*PushResult, error) {
		return nil, apperrors.New(apperrors.ErrCodeUnavailable, "remote failed")
	}
	result, err := packageAndPushHelmChartWithDependencies(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   output,
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3", IsOCI: true,
		},
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	if result != nil || layoutPath == "" {
		t.Fatalf("remote failure result=%+v layout=%q", result, layoutPath)
	}
	if _, statErr := os.Lstat(layoutPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed Helm layout remains: %v", statErr)
	}
	assertDirectoryEmpty(t, output)
}

func TestPackageAndPushHelmChartRemoteCancellationReturnsTimeout(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	output := t.TempDir()
	safeOCITempRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	deps := defaultHelmChartDependencies()
	var layoutPath string
	deps.packageOperation = func(callCtx context.Context, opts HelmChartOptions) (*packageOperation, error) {
		op, err := packageHelmOperation(callCtx, opts)
		if op != nil {
			layoutPath = op.layout.Path()
		}
		return op, err
	}
	deps.pushOperation = func(callCtx context.Context, _ *packageOperation, _ PushOptions) (*PushResult, error) {
		cancel()
		return nil, contextError(callCtx, "remote push canceled")
	}
	result, err := packageAndPushHelmChartWithDependencies(ctx, HelmChartOptions{
		SourceDir:   source,
		OutputDir:   output,
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3", IsOCI: true,
		},
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if result != nil || layoutPath == "" {
		t.Fatalf("remote cancellation result=%+v layout=%q", result, layoutPath)
	}
	if _, statErr := os.Lstat(layoutPath); !os.IsNotExist(statErr) {
		t.Fatalf("canceled Helm layout remains: %v", statErr)
	}
	assertDirectoryEmpty(t, output)
}

func TestPackageAndPushHelmChartPathSwapStopsBeforeRegistryIO(t *testing.T) {
	source := writeCheckpointCHelmChart(t, "1.2.3")
	output := t.TempDir()
	safeOCITempRoot(t)
	deps := defaultHelmChartDependencies()
	var replacement string
	deps.beforePush = func(op *packageOperation) error {
		original := op.layout.Path()
		if err := os.Rename(original, original+"-moved"); err != nil {
			return err
		}
		if err := os.Mkdir(original, 0o700); err != nil {
			return err
		}
		replacement = original
		return os.WriteFile(filepath.Join(original, "sentinel"), []byte("replacement"), 0o600)
	}
	var registryCalled atomic.Bool
	deps.pushOperation = func(context.Context, *packageOperation, PushOptions) (*PushResult, error) {
		registryCalled.Store(true)
		return nil, stderrors.New("unexpected registry call")
	}
	result, err := packageAndPushHelmChartWithDependencies(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   output,
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: "ghcr.io", Repository: "test/aicr-bundle", Tag: "1.2.3", IsOCI: true,
		},
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if result != nil || registryCalled.Load() {
		t.Fatalf("path swap result=%+v registryCalled=%v", result, registryCalled.Load())
	}
	data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel"))
	if readErr != nil || string(data) != "replacement" {
		t.Fatalf("replacement changed: data=%q err=%v", data, readErr)
	}
}

func TestHelmV4_2_3ExplicitVersionPull(t *testing.T) {
	settingsRaw, settingsReadErr := os.ReadFile(filepath.Join("..", "..", ".settings.yaml"))
	if settingsReadErr != nil {
		t.Fatalf("read .settings.yaml: %v", settingsReadErr)
	}
	var settings struct {
		TestingTools struct {
			Helm string `yaml:"helm"`
		} `yaml:"testing_tools"`
	}
	if settingsParseErr := yaml.Unmarshal(settingsRaw, &settings); settingsParseErr != nil {
		t.Fatalf("parse .settings.yaml: %v", settingsParseErr)
	}
	pin := settings.TestingTools.Helm
	if pin != "v4.2.3" {
		t.Fatalf("testing_tools.helm = %q, want v4.2.3", pin)
	}
	helmPath, lookupErr := exec.LookPath("helm")
	if lookupErr != nil {
		t.Fatalf("helm %s is required: %v", pin, lookupErr)
	}
	versionOut, versionErr := exec.Command(helmPath, "version", "--template", "{{.Version}}").CombinedOutput()
	if versionErr != nil {
		t.Fatalf("helm version: %v: %s", versionErr, versionOut)
	}
	if got := strings.TrimSpace(string(versionOut)); got != pin {
		t.Fatalf("installed helm version = %q, want exact pin %q", got, pin)
	}

	source := writeCheckpointCHelmChart(t, "1.2.3+build.5")
	registry := newFakeOCIRegistry(t)
	t.Cleanup(registry.Close)
	host := strings.TrimPrefix(registry.URL, "http://")
	_, publishErr := PackageAndPushHelmChart(context.Background(), HelmChartOptions{
		SourceDir:   source,
		OutputDir:   t.TempDir(),
		SourceFiles: checkpointCHelmFiles,
		Reference: &Reference{
			Registry: host, Repository: "test/aicr-bundle", Tag: "1.2.3_build.5", IsOCI: true,
		},
		PlainHTTP: true,
	})
	if publishErr != nil {
		t.Fatalf("publish chart: %v", publishErr)
	}
	repository := "oci://" + host + "/test/aicr-bundle"
	show := exec.Command(helmPath, "show", "chart", repository,
		"--version", "1.2.3+build.5", "--plain-http")
	show.Env = append(os.Environ(), "HELM_CACHE_HOME="+t.TempDir(), "HELM_CONFIG_HOME="+t.TempDir())
	showOut, showErr := show.CombinedOutput()
	if showErr != nil {
		t.Fatalf("helm show chart: %v: %s", showErr, showOut)
	}
	chartStart := bytes.Index(showOut, []byte("apiVersion:"))
	if chartStart < 0 {
		t.Fatalf("helm show output has no chart metadata: %s", showOut)
	}
	var shown chartYAML
	if chartParseErr := yaml.Unmarshal(showOut[chartStart:], &shown); chartParseErr != nil {
		t.Fatalf("parse helm show output: %v: %s", chartParseErr, showOut)
	}
	if shown.Version != "1.2.3+build.5" {
		t.Fatalf("helm show version = %q", shown.Version)
	}
	pullDir := t.TempDir()
	pull := exec.Command(helmPath, "pull", repository,
		"--version", "1.2.3+build.5", "--plain-http", "--destination", pullDir)
	pull.Env = append(os.Environ(), "HELM_CACHE_HOME="+t.TempDir(), "HELM_CONFIG_HOME="+t.TempDir())
	if pullOut, pullErr := pull.CombinedOutput(); pullErr != nil {
		t.Fatalf("helm pull: %v: %s", pullErr, pullOut)
	}
	if _, statErr := os.Stat(filepath.Join(pullDir, "aicr-bundle-1.2.3+build.5.tgz")); statErr != nil {
		t.Fatalf("pulled chart missing: %v", statErr)
	}
}

func TestOCIRegistryErrorCodeContract(t *testing.T) {
	rootBytes := storedTestManifestBytes("registry-code")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rootBytes)), nil
		},
	}
	tests := []struct {
		name     string
		status   int
		attempts int
		want     apperrors.ErrorCode
	}{
		{name: "unauthorized", status: 401, attempts: 1, want: apperrors.ErrCodeUnauthorized},
		{name: "forbidden", status: 403, attempts: 1, want: apperrors.ErrCodeUnauthorized},
		{name: "not found", status: 404, attempts: 1, want: apperrors.ErrCodeNotFound},
		{name: "conflict", status: 409, attempts: 1, want: apperrors.ErrCodeConflict},
		{name: "terminal 4xx", status: 422, attempts: 1, want: apperrors.ErrCodeInvalidRequest},
		{name: "exhausted rate limit", status: 429, attempts: 2, want: apperrors.ErrCodeRateLimitExceeded},
		{name: "exhausted 5xx", status: 503, attempts: 2, want: apperrors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := defaultPushOperationDependencies()
			deps.newTarget = func(PushOptions) (publicationTarget, error) { return newTestPublicationTarget(), nil }
			deps.maxAttempts = tt.attempts
			deps.initialBackoff = 0
			deps.copyGraph = func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
				return &errcode.ErrorResponse{StatusCode: tt.status}
			}
			result, err := pushFrozenDescriptor(context.Background(), source, root, PushOptions{
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, apperrors.ErrCodeInternal, func() error { return nil }, deps)
			assertErrorCode(t, err, tt.want)
			if result != nil {
				t.Fatalf("result = %+v on registry error", result)
			}
		})
	}
}

func TestOCIRegistryErrorCodeTransportCleanup(t *testing.T) {
	rootBytes := storedTestManifestBytes("registry-transport-cleanup")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rootBytes)), nil
		},
	}
	for _, tt := range []struct {
		name    string
		copyErr error
		wantErr bool
	}{
		{name: "success"},
		{name: "failure", copyErr: &errcode.ErrorResponse{StatusCode: http.StatusUnprocessableEntity}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := newTestPublicationTarget()
			target.resolve = func(context.Context, string) (ociv1.Descriptor, error) { return root, nil }
			deps := defaultPushOperationDependencies()
			deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
			deps.maxAttempts = 1
			deps.copyGraph = func(
				context.Context,
				content.ReadOnlyStorage,
				content.Storage,
				ociv1.Descriptor,
				oras.CopyGraphOptions,
			) error {

				return tt.copyErr
			}
			result, err := pushFrozenDescriptor(context.Background(), source, root, PushOptions{
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, apperrors.ErrCodeInternal, func() error { return nil }, deps)
			if tt.wantErr {
				assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
				if result != nil {
					t.Fatalf("result = %+v on failed publication", result)
				}
			} else if err != nil || result == nil {
				t.Fatalf("publication result=%+v error=%v", result, err)
			}
			if got := target.idleCloses.Load(); got != 1 {
				t.Fatalf("CloseIdleConnections calls = %d, want 1", got)
			}
		})
	}
}

func TestOCIRegistryTimeoutContract(t *testing.T) {
	rootBytes := storedTestManifestBytes("registry-timeout")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rootBytes)), nil
		},
	}
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return newTestPublicationTarget(), nil }
	deps.maxAttempts = 2
	deps.initialBackoff = 0
	deps.perAttemptTimeout = 10 * time.Millisecond
	deps.copyGraph = func(ctx context.Context, _ content.ReadOnlyStorage, _ content.Storage, _ ociv1.Descriptor, _ oras.CopyGraphOptions) error {
		<-ctx.Done()
		return ctx.Err()
	}
	result, err := pushFrozenDescriptor(context.Background(), source, root, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, apperrors.ErrCodeInternal, func() error { return nil }, deps)
	assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
	if result != nil {
		t.Fatalf("result = %+v on exhausted attempt deadlines", result)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = pushFrozenDescriptor(ctx, source, root, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, apperrors.ErrCodeInternal, func() error { return nil }, deps)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if result != nil {
		t.Fatalf("result = %+v on parent cancellation", result)
	}
}

func TestOCIRegistryNilAttemptTimeoutContract(t *testing.T) {
	rootBytes := storedTestManifestBytes("registry-nil-attempt-timeout")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	source := &testReadOnlyStorage{
		fetch: func(context.Context, ociv1.Descriptor) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rootBytes)), nil
		},
	}
	newDependencies := func() pushOperationDependencies {
		deps := defaultPushOperationDependencies()
		deps.newTarget = func(PushOptions) (publicationTarget, error) {
			return newTestPublicationTarget(), nil
		}
		deps.maxAttempts = 2
		deps.initialBackoff = 0
		return deps
	}
	options := PushOptions{Registry: "ghcr.io", Repository: "test/repo", Tag: "v1"}

	t.Run("parent cancellation is timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		deps := newDependencies()
		var attempts atomic.Int32
		deps.pushAttempt = func(
			attemptCtx context.Context,
			_ content.ReadOnlyStorage,
			_ publicationTarget,
			_ ociv1.Descriptor,
			_ string,
			_ apperrors.ErrorCode,
			_ oras.CopyGraphOptions,
			_ copyGraphFunc,
		) error {

			attempts.Add(1)
			cancel()
			<-attemptCtx.Done()
			return nil
		}
		result, err := pushFrozenDescriptor(
			ctx, source, root, options, apperrors.ErrCodeInternal, func() error { return nil }, deps)
		assertErrorCode(t, err, apperrors.ErrCodeTimeout)
		if result != nil || attempts.Load() != 1 {
			t.Fatalf("result=%+v attempts=%d, want nil,1", result, attempts.Load())
		}
	})

	t.Run("attempt expiry retries then becomes unavailable", func(t *testing.T) {
		deps := newDependencies()
		deps.perAttemptTimeout = 5 * time.Millisecond
		var attempts atomic.Int32
		deps.pushAttempt = func(
			attemptCtx context.Context,
			_ content.ReadOnlyStorage,
			_ publicationTarget,
			_ ociv1.Descriptor,
			_ string,
			_ apperrors.ErrorCode,
			_ oras.CopyGraphOptions,
			_ copyGraphFunc,
		) error {

			attempts.Add(1)
			<-attemptCtx.Done()
			return nil
		}
		result, err := pushFrozenDescriptor(
			context.Background(), source, root, options,
			apperrors.ErrCodeInternal, func() error { return nil }, deps)
		assertErrorCode(t, err, apperrors.ErrCodeUnavailable)
		if result != nil || attempts.Load() != int32(deps.maxAttempts) {
			t.Fatalf("result=%+v attempts=%d, want nil,%d", result, attempts.Load(), deps.maxAttempts)
		}
	})
}

func TestRegistryClientContextHasNoTotalTimeout(t *testing.T) {
	client, _ := createAuthClientForHost("localhost", true, false)
	if client == nil || client.Client == nil {
		t.Fatal("createAuthClientForHost() returned no HTTP client")
	}
	if client.Client.Timeout != 0 {
		t.Fatalf("registry HTTP client timeout = %v, want context-governed zero", client.Client.Timeout)
	}
}

func TestPushFrozenDescriptorCancelsAcceptedUploadAndClosesSource(t *testing.T) {
	configBytes := bytes.Repeat([]byte("accepted-upload"), 4*1024)
	config := content.NewDescriptorFromBytes(ociv1.MediaTypeImageConfig, configBytes)
	manifestBytes, marshalErr := json.Marshal(ociv1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ociv1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ociv1.Descriptor{},
	})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	manifest := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, manifestBytes)
	uploadReader := newCheckpointCPrefixThenBlockReader(configBytes, 16*1024)
	source := &testReadOnlyStorage{
		fetch: func(_ context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
			switch desc.Digest {
			case manifest.Digest:
				return io.NopCloser(bytes.NewReader(manifestBytes)), nil
			case config.Digest:
				return uploadReader, nil
			default:
				return nil, stderrors.New("unexpected source descriptor")
			}
		},
	}

	accepted := make(chan struct{})
	putStarted := make(chan struct{})
	putDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodHead:
			response.WriteHeader(http.StatusNotFound)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/blobs/uploads/"):
			response.Header().Set("Location", request.URL.Path+"accepted")
			response.WriteHeader(http.StatusAccepted)
			close(accepted)
		case request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/blobs/uploads/accepted"):
			close(putStarted)
			_, _ = io.Copy(io.Discard, request.Body)
			putDone <- request.Context().Err()
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() {
		cancel()
		server.CloseClientConnections()
		server.Close()
	})

	trackedReader := make(chan *contextReadCloser, 1)
	targetReady := make(chan *remotePublicationTarget, 1)
	deps := defaultPushOperationDependencies()
	deps.maxAttempts = 1
	deps.newTarget = func(opts PushOptions) (publicationTarget, error) {
		target, err := newPublicationTarget(opts)
		if err != nil {
			return nil, err
		}
		remoteTarget, ok := target.(*remotePublicationTarget)
		if !ok {
			return nil, stderrors.New("publication target is not remote")
		}
		targetReady <- remoteTarget
		return target, nil
	}
	deps.copyGraph = func(
		copyCtx context.Context,
		copySource content.ReadOnlyStorage,
		copyTarget content.Storage,
		root ociv1.Descriptor,
		options oras.CopyGraphOptions,
	) error {

		observed := &checkpointCObservedStorage{ReadOnlyStorage: copySource}
		observed.fetch = func(fetchCtx context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
			reader, err := copySource.Fetch(fetchCtx, desc)
			if err == nil && desc.Digest == config.Digest {
				tracked, ok := reader.(*contextReadCloser)
				if !ok {
					return nil, stderrors.New("attempt source did not return a tracked reader")
				}
				trackedReader <- tracked
			}
			return reader, err
		}
		return oras.CopyGraph(copyCtx, observed, copyTarget, root, options)
	}
	resultDone := make(chan struct {
		result *PushResult
		err    error
	}, 1)
	go func() {
		result, err := pushFrozenDescriptor(ctx, source, manifest, PushOptions{
			Registry:   strings.TrimPrefix(server.URL, "http://"),
			Repository: "test/repo",
			Tag:        "v1",
			PlainHTTP:  true,
		}, apperrors.ErrCodeInternal, func() error { return nil }, deps)
		resultDone <- struct {
			result *PushResult
			err    error
		}{result: result, err: err}
	}()

	var managed *remotePublicationTarget
	select {
	case managed = <-targetReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for remote publication target")
	}
	if managed.client == nil || managed.client.Timeout != 0 {
		t.Fatalf("registry HTTP client = %+v, want context-governed zero timeout", managed.client)
	}
	var tracked *contextReadCloser
	select {
	case tracked = <-trackedReader:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for attempt-scoped source reader")
	}
	waitForCheckpointCSignal(t, accepted, "accepted registry upload")
	waitForCheckpointCSignal(t, uploadReader.blocked, "blocked upload source read")
	waitForCheckpointCSignal(t, putStarted, "registry PUT handler")
	cancel()
	select {
	case outcome := <-resultDone:
		assertErrorCode(t, outcome.err, apperrors.ErrCodeTimeout)
		if outcome.result != nil {
			t.Fatalf("canceled registry push result = %+v", outcome.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registry push did not return after cancellation")
	}
	waitForCheckpointCSignal(t, uploadReader.closed, "underlying upload source close")
	waitForCheckpointCSignal(t, tracked.done, "attempt source watcher")
	select {
	case requestErr := <-putDone:
		if !stderrors.Is(requestErr, context.Canceled) {
			t.Fatalf("registry PUT context error = %v, want context canceled", requestErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for registry PUT cancellation")
	}
	if got := uploadReader.closeCalls.Load(); got != 1 {
		t.Fatalf("underlying upload source Close calls = %d, want 1", got)
	}
}

type checkpointCObservedStorage struct {
	content.ReadOnlyStorage
	fetch func(context.Context, ociv1.Descriptor) (io.ReadCloser, error)
}

func (s *checkpointCObservedStorage) Fetch(
	ctx context.Context,
	desc ociv1.Descriptor,
) (io.ReadCloser, error) {

	return s.fetch(ctx, desc)
}

type checkpointCPrefixThenBlockReader struct {
	data       []byte
	split      int
	offset     int
	blocked    chan struct{}
	closed     chan struct{}
	blockOnce  sync.Once
	closeOnce  sync.Once
	closeCalls atomic.Int32
}

func newCheckpointCPrefixThenBlockReader(data []byte, split int) *checkpointCPrefixThenBlockReader {
	return &checkpointCPrefixThenBlockReader{
		data:    append([]byte(nil), data...),
		split:   split,
		blocked: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (r *checkpointCPrefixThenBlockReader) Read(p []byte) (int, error) {
	if r.offset < r.split {
		n := copy(p, r.data[r.offset:r.split])
		r.offset += n
		return n, nil
	}
	r.blockOnce.Do(func() { close(r.blocked) })
	<-r.closed
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *checkpointCPrefixThenBlockReader) Close() error {
	r.closeCalls.Add(1)
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func waitForCheckpointCSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

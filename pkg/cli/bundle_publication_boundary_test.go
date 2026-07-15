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

package cli

import (
	"bytes"
	"context"
	stderrors "errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/urfave/cli/v3"
)

type deadlineOperationCase struct {
	name string
	err  error
}

func deadlineOperationCases() []deadlineOperationCase {
	return []deadlineOperationCase{
		{name: "nil operation error"},
		{
			name: "non-nil operation error",
			err:  errors.New(errors.ErrCodeInternal, "injected operation failure"),
		},
	}
}

func newBundlePublicationStage(
	t *testing.T,
	configure func(*bundleOutputDependencies, string, string),
) (*bundleOutputTarget, *bundleGenerationStage, string, string) {

	t.Helper()
	const (
		stageName  = ".aicr-bundle-generation-stage-test"
		backupName = ".aicr-bundle-generation-backup-test"
	)
	deps := defaultBundleOutputDependencies()
	names := []string{stageName, backupName}
	nextName := 0
	deps.randomName = func() (string, error) {
		if nextName >= len(names) {
			return "", stderrors.New("unexpected bundle generation name request")
		}
		name := names[nextName]
		nextName++
		return name, nil
	}
	if configure != nil {
		configure(&deps, stageName, backupName)
	}

	outputPath := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(outputPath, 0o755); err != nil {
		t.Fatalf("Mkdir(output) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputPath, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	target, err := prepareBundleOutputTargetWithDependencies(
		context.Background(), outputPath, deps)
	if err != nil {
		t.Fatalf("prepareBundleOutputTargetWithDependencies() error = %v", err)
	}
	stage, err := target.newGenerationStage(context.Background())
	if err != nil {
		_ = target.close()
		t.Fatalf("newGenerationStage() error = %v", err)
	}
	if err := stage.root.WriteFile("new.txt", []byte("new"), 0o600); err != nil {
		_ = stage.close()
		_ = target.close()
		t.Fatalf("WriteFile(new) error = %v", err)
	}
	t.Cleanup(func() {
		_ = stage.close()
		_ = target.close()
	})
	return target, stage, stageName, backupName
}

func TestBundleGenerationPublishCancellationBeforeCommitRestoresPreviousOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var outputName string
	target, stage, stageName, _ := newBundlePublicationStage(t,
		func(deps *bundleOutputDependencies, configuredStageName, _ string) {
			defaultRename := deps.rootRename
			deps.rootRename = func(root *os.Root, oldName, newName string) error {
				err := defaultRename(root, oldName, newName)
				if err == nil && oldName == configuredStageName && newName == outputName {
					cancel()
				}
				return err
			}
		})
	outputName = target.relativePath

	err := stage.publish(ctx)
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("publish() error = %v, want timeout", err)
	}
	if _, statErr := os.Lstat(filepath.Join(target.path, "old.txt")); statErr != nil {
		t.Fatalf("previous output was not restored: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(target.path, "new.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("canceled output remained published: %v", statErr)
	}
	if stage.name != stageName {
		t.Fatalf("stage name = %q after rollback, want %q", stage.name, stageName)
	}
}

func TestBundleGenerationPublishIgnoresCancellationAfterCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target, stage, _, backupName := newBundlePublicationStage(t,
		func(deps *bundleOutputDependencies, _, configuredBackupName string) {
			defaultRemoveAll := deps.rootRemoveAll
			deps.rootRemoveAll = func(root *os.Root, name string) error {
				err := defaultRemoveAll(root, name)
				if err == nil && name == configuredBackupName {
					cancel()
				}
				return err
			}
		})

	if err := stage.publish(ctx); err != nil {
		t.Fatalf("publish() after committed cleanup cancellation error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(target.path, "new.txt")); statErr != nil {
		t.Fatalf("committed output is unavailable: %v", statErr)
	}
	if _, statErr := target.deps.rootLstat(target.ancestor, backupName); !os.IsNotExist(statErr) {
		t.Fatalf("backup remained after committed cleanup: %v", statErr)
	}
}

func TestBundleGenerationPublishPreservesChangedBackupDuringCleanup(t *testing.T) {
	var (
		backupSuccesses int
		backupMovedPath string
		backupSwapped   bool
	)
	target, stage, _, backupName := newBundlePublicationStage(t,
		func(deps *bundleOutputDependencies, _, configuredBackupName string) {
			defaultLstat := deps.rootLstat
			deps.rootLstat = func(root *os.Root, name string) (fs.FileInfo, error) {
				info, err := defaultLstat(root, name)
				if name != configuredBackupName || err != nil {
					return info, err
				}
				backupSuccesses++
				if backupSuccesses != 3 {
					return info, nil
				}
				backupMovedPath = filepath.Join(root.Name(), configuredBackupName+"-moved")
				if renameErr := os.Rename(filepath.Join(root.Name(), configuredBackupName), backupMovedPath); renameErr != nil {
					return nil, renameErr
				}
				if mkdirErr := os.Mkdir(filepath.Join(root.Name(), configuredBackupName), 0o700); mkdirErr != nil {
					return nil, mkdirErr
				}
				if writeErr := os.WriteFile(
					filepath.Join(root.Name(), configuredBackupName, "replacement.txt"),
					[]byte("replacement"), 0o600,
				); writeErr != nil {
					return nil, writeErr
				}
				backupSwapped = true
				return defaultLstat(root, name)
			}
		})

	err := stage.publish(context.Background())
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("publish() error = %v, want changed-backup internal error", err)
	}
	if !backupSwapped {
		t.Fatal("backup was not revalidated immediately before cleanup")
	}
	if _, statErr := os.Lstat(filepath.Join(
		target.ancestorPath, backupName, "replacement.txt")); statErr != nil {
		t.Fatalf("changed backup entry was removed: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(backupMovedPath, "old.txt")); statErr != nil {
		t.Fatalf("retained previous output was lost: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(target.path, "new.txt")); statErr != nil {
		t.Fatalf("committed output is unavailable: %v", statErr)
	}
}

func TestBundleGenerationStageClosePreservesChangedName(t *testing.T) {
	const stageName = ".aicr-bundle-generation-stage-close-test"
	deps := defaultBundleOutputDependencies()
	deps.randomName = func() (string, error) { return stageName, nil }
	outputPath := filepath.Join(realTempDir(t), "bundle")
	target, err := prepareBundleOutputTargetWithDependencies(
		context.Background(), outputPath, deps)
	if err != nil {
		t.Fatalf("prepareBundleOutputTargetWithDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = target.close() })
	stage, err := target.newGenerationStage(context.Background())
	if err != nil {
		t.Fatalf("newGenerationStage() error = %v", err)
	}
	movedPath := stage.path + "-moved"
	if err := os.Rename(stage.path, movedPath); err != nil {
		t.Fatalf("Rename(stage) error = %v", err)
	}
	if err := os.Mkdir(stage.path, 0o700); err != nil {
		t.Fatalf("Mkdir(replacement) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(stage.path, "replacement.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}

	closeErr := stage.close()
	if !stderrors.Is(closeErr, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("stage.close() error = %v, want internal identity error", closeErr)
	}
	if _, statErr := os.Lstat(filepath.Join(stage.path, "replacement.txt")); statErr != nil {
		t.Fatalf("changed stage entry was removed: %v", statErr)
	}
	if _, statErr := os.Lstat(movedPath); statErr != nil {
		t.Fatalf("retained original stage was lost: %v", statErr)
	}
}

func TestBundleOutputTargetRejectsFilesystemVolumeRoot(t *testing.T) {
	volumeRoot := filepath.VolumeName(filepath.Clean(t.TempDir())) + string(filepath.Separator)
	volumeRoot = filepath.Clean(volumeRoot)
	if filepath.Dir(volumeRoot) != volumeRoot {
		t.Fatalf("test path %q is not a filesystem volume root", volumeRoot)
	}
	target, err := prepareBundleOutputTarget(context.Background(), volumeRoot)
	if target != nil {
		_ = target.close()
		t.Fatalf("prepareBundleOutputTarget(%q) returned a target", volumeRoot)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("prepareBundleOutputTarget(%q) error = %v, want invalid request", volumeRoot, err)
	}
}

func TestImageRefsTargetRejectsDirectoryIdentityAliases(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	nestedDir := filepath.Join(bundleDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(bundle) error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)

	for _, aliasDir := range []string{bundleDir, nestedDir} {
		t.Run(filepath.Base(aliasDir), func(t *testing.T) {
			parent := realTempDir(t)
			parentInfo, err := os.Lstat(aliasDir)
			if err != nil {
				t.Fatalf("Lstat(alias) error = %v", err)
			}
			deps := defaultImageRefsTargetDependencies()
			deps.lstat = func(path string) (fs.FileInfo, error) {
				if path == parent {
					return parentInfo, nil
				}
				return os.Lstat(path)
			}
			deps.openRoot = func(path string) (*os.Root, error) {
				if path == parent {
					return os.OpenRoot(aliasDir)
				}
				return os.OpenRoot(path)
			}
			target, prepErr := prepareImageRefsTargetWithDependencies(
				context.Background(), bundle, filepath.Join(parent, "refs.txt"), deps)
			if target != nil {
				_ = target.close()
			}
			if !stderrors.Is(prepErr, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("error = %v, want invalid request directory alias rejection", prepErr)
			}
		})
	}

	t.Run("same-file seam", func(t *testing.T) {
		parent := realTempDir(t)
		deps := defaultImageRefsTargetDependencies()
		deps.sameFile = func(first, second fs.FileInfo) bool {
			if first.IsDir() && second.IsDir() {
				return true
			}
			return os.SameFile(first, second)
		}
		target, prepErr := prepareImageRefsTargetWithDependencies(
			context.Background(), bundle, filepath.Join(parent, "refs.txt"), deps)
		if target != nil {
			_ = target.close()
		}
		if !stderrors.Is(prepErr, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("error = %v, want deterministic directory identity rejection", prepErr)
		}
	})
}

func TestPrintArgoCDHelmOCIInstructionsExactCommand(t *testing.T) {
	ref := &oci.Reference{
		IsOCI:      true,
		Registry:   "registry.example.com",
		Repository: "team/aicr-bundle",
		Tag:        "1.2.3_build.5",
	}
	var output bytes.Buffer
	printArgoCDHelmOCIInstructions(&output, ref)
	want := "\nargocd-helm bundle pushed: oci://registry.example.com/team/aicr-bundle:1.2.3_build.5\n" +
		"\nTo install:\n" +
		"  helm install <release> oci://registry.example.com/team/aicr-bundle \\\n" +
		"    --namespace argocd \\\n" +
		"    --version 1.2.3+build.5\n" +
		"    # repoURL defaults to oci://registry.example.com/team (override with --set repoURL=oci://mirror if mirroring)\n"
	if output.String() != want {
		t.Fatalf("instructions mismatch\ngot:\n%s\nwant:\n%s", output.String(), want)
	}
}

func TestBundleOutputTargetWrapsPreexistingRootLstatFailure(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	injected := stderrors.New("injected rooted lstat failure")
	deps := defaultBundleOutputDependencies()
	deps.rootLstat = func(root *os.Root, name string) (fs.FileInfo, error) {
		if name == filepath.Base(bundleDir) {
			return nil, injected
		}
		return root.Lstat(name)
	}
	target, err := prepareBundleOutputTargetWithDependencies(context.Background(), bundleDir, deps)
	if target != nil {
		_ = target.close()
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("error = %v, want structured internal", err)
	}
	if !stderrors.Is(err, injected) {
		t.Fatalf("error = %v, want injected cause", err)
	}
}

func TestBundleCommandDeadlinesAreAuthoritative(t *testing.T) {
	t.Run("preflight cancellation stops optional target preparation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var refsCalls atomic.Int32
		deps := defaultBundleCommandDependencies()
		deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
			target, err := prepareBundleOutputTarget(ctx, path)
			cancel()
			return target, err
		}
		deps.prepareImageRefsTarget = func(
			context.Context,
			*bundleOutputTarget,
			string,
		) (*imageRefsTarget, error) {

			refsCalls.Add(1)
			return nil, stderrors.New("optional target preparation should not run")
		}
		err := runDeadlineBundleCommand(t, ctx, deps, true)
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("error = %v, want timeout", err)
		}
		if refsCalls.Load() != 0 {
			t.Fatalf("image-reference preparation calls = %d, want zero", refsCalls.Load())
		}
	})

	t.Run("preflight cancellation outranks operation error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		deps := defaultBundleCommandDependencies()
		deps.prepareBundleOutput = func(context.Context, string) (*bundleOutputTarget, error) {
			cancel()
			return nil, errors.New(errors.ErrCodeInternal, "injected preflight failure")
		}
		err := runDeadlineBundleCommand(t, ctx, deps, false)
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("error = %v, want timeout", err)
		}
	})

	t.Run("capture cancellation stops publication", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var pushCalls atomic.Int32
		deps := defaultBundleCommandDependencies()
		deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
			fsDeps := defaultBundleOutputDependencies()
			fsDeps.beforeGeneratedOpen = func(*bundleOutputTarget) error {
				cancel()
				return nil
			}
			return prepareBundleOutputTargetWithDependencies(ctx, path, fsDeps)
		}
		deps.makeBundle = func(
			_ context.Context,
			_ *aicr.Client,
			_ *aicr.RecipeResult,
			opts aicr.BundleOptions,
		) (aicr.BundleArtifact, error) {

			if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
				return nil, err
			}
			return &result.Output{OutputDir: opts.OutputDir}, nil
		}
		deps.pushOCIBundle = func(
			context.Context,
			*bundleCmdOptions,
			*result.Output,
			*bundleOutputTarget,
			*imageRefsTarget,
		) error {

			pushCalls.Add(1)
			return nil
		}
		err := runDeadlineBundleCommand(t, ctx, deps, false)
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("error = %v, want timeout", err)
		}
		if pushCalls.Load() != 0 {
			t.Fatalf("publication calls = %d, want zero", pushCalls.Load())
		}
	})

	t.Run("capture cancellation outranks operation error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		deps := defaultBundleCommandDependencies()
		deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
			fsDeps := defaultBundleOutputDependencies()
			fsDeps.beforeGeneratedOpen = func(*bundleOutputTarget) error {
				cancel()
				return errors.New(errors.ErrCodeInternal, "injected capture failure")
			}
			return prepareBundleOutputTargetWithDependencies(ctx, path, fsDeps)
		}
		deps.makeBundle = func(
			_ context.Context,
			_ *aicr.Client,
			_ *aicr.RecipeResult,
			opts aicr.BundleOptions,
		) (aicr.BundleArtifact, error) {

			if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
				return nil, err
			}
			return &result.Output{OutputDir: opts.OutputDir}, nil
		}
		err := runDeadlineBundleCommand(t, ctx, deps, false)
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("error = %v, want timeout", err)
		}
	})

	t.Run("image-reference cancellation stops generation", func(t *testing.T) {
		for _, tt := range deadlineOperationCases() {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				var makeCalls atomic.Int32
				deps := defaultBundleCommandDependencies()
				defaultPrepare := deps.prepareImageRefsTarget
				deps.prepareImageRefsTarget = func(
					ctx context.Context,
					bundle *bundleOutputTarget,
					path string,
				) (*imageRefsTarget, error) {

					target, err := defaultPrepare(ctx, bundle, path)
					if err != nil {
						return target, err
					}
					cancel()
					return target, tt.err
				}
				deps.makeBundle = func(
					context.Context,
					*aicr.Client,
					*aicr.RecipeResult,
					aicr.BundleOptions,
				) (aicr.BundleArtifact, error) {

					makeCalls.Add(1)
					return nil, stderrors.New("bundle generation should not run")
				}
				err := runDeadlineBundleCommand(t, ctx, deps, true)
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Fatalf("error = %v, want timeout", err)
				}
				if makeCalls.Load() != 0 {
					t.Fatalf("bundle generation calls = %d, want zero", makeCalls.Load())
				}
			})
		}
	})
}

func TestBundleCommandGenerationDoesNotWriteThroughReplacedOutputPath(t *testing.T) {
	var replacementPath string
	var pushCalls atomic.Int32
	deps := defaultBundleCommandDependencies()
	deps.makeBundle = func(
		_ context.Context,
		_ *aicr.Client,
		_ *aicr.RecipeResult,
		opts aicr.BundleOptions,
	) (aicr.BundleArtifact, error) {

		workDir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		replacementPath = filepath.Join(workDir, "bundle")
		if err := os.Mkdir(replacementPath, 0o755); err != nil {
			return nil, err
		}
		writeVerifiedCLITestBundle(
			t, opts.OutputDir, map[string][]byte{"payload.txt": []byte("generated")})
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}
	deps.pushOCIBundle = func(
		context.Context,
		*bundleCmdOptions,
		*result.Output,
		*bundleOutputTarget,
		*imageRefsTarget,
	) error {

		pushCalls.Add(1)
		return nil
	}

	err := runDeadlineBundleCommand(t, context.Background(), deps, false)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("error = %v, want internal output-identity rejection", err)
	}
	if pushCalls.Load() != 0 {
		t.Fatalf("publication calls = %d, want zero", pushCalls.Load())
	}
	if _, statErr := os.Lstat(filepath.Join(replacementPath, "payload.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("generation wrote through replacement output path: %v", statErr)
	}
}

func TestBundleCommandPublishesPrivateGenerationStage(t *testing.T) {
	var finalPath string
	var stagePath string
	deps := defaultBundleCommandDependencies()
	deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
		finalPath = path
		if err := os.Mkdir(path, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(path, "stale.txt"), []byte("stale"), 0o600); err != nil {
			return nil, err
		}
		return prepareBundleOutputTarget(ctx, path)
	}
	deps.makeBundle = func(
		_ context.Context,
		_ *aicr.Client,
		_ *aicr.RecipeResult,
		opts aicr.BundleOptions,
	) (aicr.BundleArtifact, error) {

		stagePath = opts.OutputDir
		if filepath.Clean(stagePath) == filepath.Clean(finalPath) {
			return nil, stderrors.New("bundle generation received the caller-visible output path")
		}
		writeVerifiedCLITestBundle(
			t, stagePath, map[string][]byte{"payload.txt": []byte("generated")})
		return &result.Output{
			OutputDir: stagePath,
			Results: []*result.Result{{
				Files:   []string{filepath.Join(stagePath, "payload.txt")},
				Success: true,
			}},
		}, nil
	}
	deps.pushOCIBundle = func(
		ctx context.Context,
		_ *bundleCmdOptions,
		out *result.Output,
		bundle *bundleOutputTarget,
		_ *imageRefsTarget,
	) error {

		if out.OutputDir != finalPath {
			return stderrors.New("published output directory was not relocated")
		}
		if got := out.Results[0].Files[0]; got != filepath.Join(finalPath, "payload.txt") {
			return stderrors.New("published result path was not relocated")
		}
		payload, err := os.ReadFile(filepath.Join(finalPath, "payload.txt"))
		if err != nil {
			return err
		}
		if string(payload) != "generated" {
			return stderrors.New("published payload content mismatch")
		}
		if _, err := os.Lstat(filepath.Join(finalPath, "stale.txt")); !os.IsNotExist(err) {
			return stderrors.New("stale output survived private-stage publication")
		}
		return bundle.validate(ctx)
	}

	if err := runDeadlineBundleCommand(t, context.Background(), deps, false); err != nil {
		t.Fatalf("runBundleCmdWithDependencies() error = %v", err)
	}
	if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("private generation stage remains after publication: %v", err)
	}
}

func TestBundleCommandDoesNotFailWhenCanceledAfterOutputCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const (
		stageName  = ".aicr-bundle-generation-stage-command-test"
		backupName = ".aicr-bundle-generation-backup-command-test"
	)
	var (
		finalPath string
		pushCalls atomic.Int32
	)
	deps := defaultBundleCommandDependencies()
	deps.prepareBundleOutput = func(ctx context.Context, path string) (*bundleOutputTarget, error) {
		finalPath = path
		if err := os.Mkdir(path, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(path, "old.txt"), []byte("old"), 0o600); err != nil {
			return nil, err
		}
		fsDeps := defaultBundleOutputDependencies()
		names := []string{stageName, backupName}
		fsDeps.randomName = func() (string, error) {
			if len(names) == 0 {
				return "", stderrors.New("unexpected bundle generation name request")
			}
			name := names[0]
			names = names[1:]
			return name, nil
		}
		defaultRemoveAll := fsDeps.rootRemoveAll
		fsDeps.rootRemoveAll = func(root *os.Root, name string) error {
			err := defaultRemoveAll(root, name)
			if err == nil && name == backupName {
				cancel()
			}
			return err
		}
		return prepareBundleOutputTargetWithDependencies(ctx, path, fsDeps)
	}
	deps.makeBundle = func(
		_ context.Context,
		_ *aicr.Client,
		_ *aicr.RecipeResult,
		opts aicr.BundleOptions,
	) (aicr.BundleArtifact, error) {

		writeVerifiedCLITestBundle(
			t, opts.OutputDir, map[string][]byte{"payload.txt": []byte("generated")})
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}
	deps.pushOCIBundle = func(
		context.Context,
		*bundleCmdOptions,
		*result.Output,
		*bundleOutputTarget,
		*imageRefsTarget,
	) error {

		pushCalls.Add(1)
		return nil
	}

	if err := runDeadlineBundleCommand(t, ctx, deps, false); err != nil {
		t.Fatalf("runBundleCmdWithDependencies() after output commit error = %v", err)
	}
	if pushCalls.Load() != 1 {
		t.Fatalf("publication calls = %d, want one", pushCalls.Load())
	}
	if _, err := os.Lstat(filepath.Join(finalPath, "payload.txt")); err != nil {
		t.Fatalf("committed output is unavailable: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(finalPath, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("previous output remained after commit: %v", err)
	}
}

func TestPushOCIBundleDeadlinesAreAuthoritative(t *testing.T) {
	t.Run("stage", func(t *testing.T) {
		for _, tt := range deadlineOperationCases() {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
				var workspaceCalls atomic.Int32
				var stagedPath string
				deps := defaultBundlePublishDependencies()
				deps.stageVerifiedBundle = func(
					ctx context.Context,
					source string,
					opts checksum.InventoryOptions,
				) (string, *checksum.Inventory, func() error, error) {

					staged, inventory, cleanup, err := checksum.StageVerifiedBundle(ctx, source, opts)
					if err != nil {
						return staged, inventory, cleanup, err
					}
					stagedPath = staged
					cancel()
					return staged, inventory, cleanup, tt.err
				}
				deps.newWorkspace = func(context.Context, string, ...string) (*oci.Workspace, error) {
					workspaceCalls.Add(1)
					return nil, stderrors.New("workspace should not be created")
				}
				err := pushOCIBundleWithDependencies(ctx, opts,
					&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Fatalf("error = %v, want timeout", err)
				}
				if workspaceCalls.Load() != 0 {
					t.Fatalf("workspace calls = %d, want zero", workspaceCalls.Load())
				}
				if _, statErr := os.Lstat(stagedPath); !os.IsNotExist(statErr) {
					t.Fatalf("stage was not cleaned: %v", statErr)
				}
			})
		}
	})

	t.Run("workspace", func(t *testing.T) {
		for _, tt := range deadlineOperationCases() {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
				var packageCalls atomic.Int32
				var workspacePath string
				deps := defaultBundlePublishDependencies()
				deps.newWorkspace = func(
					ctx context.Context,
					prefix string,
					excluded ...string,
				) (*oci.Workspace, error) {

					workspace, err := oci.NewPrivateWorkspace(ctx, prefix, excluded...)
					if err != nil {
						return workspace, err
					}
					workspacePath = workspace.Path()
					cancel()
					return workspace, tt.err
				}
				deps.packageAndPush = func(context.Context, oci.OutputConfig) (*oci.PackageAndPushResult, error) {
					packageCalls.Add(1)
					return &oci.PackageAndPushResult{
						Digest: "sha256:abc", Reference: "registry.example.com/a:dev",
					}, nil
				}
				err := pushOCIBundleWithDependencies(ctx, opts,
					&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Fatalf("error = %v, want timeout", err)
				}
				if packageCalls.Load() != 0 {
					t.Fatalf("package calls = %d, want zero", packageCalls.Load())
				}
				if _, statErr := os.Lstat(workspacePath); !os.IsNotExist(statErr) {
					t.Fatalf("workspace was not cleaned: %v", statErr)
				}
			})
		}
	})

	t.Run("publisher", func(t *testing.T) {
		for _, tt := range deadlineOperationCases() {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
				deps := defaultBundlePublishDependencies()
				deps.packageAndPush = func(
					context.Context,
					oci.OutputConfig,
				) (*oci.PackageAndPushResult, error) {

					cancel()
					return &oci.PackageAndPushResult{
						Digest: "sha256:abc", Reference: "registry.example.com/a:dev",
					}, tt.err
				}
				err := pushOCIBundleWithDependencies(ctx, opts,
					&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Fatalf("error = %v, want timeout", err)
				}
			})
		}
	})

	t.Run("image refs", func(t *testing.T) {
		for _, tt := range deadlineOperationCases() {
			t.Run(tt.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
				refsPath := filepath.Join(realTempDir(t), "refs.txt")
				refs, err := prepareImageRefsTarget(context.Background(), bundle, refsPath)
				if err != nil {
					t.Fatalf("prepareImageRefsTarget() error = %v", err)
				}
				t.Cleanup(func() { _ = refs.close() })
				opts.imageRefsPath = refsPath
				deps := defaultBundlePublishDependencies()
				deps.packageAndPush = func(
					context.Context,
					oci.OutputConfig,
				) (*oci.PackageAndPushResult, error) {

					return &oci.PackageAndPushResult{
						Digest: "sha256:abc", Reference: "registry.example.com/a:dev",
					}, nil
				}
				deps.writeImageRefs = func(context.Context, *imageRefsTarget, []byte) error {
					cancel()
					return tt.err
				}
				err = pushOCIBundleWithDependencies(ctx, opts,
					&result.Output{OutputDir: bundleDir}, bundle, refs, deps)
				if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
					t.Fatalf("error = %v, want timeout", err)
				}
			})
		}
	})
}

func TestImageRefsWriteDeadlineAfterRenameIsAuthoritative(t *testing.T) {
	for _, tt := range deadlineOperationCases() {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			bundleDir := filepath.Join(realTempDir(t), "bundle")
			if err := os.Mkdir(bundleDir, 0o755); err != nil {
				t.Fatalf("Mkdir(bundle) error = %v", err)
			}
			bundle := mustPreparedBundleOutput(t, bundleDir)
			refsPath := filepath.Join(realTempDir(t), "refs.txt")
			deps := defaultImageRefsTargetDependencies()
			defaultRename := deps.rootRename
			deps.rootRename = func(root *os.Root, oldName, newName string) error {
				err := defaultRename(root, oldName, newName)
				if err != nil {
					return err
				}
				cancel()
				return tt.err
			}
			refs, err := prepareImageRefsTargetWithDependencies(
				context.Background(), bundle, refsPath, deps)
			if err != nil {
				t.Fatalf("prepareImageRefsTarget() error = %v", err)
			}
			t.Cleanup(func() { _ = refs.close() })
			err = refs.writeAtomic(ctx, []byte("sha256:abc\n"))
			if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Fatalf("error = %v, want timeout", err)
			}
		})
	}
}

func TestImageRefsWriteDeadlineAfterFinalValidationIsAuthoritative(t *testing.T) {
	for _, tt := range deadlineOperationCases() {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			bundleDir := filepath.Join(realTempDir(t), "bundle")
			if err := os.Mkdir(bundleDir, 0o755); err != nil {
				t.Fatalf("Mkdir(bundle) error = %v", err)
			}
			bundle := mustPreparedBundleOutput(t, bundleDir)
			refsPath := filepath.Join(realTempDir(t), "refs.txt")
			deps := defaultImageRefsTargetDependencies()
			defaultLstat := deps.rootLstat
			defaultRename := deps.rootRename
			var renamed atomic.Bool
			deps.rootRename = func(root *os.Root, oldName, newName string) error {
				err := defaultRename(root, oldName, newName)
				if err == nil {
					renamed.Store(true)
				}
				return err
			}
			deps.rootLstat = func(root *os.Root, name string) (fs.FileInfo, error) {
				info, err := defaultLstat(root, name)
				if err == nil && renamed.Load() && name == "refs.txt" {
					cancel()
					return info, tt.err
				}
				return info, err
			}
			refs, err := prepareImageRefsTargetWithDependencies(
				context.Background(), bundle, refsPath, deps)
			if err != nil {
				t.Fatalf("prepareImageRefsTarget() error = %v", err)
			}
			t.Cleanup(func() { _ = refs.close() })
			err = refs.writeAtomic(ctx, []byte("sha256:abc\n"))
			if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Fatalf("error = %v, want timeout", err)
			}
		})
	}
}

func TestImageRefsAdversarialFailureMatrix(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*imageRefsTargetDependencies, error)
		wantResidue bool
	}{
		{
			name: "chmod",
			configure: func(deps *imageRefsTargetDependencies, injected error) {
				deps.fileChmod = func(*os.File, fs.FileMode) error { return injected }
			},
		},
		{
			name: "mode mismatch",
			configure: func(deps *imageRefsTargetDependencies, _ error) {
				deps.fileChmod = func(file *os.File, _ fs.FileMode) error { return file.Chmod(0o644) }
			},
		},
		{
			name: "rename",
			configure: func(deps *imageRefsTargetDependencies, injected error) {
				deps.rootRename = func(*os.Root, string, string) error { return injected }
			},
		},
		{
			name:        "remove after short write",
			wantResidue: true,
			configure: func(deps *imageRefsTargetDependencies, injected error) {
				deps.fileWrite = func(*os.File, []byte) (int, error) { return 0, nil }
				deps.rootRemove = func(*os.Root, string) error { return injected }
			},
		},
		{
			name: "temporary descriptor close",
			configure: func(deps *imageRefsTargetDependencies, injected error) {
				defaultClose := deps.closeFile
				deps.closeFile = func(file *os.File) error {
					closeErr := defaultClose(file)
					if filepath.Base(file.Name()) != "refs.txt" {
						return stderrors.Join(closeErr, injected)
					}
					return closeErr
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundleDir := filepath.Join(realTempDir(t), "bundle")
			if err := os.Mkdir(bundleDir, 0o755); err != nil {
				t.Fatalf("Mkdir(bundle) error = %v", err)
			}
			bundle := mustPreparedBundleOutput(t, bundleDir)
			parent := realTempDir(t)
			refsPath := filepath.Join(parent, "refs.txt")
			original := []byte("existing-image-reference\n")
			if err := os.WriteFile(refsPath, original, 0o600); err != nil {
				t.Fatalf("WriteFile(existing target) error = %v", err)
			}
			deps := defaultImageRefsTargetDependencies()
			injected := stderrors.New("injected " + tt.name + " failure")
			tt.configure(&deps, injected)
			refs, err := prepareImageRefsTargetWithDependencies(
				context.Background(), bundle, refsPath, deps)
			if err != nil {
				t.Fatalf("prepareImageRefsTarget() error = %v", err)
			}
			t.Cleanup(func() { _ = refs.close() })
			writeErr := refs.writeAtomic(context.Background(), []byte("sha256:abc\n"))
			if !stderrors.Is(writeErr, errors.New(errors.ErrCodeInternal, "")) {
				t.Fatalf("error = %v, want internal", writeErr)
			}
			if tt.name != "mode mismatch" && !stderrors.Is(writeErr, injected) {
				t.Fatalf("error = %v, want injected cause", writeErr)
			}
			got, readErr := os.ReadFile(refsPath)
			if readErr != nil {
				t.Fatalf("ReadFile(existing target) error = %v", readErr)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("existing target changed: got=%q want=%q", got, original)
			}
			entries, readDirErr := os.ReadDir(parent)
			if readDirErr != nil {
				t.Fatalf("ReadDir(parent) error = %v", readDirErr)
			}
			var residue []fs.DirEntry
			for _, entry := range entries {
				if entry.Name() == filepath.Base(refsPath) {
					continue
				}
				if !validImageRefsTempName(entry.Name()) {
					t.Fatalf("unexpected publication residue: %q", entry.Name())
				}
				residue = append(residue, entry)
			}
			if !tt.wantResidue && len(residue) != 0 {
				t.Fatalf("unexpected image-reference temporary residue: %#v", residue)
			}
			if tt.wantResidue {
				if len(residue) != 1 {
					t.Fatalf("safe cleanup residue count = %d, want one", len(residue))
				}
				info, infoErr := residue[0].Info()
				if infoErr != nil {
					t.Fatalf("temporary residue Info() error = %v", infoErr)
				}
				if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
					t.Fatalf("unsafe temporary residue mode = %v", info.Mode())
				}
			}
		})
	}
}

func TestImageRefsTargetDescriptorCloseFailure(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)
	parent := realTempDir(t)
	refsPath := filepath.Join(parent, "refs.txt")
	if err := os.WriteFile(refsPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile(refs) error = %v", err)
	}
	injected := stderrors.New("injected target close failure")
	deps := defaultImageRefsTargetDependencies()
	defaultClose := deps.closeFile
	deps.closeFile = func(file *os.File) error {
		return stderrors.Join(defaultClose(file), injected)
	}
	target, err := prepareImageRefsTargetWithDependencies(context.Background(), bundle, refsPath, deps)
	if target != nil {
		_ = target.close()
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) || !stderrors.Is(err, injected) {
		t.Fatalf("error = %v, want structured target close failure", err)
	}
}

func TestRetainedTargetCloseIsConcurrentIdempotentAndCached(t *testing.T) {
	t.Run("bundle", func(t *testing.T) {
		bundleDir := filepath.Join(realTempDir(t), "bundle")
		if err := os.Mkdir(bundleDir, 0o755); err != nil {
			t.Fatalf("Mkdir(bundle) error = %v", err)
		}
		injected := stderrors.New("injected bundle root close failure")
		deps := defaultBundleOutputDependencies()
		defaultClose := deps.closeRoot
		var closeCalls atomic.Int32
		deps.closeRoot = func(root *os.Root) error {
			closeCalls.Add(1)
			return stderrors.Join(defaultClose(root), injected)
		}
		target, err := prepareBundleOutputTargetWithDependencies(context.Background(), bundleDir, deps)
		if err != nil {
			t.Fatalf("prepareBundleOutputTarget() error = %v", err)
		}
		assertConcurrentCachedClose(t, target.close, injected)
		if closeCalls.Load() != 2 {
			t.Fatalf("close calls = %d, want two retained roots closed once", closeCalls.Load())
		}
	})

	t.Run("image refs", func(t *testing.T) {
		bundleDir := filepath.Join(realTempDir(t), "bundle")
		if err := os.Mkdir(bundleDir, 0o755); err != nil {
			t.Fatalf("Mkdir(bundle) error = %v", err)
		}
		bundle := mustPreparedBundleOutput(t, bundleDir)
		injected := stderrors.New("injected image refs root close failure")
		deps := defaultImageRefsTargetDependencies()
		defaultClose := deps.closeRoot
		var closeCalls atomic.Int32
		deps.closeRoot = func(root *os.Root) error {
			closeCalls.Add(1)
			return stderrors.Join(defaultClose(root), injected)
		}
		target, err := prepareImageRefsTargetWithDependencies(
			context.Background(), bundle, filepath.Join(realTempDir(t), "refs.txt"), deps)
		if err != nil {
			t.Fatalf("prepareImageRefsTarget() error = %v", err)
		}
		assertConcurrentCachedClose(t, target.close, injected)
		if closeCalls.Load() != 1 {
			t.Fatalf("close calls = %d, want one retained root closed once", closeCalls.Load())
		}
	})
}

func TestImageRefsRejectsCaseAndUnsafeFinalTargets(t *testing.T) {
	bundleDir := filepath.Join(realTempDir(t), "Bundle")
	if err := os.Mkdir(bundleDir, 0o755); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	bundle := mustPreparedBundleOutput(t, bundleDir)
	caseAliasPath := filepath.Join(filepath.Dir(bundleDir), "bundle", "refs.txt")
	assertInvalidImageRefsTarget(t, bundle, caseAliasPath)

	parent := realTempDir(t)
	directoryTarget := filepath.Join(parent, "directory")
	if err := os.Mkdir(directoryTarget, 0o755); err != nil {
		t.Fatalf("Mkdir(target) error = %v", err)
	}
	assertInvalidImageRefsTarget(t, bundle, directoryTarget)
	symlinkTarget := filepath.Join(parent, "symlink")
	if err := os.Symlink(filepath.Join(parent, "missing"), symlinkTarget); err != nil {
		t.Fatalf("Symlink(target) error = %v", err)
	}
	assertInvalidImageRefsTarget(t, bundle, symlinkTarget)

	realParent := realTempDir(t)
	parentAliasBase := realTempDir(t)
	parentAlias := filepath.Join(parentAliasBase, "linked-parent")
	if err := os.Symlink(realParent, parentAlias); err != nil {
		t.Fatalf("Symlink(parent) error = %v", err)
	}
	assertInvalidImageRefsTarget(t, bundle, filepath.Join(parentAlias, "refs.txt"))

	t.Run("Unix socket", func(t *testing.T) {
		socketBase, socketBaseErr := filepath.EvalSymlinks(filepath.FromSlash("/tmp"))
		if socketBaseErr != nil {
			t.Skipf("Unix socket base is unavailable: %v", socketBaseErr)
		}
		socketParent, socketParentErr := os.MkdirTemp(socketBase, "aicr-image-refs-")
		if socketParentErr != nil {
			t.Skipf("Unix socket parent is unavailable: %v", socketParentErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(socketParent) })
		socketPath := filepath.Join(socketParent, "refs.sock")
		listener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if listenErr != nil {
			t.Skipf("Unix sockets are unsupported: %v", listenErr)
		}
		t.Cleanup(func() { _ = listener.Close() })
		assertInvalidImageRefsTarget(t, bundle, socketPath)
	})
}

func assertInvalidImageRefsTarget(t *testing.T, bundle *bundleOutputTarget, path string) {
	t.Helper()
	target, err := prepareImageRefsTarget(context.Background(), bundle, path)
	if target != nil {
		_ = target.close()
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("prepareImageRefsTarget(%q) error = %v, want invalid request", path, err)
	}
}

func TestPushOCIBundleRejectsInSourceTempAndPreservesSource(t *testing.T) {
	bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
	payloadPath := filepath.Join(bundleDir, "payload.txt")
	before, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	t.Setenv("TMPDIR", bundleDir)
	var packageCalls atomic.Int32
	deps := defaultBundlePublishDependencies()
	deps.packageAndPush = func(context.Context, oci.OutputConfig) (*oci.PackageAndPushResult, error) {
		packageCalls.Add(1)
		return nil, stderrors.New("publisher should not run")
	}
	err = pushOCIBundleWithDependencies(context.Background(), opts,
		&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("error = %v, want internal", err)
	}
	if packageCalls.Load() != 0 {
		t.Fatalf("package calls = %d, want zero", packageCalls.Load())
	}
	after, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("source changed: before=%q after=%q", before, after)
	}
}

func TestPushOCIBundleWorkspaceCleanupOnPublisherFailure(t *testing.T) {
	bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
	before, err := os.ReadFile(filepath.Join(bundleDir, "payload.txt"))
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	var workspacePath string
	deps := defaultBundlePublishDependencies()
	deps.newWorkspace = func(ctx context.Context, prefix string, excluded ...string) (*oci.Workspace, error) {
		workspace, workspaceErr := oci.NewPrivateWorkspace(ctx, prefix, excluded...)
		if workspace != nil {
			workspacePath = workspace.Path()
		}
		return workspace, workspaceErr
	}
	deps.packageAndPush = func(context.Context, oci.OutputConfig) (*oci.PackageAndPushResult, error) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "injected publisher failure")
	}
	err = pushOCIBundleWithDependencies(context.Background(), opts,
		&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("error = %v, want publisher error", err)
	}
	if _, statErr := os.Lstat(workspacePath); !os.IsNotExist(statErr) {
		t.Fatalf("workspace was not cleaned: %v", statErr)
	}
	after, err := os.ReadFile(filepath.Join(bundleDir, "payload.txt"))
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("source bytes changed on publisher failure")
	}
}

func TestPushOCIBundleCleansExposedOwnershipOnDependencyErrors(t *testing.T) {
	t.Run("stage", func(t *testing.T) {
		bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
		injected := errors.New(errors.ErrCodeInvalidRequest, "injected stage failure")
		var stagedPath string
		deps := defaultBundlePublishDependencies()
		deps.stageVerifiedBundle = func(
			ctx context.Context,
			source string,
			options checksum.InventoryOptions,
		) (string, *checksum.Inventory, func() error, error) {

			staged, inventory, cleanup, err := checksum.StageVerifiedBundle(ctx, source, options)
			if err != nil {
				return staged, inventory, cleanup, err
			}
			stagedPath = staged
			return staged, inventory, cleanup, injected
		}
		err := pushOCIBundleWithDependencies(context.Background(), opts,
			&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
		if !stderrors.Is(err, injected) {
			t.Fatalf("error = %v, want injected stage error", err)
		}
		if _, statErr := os.Lstat(stagedPath); !os.IsNotExist(statErr) {
			t.Fatalf("exposed stage was not cleaned: %v", statErr)
		}
	})

	t.Run("workspace", func(t *testing.T) {
		bundleDir, bundle, opts := newPublicationBoundaryFixture(t)
		injected := errors.New(errors.ErrCodeInvalidRequest, "injected workspace failure")
		var workspacePath string
		deps := defaultBundlePublishDependencies()
		deps.newWorkspace = func(
			ctx context.Context,
			prefix string,
			excluded ...string,
		) (*oci.Workspace, error) {

			workspace, err := oci.NewPrivateWorkspace(ctx, prefix, excluded...)
			if err != nil {
				return workspace, err
			}
			workspacePath = workspace.Path()
			return workspace, injected
		}
		err := pushOCIBundleWithDependencies(context.Background(), opts,
			&result.Output{OutputDir: bundleDir}, bundle, nil, deps)
		if !stderrors.Is(err, injected) {
			t.Fatalf("error = %v, want injected workspace error", err)
		}
		if _, statErr := os.Lstat(workspacePath); !os.IsNotExist(statErr) {
			t.Fatalf("exposed workspace was not cleaned: %v", statErr)
		}
	})
}

func runDeadlineBundleCommand(
	t *testing.T,
	ctx context.Context,
	deps bundleCommandDependencies,
	withImageRefs bool,
) error {

	t.Helper()
	workDir := realTempDir(t)
	t.Chdir(workDir)
	recipePath := filepath.Join(workDir, "recipe.yaml")
	const recipe = `kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  version: test
componentRefs: []
`
	if err := os.WriteFile(recipePath, []byte(recipe), 0o600); err != nil {
		t.Fatalf("WriteFile(recipe) error = %v", err)
	}
	args := []string{
		"bundle", "--recipe", recipePath,
		"--output", "oci://registry.example.com/team/bundle:dev",
		"--deployer", "helm",
	}
	if withImageRefs {
		args = append(args, "--image-refs", filepath.Join(workDir, "refs.txt"))
	}
	command := bundleCmd()
	command.Action = func(ctx context.Context, cmd *cli.Command) error {
		return runBundleCmdWithDependencies(ctx, cmd, deps)
	}
	return command.Run(ctx, args)
}

func newPublicationBoundaryFixture(
	t *testing.T,
) (string, *bundleOutputTarget, *bundleCmdOptions) {

	t.Helper()
	bundleDir := filepath.Join(realTempDir(t), "bundle")
	writeVerifiedCLITestBundle(t, bundleDir, map[string][]byte{"payload.txt": []byte("immutable")})
	bundle := mustPreparedBundleOutput(t, bundleDir)
	opts := &bundleCmdOptions{
		deployer: config.DeployerHelm,
		ociRef: &oci.Reference{
			IsOCI: true, Registry: "registry.example.com", Repository: "team/bundle", Tag: "dev",
		},
	}
	return bundleDir, bundle, opts
}

func assertConcurrentCachedClose(t *testing.T, closeFn func() error, injected error) {
	t.Helper()
	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for index := range callers {
		go func() {
			defer wg.Done()
			errs[index] = closeFn()
		}()
	}
	wg.Wait()
	for index, err := range errs {
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) ||
			!stderrors.Is(err, injected) {

			t.Fatalf("close[%d] error = %v", index, err)
		}
		if reflect.ValueOf(err).Pointer() != reflect.ValueOf(errs[0]).Pointer() {
			t.Fatalf("close[%d] did not return the cached error", index)
		}
	}
}

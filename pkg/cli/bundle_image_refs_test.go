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
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/urfave/cli/v3"
)

const bundleImageRefsDigest = "sha256:74c42dc3d793da22081a2e9507ca78842fe3795059f245d9b03c2edf4eccd8ab"

// runBundleImageRefsCommand runs `aicr bundle --output oci://... --image-refs
// refsPath` end to end with the registry push faked. mountPoint stands in for
// the device check, which a test cannot satisfy without a bind mount. It
// reports whether the bundle was generated.
func runBundleImageRefsCommand(t *testing.T, refsPath string, mountPoint bool) (bool, error) {
	t.Helper()
	workDir := realTempDir(t)
	t.Chdir(workDir)
	recipePath := filepath.Join(workDir, "recipe.yaml")
	writeTestFile(t, recipePath, "kind: RecipeResult\napiVersion: aicr.run/v1\nmetadata:\n  version: test\ncomponentRefs: []\n")

	var generated bool
	deps := defaultBundleCommandDependencies()
	deps.prepareImageRefsTarget = func(ctx context.Context, bundle *bundleOutputTarget, path string) (*imageRefsTarget, error) {
		refsDeps := defaultImageRefsTargetDependencies()
		refsDeps.onOtherDevice = func(fs.FileInfo, fs.FileInfo) bool { return mountPoint }
		return prepareImageRefsTargetWithDependencies(ctx, bundle, path, refsDeps)
	}
	deps.makeBundle = func(_ context.Context, _ *aicr.Client, _ *aicr.RecipeResult, opts aicr.BundleOptions) (aicr.BundleArtifact, error) {
		generated = true
		writeVerifiedCLITestBundle(t, opts.OutputDir, map[string][]byte{"payload.txt": []byte("generated")})
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}
	deps.pushOCIBundle = func(
		ctx context.Context,
		opts *bundleCmdOptions,
		out *result.Output,
		bundle *bundleOutputTarget,
		refs *imageRefsTarget,
	) error {

		publishDeps := defaultBundlePublishDependencies()
		publishDeps.packageAndPush = func(context.Context, oci.OutputConfig) (*oci.PackageAndPushResult, error) {
			return &oci.PackageAndPushResult{
				Digest:    bundleImageRefsDigest,
				Reference: "registry.example.com/team/aicr-bundle:v1.0.0",
			}, nil
		}
		return pushOCIBundleWithDependencies(ctx, opts, out, bundle, refs, publishDeps)
	}

	cmd := bundleCmd()
	cmd.Action = func(ctx context.Context, command *cli.Command) error {
		return runBundleCmdWithDependencies(ctx, command, deps)
	}
	err := cmd.Run(context.Background(), []string{
		"bundle",
		"--recipe", recipePath,
		"--output", "oci://registry.example.com/team/aicr-bundle:v1.0.0",
		"--image-refs", refsPath,
	})
	return generated, err
}

// TestBundleImageRefs_MountPointWrittenInPlace is the path a kubelet
// terminationMessagePath needs (#3239): a target on another device from its
// parent is written through the same file, where a rename would fail with
// EBUSY. What is observable without a bind mount is that the file keeps its
// identity and loses its old content.
func TestBundleImageRefs_MountPointWrittenInPlace(t *testing.T) {
	target := filepath.Join(realTempDir(t), "termination-log")
	// Longer than the digest line, so a write that skips the truncate leaves a
	// stale tail behind.
	writeTestFile(t, target, strings.Repeat("previous termination message ", 8)+"\n")
	// Not the 0600 a rename publishes, so the mode shows which path ran.
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if _, err = runBundleImageRefsCommand(t, target, true); err != nil {
		t.Fatalf("bundle --image-refs error = %v", err)
	}

	if got := readTestFile(t, target); got != bundleImageRefsDigest+"\n" {
		t.Errorf("target = %q, want %q (old content must be truncated)", got, bundleImageRefsDigest+"\n")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the target was replaced; a mount point must be written through the same file")
	}
	if after.Mode().Perm() != 0o644 {
		t.Errorf("target mode = %v, want the mount point's own 0644 kept", after.Mode().Perm())
	}
}

// TestBundleImageRefs_OrdinaryTargetRenamedIntoPlace pins that a target on its
// parent's device is still published by rename, so a reader never sees a
// partial file.
func TestBundleImageRefs_OrdinaryTargetRenamedIntoPlace(t *testing.T) {
	target := filepath.Join(realTempDir(t), "refs.txt")
	writeTestFile(t, target, "previous\n")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if _, err = runBundleImageRefsCommand(t, target, false); err != nil {
		t.Fatalf("bundle --image-refs error = %v", err)
	}

	if got := readTestFile(t, target); got != bundleImageRefsDigest+"\n" {
		t.Errorf("target = %q, want %q", got, bundleImageRefsDigest+"\n")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if os.SameFile(before, after) {
		t.Error("the target kept its identity; an ordinary target must be replaced by rename")
	}
}

// TestBundleImageRefs_UnwritableTargetRefusedBeforeGeneration pins that a
// target which cannot be written fails at preflight, before the bundle is
// generated and pushed, in both write modes. Root bypasses the permission
// bits, so it cannot run these.
func TestBundleImageRefs_UnwritableTargetRefusedBeforeGeneration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}

	t.Run("read-only parent", func(t *testing.T) {
		parent := mkdirTest(t, filepath.Join(realTempDir(t), "ro"))
		if err := os.Chmod(parent, 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

		generated, err := runBundleImageRefsCommand(t, filepath.Join(parent, "refs.txt"), false)
		if err == nil {
			t.Fatal("bundle --image-refs error = nil, want the unwritable parent refused")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), "parent is not writable") {
			t.Errorf("error = %v, want the writability probe", err)
		}
		if generated {
			t.Error("the bundle was generated before the unwritable target was refused")
		}
		if entries, _ := os.ReadDir(parent); len(entries) != 0 {
			t.Errorf("preflight left %d entries in the parent", len(entries))
		}
	})

	t.Run("read-only mount point", func(t *testing.T) {
		target := filepath.Join(realTempDir(t), "termination-log")
		writeTestFile(t, target, "")
		if err := os.Chmod(target, 0o444); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		generated, err := runBundleImageRefsCommand(t, target, true)
		if err == nil {
			t.Fatal("bundle --image-refs error = nil, want the unwritable mount point refused")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), "mount point that cannot be opened for writing") {
			t.Errorf("error = %v, want the in-place writability check", err)
		}
		if generated {
			t.Error("the bundle was generated before the unwritable mount point was refused")
		}
	})
}

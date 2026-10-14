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
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"gopkg.in/yaml.v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestValidateImageRefsUse(t *testing.T) {
	pushing := &recipeEvidenceConfig{OutDir: "out", Push: "ghcr.io/org/aicr-evidence"}
	tests := []struct {
		name        string
		path        string
		cncf        bool
		noCluster   bool
		evidence    *recipeEvidenceConfig
		wantMessage string
	}{
		{name: "unset", evidence: nil},
		{name: "with push", path: "refs.txt", evidence: pushing},
		{name: "no evidence", path: "refs.txt", wantMessage: "requires --push"},
		{name: "evidence without push", path: "refs.txt", evidence: &recipeEvidenceConfig{OutDir: "out"},
			wantMessage: "requires --push"},
		{name: "cncf submission", path: "refs.txt", cncf: true, evidence: pushing, wantMessage: "--cncf-submission"},
		{name: "no-cluster", path: "refs.txt", noCluster: true, wantMessage: "--no-cluster"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImageRefsUse(tt.path, tt.cncf, tt.noCluster, tt.evidence)
			if tt.wantMessage == "" {
				if err != nil {
					t.Fatalf("validateImageRefsUse() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateImageRefsUse() error = nil, want one mentioning %q", tt.wantMessage)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMessage)
			}
		})
	}
}

// TestPrepareEvidenceImageRefsTarget_PathRules pins the rules `aicr bundle
// --image-refs` applies to its bundle tree, applied here to the
// --emit-attestation directory. Each rejected case would let the refs file
// land in, or alias, the directory pointer.yaml and the pushed bundle come
// from.
func TestPrepareEvidenceImageRefsTarget_PathRules(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the --emit-attestation directory and the target.
		setup func(t *testing.T, dir string) (attestationDir, target string)
		// output is the --output value, relative to the test directory.
		output      string
		wantMessage string
	}{
		{
			name: "absent target outside",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), filepath.Join(dir, "refs.txt")
			},
		},
		{
			name: "existing regular file outside",
			setup: func(t *testing.T, dir string) (string, string) {
				writeTestFile(t, filepath.Join(dir, "refs.txt"), "old\n")
				return filepath.Join(dir, "out"), filepath.Join(dir, "refs.txt")
			},
		},
		{
			name: "inside a planned attestation directory",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), filepath.Join(dir, "out", "refs.txt")
			},
			wantMessage: "must be outside, and must not contain, the --emit-attestation directory",
		},
		{
			name: "the attestation directory itself",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), filepath.Join(dir, "out")
			},
			wantMessage: "must be outside, and must not contain, the --emit-attestation directory",
		},
		{
			name: "through a symlinked parent into the attestation directory",
			setup: func(t *testing.T, dir string) (string, string) {
				out := mkdirTest(t, filepath.Join(dir, "out"))
				if err := os.Symlink(out, filepath.Join(dir, "link")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return out, filepath.Join(dir, "link", "refs.txt")
			},
			wantMessage: "image-reference target aliases the --emit-attestation directory",
		},
		{
			name: "attestation directory named through a symlink",
			setup: func(t *testing.T, dir string) (string, string) {
				real := mkdirTest(t, filepath.Join(dir, "real"))
				mkdirTest(t, filepath.Join(real, "out"))
				if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return filepath.Join(dir, "link", "out"), filepath.Join(real, "out", "refs.txt")
			},
			wantMessage: "image-reference target aliases the --emit-attestation directory",
		},
		{
			// Only the resolved-path check can see this one: the directory does
			// not exist yet, so there is nothing to walk, and a file written
			// here would sit where Emit later needs to create the directory.
			name: "future attestation directory, named through a symlink",
			setup: func(t *testing.T, dir string) (string, string) {
				real := mkdirTest(t, filepath.Join(dir, "real"))
				if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return filepath.Join(dir, "link", "out"), filepath.Join(real, "out")
			},
			wantMessage: "image-reference target aliases the --emit-attestation directory",
		},
		{
			name: "hardlinked to a file in the attestation directory",
			setup: func(t *testing.T, dir string) (string, string) {
				out := mkdirTest(t, filepath.Join(dir, "out"))
				pointer := filepath.Join(out, "pointer.yaml")
				writeTestFile(t, pointer, "pointer\n")
				target := filepath.Join(dir, "refs.txt")
				if err := os.Link(pointer, target); err != nil {
					t.Fatalf("link: %v", err)
				}
				return out, target
			},
			wantMessage: "hardlinked to a file in the --emit-attestation directory",
		},
		{
			name: "ancestor of a future attestation directory",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "evidence", "out"), filepath.Join(dir, "evidence")
			},
			wantMessage: "must be outside, and must not contain, the --emit-attestation directory",
		},
		{
			name: "attestation directory named through a symlink, target hardlinked into it",
			setup: func(t *testing.T, dir string) (string, string) {
				out := mkdirTest(t, filepath.Join(dir, "out"))
				writeTestFile(t, filepath.Join(out, "pointer.yaml"), "pointer\n")
				if err := os.Symlink(out, filepath.Join(dir, "outlink")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				target := filepath.Join(dir, "refs.txt")
				if err := os.Link(filepath.Join(out, "pointer.yaml"), target); err != nil {
					t.Fatalf("link: %v", err)
				}
				return filepath.Join(dir, "outlink"), target
			},
			wantMessage: "hardlinked to a file in the --emit-attestation directory",
		},
		{
			name: "the --output report file",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), filepath.Join(dir, "ctrf.json")
			},
			output:      "ctrf.json",
			wantMessage: "must not be the --output report file",
		},
		{
			name: "the --output report file through a symlink",
			setup: func(t *testing.T, dir string) (string, string) {
				writeTestFile(t, filepath.Join(dir, "refs.txt"), "old\n")
				if err := os.Symlink(filepath.Join(dir, "refs.txt"), filepath.Join(dir, "ctrf.json")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return filepath.Join(dir, "out"), filepath.Join(dir, "refs.txt")
			},
			output:      "ctrf.json",
			wantMessage: "aliases the --output report file",
		},
		{
			name: "hardlinked to the --output report file",
			setup: func(t *testing.T, dir string) (string, string) {
				writeTestFile(t, filepath.Join(dir, "ctrf.json"), "{}\n")
				if err := os.Link(filepath.Join(dir, "ctrf.json"), filepath.Join(dir, "refs.txt")); err != nil {
					t.Fatalf("link: %v", err)
				}
				return filepath.Join(dir, "out"), filepath.Join(dir, "refs.txt")
			},
			output:      "ctrf.json",
			wantMessage: "hardlinked to the --output report file",
		},
		{
			name: "--output to stdout guards nothing",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), filepath.Join(dir, "refs.txt")
			},
			output: "-",
		},
		{
			name: "missing parent",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), filepath.Join(dir, "missing", "refs.txt")
			},
			wantMessage: "parent is unavailable",
		},
		{
			name: "target is a directory",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "out"), mkdirTest(t, filepath.Join(dir, "refs"))
			},
			wantMessage: "must be absent or a regular file",
		},
		{
			name: "target is a symlink",
			setup: func(t *testing.T, dir string) (string, string) {
				writeTestFile(t, filepath.Join(dir, "elsewhere.txt"), "x\n")
				if err := os.Symlink(filepath.Join(dir, "elsewhere.txt"), filepath.Join(dir, "refs.txt")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return filepath.Join(dir, "out"), filepath.Join(dir, "refs.txt")
			},
			wantMessage: "must be absent or a regular file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := evalTempDir(t)
			attestationDir, target := tt.setup(t, dir)
			output := tt.output
			if output != "" && output != "-" {
				output = filepath.Join(dir, output)
			}

			refs, err := prepareEvidenceImageRefsTarget(context.Background(), target, output,
				&recipeEvidenceConfig{OutDir: attestationDir, Push: "ghcr.io/org/aicr-evidence"})
			t.Cleanup(func() { _ = refs.close() })
			if tt.wantMessage == "" {
				if err != nil {
					t.Fatalf("prepareEvidenceImageRefsTarget() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("prepareEvidenceImageRefsTarget() error = nil, want one mentioning %q", tt.wantMessage)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMessage)
			}
		})
	}
}

// TestPrepareEvidenceImageRefsTarget_ProvesWritable pins that a target which
// cannot be written fails at preflight, before the run and the push, in both
// write modes. Root bypasses the permission bits, so it cannot run these.
func TestPrepareEvidenceImageRefsTarget_ProvesWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	evidence := func(dir string) *recipeEvidenceConfig {
		return &recipeEvidenceConfig{OutDir: filepath.Join(dir, "out"), Push: "ghcr.io/org/aicr-evidence"}
	}

	t.Run("read-only parent", func(t *testing.T) {
		dir := evalTempDir(t)
		parent := mkdirTest(t, filepath.Join(dir, "ro"))
		if err := os.Chmod(parent, 0o555); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

		refs, err := prepareEvidenceImageRefsTarget(context.Background(), filepath.Join(parent, "refs.txt"), "", evidence(dir))
		if err == nil {
			_ = refs.close()
			t.Fatal("prepareEvidenceImageRefsTarget() error = nil, want the unwritable parent refused")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), "parent is not writable") {
			t.Errorf("error = %v, want the writability probe", err)
		}
		if entries, _ := os.ReadDir(parent); len(entries) != 0 {
			t.Errorf("preflight left %d entries in the parent", len(entries))
		}
	})

	t.Run("read-only mount point", func(t *testing.T) {
		dir := evalTempDir(t)
		target := filepath.Join(dir, "termination-log")
		writeTestFile(t, target, "")
		if err := os.Chmod(target, 0o444); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		deps := defaultImageRefsTargetDependencies()
		deps.onOtherDevice = func(fs.FileInfo, fs.FileInfo) bool { return true }

		refs, err := prepareEvidenceImageRefsTargetWithDependencies(context.Background(), target, "", evidence(dir), deps)
		if err == nil {
			_ = refs.close()
			t.Fatal("prepareEvidenceImageRefsTarget() error = nil, want the unwritable mount point refused")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), "mount point that cannot be opened for writing") {
			t.Errorf("error = %v, want the in-place writability check", err)
		}
	})
}

// TestWriteEvidenceImageRefs_MountPointWrittenInPlace pins the in-place path
// a kubelet terminationMessagePath needs. The seam stands in for the device
// check, which a test cannot satisfy without a bind mount: what is observable
// is that the file keeps its identity, where the rename path would replace it.
func TestWriteEvidenceImageRefs_MountPointWrittenInPlace(t *testing.T) {
	const digest = "sha256:74c42dc3d793da22081a2e9507ca78842fe3795059f245d9b03c2edf4eccd8ab"
	dir := evalTempDir(t)
	target := filepath.Join(dir, "termination-log")
	// Longer than the digest line, so a write that skips the truncate leaves a
	// stale tail behind.
	writeTestFile(t, target, strings.Repeat("previous termination message ", 8)+"\n")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	deps := defaultImageRefsTargetDependencies()
	deps.onOtherDevice = func(fs.FileInfo, fs.FileInfo) bool { return true }
	refs, err := prepareEvidenceImageRefsTargetWithDependencies(context.Background(), target, "",
		&recipeEvidenceConfig{OutDir: filepath.Join(dir, "out"), Push: "ghcr.io/org/aicr-evidence"}, deps)
	if err != nil {
		t.Fatalf("prepareEvidenceImageRefsTarget: %v", err)
	}
	t.Cleanup(func() { _ = refs.close() })
	if !refs.inPlace {
		t.Fatal("a target on another device was not chosen for an in-place write")
	}

	if err = writeEvidenceImageRefs(context.Background(), refs, &aicr.EvidenceEmission{PushedDigest: digest}); err != nil {
		t.Fatalf("writeEvidenceImageRefs() error = %v", err)
	}
	if got := readTestFile(t, target); got != digest+"\n" {
		t.Errorf("target = %q, want %q (old content must be truncated)", got, digest+"\n")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the target was replaced; a mount point must be written through the same file")
	}
}

func TestNewValidateOutputsGuard_ReportFile(t *testing.T) {
	for _, tt := range []struct {
		output   string
		wantFile bool
	}{
		{output: "", wantFile: false},
		{output: "-", wantFile: false},
		{output: "cm://aicr-validation/report", wantFile: false},
		{output: "report.json", wantFile: true},
	} {
		g, err := newValidateOutputsGuard("out", tt.output)
		if err != nil {
			t.Fatalf("newValidateOutputsGuard(%q) error = %v", tt.output, err)
		}
		if (g.reportFile != "") != tt.wantFile {
			t.Errorf("newValidateOutputsGuard(%q).reportFile = %q, want a file: %v", tt.output, g.reportFile, tt.wantFile)
		}
	}
}

func TestWriteEvidenceImageRefs(t *testing.T) {
	const digest = "sha256:74c42dc3d793da22081a2e9507ca78842fe3795059f245d9b03c2edf4eccd8ab"

	t.Run("writes the digest in the bundle format", func(t *testing.T) {
		dir := evalTempDir(t)
		target := filepath.Join(dir, "refs.txt")
		writeTestFile(t, target, "stale\n")
		refs := prepareTestEvidenceRefs(t, dir, target)

		if err := writeEvidenceImageRefs(context.Background(), refs, &aicr.EvidenceEmission{PushedDigest: digest}); err != nil {
			t.Fatalf("writeEvidenceImageRefs() error = %v", err)
		}
		if got := readTestFile(t, target); got != digest+"\n" {
			t.Errorf("refs file = %q, want %q", got, digest+"\n")
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("refs file mode = %v, want 0600", info.Mode().Perm())
		}
	})

	t.Run("a target hardlinked into the attestation directory after preflight is refused", func(t *testing.T) {
		dir := evalTempDir(t)
		target := filepath.Join(dir, "refs.txt")
		writeTestFile(t, target, "stale\n")
		refs := prepareTestEvidenceRefs(t, dir, target)
		out := mkdirTest(t, filepath.Join(dir, "out"))
		if err := os.Link(target, filepath.Join(out, "pointer.yaml")); err != nil {
			t.Fatalf("link: %v", err)
		}

		err := writeEvidenceImageRefs(context.Background(), refs, &aicr.EvidenceEmission{PushedDigest: digest})
		if err == nil {
			t.Fatal("writeEvidenceImageRefs() error = nil, want the hardlink refused at write time")
		}
		if !strings.Contains(err.Error(), "hardlinked to a file in the --emit-attestation directory") {
			t.Errorf("error = %v, want the write-time alias check", err)
		}
		if got := readTestFile(t, target); got != "stale\n" {
			t.Errorf("refs file = %q, want it unchanged", got)
		}
	})

	t.Run("no target is a no-op", func(t *testing.T) {
		if err := writeEvidenceImageRefs(context.Background(), nil, nil); err != nil {
			t.Fatalf("writeEvidenceImageRefs(nil) error = %v", err)
		}
	})

	t.Run("a push with no digest fails and leaves the file alone", func(t *testing.T) {
		dir := evalTempDir(t)
		target := filepath.Join(dir, "refs.txt")
		writeTestFile(t, target, "stale\n")
		refs := prepareTestEvidenceRefs(t, dir, target)

		err := writeEvidenceImageRefs(context.Background(), refs, &aicr.EvidenceEmission{})
		if err == nil {
			t.Fatal("writeEvidenceImageRefs() error = nil, want an error for a missing digest")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		if got := readTestFile(t, target); got != "stale\n" {
			t.Errorf("refs file = %q, want it unchanged", got)
		}
	})
}

func TestResolveExistingPrefix(t *testing.T) {
	dir := evalTempDir(t)
	real := mkdirTest(t, filepath.Join(dir, "real"))
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := resolveExistingPrefix(filepath.Join(dir, "link", "not", "yet"))
	if err != nil {
		t.Fatalf("resolveExistingPrefix() error = %v", err)
	}
	if want := filepath.Join(real, "not", "yet"); got != want {
		t.Errorf("resolveExistingPrefix() = %q, want %q", got, want)
	}
}

// TestValidateCmd_ImageRefsRejectedBeforeClusterIsTouched pins that an
// unusable --image-refs fails before validation starts, when it costs nothing,
// rather than after the checks and the push. The valid control must reach the
// cluster, or "no requests" would hold for a harness that never connects.
func TestValidateCmd_ImageRefsRejectedBeforeClusterIsTouched(t *testing.T) {
	tests := []struct {
		name         string
		args         func(dir string) []string
		wantMessage  string
		wantRequests bool
	}{
		{
			name: "without --push",
			args: func(dir string) []string {
				return []string{"--image-refs", filepath.Join(dir, "refs.txt")}
			},
			wantMessage: "--image-refs requires --push",
		},
		{
			name: "inside the attestation directory",
			args: func(dir string) []string {
				return []string{"--emit-attestation", filepath.Join(dir, "out"), "--push", "registry.invalid/evidence",
					"--no-sign", "--image-refs", filepath.Join(dir, "out", "refs.txt")}
			},
			wantMessage: "must be outside, and must not contain, the --emit-attestation directory",
		},
		{
			name: "control: a usable target proceeds to the cluster",
			args: func(dir string) []string {
				return []string{"--emit-attestation", filepath.Join(dir, "out"), "--push", "registry.invalid/evidence",
					"--no-sign", "--image-refs", filepath.Join(dir, "refs.txt")}
			},
			wantMessage:  "recording apiserver",
			wantRequests: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := evalTempDir(t)
			rec := newRecordingAPIServer(t)
			kubeconfig := rec.writeKubeconfig(t, dir)
			t.Setenv("KUBECONFIG", kubeconfig)
			recipePath := filepath.Join(dir, "recipe.yaml")
			writeTestFile(t, recipePath, skipPreflightRecipeYAML)

			args := append([]string{"aicr", "validate", "--recipe", recipePath, "--kubeconfig", kubeconfig},
				tt.args(dir)...)
			err := RootCommand().Run(t.Context(), args)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMessage)
			}
			if seen := rec.seen(); tt.wantRequests == (len(seen) == 0) {
				t.Errorf("apiserver requests = %v, want requests: %v", seen, tt.wantRequests)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "refs.txt")); !os.IsNotExist(statErr) {
				t.Errorf("refs file exists after a run that pushed nothing (stat error %v)", statErr)
			}
		})
	}
}

// TestRunValidation_WritesPushedDigestToImageRefs drives runValidation through
// a real push to an in-memory registry, and pins the refs file to the digest
// the same run recorded in pointer.yaml.
func TestRunValidation_WritesPushedDigestToImageRefs(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	dir := evalTempDir(t)

	recipePath := filepath.Join(dir, "recipe.yaml")
	writeTestFile(t, recipePath, "kind: RecipeResult\napiVersion: "+header.StableGroupVersion+"\n"+
		"metadata:\n  version: test\ncriteria:\n  service: eks\n  accelerator: h100\n  intent: training\n"+
		"componentRefs:\n  - name: gpu-operator\n    type: Helm\n    source: https://helm.ngc.nvidia.com/nvidia\n"+
		"    version: v25.10.0\n    overrides:\n      devicePlugin:\n        enabled: true\n")
	snapshotPath := filepath.Join(dir, "snapshot.yaml")
	writeTestFile(t, snapshotPath, "kind: Snapshot\napiVersion: aicr.run/v1\nmetadata:\n  version: test\n"+
		"measurements:\n  - type: K8s\n")

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	rec, err := client.LoadRecipe(t.Context(), recipePath, "")
	if err != nil {
		t.Fatalf("LoadRecipe: %v", err)
	}
	snap, err := client.LoadSnapshot(t.Context(), snapshotPath, "")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	evidence := &recipeEvidenceConfig{
		OutDir:    filepath.Join(dir, "out"),
		Push:      strings.TrimPrefix(srv.URL, "http://") + "/aicr-evidence",
		PlainHTTP: true,
		NoSign:    true,

		AllowMutableValidatorTags: true,
	}
	target := filepath.Join(dir, "refs.txt")
	refs, err := prepareEvidenceImageRefsTarget(t.Context(), target, filepath.Join(dir, "ctrf.json"), evidence)
	if err != nil {
		t.Fatalf("prepareEvidenceImageRefsTarget: %v", err)
	}
	t.Cleanup(func() { _ = refs.close() })

	err = runValidation(t.Context(), client, rec, snap, validationConfig{
		runID:     "image-refs-test",
		output:    filepath.Join(dir, "ctrf.json"),
		outFormat: serializer.FormatJSON,
		noCluster: true,
		evidence:  evidence,
		imageRefs: refs,
	})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}

	var pointer struct {
		Attestations []struct {
			Bundle struct {
				Digest string `yaml:"digest"`
			} `yaml:"bundle"`
		} `yaml:"attestations"`
	}
	if err := yaml.Unmarshal([]byte(readTestFile(t, filepath.Join(evidence.OutDir, "pointer.yaml"))), &pointer); err != nil {
		t.Fatalf("parse pointer.yaml: %v", err)
	}
	if len(pointer.Attestations) != 1 || !strings.HasPrefix(pointer.Attestations[0].Bundle.Digest, "sha256:") {
		t.Fatalf("pointer.yaml attestations = %+v, want one pushed bundle", pointer.Attestations)
	}
	if got, want := readTestFile(t, target), pointer.Attestations[0].Bundle.Digest+"\n"; got != want {
		t.Errorf("refs file = %q, want pointer.yaml bundle.digest %q", got, want)
	}
}

// evalTempDir returns a temp directory with platform symlinks resolved
// (macOS /var -> /private/var), so path assertions compare like with like.
func evalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

func prepareTestEvidenceRefs(t *testing.T, dir, target string) *imageRefsTarget {
	t.Helper()
	refs, err := prepareEvidenceImageRefsTarget(context.Background(), target, "",
		&recipeEvidenceConfig{OutDir: filepath.Join(dir, "out"), Push: "ghcr.io/org/aicr-evidence"})
	if err != nil {
		t.Fatalf("prepareEvidenceImageRefsTarget: %v", err)
	}
	t.Cleanup(func() { _ = refs.close() })
	return refs
}

func mkdirTest(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// newAcceptingAPIServer is a stand-in apiserver that accepts every write the
// validator issues and echoes the object back, so a run with no declared
// checks completes against it: it reports nothing found on reads and success
// on deletes.
func newAcceptingAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// client-go sends core types as protobuf; echo in the request's
			// encoding. Apply patches arrive as JSON.
			if contentType := r.Header.Get("Content-Type"); strings.Contains(contentType, "protobuf") {
				w.Header().Set("Content-Type", contentType)
			}
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = w.Write(body)
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestValidateCmd_ImageRefsWrittenEndToEnd drives `aicr validate` itself, so
// the flag has to travel from the command line through the Action into the
// run: a wiring slip anywhere on that path leaves the file unwritten.
func TestValidateCmd_ImageRefsWrittenEndToEnd(t *testing.T) {
	registrySrv := httptest.NewServer(registry.New())
	t.Cleanup(registrySrv.Close)
	apiserver := newAcceptingAPIServer(t)
	dir := evalTempDir(t)

	kubeconfig := filepath.Join(dir, "kubeconfig")
	writeTestFile(t, kubeconfig, "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: "+
		apiserver.URL+"\ncontexts:\n- name: c\n  context:\n    cluster: c\n    user: u\ncurrent-context: c\nusers:\n- name: u\n  user: {}\n")
	t.Setenv("KUBECONFIG", kubeconfig)
	recipePath := filepath.Join(dir, "recipe.yaml")
	writeTestFile(t, recipePath, "kind: RecipeResult\napiVersion: "+header.StableGroupVersion+"\n"+
		"metadata:\n  version: test\ncriteria:\n  service: eks\n  accelerator: h100\n  intent: training\n"+
		"componentRefs:\n  - name: gpu-operator\n    type: Helm\n    source: https://helm.ngc.nvidia.com/nvidia\n"+
		"    version: v25.10.0\n    overrides:\n      devicePlugin:\n        enabled: true\n")
	snapshotPath := filepath.Join(dir, "snapshot.yaml")
	writeTestFile(t, snapshotPath, "kind: Snapshot\napiVersion: aicr.run/v1\nmetadata:\n  version: test\n"+
		"measurements:\n  - type: K8s\n")
	outDir := filepath.Join(dir, "out")
	target := filepath.Join(dir, "refs.txt")

	err := RootCommand().Run(t.Context(), []string{"aicr", "validate",
		"--recipe", recipePath, "--snapshot", snapshotPath, "--kubeconfig", kubeconfig,
		"--output", filepath.Join(dir, "ctrf.json"),
		"--emit-attestation", outDir,
		"--push", strings.TrimPrefix(registrySrv.URL, "http://") + "/aicr-evidence",
		"--plain-http", "--no-sign", "--" + flagAllowMutableValidatorTags,
		"--image-refs", target,
	})
	if err != nil {
		t.Fatalf("aicr validate error = %v", err)
	}

	var pointer struct {
		Attestations []struct {
			Bundle struct {
				Digest string `yaml:"digest"`
			} `yaml:"bundle"`
		} `yaml:"attestations"`
	}
	if err := yaml.Unmarshal([]byte(readTestFile(t, filepath.Join(outDir, "pointer.yaml"))), &pointer); err != nil {
		t.Fatalf("parse pointer.yaml: %v", err)
	}
	if len(pointer.Attestations) != 1 || !strings.HasPrefix(pointer.Attestations[0].Bundle.Digest, "sha256:") {
		t.Fatalf("pointer.yaml attestations = %+v, want one pushed bundle", pointer.Attestations)
	}
	if got, want := readTestFile(t, target), pointer.Attestations[0].Bundle.Digest+"\n"; got != want {
		t.Errorf("refs file = %q, want pointer.yaml bundle.digest %q", got, want)
	}
}

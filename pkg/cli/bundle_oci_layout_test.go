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
	"os"
	"path/filepath"
	"strings"
	"testing"

	storeoci "oras.land/oras-go/v2/content/oci"

	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/urfave/cli/v3"
)

const layoutTestTag = "1.0.0"

func layoutTestRecipe(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "bundler", "testdata", "layout", "recipe.yaml"))
	if err != nil {
		t.Fatalf("resolve recipe fixture: %v", err)
	}
	return path
}

func runBundleInWorkDir(t *testing.T, workDir string, deps bundleCommandDependencies, args ...string) error {
	t.Helper()
	t.Chdir(workDir)
	cmd := bundleCmd()
	cmd.Action = func(ctx context.Context, command *cli.Command) error {
		return runBundleCmdWithDependencies(ctx, command, deps)
	}
	return cmd.Run(context.Background(), append([]string{"bundle"}, args...))
}

func layoutTagDigest(t *testing.T, dir string) string {
	t.Helper()
	store, err := storeoci.New(dir)
	if err != nil {
		t.Fatalf("open layout %s: %v", dir, err)
	}
	desc, err := store.Resolve(context.Background(), layoutTestTag)
	if err != nil {
		t.Fatalf("resolve %s:%s: %v", dir, layoutTestTag, err)
	}
	return desc.Digest.String()
}

// captureRegistryPublication returns command dependencies that run the real
// oci:// publication path but capture the artifact it would push in a layout
// at dir. PackageAndPush pushes exactly the manifest it packages, which
// pkg/oci's layout tests check against a registry.
func captureRegistryPublication(dir string) bundleCommandDependencies {
	deps := defaultBundleCommandDependencies()
	deps.pushOCIBundle = func(
		ctx context.Context,
		opts *bundleCmdOptions,
		out *result.Output,
		bundle *bundleOutputTarget,
		imageRefs *imageRefsTarget,
	) error {

		publish := defaultBundlePublishDependencies()
		capture := &oci.Reference{IsLayout: true, LocalPath: dir, Tag: opts.ociRef.Tag}
		publish.packageAndPush = func(ctx context.Context, cfg oci.OutputConfig) (*oci.PackageAndPushResult, error) {
			cfg.Reference = capture
			return oci.WriteLayout(ctx, cfg)
		}
		publish.packageAndPushHelm = func(ctx context.Context, cfg oci.HelmChartOptions) (*oci.PackageAndPushResult, error) {
			cfg.Reference = capture
			return oci.WriteHelmChartLayout(ctx, cfg)
		}
		return pushOCIBundleWithDependencies(ctx, opts, out, bundle, imageRefs, publish)
	}
	return deps
}

func TestBundleCommandOCILayoutMatchesRegistryOutput(t *testing.T) {
	recipe := layoutTestRecipe(t)
	// argocd-helm writes the registry namespace into values.yaml, which a
	// layout takes from --repo.
	layoutArgs := map[string][]string{"argocd-helm": {"--repo", "oci://registry.example.com/team"}}
	for _, deployer := range []string{"helm", "helmfile", "terraform", "flux", "argocd-helm"} {
		t.Run(deployer, func(t *testing.T) {
			pushedLayout := filepath.Join(realTempDir(t), "pushed")
			if err := runBundleInWorkDir(t, realTempDir(t), captureRegistryPublication(pushedLayout),
				"--recipe", recipe, "--deployer", deployer,
				"--output", "oci://registry.example.com/team/aicr-bundle:"+layoutTestTag); err != nil {
				t.Fatalf("oci:// bundle: %v", err)
			}

			layout := filepath.Join(realTempDir(t), "layout")
			if err := runBundleInWorkDir(t, realTempDir(t), defaultBundleCommandDependencies(),
				append([]string{"--recipe", recipe, "--deployer", deployer,
					"--output", oci.LayoutScheme + layout + ":" + layoutTestTag}, layoutArgs[deployer]...)...); err != nil {
				t.Fatalf("oci-layout: bundle: %v", err)
			}

			if got, want := layoutTagDigest(t, layout), layoutTagDigest(t, pushedLayout); got != want {
				t.Fatalf("layout digest = %s, oci:// digest = %s", got, want)
			}
		})
	}
}

func TestBundleCommandOCILayoutRejectsExistingTarget(t *testing.T) {
	layout := filepath.Join(realTempDir(t), "layout")
	if err := os.Mkdir(layout, 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := realTempDir(t)
	deps := defaultBundleCommandDependencies()
	deps.prepareBundleOutput = func(context.Context, string) (*bundleOutputTarget, error) {
		t.Fatal("bundle generation started for an existing layout target")
		return nil, errors.New(errors.ErrCodeInternal, "unreachable")
	}
	err := runBundleInWorkDir(t, workDir, deps,
		"--recipe", layoutTestRecipe(t), "--deployer", "helm",
		"--output", oci.LayoutScheme+layout+":"+layoutTestTag)
	errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
	if workEntries, _ := os.ReadDir(workDir); len(workEntries) != 0 {
		t.Fatalf("working directory has leftovers: %v", workEntries)
	}
	entries, readErr := os.ReadDir(layout)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("existing target changed: entries=%v err=%v", entries, readErr)
	}
	parentEntries, readErr := os.ReadDir(filepath.Dir(layout))
	if readErr != nil || len(parentEntries) != 1 {
		t.Fatalf("parent has leftovers: entries=%v err=%v", parentEntries, readErr)
	}
}

func TestParseBundleCmdOptions_OCILayout(t *testing.T) {
	recipe := layoutTestRecipe(t)
	layout := oci.LayoutScheme + filepath.Join(t.TempDir(), "layout")

	tests := []struct {
		name           string
		args           []string
		wantTag        string
		wantRepo       string
		wantSourceName string
		wantChartName  string
		wantNamespace  string
		wantCode       errors.ErrorCode
	}{
		{
			name:    "tag defaults to CLI version",
			args:    []string{"--output", layout, "--deployer", "helm"},
			wantTag: version,
		},
		{
			name:           "flux uses the OCI variant source name",
			args:           []string{"--output", layout + ":v2", "--deployer", "flux", "--flux-oci-source-name", "my-source"},
			wantTag:        "v2",
			wantSourceName: "my-source",
		},
		{
			name:     "argocd takes repoURL from --repo",
			args:     []string{"--output", layout + ":v2", "--deployer", "argocd", "--repo", "oci://ghcr.io/x/y"},
			wantTag:  "v2",
			wantRepo: "oci://ghcr.io/x/y",
		},
		{
			name:     "argocd canonicalizes --repo as oci:// output derives it",
			args:     []string{"--output", layout + ":v2", "--deployer", "argocd", "--repo", "oci://ghcr.io/team/aicr-bundle"},
			wantTag:  "v2",
			wantRepo: "oci://ghcr.io/team/aicr-bundle",
		},
		{
			name:     "argocd rejects a --repo without the oci:// scheme",
			args:     []string{"--output", layout + ":v2", "--deployer", "argocd", "--repo", "ghcr.io/team/aicr-bundle"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "argocd rejects a tagged --repo",
			args:     []string{"--output", layout + ":v2", "--deployer", "argocd", "--repo", "oci://ghcr.io/team/aicr-bundle:v2"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "argocd rejects a digest-bearing --repo",
			args:     []string{"--output", layout + ":v2", "--deployer", "argocd", "--repo", "oci://ghcr.io/team/aicr-bundle@sha256:" + strings.Repeat("a", 64)},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "layout at the bundle staging directory is rejected",
			args:     []string{"--output", oci.LayoutScheme + "bundle:v2", "--deployer", "helm"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "image refs at the layout path is rejected",
			args:     []string{"--output", layout + ":v2", "--deployer", "helm", "--image-refs", strings.TrimPrefix(layout, oci.LayoutScheme)},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "argocd without --repo is rejected",
			args:     []string{"--output", layout + ":v2", "--deployer", "argocd"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:    "argocd-helm leaves chart name and repoURL default unset",
			args:    []string{"--output", layout + ":1.0.0", "--deployer", "argocd-helm"},
			wantTag: "1.0.0",
		},
		{
			name:          "argocd-helm takes chart name and repoURL default from flags",
			args:          []string{"--output", layout + ":1.0.0", "--deployer", "argocd-helm", "--chart-name", "my-chart", "--repo", "oci://ghcr.io/org/team"},
			wantTag:       "1.0.0",
			wantChartName: "my-chart",
			wantNamespace: "oci://ghcr.io/org/team",
		},
		{
			name:          "argocd-helm accepts a registry with no namespace",
			args:          []string{"--output", layout + ":1.0.0", "--deployer", "argocd-helm", "--repo", "oci://localhost:5000"},
			wantTag:       "1.0.0",
			wantNamespace: "oci://localhost:5000",
		},
		{
			name:     "argocd-helm rejects a non-oci --repo",
			args:     []string{"--output", layout + ":1.0.0", "--deployer", "argocd-helm", "--repo", "https://github.com/x/y"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "argocd-helm rejects a tagged --repo",
			args:     []string{"--output", layout + ":1.0.0", "--deployer", "argocd-helm", "--repo", "oci://ghcr.io/org:v1"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "argocd-helm rejects an invalid chart name",
			args:     []string{"--output", layout + ":1.0.0", "--deployer", "argocd-helm", "--chart-name", "My_Chart/x"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "chart name rejected with oci:// output",
			args:     []string{"--output", "oci://ghcr.io/org/aicr-bundle:1.0.0", "--deployer", "argocd-helm", "--chart-name", "x"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "chart name rejected with directory output",
			args:     []string{"--output", filepath.Join(t.TempDir(), "out"), "--deployer", "argocd-helm", "--chart-name", "x"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "chart name rejected with other deployers",
			args:     []string{"--output", layout, "--deployer", "helm", "--chart-name", "x"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "split is rejected",
			args:     []string{"--output", layout, "--deployer", "helm", "--split"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "fleet is rejected",
			args:     []string{"--output", layout, "--deployer", "fleet"},
			wantCode: errors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := tryCaptureBundleOpts(t, append([]string{"--recipe", recipe}, tt.args...))
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if opts.ociRef == nil || !opts.ociRef.IsLayout || opts.ociRef.Tag != tt.wantTag {
				t.Fatalf("ociRef = %+v, want layout tagged %q", opts.ociRef, tt.wantTag)
			}
			if opts.targetRevision != tt.wantTag || opts.repoURL != tt.wantRepo ||
				opts.ociSourceName != tt.wantSourceName || opts.bundleChartName != tt.wantChartName ||
				opts.ociParentNamespace != tt.wantNamespace {

				t.Fatalf("targetRevision=%q repoURL=%q ociSourceName=%q bundleChartName=%q ociParentNamespace=%q",
					opts.targetRevision, opts.repoURL, opts.ociSourceName, opts.bundleChartName, opts.ociParentNamespace)
			}
		})
	}
}

func TestBundleCommandOCILayoutWritesImageRefs(t *testing.T) {
	layout := filepath.Join(realTempDir(t), "layout")
	refs := filepath.Join(realTempDir(t), "refs.txt")
	if err := runBundleInWorkDir(t, realTempDir(t), defaultBundleCommandDependencies(),
		"--recipe", layoutTestRecipe(t), "--deployer", "helm",
		"--output", oci.LayoutScheme+layout+":"+layoutTestTag, "--image-refs", refs); err != nil {
		t.Fatalf("oci-layout: bundle: %v", err)
	}
	data, err := os.ReadFile(refs)
	if err != nil {
		t.Fatalf("read image refs: %v", err)
	}
	if want := layoutTagDigest(t, layout) + "\n"; string(data) != want {
		t.Fatalf("image refs = %q, want %q", data, want)
	}
}

func TestBundleCommandRejectsRepeatedChartName(t *testing.T) {
	deps := defaultBundleCommandDependencies()
	deps.prepareBundleOutput = func(context.Context, string) (*bundleOutputTarget, error) {
		t.Fatal("bundle generation started with a repeated --chart-name")
		return nil, errors.New(errors.ErrCodeInternal, "unreachable")
	}
	err := runBundleInWorkDir(t, realTempDir(t), deps,
		"--recipe", layoutTestRecipe(t), "--deployer", "argocd-helm",
		"--output", oci.LayoutScheme+filepath.Join(realTempDir(t), "layout")+":"+layoutTestTag,
		"--chart-name", "a", "--chart-name", "b")
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

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

package bundleinfo_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	stderrors "errors"

	"golang.org/x/sys/unix"

	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

func sample() *bundleinfo.BundleInfo {
	return &bundleinfo.BundleInfo{
		APIVersion: header.StableGroupVersion,
		Kind:       string(header.KindBundleInfo),
		Metadata:   bundleinfo.Metadata{Version: "v0.24.1"},
		Build: bundleinfo.Build{
			Deployer: "argocd",
			Recipe: bundleinfo.Recipe{
				Path:    "recipe.yaml",
				Digest:  "sha256:3b1f8c2ad9e7546102bb8f4c7d0e9a1358cc4f6b2e8d70a94f1c5b3e6d820947",
				Version: "v0.22.0",
			},
			Settings: bundleinfo.Settings{
				Checksums:  true,
				Attested:   true,
				Components: []string{"cert-manager", "nfd"},
				RepoURL:    "https://github.com/my-org/gitops.git",
				NodeScheduling: &bundleinfo.NodeScheduling{
					System: &bundleinfo.Scheduling{
						// Multiple keys: single-key maps cannot detect sort loss.
						Selector: map[string]string{
							"zzz-last":  "true",
							"aaa-first": "true",
							"mmm-mid":   "true",
						},
						Tolerations: []bundleinfo.Toleration{
							{Key: "nvidia.com/aicr-system", Operator: "Exists", Effect: "NoSchedule"},
						},
					},
				},
			},
		},
		Layout: bundleinfo.Layout{
			Entrypoint: "app-of-apps.yaml",
			Releases: []bundleinfo.Release{
				{Name: "cert-manager", Component: "cert-manager", Namespace: "cert-manager",
					Path: "001-cert-manager", Manifest: "001-cert-manager/application.yaml"},
				{Name: "nfd", Component: "nfd", Namespace: "node-feature-discovery",
					Path: "002-nfd", Manifest: "002-nfd/application.yaml"},
			},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := bundleinfo.Write(context.Background(), dir, sample()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := bundleinfo.Read(context.Background(), dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Build.Deployer != "argocd" {
		t.Errorf("deployer = %q, want argocd", got.Build.Deployer)
	}
	if got.Layout.Entrypoint != "app-of-apps.yaml" {
		t.Errorf("entrypoint = %q, want app-of-apps.yaml", got.Layout.Entrypoint)
	}
	if len(got.Layout.Releases) != 2 {
		t.Fatalf("releases = %d, want 2", len(got.Layout.Releases))
	}
	// Order is normative: sequence carries deployment order, so a reader
	// must see the same order the writer emitted.
	if got.Layout.Releases[0].Name != "cert-manager" || got.Layout.Releases[1].Name != "nfd" {
		t.Errorf("release order = [%s %s], want [cert-manager nfd]",
			got.Layout.Releases[0].Name, got.Layout.Releases[1].Name)
	}
}

func TestWriteIsDeterministic(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if _, err := bundleinfo.Write(context.Background(), first, sample()); err != nil {
		t.Fatalf("Write first: %v", err)
	}
	if _, err := bundleinfo.Write(context.Background(), second, sample()); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	a, err := os.ReadFile(filepath.Join(first, bundleinfo.FileName))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(second, bundleinfo.FileName))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("bundle-info.yaml is not byte-stable across runs;\nfirst:\n%s\nsecond:\n%s", a, b)
	}
}

// TestWriteRejectsEscapingPaths guards the constraint that no path escaping
// the bundle directory ever reaches bundle-info.yaml: downstream readers
// resolve emitted paths with filepath.Join(outDir, path), which returns an
// absolute right-hand argument unchanged and Cleans a leading "../" away, so
// either shape would silently resolve outside outDir.
func TestWriteRejectsEscapingPaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*bundleinfo.BundleInfo)
	}{
		{
			name: "absolute entrypoint",
			mutate: func(info *bundleinfo.BundleInfo) {
				info.Layout.Entrypoint = "/etc/passwd"
			},
		},
		{
			name: "absolute release path",
			mutate: func(info *bundleinfo.BundleInfo) {
				info.Layout.Releases[0].Path = "/etc/passwd"
			},
		},
		{
			name: "parent-traversal entrypoint",
			mutate: func(info *bundleinfo.BundleInfo) {
				info.Layout.Entrypoint = "../../../etc/passwd"
			},
		},
		{
			name: "parent-traversal release path",
			mutate: func(info *bundleinfo.BundleInfo) {
				info.Layout.Releases[0].Path = "../../../etc"
			},
		},
		{
			name: "parent-traversal release manifest",
			mutate: func(info *bundleinfo.BundleInfo) {
				info.Layout.Releases[0].Manifest = "001-cert-manager/../../../etc/passwd"
			},
		},
		{
			name: "parent-traversal recipe path",
			mutate: func(info *bundleinfo.BundleInfo) {
				info.Build.Recipe.Path = "../recipe.yaml"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := sample()
			tt.mutate(info)
			_, err := bundleinfo.Write(context.Background(), t.TempDir(), info)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

// TestReadRejectsOversizeFile guards the defaults.MaxBundleInfoBytes cap.
// The oversize content is generated in memory rather than committed as a
// fixture.
func TestReadRejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, bundleinfo.FileName)
	oversize := bytes.Repeat([]byte("a"), int(defaults.MaxBundleInfoBytes+1))
	if err := os.WriteFile(path, oversize, 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := bundleinfo.Read(context.Background(), dir)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
	}
}

// TestReadRejectsNonRegularFile guards the filesystem entry Read opens, which
// is a separate question from the paths inside the document.
//
// validateRelativePaths inspects decoded fields, and deployer.SafeJoin is
// purely lexical over the constant FileName — neither ever stats the entry. A
// bundle that arrived from an OCI registry or a GitOps clone can therefore
// carry a bundle-info.yaml that is a symlink to anywhere the process can
// reach, and a following open would feed up to MaxBundleInfoBytes of it to the
// YAML decoder.
func TestReadRejectsNonRegularFile(t *testing.T) {
	tests := []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{
			name: "symlink to a file outside the bundle",
			plant: func(t *testing.T, path string) {
				t.Helper()
				// The target parses as a valid record on purpose. A target the
				// decoder would reject anyway makes this pass whether or not
				// the link was followed, which is the wrong reason.
				outside := filepath.Join(t.TempDir(), "elsewhere.yaml")
				if err := os.WriteFile(outside, []byte(completeRecord()), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo",
			plant: func(t *testing.T, path string) {
				t.Helper()
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Skipf("FIFO unsupported: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.plant(t, filepath.Join(dir, bundleinfo.FileName))

			_, err := bundleinfo.Read(context.Background(), dir)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func TestReadFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		content string // empty means: write no file at all
		code    errors.ErrorCode
	}{
		{
			name: "missing file",
			code: errors.ErrCodeNotFound,
		},
		{
			name:    "unknown apiVersion",
			content: "apiVersion: aicr.run/v1alpha1\nkind: BundleInfo\n",
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			// BundleInfo shipped at the ADR-022 stable target with no alpha
			// predecessor, so this names a document that never legitimately
			// existed — the generic stable-track predicate accepts it, which
			// is why Read uses the BundleInfo-specific one.
			name:    "superseded alpha apiVersion",
			content: "apiVersion: " + header.GroupVersion + "\nkind: BundleInfo\n",
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name:    "wrong kind",
			content: "apiVersion: " + header.StableGroupVersion + "\nkind: RecipeResult\n",
			code:    errors.ErrCodeInvalidRequest,
		},
		{
			name: "unknown field",
			content: "apiVersion: " + header.StableGroupVersion + "\nkind: BundleInfo\n" +
				"somethingNobodyDeclared: true\n",
			code: errors.ErrCodeInvalidRequest,
		},
		{
			// The record on this side arrived from an OCI registry or a
			// GitOps clone, so a path that resolves outside the bundle has to
			// fail here — Write's guard never saw this file.
			name: "parent-traversal release path",
			content: "apiVersion: " + header.StableGroupVersion + "\nkind: BundleInfo\n" +
				"layout:\n  entrypoint: deploy.sh\n  releases:\n" +
				"    - name: cert-manager\n      component: cert-manager\n" +
				"      path: ../../../etc\n",
			code: errors.ErrCodeInvalidRequest,
		},
		{
			name: "absolute entrypoint",
			content: "apiVersion: " + header.StableGroupVersion + "\nkind: BundleInfo\n" +
				"layout:\n  entrypoint: /etc/passwd\n",
			code: errors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" {
				path := filepath.Join(dir, bundleinfo.FileName)
				if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			_, err := bundleinfo.Read(context.Background(), dir)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !stderrors.Is(err, errors.New(tt.code, "")) {
				t.Errorf("error = %v, want code %s", err, tt.code)
			}
		})
	}
}

// completeRecord is the smallest document Read must accept: every
// non-optional semantic field set, and no releases. A bundle whose recipe
// resolved to no components emits exactly this, so an empty release list is
// a legitimate record rather than a truncated one.
func completeRecord() string {
	return "apiVersion: " + header.StableGroupVersion + "\nkind: BundleInfo\n" +
		"build:\n  deployer: helm\n  recipe:\n    path: recipe.yaml\n" +
		"    digest: sha256:3b1f8c2ad9e7546102bb8f4c7d0e9a1358cc4f6b2e8d70a94f1c5b3e6d820947\n" +
		"layout:\n  entrypoint: deploy.sh\n"
}

// TestReadRejectsIncompleteRecord covers the half of "fails closed" that the
// kind, apiVersion and path checks leave open: a document that clears all
// three and still says nothing. Every field below is required by its struct
// tag, so a record missing one did not come from Write — it was hand-written
// or truncated in transit, and the file Read parses arrived from an OCI
// registry or a GitOps clone.
func TestReadRejectsIncompleteRecord(t *testing.T) {
	const header0 = "apiVersion: " + header.StableGroupVersion + "\nkind: BundleInfo\n"
	const recipeFields = "  recipe:\n    path: recipe.yaml\n" +
		"    digest: sha256:3b1f8c2ad9e7546102bb8f4c7d0e9a1358cc4f6b2e8d70a94f1c5b3e6d820947\n"

	tests := []struct {
		name    string
		content string
		wantErr string // substring naming the offending field
	}{
		{
			name:    "header only",
			content: header0,
			wantErr: "build.deployer",
		},
		{
			name: "missing deployer",
			content: header0 + "build:\n" + recipeFields +
				"layout:\n  entrypoint: deploy.sh\n",
			wantErr: "build.deployer",
		},
		{
			name: "deployer outside the accepted set",
			content: header0 + "build:\n  deployer: kustomize\n" + recipeFields +
				"layout:\n  entrypoint: deploy.sh\n",
			wantErr: "build.deployer",
		},
		{
			name: "missing recipe path",
			content: header0 + "build:\n  deployer: helm\n  recipe:\n" +
				"    digest: sha256:3b1f8c2ad9e7546102bb8f4c7d0e9a1358cc4f6b2e8d70a94f1c5b3e6d820947\n" +
				"layout:\n  entrypoint: deploy.sh\n",
			wantErr: "build.recipe.path",
		},
		{
			name: "missing recipe digest",
			content: header0 + "build:\n  deployer: helm\n  recipe:\n    path: recipe.yaml\n" +
				"layout:\n  entrypoint: deploy.sh\n",
			wantErr: "build.recipe.digest",
		},
		{
			name:    "missing entrypoint",
			content: header0 + "build:\n  deployer: helm\n" + recipeFields,
			wantErr: "layout.entrypoint",
		},
		{
			name: "release without a name",
			content: header0 + "build:\n  deployer: helm\n" + recipeFields +
				"layout:\n  entrypoint: deploy.sh\n  releases:\n" +
				"    - component: cert-manager\n      path: 001-cert-manager\n",
			wantErr: "layout.releases[0].name",
		},
		{
			name: "release without a component",
			content: header0 + "build:\n  deployer: helm\n" + recipeFields +
				"layout:\n  entrypoint: deploy.sh\n  releases:\n" +
				"    - name: cert-manager\n      path: 001-cert-manager\n",
			wantErr: "layout.releases[0].component",
		},
		{
			name: "release without a path",
			content: header0 + "build:\n  deployer: helm\n" + recipeFields +
				"layout:\n  entrypoint: deploy.sh\n  releases:\n" +
				"    - name: cert-manager\n      component: cert-manager\n",
			wantErr: "layout.releases[0].path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, bundleinfo.FileName),
				[]byte(tt.content), 0600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_, err := bundleinfo.Read(context.Background(), dir)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

// TestReadAcceptsCompleteRecord is the positive half of the rejection table:
// a record with every required field and no releases must still round-trip,
// so the new gate cannot be satisfied by rejecting everything.
func TestReadAcceptsCompleteRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, bundleinfo.FileName),
		[]byte(completeRecord()), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	info, err := bundleinfo.Read(context.Background(), dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.Build.Deployer != "helm" {
		t.Errorf("deployer = %q, want helm", info.Build.Deployer)
	}
	if len(info.Layout.Releases) != 0 {
		t.Errorf("releases = %d, want 0", len(info.Layout.Releases))
	}
}

// TestWriteRefusesSymlinkedTarget covers the window ValidateOutputRoot cannot
// close: it rejects a symlinked bundle-info.yaml at preflight, but anything
// with write access to the output directory can plant one between that check
// and the write. os.WriteFile follows the final symlink and truncates its
// target (CWE-59), so the open itself has to refuse.
func TestWriteRefusesSymlinkedTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim.yaml")
	const original = "do not truncate me\n"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, bundleinfo.FileName)); err != nil {
		t.Fatal(err)
	}

	_, err := bundleinfo.Write(context.Background(), dir, sample())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
	}

	got, readErr := os.ReadFile(target) //nolint:gosec // test-local temp path
	if readErr != nil {
		t.Fatalf("read symlink target: %v", readErr)
	}
	if string(got) != original {
		t.Errorf("symlink target was written through: content = %q, want %q", got, original)
	}
}

// TestContextErrorsAreCodedByCause pins cancellation to ErrCodeCanceled and
// only a deadline to ErrCodeTimeout. Collapsing both onto the timeout code
// makes a deliberate Ctrl-C look retryable: errors.IsTransient reports true
// for a timeout and false for a cancellation.
func TestContextErrorsAreCodedByCause(t *testing.T) {
	canceled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	expired := func() context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		t.Cleanup(cancel)
		return ctx
	}

	tests := []struct {
		name     string
		ctx      context.Context
		call     func(context.Context, string) error
		wantCode errors.ErrorCode
		wantErr  error
	}{
		{
			name: "write canceled",
			ctx:  canceled(),
			call: func(ctx context.Context, dir string) error {
				_, err := bundleinfo.Write(ctx, dir, sample())
				return err
			},
			wantCode: errors.ErrCodeCanceled,
			wantErr:  context.Canceled,
		},
		{
			name: "write deadline exceeded",
			ctx:  expired(),
			call: func(ctx context.Context, dir string) error {
				_, err := bundleinfo.Write(ctx, dir, sample())
				return err
			},
			wantCode: errors.ErrCodeTimeout,
			wantErr:  context.DeadlineExceeded,
		},
		{
			name: "read canceled",
			ctx:  canceled(),
			call: func(ctx context.Context, dir string) error {
				_, err := bundleinfo.Read(ctx, dir)
				return err
			},
			wantCode: errors.ErrCodeCanceled,
			wantErr:  context.Canceled,
		},
		{
			name: "read deadline exceeded",
			ctx:  expired(),
			call: func(ctx context.Context, dir string) error {
				_, err := bundleinfo.Read(ctx, dir)
				return err
			},
			wantCode: errors.ErrCodeTimeout,
			wantErr:  context.DeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(tt.ctx, t.TempDir())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want code %s", err, tt.wantCode)
			}
			if !stderrors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want the original %v preserved as the cause", err, tt.wantErr)
			}
		})
	}
}

// TestWriteRejectsOversizeRecord keeps Write from producing a file Read
// refuses: without the check the bundle ships, checksums cover the oversize
// record, and the failure surfaces only when a consumer tries to read it.
func TestWriteRejectsOversizeRecord(t *testing.T) {
	info := sample()
	filler := strings.Repeat("c", 1024)
	for len(info.Build.Settings.Components)*len(filler) <= int(defaults.MaxBundleInfoBytes) {
		info.Build.Settings.Components = append(info.Build.Settings.Components, filler)
	}

	_, err := bundleinfo.Write(context.Background(), t.TempDir(), info)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want code %s", err, errors.ErrCodeInvalidRequest)
	}
}

// TestSettingsKeysAreAllowlisted is the gate that makes the "already
// observable in the bundle" admission rule enforceable instead of
// remembered. A new Config accessor wired into Settings fails here until
// someone reviews whether its effect is visible in the bundle at all.
func TestSettingsKeysAreAllowlisted(t *testing.T) {
	allowed := map[string]bool{
		"checksums": true, "attested": true, "vendorCharts": true,
		"readinessHooks": true, "serial": true, "components": true,
		"repoURL": true, "targetRevision": true, "appName": true,
		"storageClass": true, "sharedStorageClass": true, "nodeScheduling": true,
	}

	typ := reflect.TypeOf(bundleinfo.Settings{})
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("yaml")
		key, _, _ := strings.Cut(tag, ",")
		if !allowed[key] {
			t.Errorf("Settings has undeclared key %q. bundle-info.yaml is pushed to "+
				"registries and committed to GitOps repos: a setting belongs here only "+
				"when its effect is ALREADY observable in the bundle's own files. "+
				"Endpoints, credentials and security posture are excluded. If this key "+
				"passes that test, add it to the allowlist and say why in the PR.", key)
		}
		delete(allowed, key)
	}
	for key := range allowed {
		t.Errorf("allowlist declares %q but Settings has no such field; remove the stale entry", key)
	}
}

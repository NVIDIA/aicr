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

package corroborate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRunMeta(t *testing.T) {
	p := filepath.Join(fixtureGCS, "results", "eks", "h100-ubuntu", "training-kubeflow",
		"a1nvidia", "run-2026-06-20T0314", "meta.json")
	m, err := readRunMeta(p)
	if err != nil {
		t.Fatalf("readRunMeta: %v", err)
	}
	if m.SchemaVersion != RunMetaSchemaVersion {
		t.Errorf("schema = %q, want %q", m.SchemaVersion, RunMetaSchemaVersion)
	}
	if m.Coordinate.Group != "eks" || m.Signer.IDHash != "a1nvidia" || m.AICRVersion != "v1.0.0" {
		t.Errorf("unexpected meta: %+v", m)
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := readRunMeta(filepath.Join(t.TempDir(), "nope.json")); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "meta.json")
		if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readRunMeta(bad); err == nil {
			t.Fatal("expected parse error")
		}
	})
}

func TestReadCTRF(t *testing.T) {
	p := filepath.Join(fixtureGCS, "results", "eks", "h100-ubuntu", "training-kubeflow",
		"a1nvidia", "run-2026-06-20T0314", "ctrf", "deployment.json")
	r, err := readCTRF(p)
	if err != nil {
		t.Fatalf("readCTRF: %v", err)
	}
	if r.ReportFormat != "CTRF" || len(r.Results.Tests) == 0 {
		t.Errorf("unexpected ctrf: %+v", r.Results.Summary)
	}

	t.Run("malformed json", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "x.json")
		if err := os.WriteFile(bad, []byte("[}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCTRF(bad); err == nil {
			t.Fatal("expected parse error")
		}
	})
}

func TestReadBoundedFileSizeLimit(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file just over the limit — no need to write 16 MiB of real bytes.
	if err := f.Truncate(maxRunFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(big); err == nil {
		t.Fatal("expected size-limit rejection")
	}
}

func TestReadBoundedFileRejectsSymlinkAndNonRegular(t *testing.T) {
	t.Run("symlink is not followed", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
		if _, err := readBoundedFile(link); err == nil {
			t.Fatal("expected symlink rejection")
		}
	})
	t.Run("non-regular file is rejected", func(t *testing.T) {
		// A directory is non-regular; readBoundedFile must reject it rather than
		// attempting to read it.
		if _, err := readBoundedFile(t.TempDir()); err == nil {
			t.Fatal("expected non-regular-file rejection")
		}
	})
}

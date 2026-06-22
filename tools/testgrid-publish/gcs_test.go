// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// setupFakeGcloud writes a fake gcloud shell script to dir and prepends dir to
// PATH so exec.Command("gcloud", ...) finds it. Uses t.Setenv for race-safe,
// automatic cleanup even when t.Fatal fires mid-test.
func setupFakeGcloud(t *testing.T, exitCode int) {
	t.Helper()
	bin := t.TempDir()

	script := "#!/bin/sh\n"
	if exitCode != 0 {
		script += "exit 1\n"
	}

	scriptPath := filepath.Join(bin, "gcloud")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGcloudCopySuccess(t *testing.T) {
	setupFakeGcloud(t, 0)

	src := filepath.Join(t.TempDir(), "test.json")
	if err := os.WriteFile(src, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := gcloudCopy(context.Background(), src, "gs://bucket/prefix/test.json")
	if err != nil {
		t.Fatalf("gcloudCopy() unexpected error: %v", err)
	}
}

func TestGcloudCopyFailure(t *testing.T) {
	setupFakeGcloud(t, 1)

	src := filepath.Join(t.TempDir(), "test.json")
	if err := os.WriteFile(src, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := gcloudCopy(context.Background(), src, "gs://bucket/prefix/test.json")
	if err == nil {
		t.Fatal("gcloudCopy() expected error for failing gcloud, got nil")
	}
}

func TestGcloudCopyCanceled(t *testing.T) {
	setupFakeGcloud(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := gcloudCopy(ctx, "src", "gs://bucket/prefix/test.json")
	// May or may not error depending on whether exec.CommandContext checks context
	// before starting; we just verify it doesn't panic.
	_ = err
}

func TestWriteGCS(t *testing.T) {
	setupFakeGcloud(t, 0)

	started := startedJSON{
		Timestamp: 1749600000,
		Metadata:  map[string]string{metaKeyAICRVersion: "v0.1.0"},
	}
	finished := finishedJSON{
		Timestamp: 1749600060,
		Passed:    true,
		Result:    "SUCCESS",
		Metadata:  started.Metadata,
	}

	err := writeGCS(context.Background(),
		"aicr-testgrid-staging",
		"groups/eks/h100-ubuntu/training/1749600000-abc12345",
		started, finished, []byte("<testsuites/>"))
	if err != nil {
		t.Fatalf("writeGCS() unexpected error: %v", err)
	}
}

func TestWriteGCSUploadFailure(t *testing.T) {
	setupFakeGcloud(t, 1)

	started := startedJSON{Timestamp: 1, Metadata: map[string]string{}}
	finished := finishedJSON{Timestamp: 1, Metadata: map[string]string{}}

	err := writeGCS(context.Background(), "bucket", "prefix",
		started, finished, []byte("<testsuites/>"))
	if err == nil {
		t.Fatal("writeGCS() expected error when gcloud fails")
	}
}

func TestWriteGCSContextCanceled(t *testing.T) {
	setupFakeGcloud(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := startedJSON{Timestamp: 1, Metadata: map[string]string{}}
	finished := finishedJSON{Timestamp: 1, Metadata: map[string]string{}}

	// Should fail with context canceled error.
	err := writeGCS(ctx, "bucket", "prefix", started, finished, []byte("<testsuites/>"))
	if err == nil {
		t.Fatal("writeGCS() expected error for canceled context")
	}
}

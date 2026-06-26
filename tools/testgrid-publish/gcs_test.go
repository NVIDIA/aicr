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
	"slices"
	"strings"
	"testing"
)

// setupFakeGcloud writes a fake gcloud shell script to bin and prepends bin to
// PATH so exec.Command("gcloud", ...) finds it. The script records each GCS
// destination ($4 of `gcloud storage cp <src> <dst>`) to GCLOUD_RECORD so
// callers can assert upload order. Uses t.Setenv for race-safe cleanup.
// Returns the path of the call-recorder file.
func setupFakeGcloud(t *testing.T, exitCode int) string {
	t.Helper()
	bin := t.TempDir()
	recordPath := filepath.Join(t.TempDir(), "calls.log")

	// $4 is the gs:// destination; $1=storage $2=cp $3=local-src $4=dst.
	script := "#!/bin/sh\nprintf '%s\\n' \"$4\" >> \"$GCLOUD_RECORD\"\n"
	if exitCode != 0 {
		script += "exit 1\n"
	}

	scriptPath := filepath.Join(bin, "gcloud")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GCLOUD_RECORD", recordPath)
	return recordPath
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
	if err == nil {
		t.Fatal("gcloudCopy() expected error for pre-canceled context")
	}
}

func TestWriteGCS(t *testing.T) {
	recordPath := setupFakeGcloud(t, 0)

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

	const prefix = "groups/eks/h100-ubuntu/training/1749600000-abc12345"
	err := writeGCS(context.Background(),
		"aicr-testgrid-staging", prefix,
		started, finished, []byte("<testsuites/>"))
	if err != nil {
		t.Fatalf("writeGCS() unexpected error: %v", err)
	}

	// Assert the three uploads happened in the mandatory order:
	// started.json → artifacts/junit.xml → finished.json
	data, readErr := os.ReadFile(recordPath)
	if readErr != nil {
		t.Fatalf("read gcloud call record: %v", readErr)
	}
	got := strings.Fields(string(data))
	base := "gs://aicr-testgrid-staging/" + prefix
	want := []string{
		base + "/started.json",
		base + "/artifacts/junit.xml",
		base + "/finished.json",
	}
	if !slices.Equal(got, want) {
		t.Errorf("upload order\n got:  %v\n want: %v", got, want)
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

	err := writeGCS(ctx, "bucket", "prefix", started, finished, []byte("<testsuites/>"))
	if err == nil {
		t.Fatal("writeGCS() expected error for canceled context")
	}
}

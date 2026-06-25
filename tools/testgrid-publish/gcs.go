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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// writeGCS writes the three TestGrid build files to GCS in the mandatory
// order: started.json → artifacts/junit.xml → finished.json.
//
// The updater discovers a build as complete only after finished.json lands,
// so this ordering ensures the updater either sees all three files or none.
//
// Files are staged to a local temp directory first, then uploaded via
// `gcloud storage cp`. Authentication uses Application Default Credentials
// (ADC) — in GitHub Actions this is provided by Workload Identity Federation.
//
// GCS path layout (4-level depth required by TestGrid updater):
//
//	gs://{bucket}/groups/{group}/{dashboard}/{tab}/{build-id}/
//	  started.json
//	  artifacts/junit.xml
//	  finished.json
func writeGCS(
	ctx context.Context,
	bucket, prefix string,
	started startedJSON,
	finished finishedJSON,
	junitXML []byte,
) error {

	if _, err := exec.LookPath("gcloud"); err != nil {
		return errors.New(errors.ErrCodeUnavailable,
			"gcloud not found in PATH: install Google Cloud SDK (https://cloud.google.com/sdk/docs/install)")
	}

	// Stage files locally first so each gcloud upload is a completed file.
	tmp, err := os.MkdirTemp("", "testgrid-publish-gcs-")
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create temp dir for GCS staging", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	startedBytes, err := json.Marshal(started)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "marshal started.json", err)
	}
	finishedBytes, err := json.Marshal(finished)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "marshal finished.json", err)
	}

	// Write local files.
	artifactsDir := filepath.Join(tmp, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o700); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create artifacts dir", err)
	}
	writes := []struct {
		localPath string
		data      []byte
	}{
		{filepath.Join(tmp, "started.json"), startedBytes},
		{filepath.Join(artifactsDir, "junit.xml"), junitXML},
		{filepath.Join(tmp, "finished.json"), finishedBytes},
	}
	for _, w := range writes {
		if err := os.WriteFile(w.localPath, w.data, 0o600); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("write local %s", filepath.Base(w.localPath)), err)
		}
	}

	// Upload in order: started → junit → finished.
	// The updater marks a build complete only when finished.json exists.
	// Idempotency: build-id is deterministic (attestation timestamp + digest),
	// so a retry after partial failure overwrites the same GCS paths cleanly.
	uploads := []struct {
		localPath string
		gcsObject string
	}{
		{filepath.Join(tmp, "started.json"), fmt.Sprintf("gs://%s/%s/started.json", bucket, prefix)},
		{filepath.Join(artifactsDir, "junit.xml"), fmt.Sprintf("gs://%s/%s/artifacts/junit.xml", bucket, prefix)},
		{filepath.Join(tmp, "finished.json"), fmt.Sprintf("gs://%s/%s/finished.json", bucket, prefix)},
	}
	for _, u := range uploads {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeUnavailable, "GCS upload canceled", err)
		}
		if err := gcloudCopy(ctx, u.localPath, u.gcsObject); err != nil {
			return err
		}
	}
	return nil
}

// gcloudCopy uploads a single local file to GCS via `gcloud storage cp`.
// Authentication is handled by ADC / the gcloud CLI configuration.
func gcloudCopy(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "cp", src, dst)
	cmd.Stdout = os.Stderr // gcloud progress to stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.Wrap(errors.ErrCodeUnavailable,
				fmt.Sprintf("gcloud storage cp %s → %s canceled", src, dst), ctx.Err())
		}
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("gcloud storage cp %s → %s", src, dst), err)
	}
	return nil
}

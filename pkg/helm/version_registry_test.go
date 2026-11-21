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

package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func loopbackChartArchive(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	content := []byte("apiVersion: v2\nname: example\nversion: v1.2.3+build.1\n")
	if err := archive.WriteHeader(&tar.Header{Name: "example/Chart.yaml", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func loopbackChartRegistry(t *testing.T, chart []byte, requested *atomic.Bool) *httptest.Server {
	t.Helper()
	digest := func(data []byte) string { sum := sha256.Sum256(data); return "sha256:" + hex.EncodeToString(sum[:]) }
	config := []byte(`{"name":"example","version":"v1.2.3+build.1","apiVersion":"v2"}`)
	descriptor := func(mediaType string, data []byte) map[string]any {
		return map[string]any{"mediaType": mediaType, "size": len(data), "digest": digest(data)}
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": descriptor("application/vnd.cncf.helm.config.v1+json", config),
		"layers": []any{descriptor("application/vnd.cncf.helm.chart.content.v1.tar+gzip", chart)},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []byte
		mediaType := "application/octet-stream"
		switch r.URL.Path {
		case "/v2/":
			data = []byte(`{}`)
		case "/v2/charts/example/manifests/v1.2.3_build.1":
			requested.Store(true)
			data = manifest
			mediaType = "application/vnd.oci.image.manifest.v1+json"
		case "/v2/charts/example/blobs/" + digest(config):
			data = config
		case "/v2/charts/example/blobs/" + digest(chart):
			data = chart
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", digest(data))
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestChartVersionPullLoopback(t *testing.T) {
	binary, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary is required for loopback OCI pull")
	}
	chart := loopbackChartArchive(t)
	var requested atomic.Bool
	server := loopbackChartRegistry(t, chart, &requested)
	repository := "oci://" + strings.TrimPrefix(server.URL, "http://") + "/charts"
	output := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "pull", repository+"/example", "--version", ChartVersion(repository, "v1.2.3_build.1"), "--plain-http", "--destination", output)
	command.Env = append(os.Environ(), "HELM_REGISTRY_CONFIG="+filepath.Join(t.TempDir(), "registry.json"))
	if result, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("helm pull: %v\n%s", runErr, result)
	}
	if !requested.Load() {
		t.Fatal("Helm did not retrieve the literal OCI tag")
	}
	files, err := filepath.Glob(filepath.Join(output, "*.tgz"))
	if err != nil || len(files) != 1 {
		t.Fatalf("chart archives %v, error %v", files, err)
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, chart) {
		t.Fatal("Helm pulled different archive bytes")
	}
}

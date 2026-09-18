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

package inventory

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	stderrors "errors"
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// gzipBytes compresses raw the way Helm's encodeRelease does, stopping short
// of the base64 layer so gunzipBounded can be fed directly.
func gzipBytes(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// encodeReleaseFixture mirrors Helm's encodeRelease (pkg/storage/driver/util.go)
// so the decoder is tested against the encoding it will actually meet.
func encodeReleaseFixture(t *testing.T, raw string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(gzipBytes(t, raw))
}

// releaseFields is the flat expectation for a decoded record. Every field of
// helmRelease appears here: the struct is a hand-written mirror of Helm's wire
// format, so a mistyped json tag is the failure mode to guard, and it is
// invisible to any assertion that checks only the fields a test happens to
// care about.
//
// The release's own name, namespace and revision are absent because the
// decoder no longer reads them; the storage labels are their one source, which
// TestHelmReleases pins against a payload that disagrees.
type releaseFields struct {
	status       string
	chartName    string
	chartVersion string
	appVersion   string
	annotations  map[string]string
}

func TestDecodeRelease(t *testing.T) {
	const releaseName = "gpu-operator"

	// Embedded in a payload that fails to parse. A record carries rendered
	// manifests, so the decoder must not lift any span of the payload into the
	// error; the one byte json.SyntaxError names is the documented exception.
	const payloadMarker = "SUPERSECRETTOKENVALUE"

	// Every field distinct, and deliberately not equal to its neighbors:
	// release name differs from chart name, appVersion from chart version.
	const fullRecord = `{"name":"gpu-operator-release","info":{"status":"deployed"},` +
		`"chart":{"metadata":{"name":"gpu-operator","version":"v25.3.3","appVersion":"25.3.3",` +
		`"annotations":{"aicr.run/component-version":"1.4.2"}}},` +
		`"version":7,"namespace":"gpu-operator-system"}`

	const minimalRecord = `{"name":"gpu-operator","chart":{"metadata":{"name":"gpu-operator","version":"v25.3.3"}}}`

	gzipMagicOnly := []byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad, 0xbe, 0xef}

	tests := []struct {
		name               string
		data               string
		wantErr            bool
		wantErrContext     map[string]any
		wantErrContains    []string
		wantErrNotContains []string
		want               releaseFields
	}{
		{
			name: "gzipped release populates every field",
			data: encodeReleaseFixture(t, fullRecord),
			want: releaseFields{
				status:       "deployed",
				chartName:    "gpu-operator",
				chartVersion: "v25.3.3",
				appVersion:   "25.3.3",
				annotations:  map[string]string{"aicr.run/component-version": "1.4.2"},
			},
		},
		{
			name: "uncompressed release",
			data: base64.StdEncoding.EncodeToString([]byte(minimalRecord)),
			want: releaseFields{
				chartName:    "gpu-operator",
				chartVersion: "v25.3.3",
			},
		},
		{
			name: "annotations preserved",
			data: encodeReleaseFixture(t,
				`{"name":"gpu-operator","chart":{"metadata":{"name":"gpu-operator","version":"v25.3.3",`+
					`"annotations":{"aicr.run/component-version":"1.4.2"}}}}`),
			want: releaseFields{
				chartName:    "gpu-operator",
				chartVersion: "v25.3.3",
				annotations:  map[string]string{"aicr.run/component-version": "1.4.2"},
			},
		},
		{
			name:           "not base64",
			data:           "!!!",
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
		{
			name:           "gzip magic but truncated",
			data:           base64.StdEncoding.EncodeToString(gzipMagicOnly),
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
		{
			name:               "not json",
			data:               encodeReleaseFixture(t, `{"name": `+payloadMarker+`}`),
			wantErr:            true,
			wantErrContext:     map[string]any{"release": releaseName},
			wantErrNotContains: []string{payloadMarker},
		},
		{
			name:           "null record",
			data:           encodeReleaseFixture(t, `null`),
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
		{
			name:           "chart without metadata",
			data:           encodeReleaseFixture(t, `{"name":"gpu-operator","chart":{}}`),
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
		{
			name:           "chart metadata without version",
			data:           encodeReleaseFixture(t, `{"name":"gpu-operator","chart":{"metadata":{"name":"gpu-operator"}}}`),
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeRelease(releaseName, tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeRelease() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if got != nil {
					t.Errorf("decodeRelease() returned a partial result %+v alongside an error", got)
				}
				assertErrorCarriesOnly(t, err, tt.wantErrContext)
				assertErrorMessage(t, err, releaseName, tt.wantErrContains, tt.wantErrNotContains)

				return
			}
			assertFields(t, got, tt.want)
		})
	}
}

func TestGunzipBounded(t *testing.T) {
	const releaseName = "gpu-operator"
	const atLimit = "0123456789abcdef" // 16 bytes

	// A header gzip.NewReader accepts, over a body whose trailer is gone, so
	// the failure surfaces from the read rather than from the open.
	truncatedBody := gzipBytes(t, strings.Repeat("payload", 32))
	truncatedBody = truncatedBody[:len(truncatedBody)-8]

	tests := []struct {
		name           string
		raw            []byte
		limit          int64
		wantErr        bool
		wantErrContext map[string]any
		want           string
	}{
		{
			name:  "under limit",
			raw:   gzipBytes(t, atLimit),
			limit: 64,
			want:  atLimit,
		},
		{
			name:  "exactly at limit",
			raw:   gzipBytes(t, atLimit),
			limit: int64(len(atLimit)),
			want:  atLimit,
		},
		{
			name:    "one byte over limit",
			raw:     gzipBytes(t, atLimit+"!"),
			limit:   int64(len(atLimit)),
			wantErr: true,
			wantErrContext: map[string]any{
				"release": releaseName,
				"limit":   int64(len(atLimit)),
			},
		},
		{
			name:           "truncated body",
			raw:            truncatedBody,
			limit:          defaults.HelmReleaseDecodeLimit,
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
		{
			name:           "not gzip",
			raw:            []byte("plain json, no header"),
			limit:          defaults.HelmReleaseDecodeLimit,
			wantErr:        true,
			wantErrContext: map[string]any{"release": releaseName},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gunzipBounded(releaseName, tt.raw, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("gunzipBounded() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if got != nil {
					t.Errorf("gunzipBounded() returned %d bytes alongside an error", len(got))
				}
				assertErrorCarriesOnly(t, err, tt.wantErrContext)

				var wantInMessage []string
				if limit, ok := tt.wantErrContext["limit"]; ok {
					wantInMessage = append(wantInMessage, strconv.FormatInt(limit.(int64), 10))
				}
				assertErrorMessage(t, err, releaseName, wantInMessage, nil)

				return
			}
			if string(got) != tt.want {
				t.Errorf("gunzipBounded() = %q, want %q", got, tt.want)
			}
		})
	}
}

// assertFields pins every field of the decoded record, so a wrong or missing
// json tag fails here rather than reading as an empty value on a cluster.
func assertFields(t *testing.T, got *helmRelease, want releaseFields) {
	t.Helper()
	if got.Info.Status != want.status {
		t.Errorf("status = %q, want %q", got.Info.Status, want.status)
	}
	if got.Chart.Metadata.Name != want.chartName {
		t.Errorf("chart name = %q, want %q", got.Chart.Metadata.Name, want.chartName)
	}
	if got.Chart.Metadata.Version != want.chartVersion {
		t.Errorf("chart version = %q, want %q", got.Chart.Metadata.Version, want.chartVersion)
	}
	if got.Chart.Metadata.AppVersion != want.appVersion {
		t.Errorf("app version = %q, want %q", got.Chart.Metadata.AppVersion, want.appVersion)
	}
	if !maps.Equal(got.Chart.Metadata.Annotations, want.annotations) {
		t.Errorf("annotations = %#v, want %#v", got.Chart.Metadata.Annotations, want.annotations)
	}
}

// assertErrorCarriesOnly pins the structured error's context to exactly the
// keys and values the decoder is allowed to publish. Asserting the absence of
// payload bytes is unprovable; asserting the whole map is not, and a change
// that attached the failing record would fail here.
func assertErrorCarriesOnly(t *testing.T, err error, want map[string]any) {
	t.Helper()
	var structured *errors.StructuredError
	if !stderrors.As(err, &structured) {
		t.Fatalf("error %v is not a *errors.StructuredError", err)
	}
	if structured.Code != errors.ErrCodeInternal {
		t.Errorf("error code = %q, want %q", structured.Code, errors.ErrCodeInternal)
	}
	if !maps.Equal(structured.Context, want) {
		t.Errorf("error context = %#v, want exactly %#v", structured.Context, want)
	}
}

func assertErrorMessage(t *testing.T, err error, releaseName string, contains, notContains []string) {
	t.Helper()
	msg := err.Error()
	if !strings.Contains(msg, releaseName) {
		t.Errorf("error %q does not name release %q", msg, releaseName)
	}
	for _, want := range contains {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	for _, unwanted := range notContains {
		if strings.Contains(msg, unwanted) {
			t.Errorf("error %q echoes payload content %q", msg, unwanted)
		}
	}
}

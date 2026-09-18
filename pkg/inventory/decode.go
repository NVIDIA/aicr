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
	"encoding/json"
	"fmt"
	"io"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// helmRelease is the subset of Helm's release record this package reads.
//
// Deliberately a local struct rather than helm.sh/helm/v4/pkg/release/v1: the
// whole point of reading storage directly is to avoid that module. The encoded
// record also carries `manifest` and `hooks`, which are large and which this
// struct drops on the floor.
//
// It drops the release's own name, namespace and revision for a different
// reason: the storage labels carry all three and are what this package reads
// them from, so decoding them here would offer a second, unauthoritative
// source for an identity that has one.
type helmRelease struct {
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name        string            `json:"name"`
			Version     string            `json:"version"`
			AppVersion  string            `json:"appVersion"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	} `json:"chart"`
}

// magicGzip is the gzip header Helm's decoder tests for. Records written
// before Helm compressed them are stored as plain JSON, and Helm still reads
// those, so this decoder does too.
var magicGzip = []byte{0x1f, 0x8b, 0x08}

// decodeRelease turns a stored payload into the subset of the release record
// this package reads. name identifies the release in any error: the payload
// carries rendered manifests, so no slice of it is attached to a message or to
// the error context. A json.SyntaxError cause still names the single offending
// byte, which is worth the parse diagnostics it buys.
func decodeRelease(name, data string) (*helmRelease, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("failed to base64-decode the stored record for Helm release %q", name),
			err, releaseContext(name))
	}

	// Strictly greater, matching Helm: a 3-byte payload is not treated as gzip.
	if len(raw) > len(magicGzip) && bytes.Equal(raw[:len(magicGzip)], magicGzip) {
		raw, err = gunzipBounded(name, raw, defaults.HelmReleaseDecodeLimit)
		if err != nil {
			return nil, err
		}
	}

	var rel helmRelease
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("failed to parse the stored record for Helm release %q", name),
			err, releaseContext(name))
	}

	// Helm writes both on every record, so their absence means a corrupt or
	// truncated record rather than a chart that declined to name itself.
	// Returning it would answer the version question with "", which reads
	// downstream as a component pinned to nothing.
	if rel.Chart.Metadata.Name == "" || rel.Chart.Metadata.Version == "" {
		return nil, errors.NewWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("the stored record for Helm release %q carries no chart metadata", name),
			releaseContext(name))
	}

	return &rel, nil
}

// gunzipBounded inflates raw up to limit bytes and refuses anything larger,
// rather than truncating: a half-read record parses into silence, and a
// component missing from an upgrade comparison reads as new.
func gunzipBounded(name string, raw []byte, limit int64) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("failed to open the compressed record for Helm release %q", name),
			err, releaseContext(name))
	}
	defer func() { _ = r.Close() }()

	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("failed to decompress the stored record for Helm release %q", name),
			err, releaseContext(name))
	}
	if int64(len(out)) > limit {
		errCtx := releaseContext(name)
		errCtx["limit"] = limit

		return nil, errors.NewWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("the stored record for Helm release %q exceeds the %d byte decode limit",
				name, limit),
			errCtx)
	}

	return out, nil
}

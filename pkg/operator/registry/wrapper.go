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

package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Chart content a wrapper archive carries. AICR's install script and
// cluster-values.yaml are deployment inputs, not chart content: the plan
// applies cluster-values.yaml through the engine's values.
var (
	wrapperFiles = map[string]bool{"Chart.yaml": true, "values.yaml": true, "values.schema.json": true}
	wrapperDirs  = []string{"templates/", "crds/", "charts/"}
)

// wrapperEpoch is the fixed modification time of every archive entry.
var wrapperEpoch = time.Unix(0, 0).UTC()

// PackageWrapper packages the local chart AICR rendered for a release (pre,
// post, readiness and manifest-only releases) as a Helm chart archive. The
// archive is deterministic: sorted entries, fixed times, owners and modes,
// and a gzip header without name or time, so the same files always produce
// the same bytes and therefore the same digest. files are the
// release directory's files by relative path.
func PackageWrapper(files map[string][]byte) ([]byte, *ChartMeta, error) {
	chartYAML, ok := files["Chart.yaml"]
	if !ok {
		return nil, nil, invalid("wrapper chart has no Chart.yaml")
	}
	meta, err := parseChartYAML(chartYAML)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if wrapperContent(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, nil, operrors.Wrap(operrors.ErrCodeInternal, "create gzip writer", err)
	}
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := files[name]
		h := &tar.Header{
			Name:     meta.Name + "/" + name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(data)),
			ModTime:  wrapperEpoch,
			Format:   tar.FormatPAX,
		}
		if err = tw.WriteHeader(h); err != nil {
			return nil, nil, operrors.Wrap(operrors.ErrCodeInternal, "write wrapper archive header", err)
		}
		if _, err = tw.Write(data); err != nil {
			return nil, nil, operrors.Wrap(operrors.ErrCodeInternal, "write wrapper archive entry", err)
		}
	}
	if err = tw.Close(); err != nil {
		return nil, nil, operrors.Wrap(operrors.ErrCodeInternal, "close wrapper archive", err)
	}
	if err = gz.Close(); err != nil {
		return nil, nil, operrors.Wrap(operrors.ErrCodeInternal, "close wrapper archive", err)
	}
	if buf.Len() > defaults.MaxChartArchiveBytes {
		return nil, nil, invalid("wrapper chart %s is %d bytes, over %d", meta.Name, buf.Len(), defaults.MaxChartArchiveBytes)
	}
	return buf.Bytes(), meta, nil
}

// wrapperContent reports whether a release file belongs in the chart.
func wrapperContent(name string) bool {
	if wrapperFiles[name] {
		return true
	}
	clean := path.Clean(name)
	for _, dir := range wrapperDirs {
		if strings.HasPrefix(clean, dir) {
			return true
		}
	}
	return false
}

// PushWrapper packages a release's local chart and pushes it to target. The
// tag carries the archive's own digest, so different content never shares a
// tag and an existing tag is reused only when its content matches.
func (c *Client) PushWrapper(ctx context.Context, target Repository, files map[string][]byte) (*MirroredChart, error) {
	archive, meta, err := PackageWrapper(files)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(archive)
	tag := meta.Tag() + "-" + hex.EncodeToString(sum[:])[:16]
	digest, err := c.pushChart(ctx, target, archive, meta, tag)
	if err != nil {
		return nil, err
	}
	return &MirroredChart{Repository: target, Digest: digest, ArchiveDigest: "sha256:" + hex.EncodeToString(sum[:]), Archive: archive}, nil
}

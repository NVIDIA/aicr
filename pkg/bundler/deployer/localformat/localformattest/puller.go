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

// Package localformattest provides a localformat.ChartPuller for tests.
package localformattest

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// StubPuller is a localformat.ChartPuller that returns StubArchive(c) for every
// pull without fetching anything.
type StubPuller struct {
	// RecordSHA256, if set, is the VendorRecord's SHA256 in place of the
	// archive's, so a digest test fails when the caller trusts the record
	// over the pulled bytes.
	RecordSHA256 string
}

var _ localformat.ChartPuller = StubPuller{}

// StubArchive returns the bytes StubPuller returns for c.
func StubArchive(c localformat.Component) []byte {
	return []byte("fake-tgz-" + chartName(c) + "-" + c.Version)
}

// Pull returns StubArchive(c) and a VendorRecord carrying its SHA-256, or
// p.RecordSHA256 when set.
func (p StubPuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	tgz := StubArchive(c)
	sum := sha256.Sum256(tgz)
	tarball := chartName(c) + "-" + c.Version + ".tgz"
	return tgz, localformat.VendorRecord{
		Name:          c.Name,
		Chart:         chartName(c),
		Version:       c.Version,
		Repository:    c.Repository,
		SHA256:        cmp.Or(p.RecordSHA256, hex.EncodeToString(sum[:])),
		TarballName:   tarball,
		PullerVersion: "stub v0.0.0",
	}, tarball, nil
}

func chartName(c localformat.Component) string {
	return cmp.Or(c.ChartName, c.Name)
}

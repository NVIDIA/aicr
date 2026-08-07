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

package attestation

import (
	"context"
	stderrors "errors"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestHasBundleMarkers_StatFaultIsNotANonBundle covers the diagnostic defect
// from #2083: every stat error used to collapse to "not a bundle", so a
// soft-mounted NFS returning EIO, or a directory the operator cannot read,
// was reported as INVALID_REQUEST "does not look like a summary bundle" — a
// user-input diagnostic for a storage fault.
//
// The stat function is injected rather than provoked with chmod, because a
// chmod-based EACCES test silently no-ops when the suite runs as root, which
// is common in CI containers. An injected fault always exercises the branch.
func TestHasBundleMarkers_StatFaultIsNotANonBundle(t *testing.T) {
	tests := []struct {
		name       string
		statErr    error
		wantOK     bool
		wantErr    bool
		wantCode   errors.ErrorCode
		wantReason string
	}{
		{
			name:       "absent marker is genuinely not a bundle",
			statErr:    fs.ErrNotExist,
			wantOK:     false,
			wantErr:    false,
			wantReason: "a missing file is a real answer, not a fault",
		},
		{
			// ENOTDIR/ENAMETOOLONG mean the path cannot name a marker at all,
			// so they are answers, not faults. Dropping either name from the
			// guard would turn an operator typo (passing a pointer file where
			// a bundle directory belongs) into SERVICE_UNAVAILABLE instead of
			// the actionable "does not look like a summary bundle".
			name:       "a non-directory component is not a bundle",
			statErr:    syscall.ENOTDIR,
			wantOK:     false,
			wantErr:    false,
			wantReason: "ENOTDIR means the path cannot name a marker at all",
		},
		{
			name:       "an over-long path is not a bundle",
			statErr:    syscall.ENAMETOOLONG,
			wantOK:     false,
			wantErr:    false,
			wantReason: "ENAMETOOLONG means the path cannot name a marker at all",
		},
		{
			name:     "permission fault surfaces as a fault",
			statErr:  fs.ErrPermission,
			wantOK:   false,
			wantErr:  true,
			wantCode: errors.ErrCodeUnavailable,
		},
		{
			name:     "I/O fault surfaces as a fault",
			statErr:  syscall.EIO,
			wantOK:   false,
			wantErr:  true,
			wantCode: errors.ErrCodeUnavailable,
		},
		{
			name:     "stale NFS handle surfaces as a fault",
			statErr:  syscall.ESTALE,
			wantOK:   false,
			wantErr:  true,
			wantCode: errors.ErrCodeUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statFn := func(string) (os.FileInfo, error) { return nil, tt.statErr }

			ok, err := hasBundleMarkersWithStat(context.Background(), "/any/dir", statFn)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v (%s)", err, tt.wantErr, tt.wantReason)
			}
			if tt.wantErr && !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("expected code %s, got %v", tt.wantCode, err)
			}
		})
	}
}

// TestHasBundleMarkers_RealBundleStillDetected guards against the fail-closed
// change breaking ordinary detection, including the probe of a candidate
// directory that does not exist (which must stay a clean "not a bundle").
func TestHasBundleMarkers_RealBundleStillDetected(t *testing.T) {
	dir := emitUnsignedBundle(t)

	ok, err := HasBundleMarkers(context.Background(), dir+"/"+SummaryBundleDirName)
	if err != nil || !ok {
		t.Fatalf("HasBundleMarkers on a real bundle = (%v, %v), want (true, nil)", ok, err)
	}

	ok, err = HasBundleMarkers(context.Background(), dir+"/does-not-exist")
	if err != nil || ok {
		t.Fatalf("HasBundleMarkers on a missing dir = (%v, %v), want (false, nil)", ok, err)
	}
}

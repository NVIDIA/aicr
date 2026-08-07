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

package boundedio

import (
	stderrors "errors"
	"io"
	"os"
	"strconv"
	"syscall"

	"context"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ReadFile reads path into memory bounded three ways: by size (max bytes), by
// time (the Do boundary), and by shape (descriptor-first validation).
//
// The shape checks matter because a bundle root can be attacker-influenced —
// an extracted archive, a symlink-rich tarball, a path on a network mount.
// O_NONBLOCK keeps a FIFO substituted for a regular file from blocking the
// open, and validating the *opened descriptor* rather than the path closes the
// swap window between the check and the read.
//
// Symlink scope, precisely: O_NOFOLLOW rejects a symlink at the FINAL path
// component only. An intermediate directory component that is a symlink
// (bundle/ctrf -> /elsewhere) is still traversed, so this is not a containment
// boundary — callers needing one must resolve against an os.Root. Callers here
// rely on containment from elsewhere: manifest entries are rejected by
// filepath.IsLocal before use, and every byte read is hash-bound to a
// manifest that is itself digest-bound to the predicate, so content read
// through such a path cannot pass verification. Closing the traversal gap is
// tracked as follow-up work on #2083.
//
// label names the input in error messages (e.g. "in-toto Statement").
func ReadFile(ctx context.Context, path, label string, max int64) ([]byte, error) {
	var body []byte
	if err := Do(ctx, label, func() error {
		b, readErr := readBoundedWithOpener(path, label, max, os.OpenFile)
		if readErr != nil {
			return readErr
		}
		body = b
		return nil
	}); err != nil {
		return nil, err
	}
	return body, nil
}

// fileOpener matches os.OpenFile so tests can inject the syscall-level faults
// (EIO, EACCES, ESTALE) that a soft-mounted NFS returns. Provoking those with
// chmod would silently no-op when the suite runs as root, which is common in
// CI containers.
type fileOpener func(name string, flag int, perm os.FileMode) (*os.File, error)

func readBoundedWithOpener(path, label string, max int64, open fileOpener) ([]byte, error) {
	f, err := open( //nolint:gosec // caller-validated, bundle-local path
		path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		switch {
		case stderrors.Is(err, syscall.ELOOP):
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"refusing to read "+label+" through a symlink", err)
		case os.IsNotExist(err):
			return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read "+label, err)
		case IsStorageFault(err):
			// A mount that answers with EIO/ESTALE/EACCES has told us nothing
			// about the file. Reporting this as Internal would let callers
			// bucket it with real content failures — the verifier would render
			// a soft-mount fault as a digest mismatch, i.e. "tampered bundle".
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to read "+label, err)
		default:
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open "+label, err)
		}
	}
	defer func() { _ = f.Close() }() // read-only handle

	opened, err := f.Stat()
	if err != nil {
		return nil, statError(label, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, label+" is not a regular file")
	}
	if opened.Size() > max {
		return nil, oversize(label, max)
	}

	return readAllBounded(f, label, max)
}

// readAllBounded consumes r under the size cap and classifies a mid-transfer
// failure. Split from the open/stat work so it is reachable with a faulting
// io.Reader: a mount that answers the open and then faults mid-read is the
// precise soft-NFS case this package exists for, and it cannot be provoked
// through a real file.
func readAllBounded(r io.Reader, label string, max int64) ([]byte, error) {
	// The +1 detects "over the cap" without reading an oversized payload, and
	// also catches a file that grew between the stat above and this read.
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, readError(label, err)
	}
	if int64(len(body)) > max {
		return nil, oversize(label, max)
	}
	return body, nil
}

// readError classifies a mid-transfer failure. Mirrors statError: a storage
// errno here is environmental, and misreporting it as Internal would let the
// verifier fold it into the inventory mismatch rows and render a soft-mount
// fault as an exit-2 "tampered bundle".
func readError(label string, err error) error {
	if IsStorageFault(err) {
		return errors.Wrap(errors.ErrCodeUnavailable, "failed to read "+label, err)
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to read "+label, err)
}

// IsStorageFault reports whether err is the filesystem saying it could not
// answer, as opposed to answering "no such file". These are environmental and
// must stay distinguishable from a verdict about the bundle's contents:
// callers that stat outside this package (the verifier's signature probe)
// need the same split.
//
// EACCES/EPERM are included deliberately, accepting a known imprecision: a
// permanently-unreadable local file (mode 000) is labeled transient and
// invites a retry that will never succeed. The alternative — treating them as
// verdicts — would misreport the far more consequential case, a soft-mounted
// NFS returning EACCES while the server is unreachable, as "this bundle is
// invalid". Both directions fail closed (nothing is ever accepted), so the
// tradeoff is which useless remediation the operator is pointed at, and
// distinguishing a local-fs EACCES from a network-mount one is not portably
// worth it.
func IsStorageFault(err error) bool {
	return stderrors.Is(err, syscall.EIO) ||
		stderrors.Is(err, syscall.ESTALE) ||
		stderrors.Is(err, syscall.EACCES) ||
		stderrors.Is(err, syscall.EPERM) ||
		stderrors.Is(err, syscall.ENXIO) ||
		stderrors.Is(err, syscall.ETIMEDOUT)
}

// statError classifies a failure of the post-open descriptor check. A mount can
// fault after the open succeeds, and the descriptor check is no more
// authoritative about content than the open was, so a storage errno here is an
// environmental fault rather than a verdict. Split out so both branches are
// directly testable: a closed descriptor yields os.ErrClosed, which is NOT a
// storage fault, so driving this through a real file cannot reach the
// IsStorageFault arm.
func statError(label string, err error) error {
	if IsStorageFault(err) {
		return errors.Wrap(errors.ErrCodeUnavailable, "failed to inspect "+label, err)
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to inspect "+label, err)
}

func oversize(label string, max int64) error {
	return errors.New(errors.ErrCodeInvalidRequest,
		label+" exceeds maximum size of "+strconv.FormatInt(max, 10)+" bytes")
}

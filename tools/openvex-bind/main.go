// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Command openvex-bind projects the committed OpenVEX document onto one
// platform manifest digest, producing the document the release publishes as a
// `--type openvex` Cosign attestation on that manifest (NVIDIA/aicr#2426).
//
// Why a projection and not a committed file: verification requires the VEX
// product identifier to bind to the specific manifest the claim covers, and
// that digest does not exist until the image is built. `.openvex.json` stays
// the reviewed source of truth with bare `pkg:oci/<name>` products; this tool
// rewrites those to `pkg:oci/<name>@sha256:<platform-manifest-digest>` and
// writes the result to a file the attest step feeds to `cosign attest`. The
// source document is never modified.
//
// What it does not do: no format translation, no status or justification
// mapping, no merging of scan results. Statuses, justifications, impact
// statements and subcomponents pass through untouched, because the curated
// judgment is exactly what has value and any rewrite of it is a guess.
//
// Output is deterministic — a pure function of the source bytes, the image
// name and the digest, with no wall clock and no UUID — so a re-run of a
// release produces byte-identical evidence.
//
// Usage:
//
//	openvex-bind -in .openvex.json -out vex-linux-amd64.openvex.json \
//	  -image ghcr.io/nvidia/aicr-validators/aiperf-bench \
//	  -digest sha256:<64 hex>
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// options is the parsed command line.
type options struct {
	in     string
	out    string
	image  string
	digest string
}

func main() {
	var o options
	flag.StringVar(&o.in, "in", ".openvex.json", "source OpenVEX document to project")
	flag.StringVar(&o.out, "out", "", "path to write the digest-bound projection (required)")
	flag.StringVar(&o.image, "image", "", "image name without tag or digest, e.g. ghcr.io/nvidia/aicr (required)")
	flag.StringVar(&o.digest, "digest", "", "platform manifest digest to bind products to, sha256:<64 hex> (required)")
	flag.Parse()

	if err := run(o, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "openvex-bind: %v\n", err)
		os.Exit(1)
	}
}

// run reads the source document, binds it, and writes the projection. It
// reports the kept/dropped statement counts on stdout so a release log records
// which statements this platform actually received.
func run(o options, stdout io.Writer) error {
	if o.out == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "-out is required")
	}
	source, err := readSource(o.in)
	if err != nil {
		return err
	}
	result, err := Bind(source, Options{Image: o.image, Digest: o.digest})
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.out, result.Document, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to write %s", o.out), err)
	}
	fmt.Fprintf(stdout, "bound %d statement(s) to %s@%s (%d not for this image)\n",
		result.Kept, o.image, o.digest, result.Dropped)
	return nil
}

// readSource reads the OpenVEX document under a size cap. os.ReadFile would
// allocate the whole file before any check, so a path that resolves to /proc,
// a FUSE mount or an NFS share could exhaust memory.
func readSource(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "-in is required")
	}
	file, err := os.Open(path) // #nosec G304 -- release-controlled path, size-bounded below
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to open %s", path), err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, defaults.MaxOpenVEXBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read %s", path), err)
	}
	if int64(len(data)) > defaults.MaxOpenVEXBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s exceeds the %d byte OpenVEX size limit", path, defaults.MaxOpenVEXBytes))
	}
	if len(data) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("%s is empty", path))
	}
	return data, nil
}

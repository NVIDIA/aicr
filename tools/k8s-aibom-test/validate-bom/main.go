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

// Command validate-bom checks that a k8s-aibom-emitted document is a
// well-formed CycloneDX 1.6 ML-BOM.
//
// It decodes through github.com/CycloneDX/cyclonedx-go — already an AICR
// dependency, used by pkg/bom and pkg/evidence/attestation to build AICR's own
// BOMs — rather than validating against the published JSON Schema. That is a
// deliberate trade: schema validation is strictly stronger, but the only Go
// implementation would be a new production dependency carried solely for a
// local qualification command. The one-time full JSON-Schema verdict for the
// qualified v1.2.0 artifact set is recorded in the ADR-019 implementation PR;
// this command is the standing regression check.
//
// What it therefore proves: the document parses as CycloneDX, declares
// bomFormat CycloneDX and specVersion 1.6, and carries the structural fields a
// consumer needs. What it does not prove: conformance to every schema
// constraint (pattern, enum, and cross-field rules).
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	cyclonedx "github.com/CycloneDX/cyclonedx-go"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// maxInputBytes bounds the document read. k8s-aibom stores BOMs inline only
// below its configured inlineThresholdBytes (256 KiB by default), so 2 MiB is
// generous headroom while still refusing to allocate an unbounded file.
const maxInputBytes = 2 << 20

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) != 1 {
		return errors.New(errors.ErrCodeInvalidRequest, "usage: validate-bom <bom.json>")
	}

	raw, err := readBounded(args[0])
	if err != nil {
		return err
	}

	var bom cyclonedx.BOM
	if err := cyclonedx.NewBOMDecoder(bytes.NewReader(raw), cyclonedx.BOMFileFormatJSON).Decode(&bom); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "decode AIBOM as CycloneDX JSON", err)
	}

	if bom.BOMFormat != cyclonedx.BOMFormat {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("bomFormat = %q, want %q", bom.BOMFormat, cyclonedx.BOMFormat))
	}
	if bom.SpecVersion != cyclonedx.SpecVersion1_6 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("specVersion = %q, want %q",
				bom.SpecVersion.String(), cyclonedx.SpecVersion1_6.String()))
	}
	// A document that parses but describes nothing would satisfy every check
	// above. ADR-019 admits the component to inventory AI workloads, so an
	// empty component list is a failed observation, not a valid empty BOM.
	if bom.Components == nil || len(*bom.Components) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"BOM declares no components; an inventory that inventories nothing is not a pass")
	}
	if bom.Metadata == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "BOM has no metadata section")
	}

	if _, err := fmt.Fprintf(output, "CycloneDX %s validation passed (%d components)\n",
		bom.SpecVersion.String(), len(*bom.Components)); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write validation result", err)
	}
	return nil
}

// readBounded reads at most maxInputBytes from path, refusing anything larger
// rather than allocating it. os.ReadFile would allocate the whole file before
// any size check could run.
func readBounded(path string) ([]byte, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "open AIBOM document", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read AIBOM document", err)
	}
	if len(data) > maxInputBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "AIBOM document exceeds 2 MiB")
	}
	return data, nil
}

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

package verifier

import (
	"os"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// DetectInputForm classifies a user-supplied input string into one of
// the three supported transport forms. Detection precedence:
//
//  1. URL prefix: oci:// → OCI; http(s):// is rejected.
//  2. Filesystem: directory → dir; .yaml/.yml file → pointer.
//  3. Bare OCI ref shape ("registry/repo[:tag][@digest]") → OCI.
func DetectInputForm(input string) (InputForm, error) {
	if input == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "input is empty")
	}
	if strings.HasPrefix(input, "oci://") {
		return InputFormOCI, nil
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"http(s):// inputs are not supported — use oci://, a pointer file, or a local directory")
	}
	if info, err := os.Stat(input); err == nil {
		if info.IsDir() {
			return InputFormDir, nil
		}
		if strings.HasSuffix(input, ".yaml") || strings.HasSuffix(input, ".yml") {
			return InputFormPointer, nil
		}
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"input "+input+" is a file with an unrecognized extension (expected .yaml/.yml pointer)")
	}
	if looksLikeOCIRef(input) {
		return InputFormOCI, nil
	}
	return "", errors.New(errors.ErrCodeInvalidRequest,
		"input "+input+" is not a recognizable pointer / OCI ref / directory")
}

// looksLikeOCIRef is a cheap shape check for a bare reference. The
// full parse happens in parseOCIReference when materialization runs.
func looksLikeOCIRef(s string) bool {
	first, _, ok := strings.Cut(s, "/")
	if !ok {
		return false
	}
	// Relative-path tokens contain dots but aren't registries.
	if first == "." || first == ".." {
		return false
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return false
	}
	if strings.ContainsAny(s, " \t") {
		return false
	}
	return true
}

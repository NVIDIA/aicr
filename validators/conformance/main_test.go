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

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

// Every catalog entry that runs the conformance image must name a check the
// binary registers; otherwise its Job exits "unknown check" on a live cluster,
// which no CI lane reaches for Slurm-only checks.
func TestConformanceChecksMatchCatalog(t *testing.T) {
	cat, err := catalog.LoadWithDataProvider(context.Background(), nil, "", "")
	if err != nil {
		t.Fatalf("load embedded catalog: %v", err)
	}
	fromCatalog := map[string]bool{}
	for _, entry := range cat.Validators {
		if !strings.Contains(entry.Image, "/aicr-validators/conformance") {
			continue
		}
		if len(entry.Args) == 0 {
			t.Errorf("catalog entry %q runs the conformance image with no check argument", entry.Name)
			continue
		}
		fromCatalog[entry.Args[0]] = true
	}
	if len(fromCatalog) == 0 {
		t.Fatal("no catalog entry runs the conformance image; the image match is wrong")
	}
	registered := conformanceChecks()
	for name := range fromCatalog {
		if _, ok := registered[name]; !ok {
			t.Errorf("catalog check %q is not registered in conformanceChecks()", name)
		}
	}
	for name := range registered {
		if !fromCatalog[name] {
			t.Errorf("conformanceChecks() registers %q but no catalog entry runs it", name)
		}
	}
}

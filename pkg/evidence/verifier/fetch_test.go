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
	"context"
	"testing"
)

func TestMaterializeBundle_DirAcceptsParentOrSummary(t *testing.T) {
	bundleDir := buildTestBundle(t)

	mat, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: bundleDir}, InputFormDir)
	if err != nil {
		t.Fatalf("MaterializeBundle(parent): %v", err)
	}
	mat.Cleanup()

	mat2, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: summaryDirOf(t, bundleDir)}, InputFormDir)
	if err != nil {
		t.Fatalf("MaterializeBundle(summary): %v", err)
	}
	mat2.Cleanup()
}

func TestMaterializeBundle_DirRejectsNonBundle(t *testing.T) {
	_, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: t.TempDir()}, InputFormDir)
	if err == nil {
		t.Errorf("expected error for empty directory")
	}
}

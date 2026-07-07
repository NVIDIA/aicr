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

package docgen_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/tools/internal/docgen"
)

func TestWriteRendered(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.md")
	err := docgen.WriteRendered(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, func(w io.Writer) error {
		_, e := fmt.Fprint(w, "hello")
		return e
	})
	if err != nil {
		t.Fatalf("WriteRendered: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestWriteRendered_RenderErrorPropagates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.md")
	want := fmt.Errorf("boom")
	err := docgen.WriteRendered(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, func(io.Writer) error {
		return want
	})
	if err == nil {
		t.Fatal("expected render error to propagate")
	}
}

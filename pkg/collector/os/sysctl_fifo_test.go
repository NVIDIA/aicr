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

//go:build unix

package os

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

// A blocking sysctl must be skipped before it is opened, not filtered after:
// its read(2) can block where ctx cannot reach it (#3247). Each one is planted
// as a FIFO, whose open(2) blocks until a writer appears, so a collector that
// opens it hangs instead of passing.
func TestSysctlCollector_SkipsBlockingSysctlsWithoutOpening(t *testing.T) {
	tmpRoot := t.TempDir()

	// The two entries observed wedging in #3247 are named explicitly so they
	// cannot silently drop out of blockingSysctls.
	blocking := []string{
		"vm/stat_refresh",
		"kernel/ftrace_enabled",
	}
	for rel := range blockingSysctls {
		if !slices.Contains(blocking, rel) {
			blocking = append(blocking, rel)
		}
	}
	for _, rel := range blocking {
		fullPath := filepath.Join(tmpRoot, rel)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		if err := syscall.Mkfifo(fullPath, 0600); err != nil {
			t.Fatalf("Failed to create FIFO %s: %v", fullPath, err)
		}
	}
	readable := map[string]string{
		"kernel/osrelease": "6.17.0-1029-nvidia-64k",
		"vm/swappiness":    "60",
	}
	for rel, content := range readable {
		fullPath := filepath.Join(tmpRoot, rel)
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write file %s: %v", fullPath, err)
		}
	}

	originalRoot := sysctlRoot
	originalNetPrefix := sysctlNetPrefix
	sysctlRoot = tmpRoot
	sysctlNetPrefix = filepath.Join(tmpRoot, "net")
	// Registered before the release cleanup below so it runs after it: the
	// collector goroutine reads these globals until it exits.
	t.Cleanup(func() {
		sysctlRoot = originalRoot
		sysctlNetPrefix = originalNetPrefix
	})

	ctx, cancel := context.WithCancel(context.Background())
	var subtype *measurement.Subtype
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		subtype, err = (&Collector{}).collectSysctl(ctx)
	}()
	// On failure the goroutine is parked in open(2) on a FIFO. Opening it
	// read-write never blocks and completes that open; the close then ends its
	// read with EOF. The walk can reach one more FIFO before it observes the
	// cancellation, so keep releasing until the goroutine exits.
	t.Cleanup(func() {
		cancel()
		for {
			for _, rel := range blocking {
				if f, openErr := os.OpenFile(filepath.Join(tmpRoot, rel), os.O_RDWR, 0); openErr == nil {
					_ = f.Close()
				}
			}
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("collectSysctl did not return: it opened one of %v", blocking)
	}
	if err != nil {
		t.Fatalf("collectSysctl() failed: %v", err)
	}

	for _, rel := range blocking {
		if _, exists := subtype.Data[filepath.Join(tmpRoot, rel)]; exists {
			t.Errorf("blocking sysctl %q should not be in results", rel)
		}
	}
	for rel, want := range readable {
		got, exists := subtype.Data[filepath.Join(tmpRoot, rel)]
		if !exists {
			t.Errorf("readable sysctl %q missing from results", rel)
			continue
		}
		if got.String() != want {
			t.Errorf("sysctl %q = %q, want %q", rel, got.String(), want)
		}
	}
}

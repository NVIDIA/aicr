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
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Every test here plants FIFOs, whose open(2) blocks until a writer appears,
// to stand in for a procfs read the kernel never returns from.

// stalledReadTimeout is the per-read deadline the stalled-read tests install.
// It also bounds the sweep's ordinary reads, so it must not fail a plain file
// read under a loaded -race run, and every stall waits it out, so it must stay
// short.
const stalledReadTimeout = 500 * time.Millisecond

// collectorReturnTimeout bounds how long a test waits for the collector to
// return.
const collectorReturnTimeout = 5 * time.Second

func plantFIFOs(t *testing.T, root string, rels ...string) []string {
	t.Helper()
	paths := make([]string, 0, len(rels))
	for _, rel := range rels {
		fullPath := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		if err := syscall.Mkfifo(fullPath, 0600); err != nil {
			t.Fatalf("Failed to create FIFO %s: %v", fullPath, err)
		}
		paths = append(paths, fullPath)
	}
	return paths
}

func plantFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		fullPath := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write file %s: %v", fullPath, err)
		}
	}
}

// releaseFIFOs unblocks any reader parked on paths.
func releaseFIFOs(paths []string) {
	for _, p := range paths {
		// O_RDWR never blocks and counts as the writer, so the close gives the
		// reader EOF.
		if f, err := os.OpenFile(p, os.O_RDWR, 0); err == nil {
			_ = f.Close()
		}
	}
}

func withSysctlRoot(t *testing.T, root string) {
	t.Helper()
	originalRoot, originalNetPrefix := sysctlRoot, sysctlNetPrefix
	sysctlRoot = root
	sysctlNetPrefix = filepath.Join(root, "net")
	t.Cleanup(func() {
		sysctlRoot, sysctlNetPrefix = originalRoot, originalNetPrefix
	})
}

func withHostReadTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := hostReadTimeout
	hostReadTimeout = d
	t.Cleanup(func() { hostReadTimeout = original })
}

// stallWarning is the start of the message readHostFile logs for a stalled read.
const stallWarning = "host read did not return"

// captureWarnings returns a buffer that receives the default logger's warnings
// until the test ends.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// runSysctl returns collectSysctl's result, failing the test with what if it
// does not return within collectorReturnTimeout.
func runSysctl(t *testing.T, ctx context.Context, what string) (*measurement.Subtype, error) {
	t.Helper()
	var subtype *measurement.Subtype
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		subtype, err = (&Collector{}).collectSysctl(ctx)
	}()
	select {
	case <-done:
	case <-time.After(collectorReturnTimeout):
		t.Fatalf("collectSysctl did not return: %s", what)
	}
	return subtype, err
}

// TestSysctlCollector_SkipsBlockingSysctlsWithoutOpening checks that a
// blockingSysctls entry is never opened. The per-read deadline is raised out of
// reach, so a collector that opens one hangs instead of passing.
func TestSysctlCollector_SkipsBlockingSysctlsWithoutOpening(t *testing.T) {
	tmpRoot := t.TempDir()

	// Entries whose read does real work are named explicitly so they cannot
	// silently drop out of blockingSysctls.
	blocking := []string{
		"vm/stat_refresh",
		"kernel/ftrace_enabled",
		"kernel/sched_rt_period_us",
		"kernel/sched_rt_runtime_us",
		"debug/kprobes-optimization",
		"kernel/shm_rmid_forced",
	}
	for _, rel := range blocking {
		if _, listed := blockingSysctls[rel]; !listed {
			t.Errorf("%q must stay in blockingSysctls", rel)
		}
	}
	for rel := range blockingSysctls {
		if !slices.Contains(blocking, rel) {
			blocking = append(blocking, rel)
		}
	}
	fifos := plantFIFOs(t, tmpRoot, blocking...)
	readable := map[string]string{
		"kernel/osrelease": "6.17.0-1029-nvidia-64k",
		"vm/swappiness":    "60",
	}
	plantFiles(t, tmpRoot, readable)

	// Registered before the release cleanup below so they run after it: the
	// collector goroutine reads these globals until it exits.
	withSysctlRoot(t, tmpRoot)
	withHostReadTimeout(t, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	// On failure the collector waits on a read parked on a FIFO. Canceling
	// ends that wait, and releasing the FIFOs ends the parked read.
	t.Cleanup(func() {
		cancel()
		releaseFIFOs(fifos)
	})

	subtype, err := runSysctl(t, ctx, "it opened a blocking sysctl")
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

// TestSysctlCollector_SkipsStalledRead checks that a read outside
// blockingSysctls is abandoned at the per-read deadline, the sweep continues,
// and the warning names the path.
func TestSysctlCollector_SkipsStalledRead(t *testing.T) {
	tmpRoot := t.TempDir()
	fifos := plantFIFOs(t, tmpRoot, "kernel/wedged")
	readable := map[string]string{
		"kernel/osrelease": "6.17.0-1029-nvidia-64k",
		"kernel/watchdog":  "1",
		"vm/numa_stat":     "1",
	}
	plantFiles(t, tmpRoot, readable)

	withSysctlRoot(t, tmpRoot)
	withHostReadTimeout(t, stalledReadTimeout)
	logs := captureWarnings(t)
	t.Cleanup(func() { releaseFIFOs(fifos) })

	subtype, err := runSysctl(t, context.Background(), "a stalled read was not abandoned")
	if err != nil {
		t.Fatalf("collectSysctl() failed: %v", err)
	}

	if _, exists := subtype.Data[fifos[0]]; exists {
		t.Errorf("stalled sysctl %q should not be in results", fifos[0])
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
	if got := strings.Count(logs.String(), stallWarning); got != len(fifos) {
		t.Errorf("stall warnings = %d, want %d; log:\n%s", got, len(fifos), logs)
	}
	for _, p := range fifos {
		if !strings.Contains(logs.String(), p) {
			t.Errorf("no warning names %s; log:\n%s", p, logs)
		}
	}
}

// TestSysctlCollector_AbandonsSubtypeAfterMaxStalls checks that the sweep
// stops after defaults.MaxStalledHostReads stalls, without opening more.
func TestSysctlCollector_AbandonsSubtypeAfterMaxStalls(t *testing.T) {
	tmpRoot := t.TempDir()
	rels := make([]string, 0, defaults.MaxStalledHostReads+2)
	for i := range defaults.MaxStalledHostReads + 2 {
		rels = append(rels, filepath.Join("kernel", fmt.Sprintf("wedged%d", i)))
	}
	fifos := plantFIFOs(t, tmpRoot, rels...)

	withSysctlRoot(t, tmpRoot)
	withHostReadTimeout(t, stalledReadTimeout)
	logs := captureWarnings(t)
	t.Cleanup(func() { releaseFIFOs(fifos) })

	_, err := runSysctl(t, context.Background(), "the sweep did not give up")
	if err == nil {
		t.Fatal("collectSysctl() succeeded, want an error once the stall cap is reached")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
	if errors.IsTransient(err) {
		t.Errorf("stall-cap error must not be transient, or the OS collector drops every subtype: %v", err)
	}
	if got := strings.Count(logs.String(), stallWarning); got != defaults.MaxStalledHostReads {
		t.Errorf("stalled reads = %d, want %d", got, defaults.MaxStalledHostReads)
	}
	for _, p := range fifos[:defaults.MaxStalledHostReads] {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("stall-cap error does not name %s: %v", p, err)
		}
	}
}

// TestCollector_StalledSingleFileReadSkipsOnlyItsSubtype checks that a stalled
// grub or kmod read drops only that subtype from the OS measurement.
func TestCollector_StalledSingleFileReadSkipsOnlyItsSubtype(t *testing.T) {
	tests := []struct {
		name    string
		path    *string
		subtype string
	}{
		{"grub", &filePathGrub, "grub"},
		{"kmod", &filePathKMod, "kmod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stallSingleFileRead(t, tt.path)
			withHostReadTimeout(t, stalledReadTimeout)

			m, err := (&Collector{}).Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect() failed: %v", err)
			}
			names := make([]string, 0, len(m.Subtypes))
			for _, st := range m.Subtypes {
				names = append(names, st.Name)
			}
			if slices.Contains(names, tt.subtype) {
				t.Errorf("subtypes = %v, want %q skipped", names, tt.subtype)
			}
			if !slices.Contains(names, "sysctl") {
				t.Errorf("subtypes = %v, want sysctl present", names)
			}
		})
	}
}

// TestCollector_ContextEndsDuringSingleFileReadFails checks that ctx ending
// while a grub or kmod read is parked fails the OS measurement from that
// subtype, without first logging it as skipped.
func TestCollector_ContextEndsDuringSingleFileReadFails(t *testing.T) {
	tests := []struct {
		name string
		path *string
	}{
		{"grub", &filePathGrub},
		{"kmod", &filePathKMod},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stallSingleFileRead(t, tt.path)
			withHostReadTimeout(t, time.Hour)
			logs := captureWarnings(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(stalledReadTimeout, cancel)

			m, err := (&Collector{}).Collect(ctx)
			if err == nil {
				t.Fatalf("Collect() = %v, want an error once ctx is canceled", m)
			}
			if !stderrors.Is(err, context.Canceled) {
				t.Errorf("Collect() error = %v, want it to wrap context.Canceled", err)
			}
			if skipped := "failed to collect " + tt.name; strings.Contains(logs.String(), skipped) {
				t.Errorf("logs contain %q, want the cancel reported only as an error:\n%s", skipped, logs)
			}
		})
	}
}

// stallSingleFileRead points the grub and kmod paths at plain files under a
// temporary root, then points path at a FIFO whose read never returns.
func stallSingleFileRead(t *testing.T, path *string) {
	t.Helper()
	tmpRoot := t.TempDir()
	fifos := plantFIFOs(t, tmpRoot, "stalled")
	plantFiles(t, tmpRoot, map[string]string{
		"cmdline":           "BOOT_IMAGE=/vmlinuz quiet",
		"modules":           "nvidia 1 0 - Live 0x0",
		"sys/vm/swappiness": "60",
	})

	originalGrub, originalKMod := filePathGrub, filePathKMod
	filePathGrub = filepath.Join(tmpRoot, "cmdline")
	filePathKMod = filepath.Join(tmpRoot, "modules")
	*path = fifos[0]
	t.Cleanup(func() { filePathGrub, filePathKMod = originalGrub, originalKMod })
	withSysctlRoot(t, filepath.Join(tmpRoot, "sys"))
	t.Cleanup(func() { releaseFIFOs(fifos) })
}

// TestReadHostFile_ContextEndsWhileStalled checks that ctx ending during a
// parked read, by deadline or cancel, returns a transient error, not a
// skipped-read stall.
func TestReadHostFile_ContextEndsWhileStalled(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), stalledReadTimeout)
		}, context.DeadlineExceeded},
		{"cancel", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(stalledReadTimeout, cancel)
			return ctx, cancel
		}, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fifos := plantFIFOs(t, t.TempDir(), "stalled")
			withHostReadTimeout(t, time.Hour)
			t.Cleanup(func() { releaseFIFOs(fifos) })

			ctx, cancel := tt.ctx()
			defer cancel()

			_, err := readHostFile(ctx, fifos[0], os.ReadFile)
			if !errors.IsTransient(err) {
				t.Fatalf("readHostFile() error = %v, want a transient timeout", err)
			}
			if isHostReadStalled(err) {
				t.Errorf("an ended context must not be reported as a stalled read: %v", err)
			}
			if !stderrors.Is(err, tt.want) {
				t.Errorf("readHostFile() error = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

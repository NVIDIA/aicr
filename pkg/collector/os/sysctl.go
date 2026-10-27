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

package os

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/collector/file"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

var (
	// Keys to filter out from sysctl properties for privacy/security or noise reduction
	filterOutSysctlKeys = []string{
		"/proc/sys/dev/cdrom/*",
	}

	sysctlRoot      = "/proc/sys"
	sysctlNetPrefix = "/proc/sys/net"
)

// blockingSysctls are /proc/sys entries, relative to sysctlRoot, that are
// skipped without being opened. Audited against Linux 5.15, 6.1, and 6.17
// outside net/.
//
// An entry is listed when its read either:
//   - does real work, such as waiting on every CPU, rebuilding scheduler
//     domains, re-optimizing kprobes, or destroying shm segments
//   - takes a sleeping lock whose holder can wait on other CPUs, CPU hotplug,
//     code patching, or device I/O, for a value nobody tunes
//
// Every other read is bounded by readHostFile.
var blockingSysctls = map[string]struct{}{
	"abi/cp15_barrier":                   {}, // insn_emulation_mutex (arm64, 6.1+)
	"abi/setend":                         {}, // insn_emulation_mutex (arm64, 6.1+)
	"abi/swp":                            {}, // insn_emulation_mutex (arm64, 6.1+)
	"debug/kprobes-optimization":         {}, // kprobe_sysctl_mutex; re-optimizes every kprobe
	"dev/cdrom/info":                     {}, // cdrom_mutex
	"dev/mac_hid/mouse_button_emulation": {}, // mac_hid_emumouse_mutex
	"kernel/ftrace_enabled":              {}, // ftrace_lock
	"kernel/latencytop":                  {}, // enables schedstats when set
	"kernel/sched_itmt_enabled":          {}, // itmt_update_mutex (x86, before 6.14)
	"kernel/sched_rt_period_us":          {}, // sched_domains_mutex; rebuilds sched domains
	"kernel/sched_rt_runtime_us":         {}, // sched_domains_mutex; rebuilds sched domains
	"kernel/shm_rmid_forced":             {}, // shm ids rwsem; destroys orphaned segments when set
	"kernel/stack_tracer_enabled":        {}, // stack_sysctl_mutex
	"kernel/tracepoint_printk":           {}, // tracepoint_printk_mutex
	"kernel/user_events_max":             {}, // event_mutex
	"vm/mem_profiling":                   {}, // static_key_mutex
	"vm/percpu_pagelist_high_fraction":   {}, // pcp_batch_high_lock
	"vm/stat_refresh":                    {}, // schedule_on_each_cpu
}

// collectSysctl gathers sysctl configurations from /proc/sys, excluding /proc/sys/net
// and blockingSysctls, and returns them as a subtype with file paths as keys and their
// contents as values.
//
// A stalled read is skipped. After defaults.MaxStalledHostReads stalls,
// collectSysctl returns an ErrCodeUnavailable error.
func (c *Collector) collectSysctl(ctx context.Context) (*measurement.Subtype, error) {
	params := make(map[string]measurement.Reading)

	// Create parser for reading file contents
	parser := file.NewParser()

	var stalled []string

	err := filepath.WalkDir(sysctlRoot, func(path string, d fs.DirEntry, err error) error {
		// Check if context is canceled — fail loud so a timed-out walk is never
		// reported as a (partial) success.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Wrap(errors.ErrCodeTimeout, "sysctl collection cancelled", ctxErr)
		}

		// A per-entry walk error (e.g. a restricted directory under /proc/sys)
		// must skip only that entry, not abort the whole subtype. Aborting here
		// would zero out all sysctl data because one path was unreadable.
		if err != nil {
			slog.Debug("skipping unreadable sysctl path",
				slog.String("path", path), slog.String("error", err.Error()))
			return nil
		}

		// Skip symlinks to prevent directory traversal attacks
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Ensure path is under root (defense in depth)
		if !strings.HasPrefix(path, sysctlRoot) {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("path traversal detected: %s", path))
		}

		// Exclude network parameters
		if strings.HasPrefix(path, sysctlNetPrefix) {
			return nil
		}

		if _, blocking := blockingSysctls[strings.TrimPrefix(path, sysctlRoot+"/")]; blocking {
			return nil
		}

		lines, err := readHostFile(ctx, path, parser.GetLines)
		if err != nil {
			if errors.IsTransient(err) {
				return err
			}
			if isHostReadStalled(err) {
				stalled = append(stalled, path)
				if len(stalled) >= defaults.MaxStalledHostReads {
					return errors.NewWithContext(errors.ErrCodeUnavailable,
						fmt.Sprintf("abandoning sysctl collection after %d reads did not return: %s",
							len(stalled), strings.Join(stalled, ", ")),
						map[string]any{"paths": stalled})
				}
			}
			// Skip files we can't read (some proc files are write-only or restricted)
			return nil
		}

		// Handle multi-line files with space-separated key-value pairs
		if len(lines) > 1 {
			allParsed := c.parseMultiLineKeyValue(path, lines, params)
			if allParsed {
				// All lines were successfully parsed as key-value pairs
				return nil
			}
		}

		// Store single-line or non-key-value content as-is
		// Join lines back if it's multi-line but not key-value format
		content := strings.Join(lines, "\n")
		params[path] = measurement.Str(content)

		return nil
	})
	if err != nil {
		// Preserve a structured code from the callback (e.g. ErrCodeTimeout on
		// cancellation) instead of flattening every walk error to Internal.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to collect sysctl parameters")
	}

	res := &measurement.Subtype{
		Name: "sysctl",
		Data: measurement.FilterOut(params, filterOutSysctlKeys),
	}

	return res, nil
}

// parseMultiLineKeyValue attempts to parse lines as space-separated key-value pairs.
// Returns true if all non-empty lines were successfully parsed as key-value pairs.
func (c *Collector) parseMultiLineKeyValue(path string, lines []string, params map[string]measurement.Reading) bool {
	tmp := make(map[string]measurement.Reading)

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Check if line has space-separated key and value
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			// Create new entry with extended path: /proc/sys/path/key
			key := parts[0]
			value := strings.Join(parts[1:], " ")
			extendedPath := path + "/" + key
			tmp[extendedPath] = measurement.Str(value)
		} else {
			// Not a key-value pair format
			return false
		}
	}

	maps.Copy(params, tmp)

	return true
}

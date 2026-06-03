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

package cli

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func typedOverrideFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{Name: "set-json"},
		&cli.StringSliceFlag{Name: "set-file"},
	}
}

func TestResolveTypedOverrides_SetJSON(t *testing.T) {
	runWith(t, typedOverrideFlags(),
		[]string{"--set-json", `agentgateway:allowedSourceRanges=["216.228.127.128/30"]`},
		func(c *cli.Command) {
			got, err := resolveTypedOverrides(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			if got[0].Component != "agentgateway" || got[0].Path != "allowedSourceRanges" {
				t.Errorf("got %q/%q", got[0].Component, got[0].Path)
			}
			if !reflect.DeepEqual(got[0].Value, []any{"216.228.127.128/30"}) {
				t.Errorf("value = %#v", got[0].Value)
			}
		})
}

// TestResolveTypedOverrides_SetJSONWithCommas verifies that a multi-element
// JSON list (comma-heavy) survives flag parsing when the command disables the
// slice-flag separator, as the real bundle command does. Without
// DisableSliceFlagSeparator the value would be split on commas and fail to
// parse — this is the regression guard for that wiring.
func TestResolveTypedOverrides_SetJSONWithCommas(t *testing.T) {
	cmd := &cli.Command{
		Name:                      "t",
		DisableSliceFlagSeparator: true,
		Flags:                     typedOverrideFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			got, err := resolveTypedOverrides(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			want := []any{"216.228.127.128/30", "10.0.0.0/8"}
			if !reflect.DeepEqual(got[0].Value, want) {
				t.Errorf("value = %#v, want %#v", got[0].Value, want)
			}
			return nil
		},
	}
	args := []string{"t", "--set-json", `agentgateway:allowedSourceRanges=["216.228.127.128/30","10.0.0.0/8"]`}
	if err := cmd.Run(context.Background(), args); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestResolveTypedOverrides_NeitherFlagReturnsNil(t *testing.T) {
	runWith(t, typedOverrideFlags(), []string{}, func(c *cli.Command) {
		got, err := resolveTypedOverrides(c)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %#v, want nil", got)
		}
	})
}

func TestResolveTypedOverrides_InvalidJSON(t *testing.T) {
	runWith(t, typedOverrideFlags(), []string{"--set-json", "comp:field=[bad"}, func(c *cli.Command) {
		_, err := resolveTypedOverrides(c)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
		if !strings.Contains(err.Error(), "--set-json") {
			t.Errorf("error %q must mention --set-json", err.Error())
		}
	})
}

func TestResolveTypedOverrides_SetFile(t *testing.T) {
	dir := t.TempDir()
	valFile := filepath.Join(dir, "ranges.yaml")
	if err := os.WriteFile(valFile, []byte("- 10.0.0.0/8\n- 192.168.0.0/16\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runWith(t, typedOverrideFlags(),
		[]string{"--set-file", "agentgateway:allowedSourceRanges=" + valFile},
		func(c *cli.Command) {
			got, err := resolveTypedOverrides(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			want := []any{"10.0.0.0/8", "192.168.0.0/16"}
			if !reflect.DeepEqual(got[0].Value, want) {
				t.Errorf("value = %#v, want %#v", got[0].Value, want)
			}
		})
}

func TestResolveTypedOverrides_BothFlagsCombine(t *testing.T) {
	dir := t.TempDir()
	valFile := filepath.Join(dir, "v.json")
	if err := os.WriteFile(valFile, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runWith(t, typedOverrideFlags(),
		[]string{
			"--set-json", `a:list=[1]`,
			"--set-file", "b:obj=" + valFile,
		},
		func(c *cli.Command) {
			got, err := resolveTypedOverrides(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("got %d entries, want 2 (set-json + set-file)", len(got))
			}
		})
}

// TestResolveTypedOverrides_SetJSONWinsOverSetFile locks in the precedence
// contract between the two typed flags: when --set-json and --set-file target
// the same component:path, the inline --set-json must win (mirroring Helm's
// --set over -f). resolveTypedOverrides collects --set-file first so the
// --set-json entry is applied last by WithValueOverridesTypedPaths. The
// command-line order is deliberately --set-json before --set-file to prove the
// outcome is governed by the fixed precedence, not raw argument order.
func TestResolveTypedOverrides_SetJSONWinsOverSetFile(t *testing.T) {
	dir := t.TempDir()
	valFile := filepath.Join(dir, "ranges.yaml")
	if err := os.WriteFile(valFile, []byte("- from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWith(t, typedOverrideFlags(),
		[]string{
			"--set-json", `agentgateway:allowedSourceRanges=["from-json"]`,
			"--set-file", "agentgateway:allowedSourceRanges=" + valFile,
		},
		func(c *cli.Command) {
			got, err := resolveTypedOverrides(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("got %d entries, want 2", len(got))
			}
			// The last entry for a given component:path wins when applied; the
			// --set-json entry must be ordered after the --set-file entry.
			last := got[len(got)-1]
			if last.Component != "agentgateway" || last.Path != "allowedSourceRanges" {
				t.Fatalf("last entry = %q/%q, want agentgateway/allowedSourceRanges", last.Component, last.Path)
			}
			if !reflect.DeepEqual(last.Value, []any{"from-json"}) {
				t.Errorf("last (winning) value = %#v, want [from-json] (--set-json must win over --set-file)", last.Value)
			}
		})
}

// TestResolveTypedOverrides_RejectsEnabledToggle verifies the top-level
// "enabled" component toggle is rejected on the typed flags — it is honored
// only via scalar --set, so routing it through --set-json/--set-file would
// silently ship a component the operator believed disabled.
func TestResolveTypedOverrides_RejectsEnabledToggle(t *testing.T) {
	runWith(t, typedOverrideFlags(),
		[]string{"--set-json", "gpuoperator:enabled=false"},
		func(c *cli.Command) {
			_, err := resolveTypedOverrides(c)
			if err == nil {
				t.Fatal("expected error: 'enabled' toggle must use --set")
			}
			if !strings.Contains(err.Error(), "enabled") || !strings.Contains(err.Error(), "--set") {
				t.Errorf("error %q must name the enabled toggle and point to --set", err.Error())
			}
		})
}

// TestResolveTypedOverrides_AllowsNestedEnabledValue verifies that only the
// exact top-level toggle key is blocked; a nested chart value such as
// gds.enabled is a legitimate boolean override.
func TestResolveTypedOverrides_AllowsNestedEnabledValue(t *testing.T) {
	runWith(t, typedOverrideFlags(),
		[]string{"--set-json", "gpuoperator:gds.enabled=true"},
		func(c *cli.Command) {
			got, err := resolveTypedOverrides(c)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || got[0].Path != "gds.enabled" || got[0].Value != true {
				t.Errorf("got %#v, want gds.enabled=true allowed", got)
			}
		})
}

func TestDecodeSetFileValue_MissingFile(t *testing.T) {
	_, err := decodeSetFileValue(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDecodeSetFileValue_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("key: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := decodeSetFileValue(bad)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestDecodeSetFileValue_ExceedsSizeLimit(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.yaml")
	// Write a valid-ish YAML scalar larger than the 1 MiB cap.
	payload := make([]byte, 1*1024*1024+10)
	for i := range payload {
		payload[i] = 'a'
	}
	if err := os.WriteFile(big, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := decodeSetFileValue(big)
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q must mention size limit", err.Error())
	}
}

// TestDecodeSetFileValue_MissingFileIsNotFound verifies a genuinely absent file
// maps to ErrCodeNotFound.
func TestDecodeSetFileValue_MissingFileIsNotFound(t *testing.T) {
	_, err := decodeSetFileValue(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("missing file: got code != NOT_FOUND for %v", err)
	}
}

// TestDecodeSetFileValue_NonNotExistOpenErrorIsInvalidRequest verifies an
// os.Open failure that is NOT "does not exist" (here ENOTDIR: a path component
// is a regular file, not a directory) is reported as ErrCodeInvalidRequest
// rather than masquerading as NOT_FOUND.
func TestDecodeSetFileValue_NonNotExistOpenErrorIsInvalidRequest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular.yaml")
	if err := os.WriteFile(file, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Treating the regular file as a directory triggers ENOTDIR on open.
	notADir := filepath.Join(file, "child.yaml")

	_, err := decodeSetFileValue(notADir)
	if err == nil {
		t.Fatal("expected error opening a path under a non-directory")
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("ENOTDIR open error must not be NOT_FOUND: %v", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("ENOTDIR open error should be INVALID_REQUEST: %v", err)
	}
}

// TestDecodeSetFileValue_DirectoryIsInvalidRequest verifies that pointing
// --set-file at a directory is reported as ErrCodeInvalidRequest (bad operator
// input), not ErrCodeInternal. os.Open succeeds on a directory on Unix; the
// read would otherwise fail later with EISDIR and be wrapped as internal.
func TestDecodeSetFileValue_DirectoryIsInvalidRequest(t *testing.T) {
	dir := t.TempDir()

	_, err := decodeSetFileValue(dir)
	if err == nil {
		t.Fatal("expected error when --set-file points at a directory")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("directory path should be INVALID_REQUEST, got %v", err)
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("directory path must not be INTERNAL: %v", err)
	}
}

// TestDecodeSetFileValue_NonRegularFileIsInvalidRequest verifies that a
// non-regular --set-file path (here a FIFO) is rejected as
// ErrCodeInvalidRequest by the up-front stat, rather than blocking in os.Open
// until a writer appears (which would hang the CLI/CI job).
func TestDecodeSetFileValue_NonRegularFileIsInvalidRequest(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "set-file.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this platform: %v", err)
	}

	// decodeSetFileValue must return promptly with an invalid-request error;
	// because the stat happens before os.Open, no writer is needed and the
	// call does not block.
	_, err := decodeSetFileValue(fifo)
	if err == nil {
		t.Fatal("expected error when --set-file points at a FIFO")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("non-regular path should be INVALID_REQUEST, got %v", err)
	}
	if stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("non-regular path must not be INTERNAL: %v", err)
	}
}

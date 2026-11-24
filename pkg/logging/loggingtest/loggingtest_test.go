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

package loggingtest

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// The tests are not parallel because they swap the default slog logger.

func TestCaptureText(t *testing.T) {
	prev := slog.Default()
	t.Run("capture", func(t *testing.T) {
		logs := CaptureText(t, slog.LevelWarn)
		slog.Info("below the level")
		slog.Warn("captured", "key", "value")
		if got := logs.String(); strings.Contains(got, "below the level") || !strings.Contains(got, "msg=captured key=value") {
			t.Errorf("captured = %q, want only the warning as text", got)
		}
	})
	if slog.Default() != prev {
		t.Error("the default logger was not restored when the test ended")
	}
}

func TestCaptureJSON(t *testing.T) {
	logs := CaptureJSON(t, slog.LevelInfo)
	slog.Debug("below the level")
	slog.Info("first", "n", 1)
	slog.Info("second", "n", 2)

	if strings.Contains(logs.String(), "below the level") {
		t.Errorf("captured %q, want no debug record", logs.String())
	}
	WantIntAttr(t, FindRecord(t, logs.Bytes(), "second"), "n", 2)
}

func TestBufferConcurrentWrites(t *testing.T) {
	logs := CaptureJSON(t, slog.LevelInfo)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { slog.Info("concurrent") })
	}
	for range 8 {
		_ = logs.String()
	}
	wg.Wait()
	if got := strings.Count(logs.String(), `"msg":"concurrent"`); got != 8 {
		t.Errorf("captured %d records, want 8", got)
	}
}

type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Helper()               {}
func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Fatalf(string, ...any) { r.failed = true }

func TestFindRecord(t *testing.T) {
	logs := []byte(`{"msg":"a","n":1}` + "\n" + `{"msg":"b","n":2}` + "\n" + `{"msg":"b","n":3}` + "\n")
	tests := []struct {
		name     string
		logs     []byte
		want     string
		wantN    string
		wantFail bool
	}{
		{"first match", logs, "b", "2", false},
		{"no match", logs, "c", "", true},
		{"undecodable", []byte("not json"), "a", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingTB{TB: t}
			got := FindRecord(rec, tt.logs, tt.want)
			if rec.failed != tt.wantFail {
				t.Fatalf("failed = %v, want %v", rec.failed, tt.wantFail)
			}
			if !tt.wantFail && fmt.Sprint(got["n"]) != tt.wantN {
				t.Errorf("record n = %v, want %s", got["n"], tt.wantN)
			}
		})
	}
}

func TestWantIntAttr(t *testing.T) {
	rec := FindRecord(t, []byte(`{"msg":"m","n":3,"s":"x","f":1.5}`), "m")
	tests := []struct {
		name     string
		key      string
		want     int64
		wantFail bool
	}{
		{"match", "n", 3, false},
		{"wrong value", "n", 4, true},
		{"missing key", "absent", 3, true},
		{"not a number", "s", 3, true},
		{"not an integer", "f", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recordingTB{TB: t}
			WantIntAttr(r, rec, tt.key, tt.want)
			if r.failed != tt.wantFail {
				t.Errorf("failed = %v, want %v", r.failed, tt.wantFail)
			}
		})
	}
}

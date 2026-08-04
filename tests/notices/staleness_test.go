// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package notices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	noticesTool           = "tools/generate-notices"
	committedRequirements = "validators/performance/requirements.txt"
	committedFragment     = "validators/performance/licenses/python-notices.md"
)

// TestCommittedFragmentMatchesRequirements is the cheap, always-runnable half
// of the freshness contract: the committed fragment records the sha256 of the
// requirements file it was generated from, so a requirements edit that skips
// `make python-licenses` is caught here at `make test` time rather than only by
// the notices-freshness merge-gate job.
//
// It needs no tooling and no network, which is the point — the expensive
// regeneration cannot run in every CI job, but this invariant can.
func TestCommittedFragmentMatchesRequirements(t *testing.T) {
	requirements, err := os.ReadFile(repositoryPath(t, committedRequirements))
	if err != nil {
		t.Fatalf("read committed requirements: %v", err)
	}
	fragment, err := os.ReadFile(repositoryPath(t, committedFragment))
	if err != nil {
		t.Fatalf("read committed fragment: %v", err)
	}

	sum := sha256.Sum256(requirements)
	want := hex.EncodeToString(sum[:])

	if !strings.Contains(string(fragment), want) {
		t.Errorf("%s does not record the sha256 of %s (%s).\n"+
			"Run 'make python-licenses' and 'make notices', then commit both.",
			committedFragment, committedRequirements, want)
	}
}

// TestCommittedFragmentIsWellFormed guards the committed artifact itself
// against the corruption modes the generator can produce, independent of
// regenerating it.
func TestCommittedFragmentIsWellFormed(t *testing.T) {
	fragment, err := os.ReadFile(repositoryPath(t, committedFragment))
	if err != nil {
		t.Fatalf("read committed fragment: %v", err)
	}
	text := string(fragment)

	for _, heading := range []string{"## Python Package Index", "## Python Package License Texts"} {
		if !strings.Contains(text, heading) {
			t.Errorf("fragment missing %q", heading)
		}
	}

	// Every index row must have the full cell count; a license string that
	// leaked prose would break the table.
	rows := 0
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		rows++
		if strings.Count(line, "|") != 5 {
			t.Errorf("malformed index row: %q", line)
		}
	}
	if rows == 0 {
		t.Error("fragment index table has no rows")
	}
}

// TestCommittedNoticesFencesBalance guards the committed release artifact
// against license text escaping the code block that wraps it.
//
// Both halves embed arbitrary upstream text, and some of it is Markdown:
// hashicorp/errwrap ships ```go samples and pillow's LICENSE has bare fences.
// A fixed ``` wrapper is closed early by that content, after which the
// remainder renders as Markdown and every following block is inverted for the
// rest of the file. The corruption is invisible in a diff and only shows up
// when the file is rendered, so it is pinned here rather than left to review.
//
// This reads the committed file, so it needs no tooling and runs in every
// `make test`.
func TestCommittedNoticesFencesBalance(t *testing.T) {
	content, err := os.ReadFile(repositoryPath(t, "THIRD_PARTY_NOTICES.md"))
	if err != nil {
		t.Fatalf("read committed notices: %v", err)
	}

	fence := regexp.MustCompile("^(`{3,})(\\w*)\\s*$")
	var open string
	blocks, line := 0, 0
	for text := range strings.SplitSeq(string(content), "\n") {
		line++
		match := fence.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		ticks, info := match[1], match[2]
		if open == "" {
			if info == "" {
				t.Fatalf("line %d: closing fence with no opener (license text escaped "+
					"its block; regenerate with 'make notices')", line)
			}
			open = ticks
			continue
		}
		if info == "" && len(ticks) >= len(open) {
			open = ""
			blocks++
		}
	}
	if open != "" {
		t.Error("unterminated code fence in THIRD_PARTY_NOTICES.md")
	}
	if blocks == 0 {
		t.Error("no fenced license blocks found; the file is not being generated")
	}
}

// TestNoticesRejectsStaleFragment drives the generator's fail-closed path: a
// fragment whose recorded hash does not match the requirements must abort the
// run rather than emit a notices file describing the wrong dependency set.
func TestNoticesRejectsStaleFragment(t *testing.T) {
	// The generator checks its own tool prerequisites before reaching the
	// staleness gate, so skip rather than fail where those are absent. The
	// notices-freshness merge-gate job has them.
	if _, err := exec.LookPath("go-licenses"); err != nil {
		t.Skip("go-licenses not installed; the merge gate covers this path")
	}

	dir := t.TempDir()
	requirements := filepath.Join(dir, "requirements.txt")
	fragment := filepath.Join(dir, "python-notices.md")

	if err := os.WriteFile(requirements, []byte("example==9.9.9\n"), 0o600); err != nil {
		t.Fatalf("write requirements: %v", err)
	}
	// Records a hash that cannot match the requirements above.
	if err := os.WriteFile(fragment, []byte("## Python Package Index\n\nsha256 `"+
		strings.Repeat("0", 64)+"`\n"), 0o600); err != nil {
		t.Fatalf("write fragment: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, "bash", repositoryPath(t, noticesTool))
	command.Dir = repositoryPath(t, ".")
	command.Env = append(os.Environ(),
		"PYTHON_REQUIREMENTS="+requirements,
		"PYTHON_NOTICES="+fragment,
		"OUTPUT="+filepath.Join(dir, "NOTICES.md"),
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("generator exceeded deadline: %v\n%s", ctx.Err(), output)
	}

	if err == nil {
		t.Fatal("generator succeeded with a stale fragment; it must fail closed")
	}
	if !strings.Contains(string(output), "is stale") {
		t.Errorf("expected a staleness error, got:\n%s", output)
	}
}

// TestNoticesRejectsMissingFragment covers the other fail-closed path: without
// the Python fragment the generator must not quietly emit a Go-only file.
func TestNoticesRejectsMissingFragment(t *testing.T) {
	if _, err := exec.LookPath("go-licenses"); err != nil {
		t.Skip("go-licenses not installed; the merge gate covers this path")
	}

	dir := t.TempDir()
	requirements := filepath.Join(dir, "requirements.txt")
	if err := os.WriteFile(requirements, []byte("example==9.9.9\n"), 0o600); err != nil {
		t.Fatalf("write requirements: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, "bash", repositoryPath(t, noticesTool))
	command.Dir = repositoryPath(t, ".")
	command.Env = append(os.Environ(),
		"PYTHON_REQUIREMENTS="+requirements,
		"PYTHON_NOTICES="+filepath.Join(dir, "does-not-exist.md"),
		"OUTPUT="+filepath.Join(dir, "NOTICES.md"),
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("generator exceeded deadline: %v\n%s", ctx.Err(), output)
	}

	if err == nil {
		t.Fatal("generator succeeded without the Python fragment; it must fail closed")
	}
	if !strings.Contains(string(output), "missing or empty") {
		t.Errorf("expected a missing-fragment error, got:\n%s", output)
	}
}

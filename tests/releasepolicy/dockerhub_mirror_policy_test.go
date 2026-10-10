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

package releasepolicy

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// dockerHubMirrorCheckTimeout bounds one bash evaluation of the validator.
const dockerHubMirrorCheckTimeout = 10 * time.Second

// dockerHubMirrorValidators are the two checks of .settings.yaml
// build.dockerhub_mirror: load-versions for every CI job, the Makefile for
// local `make cluster-create`. A looser copy lets a malformed override through
// to an invalid image ref, which the pre-pull answers with a warning and an
// anonymous Docker Hub pull. Submatch 1 is the pattern as written in the file.
var dockerHubMirrorValidators = []struct {
	file     string
	locate   *regexp.Regexp
	unescape func(string) string
}{
	{
		file:     ".github/actions/load-versions/action.yml",
		locate:   regexp.MustCompile(`\$\{dockerhub_mirror\} =~ (\^https://\S+) \]\]`),
		unescape: func(s string) string { return s },
	},
	{
		file:     "Makefile",
		locate:   regexp.MustCompile(`\[\[ \$\$DOCKERHUB_MIRROR =~ (\^https://\S+) \]\]`),
		unescape: func(s string) string { return strings.ReplaceAll(s, "$$", "$") },
	},
}

// TestDockerHubMirrorValidatorsAgree holds both checks to one pattern and pins
// what that pattern accepts. Values are judged by bash's own [[ =~ ]], the
// construct both checks use: Go's POSIX mode lets ^ and $ match at line
// breaks, which bash does not.
func TestDockerHubMirrorValidatorsAgree(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is required to evaluate the validators: %v", err)
	}

	patterns := make([]string, 0, len(dockerHubMirrorValidators))
	for _, v := range dockerHubMirrorValidators {
		matches := v.locate.FindAllStringSubmatch(string(readFile(t, v.file)), -1)
		if len(matches) != 1 {
			t.Fatalf("%s: found %d dockerhub_mirror validation patterns, want 1", v.file, len(matches))
		}
		patterns = append(patterns, v.unescape(matches[0][1]))
	}
	for i := 1; i < len(patterns); i++ {
		if patterns[i] != patterns[0] {
			t.Fatalf("dockerhub_mirror validators disagree:\n  %s: %s\n  %s: %s",
				dockerHubMirrorValidators[0].file, patterns[0], dockerHubMirrorValidators[i].file, patterns[i])
		}
	}

	matches := func(value string) bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), dockerHubMirrorCheckTimeout)
		defer cancel()
		err := exec.CommandContext(ctx, bash, "-c", `[[ $1 =~ $2 ]]`, "_", value, patterns[0]).Run()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			return true
		case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
			return false
		default:
			t.Fatalf("bash [[ %q =~ %s ]]: %v", value, patterns[0], err)
			return false
		}
	}

	tests := []struct {
		value string
		want  bool
	}{
		{"https://mirror.gcr.io", true},
		{"https://harbor.example.com:5000", true},
		{"", false},
		{"null", false},
		{"mirror.gcr.io", false},
		{"http://mirror.gcr.io", false},
		{"https://", false},
		{"https://mirror.gcr.io/", false},
		{"https://mirror.gcr.io/v2", false},
		{"https://mirror.gcr.io:", false},
		{"https://host:abc", false},
		{"https://user:token@mirror.gcr.io", false},
		{"https://mirror.gcr.io?x", false},
		{"https://mirror gcr.io", false},
		{"https://mirror.gcr.io\nhttps://evil.example", false},
	}
	for _, tt := range tests {
		if got := matches(tt.value); got != tt.want {
			t.Errorf("validator(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}

	committed := settingsDockerHubMirror(t)
	if !matches(committed) {
		t.Errorf(".settings.yaml build.dockerhub_mirror %q fails the validator %s", committed, patterns[0])
	}
}

// TestDockerHubMirrorIsNotHardCoded keeps .settings.yaml the only place the
// mirror host is written: a literal in a workflow, action or the Makefile is a
// second copy that a mirror switch silently misses (#3170).
func TestDockerHubMirrorIsNotHardCoded(t *testing.T) {
	host := strings.TrimPrefix(settingsDockerHubMirror(t), "https://")
	if host == "" {
		t.Fatal(".settings.yaml build.dockerhub_mirror is empty")
	}

	files := []string{"Makefile"}
	root := repositoryRoot(t)
	walkErr := filepath.WalkDir(filepath.Join(root, ".github"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk .github: %v", walkErr)
	}

	for _, file := range files {
		for i, line := range strings.Split(string(readFile(t, file)), "\n") {
			if strings.Contains(line, host) {
				t.Errorf("%s:%d hard-codes the Docker Hub mirror %q; read build.dockerhub_mirror "+
					"(load-versions outputs dockerhub_mirror/dockerhub_mirror_host, Makefile "+
					"$(DOCKERHUB_MIRROR_HOST)) instead", file, i+1, host)
			}
		}
	}
}

func settingsDockerHubMirror(t *testing.T) string {
	t.Helper()
	build, ok := loadYAML(t, ".settings.yaml")["build"].(map[string]any)
	if !ok {
		t.Fatal(".settings.yaml has no build map")
	}
	mirror, ok := build["dockerhub_mirror"].(string)
	if !ok {
		t.Fatalf(".settings.yaml build.dockerhub_mirror must be a string, got %T", build["dockerhub_mirror"])
	}
	return mirror
}

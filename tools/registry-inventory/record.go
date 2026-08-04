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
	"net/url"
	"sort"
	"strings"
)

// Direction of a registry interaction from AICR's point of view.
const (
	DirPull = "pull"
	DirPush = "push"
)

// Consumer classifies who actually performs the fetch or publish. This is the
// most operationally useful axis (see the report's "two consumers" framing) but
// it is an inference from the source location, not a declared field — treat it
// as best-effort.
const (
	ConsumerCIRunner      = "ci-runner"      // GitHub/self-hosted runner build-time
	ConsumerTargetCluster = "target-cluster" // pulled by the cluster a bundle deploys to
	ConsumerCompiledIn    = "compiled-in"    // ships inside an AICR image
	ConsumerDev           = "dev"            // local dev / Tilt / ctlptl only
)

// PackageType is the kind of artifact fetched or published.
const (
	PkgContainerImage = "container-image"
	PkgOCIHelmChart   = "oci-helm-chart"
	PkgHelmChartHTTP  = "helm-chart-http"
	PkgHTTP           = "http" // generic https:// egress that isn't a declared chart repo
	PkgGitHubAction   = "github-action"
	// Shell-installer package types (best-effort extraction; see shell.go).
	PkgBinaryRelease = "binary-release"
	PkgInstallScript = "install-script"
	PkgGoModule      = "go-module"
	PkgPyPI          = "pypi"
	PkgApt           = "apt"
	PkgBrew          = "brew"
)

// Symbolic hosts for package managers that resolve to a mirror set rather than a
// single DNS name. They are still allowlist units — a reviewer approves "this
// build may install apt/brew packages" — but they are not literal hostnames.
const (
	hostGoProxy  = "proxy.golang.org" // default GOPROXY for `go install`
	hostGoSum    = "sum.golang.org"   // checksum db `go install` also contacts
	hostPyPI     = "pypi.org"         // pip
	hostApt      = "apt"              // symbolic: distro apt mirror set
	hostHomebrew = "homebrew"         // symbolic: Homebrew bottle infra
)

// PinType records how tightly a reference is pinned, strongest first.
const (
	PinDigest = "digest" // @sha256:...
	PinSHA    = "sha"    // 40-char git commit (GitHub Actions)
	PinTag    = "tag"    // :v1.2.3 or version field
	PinBranch = "branch" // @main / @master — mutable
	PinLatest = "latest" // :latest — mutable
	PinArg    = "arg"    // build-arg / unresolved ${VAR} interpolation
	PinVar    = "var"    // pinned via a shell var (resolved from .settings.yaml at runtime)
	PinNone   = "none"   // no discernible pin
)

// dockerHubHost is the registry a bare or single-org image reference resolves to
// under Docker's implicit-registry convention.
const dockerHubHost = "docker.io"

// Record is one normalized registry-egress fact: a single (host, artifact, direction)
// tuple with provenance. The whole point of the tool is to reduce every source
// — YAML, Dockerfiles, workflows — to a stream of these.
type Record struct {
	Host        string `yaml:"host"`
	PackageType string `yaml:"packageType"`
	Direction   string `yaml:"direction"`
	Consumer    string `yaml:"consumer"`
	PinType     string `yaml:"pinType"`
	Pin         string `yaml:"pin,omitempty"`
	Detail      string `yaml:"detail,omitempty"`
	Source      string `yaml:"source"`
}

// key is the dedup/sort key. Two records with the same key are considered the
// same fact even if discovered via different passes. Consumer and PinType are
// included so records that genuinely differ only in those fields are not
// collapsed into an arbitrary survivor (F7).
func (r Record) key() string {
	return strings.Join([]string{
		r.Host, r.PackageType, r.Direction, r.Consumer, r.PinType, r.Detail, r.Pin, r.Source,
	}, "\x00")
}

// sortRecords orders records deterministically so the emitted inventory is
// byte-stable across runs (required for a committable artifact / diff gate).
func sortRecords(recs []Record) {
	sort.SliceStable(recs, func(i, j int) bool {
		return recs[i].key() < recs[j].key()
	})
}

// dedupRecords removes exact-duplicate facts, preserving order.
func dedupRecords(recs []Record) []Record {
	seen := make(map[string]struct{}, len(recs))
	out := recs[:0]
	for _, r := range recs {
		k := r.key()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// imageHost extracts the registry host from a container image reference,
// applying Docker's implicit-registry convention: a first path segment with no
// "." or ":" (and not "localhost") means the default registry, docker.io.
//
//	nvcr.io/nvidia/distroless/static:v4.0.0@sha256:...  -> nvcr.io
//	public.ecr.aws/docker/library/registry:3.1.1        -> public.ecr.aws
//	kindest/node:v1.36.1                                 -> docker.io
//	busybox:1.37                                         -> docker.io
//	localhost:5001/aicrd:tilt                            -> localhost:5001
func imageHost(ref string) string {
	name := ref
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	slash := strings.Index(name, "/")
	if slash < 0 {
		return dockerHubHost
	}
	first := name[:slash]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return dockerHubHost
}

// imagePin classifies how a container image ref is pinned. A digest always
// wins over a co-present tag because it is the immutable identity.
func imagePin(ref string) (pinType, pin string) {
	if i := strings.Index(ref, "@sha256:"); i >= 0 {
		return PinDigest, ref[i+1:]
	}
	if strings.Contains(ref, "${") || strings.Contains(ref, "{{") {
		// Unresolved build-arg / template — host may still be literal, but the
		// tag is not knowable from a static read.
		return PinArg, ""
	}
	lastSlash := strings.LastIndex(ref, "/")
	tagPart := ref[lastSlash+1:]
	if i := strings.LastIndex(tagPart, ":"); i >= 0 {
		// A slashless ref's colon may be a registry port (host:port), not an
		// image tag — don't misread `localhost:5001` as tag "5001" (F9).
		if lastSlash < 0 {
			prefix := tagPart[:i]
			if strings.Contains(prefix, ".") || prefix == "localhost" {
				return PinNone, ""
			}
		}
		tag := tagPart[i+1:]
		if tag == "latest" || tag == "" {
			return PinLatest, tag
		}
		return PinTag, tag
	}
	return PinNone, ""
}

// urlHost extracts the host from a Helm repository URL, tolerating the oci://
// scheme (which net/url otherwise parses as opaque).
//
//	https://helm.ngc.nvidia.com/nvidia        -> helm.ngc.nvidia.com
//	oci://ghcr.io/nvidia/nodewright/charts     -> ghcr.io
func urlHost(raw string) string {
	r := strings.TrimSpace(raw)
	r = strings.TrimPrefix(r, "oci://")
	if !strings.Contains(r, "://") {
		r = "https://" + r
	}
	u, err := url.Parse(r)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// actionPin classifies a GitHub Actions `uses:` ref pin (the part after the
// last "@").
func actionPin(ref string) (pinType, pin string) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return PinNone, ""
	}
	r := ref[at+1:]
	switch {
	case isSHA40(r):
		return PinSHA, r
	case r == "main" || r == "master":
		return PinBranch, r
	default:
		return PinTag, r
	}
}

// isSHA40 reports whether s is a 40-character hex git commit SHA (either case).
func isSHA40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

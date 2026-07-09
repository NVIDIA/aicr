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

package config

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

// DeployerOverrideKey is the reserved --set component key that carries
// deployer-level Argo Application options instead of component Helm
// values (e.g. `--set deployer:namePrefix=tenant-a-`). The component
// registry must never declare a component with this name or override
// key — a guard test in pkg/recipe enforces the reservation. See #1625.
const DeployerOverrideKey = "deployer"

// Allowlisted deployer option keys. Free-form paths into the generated
// Application YAML are intentionally NOT supported: installs must not
// clobber generator-owned fields (repoURL assembly, path, helm.values),
// and unknown or future keys must fail closed. See #1625 / #1628.
const (
	deployerKeyNamePrefix        = "namePrefix"
	deployerKeyDestinationServer = "destinationServer"
	deployerKeyProject           = "project"
	deployerKeyCascadeDelete     = "cascadeDelete"
)

// namePrefixPattern constrains child-name prefixes to lowercase DNS-safe
// characters. A trailing hyphen is allowed (the idiomatic "tenant-a-"
// shape); the composed <prefix><child> name is additionally validated as
// a full DNS-1123 subdomain at generation time.
var namePrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateNamePrefix reports whether prefix is a usable child-name
// prefix. Empty is allowed (means "no prefix"); non-empty values must
// be lowercase alphanumeric with hyphens, starting with an alphanumeric
// character (trailing hyphen allowed — the idiomatic "tenant-a-" shape).
func ValidateNamePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !namePrefixPattern.MatchString(prefix) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid deployer namePrefix %q: must be lowercase alphanumeric with hyphens, starting with an alphanumeric character", prefix))
	}
	return nil
}

// ValidateDestinationServer reports whether raw is a usable child
// Application spec.destination.server value. Beyond the https:// shape
// enforced by ValidateHTTPSURL, the value must not contain quotes,
// spaces, or control characters: the argocd-helm child templates render
// the value inside fixed single quotes, so an embedded quote (e.g.
// https://host/o'brien:6443 — accepted by url.Parse) would produce
// invalid YAML at `helm template` time. Cluster API URLs never contain
// such characters, so rejecting them at validation time is safe.
// Empty is allowed (means "use the in-cluster default").
func ValidateDestinationServer(raw string) error {
	if err := ValidateHTTPSURL("deployer destinationServer", raw); err != nil {
		return err
	}
	// Mirror the install-time values.schema.json pattern exactly so the two
	// gates cannot drift (see the validation-contract parity test in
	// pkg/bundler/deployer/argocdhelm): the scheme must be lowercase
	// (url.Parse lowercases it, so ValidateHTTPSURL alone would accept
	// "HTTPS://..."), and "@" is rejected anywhere — not just as userinfo —
	// because the schema regex cannot distinguish authority from path.
	if raw != "" && !strings.HasPrefix(raw, "https://") {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid deployer destinationServer %q: scheme must be lowercase https://", raw))
	}
	if strings.ContainsAny(raw, `'" @`) || strings.ContainsFunc(raw, unicode.IsControl) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid deployer destinationServer %q: must not contain quotes, spaces, control characters, or '@'", raw))
	}
	return nil
}

// ValidateProject reports whether value is a usable child Application
// spec.project. Empty is allowed (means "use the default project").
// Bundle-time and install-time (values.schema.json) gates both mirror
// IsDNS1123Subdomain exactly: total length capped at 253 characters,
// per-label caps deliberately NOT enforced — Kubernetes object names
// don't enforce them, so an AppProject with a 64+-character label is a
// legal object and rejecting a reference to it would be a false positive
// with no workaround (see the validation-contract parity test in
// pkg/bundler/deployer/argocdhelm).
func ValidateProject(value string) error {
	if value == "" {
		return nil
	}
	if errs := validation.IsDNS1123Subdomain(value); len(errs) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid deployer project %q: %s", value, errs[0]))
	}
	return nil
}

// ArgoDeployerOptions carries the deployer-level Argo Application options
// supplied via `--set deployer:<key>=<value>`. All fields apply to the
// generated child Applications only, except CascadeDelete which also
// applies to the parent — the parent Application must stay on the Argo CD
// control-plane cluster in the default project (App-of-Apps is an
// admin-only pattern; see the #1625 discussion for the upstream doc
// references).
type ArgoDeployerOptions struct {
	// NamePrefix is prepended to every child Application metadata.name
	// (fixes same-namespace collisions for multi-tenant installs).
	NamePrefix string
	// DestinationServer overrides spec.destination.server on child
	// Applications (default https://kubernetes.default.svc).
	DestinationServer string
	// Project overrides spec.project on child Applications (default
	// "default").
	Project string
	// CascadeDelete adds resources-finalizer.argocd.argoproj.io to the
	// parent and every child Application so deleting the parent cascades
	// to managed resources. See #1628.
	CascadeDelete bool
}

// ParseArgoDeployerOptions validates and converts the raw
// `--set deployer:*` override map into ArgoDeployerOptions. Returns
// (nil, nil) when no deployer overrides were supplied. Unknown keys and
// invalid values are rejected with ErrCodeInvalidRequest — a CLI typo
// must not silently ship a misconfigured artifact.
func ParseArgoDeployerOptions(overrides map[string]string) (*ArgoDeployerOptions, error) {
	if len(overrides) == 0 {
		//nolint:nilnil // returning (nil, nil) is intentional for optional configuration
		return nil, nil
	}
	opts := &ArgoDeployerOptions{}
	// Iterate keys in sorted order so error reporting is deterministic
	// when multiple entries are invalid (Go map order is randomized).
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := overrides[key]
		switch key {
		case deployerKeyNamePrefix:
			if err := ValidateNamePrefix(value); err != nil {
				return nil, err
			}
			opts.NamePrefix = value
		case deployerKeyDestinationServer:
			if err := ValidateDestinationServer(value); err != nil {
				return nil, err
			}
			opts.DestinationServer = value
		case deployerKeyProject:
			// An explicit empty value is a user error at parse time (the
			// generator boundary allows empty as "use the default", but an
			// explicit `--set deployer:project=` must fail closed).
			if value == "" {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					`invalid deployer project "": must not be empty`)
			}
			if err := ValidateProject(value); err != nil {
				return nil, err
			}
			opts.Project = value
		case deployerKeyCascadeDelete:
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("invalid deployer cascadeDelete value %q: must be a boolean", value), err)
			}
			opts.CascadeDelete = parsed
		default:
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown deployer option %q: valid keys are cascadeDelete, destinationServer, namePrefix, project", key))
		}
	}
	return opts, nil
}

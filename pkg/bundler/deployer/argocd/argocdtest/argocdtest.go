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

// Package argocdtest provides child Application name fixtures shared by the
// argocd and argocd-helm deployer tests. It must not import either deployer,
// so their internal tests can use it.
package argocdtest

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

const component = "gpu-operator"

// ChildNameCase is a generator configuration whose composed child
// Application names both Argo CD deployers must reject.
type ChildNameCase struct {
	Name          string
	AppName       string
	NamePrefix    string
	PreManifests  map[string]map[string][]byte
	PostManifests map[string]map[string][]byte
	Readiness     map[string]map[string][]byte
	// ErrSubstr is a substring the rejection error must contain.
	ErrSubstr string
}

// ChildNameCases returns child names over Helm's 53-character release-name
// cap, and a primary, -pre, -post and -readiness child that each collide
// with the parent Application name. Every case assumes the recipe holds
// gpu-operator, sourced from a Helm repository so a post manifest emits a
// -post child.
func ChildNameCases() []ChildNameCase {
	manifest := func() map[string]map[string][]byte {
		return map[string]map[string][]byte{
			component: {"extra.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n")},
		}
	}
	const (
		prefix   = "tenant-"
		collides = "collides"
	)
	return []ChildNameCase{
		{
			// prefix (50) + "gpu-operator" (12) = 62 > 53, but each label
			// stays DNS-1123-valid so only the release-name cap fires.
			Name:       "composed name exceeds Helm release-name cap",
			NamePrefix: strings.Repeat("a", 49) + "-",
			ErrSubstr:  "53",
		},
		{
			Name:       "child name collides with parent app name",
			AppName:    prefix + component,
			NamePrefix: prefix,
			ErrSubstr:  collides,
		},
		{
			Name:         "pre child collides with parent app name",
			AppName:      prefix + component + "-pre",
			NamePrefix:   prefix,
			PreManifests: manifest(),
			ErrSubstr:    collides,
		},
		{
			Name:          "post child collides with parent app name",
			AppName:       prefix + component + "-post",
			NamePrefix:    prefix,
			PostManifests: manifest(),
			ErrSubstr:     collides,
		},
		{
			Name:       "readiness child collides with parent app name",
			AppName:    prefix + component + "-readiness",
			NamePrefix: prefix,
			Readiness:  manifest(),
			ErrSubstr:  collides,
		},
	}
}

// WantRejected fails t unless err reports ErrCodeInvalidRequest and contains
// want. entry names the call that returned err.
func WantRejected(t testing.TB, entry string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: error = nil, want a rejection containing %q", entry, want)
		return
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: error %q does not contain %q", entry, err.Error(), want)
	}
}

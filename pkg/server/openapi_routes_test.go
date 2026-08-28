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

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// REST is one of the four surfaces ROADMAP §1 freezes at v1, and
// api/aicr/v1/server.yaml is its declared contract. Until this file, nothing in
// the tree read that spec for routing purposes: no workflow, no Makefile target,
// and no tool validated it, diffed it, or checked it against the handlers. The
// published contract and the running server were free to drift, and did — #1943
// had to retroactively align the spec with what the handler actually accepted.
//
// These tests close the routing half of that gap (issue #2112). They are
// deliberately derived from the spec rather than from a hand-maintained list:
// TestRouteConfiguration in serve_test.go already pins the six application
// routes by hand, which catches a deleted route but cannot catch a route the
// spec promises and the server never registers.
//
// Scope: paths and methods only. Request and response *shapes* are covered by
// the contract tests in openapi_sync_test.go, and the breaking-change diff gate
// against a committed baseline is the remaining part of #2112 — that baseline
// cannot be captured until #2417 removes the alpha apiVersion enum values, or
// it would fail on its own planned removal.

const specRelPath = "../../api/aicr/v1/server.yaml"

// httpMethods are the operation keys OpenAPI allows under a path item. Anything
// else at that level (parameters, summary, servers, $ref) is not an operation.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// systemRoutes are registered directly on the mux in New rather than through
// newRoutes, so they have no other in-code source of truth to compare against.
// Keep in sync with the mux.HandleFunc calls in server.go.
var systemRoutes = []string{"/health", "/ready", "/metrics"}

// specOperations returns the spec's declared path -> sorted uppercase methods.
func specOperations(t *testing.T) map[string][]string {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(specRelPath))
	if err != nil {
		t.Fatalf("read spec %q: %v", specRelPath, err)
	}

	var spec struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("spec declares no paths; the parse shape is wrong and every " +
			"assertion below would pass vacuously")
	}

	ops := make(map[string][]string, len(spec.Paths))
	for path, item := range spec.Paths {
		var methods []string
		for key := range item {
			if httpMethods[strings.ToLower(key)] {
				methods = append(methods, strings.ToUpper(key))
			}
		}
		sort.Strings(methods)
		ops[path] = methods
	}
	return ops
}

// newSpecTestServer builds a server wired exactly as Serve wires it, with rate
// limiting effectively disabled.
//
// The method tests below send many requests through one server. At the default
// limit they would start collecting 429s, and a 429 is neither the 405 nor the
// not-405 those tests assert — the suite would report contract violations that
// are really throttling. Raising the limit keeps the assertions about methods.
func newSpecTestServer(t *testing.T) *Server {
	t.Helper()

	cfg := parseConfig()
	cfg.Handlers = newRoutes(newTestHandler(t, nil), newTestBundleHandler(t))
	cfg.RateLimit = 1e6
	cfg.RateLimitBurst = 1e6
	return New(withConfig(cfg))
}

// registeredPaths returns every path the server actually serves.
//
// It builds a real Server rather than reading newRoutes directly, because
// New also installs the root "/" handler via configureRootHandler. Reading
// newRoutes alone would miss it and report "/" as an undelivered promise of the
// spec, which is how this helper was wrong on its first draft.
func registeredPaths(t *testing.T) map[string]bool {
	t.Helper()

	s := newSpecTestServer(t)

	paths := make(map[string]bool, len(s.config.Handlers)+len(systemRoutes))
	for path := range s.config.Handlers {
		paths[path] = true
	}
	for _, path := range systemRoutes {
		paths[path] = true
	}
	return paths
}

// probeMethods is every method the spec's own operation vocabulary allows, so a
// path that quietly answers OPTIONS or HEAD cannot escape the undeclared-method
// check by being outside a hand-picked probe list.
func probeMethods() []string {
	methods := make([]string, 0, len(httpMethods))
	for m := range httpMethods {
		methods = append(methods, strings.ToUpper(m))
	}
	sort.Strings(methods)
	return methods
}

// TestOpenAPISpecPathsMatchRegisteredRoutes asserts the published contract and
// the running server describe the same set of paths, in both directions.
//
// A spec path with no route is a promise the server does not keep: a client
// generated from the spec gets a 404 on an endpoint the contract advertises. A
// route missing from the spec is an undocumented public endpoint that the
// forthcoming breaking-change gate would never protect, because a gate cannot
// diff what the baseline never contained.
func TestOpenAPISpecPathsMatchRegisteredRoutes(t *testing.T) {
	ops := specOperations(t)
	registered := registeredPaths(t)

	var promisedButNotRouted, routedButNotDocumented []string

	for path := range ops {
		if !registered[path] {
			promisedButNotRouted = append(promisedButNotRouted, path)
		}
	}
	for path := range registered {
		if _, ok := ops[path]; !ok {
			routedButNotDocumented = append(routedButNotDocumented, path)
		}
	}
	sort.Strings(promisedButNotRouted)
	sort.Strings(routedButNotDocumented)

	for _, path := range promisedButNotRouted {
		t.Errorf("api/aicr/v1/server.yaml declares %q but pkg/server registers no "+
			"such route; a client generated from the spec would get a 404", path)
	}
	for _, path := range routedButNotDocumented {
		t.Errorf("pkg/server serves %q but api/aicr/v1/server.yaml does not declare "+
			"it; an undocumented endpoint cannot be protected by the REST "+
			"breaking-change gate", path)
	}
}

// TestOpenAPISpecMethodsAreAccepted asserts every method the spec declares is
// actually accepted by the handler behind that path.
//
// The check is deliberately narrow: it asserts only that the response is not
// 405. A documented operation may legitimately answer 400 for a request this
// test does not bother to populate, and asserting a success status would make
// the test a fixture-maintenance burden rather than a contract check.
func TestOpenAPISpecMethodsAreAccepted(t *testing.T) {
	ops := specOperations(t)
	// Drive the assembled mux, not the bare handler map. /, /health, /ready and
	// /metrics are registered outside newRoutes, so a handler-map loop skips the
	// four routes most likely to be forgotten.
	mux := newSpecTestServer(t).httpServer.Handler

	paths := make([]string, 0, len(ops))
	for path := range ops {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		if len(ops[path]) == 0 {
			t.Errorf("spec path %q declares no HTTP operations", path)
			continue
		}

		for _, method := range ops[path] {
			t.Run(method+" "+path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

				if rec.Code == http.StatusMethodNotAllowed {
					t.Errorf("spec declares %s %s but the server answers 405; "+
						"the published contract advertises an operation the "+
						"server rejects", method, path)
				}
			})
		}
	}
}

// TestOpenAPIUndeclaredMethodsAreRejected asserts the contract is not narrower
// than the server: a method the spec omits must not quietly work.
//
// This is the direction that rots silently. An endpoint that accepts POST while
// the spec documents only GET is an undocumented, ungated public operation, and
// nothing else in the tree would notice.
func TestOpenAPIUndeclaredMethodsAreRejected(t *testing.T) {
	ops := specOperations(t)
	mux := newSpecTestServer(t).httpServer.Handler

	// Every public route, not just the application ones: /health, /ready and
	// /metrics are registered straight onto the mux, and an undeclared method
	// quietly working there is exactly as much of an ungated operation.
	registered := registeredPaths(t)
	paths := make([]string, 0, len(registered))
	for path := range registered {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		declared := make(map[string]bool, len(ops[path]))
		for _, m := range ops[path] {
			declared[m] = true
		}

		for _, method := range probeMethods() {
			if declared[method] {
				continue
			}
			t.Run(method+" "+path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s is not declared in api/aicr/v1/server.yaml but "+
						"the server answered %d instead of 405; either document "+
						"the operation or reject it", method, path, rec.Code)
				}
			})
		}
	}
}

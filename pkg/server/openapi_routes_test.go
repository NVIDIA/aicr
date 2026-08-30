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
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
// The set comes from Server.routePaths, recorded as New registers each pattern
// via s.handle. That covers cooperating registrations only — a raw
// mux.HandleFunc would be served and never recorded — so the invariant is
// enforced separately at the source by TestMuxRegistrationsGoThroughHandle. Two earlier revisions of
// this helper were wrong in exactly that way: reading newRoutes alone missed the
// root "/" handler installed by configureRootHandler, and the hand-maintained
// systemRoutes list that replaced it would have missed any future route wired
// directly in New -- which is where the system endpoints already live.
func registeredPaths(t *testing.T) map[string]bool {
	t.Helper()

	s := newSpecTestServer(t)
	if len(s.routePaths) == 0 {
		t.Fatal("server recorded no routes; every assertion below would pass vacuously")
	}

	paths := make(map[string]bool, len(s.routePaths))
	for _, path := range s.routePaths {
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

// TestOpenAPIDeclaredMethodsAreNotRejected asserts no method the spec declares
// is refused as unsupported by the handler behind that path.
//
// The oracle is deliberately narrow, and the name says so rather than promising
// more: it checks only that the response is not 405. A documented operation may
// legitimately answer 400 for a request this test does not populate, and one
// that 500s still passes here. Asserting success codes would turn this into a
// fixture treadmill for every endpoint's required inputs, which is a different
// test with a different maintenance cost.
func TestOpenAPIDeclaredMethodsAreNotRejected(t *testing.T) {
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
//
// A deviation this pins, deliberately: HEAD is rejected on /health, /ready, and
// the v1/v2 endpoints, because their handlers gate on r.Method != GET. RFC 9110
// §9.1 makes GET and HEAD mandatory for a general-purpose server, so that is a
// standing wart — pre-existing, not introduced here, and left alone rather than
// widened as a side effect of a conformance test. /metrics is the exception: it
// accepted HEAD before this gate existed, so it declares head: and readOnly
// honors it. If the others are aligned later, declare head: for them too rather
// than deleting this assertion.
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

// TestMetricsMethodRejectionIsStructured pins the 405 body shape on /metrics.
//
// An earlier revision used http.Error, which writes a bare string, while the
// seven other 405 sites in this package — including handleHealth and
// handleReady, registered on the same mux — return the structured envelope.
// A client parsing the error for one system endpoint would have received JSON
// from /health and plain text from /metrics for the identical condition.
func TestMetricsMethodRejectionIsStructured(t *testing.T) {
	mux := newSpecTestServer(t).httpServer.Handler

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/metrics", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON to match the other 405 sites", ct)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) ||
		!strings.Contains(allow, http.MethodHead) {

		t.Errorf("Allow = %q, want it to advertise GET and HEAD", allow)
	}

	// HEAD must reach promhttp rather than being refused (RFC 9110 §9.1).
	headRec := httptest.NewRecorder()
	mux.ServeHTTP(headRec, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if headRec.Code != http.StatusOK {
		t.Errorf("HEAD /metrics = %d, want 200", headRec.Code)
	}
}

// TestMuxRegistrationsGoThroughHandle is what actually closes the gap that
// Server.routePaths only appears to close.
//
// routePaths records a pattern when it is registered *via s.handle*. A raw
// mux.HandleFunc("/debug", ...) written directly in New bypasses the recording,
// so the route is served, absent from routePaths, and therefore invisible to
// all three conformance tests above — the exact scenario those tests exist to
// catch. An earlier revision of this file claimed the route set "cannot drift";
// that claim was only true for registrations that already cooperated, and the
// mutation used to check it went through s.handle, so it proved the recording
// path worked rather than that the bypass was caught.
//
// Go's http.ServeMux exposes no way to enumerate its patterns, so the invariant
// cannot be verified at runtime. It is enforced at the source instead: every
// mux.Handle / mux.HandleFunc call site must live inside the handle helper.
func TestMuxRegistrationsGoThroughHandle(t *testing.T) {
	t.Parallel()

	const src = "server.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	// Locate the helper's body so call sites inside it can be excluded.
	var helperStart, helperEnd token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "handle" || fn.Recv == nil {
			return true
		}
		helperStart, helperEnd = fn.Pos(), fn.End()
		return false
	})
	if !helperStart.IsValid() {
		t.Fatal("could not find func (s *Server) handle; this test cannot enforce its invariant")
	}

	var offenders []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "mux" {
			return true
		}
		if call.Pos() >= helperStart && call.Pos() < helperEnd {
			return true // the one legitimate site
		}
		offenders = append(offenders,
			fmt.Sprintf("%s: mux.%s", fset.Position(call.Pos()), sel.Sel.Name))
		return true
	})

	for _, o := range offenders {
		t.Errorf("%s registers a route without going through s.handle, so it is "+
			"absent from Server.routePaths and invisible to the OpenAPI route "+
			"conformance tests; use s.handle(mux, pattern, handler) instead", o)
	}
}

// TestOpenAPIRootRoutesExampleMatchesDiscovery asserts the GET / root discovery
// example lists exactly the routes the running handler advertises.
//
// That example is rendered into the generated client docs as the advertised
// route list, but nothing derived it from anything, so it silently outlived the
// /v2 family it used to name and — after the mechanical rewrite that removed
// that family — listed each surviving route twice. Route conformance above
// compares the spec's paths to the mux and never looks inside an example.
//
// The comparison is against the handler's own output rather than against the
// spec's declared paths, because those two sets are legitimately different:
// /health, /ready and /metrics are declared operations but are registered
// straight onto the mux and deliberately absent from config.Handlers, so the
// discovery response does not advertise them. Asserting "every declared path
// appears" would demand they be listed and fail on correct code.
func TestOpenAPIRootRoutesExampleMatchesDiscovery(t *testing.T) {
	example := rootRoutesExample(t)
	if len(example) == 0 {
		t.Fatal("GET / routes example is empty or did not parse; it should " +
			"advertise the application routes a client can call, and an empty " +
			"result would make every assertion below pass vacuously")
	}

	seen := make(map[string]bool, len(example))
	for _, path := range example {
		if seen[path] {
			t.Errorf("GET / routes example lists %q more than once; the handler "+
				"builds the list from a map of registered paths, so it can "+
				"never repeat a route", path)
		}
		seen[path] = true
	}

	// Every advertised route must also be a documented operation. Discovery
	// pointing at an endpoint the contract omits would route clients to a
	// surface the breaking-change gate cannot protect.
	declared := specOperations(t)
	for path := range seen {
		if _, ok := declared[path]; !ok {
			t.Errorf("GET / routes example advertises %q, which "+
				"api/aicr/v1/server.yaml does not declare as a path", path)
		}
	}

	actual := discoveryRoutes(t)
	if len(actual) == 0 {
		t.Fatal("GET / advertised no routes; the response shape changed and " +
			"the comparison below would be meaningless")
	}

	for path := range actual {
		if !seen[path] {
			t.Errorf("GET / serves %q but the spec's routes example omits it; a "+
				"client reading the generated docs would not know the route "+
				"exists", path)
		}
	}
	for path := range seen {
		if !actual[path] {
			t.Errorf("GET / routes example advertises %q but the handler does "+
				"not return it; the generated docs promise a route discovery "+
				"never mentions", path)
		}
	}
}

// rootRoutesExample returns the routes example the spec publishes for GET /.
func rootRoutesExample(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(specRelPath))
	if err != nil {
		t.Fatalf("read spec %q: %v", specRelPath, err)
	}

	var spec struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Properties struct {
								Routes struct {
									Example []string `yaml:"example"`
								} `yaml:"routes"`
							} `yaml:"properties"`
						} `yaml:"schema"`
					} `yaml:"content"`
				} `yaml:"responses"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	root, ok := spec.Paths["/"]
	if !ok {
		t.Fatal(`spec declares no "/" path; the root discovery endpoint is part ` +
			"of the frozen REST surface")
	}
	res, ok := root.Get.Responses["200"]
	if !ok {
		t.Fatal("spec declares no 200 response for GET /")
	}
	return res.Content["application/json"].Schema.Properties.Routes.Example
}

// discoveryRoutes returns the route set GET / actually advertises at runtime.
func discoveryRoutes(t *testing.T) map[string]bool {
	t.Helper()

	mux := newSpecTestServer(t).httpServer.Handler
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d; body: %s",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Routes []string `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET / body: %v; body: %s", err, rec.Body.String())
	}

	routes := make(map[string]bool, len(body.Routes))
	for _, path := range body.Routes {
		routes[path] = true
	}
	return routes
}

// missingKeys returns the keys present in want but absent from got, sorted.
func missingKeys(want, got map[string]yaml.Node) []string {
	var missing []string
	for name := range want {
		if _, ok := got[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// TestOpenAPIHeadMirrorsGetResponses asserts each HEAD operation declares the
// same statuses and headers as the GET it shadows.
//
// The handlers route HEAD through the GET resolution path, so every status GET
// can produce, HEAD can produce: a missing selector still yields 404 on
// /v1/query, and both endpoints can still time out. Declaring a narrower set
// leaves a generated client unable to model responses the server really sends,
// and the two lists drift apart the first time a status is added to GET alone.
func TestOpenAPIHeadMirrorsGetResponses(t *testing.T) {
	data, err := os.ReadFile(filepath.Clean(specRelPath))
	if err != nil {
		t.Fatalf("read spec %q: %v", specRelPath, err)
	}

	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Headers map[string]yaml.Node `yaml:"headers"`
				Content map[string]yaml.Node `yaml:"content"`
			} `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	var checked int
	for path, item := range spec.Paths {
		head, hasHead := item["head"]
		get, hasGet := item["get"]
		if !hasHead || !hasGet {
			continue
		}
		checked++

		for status, getResponse := range get.Responses {
			headResponse, declared := head.Responses[status]
			if !declared {
				t.Errorf("GET %s declares response %s but HEAD does not; the "+
					"handlers share a resolution path, so HEAD can return it too",
					path, status)
				continue
			}

			// Comparing the full header-name sets, not just "HEAD declares
			// some headers". A HEAD 200 carrying only X-Request-Id would have
			// passed an emptiness check while omitting Cache-Control and the
			// rate-limit headers -- the ones a caller issues HEAD to read.
			if missing := missingKeys(getResponse.Headers, headResponse.Headers); len(missing) > 0 {
				t.Errorf("HEAD %s response %s omits header(s) %s that GET declares; "+
					"HEAD exists to return exactly these", path, status,
					strings.Join(missing, ", "))
			}
			if extra := missingKeys(headResponse.Headers, getResponse.Headers); len(extra) > 0 {
				t.Errorf("HEAD %s response %s declares header(s) %s that GET does "+
					"not; the two describe one resolution path", path, status,
					strings.Join(extra, ", "))
			}
		}

		// The reverse direction: a status only HEAD declares describes a
		// response the shared handler cannot produce.
		for status := range head.Responses {
			if _, declared := get.Responses[status]; !declared {
				t.Errorf("HEAD %s declares response %s but GET does not; both run "+
					"the same resolution path", path, status)
			}
		}

		// A HEAD response carrying content contradicts the method.
		for status, headResponse := range head.Responses {
			if len(headResponse.Content) > 0 {
				t.Errorf("HEAD %s response %s declares content; HEAD returns no "+
					"body", path, status)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no path declares both GET and HEAD; this assertion would pass " +
			"vacuously")
	}
}

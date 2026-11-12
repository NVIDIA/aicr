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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// The traffic phase of the inference-gateway check mirrors the upstream
// kubernetes-sigs/ai-conformance TestGatewayAPIInference: two CPU backends
// that answer with their own name, one HTTPRoute whose first rule sends
// requests carrying the canary header to the alternate backend and whose
// default rule splits the rest by weight, and a probe pod that counts which
// backend answered. Unlike upstream, which creates its own Gateway, the
// route attaches to the Gateway the recipe's inference gateway profile names,
// so the data plane under test is the one AICR deployed. The test prefix
// names the per-run namespace, by which leftovers from interrupted runs are
// found; the "other" key is the probe's count of requests that failed or were
// answered by neither test backend; the hostname prefix and suffix, around
// the run token, form the route's one hostname, which the probe sends as the
// Host header of every request, and the path prefix, suffixed with the run
// token, is the path match on both rules. Gateway API ranks routes on a
// listener by hostname, then longest path, then method, then header matches,
// and a route with no hostname matches any Host, so a rule with neither
// would lose every request, header-matched ones included, to a pre-existing
// route such as a tenant's "/v1" route on the recipe's Gateway; with the
// per-run hostname, a tenant route with another hostname does not match the
// probe's requests at all, a hostname-less one ranks below the exact
// hostname, and only a route carrying the identical random hostname could
// tie. The request path appended to the path prefix only makes the traffic
// look like inference, since the backends answer every path; the sample
// error bound caps how many probe error samples the verdict carries; the
// interpreter runs both scripts in defaults.PythonProbeImage.
const (
	gatewayTrafficTestPrefix      = "gateway-traffic-"
	gatewayTrafficHostPrefix      = "ai-conformance-"
	gatewayTrafficHostSuffix      = ".conformance.invalid"
	gatewayTrafficPathPrefix      = "/ai-conformance-"
	gatewayTrafficRouteName       = "ai-conformance-route"
	gatewayTrafficPrimary         = "model-v1"
	gatewayTrafficAlternate       = "model-v2"
	gatewayTrafficProbeName       = "gateway-probe"
	gatewayTrafficOtherKey        = "other"
	gatewayTrafficBackendPort     = 8080
	gatewayTrafficPortName        = "http"
	gatewayTrafficServerName      = "server"
	gatewayTrafficProbeContainer  = "probe"
	gatewayTrafficHeaderName      = "OpenAI-Project"
	gatewayTrafficHeaderValue     = "ai-conformance-canary"
	gatewayTrafficRequestPath     = "/v1/chat/completions"
	gatewayTrafficMaxSampleErrors = 3
	gatewayTrafficInterpreter     = "python3"
)

// capabilityAll is the Linux capability set the test pods drop entirely.
const capabilityAll corev1.Capability = "ALL"

// gatewayTrafficPhase names the step of the traffic phase that produced a
// verdict.
type gatewayTrafficPhase string

const (
	gatewayTrafficPhaseDeploy  gatewayTrafficPhase = "deploy"
	gatewayTrafficPhaseRoute   gatewayTrafficPhase = "route"
	gatewayTrafficPhaseProbe   gatewayTrafficPhase = "probe"
	gatewayTrafficPhaseVerdict gatewayTrafficPhase = "verdict"
)

// gatewayTrafficCounts is one probe round: requests answered per backend
// name, plus gatewayTrafficOtherKey.
type gatewayTrafficCounts map[string]int

// gatewayTrafficBackendScript answers every request with the backend's name,
// so the probe can tell which backend the gateway chose.
const gatewayTrafficBackendScript = `import http.server, os, socket
NAME = os.environ["BACKEND"].encode()
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def reply(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(NAME)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(NAME)
    do_GET = do_POST = reply
    def log_message(self, *args):
        pass
class Server(http.server.ThreadingHTTPServer):
    address_family = socket.AF_INET6 if socket.has_dualstack_ipv6() else socket.AF_INET
    request_queue_size = 128
    def server_bind(self):
        if self.address_family == socket.AF_INET6:
            self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
        super().server_bind()
Server(("::" if Server.address_family == socket.AF_INET6 else "", int(os.environ["PORT"])), H).serve_forever()
`

// gatewayTrafficProbeScript sends every request with the route's hostname as
// Host, waits until the route is live (CONSECUTIVE consecutive header and
// default requests each answered by a test backend, both backends seen;
// which backend answered the header request is the verdict's question, not
// readiness's, so a Gateway that ignores the header match fails at the
// verdict with the header rule named rather than here as not routed), then
// prints one "HEADER name=n ... other=n" line per
// header round, stopping at the first without errors, and one "WEIGHTED ..."
// line per weighted round, stopping at the first within tolerance. The Go
// side re-evaluates every printed line; the early stops only save time.
const gatewayTrafficProbeScript = `import os, sys, time, urllib.error, urllib.request
from concurrent.futures import ThreadPoolExecutor
URL, PRIMARY, ALTERNATE = os.environ["URL"], os.environ["PRIMARY"], os.environ["ALTERNATE"]
HEADER = {os.environ["HEADER_NAME"]: os.environ["HEADER_VALUE"]}
BODY = b'{"model":"ai-conformance","messages":[{"role":"user","content":"hello"}]}'
def call(extra):
    req = urllib.request.Request(URL, data=BODY, headers={"Content-Type": "application/json", "Host": os.environ["HOST_HEADER"], **extra})
    err = None
    for _ in range(3):
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return r.read().decode().strip()
        except urllib.error.HTTPError as e:
            return "error: %s" % e
        except Exception as e:
            err = e
    return "error: %s" % err
def counts(kind, n, extra):
    with ThreadPoolExecutor(10) as ex:
        res = list(ex.map(lambda _: call(extra), range(n)))
    primary, alternate = res.count(PRIMARY), res.count(ALTERNATE)
    other = [r for r in res if r not in (PRIMARY, ALTERNATE)]
    print("%s %s=%d %s=%d other=%d" % (kind, PRIMARY, primary, ALTERNATE, alternate, len(other)), flush=True)
    if other:
        print("SAMPLE_ERROR %s" % other[0], flush=True)
    return primary, alternate, len(other)
deadline = time.time() + float(os.environ["READY_SECONDS"])
seen, ok = set(), 0
while True:
    h, w = call(HEADER), call({})
    seen.add(w)
    ok = ok + 1 if h in (PRIMARY, ALTERNATE) and w in (PRIMARY, ALTERNATE) else 0
    if ok >= int(os.environ["CONSECUTIVE"]) and {PRIMARY, ALTERNATE} <= seen:
        break
    if time.time() > deadline:
        print("NOT_READY after %ss header=%r default=%r seen=%r" % (os.environ["READY_SECONDS"], h, w, sorted(seen)), flush=True)
        sys.exit(1)
    time.sleep(2)
for _ in range(int(os.environ["ROUNDS"])):
    if counts("HEADER", int(os.environ["HEADER_REQUESTS"]), HEADER)[2] == 0:
        break
    time.sleep(5)
for _ in range(int(os.environ["ROUNDS"])):
    primary, alternate, other = counts("WEIGHTED", int(os.environ["REQUESTS"]), {})
    if other == 0 and primary + alternate > 0 and abs(primary / (primary + alternate) - float(os.environ["PRIMARY_SHARE"])) <= float(os.environ["TOLERANCE"]):
        break
    time.sleep(5)
`

// gatewayTrafficRunner runs the traffic phase against the Gateway the status
// checks read; CheckInferenceGateway uses runGatewayTrafficPhase and tests
// substitute one.
type gatewayTrafficRunner func(ctx *validators.Context, profile *validators.InferenceGatewayProfile, gw *unstructured.Unstructured) error

// runGatewayTrafficPhase bounds the traffic phase to the check's work budget,
// resolves the gateway endpoint before creating anything, and runs the
// scenario with its cleanup.
func runGatewayTrafficPhase(ctx *validators.Context, profile *validators.InferenceGatewayProfile, gw *unstructured.Unstructured) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available for the gateway traffic phase")
	}
	// Bound all work so one bounded namespace cleanup plus the skew/flush
	// margin fit inside the check's deadline (see gpuCheckWorkBudget).
	budget, err := gpuCheckWorkBudget(ctx.Ctx, inferenceGatewayWorkBudget)
	if err != nil {
		return err
	}
	workCtx, cancelWork := ctx.Timeout(budget)
	defer cancelWork()
	work := *ctx
	work.Ctx = workCtx
	ctx = &work

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	s, err := newGatewayTrafficScenario(ctx.Clientset, dynClient, profile, gw, ctx.Tolerations)
	if err != nil {
		return err
	}
	// Resolve the endpoint BEFORE creating anything: a Gateway with no plain
	// HTTP listener or no reachable proxy is a configuration finding, not a
	// routing failure, and must not leave test resources behind.
	if err := s.resolveEndpoint(ctx.Ctx); err != nil {
		return err
	}
	return s.runWithCleanup(ctx)
}

// gatewayTrafficScenario drives one deploy, route, probe and verdict cycle.
// The log reader and the timing fields are injectable so unit tests can drive
// the cycle against fake API servers.
type gatewayTrafficScenario struct {
	clientset kubernetes.Interface
	dynClient dynamic.Interface
	profile   *validators.InferenceGatewayProfile
	gw        *unstructured.Unstructured
	testRun   *gpuTestRun
	// tolerations is applied to every pod the phase creates; nil means
	// tolerate-all, as the other conformance probes do.
	tolerations []corev1.Toleration

	// endpoint is the URL the probe posts to, resolved from the profile's
	// data plane; listenerName and listenerPort are the Gateway HTTP listener
	// it targets, which the HTTPRoute's parentRef pins as its sectionName so
	// the Accepted wait judges that listener and not another the Gateway may
	// also attach the route to; routeHostname is the HTTPRoute's one
	// hostname, sent as the probe's Host header; routePath is the per-run
	// path prefix both rules match.
	endpoint      string
	listenerName  string
	listenerPort  int64
	routeHostname string
	routePath     string
	// attachedRoutes are the HTTPRoutes already attached to the Gateway when
	// the phase ran, named in the verdict because precedence between routes
	// on the listener is the first thing to rule out when requests reached a
	// non-test backend; attachedRoutesErr records a listing that could not
	// be read.
	attachedRoutes    []string
	attachedRoutesErr error

	readProbeLogs func(ctx context.Context) (string, error)

	backendReadyTimeout time.Duration
	routeAcceptTimeout  time.Duration
	routeReadyWindow    time.Duration
	probeTimeout        time.Duration
	headerRequests      int
	requests            int
	rounds              int
	consecutive         int
	primaryWeight       int
	alternateWeight     int
	tolerance           float64

	phase    gatewayTrafficPhase
	header   gatewayTrafficCounts
	weighted []gatewayTrafficCounts
	// sampleErrors are the probe's SAMPLE_ERROR lines, one per round with
	// requests that reached no test backend.
	sampleErrors []string
	timings      map[gatewayTrafficPhase]time.Duration
}

func newGatewayTrafficScenario(clientset kubernetes.Interface, dynClient dynamic.Interface,
	profile *validators.InferenceGatewayProfile, gw *unstructured.Unstructured, tolerations []corev1.Toleration) (*gatewayTrafficScenario, error) {

	run, err := newGPUTestRun()
	if err != nil {
		return nil, err
	}
	run.namespace = gatewayTrafficTestPrefix + run.token
	// The documented listener contract admits routes from namespaces labeled
	// app.kubernetes.io/component=inference-perf (the performance
	// validator's per-run namespaces), so the traffic namespace carries that
	// label too; labels.ManagedBy is deliberately absent, since the
	// performance validator's stale-namespace prune selects on both.
	run.namespaceLabels = map[string]string{labels.Component: labels.ValueInferencePerf}
	s := &gatewayTrafficScenario{
		clientset:           clientset,
		dynClient:           dynClient,
		profile:             profile,
		gw:                  gw,
		testRun:             run,
		tolerations:         tolerations,
		routeHostname:       gatewayTrafficHostPrefix + run.token + gatewayTrafficHostSuffix,
		routePath:           gatewayTrafficPathPrefix + run.token,
		backendReadyTimeout: defaults.GatewayTrafficBackendReadyTimeout,
		routeAcceptTimeout:  defaults.GatewayTrafficRouteAcceptTimeout,
		routeReadyWindow:    defaults.GatewayTrafficRouteReadyWindow,
		probeTimeout:        defaults.GatewayTrafficProbeTimeout,
		headerRequests:      defaults.GatewayTrafficHeaderRequests,
		requests:            defaults.GatewayTrafficRequests,
		rounds:              defaults.GatewayTrafficRounds,
		consecutive:         defaults.GatewayTrafficConsecutive,
		primaryWeight:       defaults.GatewayTrafficPrimaryWeight,
		alternateWeight:     defaults.GatewayTrafficAlternateWeight,
		tolerance:           defaults.GatewayTrafficTolerance,
		timings:             map[gatewayTrafficPhase]time.Duration{},
	}
	s.readProbeLogs = func(c context.Context) (string, error) {
		raw, logErr := clientset.CoreV1().Pods(run.namespace).GetLogs(gatewayTrafficProbeName,
			&corev1.PodLogOptions{Container: gatewayTrafficProbeContainer}).DoRaw(c)
		if logErr != nil {
			return "", errors.Wrap(errors.ErrCodeInternal, "failed to read traffic probe logs", logErr)
		}
		return string(raw), nil
	}
	return s, nil
}

// primaryShare is the share of unmatched requests the primary backend should
// answer, from the two weights.
func (s *gatewayTrafficScenario) primaryShare() float64 {
	return float64(s.primaryWeight) / float64(s.primaryWeight+s.alternateWeight)
}

// resolveEndpoint picks the Gateway's plain HTTP listener and the address the
// probe reaches it at: for a Service-backed data plane the proxy Service's
// in-cluster name on the port that serves the listener, for an address-backed
// one the Gateway's first IP or hostname status address on the listener port.
// A hostname-bound listener is refused, as the performance validator refuses
// it: the route's per-run .conformance.invalid hostname never intersects a
// listener hostname, so such a listener would never accept the route.
func (s *gatewayTrafficScenario) resolveEndpoint(ctx context.Context) error {
	name, port, err := gatewayHTTPListener(s.gw, s.profile)
	if err != nil {
		return err
	}
	s.listenerName, s.listenerPort = name, port
	if s.profile.Probe == validators.GatewayProbeGatewayAddress {
		host := firstReachableGatewayAddress(s.gw)
		if host == "" {
			return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"Gateway %s/%s reports no IPAddress or Hostname status address for the traffic probe to reach",
				s.profile.Namespace, s.profile.GatewayName))
		}
		s.endpoint = "http://" + net.JoinHostPort(host, strconv.FormatInt(port, 10)) + s.routePath + gatewayTrafficRequestPath
		return nil
	}
	svcs, err := s.clientset.CoreV1().Services(s.profile.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return classifyK8sReadError(err, "Services in "+s.profile.Namespace)
	}
	var selected *corev1.Service
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if svc.Name == s.profile.GatewayName {
			selected = svc
			break
		}
		if selected == nil && s.profile.IsProxyName(svc.Name) {
			selected = svc
		}
	}
	if selected == nil {
		return errors.New(errors.ErrCodeNotFound, fmt.Sprintf(
			"no Service in %s is named %q or %q-*: the %s proxy Service the traffic probe would reach was not found",
			s.profile.Namespace, s.profile.GatewayName, s.profile.GatewayName, s.profile.Component))
	}
	svcPort := port
	if len(selected.Spec.Ports) > 0 {
		svcPort = int64(selected.Spec.Ports[0].Port)
		for _, p := range selected.Spec.Ports {
			if int64(p.Port) == port {
				svcPort = port
				break
			}
		}
	}
	s.endpoint = fmt.Sprintf("http://%s.%s.svc:%d%s%s", selected.Name, selected.Namespace, svcPort, s.routePath, gatewayTrafficRequestPath)
	return nil
}

// gatewayHTTPListener returns the name and port of the Gateway's first HTTP
// listener without a hostname. The route the phase creates carries a per-run
// hostname under .conformance.invalid, which no listener hostname can
// intersect, so a listener bound to one would never accept it.
func gatewayHTTPListener(gw *unstructured.Unstructured, profile *validators.InferenceGatewayProfile) (string, int64, error) {
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	var (
		protocols     []string
		hostnameBound []string
	)
	for _, l := range listeners {
		lMap, ok := l.(map[string]any)
		if !ok {
			continue
		}
		protocol, _, _ := unstructured.NestedString(lMap, "protocol")
		name, _, _ := unstructured.NestedString(lMap, "name")
		protocols = append(protocols, name+"="+valueOrUnknown(protocol))
		if protocol != "HTTP" {
			continue
		}
		if hostname, _, _ := unstructured.NestedString(lMap, "hostname"); hostname != "" {
			hostnameBound = append(hostnameBound, name+"="+hostname)
			continue
		}
		port, found, _ := unstructured.NestedInt64(lMap, "port")
		if !found || port <= 0 || name == "" {
			continue
		}
		return name, port, nil
	}
	if len(hostnameBound) > 0 {
		return "", 0, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"Gateway %s/%s HTTP listener(s) set a hostname (%s); the traffic route uses a per-run .conformance.invalid hostname that a hostname-bound listener never matches, so it needs a hostname-free HTTP listener",
			profile.Namespace, profile.GatewayName, strings.Join(hostnameBound, ", ")))
	}
	return "", 0, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
		"Gateway %s/%s has no HTTP listener (listeners: %s); the traffic probe speaks plain HTTP",
		profile.Namespace, profile.GatewayName, valueOrUnknown(strings.Join(protocols, ", "))))
}

// firstReachableGatewayAddress returns the first status address a pod can
// connect to by value: an IPAddress, a Hostname, or one with no type.
func firstReachableGatewayAddress(gw *unstructured.Unstructured) string {
	addresses, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	for _, a := range addresses {
		aMap, ok := a.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(aMap, "type")
		if typ != "" && typ != "IPAddress" && typ != "Hostname" {
			continue
		}
		if value, _, _ := unstructured.NestedString(aMap, "value"); value != "" {
			return value
		}
	}
	return ""
}

// runWithCleanup creates the per-run namespace, runs the cycle, and always
// tears down the HTTPRoute and the namespace. The return is NAMED so a
// terminally failed cleanup fails an otherwise-passing check; a primary error
// always wins.
//
// The teardown is deferred BEFORE the namespace create: a Create that
// returns an error may still have persisted the namespace, and only a
// cleanup registered first can reconcile it (see cleanupGPUTestNamespace).
func (s *gatewayTrafficScenario) runWithCleanup(ctx *validators.Context) (err error) {
	defer func() {
		status, cleanupErr := cleanupGatewayTrafficResources(s.clientset, s.dynClient, s.testRun)
		recordRawTextArtifact(ctx, "Delete test namespace", cleanupInspectEquivalent(s.testRun.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = createGPUTestNamespace(ctx.Ctx, s.clientset, s.testRun); err != nil {
		return err
	}

	err = s.run(ctx)
	recordRawTextArtifact(ctx, "Gateway traffic verdict", "", s.verdictSummary(err))
	if err != nil {
		return wrapWithCode(err, errors.ErrCodeInternal, fmt.Sprintf("gateway traffic phase failed in step %s", s.phase))
	}
	return nil
}

func (s *gatewayTrafficScenario) run(ctx *validators.Context) error {
	s.phase = gatewayTrafficPhaseDeploy
	if err := s.deploy(ctx); err != nil {
		return err
	}
	s.phase = gatewayTrafficPhaseRoute
	if err := s.attachRoute(ctx); err != nil {
		return err
	}
	s.phase = gatewayTrafficPhaseProbe
	if err := s.probe(ctx); err != nil {
		return err
	}
	s.phase = gatewayTrafficPhaseVerdict
	return s.judge()
}

// attachedRoutesHint names the routes that shared the Gateway when the probe
// ran, the first thing to look at when requests reached a non-test backend.
func (s *gatewayTrafficScenario) attachedRoutesHint() string {
	switch {
	case s.attachedRoutesErr != nil:
		return fmt.Sprintf("; the HTTPRoutes already attached to Gateway %s/%s could not be listed (%v)",
			s.profile.Namespace, s.profile.GatewayName, s.attachedRoutesErr)
	case len(s.attachedRoutes) > 0:
		return fmt.Sprintf("; HTTPRoutes already attached to Gateway %s/%s when the probe ran: [%s] (only a route on the test route's exact hostname and path can take precedence over it)",
			s.profile.Namespace, s.profile.GatewayName, strings.Join(s.attachedRoutes, ", "))
	}
	return ""
}

// verdictSummary renders the counts the evidence page and the CTRF detail
// carry: the header round, every weighted round with the primary share, and
// the step that decided.
func (s *gatewayTrafficScenario) verdictSummary(err error) string {
	var sb strings.Builder
	if err == nil {
		fmt.Fprintf(&sb, "Verdict:            PASS (all steps completed; last step: %s)\n", s.phase)
	} else {
		fmt.Fprintf(&sb, "Verdict:            FAIL in step %s\nError:              %v\n", s.phase, err)
	}
	fmt.Fprintf(&sb, "Gateway:            %s/%s (listener %s, port %d)\nEndpoint:           %s\nHTTPRoute:          %s/%s (hostname %s, path prefix %s, sectionName %s; the probe sends the hostname as Host)\n",
		s.profile.Namespace, s.profile.GatewayName, valueOrUnknown(s.listenerName), s.listenerPort, valueOrUnknown(s.endpoint), s.testRun.namespace, gatewayTrafficRouteName, s.routeHostname, s.routePath, valueOrUnknown(s.listenerName))
	switch {
	case s.attachedRoutesErr != nil:
		fmt.Fprintf(&sb, "Attached routes:    unavailable (%v)\n", s.attachedRoutesErr)
	case len(s.attachedRoutes) > 0:
		fmt.Fprintf(&sb, "Attached routes:    %s (already on the Gateway before the test route)\n", strings.Join(s.attachedRoutes, ", "))
	default:
		sb.WriteString("Attached routes:    none before the test route\n")
	}
	fmt.Fprintf(&sb, "Header rule:        %s: %s -> %s\nDefault rule:       %s %d / %s %d (tolerance %.0f points)\n",
		gatewayTrafficHeaderName, gatewayTrafficHeaderValue, gatewayTrafficAlternate,
		gatewayTrafficPrimary, s.primaryWeight, gatewayTrafficAlternate, s.alternateWeight, s.tolerance*100)
	if s.header != nil {
		fmt.Fprintf(&sb, "Header requests:    %s (want %s=%d)\n", s.header.String(), gatewayTrafficAlternate, s.headerRequests)
	} else {
		sb.WriteString("Header requests:    not measured\n")
	}
	if len(s.weighted) == 0 {
		sb.WriteString("Weighted rounds:    not measured\n")
	}
	for i, r := range s.weighted {
		fmt.Fprintf(&sb, "Weighted round %-2d   %s (%s share %s, want %.0f%%)\n", i+1, r.String(), gatewayTrafficPrimary,
			r.primaryShareText(), s.primaryShare()*100)
	}
	for _, sample := range s.sampleErrors {
		fmt.Fprintf(&sb, "Sample error:       %s\n", sample)
	}
	for _, phase := range []gatewayTrafficPhase{gatewayTrafficPhaseDeploy, gatewayTrafficPhaseRoute, gatewayTrafficPhaseProbe} {
		if d, ok := s.timings[phase]; ok {
			fmt.Fprintf(&sb, "Step %-14s %s\n", string(phase)+":", d.Round(time.Second))
		}
	}
	return sb.String()
}

// String renders the counts in a fixed order: primary, alternate, other.
func (c gatewayTrafficCounts) String() string {
	return fmt.Sprintf("%s=%d %s=%d %s=%d", gatewayTrafficPrimary, c[gatewayTrafficPrimary],
		gatewayTrafficAlternate, c[gatewayTrafficAlternate], gatewayTrafficOtherKey, c[gatewayTrafficOtherKey])
}

// primaryShareText is the primary backend's share of the requests a test
// backend answered, or "n/a" when none did.
func (c gatewayTrafficCounts) primaryShareText() string {
	total := c[gatewayTrafficPrimary] + c[gatewayTrafficAlternate]
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(c[gatewayTrafficPrimary])/float64(total)*100)
}

// deploy creates both backends and their Services, then waits for both
// Deployments to be available.
func (s *gatewayTrafficScenario) deploy(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	for _, name := range []string{gatewayTrafficPrimary, gatewayTrafficAlternate} {
		deploy, svc := buildGatewayTrafficBackend(ns, name, s.tolerations, s.testRun.imagePullSecretRefs())
		if _, err := s.clientset.AppsV1().Deployments(ns).Create(ctx.Ctx, deploy, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create backend Deployment "+name, err)
		}
		if _, err := s.clientset.CoreV1().Services(ns).Create(ctx.Ctx, svc, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create backend Service "+name, err)
		}
	}
	recordRawTextArtifact(ctx, "Gateway traffic: test backends",
		fmt.Sprintf("kubectl get deploy,svc -n %s", ns),
		fmt.Sprintf("Namespace:   %s (label %s=%s, as the documented listener selector admits)\nBackends:    %s, %s (Deployment + Service each, 1 replica, image %s, no accelerator, port %s/%d)\nEach backend answers every request with its own name.",
			ns, labels.Component, labels.ValueInferencePerf, gatewayTrafficPrimary, gatewayTrafficAlternate, defaults.PythonProbeImage, gatewayTrafficPortName, gatewayTrafficBackendPort))

	for _, name := range []string{gatewayTrafficPrimary, gatewayTrafficAlternate} {
		if _, err := waitForDeploymentAvailable(ctx, ns, name, s.backendReadyTimeout); err != nil {
			return wrapWithCode(err, errors.ErrCodeTimeout, "backend "+name+" did not become available")
		}
	}
	s.timings[gatewayTrafficPhaseDeploy] = time.Since(start)
	return nil
}

// attachRoute creates the HTTPRoute on the profile's Gateway, pinned to the
// listener the probe targets, and waits until that Gateway reports it
// Accepted with ResolvedRefs on that listener.
func (s *gatewayTrafficScenario) attachRoute(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	s.recordAttachedRoutes(ctx)
	route := buildGatewayTrafficHTTPRoute(ns, s.profile, s.listenerName, s.routeHostname, s.routePath, s.primaryWeight, s.alternateWeight)
	recordObjectYAMLArtifact(ctx, "Gateway traffic: HTTPRoute manifest",
		fmt.Sprintf("kubectl apply -n %s -f <httproute>", ns), route.Object)
	if _, err := s.dynClient.Resource(httpRouteGVR).Namespace(ns).Create(ctx.Ctx, route, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"failed to create HTTPRoute %s/%s on Gateway %s/%s", ns, gatewayTrafficRouteName, s.profile.Namespace, s.profile.GatewayName), err)
	}
	observed, err := waitForHTTPRouteAccepted(ctx.Ctx, s.dynClient, ns, s.profile, s.listenerName, s.routeAcceptTimeout)
	status := "HTTPRoute status not observed"
	if observed != nil {
		status = httpRouteParentStatusText(observed, s.profile)
	}
	recordRawTextArtifact(ctx, "Gateway traffic: HTTPRoute status",
		fmt.Sprintf("kubectl get httproute %s -n %s -o jsonpath='{.status.parents}'", gatewayTrafficRouteName, ns),
		fmt.Sprintf("%s\nElapsed: %s", status, time.Since(start).Round(time.Second)))
	if err != nil {
		return err
	}
	s.timings[gatewayTrafficPhaseRoute] = time.Since(start)
	return nil
}

// recordAttachedRoutes lists the HTTPRoutes already attached to the Gateway
// and records them, as a warning when there are any: they share the listener
// with the test route, and the evidence page must show what the probe's
// requests competed with. A listing that fails is recorded as unavailable,
// not as no routes.
func (s *gatewayTrafficScenario) recordAttachedRoutes(ctx *validators.Context) {
	cmd := attachedHTTPRoutesCommand(s.profile)
	s.attachedRoutes, s.attachedRoutesErr = attachedHTTPRouteNames(ctx.Ctx, s.dynClient, s.profile)
	switch {
	case s.attachedRoutesErr != nil:
		recordRawTextArtifact(ctx, "Gateway traffic: routes already attached", cmd,
			fmt.Sprintf("unavailable: %v", s.attachedRoutesErr))
	case len(s.attachedRoutes) > 0:
		slog.Warn("HTTPRoutes already attached to the inference gateway share the listener with the traffic test route",
			"gateway", s.profile.GatewayName, "routes", s.attachedRoutes)
		recordRawTextArtifact(ctx, "Gateway traffic: routes already attached WARNING", cmd,
			fmt.Sprintf("WARNING: %d HTTPRoute(s) already attached to Gateway %s/%s: %s\n"+
				"The test route carries the per-run hostname %s (sent as the probe's Host header) and path %s, so a route with another hostname does not match the probe's requests and a hostname-less one ranks below it; only a pre-existing route on the identical hostname and path could answer the probe's requests, which fails the check as a non-test backend.",
				len(s.attachedRoutes), s.profile.Namespace, s.profile.GatewayName, strings.Join(s.attachedRoutes, ", "), s.routeHostname, s.routePath))
	default:
		recordRawTextArtifact(ctx, "Gateway traffic: routes already attached", cmd,
			fmt.Sprintf("none: no HTTPRoute was attached to Gateway %s/%s before the test route", s.profile.Namespace, s.profile.GatewayName))
	}
}

// attachedHTTPRoutesCommand is the kubectl/jq equivalent of
// attachedHTTPRouteNames, recorded beside its result: the same parentRef
// test, a parentRef that names no namespace defaulting to the route's own.
func attachedHTTPRoutesCommand(profile *validators.InferenceGatewayProfile) string {
	return fmt.Sprintf("kubectl get httproute -A -o json | jq '.items[] | select(.metadata.namespace as $ns | any(.spec.parentRefs[]?; .name == %q and (.namespace // $ns) == %q))'",
		profile.GatewayName, profile.Namespace)
}

// attachedHTTPRouteNames lists every HTTPRoute whose parentRefs name the
// profile's Gateway (httpRouteTargetsGateway), as namespace/name, sorted.
func attachedHTTPRouteNames(ctx context.Context, dyn dynamic.Interface, profile *validators.InferenceGatewayProfile) ([]string, error) {
	list, err := dyn.Resource(httpRouteGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, classifyK8sReadError(err, "HTTPRoutes")
	}
	var names []string
	for i := range list.Items {
		route := &list.Items[i]
		if httpRouteTargetsGateway(route, profile) {
			names = append(names, route.GetNamespace()+"/"+route.GetName())
		}
	}
	sort.Strings(names)
	return names, nil
}

// httpRouteTargetsGateway reports whether any of the route's parentRefs
// names the profile's Gateway. A parentRef names its Gateway's namespace
// only when it differs from the route's own, so one without a namespace is
// resolved in the route's namespace and a same-named Gateway elsewhere does
// not count.
func httpRouteTargetsGateway(route *unstructured.Unstructured, profile *validators.InferenceGatewayProfile) bool {
	parentRefs, _, _ := unstructured.NestedSlice(route.Object, keySpec, "parentRefs")
	for _, ref := range parentRefs {
		refMap, ok := ref.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(refMap, keyName)
		namespace, _, _ := unstructured.NestedString(refMap, keyNamespace)
		if namespace == "" {
			namespace = route.GetNamespace()
		}
		if name == profile.GatewayName && namespace == profile.Namespace {
			return true
		}
	}
	return false
}

// probe runs the probe pod to completion and parses its counts. The logs are
// recorded before any verdict so a failing probe still ships its diagnostics.
func (s *gatewayTrafficScenario) probe(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	pod := buildGatewayTrafficProbe(ns, s)
	if _, err := s.clientset.CoreV1().Pods(ns).Create(ctx.Ctx, pod, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create traffic probe pod", err)
	}
	recordRawTextArtifact(ctx, "Gateway traffic: probe",
		fmt.Sprintf("kubectl get pod %s -n %s", gatewayTrafficProbeName, ns),
		fmt.Sprintf("Endpoint:         %s\nHost header:      %s\nHeader requests:  %d with %s: %s\nWeighted rounds:  up to %d of %d requests each\nReady window:     %s (%d consecutive routed request pairs)",
			s.endpoint, s.routeHostname, s.headerRequests, gatewayTrafficHeaderName, gatewayTrafficHeaderValue, s.rounds, s.requests, s.routeReadyWindow, s.consecutive))

	result, waitErr := waitForTerminalPodWithin(ctx.Ctx, s.clientset, ns, gatewayTrafficProbeName, "traffic probe pod", s.probeTimeout)
	logs, logErr := s.readProbeLogs(ctx.Ctx)
	if logErr != nil {
		logs = logErr.Error()
	}
	recordRawTextArtifact(ctx, "Gateway traffic: probe logs",
		fmt.Sprintf("kubectl logs %s -n %s", gatewayTrafficProbeName, ns), logs)
	if waitErr != nil {
		return waitErr
	}
	if logErr != nil {
		return logErr
	}
	header, weighted, samples, parseErr := parseGatewayProbeLogs(logs)
	s.header, s.weighted, s.sampleErrors = header, weighted, samples
	if result.Status.Phase == corev1.PodFailed {
		return errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"traffic through Gateway %s/%s did not reach the test backends as routed within the %s ready window (probe exit code %d): %s",
			s.profile.Namespace, s.profile.GatewayName, s.routeReadyWindow, podExitCode(result), gatewayProbeNotReadyLine(logs)))
	}
	if parseErr != nil {
		return parseErr
	}
	s.timings[gatewayTrafficPhaseProbe] = time.Since(start)
	return nil
}

// judge applies the two assertions to the parsed counts; a failure names the
// routes that shared the Gateway, since precedence between them is the first
// thing to rule out.
func (s *gatewayTrafficScenario) judge() error {
	err := evaluateGatewayHeaderRouting(s.header, s.headerRequests)
	if err == nil {
		err = evaluateGatewayWeightedRounds(s.weighted, s.primaryShare(), s.tolerance)
	}
	if err == nil {
		return nil
	}
	if hint := s.attachedRoutesHint(); hint != "" {
		return errors.New(errors.ErrCodeInternal, err.Error()+hint)
	}
	return err
}

// evaluateGatewayHeaderRouting requires every header-matched request to have
// reached the alternate backend. A route whose header rule is missing sends
// those requests through the default split, so some reach the primary.
func evaluateGatewayHeaderRouting(header gatewayTrafficCounts, want int) error {
	if header == nil {
		return errors.New(errors.ErrCodeInternal, "probe logs carry no HEADER round")
	}
	if header[gatewayTrafficAlternate] != want || header[gatewayTrafficOtherKey] != 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"all %d requests with %s: %s must reach %s, got %s",
			want, gatewayTrafficHeaderName, gatewayTrafficHeaderValue, gatewayTrafficAlternate, header.String()))
	}
	return nil
}

// evaluateGatewayWeightedRounds passes when any round split the unmatched
// requests within tolerance of the weights with no request reaching a
// non-test backend, like the upstream test's retries. Swapped weights fail
// every round: the primary's share is then one minus what is expected.
func evaluateGatewayWeightedRounds(rounds []gatewayTrafficCounts, wantShare, tolerance float64) error {
	if len(rounds) == 0 {
		return errors.New(errors.ErrCodeInternal, "probe logs carry no WEIGHTED round")
	}
	var reasons []string
	for i, r := range rounds {
		finding := gatewayWeightedSplitFinding(r, wantShare, tolerance)
		if finding == "" {
			return nil
		}
		reasons = append(reasons, fmt.Sprintf("round %d: %s", i+1, finding))
	}
	return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
		"unmatched requests did not split %.0f/%.0f within %.0f points in %d round(s): %s",
		wantShare*100, (1-wantShare)*100, tolerance*100, len(rounds), strings.Join(reasons, "; ")))
}

// gatewayWeightedSplitFinding reports why one round does not satisfy the
// split, or "" when it does; the round verdict is composed by the caller.
func gatewayWeightedSplitFinding(r gatewayTrafficCounts, wantShare, tolerance float64) string {
	if other := r[gatewayTrafficOtherKey]; other > 0 {
		return fmt.Sprintf("%d request(s) failed or reached a non-test backend (%s)", other, r.String())
	}
	total := r[gatewayTrafficPrimary] + r[gatewayTrafficAlternate]
	if total == 0 {
		return "no request reached a test backend"
	}
	got := float64(r[gatewayTrafficPrimary]) / float64(total)
	if math.Abs(got-wantShare) > tolerance {
		return fmt.Sprintf("%s got %.1f%%, want %.0f%% (%s)", gatewayTrafficPrimary, got*100, wantShare*100, r.String())
	}
	return ""
}

// parseGatewayProbeLogs reads the HEADER lines (the last one counts, as the
// probe stops at the first clean round), every WEIGHTED line and the
// SAMPLE_ERROR lines printed by gatewayTrafficProbeScript. Counts for names
// other than the two backends and "other" are rejected: the probe prints
// only those, so anything else is not its output.
func parseGatewayProbeLogs(logs string) (header gatewayTrafficCounts, weighted []gatewayTrafficCounts, samples []string, err error) {
	known := map[string]bool{gatewayTrafficPrimary: true, gatewayTrafficAlternate: true, gatewayTrafficOtherKey: true}
	for _, line := range strings.Split(logs, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case "SAMPLE_ERROR":
			if len(samples) < gatewayTrafficMaxSampleErrors {
				samples = append(samples, rest)
			}
			continue
		case "HEADER", "WEIGHTED":
		default:
			continue
		}
		counts := gatewayTrafficCounts{}
		for _, f := range strings.Fields(rest) {
			k, v, ok := strings.Cut(f, "=")
			n, convErr := strconv.Atoi(v)
			if !ok || convErr != nil || !known[k] || n < 0 {
				return nil, nil, samples, errors.New(errors.ErrCodeInternal, fmt.Sprintf("malformed probe line %q", line))
			}
			counts[k] = n
		}
		if kind == "HEADER" {
			header = counts
		} else {
			weighted = append(weighted, counts)
		}
	}
	if header == nil || len(weighted) == 0 {
		return header, weighted, samples, errors.New(errors.ErrCodeInternal,
			"probe logs carry no HEADER or no WEIGHTED round; the probe did not finish counting")
	}
	return header, weighted, samples, nil
}

// gatewayProbeNotReadyLine returns the probe's NOT_READY line, or the last
// non-empty log line when there is none.
func gatewayProbeNotReadyLine(logs string) string {
	last := ""
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "NOT_READY") {
			return line
		}
		if line != "" {
			last = line
		}
	}
	return valueOrUnknown(last)
}

// gatewayTrafficPodSpec is the restricted CPU pod shape both backends and the
// probe use. tolerations, when nil, defaults to tolerate-all so the pods
// schedule on all-tainted clusters.
func gatewayTrafficPodSpec(container corev1.Container, tolerations []corev1.Toleration, pullSecrets []corev1.LocalObjectReference) corev1.PodSpec {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	container.Image = defaults.PythonProbeImage
	container.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	container.SecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
	}
	return corev1.PodSpec{
		Tolerations:      tolerations,
		ImagePullSecrets: pullSecrets,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			RunAsUser:      ptr.To(int64(65534)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
	}
}

// buildGatewayTrafficBackend returns one backend's Deployment and Service.
func buildGatewayTrafficBackend(ns, name string, tolerations []corev1.Toleration, pullSecrets []corev1.LocalObjectReference) (*appsv1.Deployment, *corev1.Service) {
	labels := map[string]string{hpaAppLabelKey: name}
	spec := gatewayTrafficPodSpec(corev1.Container{
		Name:    gatewayTrafficServerName,
		Command: []string{gatewayTrafficInterpreter, "-u", "-c", gatewayTrafficBackendScript},
		Env:     []corev1.EnvVar{{Name: "BACKEND", Value: name}, {Name: "PORT", Value: strconv.Itoa(gatewayTrafficBackendPort)}},
		Ports:   []corev1.ContainerPort{{Name: gatewayTrafficPortName, ContainerPort: gatewayTrafficBackendPort}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString(gatewayTrafficPortName)}},
			PeriodSeconds: 2,
		},
	}, tolerations, pullSecrets)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: gatewayTrafficPortName, Port: gatewayTrafficBackendPort, TargetPort: intstr.FromString(gatewayTrafficPortName)}},
		},
	}
	return deploy, svc
}

// buildGatewayTrafficHTTPRoute attaches to the profile's Gateway across
// namespaces (the parentRef names the Gateway's namespace, which the listener
// must allow through allowedRoutes) and pins the parentRef to the listener
// the probe targets through sectionName, so the Gateway cannot accept the
// route through another listener while the probe's requests arrive on this
// one. The route carries the per-run hostname
// and both rules match the per-run path prefix, so a pre-existing route can
// outrank it only on that exact hostname and path; the first rule
// additionally matches the canary header and sends to the alternate backend,
// the default rule splits all other requests by weight.
func buildGatewayTrafficHTTPRoute(ns string, profile *validators.InferenceGatewayProfile, listener, hostname, path string, primaryWeight, alternateWeight int) *unstructured.Unstructured {
	ref := func(name string, weight int) map[string]any {
		r := map[string]any{keyName: name, keyPort: int64(gatewayTrafficBackendPort)}
		if weight > 0 {
			r["weight"] = int64(weight)
		}
		return r
	}
	pathMatch := func() map[string]any {
		return map[string]any{"type": "PathPrefix", "value": path}
	}
	route := &unstructured.Unstructured{Object: map[string]any{
		keySpec: map[string]any{
			"parentRefs": []any{map[string]any{keyName: profile.GatewayName, keyNamespace: profile.Namespace, keySection: listener}},
			"hostnames":  []any{hostname},
			"rules": []any{
				map[string]any{
					"matches": []any{map[string]any{
						keyPath: pathMatch(),
						"headers": []any{map[string]any{
							"type": "Exact", keyName: gatewayTrafficHeaderName, "value": gatewayTrafficHeaderValue,
						}},
					}},
					"backendRefs": []any{ref(gatewayTrafficAlternate, 0)},
				},
				map[string]any{
					"matches":     []any{map[string]any{keyPath: pathMatch()}},
					"backendRefs": []any{ref(gatewayTrafficPrimary, primaryWeight), ref(gatewayTrafficAlternate, alternateWeight)},
				},
			},
		},
	}}
	route.SetAPIVersion(httpRouteGVR.GroupVersion().String())
	route.SetKind("HTTPRoute")
	route.SetName(gatewayTrafficRouteName)
	route.SetNamespace(ns)
	return route
}

// buildGatewayTrafficProbe is the probe pod. Its activeDeadlineSeconds caps
// its lifetime past the probe wait so a leaked pod ends on its own.
func buildGatewayTrafficProbe(ns string, s *gatewayTrafficScenario) *corev1.Pod {
	spec := gatewayTrafficPodSpec(corev1.Container{
		Name:    gatewayTrafficProbeContainer,
		Command: []string{gatewayTrafficInterpreter, "-u", "-c", gatewayTrafficProbeScript},
		Env: []corev1.EnvVar{
			{Name: "URL", Value: s.endpoint},
			{Name: "HOST_HEADER", Value: s.routeHostname},
			{Name: "PRIMARY", Value: gatewayTrafficPrimary},
			{Name: "ALTERNATE", Value: gatewayTrafficAlternate},
			{Name: "HEADER_NAME", Value: gatewayTrafficHeaderName},
			{Name: "HEADER_VALUE", Value: gatewayTrafficHeaderValue},
			{Name: "HEADER_REQUESTS", Value: strconv.Itoa(s.headerRequests)},
			{Name: "REQUESTS", Value: strconv.Itoa(s.requests)},
			{Name: "ROUNDS", Value: strconv.Itoa(s.rounds)},
			{Name: "CONSECUTIVE", Value: strconv.Itoa(s.consecutive)},
			{Name: "PRIMARY_SHARE", Value: strconv.FormatFloat(s.primaryShare(), 'f', -1, 64)},
			{Name: "TOLERANCE", Value: strconv.FormatFloat(s.tolerance, 'f', -1, 64)},
			{Name: "READY_SECONDS", Value: strconv.Itoa(int(s.routeReadyWindow / time.Second))},
		},
	}, s.tolerations, s.testRun.imagePullSecretRefs())
	spec.RestartPolicy = corev1.RestartPolicyNever
	lifetime := s.probeTimeout + defaults.K8sCleanupTimeout + gpuCheckSkewAndFlushMargin
	spec.ActiveDeadlineSeconds = ptr.To(int64(lifetime / time.Second))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayTrafficProbeName, Namespace: ns, Labels: map[string]string{hpaAppLabelKey: gatewayTrafficProbeName}},
		Spec:       spec,
	}
}

// httpRouteAcceptedByGateway reports whether the route's status carries a
// parent entry for the profile's Gateway on the given listener with
// Accepted=True and ResolvedRefs=True. A parentRef that names no namespace
// is the route's own, which is never the Gateway's here, so it is not
// matched; a parent entry for another listener, or for none, is not the
// pinned one and does not count.
func httpRouteAcceptedByGateway(route *unstructured.Unstructured, profile *validators.InferenceGatewayProfile, listener string) bool {
	for _, p := range httpRouteParentsFor(route, profile, listener) {
		if conditionStatus(p, "Accepted") == conditionStatusTrue && conditionStatus(p, "ResolvedRefs") == conditionStatusTrue {
			return true
		}
	}
	return false
}

// httpRouteParentsFor returns the status.parents entries whose parentRef is
// the profile's Gateway; with a listener, only the entry whose parentRef
// names it as sectionName (an empty listener returns every entry for the
// Gateway, for the status artifact).
func httpRouteParentsFor(route *unstructured.Unstructured, profile *validators.InferenceGatewayProfile, listener string) []map[string]any {
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	var out []map[string]any
	for _, p := range parents {
		pMap, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(pMap, "parentRef", keyName)
		namespace, _, _ := unstructured.NestedString(pMap, "parentRef", keyNamespace)
		if namespace == "" {
			namespace = route.GetNamespace()
		}
		if name != profile.GatewayName || namespace != profile.Namespace {
			continue
		}
		if section, _, _ := unstructured.NestedString(pMap, "parentRef", keySection); listener != "" && section != listener {
			continue
		}
		out = append(out, pMap)
	}
	return out
}

// conditionStatus returns the status string of the named condition in a
// status.parents entry, or "" when absent.
func conditionStatus(parent map[string]any, condType string) string {
	conds, _, _ := unstructured.NestedSlice(parent, "conditions")
	for _, c := range conds {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if typ, _, _ := unstructured.NestedString(cMap, "type"); typ == condType {
			status, _, _ := unstructured.NestedString(cMap, "status")
			return status
		}
	}
	return ""
}

// httpRouteParentStatusText renders the Accepted and ResolvedRefs conditions
// the profile's Gateway reported on the route, on every listener it reported
// for, for the evidence artifact and the timeout message.
func httpRouteParentStatusText(route *unstructured.Unstructured, profile *validators.InferenceGatewayProfile) string {
	parents := httpRouteParentsFor(route, profile, "")
	if len(parents) == 0 {
		return fmt.Sprintf("Gateway %s/%s has not reported status on the route (no matching status.parents entry)", profile.Namespace, profile.GatewayName)
	}
	var sb strings.Builder
	for _, p := range parents {
		controller, _, _ := unstructured.NestedString(p, "controllerName")
		section, _, _ := unstructured.NestedString(p, "parentRef", keySection)
		fmt.Fprintf(&sb, "Parent %s/%s listener %s (controller %s):\n", profile.Namespace, profile.GatewayName, valueOrUnknown(section), valueOrUnknown(controller))
		conds, _, _ := unstructured.NestedSlice(p, "conditions")
		for _, c := range conds {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(cMap, "type")
			status, _, _ := unstructured.NestedString(cMap, "status")
			reason, _, _ := unstructured.NestedString(cMap, "reason")
			message, _, _ := unstructured.NestedString(cMap, "message")
			fmt.Fprintf(&sb, "  %-13s %-6s reason=%s message=%s\n", typ, status, valueOrUnknown(reason), valueOrUnknown(hpaOneLine(message)))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// waitForHTTPRouteAccepted watches the test route in ns until the profile's
// Gateway reports it Accepted with ResolvedRefs on the pinned listener, or
// the timeout elapses. A watch
// channel the apiserver closes is followed by a re-Get before the watch is
// re-established, so a closed channel is never a verdict; a transient read or
// watch error is retried after defaults.PodPollInterval within the timeout;
// only a definitive API verdict such as Forbidden ends the wait early. The
// last observed route is returned with a timeout error so the caller can
// record the conditions the Gateway did report (NotAllowedByListeners when
// the listener's allowedRoutes excludes the test namespace, BackendNotFound
// when the Services are not visible yet).
func waitForHTTPRouteAccepted(ctx context.Context, dyn dynamic.Interface, ns string,
	profile *validators.InferenceGatewayProfile, listener string, timeout time.Duration) (*unstructured.Unstructured, error) {

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	const name = gatewayTrafficRouteName
	what := fmt.Sprintf("HTTPRoute %s/%s", ns, name)
	var last *unstructured.Unstructured
	retryAfterTransient := func() bool {
		select {
		case <-waitCtx.Done():
			return false
		case <-time.After(defaults.PodPollInterval):
			return true
		}
	}
	for {
		route, err := dyn.Resource(httpRouteGVR).Namespace(ns).Get(waitCtx, name, metav1.GetOptions{})
		if err != nil {
			if isPermanentAPIError(err) {
				return last, classifyK8sReadError(err, what)
			}
			if retryAfterTransient() {
				continue
			}
			break
		}
		last = route
		if httpRouteAcceptedByGateway(route, profile, listener) {
			return route, nil
		}
		watcher, err := dyn.Resource(httpRouteGVR).Namespace(ns).Watch(waitCtx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + name, ResourceVersion: route.GetResourceVersion(),
		})
		if err != nil {
			if isPermanentAPIError(err) {
				return last, classifyK8sReadError(err, what+" watch")
			}
			if retryAfterTransient() {
				continue
			}
			break
		}
		route, done, err := consumeHTTPRouteEvents(waitCtx, watcher, profile, listener)
		watcher.Stop()
		if route != nil {
			last = route
		}
		if done || err != nil {
			return last, err
		}
		if waitCtx.Err() != nil {
			break
		}
		// The channel closed without a verdict: pause, so an apiserver that
		// closes watches at once cannot turn the loop into a request spin,
		// then re-Get and watch again.
		if !retryAfterTransient() {
			break
		}
	}
	if ctx.Err() != nil {
		return last, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting for "+what+" canceled")
	}
	detail := "route not observed"
	if last != nil {
		detail = httpRouteParentStatusText(last, profile)
	}
	return last, errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
		"%s was not Accepted with ResolvedRefs by Gateway %s/%s on listener %s within %s; %s. "+
			"That HTTP listener must allow routes from other namespaces (allowedRoutes.namespaces) and resolve Services in the test namespace",
		what, profile.Namespace, profile.GatewayName, valueOrUnknown(listener), timeout, detail))
}

// consumeHTTPRouteEvents reads watch events until the test route is
// accepted, the context ends, or the channel closes (done=false, err=nil).
// Events for other routes are skipped: the fake watch used in tests ignores
// field selectors.
func consumeHTTPRouteEvents(ctx context.Context, watcher watch.Interface,
	profile *validators.InferenceGatewayProfile, listener string) (*unstructured.Unstructured, bool, error) {

	var last *unstructured.Unstructured
	for {
		select {
		case <-ctx.Done():
			return last, false, nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return last, false, nil
			}
			route, isRoute := event.Object.(*unstructured.Unstructured)
			if !isRoute || route.GetName() != gatewayTrafficRouteName {
				continue
			}
			last = route
			if event.Type == watch.Deleted {
				return route, true, errors.New(errors.ErrCodeNotFound, fmt.Sprintf(
					"HTTPRoute %s/%s was deleted while waiting for the Gateway to accept it", route.GetNamespace(), gatewayTrafficRouteName))
			}
			if httpRouteAcceptedByGateway(route, profile, listener) {
				return route, true, nil
			}
		}
	}
}

// cleanupGatewayTrafficResources deletes the HTTPRoute explicitly (the route
// must not stay attached to the recipe's Gateway while the namespace drains)
// and then the per-run namespace, which takes the backends and the probe with
// it. Runs on its own context so it completes from a deferred block whose
// parent is canceled; see cleanupGPUTestNamespace for the status and error
// semantics.
func cleanupGatewayTrafficResources(clientset kubernetes.Interface, dynClient dynamic.Interface, run *gpuTestRun) (string, error) {
	if run.nsObserved {
		// Best effort on a fraction of the cleanup budget: the namespace
		// deletion that follows removes the route anyway, and it is the
		// namespace deletion that must fit the check's cleanup reserve.
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout/4)
		defer cancel()
		if err := dynClient.Resource(httpRouteGVR).Namespace(run.namespace).Delete(
			ctx, gatewayTrafficRouteName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("failed to delete the HTTPRoute before namespace cleanup", "namespace", run.namespace, "error", err)
		}
	}
	return cleanupGPUTestNamespace(clientset, run)
}

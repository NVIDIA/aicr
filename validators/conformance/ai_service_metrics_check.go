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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	prometheusComponentName = "kube-prometheus-stack"
	prometheusDefaultPort   = 9090

	// aiMetricsTestPrefix names the per-run test namespace; leftovers from
	// interrupted runs are found by this prefix.
	aiMetricsTestPrefix    = "ai-metrics-test-"
	aiMetricsWorkloadName  = "ai-metrics-stub"
	aiMetricsContainerName = "server"
	aiMetricsPortName      = "http"
	aiMetricsPort          = 8080
	// aiMetricsRunLabel is the series label carrying the per-run id, so the
	// queries read this run's counters and nothing a previous run left in
	// Prometheus.
	aiMetricsRunLabel     = "conformance_run"
	aiMetricRequestsTotal = "ai_conformance_inference_requests_total"
	aiMetricLatency       = "ai_conformance_inference_request_duration_seconds"
	aiMetricQueueDepth    = "ai_conformance_inference_queue_depth"
)

// aiMetricsPhase names the phase of the check that produced a verdict.
type aiMetricsPhase string

const (
	aiMetricsPhaseDeploy   aiMetricsPhase = "deploy"
	aiMetricsPhaseBaseline aiMetricsPhase = "baseline"
	aiMetricsPhaseTraffic  aiMetricsPhase = "traffic"
	aiMetricsPhaseIncrease aiMetricsPhase = "increase"
)

// aiMetricsStubScript is the CPU stub inference server, mirroring
// kubernetes-sigs/ai-conformance TestAIServiceMetrics: /v1/infer answers a
// request and counts it, /metrics serves a request counter, a latency
// histogram and a queue-depth gauge, each labeled with the run id.
const aiMetricsStubScript = `
import json, os, socket, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

RUN = os.environ["RUN_ID"]
PORT = int(os.environ["PORT"])
BUCKETS = (0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0)
lock = threading.Lock()
state = {"count": 0, "sum": 0.0, "buckets": [0] * len(BUCKETS), "inflight": 0}

def render():
    lbl = 'conformance_run="%s"' % RUN
    with lock:
        count, total, inflight = state["count"], state["sum"], state["inflight"]
        buckets = list(state["buckets"])
    out = [
        "# HELP ai_conformance_inference_requests_total Total inference requests served.",
        "# TYPE ai_conformance_inference_requests_total counter",
        "ai_conformance_inference_requests_total{%s} %d" % (lbl, count),
        "# HELP ai_conformance_inference_request_duration_seconds Inference request latency.",
        "# TYPE ai_conformance_inference_request_duration_seconds histogram",
    ]
    for le, n in zip(BUCKETS, buckets):
        out.append('ai_conformance_inference_request_duration_seconds_bucket{%s,le="%s"} %d' % (lbl, le, n))
    out += [
        'ai_conformance_inference_request_duration_seconds_bucket{%s,le="+Inf"} %d' % (lbl, count),
        "ai_conformance_inference_request_duration_seconds_sum{%s} %f" % (lbl, total),
        "ai_conformance_inference_request_duration_seconds_count{%s} %d" % (lbl, count),
        "# HELP ai_conformance_inference_queue_depth Requests currently queued or in flight.",
        "# TYPE ai_conformance_inference_queue_depth gauge",
        "ai_conformance_inference_queue_depth{%s} %d" % (lbl, inflight),
    ]
    return "\n".join(out) + "\n"

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send(self, code, body, ctype):
        data = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/healthz":
            return self.send(200, "ok", "text/plain")
        if self.path == "/metrics":
            return self.send(200, render(), "text/plain; version=0.0.4; charset=utf-8")
        if self.path.startswith("/v1/infer"):
            return self.infer()
        self.send(404, "not found", "text/plain")

    def infer(self):
        start = time.monotonic()
        with lock:
            state["inflight"] += 1
        time.sleep(0.01)
        elapsed = time.monotonic() - start
        with lock:
            state["inflight"] -= 1
            state["count"] += 1
            state["sum"] += elapsed
            for i, le in enumerate(BUCKETS):
                if elapsed <= le:
                    state["buckets"][i] += 1
        self.send(200, json.dumps({"output": "ok"}), "application/json")

class Server(ThreadingHTTPServer):
    address_family = socket.AF_INET6 if socket.has_dualstack_ipv6() else socket.AF_INET

    def server_bind(self):
        if self.address_family == socket.AF_INET6:
            self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
        super().server_bind()

Server(("::" if Server.address_family == socket.AF_INET6 else "", PORT), Handler).serve_forever()
`

// CheckAIServiceMetrics validates CNCF requirement ai_service_metrics: the
// recipe's monitoring system discovers a workload's Prometheus-format endpoint
// through a scrape configuration the check creates and collects its metrics.
// It mirrors kubernetes-sigs/ai-conformance TestAIServiceMetrics:
//
//  1. deploy: a CPU stub workload (no accelerator) exposing a request counter,
//     a latency histogram and a queue-depth gauge labeled with a per-run id,
//     its Service, and a ServiceMonitor carrying the recipe's Prometheus
//     selector label;
//  2. baseline: wait until Prometheus has loaded the ServiceMonitor into its
//     scrape pools, then until it has collected all three series; the
//     collection clock starts at the load, not at the ServiceMonitor's
//     creation, so prometheus-operator's reconcile schedule does not consume it;
//  3. traffic: send exactly defaults.AIServiceMetricsRequestCount requests
//     through the Service; any failure aborts, since the expected count would
//     be ambiguous;
//  4. increase: wait until the collected counter equals the baseline plus the
//     requests sent. A value above that fails at once: something else sent
//     traffic, and the collection cannot be attributed.
//
// The check does not depend on a model being served; it passes on every
// recipe that carries kube-prometheus-stack and fails when Prometheus does not
// load the ServiceMonitor within the monitor-load timeout or, once loaded,
// does not collect its series within the collection timeout.
func CheckAIServiceMetrics(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	promURL, err := discoverPrometheusURL(ctx)
	if err != nil {
		return err
	}
	return checkAIServiceMetricsWithURL(ctx, promURL)
}

// discoverPrometheusURL finds the Prometheus service URL by looking up the
// kube-prometheus-stack component namespace in the recipe and discovering
// the Prometheus service via label selector. No hardcoded service names.
func discoverPrometheusURL(ctx *validators.Context) (string, error) {
	if ctx.ValidationInput == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "validation is not available")
	}

	var namespace string
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == prometheusComponentName {
			namespace = ref.Namespace
			break
		}
	}
	if namespace == "" {
		return "", errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("component %q not found in recipe or has no namespace", prometheusComponentName))
	}

	// Try multiple label selectors to handle different kube-prometheus-stack versions.
	// Older versions (<=81.x) use app.kubernetes.io/name=prometheus;
	// newer versions (>=82.x) dropped that label but retain self-monitor=true.
	selectors := []string{
		"app.kubernetes.io/name=prometheus",
		"self-monitor=true",
	}
	for _, selector := range selectors {
		services, err := ctx.Clientset.CoreV1().Services(namespace).List(ctx.Ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Prometheus services", err)
		}
		for i := range services.Items {
			svc := &services.Items[i]
			for _, port := range svc.Spec.Ports {
				if port.Port == int32(prometheusDefaultPort) {
					return fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, namespace, prometheusDefaultPort), nil
				}
			}
		}
	}

	return "", errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("no Prometheus service with port %d found in namespace %q", prometheusDefaultPort, namespace))
}

// checkAIServiceMetricsWithURL is the testable implementation that accepts the
// Prometheus base URL.
func checkAIServiceMetricsWithURL(ctx *validators.Context, promBaseURL string) error {
	// Bound all work so one bounded namespace cleanup plus the skew/flush
	// margin fit inside the check's deadline (see gpuCheckWorkBudget).
	budget, err := gpuCheckWorkBudget(ctx.Ctx, aiServiceMetricsWorkBudget)
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
	s, err := newAIMetricsScenario(ctx.Clientset, dynClient, promBaseURL, ctx.Tolerations)
	if err != nil {
		return err
	}
	// Preflight BEFORE creating anything: a Prometheus that cannot answer
	// vector(1) is an environment problem, not a collection failure.
	if err := s.preflight(ctx.Ctx); err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Prometheus API preflight",
		fmt.Sprintf("curl -sf '%s/api/v1/query?query=vector(1)'; curl -sf '%s/api/v1/scrape_pools'", promBaseURL, promBaseURL),
		fmt.Sprintf("Endpoint: %s\nResult:   vector(1) = 1; scrape pools listed", promBaseURL))
	return s.runWithCleanup(ctx)
}

// aiMetricsScenario drives one deploy, baseline, traffic and increase cycle.
// The query, request and timing fields are injectable so unit tests can drive
// the cycle against a fake API server and a stub Prometheus.
type aiMetricsScenario struct {
	clientset kubernetes.Interface
	dynClient dynamic.Interface
	testRun   *gpuTestRun
	runID     string
	promURL   string
	// tolerations is applied to the stub pod; nil means tolerate-all, as
	// buildHPADeployment does.
	tolerations []corev1.Toleration

	query           func(ctx context.Context, promql string) ([]byte, error)
	listScrapePools func(ctx context.Context) ([]byte, error)
	sendRequest     func(ctx context.Context) error

	pollInterval       time.Duration
	readyTimeout       time.Duration
	monitorLoadTimeout time.Duration
	collectionTimeout  time.Duration
	requestCount       int
	sent               int // requests answered so far; the verdict reports it against requestCount

	phase   aiMetricsPhase
	timings map[aiMetricsPhase]time.Duration
}

func newAIMetricsScenario(clientset kubernetes.Interface, dynClient dynamic.Interface, promURL string, tolerations []corev1.Toleration) (*aiMetricsScenario, error) {
	run, err := newGPUTestRun()
	if err != nil {
		return nil, err
	}
	run.namespace = aiMetricsTestPrefix + run.token
	s := &aiMetricsScenario{
		clientset:          clientset,
		dynClient:          dynClient,
		testRun:            run,
		runID:              run.token[:10],
		promURL:            promURL,
		tolerations:        tolerations,
		pollInterval:       defaults.AIServiceMetricsPollInterval,
		readyTimeout:       defaults.AIServiceMetricsWorkloadReadyTimeout,
		monitorLoadTimeout: defaults.PrometheusMonitorLoadTimeout,
		collectionTimeout:  defaults.AIServiceMetricsCollectionTimeout,
		requestCount:       defaults.AIServiceMetricsRequestCount,
		timings:            map[aiMetricsPhase]time.Duration{},
	}
	s.query = func(c context.Context, promql string) ([]byte, error) {
		return httpGet(c, s.promURL+"/api/v1/query?"+url.Values{"query": {promql}}.Encode())
	}
	s.listScrapePools = promScrapePoolLister(promURL)
	s.sendRequest = func(c context.Context) error {
		_, err := httpGet(c, fmt.Sprintf("http://%s.%s.svc:%d/v1/infer", aiMetricsWorkloadName, s.testRun.namespace, aiMetricsPort))
		return err
	}
	return s, nil
}

// preflight requires the query API to evaluate vector(1) and the scrape pool
// API to answer, so a Prometheus that cannot be observed fails before any
// resource is created.
func (s *aiMetricsScenario) preflight(ctx context.Context) error {
	body, err := s.query(ctx, "vector(1)")
	if err != nil {
		return wrapWithCode(err, errors.ErrCodeUnavailable, fmt.Sprintf(
			"Prometheus unreachable at %s — verify network connectivity (security groups, network policies) between validator pod and Prometheus service",
			s.promURL))
	}
	v, found, err := parsePromSingleSample(body)
	if err != nil {
		return wrapWithCode(err, errors.ErrCodeInternal, "Prometheus query API preflight failed")
	}
	if !found || v != 1 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"Prometheus query API preflight failed: vector(1) returned found=%t value=%v, want 1", found, v))
	}
	return preflightScrapePools(ctx, s.promURL, s.listScrapePools)
}

// runWithCleanup creates the per-run namespace, runs the cycle, and always
// tears down the ServiceMonitor and the namespace. The return is NAMED so a
// terminally failed cleanup fails an otherwise-passing check; a primary error
// always wins.
//
// The teardown is deferred BEFORE the namespace create: a Create that
// returns an error may still have persisted the namespace, and only a
// cleanup registered first can reconcile it (see cleanupGPUTestNamespace).
func (s *aiMetricsScenario) runWithCleanup(ctx *validators.Context) (err error) {
	defer func() {
		status, cleanupErr := cleanupAIMetricsWorkload(s.clientset, s.dynClient, s.testRun)
		recordRawTextArtifact(ctx, "Delete test namespace", cleanupInspectEquivalent(s.testRun.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = createGPUTestNamespace(ctx.Ctx, s.clientset, s.testRun); err != nil {
		return err
	}

	err = s.run(ctx)
	recordRawTextArtifact(ctx, "AI service metrics verdict", "", s.verdictSummary(err))
	if err != nil {
		return wrapWithCode(err, errors.ErrCodeInternal, fmt.Sprintf("AI service metrics test failed in phase %s", s.phase))
	}
	return nil
}

func (s *aiMetricsScenario) run(ctx *validators.Context) error {
	s.phase = aiMetricsPhaseDeploy
	if err := s.deploy(ctx); err != nil {
		return err
	}
	s.phase = aiMetricsPhaseBaseline
	if err := s.waitForMonitorLoaded(ctx); err != nil {
		return err
	}
	baselines, err := s.waitForBaselines(ctx)
	if err != nil {
		return err
	}
	s.phase = aiMetricsPhaseTraffic
	if trafficErr := s.sendTraffic(ctx); trafficErr != nil {
		return trafficErr
	}
	s.phase = aiMetricsPhaseIncrease
	return s.waitForIncrease(ctx, baselines)
}

func (s *aiMetricsScenario) verdictSummary(err error) string {
	var sb strings.Builder
	if err == nil {
		fmt.Fprintf(&sb, "Verdict:        PASS (all phases completed; last phase: %s)\n", s.phase)
	} else {
		fmt.Fprintf(&sb, "Verdict:        FAIL in phase %s\nError:          %v\n", s.phase, err)
	}
	fmt.Fprintf(&sb, "Run id:         %s (series label %s)\nRequests sent:  %d of %d\n", s.runID, aiMetricsRunLabel, s.sent, s.requestCount)
	for _, phase := range []aiMetricsPhase{aiMetricsPhaseDeploy, aiMetricsPhaseBaseline, aiMetricsPhaseTraffic, aiMetricsPhaseIncrease} {
		if d, ok := s.timings[phase]; ok {
			fmt.Fprintf(&sb, "Phase %-10s %s\n", string(phase)+":", d.Round(time.Second))
		}
	}
	return sb.String()
}

// deploy creates the stub pod, its Service and the ServiceMonitor, then waits
// for the pod to be Ready.
func (s *aiMetricsScenario) deploy(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	pod, svc := buildAIMetricsWorkload(ns, s.runID, s.collectionTimeout, s.tolerations, s.testRun.imagePullSecretRefs())
	if _, err := s.clientset.CoreV1().Pods(ns).Create(ctx.Ctx, pod, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create metrics stub pod", err)
	}
	if _, err := s.clientset.CoreV1().Services(ns).Create(ctx.Ctx, svc, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create metrics stub Service", err)
	}
	if _, err := s.dynClient.Resource(serviceMonitorGVR).Namespace(ns).Create(ctx.Ctx,
		buildAIMetricsServiceMonitor(ns), metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to create ServiceMonitor for the metrics stub — the recipe's kube-prometheus-stack must serve monitoring.coreos.com/v1", err)
	}
	recordRawTextArtifact(ctx, "AI service metrics test resources",
		fmt.Sprintf("kubectl get pod,svc,servicemonitor -n %s", ns),
		fmt.Sprintf("Namespace:       %s\nPod:             %s (image %s, no accelerator, run id %s)\nService:         %s (port %s/%d)\nServiceMonitor:  %s (label %s=%s, scrape %s)\nSeries:          %s, %s, %s",
			ns, aiMetricsWorkloadName, defaults.PythonProbeImage, s.runID, aiMetricsWorkloadName, aiMetricsPortName, aiMetricsPort,
			aiMetricsWorkloadName, prometheusReleaseLabel, prometheusComponentName, defaults.AIServiceMetricsScrapeInterval,
			aiMetricRequestsTotal, aiMetricLatency, aiMetricQueueDepth))

	if _, err := waitForPodReady(ctx.Ctx, s.clientset, ns, aiMetricsWorkloadName, s.readyTimeout); err != nil {
		return wrapWithCode(err, errors.ErrCodeTimeout, "metrics stub pod did not become Ready")
	}
	s.timings[aiMetricsPhaseDeploy] = time.Since(start)
	return nil
}

// aiMetricExpectation is one series the monitoring system must collect.
type aiMetricExpectation struct {
	description string
	query       string
	counter     bool
}

func aiMetricExpectations(runID string) []aiMetricExpectation {
	sel := fmt.Sprintf(`{%s=%q}`, aiMetricsRunLabel, runID)
	return []aiMetricExpectation{
		{"request count", "max(" + aiMetricRequestsTotal + sel + ")", true},
		{"request latency histogram count", "max(" + aiMetricLatency + "_count" + sel + ")", true},
		{"queue depth gauge", "max(" + aiMetricQueueDepth + sel + ")", false},
	}
}

// waitForMonitorLoaded holds the baseline until Prometheus has loaded the
// ServiceMonitor, so the collection timeout that follows measures scraping
// rather than prometheus-operator's reconcile schedule (#3671).
func (s *aiMetricsScenario) waitForMonitorLoaded(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	prefix := scrapePoolPrefix(monitorKindService, ns, aiMetricsWorkloadName)
	slog.Info("waiting for Prometheus to load the ServiceMonitor", "scrapePool", prefix, "timeout", s.monitorLoadTimeout)
	if err := waitForMonitorLoaded(ctx.Ctx, s.listScrapePools, monitorKindService, ns, aiMetricsWorkloadName,
		s.monitorLoadTimeout, s.pollInterval); err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "AI service metrics: ServiceMonitor loaded",
		fmt.Sprintf("curl -sf '%s/api/v1/scrape_pools' | grep '%s'", s.promURL, prefix),
		fmt.Sprintf("Scrape pool:   %s*\nLoaded after:  %s", prefix, time.Since(start).Round(time.Second)))
	return nil
}

func (s *aiMetricsScenario) waitForBaselines(ctx *validators.Context) ([]float64, error) {
	start := time.Now()
	slog.Info("waiting for the monitoring system to collect the stub's metrics", "timeout", s.collectionTimeout)
	values, status, err := s.pollMetrics(ctx.Ctx, aiMetricExpectations(s.runID), func(_ int, v float64) (string, bool, error) {
		return fmt.Sprintf("collected (baseline %v)", v), true, nil
	})
	if err != nil {
		return nil, wrapWithCode(err, errors.ErrCodeNotFound, "Prometheus loaded the ServiceMonitor but the workload metrics were not collected")
	}
	s.timings[aiMetricsPhaseBaseline] = time.Since(start)
	recordRawTextArtifact(ctx, "AI service metrics: baseline collection",
		fmt.Sprintf("curl -sf '%s/api/v1/query?query=%s'", s.promURL, url.QueryEscape(aiMetricExpectations(s.runID)[0].query)),
		strings.Join(status, "\n")+fmt.Sprintf("\nElapsed: %s", s.timings[aiMetricsPhaseBaseline].Round(time.Second)))
	return values, nil
}

// sendTraffic sends exactly requestCount requests; any failure aborts because
// the expected count would then be ambiguous.
func (s *aiMetricsScenario) sendTraffic(ctx *validators.Context) error {
	start := time.Now()
	for i := 1; i <= s.requestCount; i++ {
		if err := ctx.Ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, fmt.Sprintf("check budget exhausted before request %d/%d", i, s.requestCount))
		}
		if err := s.sendRequest(ctx.Ctx); err != nil {
			return wrapWithCode(err, errors.ErrCodeUnavailable, fmt.Sprintf(
				"inference request %d/%d failed; aborting because the expected request count is now ambiguous", i, s.requestCount))
		}
		s.sent++
	}
	s.timings[aiMetricsPhaseTraffic] = time.Since(start)
	recordRawTextArtifact(ctx, "AI service metrics: traffic",
		fmt.Sprintf("for i in $(seq %d); do curl -sf http://%s.%s.svc:%d/v1/infer; done", s.requestCount, aiMetricsWorkloadName, s.testRun.namespace, aiMetricsPort),
		fmt.Sprintf("Requests sent: %d (all answered)\nElapsed:       %s", s.requestCount, s.timings[aiMetricsPhaseTraffic].Round(time.Second)))
	return nil
}

// waitForIncrease requires every counter to equal its baseline plus the
// requests sent. A value above that fails at once.
func (s *aiMetricsScenario) waitForIncrease(ctx *validators.Context, baselines []float64) error {
	start := time.Now()
	checks := aiMetricExpectations(s.runID)
	slog.Info("waiting for the collected request counts to increase by the requests sent", "requests", s.requestCount, "timeout", s.collectionTimeout)
	_, status, err := s.pollMetrics(ctx.Ctx, checks, func(i int, v float64) (string, bool, error) {
		return evaluateCounterIncrease(checks[i].counter, baselines[i], v, s.requestCount)
	})
	if err != nil {
		return wrapWithCode(err, errors.ErrCodeNotFound, "collected request counts did not increase as expected")
	}
	s.timings[aiMetricsPhaseIncrease] = time.Since(start)
	recordRawTextArtifact(ctx, "AI service metrics: exact-count assertion",
		fmt.Sprintf("curl -sf '%s/api/v1/query?query=%s'", s.promURL, url.QueryEscape(checks[0].query)),
		strings.Join(status, "\n")+fmt.Sprintf("\nElapsed: %s", s.timings[aiMetricsPhaseIncrease].Round(time.Second)))
	return nil
}

// evaluateCounterIncrease judges one collected value against its baseline:
// exactly baseline+requests passes, above fails at once (extra traffic reached
// the workload), below the baseline or short of the target keeps waiting. A
// gauge passes on any collected value.
func evaluateCounterIncrease(counter bool, baseline, v float64, requests int) (string, bool, error) {
	if !counter {
		return fmt.Sprintf("ok (%v)", v), true, nil
	}
	want := baseline + float64(requests)
	switch {
	case v > want:
		return "", false, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"got %v, want %v (baseline %v + %d); extra requests reached the workload", v, want, baseline, requests))
	case v < baseline:
		return fmt.Sprintf("got %v, below baseline %v; the counter was reset or the collector restarted", v, baseline), false, nil
	case v != want:
		return fmt.Sprintf("got %v, want %v (baseline %v + %d)", v, want, baseline, requests), false, nil
	}
	return fmt.Sprintf("ok (%v = baseline %v + %d)", v, baseline, requests), true, nil
}

// pollMetrics queries every expectation each interval until eval accepts all
// of them or the collection timeout elapses. An eval error (a verdict) ends
// the wait at once. Polling, not a watch: Prometheus has no watch API.
func (s *aiMetricsScenario) pollMetrics(ctx context.Context, checks []aiMetricExpectation,
	eval func(i int, v float64) (status string, ok bool, err error)) ([]float64, []string, error) {

	values := make([]float64, len(checks))
	status := make([]string, len(checks))
	var verdict error
	err := wait.PollUntilContextTimeout(ctx, s.pollInterval, s.collectionTimeout, true, func(ctx context.Context) (bool, error) {
		if intactErr := checkAIMetricsWorkloadIntact(ctx, s.clientset, s.testRun.namespace); intactErr != nil {
			return false, intactErr
		}
		done := true
		for i, c := range checks {
			body, queryErr := s.query(ctx, c.query)
			if queryErr != nil {
				if ctx.Err() != nil {
					return false, nil
				}
				status[i] = fmt.Sprintf("%s: query error: %v", c.description, queryErr)
				done = false
				continue
			}
			v, found, parseErr := parsePromSingleSample(body)
			switch {
			case parseErr != nil:
				status[i] = fmt.Sprintf("%s: %v", c.description, parseErr)
				done = false
			case !found:
				status[i] = fmt.Sprintf("%s: not collected yet (%s)", c.description, c.query)
				done = false
			default:
				text, ok, evalErr := eval(i, v)
				if evalErr != nil {
					verdict = wrapWithCode(evalErr, errors.ErrCodeInternal, c.description)
					return false, verdict
				}
				values[i], status[i] = v, c.description+": "+text
				done = done && ok
			}
		}
		return done, nil
	})
	switch {
	case err == nil:
		return values, status, nil
	case verdict != nil:
		return nil, status, verdict
	case ctx.Err() != nil:
		return nil, status, errors.Wrap(errors.ErrCodeTimeout, "check budget exhausted while waiting for collection:\n  "+strings.Join(status, "\n  "), ctx.Err())
	case wait.Interrupted(err):
		return nil, status, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf(
			"not collected within %s:\n  %s\n  If the scrape interval or ingestion delay is long, raise the collection timeout; "+
				"also check the stub's target health at %s/api/v1/targets",
			s.collectionTimeout, strings.Join(status, "\n  "), s.promURL), err)
	default:
		return nil, status, wrapWithCode(err, errors.ErrCodeInternal, "failed while waiting for collection")
	}
}

// checkAIMetricsWorkloadIntact fails the wait when the stub pod is no longer
// Running or its container restarted: either resets the counters the check
// compares. Only a confirmed state is a verdict: a pod that is gone, or a
// definitive API verdict such as Forbidden. A read that fails transiently
// (timeout, throttling, 5xx, transport) is left for the next poll.
func checkAIMetricsWorkloadIntact(ctx context.Context, c kubernetes.Interface, ns string) error {
	pod, err := c.CoreV1().Pods(ns).Get(ctx, aiMetricsWorkloadName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) || isPermanentAPIError(err) {
			return classifyK8sReadError(err, "metrics stub pod")
		}
		return nil
	}
	if pod.Status.Phase != corev1.PodRunning {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("metrics stub pod is %s; its counters were lost", pod.Status.Phase))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > 0 {
			return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"metrics stub container %s restarted %d time(s); its counters were reset", cs.Name, cs.RestartCount))
		}
	}
	return nil
}

// promQueryResponse is the Prometheus instant-query response envelope.
type promQueryResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// parsePromSingleSample decodes an instant vector with at most one sample.
func parsePromSingleSample(body []byte) (value float64, found bool, err error) {
	var r promQueryResponse
	if decodeErr := json.Unmarshal(body, &r); decodeErr != nil {
		return 0, false, errors.Wrap(errors.ErrCodeInternal, "failed to decode Prometheus query response", decodeErr)
	}
	if r.Status != "success" {
		return 0, false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("Prometheus query failed: %s: %s", r.ErrorType, r.Error))
	}
	if r.Data.ResultType != "vector" {
		return 0, false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("expected an instant vector result, got %q", r.Data.ResultType))
	}
	switch len(r.Data.Result) {
	case 0:
		return 0, false, nil
	case 1:
	default:
		return 0, false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("expected at most one sample, got %d", len(r.Data.Result)))
	}
	v := r.Data.Result[0].Value
	if len(v) != 2 {
		return 0, false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("malformed sample %v", v))
	}
	str, ok := v[1].(string)
	if !ok {
		return 0, false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("malformed sample value %v", v[1]))
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0, false, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to parse sample value %q", str), err)
	}
	return f, true, nil
}

// buildAIMetricsWorkload returns the CPU stub pod and its Service. The pod's
// activeDeadlineSeconds caps its lifetime past the check's longest path so a
// leaked pod ends on its own. tolerations, when nil, defaults to tolerate-all
// so the stub schedules on all-tainted clusters.
func buildAIMetricsWorkload(ns, runID string, collectionTimeout time.Duration, tolerations []corev1.Toleration, pullSecrets []corev1.LocalObjectReference) (*corev1.Pod, *corev1.Service) {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	podLabels := map[string]string{hpaAppLabelKey: aiMetricsWorkloadName}
	lifetime := defaults.AIServiceMetricsWorkloadReadyTimeout + defaults.PrometheusMonitorLoadTimeout + 2*collectionTimeout +
		defaults.K8sCleanupTimeout + gpuCheckSkewAndFlushMargin
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: aiMetricsWorkloadName, Namespace: ns, Labels: podLabels},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: ptr.To(int64(lifetime / time.Second)),
			Tolerations:           tolerations,
			ImagePullSecrets:      pullSecrets,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To(int64(65534)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    aiMetricsContainerName,
				Image:   defaults.PythonProbeImage,
				Command: []string{"python3", "-u", "-c", aiMetricsStubScript},
				Env:     []corev1.EnvVar{{Name: "RUN_ID", Value: runID}, {Name: "PORT", Value: strconv.Itoa(aiMetricsPort)}},
				Ports:   []corev1.ContainerPort{{Name: aiMetricsPortName, ContainerPort: aiMetricsPort}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString(aiMetricsPortName)}},
					PeriodSeconds: 2,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: aiMetricsWorkloadName, Namespace: ns, Labels: podLabels},
		Spec: corev1.ServiceSpec{
			Selector: podLabels,
			Ports:    []corev1.ServicePort{{Name: aiMetricsPortName, Port: aiMetricsPort, TargetPort: intstr.FromString(aiMetricsPortName)}},
		},
	}
	return pod, svc
}

// buildAIMetricsServiceMonitor selects the stub's Service by its app label
// and carries the recipe's Prometheus selector label.
func buildAIMetricsServiceMonitor(ns string) *unstructured.Unstructured {
	sm := &unstructured.Unstructured{Object: map[string]any{
		keySpec: map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]any{hpaAppLabelKey: aiMetricsWorkloadName},
			},
			"namespaceSelector": map[string]any{
				"matchNames": []any{ns},
			},
			"endpoints": []any{map[string]any{
				"port":     aiMetricsPortName,
				"path":     "/metrics",
				"interval": defaults.AIServiceMetricsScrapeInterval.String(),
			}},
		},
	}}
	sm.SetAPIVersion(serviceMonitorGVR.GroupVersion().String())
	sm.SetKind("ServiceMonitor")
	sm.SetName(aiMetricsWorkloadName)
	sm.SetNamespace(ns)
	sm.SetLabels(map[string]string{prometheusReleaseLabel: prometheusComponentName})
	return sm
}

// waitForPodReady watches the named pod until it is Running and Ready, or
// reports why it cannot get there (stuck image pull, unschedulable, failed).
// A watch channel the apiserver closes is followed by a re-Get before the
// watch is re-established, so a closed channel is never a verdict; nor is a
// transient read or watch error, which is retried after defaults.PodPollInterval
// within the timeout. Only a definitive API verdict such as Forbidden ends
// the wait early.
func waitForPodReady(ctx context.Context, c kubernetes.Interface, ns, name string, timeout time.Duration) (*corev1.Pod, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last *corev1.Pod
	// retryAfterTransient reports whether a failed read should be retried,
	// having waited one poll interval; false ends the loop or the wait.
	retryAfterTransient := func() bool {
		if waitCtx.Err() != nil {
			return false
		}
		select {
		case <-waitCtx.Done():
			return false
		case <-time.After(defaults.PodPollInterval):
			return true
		}
	}
	for {
		pod, err := c.CoreV1().Pods(ns).Get(waitCtx, name, metav1.GetOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			if isPermanentAPIError(err) {
				return last, classifyK8sReadError(err, fmt.Sprintf("pod %s/%s", ns, name))
			}
			if retryAfterTransient() {
				continue
			}
			break
		}
		if err == nil {
			last = pod
			if done, podErr := podReadyOrStuck(pod); done || podErr != nil {
				return pod, podErr
			}
		}
		watcher, err := c.CoreV1().Pods(ns).Watch(waitCtx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
		if err != nil {
			if isPermanentAPIError(err) {
				return last, classifyK8sReadError(err, fmt.Sprintf("pod %s/%s watch", ns, name))
			}
			if retryAfterTransient() {
				continue
			}
			break
		}
		pod, done, err := consumePodReadyEvents(waitCtx, watcher)
		watcher.Stop()
		if pod != nil {
			last = pod
		}
		if done || err != nil {
			return pod, err
		}
		if waitCtx.Err() != nil {
			break
		}
		// The channel closed without a verdict: re-Get and watch again.
	}
	if ctx.Err() != nil {
		return last, errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf("waiting for pod %s/%s canceled", ns, name), ctx.Err())
	}
	detail := "pod not observed"
	if last != nil {
		detail = fmt.Sprintf("phase=%s ready=%t waiting=%s", last.Status.Phase, podIsReady(last), podWaitingStatus(last))
	}
	return last, errors.New(errors.ErrCodeTimeout, fmt.Sprintf("pod %s/%s not Ready after %s (%s)", ns, name, timeout, detail))
}

// consumePodReadyEvents reads watch events until the pod is Ready or stuck,
// the context ends, or the channel closes (done=false, err=nil).
func consumePodReadyEvents(ctx context.Context, watcher watch.Interface) (*corev1.Pod, bool, error) {
	var last *corev1.Pod
	for {
		select {
		case <-ctx.Done():
			return last, false, nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return last, false, nil
			}
			pod, isPod := event.Object.(*corev1.Pod)
			if !isPod {
				continue
			}
			last = pod
			if event.Type == watch.Deleted {
				return pod, true, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("pod %s/%s was deleted while waiting for it to become Ready", pod.Namespace, pod.Name))
			}
			if done, err := podReadyOrStuck(pod); done || err != nil {
				return pod, done, err
			}
		}
	}
}

// podReadyOrStuck reports done when the pod is Running and Ready, or an error
// when it cannot get there (image pull failure, unschedulable, CrashLoopBackOff,
// or a terminal phase).
func podReadyOrStuck(pod *corev1.Pod) (bool, error) {
	if pod.Status.Phase == corev1.PodRunning && podIsReady(pod) {
		return true, nil
	}
	if reason := podStuckReason(pod); reason != "" {
		return false, errors.New(errors.ErrCodeUnavailable, fmt.Sprintf("pod %s/%s cannot become Ready: %s", pod.Namespace, pod.Name, reason))
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("pod %s/%s is %s (%s: %s), want Running",
			pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.Reason, hpaOneLine(pod.Status.Message)))
	}
	return false, nil
}

// cleanupAIMetricsWorkload deletes the ServiceMonitor explicitly (the scrape
// configuration must not outlive the run) and then the per-run namespace,
// which takes the pod and Service with it. Runs on its own context so it
// completes from a deferred block whose parent is canceled; see
// cleanupGPUTestNamespace for the status and error semantics.
func cleanupAIMetricsWorkload(clientset kubernetes.Interface, dynClient dynamic.Interface, run *gpuTestRun) (string, error) {
	if run.nsObserved {
		// Best effort on a fraction of the cleanup budget: the namespace
		// deletion that follows removes the ServiceMonitor anyway, and it is
		// the namespace deletion that must fit the check's cleanup reserve.
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout/4)
		defer cancel()
		if err := dynClient.Resource(serviceMonitorGVR).Namespace(run.namespace).Delete(
			ctx, aiMetricsWorkloadName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("failed to delete the ServiceMonitor before namespace cleanup", "namespace", run.namespace, "error", err)
		}
	}
	return cleanupGPUTestNamespace(clientset, run)
}

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
	stderrors "errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

// Integration contract, mirroring kubernetes-sigs/ai-conformance
// TestHPAAutoscaling: the recipe's Prometheus scrapes pods labeled
// hpaMetricSourceLabel on the hpaMetricsPortName port at /metrics (through
// the PodMonitor this check creates), and the recipe's prometheus-adapter
// serves the hpaMetricSeries gauge as the per-pod custom metric of the same
// name (the rule ships in recipes/components/prometheus-adapter/values.yaml).
// The gauge stands in for an inference server's per-replica queue depth; the
// check sets it directly through a mounted ConfigMap.
const (
	hpaWorkloadName       = "hpa-workload"
	hpaMetricSourceLabel  = "ai-conformance.kubernetes.io/hpa-metric-source"
	hpaAppLabelKey        = "app.kubernetes.io/name"
	hpaMetricSeries       = "ai_conformance_hpa_queue_depth"
	hpaMetricsPortName    = "metrics"
	hpaMetricsPort        = 8080
	hpaMetricMountPath    = "/etc/hpa-metric"
	hpaMetricValueKey     = "value"
	hpaContainerName      = "workload"
	hpaCustomMetricsGroup = "custom.metrics.k8s.io"

	hpaMinReplicas int32 = 1
	hpaMaxReplicas int32 = 2
)

// hpaTestPrefix names the per-run test namespace; leftovers from interrupted
// runs are found by this prefix.
const hpaTestPrefix = "hpa-test-"

// prometheusReleaseLabel is the label kube-prometheus-stack's Prometheus
// selects PodMonitors and ServiceMonitors by when selectors are not opened
// up; the recipe opens them, and the label is set anyway so a restricted
// selector still matches.
const prometheusReleaseLabel = "release"

// Each phase sets a distinct metric value, so a sample carrying the current
// phase's value cannot be stale. Adapter timestamps are not consulted:
// prometheus-adapter reports the query time, not the sample time.
const (
	hpaTargetValue    = 10
	hpaBaselineValue  = 1
	hpaScaleUpValue   = 100
	hpaScaleDownValue = 2
)

// hpaTerminationGracePeriod shortens pod removal on scale-down and cleanup;
// the metric server holds no state worth draining.
const hpaTerminationGracePeriod int64 = 5

// hpaCustomMetricsVersions are the versions the HPA controller's custom
// metrics client reads, in its preference order.
var hpaCustomMetricsVersions = []string{"v1beta2", versionV1beta1}

// metricsAPIWarmup bounds the wait for a declared adapter's custom metrics
// API to become available. A var (not const) only so unit tests can shorten
// it; production code must not reassign it.
var (
	metricsAPIWarmup        = defaults.MetricsAPIWarmupTimeout
	metricsAPIProbeInterval = defaults.HPAPollInterval
)

// hpaPhase names the phase of the behavioral test that produced a verdict.
type hpaPhase string

const (
	hpaPhaseDeploy          hpaPhase = "deploy"
	hpaPhaseBaseline        hpaPhase = "baseline"
	hpaPhaseNegativeControl hpaPhase = "negative-control"
	hpaPhaseScaleUp         hpaPhase = "scale-up"
	hpaPhaseScaleDown       hpaPhase = "scale-down"
)

// hpaMetricServerScript serves the mounted ConfigMap value as a Prometheus
// gauge, re-reading it on every request. /healthz (the readiness probe)
// succeeds only while the value is a finite number.
const hpaMetricServerScript = `
import math, os, socket
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

VALUE_FILE = os.environ["HPA_METRIC_FILE"]
SERIES = os.environ["HPA_METRIC_SERIES"]
PORT = int(os.environ["HPA_METRIC_PORT"])

def read_value():
    with open(VALUE_FILE) as f:
        value = float(f.read().strip())
    if not math.isfinite(value):
        raise ValueError("metric value %r is not finite" % value)
    return value

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path not in ("/healthz", "/metrics"):
            return self.reply(404, "text/plain", b"not found\n")
        try:
            value = read_value()
        except (OSError, ValueError) as e:
            return self.reply(503, "text/plain", (str(e) + "\n").encode())
        if self.path == "/healthz":
            return self.reply(200, "text/plain", b"ok\n")
        body = "# TYPE %s gauge\n%s %r\n" % (SERIES, SERIES, value)
        self.reply(200, "text/plain; version=0.0.4; charset=utf-8", body.encode())

    def reply(self, code, ctype, body):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

class DualStackServer(ThreadingHTTPServer):
    address_family = socket.AF_INET6
    def server_bind(self):
        self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
        super().server_bind()

try:
    server = DualStackServer(("::", PORT), Handler)
except OSError:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
server.serve_forever()
`

// hpaWorkloadCommand starts the metric server only if the container sees
// exactly one NVIDIA accelerator device, so a pod that was not actually
// granted its accelerator never reports Ready.
const hpaWorkloadCommand = `count=0
for device in /dev/nvidia[0-9]*; do
  [ -e "$device" ] || continue
  count=$((count + 1))
done
if [ "$count" -ne 1 ]; then
  echo "expected 1 accelerator device, found $count" >&2
  exit 1
fi
exec python3 -c "$HPA_METRIC_SERVER"`

// CheckPodAutoscaling validates CNCF requirement pod_autoscaling: the
// HorizontalPodAutoscaler scales accelerator-backed pods up and back down on a
// custom per-pod metric served through the custom metrics API. It mirrors
// kubernetes-sigs/ai-conformance TestHPAAutoscaling:
//
//  1. deploy: a one-replica Deployment whose pod requests exactly one
//     accelerator (DRA claim or nvidia.com/gpu: 1, following the resolved
//     allocation mode as secure-accelerator-access does) and serves the gauge
//     from a mounted ConfigMap, plus the PodMonitor that makes the recipe's
//     Prometheus scrape it;
//  2. baseline: the pod is Running and Ready, the recipe's Prometheus has
//     loaded the PodMonitor into its scrape pools (so the metric wait that
//     follows measures the pipeline, not prometheus-operator's reconcile
//     schedule), and the custom metrics API reports 1 for the pod; only then
//     is the HPA created, and it must reconcile at
//     one replica; a one-minute negative control requires it to keep deciding
//     on one replica while the metric is below target;
//  3. scale-up: the metric is raised to 100; the HPA must scale to two
//     replicas, every pod Running and Ready and reporting 100;
//  4. scale-down: the metric is lowered to 2; the HPA must return to one
//     replica after its 60s stabilization window.
//
// The recipe owns the adapter, so a custom metrics API that does not serve the
// probe's metric fails the check (upstream skips) when prometheus-adapter (or
// prometheus-adapter-ocp) is declared, and skips only when neither is. DCGM cannot attribute GPUs to pods
// under DRA, so the HPA is driven by the probe's own gauge, never by
// gpu_utilization.
func CheckPodAutoscaling(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// Applicability gate (#2122): prometheus-adapter (prometheus-adapter-ocp
	// on OpenShift) is the component that serves custom.metrics.k8s.io.
	// Declared but not served fails closed; not declared and not served
	// skips. A declared adapter is given the warm-up window on any probe
	// outcome short of a served version: its APIService registers before it
	// is available, and discovery reports that state as an error.
	adapter, declared := hpaAdapterComponent(ctx)
	metricsVersion, probeErr := resolveCustomMetricsVersion(ctx.Clientset)
	if declared && (probeErr != nil || metricsVersion == "") {
		probeErr = waitForMetricsAPI(ctx.Ctx, metricsAPIWarmup, metricsAPIProbeInterval,
			func(context.Context) error {
				var err error
				if metricsVersion, err = resolveCustomMetricsVersion(ctx.Clientset); err != nil {
					return err
				}
				if metricsVersion == "" {
					return errors.New(errors.ErrCodeNotFound, hpaCustomMetricsGroup+" is not served")
				}
				return nil
			})
		if probeErr != nil && stderrors.Is(probeErr, errors.New(errors.ErrCodeNotFound, "")) {
			probeErr = nil // settled: still absent after the warm-up window
		}
	}
	if err := (validators.Capability{
		Component: adapter,
		Subject:   hpaCustomMetricsGroup + " API group",
		AbsentMsg: "recipe declares " + adapter + " but " + hpaCustomMetricsGroup + " is not served — " +
			"apply the bundle or check the adapter's APIService and pods; the HPA cannot read a custom metric without it",
		InapplicableMsg: hpaCustomMetricsGroup + " not served and prometheus-adapter not declared in recipe (nor prometheus-adapter-ocp) — " +
			"no custom metrics API for the HPA to read",
	}).Require(ctx, probeErr, metricsVersion != ""); err != nil {
		return err
	}

	// Bound all work so one bounded namespace cleanup plus the skew/flush
	// margin fit inside the check's deadline (see gpuCheckWorkBudget).
	budget, err := gpuCheckWorkBudget(ctx.Ctx, podAutoscalingWorkBudget)
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
	scenario, err := newHPAScenario(ctx, dynClient, metricsVersion)
	if err != nil {
		return err
	}
	if err := scenario.attachPrometheus(ctx); err != nil {
		return err
	}
	return scenario.runWithCleanup(ctx)
}

// hpaScenario drives one baseline, scale-up and scale-down cycle. Timings
// and the metrics reader are fields so unit tests can drive the cycle against
// a fake control plane without the aggregated API.
type hpaScenario struct {
	clientset kubernetes.Interface
	dynClient dynamic.Interface
	testRun   *gpuTestRun

	// dra selects the accelerator request shape: a ResourceClaimTemplate
	// claim at draVersion, or nvidia.com/gpu limits pinned to nodeNames.
	dra         bool
	draVersion  string
	nodeNames   []string
	tolerations []corev1.Toleration

	metricsVersion string
	readMetrics    func(ctx context.Context) ([]byte, error)

	// promURL and listScrapePools observe the recipe's Prometheus so the
	// baseline waits for the PodMonitor to be loaded before judging the
	// metric pipeline. A nil listScrapePools skips that wait: the recipe
	// declares no kube-prometheus-stack (attachPrometheus).
	promURL         string
	listScrapePools func(ctx context.Context) ([]byte, error)

	pollInterval          time.Duration
	workloadReadyTimeout  time.Duration
	monitorLoadTimeout    time.Duration
	metricTimeout         time.Duration
	scaleTimeout          time.Duration
	scaleDownTimeout      time.Duration
	negativeControlWindow time.Duration
	stabilization         time.Duration

	phase   hpaPhase
	timings map[hpaPhase]time.Duration
}

// newHPAScenario resolves the GPU allocation mode the way
// secure-accelerator-access does (VERIFY the recipe policy, then SELECT) and
// builds the scenario with production timings.
func newHPAScenario(ctx *validators.Context, dynClient dynamic.Interface, metricsVersion string) (*hpaScenario, error) {
	mode, err := detectGPUAllocationMode(ctx.Ctx, ctx.Clientset, dynClient)
	if err != nil {
		return nil, err
	}
	recordRawTextArtifact(ctx, "GPU allocation mode",
		"kubectl get deviceclass gpu.nvidia.com; kubectl get resourceslices; kubectl get nodes -o wide",
		mode.Summary())
	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	recordRawTextArtifact(ctx, "GPU allocation policy", "", fmt.Sprintf("configured policy: %s", policy))
	if verifyErr := verifyGPUAllocationPolicy(policy, mode); verifyErr != nil {
		return nil, verifyErr
	}

	run, err := newGPUTestRun()
	if err != nil {
		return nil, err
	}
	run.namespace = hpaTestPrefix + run.token
	s := &hpaScenario{
		clientset:             ctx.Clientset,
		dynClient:             dynClient,
		testRun:               run,
		tolerations:           ctx.Tolerations,
		metricsVersion:        metricsVersion,
		pollInterval:          defaults.HPAPollInterval,
		workloadReadyTimeout:  defaults.HPAWorkloadReadyTimeout,
		monitorLoadTimeout:    defaults.PrometheusMonitorLoadTimeout,
		metricTimeout:         defaults.HPAMetricTimeout,
		scaleTimeout:          defaults.HPAScaleTimeout,
		scaleDownTimeout:      defaults.HPAScaleDownTimeout,
		negativeControlWindow: defaults.HPANegativeControlWindow,
		stabilization:         defaults.HPAScaleDownStabilization,
		timings:               map[hpaPhase]time.Duration{},
	}
	s.readMetrics = func(c context.Context) ([]byte, error) {
		return readHPAPodMetrics(c, s.clientset, s.metricsVersion, s.testRun.namespace)
	}

	switch {
	case policy == validatorv1.GPUAllocationPolicyDRAResourceClaim,
		policy == validatorv1.GPUAllocationPolicyUnspecified && mode.DRAUsable:
		s.dra = true
		s.draVersion = mode.APIVersion
		recordRawTextArtifact(ctx, "HPA workload accelerator request", "",
			fmt.Sprintf("mode exercised: DRA (%s ResourceClaimTemplate at %s/%s, one device per pod)",
				draDriverGPU, apiGroupResourceK8sIO, mode.APIVersion))
		validators.EmitExtraOrWarn(validators.GPUAllocationResolvedExtra(true, false))
	case policy == validatorv1.GPUAllocationPolicyDevicePluginExtendedResource,
		policy == validatorv1.GPUAllocationPolicyUnspecified && mode.DevicePluginUsable:
		s.nodeNames = mode.DevicePluginNodes
		recordRawTextArtifact(ctx, "HPA workload accelerator request", "",
			fmt.Sprintf("mode exercised: device plugin (%s: 1 per pod; node affinity: %s)",
				resourceNVIDIAGPU, strings.Join(mode.DevicePluginNodes, ",")))
		validators.EmitExtraOrWarn(validators.GPUAllocationResolvedExtra(false, false))
	default:
		return nil, errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"no usable GPU allocation mechanism for the HPA autoscaling test: %s; %s",
			mode.DRADetail, mode.DevicePluginDetail))
	}
	return s, nil
}

// attachPrometheus points the scenario at the recipe's Prometheus so the
// baseline can wait for the PodMonitor to be loaded before judging the metric
// pipeline (#3671), and preflights its scrape pool API before any resource is
// created. The custom metrics API cannot show whether a monitor is loaded, so
// Prometheus itself is asked. A recipe without kube-prometheus-stack
// (OpenShift serves the adapter from the platform monitoring stack) has no
// Prometheus to ask; the wait is skipped and the metric timeout alone bounds
// the baseline, as before.
func (s *hpaScenario) attachPrometheus(ctx *validators.Context) error {
	if !validators.RecipeDeclares(ctx, prometheusComponentName) {
		recordRawTextArtifact(ctx, "PodMonitor load wait", "",
			"skipped: "+prometheusComponentName+" is not declared in the recipe, so there is no scrape pool API to observe; the metric timeout alone bounds the baseline")
		return nil
	}
	promURL, err := discoverPrometheusURL(ctx)
	if err != nil {
		return err
	}
	listPools := promScrapePoolLister(promURL)
	if err := preflightScrapePools(ctx.Ctx, promURL, listPools); err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Prometheus scrape pool API preflight",
		fmt.Sprintf("curl -sf '%s/api/v1/scrape_pools'", promURL),
		fmt.Sprintf("Endpoint: %s\nResult:   scrape pools listed", promURL))
	s.promURL, s.listScrapePools = promURL, listPools
	return nil
}

// waitForMonitorLoaded holds the baseline until Prometheus has loaded the
// PodMonitor, so the metric timeout that follows measures the scrape and the
// adapter's query rather than prometheus-operator's reconcile schedule.
func (s *hpaScenario) waitForMonitorLoaded(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	prefix := scrapePoolPrefix(monitorKindPod, ns, hpaWorkloadName)
	slog.Info("waiting for Prometheus to load the PodMonitor", "scrapePool", prefix, "timeout", s.monitorLoadTimeout)
	if err := waitForMonitorLoaded(ctx.Ctx, s.listScrapePools, monitorKindPod, ns, hpaWorkloadName,
		s.monitorLoadTimeout, s.pollInterval); err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "HPA phase: PodMonitor loaded",
		fmt.Sprintf("curl -sf '%s/api/v1/scrape_pools' | grep '%s'", s.promURL, prefix),
		fmt.Sprintf("Scrape pool:   %s*\nLoaded after:  %s", prefix, time.Since(start).Round(time.Second)))
	return nil
}

// runWithCleanup creates the per-run namespace, runs the cycle, and always
// tears the namespace down. The return is NAMED so a terminally failed
// cleanup fails an otherwise-passing check: a PASS that leaks two allocated
// accelerators is not a pass. A primary test error always wins.
//
// The teardown is deferred BEFORE the namespace create: a Create that
// returns an error may still have persisted the namespace, and only a
// cleanup registered first can reconcile it (see cleanupGPUTestNamespace).
func (s *hpaScenario) runWithCleanup(ctx *validators.Context) (err error) {
	defer func() {
		status, cleanupErr := cleanupHPAWorkload(s.clientset, s.dynClient, s.testRun)
		recordRawTextArtifact(ctx, "Delete test namespace", cleanupInspectEquivalent(s.testRun.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = createGPUTestNamespace(ctx.Ctx, s.clientset, s.testRun); err != nil {
		return err
	}

	err = s.run(ctx)
	recordRawTextArtifact(ctx, "HPA behavioral test verdict", "", s.verdictSummary(err))
	if err != nil {
		return phaseError(s.phase, err)
	}
	return nil
}

// run executes the phases in order.
func (s *hpaScenario) run(ctx *validators.Context) error {
	s.phase = hpaPhaseDeploy
	if err := s.deploy(ctx); err != nil {
		return err
	}
	if _, err := s.establishBaseline(ctx); err != nil {
		return err
	}
	if upErr := s.scaleUp(ctx); upErr != nil {
		return upErr
	}
	return s.scaleDown(ctx)
}

// verdictSummary renders which phase produced the verdict and how long each
// completed phase took.
func (s *hpaScenario) verdictSummary(err error) string {
	var sb strings.Builder
	if err == nil {
		fmt.Fprintf(&sb, "Verdict:          PASS (all phases completed; last phase: %s)\n", s.phase)
	} else {
		fmt.Fprintf(&sb, "Verdict:          FAIL in phase %s\nError:            %v\n", s.phase, err)
	}
	fmt.Fprintf(&sb, "Metric:           %s (%s/%s, Pods type, target averageValue %d)\n",
		hpaMetricSeries, hpaCustomMetricsGroup, s.metricsVersion, hpaTargetValue)
	fmt.Fprintf(&sb, "Replicas:         min %d, max %d, scale-down stabilization %s\n",
		hpaMinReplicas, hpaMaxReplicas, s.stabilization)
	for _, phase := range []hpaPhase{hpaPhaseDeploy, hpaPhaseBaseline, hpaPhaseNegativeControl, hpaPhaseScaleUp, hpaPhaseScaleDown} {
		if d, ok := s.timings[phase]; ok {
			fmt.Fprintf(&sb, "Phase %-17s %s\n", string(phase)+":", d.Round(time.Second))
		}
	}
	return sb.String()
}

// phaseError names the phase in the error while keeping the inner code, so
// the reported code (what ExitCodeFromError reads) is unchanged.
func phaseError(phase hpaPhase, err error) error {
	return wrapWithCode(err, errors.ErrCodeInternal, fmt.Sprintf("HPA autoscaling test failed in phase %s", phase))
}

// deploy creates the metric ConfigMap at the baseline value, the PodMonitor,
// in DRA mode the ResourceClaimTemplate, and the one-replica Deployment.
func (s *hpaScenario) deploy(ctx *validators.Context) error {
	start := time.Now()
	ns := s.testRun.namespace
	if _, err := s.clientset.CoreV1().ConfigMaps(ns).Create(ctx.Ctx,
		buildHPAMetricConfigMap(ns, hpaBaselineValue), metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create HPA metric ConfigMap", err)
	}
	if _, err := s.dynClient.Resource(podMonitorGVR).Namespace(ns).Create(ctx.Ctx,
		buildHPAPodMonitor(ns), metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to create PodMonitor for the HPA workload — the recipe's kube-prometheus-stack must serve monitoring.coreos.com/v1", err)
	}
	if s.dra {
		if _, err := s.dynClient.Resource(draGVRAt(s.draVersion, "resourceclaimtemplates")).Namespace(ns).Create(ctx.Ctx,
			buildHPAClaimTemplate(ns, s.draVersion), metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create HPA ResourceClaimTemplate", err)
		}
	}
	deployment := buildHPADeployment(ns, s.dra, s.nodeNames, s.tolerations)
	if _, err := s.clientset.AppsV1().Deployments(ns).Create(ctx.Ctx, deployment, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create HPA workload Deployment", err)
	}
	recordRawTextArtifact(ctx, "HPA test resources",
		fmt.Sprintf("kubectl get configmap,podmonitor,resourceclaimtemplate,deploy,hpa,pods -n %s", ns),
		fmt.Sprintf("Namespace:    %s\nConfigMap:    %s (%s=%d)\nPodMonitor:   %s (scrape %s)\nDeployment:   %s (replicas %d, image %s)\nHPA:          %s (created after the baseline is established)",
			ns, hpaWorkloadName, hpaMetricValueKey, hpaBaselineValue, hpaWorkloadName, defaults.HPAScrapeInterval,
			hpaWorkloadName, hpaMinReplicas, defaults.PythonProbeImage, hpaWorkloadName))
	s.timings[hpaPhaseDeploy] = time.Since(start)
	return nil
}

// establishBaseline waits for the baseline pod and its metric, creates the
// HPA, requires it to reconcile at one replica, and holds the negative
// control.
func (s *hpaScenario) establishBaseline(ctx *validators.Context) (hpaState, error) {
	s.phase = hpaPhaseBaseline
	start := time.Now()
	slog.Info("waiting for the baseline accelerator pod to become Ready", "namespace", s.testRun.namespace)
	state, err := s.waitForState(ctx.Ctx, s.workloadReadyTimeout, func(st hpaState) error {
		if st.hpa != nil {
			return errors.New(errors.ErrCodeConflict, "HPA exists before the baseline was established")
		}
		return checkHPAWorkload(st, hpaMinReplicas)
	})
	if err != nil {
		if problems := hpaPodPrerequisiteProblems(state.allPods()); len(problems) > 0 {
			err = errors.Wrap(errors.ErrCodeUnavailable, strings.Join(problems, "; "), err)
		}
		return hpaState{}, wrapWithCode(err, errors.ErrCodeTimeout, "baseline accelerator pod did not become Ready")
	}

	// The monitor-load wait comes after the pod is Ready, not before: the
	// two overlap in wall time, and a workload pod that never comes up is
	// reported as such rather than as a monitor Prometheus did not load.
	pipeline := "custom metrics pipeline does not serve the workload metric — check the PodMonitor scrape and the prometheus-adapter rule for " + hpaMetricSeries
	if s.listScrapePools != nil {
		if loadErr := s.waitForMonitorLoaded(ctx); loadErr != nil {
			return hpaState{}, loadErr
		}
		pipeline = "Prometheus loaded the PodMonitor, but the " + pipeline
	}
	slog.Info("waiting for the custom metrics API to report the baseline value", "value", hpaBaselineValue, "pods", state.podNames())
	if metricErr := s.waitForMetric(ctx.Ctx, hpaBaselineValue, fixedPods(state.podNames())); metricErr != nil {
		return hpaState{}, wrapWithCode(metricErr, errors.ErrCodeNotFound, pipeline)
	}

	// Created only now, so any scaling from here on is the HPA's.
	if _, createErr := s.clientset.AutoscalingV2().HorizontalPodAutoscalers(s.testRun.namespace).Create(ctx.Ctx,
		buildHPA(s.testRun.namespace, s.stabilization), metav1.CreateOptions{}); createErr != nil {
		return hpaState{}, errors.Wrap(errors.ErrCodeInternal, "failed to create HorizontalPodAutoscaler", createErr)
	}
	state, err = s.waitForState(ctx.Ctx, s.metricTimeout, func(st hpaState) error {
		return checkHPAScaledTo(st, hpaMinReplicas, hpaBaselineValue)
	})
	if err != nil {
		return hpaState{}, hpaPhaseFailure(err, "HPA did not reconcile on the custom metric at baseline", state)
	}
	s.timings[hpaPhaseBaseline] = time.Since(start)
	recordRawTextArtifact(ctx, "HPA phase: baseline",
		fmt.Sprintf("kubectl get hpa %s -n %s -o yaml", hpaWorkloadName, s.testRun.namespace),
		fmt.Sprintf("Pods:             %s\nMetric reported:  %d for every pod\nHPA:              ScalingActive=True, average %d, desired %d, current %d\nElapsed:          %s",
			strings.Join(state.podNames(), ","), hpaBaselineValue, hpaBaselineValue, hpaMinReplicas, hpaMinReplicas,
			s.timings[hpaPhaseBaseline].Round(time.Second)))

	s.phase = hpaPhaseNegativeControl
	start = time.Now()
	slog.Info("negative control: the HPA must hold one replica while the metric is below target", "window", s.negativeControlWindow)
	last, holdErr := s.holdState(ctx.Ctx, s.negativeControlWindow, func(st hpaState) error {
		return checkHPADecided(st, hpaMinReplicas, hpaBaselineValue)
	})
	if holdErr != nil {
		return hpaState{}, hpaPhaseFailure(holdErr, fmt.Sprintf("HPA did not hold %d replica with the metric below target", hpaMinReplicas), last)
	}
	s.timings[hpaPhaseNegativeControl] = time.Since(start)
	recordRawTextArtifact(ctx, "HPA phase: negative control", "",
		fmt.Sprintf("Window:           %s\nMetric:           %d (target %d)\nResult:           HPA kept deciding on %d replica; Deployment stayed at %d",
			s.negativeControlWindow, hpaBaselineValue, hpaTargetValue, hpaMinReplicas, hpaMinReplicas))
	return state, nil
}

// scaleUp raises the metric and requires the HPA to scale to hpaMaxReplicas
// with every pod Running, Ready and reporting the raised value. The raised
// metric is awaited on the live Ready pods, not the baseline's names: the
// negative control tolerates a pod being replaced, and the replacement
// reports under its own name; and with no scale-up stabilization the HPA may
// add a pod that is still Pending before this wait ends, which has no series
// yet. The scaled-up wait below covers the pods the HPA adds.
func (s *hpaScenario) scaleUp(ctx *validators.Context) error {
	s.phase = hpaPhaseScaleUp
	start := time.Now()
	slog.Info("raising the custom metric", "value", hpaScaleUpValue, "target", hpaTargetValue)
	if err := setHPAMetricValue(ctx.Ctx, s.clientset, s.testRun.namespace, hpaScaleUpValue); err != nil {
		return err
	}
	if err := s.waitForMetric(ctx.Ctx, hpaScaleUpValue, s.readyLivePods); err != nil {
		return wrapWithCode(err, errors.ErrCodeNotFound, "custom metrics pipeline did not report the raised metric")
	}

	deadline := time.Now().Add(s.scaleTimeout)
	state, err := s.waitForState(ctx.Ctx, time.Until(deadline), func(st hpaState) error {
		return checkHPADecided(st, hpaMaxReplicas, hpaScaleUpValue)
	})
	if err != nil {
		return hpaPhaseFailure(err, fmt.Sprintf("HPA did not scale the Deployment to %d replicas on the raised metric", hpaMaxReplicas), state)
	}
	decided := time.Since(start)
	slog.Info("HPA set the Deployment to the maximum; waiting for the new accelerator pods", "replicas", hpaMaxReplicas)

	// The scaled-up state and the raised metric for exactly its pods must hold
	// in one observation; the extra metricTimeout lets the new pods' samples
	// reach the custom metrics API.
	var metricProblem error
	state, err = s.waitForState(ctx.Ctx, time.Until(deadline.Add(s.metricTimeout)), func(st hpaState) error {
		metricProblem = nil
		if problem := checkHPAScaledTo(st, hpaMaxReplicas, hpaScaleUpValue); problem != nil {
			return problem
		}
		body, readErr := s.readMetrics(ctx.Ctx)
		if readErr == nil {
			readErr = checkHPAMetricSamples(body, s.testRun.namespace, st.podNames(), hpaScaleUpValue)
		}
		if readErr != nil {
			metricProblem = readErr
			return wrapWithCode(readErr, errors.ErrCodeNotFound, "custom metric for the scaled-up pods")
		}
		return nil
	})
	switch {
	case err == nil:
	case metricProblem != nil:
		return wrapWithCode(err, errors.ErrCodeNotFound,
			"custom metrics pipeline did not report the raised metric for the scaled-up pods")
	default:
		if checkHPAStatus(state.hpa, hpaMaxReplicas, hpaScaleUpValue) == nil {
			if problems := hpaPodPrerequisiteProblems(state.allPods()); len(problems) > 0 {
				return errors.Wrap(errors.ErrCodeUnavailable,
					"scaled-up pods cannot start: "+strings.Join(problems, "; "), err)
			}
		}
		return hpaPhaseFailure(err, fmt.Sprintf("the %d scaled-up pods did not all become Running and Ready", hpaMaxReplicas), state)
	}
	s.timings[hpaPhaseScaleUp] = time.Since(start)
	recordRawTextArtifact(ctx, "HPA phase: scale-up",
		fmt.Sprintf("kubectl get hpa,deploy,pods -n %s", s.testRun.namespace),
		fmt.Sprintf("Metric raised to: %d (target %d)\nHPA decided on %d replicas after %s\nPods:             %s (all Running and Ready, each reporting %d)\nElapsed:          %s",
			hpaScaleUpValue, hpaTargetValue, hpaMaxReplicas, decided.Round(time.Second),
			strings.Join(state.podNames(), ","), hpaScaleUpValue, s.timings[hpaPhaseScaleUp].Round(time.Second)))
	return nil
}

// scaleDown lowers the metric and requires the HPA to return to one replica.
func (s *hpaScenario) scaleDown(ctx *validators.Context) error {
	s.phase = hpaPhaseScaleDown
	start := time.Now()
	slog.Info("lowering the custom metric", "value", hpaScaleDownValue, "target", hpaTargetValue)
	if err := setHPAMetricValue(ctx.Ctx, s.clientset, s.testRun.namespace, hpaScaleDownValue); err != nil {
		return err
	}
	if err := s.waitForMetric(ctx.Ctx, hpaScaleDownValue, s.livePods); err != nil {
		return wrapWithCode(err, errors.ErrCodeNotFound, "custom metrics pipeline did not report the lowered metric")
	}
	lowered := time.Since(start)

	slog.Info("waiting for the HPA to scale down after its stabilization window",
		"timeout", s.scaleDownTimeout, "stabilization", s.stabilization)
	state, err := s.waitForState(ctx.Ctx, s.scaleDownTimeout, func(st hpaState) error {
		return checkHPAScaledTo(st, hpaMinReplicas, hpaScaleDownValue)
	})
	if err != nil {
		return hpaPhaseFailure(err, fmt.Sprintf("HPA did not scale back down to %d replica", hpaMinReplicas), state)
	}
	s.timings[hpaPhaseScaleDown] = time.Since(start)
	// The stabilization window is reported, not asserted: how soon a correct
	// HPA scales down also depends on its sync period.
	recordRawTextArtifact(ctx, "HPA phase: scale-down",
		fmt.Sprintf("kubectl get hpa,deploy,pods -n %s", s.testRun.namespace),
		fmt.Sprintf("Metric lowered to: %d (target %d), reported after %s\nScaled down to:    %d replica (%s) %s after the metric was lowered\nStabilization:     %s\nElapsed:           %s",
			hpaScaleDownValue, hpaTargetValue, lowered.Round(time.Second), hpaMinReplicas,
			strings.Join(state.podNames(), ","), s.timings[hpaPhaseScaleDown].Round(time.Second),
			s.stabilization, s.timings[hpaPhaseScaleDown].Round(time.Second)))
	return nil
}

// hpaPhaseFailure classifies a failed phase from its last observation: an
// unreadable metric (ScalingActive=False, FailedGetPodsMetric) is the
// pipeline's fault; otherwise the HPA's conditions are attached.
func hpaPhaseFailure(err error, what string, last hpaState) error {
	if message, ok := hpaMetricsUnreadable(last.hpa); ok {
		return errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("%s: the HPA controller cannot read the custom metric (FailedGetPodsMetric: %s)", what, message), err)
	}
	if last.hpa != nil && len(last.hpa.Status.Conditions) > 0 {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("%s; HPA conditions: %s", what, hpaConditionsSummary(last.hpa)), err)
	}
	return wrapWithCode(err, errors.ErrCodeInternal, what)
}

// hpaMetricsUnreadable returns the HPA's own report that it cannot read its
// metric.
func hpaMetricsUnreadable(hpa *autoscalingv2.HorizontalPodAutoscaler) (string, bool) {
	if hpa == nil {
		return "", false
	}
	for _, c := range hpa.Status.Conditions {
		if c.Type == autoscalingv2.ScalingActive && c.Status == corev1.ConditionFalse && c.Reason == "FailedGetPodsMetric" {
			return c.Message, true
		}
	}
	return "", false
}

func hpaConditionsSummary(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	parts := make([]string, 0, len(hpa.Status.Conditions))
	for _, c := range hpa.Status.Conditions {
		parts = append(parts, fmt.Sprintf("%s=%s (%s: %s)", c.Type, c.Status, c.Reason, hpaOneLine(c.Message)))
	}
	return strings.Join(parts, "; ")
}

// hpaOneLine shortens a message for an error line.
func hpaOneLine(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return "(none recorded)"
	}
	const limit = 300
	if len(message) > limit {
		message = "..." + message[len(message)-limit:]
	}
	return message
}

// hpaAdapterComponent names the recipe component that serves the custom
// metrics API and whether the recipe declares it: prometheus-adapter-ocp when
// the OpenShift overlay declares it (it disables prometheus-adapter), else
// prometheus-adapter.
func hpaAdapterComponent(ctx *validators.Context) (string, bool) {
	for _, name := range []string{"prometheus-adapter", "prometheus-adapter-ocp"} {
		if validators.RecipeDeclares(ctx, name) {
			return name, true
		}
	}
	return "prometheus-adapter", false
}

// resolveCustomMetricsVersion picks the custom metrics API version the way
// the HPA controller's client does: the preferred version if supported, else
// the first supported one listed. It returns "" without error when the group
// is not served at all (the clean-absence signal), and an error when the
// group is registered but no usable version is available.
func resolveCustomMetricsVersion(c kubernetes.Interface) (string, error) {
	groups, err := c.Discovery().ServerGroups()
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to discover API groups", err)
	}
	var group *metav1.APIGroup
	for i := range groups.Groups {
		if groups.Groups[i].Name == hpaCustomMetricsGroup {
			group = &groups.Groups[i]
		}
	}
	if group == nil {
		return "", nil
	}
	version := ""
	if slices.Contains(hpaCustomMetricsVersions, group.PreferredVersion.Version) {
		version = group.PreferredVersion.Version
	} else {
		for _, v := range group.Versions {
			if slices.Contains(hpaCustomMetricsVersions, v.Version) {
				version = v.Version
				break
			}
		}
	}
	switch {
	case version == "" && len(group.Versions) == 0:
		// Aggregated discovery omits versions whose APIService is unavailable.
		return "", errors.New(errors.ErrCodeUnavailable, hpaCustomMetricsGroup+
			" is registered but none of its versions is available — check the prometheus-adapter APIService and pods")
	case version == "":
		return "", errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"%s is served in versions %v, none of which the HPA supports (%v)", hpaCustomMetricsGroup, group.Versions, hpaCustomMetricsVersions))
	}
	if _, err := c.Discovery().ServerResourcesForGroupVersion(hpaCustomMetricsGroup + "/" + version); err != nil {
		return "", errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("custom metrics API %s/%s is registered but not available", hpaCustomMetricsGroup, version), err)
	}
	return version, nil
}

// readHPAPodMetrics reads the workload pods' metric from the custom metrics
// API, scoped by the workload label selector the way the HPA controller does.
func readHPAPodMetrics(ctx context.Context, c kubernetes.Interface, version, namespace string) ([]byte, error) {
	result := c.CoreV1().RESTClient().Get().
		AbsPath("/apis", hpaCustomMetricsGroup, version).
		Namespace(namespace).Resource("pods").Name("*").SubResource(hpaMetricSeries).
		Param("labelSelector", hpaWorkloadSelector()).Do(ctx)
	// Error decodes the adapter's Status (why a metric is not found); DoRaw
	// would reduce it to a generic message.
	if err := result.Error(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to read %s from %s/%s", hpaMetricSeries, hpaCustomMetricsGroup, version), err)
	}
	body, err := result.Raw()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read the custom metrics response body", err)
	}
	return body, nil
}

// hpaMetricValueList decodes both custom.metrics.k8s.io/v1beta2 (metric.name)
// and v1beta1 (metricName) MetricValueList responses.
type hpaMetricValueList struct {
	Items []struct {
		DescribedObject struct {
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"describedObject"`
		MetricName string `json:"metricName"`
		Metric     struct {
			Name string `json:"name"`
		} `json:"metric"`
		Value resource.Quantity `json:"value"`
	} `json:"items"`
}

// checkHPAMetricSamples requires every expected pod to report exactly want.
// Samples for other pods (e.g. ones still terminating) are ignored.
func checkHPAMetricSamples(body []byte, namespace string, expectedPods []string, want int) error {
	var list hpaMetricValueList
	if err := json.Unmarshal(body, &list); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to decode custom metrics response", err)
	}
	wantQ := resource.MustParse(strconv.Itoa(want))
	got := map[string]resource.Quantity{}
	var problems []string
	for _, item := range list.Items {
		obj := item.DescribedObject
		name := item.Metric.Name
		if name == "" {
			name = item.MetricName
		}
		switch {
		case obj.Kind != "Pod" || obj.Namespace != namespace:
			problems = append(problems, fmt.Sprintf("unexpected object %s %s/%s", obj.Kind, obj.Namespace, obj.Name))
			continue
		case name != hpaMetricSeries:
			problems = append(problems, fmt.Sprintf("pod %s reports metric %q, want %q", obj.Name, name, hpaMetricSeries))
			continue
		}
		if prev, dup := got[obj.Name]; dup && prev.Cmp(item.Value) != 0 {
			problems = append(problems, fmt.Sprintf("pod %s has conflicting values %s and %s", obj.Name, prev.String(), item.Value.String()))
			continue
		}
		got[obj.Name] = item.Value
	}
	for _, pod := range expectedPods {
		v, ok := got[pod]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("no sample for pod %s", pod))
		case v.Cmp(wantQ) != 0:
			problems = append(problems, fmt.Sprintf("pod %s reports %s, want %d", pod, v.String(), want))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(errors.ErrCodeNotFound, strings.Join(problems, "; "))
	}
	return nil
}

// hpaState is one observation of the HPA, the Deployment and its pods.
type hpaState struct {
	hpa        *autoscalingv2.HorizontalPodAutoscaler // nil until created
	deployment *appsv1.Deployment
	pods       []corev1.Pod // not terminating or finished, sorted by name
	inactive   []corev1.Pod // terminating, Succeeded or Failed
}

func getHPAState(ctx context.Context, c kubernetes.Interface, namespace string) (hpaState, error) {
	hpa, err := c.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, hpaWorkloadName, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(err):
		hpa = nil // not created yet
	case err != nil:
		return hpaState{}, errors.Wrap(errors.ErrCodeInternal, "failed to get the HPA", err)
	}
	deployment, err := c.AppsV1().Deployments(namespace).Get(ctx, hpaWorkloadName, metav1.GetOptions{})
	if err != nil {
		return hpaState{}, errors.Wrap(errors.ErrCodeInternal, "failed to get the HPA workload Deployment", err)
	}
	list, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: hpaWorkloadSelector()})
	if err != nil {
		return hpaState{}, errors.Wrap(errors.ErrCodeInternal, "failed to list the HPA workload pods", err)
	}
	s := hpaState{hpa: hpa, deployment: deployment}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			s.pods = append(s.pods, pod)
		} else {
			s.inactive = append(s.inactive, pod)
		}
	}
	return s, nil
}

func (s hpaState) podNames() []string {
	names := make([]string, 0, len(s.pods))
	for _, pod := range s.pods {
		names = append(names, pod.Name)
	}
	return names
}

// allPods returns the active and inactive workload pods.
func (s hpaState) allPods() []corev1.Pod {
	return slices.Concat(s.pods, s.inactive)
}

// checkHPAStatus requires the HPA to be active on the custom metric, to have
// recorded exactly the phase's value as its average, and to decide on want
// replicas. The HPA records the plain mean of its pods' samples, so once every
// pod reports value the average is exactly value; matching it, rather than
// just the side of the target, ties the decision to the current phase.
func checkHPAStatus(hpa *autoscalingv2.HorizontalPodAutoscaler, want int32, value int) error {
	if hpa == nil {
		return errors.New(errors.ErrCodeNotFound, "HPA does not exist")
	}
	// Released HPA controllers do not set status.observedGeneration, so it is
	// checked only when reported.
	if og := hpa.Status.ObservedGeneration; og != nil && *og < hpa.Generation {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("HPA status has not observed generation %d", hpa.Generation))
	}
	var active *autoscalingv2.HorizontalPodAutoscalerCondition
	for i := range hpa.Status.Conditions {
		if hpa.Status.Conditions[i].Type == autoscalingv2.ScalingActive {
			active = &hpa.Status.Conditions[i]
		}
	}
	if active == nil || active.Status != corev1.ConditionTrue {
		reason := "not reported"
		if active != nil {
			reason = fmt.Sprintf("%s (%s: %s)", active.Status, active.Reason, active.Message)
		}
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("HPA condition ScalingActive is %s", reason))
	}
	var current *resource.Quantity
	for _, m := range hpa.Status.CurrentMetrics {
		if m.Type == autoscalingv2.PodsMetricSourceType && m.Pods != nil && m.Pods.Metric.Name == hpaMetricSeries {
			current = m.Pods.Current.AverageValue
		}
	}
	if current == nil {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("HPA status.currentMetrics has no Pods metric %q", hpaMetricSeries))
	}
	if wantQ := resource.MustParse(strconv.Itoa(value)); current.Cmp(wantQ) != 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"HPA recorded average %s for %q, want the current value %d", current.String(), hpaMetricSeries, value))
	}
	if hpa.Status.DesiredReplicas != want {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("HPA desiredReplicas=%d, want %d", hpa.Status.DesiredReplicas, want))
	}
	return nil
}

// checkHPAWorkload requires the Deployment and its active pods to agree on
// want Running and Ready replicas. Only the HPA changes spec.replicas.
func checkHPAWorkload(s hpaState, want int32) error {
	d := s.deployment
	if d == nil {
		return errors.New(errors.ErrCodeNotFound, "Deployment does not exist")
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != want {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("Deployment spec.replicas=%d, want %d", ptr.Deref(d.Spec.Replicas, 0), want))
	}
	if d.Status.ObservedGeneration < d.Generation {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("Deployment status has not observed generation %d", d.Generation))
	}
	if d.Status.Replicas != want || d.Status.UpdatedReplicas != want || d.Status.ReadyReplicas != want {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("Deployment status replicas=%d updated=%d ready=%d, want %d",
			d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.ReadyReplicas, want))
	}
	if int32(len(s.pods)) != want { //nolint:gosec // G115: pod counts are tiny
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("found %d active workload pods %v, want %d", len(s.pods), s.podNames(), want))
	}
	for i := range s.pods {
		pod := &s.pods[i]
		if pod.Status.Phase != corev1.PodRunning || !podIsReady(pod) || pod.Spec.NodeName == "" {
			return errors.New(errors.ErrCodeInternal, fmt.Sprintf("pod %s is %s (ready=%t, node=%q), want Running and Ready",
				pod.Name, pod.Status.Phase, podIsReady(pod), pod.Spec.NodeName))
		}
	}
	return nil
}

func checkHPAScaledTo(s hpaState, want int32, value int) error {
	if err := checkHPAStatus(s.hpa, want, value); err != nil {
		return err
	}
	if s.hpa.Status.CurrentReplicas != want {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("HPA currentReplicas=%d, want %d", s.hpa.Status.CurrentReplicas, want))
	}
	return checkHPAWorkload(s, want)
}

// checkHPADecided requires the HPA to decide on want replicas for value and
// the Deployment to be set to want. It does not check the pods, so an
// eviction during the negative control does not fail it.
func checkHPADecided(s hpaState, want int32, value int) error {
	if err := checkHPAStatus(s.hpa, want, value); err != nil {
		return err
	}
	if s.deployment == nil {
		return errors.New(errors.ErrCodeNotFound, "Deployment does not exist")
	}
	if n := ptr.Deref(s.deployment.Spec.Replicas, 0); n != want {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("Deployment spec.replicas=%d, want %d", n, want))
	}
	return nil
}

// hpaPodPrerequisiteProblems lists pods held back by something other than the
// HPA: unschedulable, unable to pull or start, a container that exited (e.g.
// the accelerator check), or a pod its node ended. Pods being deleted are
// skipped, since their containers stop as part of the removal.
func hpaPodPrerequisiteProblems(pods []corev1.Pod) []string {
	var out []string
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if reason := podStuckReason(pod); reason != "" {
			out = append(out, fmt.Sprintf("%s: %s", pod.Name, reason))
			continue
		}
		reported := len(out)
		for _, cs := range pod.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				out = append(out, fmt.Sprintf("%s: container exited with code %d (%s), restarts=%d; output: %s",
					pod.Name, t.ExitCode, t.Reason, cs.RestartCount, hpaOneLine(t.Message)))
			} else if w := cs.State.Waiting; w != nil && w.Reason == waitingReasonCrashLoopBackOff {
				if lt := cs.LastTerminationState.Terminated; lt != nil {
					out = append(out, fmt.Sprintf("%s: container in CrashLoopBackOff after exit code %d, restarts=%d; output: %s",
						pod.Name, lt.ExitCode, cs.RestartCount, hpaOneLine(lt.Message)))
				}
			}
		}
		if pod.Status.Phase == corev1.PodFailed && len(out) == reported {
			out = append(out, fmt.Sprintf("%s failed (%s: %s)", pod.Name, pod.Status.Reason, hpaOneLine(pod.Status.Message)))
		}
	}
	return out
}

func fixedPods(names []string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return names, nil }
}

// livePods returns the current active pods, for phases in which the HPA may
// remove pods while the check waits for their metric.
func (s *hpaScenario) livePods(ctx context.Context) ([]string, error) {
	st, err := getHPAState(ctx, s.clientset, s.testRun.namespace)
	return st.podNames(), err
}

// readyLivePods returns the current active pods that are Ready, for the wait
// on the raised metric: a pod the HPA has just added is Pending or not yet
// Ready, and has no series to await.
func (s *hpaScenario) readyLivePods(ctx context.Context) ([]string, error) {
	st, err := getHPAState(ctx, s.clientset, s.testRun.namespace)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(st.pods))
	for i := range st.pods {
		if podIsReady(&st.pods[i]) {
			names = append(names, st.pods[i].Name)
		}
	}
	return names, nil
}

// waitForMetric waits until the custom metrics API reports want for every pod
// returned by expected. Adapters answer NotFound until a new series is
// scraped, so only a definitive non-NotFound API verdict ends the wait early.
//
// Polling, not a watch: the aggregated custom metrics API has no watch verb.
func (s *hpaScenario) waitForMetric(ctx context.Context, want int, expected func(context.Context) ([]string, error)) error {
	var pods []string
	var lastSamples, lastAPIError error
	err := wait.PollUntilContextTimeout(ctx, s.pollInterval, s.metricTimeout, true, func(ctx context.Context) (bool, error) {
		var listErr error
		if pods, listErr = expected(ctx); listErr != nil {
			lastAPIError = listErr
			return false, nil
		}
		if len(pods) == 0 {
			lastSamples = errors.New(errors.ErrCodeNotFound, "no live workload pods")
			return false, nil
		}
		body, readErr := s.readMetrics(ctx)
		if readErr != nil {
			lastAPIError = readErr
			if isPermanentAPIError(readErr) {
				return false, readErr
			}
			return false, nil
		}
		lastAPIError = nil
		lastSamples = checkHPAMetricSamples(body, s.testRun.namespace, pods, want)
		return lastSamples == nil, nil
	})
	if err == nil {
		return nil
	}
	if isPermanentAPIError(err) {
		return errors.Wrap(errors.ErrCodeUnavailable, "custom metrics API rejected the metric read", err)
	}
	detail := ""
	if lastSamples != nil {
		detail = "; last samples: " + lastSamples.Error()
	}
	if lastAPIError != nil {
		detail += "; last API error: " + lastAPIError.Error()
	}
	code := errors.ErrCodeNotFound
	if ctx.Err() != nil {
		code = errors.ErrCodeTimeout
	}
	return errors.Wrap(code, fmt.Sprintf("custom metric %q did not report %d for pods %v within %s%s",
		hpaMetricSeries, want, pods, s.metricTimeout, detail), err)
}

// waitForState polls until check accepts an observation, and returns it (or
// the last one seen). Observing the HPA, the Deployment and the pods together
// is a multi-object state, so it polls rather than watching one object.
func (s *hpaScenario) waitForState(ctx context.Context, timeout time.Duration, check func(hpaState) error) (hpaState, error) {
	var last hpaState
	var lastProblem, lastAPIError error
	err := wait.PollUntilContextTimeout(ctx, s.pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		st, readErr := getHPAState(ctx, s.clientset, s.testRun.namespace)
		if readErr != nil {
			if ctx.Err() != nil {
				return false, nil // the wait's own deadline cut the request short
			}
			lastAPIError = readErr
			if isPermanentAPIError(readErr) {
				return false, classifyK8sReadError(readErr, "HPA test workload")
			}
			return false, nil
		}
		last = st
		lastAPIError = nil
		lastProblem = check(st)
		return lastProblem == nil, nil
	})
	switch {
	case err == nil:
		return last, nil
	case ctx.Err() != nil:
		return last, errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf("check budget exhausted; last observation: %v", lastProblem), ctx.Err())
	case wait.Interrupted(err):
		msg := fmt.Sprintf("not reached within %s; last observation: %v", timeout, lastProblem)
		if lastAPIError != nil {
			msg += fmt.Sprintf("; last API error: %v", lastAPIError)
		}
		return last, errors.Wrap(errors.ErrCodeTimeout, msg, err)
	default:
		return last, wrapWithCode(err, errors.ErrCodeInternal, "failed to observe the HPA test workload")
	}
}

// holdState requires check to accept every observation for window, including
// one at or after its end.
func (s *hpaScenario) holdState(ctx context.Context, window time.Duration, check func(hpaState) error) (hpaState, error) {
	end := time.Now().Add(window)
	var last hpaState
	for {
		st, err := getHPAState(ctx, s.clientset, s.testRun.namespace)
		switch {
		case err == nil:
			last = st
			if problem := check(st); problem != nil {
				return last, problem
			}
			if !time.Now().Before(end) {
				return last, nil
			}
		case isPermanentAPIError(err):
			return last, classifyK8sReadError(err, "HPA test workload")
		default:
			slog.Debug("transient read during the negative control; retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return last, errors.Wrap(errors.ErrCodeTimeout, "check budget exhausted during the negative control", ctx.Err())
		case <-time.After(s.pollInterval):
		}
	}
}

// isPermanentAPIError reports a definitive API verdict, such as Forbidden.
// NotFound and transient errors (timeouts, throttling, transport) are not
// permanent, so polls retry them.
func isPermanentAPIError(err error) bool {
	var status k8serrors.APIStatus
	if !stderrors.As(err, &status) {
		return false
	}
	return !k8serrors.IsNotFound(err) && !isK8sTimeoutErr(err) &&
		!k8serrors.IsTooManyRequests(err) && !k8serrors.IsServerTimeout(err) &&
		!k8serrors.IsServiceUnavailable(err) && !k8serrors.IsInternalError(err)
}

// setHPAMetricValue changes the value every workload pod reports; the kubelet
// propagates the ConfigMap update into the mounted volume.
func setHPAMetricValue(ctx context.Context, c kubernetes.Interface, namespace string, value int) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := c.CoreV1().ConfigMaps(namespace).Get(ctx, hpaWorkloadName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[hpaMetricValueKey] = strconv.Itoa(value)
		_, err = c.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to set the HPA metric ConfigMap to %d", value), err)
	}
	return nil
}

func hpaWorkloadLabels() map[string]string {
	return map[string]string{hpaAppLabelKey: hpaWorkloadName, hpaMetricSourceLabel: "true"}
}

func hpaWorkloadSelector() string {
	return labels.SelectorFromSet(hpaWorkloadLabels()).String()
}

func buildHPAMetricConfigMap(namespace string, value int) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hpaWorkloadName, Namespace: namespace},
		Data:       map[string]string{hpaMetricValueKey: strconv.Itoa(value)},
	}
}

// buildHPAPodMonitor makes the recipe's Prometheus scrape the workload pods'
// metrics port, attaching namespace and pod labels the adapter rule maps to
// Kubernetes resources.
func buildHPAPodMonitor(namespace string) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{Object: map[string]any{
		keySpec: map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]any{hpaMetricSourceLabel: "true"},
			},
			"namespaceSelector": map[string]any{
				"matchNames": []any{namespace},
			},
			"podMetricsEndpoints": []any{map[string]any{
				"port":     hpaMetricsPortName,
				"path":     "/metrics",
				"interval": defaults.HPAScrapeInterval.String(),
			}},
		},
	}}
	pm.SetAPIVersion(podMonitorGVR.GroupVersion().String())
	pm.SetKind("PodMonitor")
	pm.SetName(hpaWorkloadName)
	pm.SetNamespace(namespace)
	pm.SetLabels(map[string]string{prometheusReleaseLabel: prometheusComponentName})
	return pm
}

// buildHPAClaimTemplate returns the ResourceClaimTemplate each workload pod
// draws one gpu.nvidia.com device from, shaped for the served version.
func buildHPAClaimTemplate(namespace, version string) *unstructured.Unstructured {
	request := map[string]any{keyName: gpuClaimName}
	if version == versionV1beta1 {
		request["deviceClassName"] = draDriverGPU
		request[keyAllocationMode] = allocationModeExactCount
		request["count"] = int64(1)
	} else {
		request["exactly"] = map[string]any{
			"deviceClassName": draDriverGPU,
			keyAllocationMode: allocationModeExactCount,
			"count":           int64(1),
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		keyAPIVersion: apiGroupResourceK8sIO + "/" + version,
		keyKind:       "ResourceClaimTemplate",
		keyMetadata:   map[string]any{keyName: hpaWorkloadName, keyNamespace: namespace},
		keySpec: map[string]any{
			keySpec: map[string]any{
				"devices": map[string]any{"requests": []any{request}},
			},
		},
	}}
}

// buildHPADeployment returns the one-replica Deployment of metric-serving
// accelerator pods. In DRA mode each pod draws its device from the
// ResourceClaimTemplate; otherwise it requests nvidia.com/gpu: 1 and is
// pinned to the probe's device-plugin nodes (see secure-accelerator-access).
// tolerations, when nil, defaults to tolerate-all.
func buildHPADeployment(namespace string, dra bool, nodeNames []string, tolerations []corev1.Toleration) *appsv1.Deployment {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	container := corev1.Container{
		Name:    hpaContainerName,
		Image:   defaults.PythonProbeImage,
		Command: []string{posixShell, "-c"},
		Args:    []string{hpaWorkloadCommand},
		Env: []corev1.EnvVar{
			{Name: "HPA_METRIC_SERVER", Value: hpaMetricServerScript},
			{Name: "HPA_METRIC_FILE", Value: hpaMetricMountPath + "/" + hpaMetricValueKey},
			{Name: "HPA_METRIC_SERIES", Value: hpaMetricSeries},
			{Name: "HPA_METRIC_PORT", Value: strconv.Itoa(hpaMetricsPort)},
		},
		Ports: []corev1.ContainerPort{{Name: hpaMetricsPortName, ContainerPort: hpaMetricsPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString(hpaMetricsPortName)}},
			PeriodSeconds: 5,
		},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}},
		// The whole ConfigMap directory, never a subPath: subPath mounts do
		// not receive ConfigMap updates.
		VolumeMounts: []corev1.VolumeMount{{Name: "metric", MountPath: hpaMetricMountPath, ReadOnly: true}},
		// Records the accelerator check's output in the container status.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	spec := corev1.PodSpec{
		Tolerations:                   tolerations,
		TerminationGracePeriodSeconds: ptr.To(hpaTerminationGracePeriod),
		Volumes: []corev1.Volume{{
			Name: "metric",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: hpaWorkloadName},
			}},
		}},
	}
	if dra {
		spec.ResourceClaims = []corev1.PodResourceClaim{{Name: gpuClaimName, ResourceClaimTemplateName: ptr.To(hpaWorkloadName)}}
		container.Resources.Claims = []corev1.ResourceClaim{{Name: gpuClaimName}}
	} else {
		container.Resources.Limits = corev1.ResourceList{corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1")}
		if len(nodeNames) > 0 {
			spec.Affinity = nodeNameAffinity(nodeNames)
		}
	}
	spec.Containers = []corev1.Container{container}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: hpaWorkloadName, Namespace: namespace, Labels: hpaWorkloadLabels()},
		Spec: appsv1.DeploymentSpec{
			Replicas: helper.Int32Ptr(hpaMinReplicas),
			Selector: &metav1.LabelSelector{MatchLabels: hpaWorkloadLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: hpaWorkloadLabels()},
				Spec:       spec,
			},
		},
	}
}

// buildHPA returns the autoscaling/v2 HPA on the per-pod custom metric.
func buildHPA(namespace string, stabilization time.Duration) *autoscalingv2.HorizontalPodAutoscaler {
	target := resource.MustParse(strconv.Itoa(hpaTargetValue))
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: hpaWorkloadName, Namespace: namespace},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       hpaWorkloadName,
			},
			MinReplicas: helper.Int32Ptr(hpaMinReplicas),
			MaxReplicas: hpaMaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: hpaMetricSeries},
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: &target},
				},
			}},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp:   &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: helper.Int32Ptr(0)},
				ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: helper.Int32Ptr(int32(stabilization / time.Second))}, //nolint:gosec // G115: a minute in seconds
			},
		},
	}
}

// cleanupHPAWorkload deletes the HPA and the Deployment (so the controllers
// stop reconciling while the namespace terminates) and the PodMonitor (the
// scrape configuration must not outlive the run), then the per-run namespace,
// which takes the pods, their claims and the ConfigMap with it. Runs on its
// own context so it completes from a deferred block whose parent is canceled;
// see cleanupGPUTestNamespace for the status and error semantics.
func cleanupHPAWorkload(clientset kubernetes.Interface, dynClient dynamic.Interface, run *gpuTestRun) (string, error) {
	if run.nsObserved {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := dynClient.Resource(podMonitorGVR).Namespace(run.namespace).Delete(
			ctx, hpaWorkloadName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("failed to delete the PodMonitor before namespace cleanup", "namespace", run.namespace, "error", err)
		}
		if err := clientset.AutoscalingV2().HorizontalPodAutoscalers(run.namespace).Delete(
			ctx, hpaWorkloadName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("failed to delete the HPA before namespace cleanup", "namespace", run.namespace, "error", err)
		}
		if err := clientset.AppsV1().Deployments(run.namespace).Delete(ctx, hpaWorkloadName,
			metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)}); err != nil && !k8serrors.IsNotFound(err) {
			slog.Warn("failed to delete the HPA workload Deployment before namespace cleanup", "namespace", run.namespace, "error", err)
		}
	}
	return cleanupGPUTestNamespace(clientset, run)
}

// waitForMetricsAPI polls probe until it returns nil or the budget elapses,
// returning the last probe error on timeout. prometheus-adapter registers its
// aggregated APIService before it is available, so a single-shot discovery
// right after the deployment phase can race that warm-up; this bounds the
// wait and fails closed. probe runs immediately, then every interval.
func waitForMetricsAPI(ctx context.Context, timeout, interval time.Duration, probe func(context.Context) error) error {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	if err := wait.PollUntilContextCancel(pollCtx, interval, true,
		func(c context.Context) (bool, error) {
			lastErr = probe(c)
			return lastErr == nil, nil
		}); err != nil {
		if lastErr != nil {
			return lastErr
		}
		return err
	}
	return nil
}

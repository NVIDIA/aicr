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
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// promVector renders an instant-vector response with the given samples.
func promVector(values ...float64) []byte {
	type result struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	resp := map[string]any{"status": "success"}
	results := make([]result, 0, len(values))
	for _, v := range values {
		results = append(results, result{Metric: map[string]string{}, Value: []any{1700000000.0, fmt.Sprintf("%v", v)}})
	}
	resp["data"] = map[string]any{"resultType": "vector", "result": results}
	b, _ := json.Marshal(resp)
	return b
}

func TestParsePromSingleSample(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      []byte
		wantValue float64
		wantFound bool
		wantErr   string
	}{
		{name: "one sample", body: promVector(42), wantValue: 42, wantFound: true},
		{name: "empty vector is not found", body: promVector()},
		{name: "two samples is ambiguous", body: promVector(1, 2), wantErr: "at most one sample"},
		{name: "query error", body: []byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`), wantErr: "bad_data: parse error"},
		{name: "matrix result", body: []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), wantErr: "instant vector"},
		{name: "garbage", body: []byte("nope"), wantErr: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, found, err := parsePromSingleSample(tt.body)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if found != tt.wantFound || v != tt.wantValue {
				t.Errorf("got (%v, %t), want (%v, %t)", v, found, tt.wantValue, tt.wantFound)
			}
		})
	}
}

// TestEvaluateCounterIncrease pins the exact-count rule: baseline plus the
// requests sent passes, above fails at once, below keeps waiting, and a
// gauge passes on any value.
func TestEvaluateCounterIncrease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		counter  bool
		baseline float64
		value    float64
		wantOK   bool
		wantErr  string
		wantText string
	}{
		{name: "exact increase passes", counter: true, baseline: 5, value: 15, wantOK: true, wantText: "ok (15 = baseline 5 + 10)"},
		{name: "not yet collected keeps waiting", counter: true, baseline: 5, value: 9, wantText: "got 9, want 15"},
		{name: "above baseline plus N fails", counter: true, baseline: 5, value: 16, wantErr: "extra requests reached the workload"},
		{name: "below baseline keeps waiting and names a reset", counter: true, baseline: 5, value: 2, wantText: "below baseline 5"},
		{name: "gauge passes on any value", counter: false, baseline: 0, value: 3, wantOK: true, wantText: "ok (3)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text, ok, err := evaluateCounterIncrease(tt.counter, tt.baseline, tt.value, 10)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if ok != tt.wantOK || !strings.Contains(text, tt.wantText) {
				t.Errorf("got (%q, %t), want (containing %q, %t)", text, ok, tt.wantText, tt.wantOK)
			}
		})
	}
}

func TestCheckAIServiceMetricsPrerequisites(t *testing.T) {
	t.Parallel()
	t.Run("nil clientset", func(t *testing.T) {
		t.Parallel()
		err := CheckAIServiceMetrics(&validators.Context{Ctx: context.Background()})
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
	t.Run("kube-prometheus-stack not in recipe", func(t *testing.T) {
		t.Parallel()
		err := CheckAIServiceMetrics(&validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(),
			ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator", Namespace: "gpu-operator"}}}})
		errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
		if !strings.Contains(err.Error(), prometheusComponentName) {
			t.Errorf("error %q does not name the missing component", err.Error())
		}
	})
	t.Run("Prometheus Service discovered by label", func(t *testing.T) {
		t.Parallel()
		c := k8sfake.NewClientset(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-prometheus-prometheus", Namespace: "monitoring", Labels: map[string]string{"self-monitor": "true"}},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 9090}}},
		})
		got, err := discoverPrometheusURL(&validators.Context{Ctx: context.Background(), Clientset: c,
			ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: prometheusComponentName, Namespace: "monitoring"}}}})
		if err != nil || got != "http://kube-prometheus-prometheus.monitoring.svc:9090" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
}

// fakeMonitoring stands in for the stub workload and Prometheus: requests the
// check sends are counted as served, and a scraper goroutine copies served to
// collected so queries lag the traffic the way a scrape does.
type fakeMonitoring struct {
	mu        sync.Mutex
	served    float64
	collected float64
	// scrapes toggles collection: false means the ServiceMonitor was never
	// selected and every query stays empty.
	scrapes bool
	// extraTraffic is foreign traffic that lands once, with the first request
	// the check sends. Sized at or above the request count, it keeps every
	// value the scraper can copy strictly above baseline plus N, so the
	// excess-traffic case cannot pass by chance on a mid-run scrape.
	extraTraffic float64
	extraLanded  bool
	// poolNamespace is the run namespace the ServiceMonitor's scrape pool
	// is reported in; poolLoadedAfter delays that pool's appearance from the
	// fake's creation, standing in for prometheus-operator's reconcile gap,
	// and poolNever withholds it. No sample is collected before the pool is
	// loaded, as no scrape can happen before the configuration is.
	poolNamespace   string
	poolLoadedAfter time.Duration
	poolNever       bool
	start           time.Time
	stop            chan struct{}
}

func newFakeMonitoring(scrapes bool) *fakeMonitoring {
	f := &fakeMonitoring{scrapes: scrapes, start: time.Now(), stop: make(chan struct{})}
	go func() {
		for {
			select {
			case <-f.stop:
				return
			case <-time.After(2 * time.Millisecond):
				f.mu.Lock()
				f.collected = f.served
				f.mu.Unlock()
			}
		}
	}()
	return f
}

// poolLoaded reports whether Prometheus has the ServiceMonitor's scrape pool.
func (f *fakeMonitoring) poolLoaded() bool {
	return !f.poolNever && time.Since(f.start) >= f.poolLoadedAfter
}

// scrapePools renders the scrape_pools response: an unrelated pool always,
// the ServiceMonitor's once loaded.
func (f *fakeMonitoring) scrapePools() []byte {
	pools := []string{"serviceMonitor/monitoring/kube-prometheus-stack-kubelet/0"}
	if f.poolLoaded() {
		pools = append(pools, scrapePoolPrefix(monitorKindService, f.poolNamespace, aiMetricsWorkloadName)+"0")
	}
	return promScrapePools(pools...)
}

func (f *fakeMonitoring) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/scrape_pools" {
		_, _ = w.Write(f.scrapePools())
		return
	}
	q := r.URL.Query().Get("query")
	if q == "vector(1)" {
		_, _ = w.Write(promVector(1))
		return
	}
	if !f.scrapes || !f.poolLoaded() {
		_, _ = w.Write(promVector())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "max("+aiMetricQueueDepth):
		_, _ = w.Write(promVector(0))
	default:
		_, _ = w.Write(promVector(f.collected))
	}
}

func (f *fakeMonitoring) sendRequest(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served++
	if f.extraTraffic > 0 && !f.extraLanded {
		f.served += f.extraTraffic
		f.extraLanded = true
	}
	return nil
}

// readyOnCreate makes every created pod Running and Ready, standing in for
// the kubelet.
func readyOnCreate(c *k8sfake.Clientset) {
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		return false, nil, nil
	})
}

// TestAIMetricsScenarioPhases drives the cycle against the fake monitoring
// and pins which phase produces each verdict: a loaded ServiceMonitor with
// exactly the sent traffic passes, also when Prometheus loads it only after
// the collection timeout would have expired (#3671); a ServiceMonitor never
// loaded fails at baseline naming the load; one loaded but never scraped
// fails at baseline naming the collection; foreign traffic above baseline
// plus N fails the increase phase.
func TestAIMetricsScenarioPhases(t *testing.T) {
	t.Parallel()
	const collectionTimeout = 200 * time.Millisecond
	tests := []struct {
		name      string
		scrapes   bool
		poolAfter time.Duration
		poolNever bool
		extra     float64
		wantPhase aiMetricsPhase
		wantCode  errors.ErrorCode
		wantMsg   string
	}{
		{name: "exact count passes", scrapes: true, wantPhase: aiMetricsPhaseIncrease},
		{name: "ServiceMonitor loaded after the collection timeout passes", scrapes: true, poolAfter: 3 * collectionTimeout, wantPhase: aiMetricsPhaseIncrease},
		{name: "ServiceMonitor never loaded fails at baseline", scrapes: true, poolNever: true, wantPhase: aiMetricsPhaseBaseline,
			wantCode: errors.ErrCodeNotFound, wantMsg: "Prometheus did not load the ServiceMonitor"},
		{name: "ServiceMonitor loaded but not scraped fails at baseline", scrapes: false, wantPhase: aiMetricsPhaseBaseline,
			wantCode: errors.ErrCodeNotFound, wantMsg: "Prometheus loaded the ServiceMonitor but the workload metrics were not collected"},
		{name: "traffic above baseline plus N fails the increase", scrapes: true, extra: float64(defaults.AIServiceMetricsRequestCount), wantPhase: aiMetricsPhaseIncrease,
			wantCode: errors.ErrCodeInternal, wantMsg: "extra requests reached the workload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mon := newFakeMonitoring(tt.scrapes)
			defer close(mon.stop)
			mon.extraTraffic = tt.extra
			mon.poolLoadedAfter, mon.poolNever = tt.poolAfter, tt.poolNever
			srv := httptest.NewServer(http.HandlerFunc(mon.handler))
			defer srv.Close()

			c := k8sfake.NewClientset()
			readyOnCreate(c)
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{serviceMonitorGVR: "ServiceMonitorList"})
			s, err := newAIMetricsScenario(c, dyn, srv.URL, nil)
			if err != nil {
				t.Fatalf("newAIMetricsScenario: %v", err)
			}
			mon.poolNamespace = s.testRun.namespace
			s.sendRequest = mon.sendRequest
			s.pollInterval = 2 * time.Millisecond
			s.readyTimeout = time.Second
			s.monitorLoadTimeout = 2 * time.Second
			s.collectionTimeout = collectionTimeout

			ctx := &validators.Context{Ctx: context.Background(), Clientset: c}
			if preErr := s.preflight(ctx.Ctx); preErr != nil {
				t.Fatalf("preflight: %v", preErr)
			}
			err = s.runWithCleanup(ctx)
			if s.phase != tt.wantPhase {
				t.Errorf("verdict phase = %s, want %s (err=%v)", s.phase, tt.wantPhase, err)
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				mon.mu.Lock()
				served := mon.served
				mon.mu.Unlock()
				if served != float64(s.requestCount) {
					t.Errorf("served %v requests, want exactly %d", served, s.requestCount)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) || !strings.Contains(err.Error(), "phase "+string(tt.wantPhase)) {
					t.Errorf("error %q lacks %q or the phase name", err.Error(), tt.wantMsg)
				}
			}
			if _, getErr := c.CoreV1().Namespaces().Get(context.Background(), s.testRun.namespace, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
				t.Errorf("namespace %s not deleted after the run: %v", s.testRun.namespace, getErr)
			}
			if _, getErr := dyn.Resource(serviceMonitorGVR).Namespace(s.testRun.namespace).Get(context.Background(), aiMetricsWorkloadName, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
				t.Errorf("ServiceMonitor not deleted after the run: %v", getErr)
			}
		})
	}
}

// TestWaitForPodReadyReadErrors pins that a transient read failure does not
// end the readiness wait (the next poll finds the Ready pod), while a
// definitive API verdict does.
func TestWaitForPodReadyReadErrors(t *testing.T) {
	t.Parallel()
	const ns = "aicr-metrics-ready-unit"
	tests := []struct {
		name     string
		getErr   error
		once     bool
		wantCode errors.ErrorCode
	}{
		{name: "503 once, then Ready", getErr: k8serrors.NewServiceUnavailable("apiserver draining"), once: true},
		{name: "Forbidden ends the wait", getErr: k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, aiMetricsWorkloadName, nil),
			wantCode: errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ready := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: aiMetricsWorkloadName, Namespace: ns},
				Status: corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			c := k8sfake.NewClientset(ready)
			var mu sync.Mutex
			failed := false
			c.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				mu.Lock()
				defer mu.Unlock()
				if tt.once && failed {
					return false, nil, nil
				}
				failed = true
				return true, nil, tt.getErr
			})
			pod, err := waitForPodReady(context.Background(), c, ns, aiMetricsWorkloadName, 5*time.Second)
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("waitForPodReady: %v", err)
			}
			if pod == nil || !podIsReady(pod) {
				t.Errorf("pod = %+v, want the Ready pod", pod)
			}
		})
	}
}

// TestCheckAIMetricsWorkloadIntact pins what ends the collection wait: a stub
// pod that is gone, not Running or restarted is a verdict, as is a definitive
// API verdict on the read; a read that fails transiently is left for the next
// poll.
func TestCheckAIMetricsWorkloadIntact(t *testing.T) {
	t.Parallel()
	const ns = "aicr-metrics-unit"
	stub := func(mutate func(*corev1.Pod)) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: aiMetricsWorkloadName, Namespace: ns},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: aiMetricsWorkloadName}}},
		}
		if mutate != nil {
			mutate(pod)
		}
		return pod
	}
	tests := []struct {
		name     string
		pod      *corev1.Pod
		getErr   error
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{name: "Running pod with no restarts passes", pod: stub(nil)},
		{name: "pod gone fails", wantCode: errors.ErrCodeNotFound, wantMsg: "metrics stub pod not found"},
		{
			name:     "pod not Running fails",
			pod:      stub(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
			wantCode: errors.ErrCodeInternal, wantMsg: "metrics stub pod is Pending",
		},
		{
			name:     "container restart fails",
			pod:      stub(func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 }),
			wantCode: errors.ErrCodeInternal, wantMsg: "restarted 1 time(s)",
		},
		{name: "503 read is left for the next poll", pod: stub(nil), getErr: k8serrors.NewServiceUnavailable("apiserver draining")},
		{
			name: "internal error read is left for the next poll", pod: stub(nil),
			getErr: k8serrors.NewInternalError(errors.New(errors.ErrCodeInternal, "etcd leader changed")),
		},
		{name: "throttled read is left for the next poll", pod: stub(nil), getErr: k8serrors.NewTooManyRequests("slow down", 1)},
		{
			name: "Forbidden read fails", pod: stub(nil),
			getErr:   k8serrors.NewForbidden(schema.GroupResource{Resource: "pods"}, aiMetricsWorkloadName, nil),
			wantCode: errors.ErrCodeInternal, wantMsg: "failed to read metrics stub pod",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := k8sfake.NewClientset()
			if tt.pod != nil {
				c = k8sfake.NewClientset(tt.pod)
			}
			if tt.getErr != nil {
				c.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}
			err := checkAIMetricsWorkloadIntact(context.Background(), c, ns)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want nil (wait continues), got %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q lacks %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

// TestAIMetricsScenarioPreflight pins the preflight's verdicts: a query API
// that does not evaluate vector(1), a scrape pool API that answers an error
// (a Prometheus older than v2.42 has none), and an unreachable Prometheus
// each fail before any resource is created, naming what was probed.
func TestAIMetricsScenarioPreflight(t *testing.T) {
	t.Parallel()
	var (
		mu         sync.Mutex
		vectorOK   bool
		poolsOK    bool
		poolsReply = promScrapePools("kubelet/0")
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/scrape_pools" && poolsOK:
			_, _ = w.Write(poolsReply)
		case r.URL.Path == "/api/v1/scrape_pools":
			http.NotFound(w, r)
		case vectorOK:
			_, _ = w.Write(promVector(1))
		default:
			_, _ = w.Write(promVector())
		}
	}))
	defer srv.Close()
	s, err := newAIMetricsScenario(k8sfake.NewClientset(), nil, srv.URL, nil)
	if err != nil {
		t.Fatalf("newAIMetricsScenario: %v", err)
	}
	err = s.preflight(context.Background())
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	if !strings.Contains(err.Error(), "vector(1)") {
		t.Errorf("error %q does not describe the preflight", err.Error())
	}

	mu.Lock()
	vectorOK = true
	mu.Unlock()
	err = s.preflight(context.Background())
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	if err == nil || !strings.Contains(err.Error(), "scrape pools") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error %v does not describe the scrape pool preflight", err)
	}

	mu.Lock()
	poolsOK = true
	mu.Unlock()
	if err = s.preflight(context.Background()); err != nil {
		t.Errorf("preflight with both APIs answering: %v", err)
	}

	srv.Close()
	err = s.preflight(context.Background())
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
}

// TestBuildAIMetricsWorkload pins the stub's shape: CPU only, restricted
// security context, the run id in its environment, and a ServiceMonitor that
// selects the Service by app label and carries the release label.
func TestBuildAIMetricsWorkload(t *testing.T) {
	t.Parallel()
	pod, svc := buildAIMetricsWorkload("ns", "run123", time.Minute, nil)
	c := pod.Spec.Containers[0]
	if _, has := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; has || len(pod.Spec.ResourceClaims) != 0 {
		t.Error("the stub must not request an accelerator")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("the stub must run as non-root")
	}
	var runID string
	for _, e := range c.Env {
		if e.Name == "RUN_ID" {
			runID = e.Value
		}
	}
	if runID != "run123" {
		t.Errorf("RUN_ID = %q", runID)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds <= 0 {
		t.Error("the stub needs an activeDeadlineSeconds so a leaked pod ends on its own")
	}
	if svc.Spec.Selector[hpaAppLabelKey] != aiMetricsWorkloadName || svc.Spec.Ports[0].Name != aiMetricsPortName {
		t.Errorf("service = %+v", svc.Spec)
	}
	sm := buildAIMetricsServiceMonitor("ns")
	if sm.GetLabels()[prometheusReleaseLabel] != prometheusComponentName {
		t.Errorf("ServiceMonitor labels = %v, want %s=%s", sm.GetLabels(), prometheusReleaseLabel, prometheusComponentName)
	}
	endpoints, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if len(endpoints) != 1 || endpoints[0].(map[string]any)["port"] != aiMetricsPortName {
		t.Errorf("ServiceMonitor endpoints = %v", endpoints)
	}
}

// TestBuildAIMetricsWorkloadTolerations pins the stub's scheduling on tainted
// nodes: nil defaults to tolerate-all and a caller's tolerations are applied
// verbatim, the same contract as buildHPADeployment.
func TestBuildAIMetricsWorkloadTolerations(t *testing.T) {
	t.Parallel()
	caller := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	tests := []struct {
		name        string
		tolerations []corev1.Toleration
		want        []corev1.Toleration
	}{
		{"nil defaults to tolerate-all", nil, []corev1.Toleration{{Operator: corev1.TolerationOpExists}}},
		{"caller's tolerations are applied", caller, caller},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod, _ := buildAIMetricsWorkload("ns", "run123", time.Minute, tt.tolerations)
			if !reflect.DeepEqual(pod.Spec.Tolerations, tt.want) {
				t.Errorf("tolerations = %+v, want %+v", pod.Spec.Tolerations, tt.want)
			}
		})
	}
}

// TestAIMetricsScenarioRunWithCleanup_NamespaceCreateFailure pins that the AI
// service metrics check's teardown runs even when createGPUTestNamespace
// fails (#3648), and never starts the cycle on that path. The cases and the
// assertions are shared with the HPA check (namespaceCreateFailureCases).
// Sequential: captureStdout swaps os.Stdout.
func TestAIMetricsScenarioRunWithCleanup_NamespaceCreateFailure(t *testing.T) {
	shortenCleanupTimeout(t)
	for _, tt := range namespaceCreateFailureCases {
		t.Run(tt.name, func(t *testing.T) {
			c := k8sfake.NewClientset()
			failNamespaceCreate(c, tt.persists, tt.surfacesLate, tt.createErr)
			if tt.deleteErr != nil {
				failNamespaceDelete(c, tt.deleteErr)
			}
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{serviceMonitorGVR: "ServiceMonitorList"})
			s, err := newAIMetricsScenario(c, dyn, "http://prometheus.invalid", nil)
			if err != nil {
				t.Fatalf("newAIMetricsScenario: %v", err)
			}

			out := captureStdout(t, func() {
				err = s.runWithCleanup(&validators.Context{Ctx: context.Background(), Clientset: c})
			})
			assertNamespaceCreateFailureCleanup(t, c, s.testRun.namespace, out, err, tt.persists || tt.surfacesLate, tt.deleteErr, tt.wantDeleted, tt.wantStatus)
			if s.phase != "" {
				t.Errorf("phase = %q, want none — the cycle must not start without a namespace", s.phase)
			}
		})
	}
}

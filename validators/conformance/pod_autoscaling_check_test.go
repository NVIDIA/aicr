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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

// TestWaitForMetricsAPI covers the bounded-retry helper that wraps the
// custom-metrics discovery, which can race the prometheus-adapter warm-up
// right after the deployment phase. A tiny interval keeps the test fast.
func TestWaitForMetricsAPI(t *testing.T) {
	t.Run("immediate success runs the probe once", func(t *testing.T) {
		calls := 0
		err := waitForMetricsAPI(context.Background(), time.Second, time.Millisecond,
			func(context.Context) error { calls++; return nil })
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if calls != 1 {
			t.Errorf("want 1 probe call, got %d", calls)
		}
	})

	t.Run("retries until the probe succeeds", func(t *testing.T) {
		calls := 0
		err := waitForMetricsAPI(context.Background(), 5*time.Second, time.Millisecond,
			func(context.Context) error {
				calls++
				if calls < 3 {
					return stderrors.New("not ready")
				}
				return nil
			})
		if err != nil {
			t.Fatalf("want success after retries, got %v", err)
		}
		if calls < 3 {
			t.Errorf("want >=3 probe calls, got %d", calls)
		}
	})

	t.Run("returns the last probe error on timeout (fail closed)", func(t *testing.T) {
		sentinel := stderrors.New("still not ready")
		err := waitForMetricsAPI(context.Background(), 30*time.Millisecond, time.Millisecond,
			func(context.Context) error { return sentinel })
		if !stderrors.Is(err, sentinel) {
			t.Fatalf("want last probe error %v, got %v", sentinel, err)
		}
	})

	t.Run("returns when the parent context is already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := waitForMetricsAPI(ctx, time.Second, time.Millisecond,
			func(context.Context) error { return stderrors.New("not ready") })
		if err == nil {
			t.Fatal("want non-nil error when parent context is canceled")
		}
	})
}

// withCustomMetricsAPI makes the fake discovery serve custom.metrics.k8s.io at
// the given versions (first listed is preferred, as the fake derives it).
func withCustomMetricsAPI(c *k8sfake.Clientset, versions ...string) {
	fd := c.Discovery().(*fakediscovery.FakeDiscovery)
	for _, v := range versions {
		fd.Resources = append(fd.Resources, &metav1.APIResourceList{
			GroupVersion: hpaCustomMetricsGroup + "/" + v,
			APIResources: []metav1.APIResource{{Name: "pods", Namespaced: true}},
		})
	}
}

func TestResolveCustomMetricsVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		versions []string
		want     string
		wantErr  bool
	}{
		{name: "group not served reports absence without error", want: ""},
		{name: "v1beta1 only (prometheus-adapter chart)", versions: []string{"v1beta1"}, want: "v1beta1"},
		{name: "preferred v1beta2 wins", versions: []string{"v1beta2", "v1beta1"}, want: "v1beta2"},
		{name: "unsupported preferred falls back to a supported version", versions: []string{"v2", "v1beta1"}, want: "v1beta1"},
		{name: "only unsupported versions is an error", versions: []string{"v2"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := k8sfake.NewClientset()
			withCustomMetricsAPI(c, tt.versions...)
			got, err := resolveCustomMetricsVersion(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("version = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCheckPodAutoscalingApplicabilityGate pins the #2122 contract for the
// custom metrics API: not served and no prometheus-adapter in the recipe
// skips; not served with the adapter declared fails (the recipe owns the
// adapter, so upstream's skip is a failure here).
func TestCheckPodAutoscalingApplicabilityGate(t *testing.T) {
	oldWarmup, oldInterval := metricsAPIWarmup, metricsAPIProbeInterval
	metricsAPIWarmup, metricsAPIProbeInterval = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { metricsAPIWarmup, metricsAPIProbeInterval = oldWarmup, oldInterval })
	declares := func(name string) *v1.ValidationInput {
		return &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: name}}}
	}
	// resourcesUnavailable makes ServerResourcesForGroupVersion fail until
	// the given number of calls, the discovery shape of an APIService that
	// is registered but not yet available.
	resourcesUnavailable := func(c *k8sfake.Clientset, failUntil int) *int {
		calls := 0
		c.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls <= failUntil {
				return true, nil, stderrors.New("the server is currently unable to handle the request")
			}
			return false, nil, nil
		})
		return &calls
	}

	t.Run("nil clientset", func(t *testing.T) {
		err := CheckPodAutoscaling(&validators.Context{Ctx: context.Background()})
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
	t.Run("not served, adapter not declared: skip", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(),
			ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}}
		err := CheckPodAutoscaling(ctx)
		if !validators.IsSkip(err) {
			t.Fatalf("want skip, got %v", err)
		}
		if !strings.Contains(err.Error(), "prometheus-adapter not declared") {
			t.Errorf("skip reason %q does not name the adapter", err.Error())
		}
	})
	t.Run("not served, adapter declared: fail closed", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(),
			ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "prometheus-adapter"}}}}
		err := CheckPodAutoscaling(ctx)
		if validators.IsSkip(err) {
			t.Fatalf("declared adapter must not skip, got %v", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
		if !strings.Contains(err.Error(), "recipe declares prometheus-adapter") {
			t.Errorf("error %q does not explain the declared-but-absent adapter", err.Error())
		}
	})
	t.Run("not served, OCP adapter declared: fail closed", func(t *testing.T) {
		ctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(),
			ValidationInput: declares("prometheus-adapter-ocp")}
		err := CheckPodAutoscaling(ctx)
		if validators.IsSkip(err) {
			t.Fatalf("declared OCP adapter must not skip, got %v", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
		if err == nil || !strings.Contains(err.Error(), "recipe declares prometheus-adapter-ocp") {
			t.Errorf("error %v does not name the declared OCP adapter", err)
		}
	})
	t.Run("registered with only an unsupported version, adapter declared: waits, then fails closed", func(t *testing.T) {
		c := k8sfake.NewClientset()
		withCustomMetricsAPI(c, "v2")
		ctx := &validators.Context{Ctx: context.Background(), Clientset: c, ValidationInput: declares("prometheus-adapter")}
		start := time.Now()
		err := CheckPodAutoscaling(ctx)
		if elapsed := time.Since(start); elapsed < metricsAPIWarmup {
			t.Errorf("returned after %s, want the %s warm-up window spent first", elapsed, metricsAPIWarmup)
		}
		if validators.IsSkip(err) {
			t.Fatalf("declared adapter must not skip, got %v", err)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
			t.Errorf("error %v does not carry the unavailable probe verdict", err)
		}
	})
	t.Run("registered but resources unavailable throughout, adapter declared: waits, then fails closed", func(t *testing.T) {
		c := k8sfake.NewClientset()
		withCustomMetricsAPI(c, "v1beta1")
		calls := resourcesUnavailable(c, 1<<30)
		ctx := &validators.Context{Ctx: context.Background(), Clientset: c, ValidationInput: declares("prometheus-adapter")}
		start := time.Now()
		err := CheckPodAutoscaling(ctx)
		if elapsed := time.Since(start); elapsed < metricsAPIWarmup {
			t.Errorf("returned after %s, want the %s warm-up window spent first", elapsed, metricsAPIWarmup)
		}
		if *calls < 2 {
			t.Errorf("discovery probed %d time(s), want the warm-up loop to retry", *calls)
		}
		if validators.IsSkip(err) {
			t.Fatalf("declared adapter must not skip, got %v", err)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
			t.Errorf("error %v does not carry the unavailable probe verdict", err)
		}
	})
	t.Run("registered, becomes available mid-window, adapter declared: proceeds", func(t *testing.T) {
		c := k8sfake.NewClientset()
		withCustomMetricsAPI(c, "v1beta1")
		resourcesUnavailable(c, 3)
		ctx := &validators.Context{Ctx: context.Background(), Clientset: c, ValidationInput: declares("prometheus-adapter")}
		err := CheckPodAutoscaling(ctx)
		// With no RESTConfig the check stops right after the gate, which is
		// the proof that the gate let it through.
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if err == nil || !strings.Contains(err.Error(), "RESTConfig is not available") {
			t.Errorf("error %v, want the gate passed and the dynamic client lookup to be what stops the check", err)
		}
	})
}

func hpaTestState(hpa *autoscalingv2.HorizontalPodAutoscaler, replicas int32, pods ...corev1.Pod) hpaState {
	return hpaState{
		hpa: hpa,
		deployment: &appsv1.Deployment{
			Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
			Status: appsv1.DeploymentStatus{Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas},
		},
		pods: pods,
	}
}

func readyHPAPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: hpaWorkloadLabels()},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func hpaWithStatus(active corev1.ConditionStatus, reason string, average int, desired, current int32) *autoscalingv2.HorizontalPodAutoscaler {
	avg := resource.MustParse(strconv.Itoa(average))
	return &autoscalingv2.HorizontalPodAutoscaler{
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			DesiredReplicas: desired,
			CurrentReplicas: current,
			Conditions:      []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive, Status: active, Reason: reason}},
			CurrentMetrics: []autoscalingv2.MetricStatus{{
				Type: autoscalingv2.PodsMetricSourceType,
				Pods: &autoscalingv2.PodsMetricStatus{Metric: autoscalingv2.MetricIdentifier{Name: hpaMetricSeries},
					Current: autoscalingv2.MetricValueStatus{AverageValue: &avg}},
			}},
		},
	}
}

// TestCheckHPAScaledTo pins the per-observation assertions: ScalingActive,
// the exact phase average, desired and current replicas, and every pod
// Running and Ready.
func TestCheckHPAScaledTo(t *testing.T) {
	t.Parallel()
	notReady := readyHPAPod("hpa-workload-b")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	tests := []struct {
		name    string
		state   hpaState
		want    int32
		value   int
		wantErr string
	}{
		{name: "one replica at baseline", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 1, 1, 1), 1, readyHPAPod("hpa-workload-a")), want: 1, value: 1},
		{name: "two replicas scaled up", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 100, 2, 2), 2, readyHPAPod("hpa-workload-a"), readyHPAPod("hpa-workload-b")), want: 2, value: 100},
		{name: "HPA missing", state: hpaTestState(nil, 1, readyHPAPod("hpa-workload-a")), want: 1, value: 1, wantErr: "HPA does not exist"},
		{name: "ScalingActive false", state: hpaTestState(hpaWithStatus(corev1.ConditionFalse, "FailedGetPodsMetric", 1, 1, 1), 1, readyHPAPod("hpa-workload-a")), want: 1, value: 1, wantErr: "ScalingActive is False"},
		{name: "stale average from the previous phase", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 1, 2, 2), 2, readyHPAPod("hpa-workload-a"), readyHPAPod("hpa-workload-b")), want: 2, value: 100, wantErr: "recorded average 1"},
		{name: "desired replicas disagree", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 100, 1, 1), 1, readyHPAPod("hpa-workload-a")), want: 2, value: 100, wantErr: "desiredReplicas=1, want 2"},
		{name: "current replicas lag", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 100, 2, 1), 2, readyHPAPod("hpa-workload-a"), readyHPAPod("hpa-workload-b")), want: 2, value: 100, wantErr: "currentReplicas=1, want 2"},
		{name: "a pod not Ready", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 100, 2, 2), 2, readyHPAPod("hpa-workload-a"), notReady), want: 2, value: 100, wantErr: "want Running and Ready"},
		{name: "fewer pods than replicas", state: hpaTestState(hpaWithStatus(corev1.ConditionTrue, "", 100, 2, 2), 2, readyHPAPod("hpa-workload-a")), want: 2, value: 100, wantErr: "found 1 active workload pods"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkHPAScaledTo(tt.state, tt.want, tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func metricSamples(namespace string, values map[string]int, extraKind string) []byte {
	type item struct {
		DescribedObject map[string]string `json:"describedObject"`
		MetricName      string            `json:"metricName"`
		Value           string            `json:"value"`
	}
	var items []item
	for pod, v := range values {
		items = append(items, item{
			DescribedObject: map[string]string{"kind": "Pod", "namespace": namespace, "name": pod},
			MetricName:      hpaMetricSeries, Value: strconv.Itoa(v),
		})
	}
	if extraKind != "" {
		items = append(items, item{DescribedObject: map[string]string{"kind": extraKind, "namespace": namespace, "name": "x"},
			MetricName: hpaMetricSeries, Value: "1"})
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

// TestCheckHPAMetricSamples pins "the custom metrics API reports the new
// value for every expected pod": a stale value, a missing pod or a foreign
// object fails; samples for pods no longer expected are ignored.
func TestCheckHPAMetricSamples(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     []byte
		expected []string
		want     int
		wantErr  string
	}{
		{name: "every pod reports the value", body: metricSamples("ns", map[string]int{"a": 100, "b": 100}, ""), expected: []string{"a", "b"}, want: 100},
		{name: "a terminating pod's sample is ignored", body: metricSamples("ns", map[string]int{"a": 2, "old": 100}, ""), expected: []string{"a"}, want: 2},
		{name: "stale value", body: metricSamples("ns", map[string]int{"a": 1, "b": 100}, ""), expected: []string{"a", "b"}, want: 100, wantErr: "pod a reports 1, want 100"},
		{name: "missing pod", body: metricSamples("ns", map[string]int{"a": 100}, ""), expected: []string{"a", "b"}, want: 100, wantErr: "no sample for pod b"},
		{name: "foreign object", body: metricSamples("ns", map[string]int{"a": 1}, "Service"), expected: []string{"a"}, want: 1, wantErr: "unexpected object Service"},
		{name: "wrong namespace", body: metricSamples("other", map[string]int{"a": 1}, ""), expected: []string{"a"}, want: 1, wantErr: "unexpected object Pod other/a"},
		{name: "garbage", body: []byte("{"), expected: []string{"a"}, want: 1, wantErr: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkHPAMetricSamples(tt.body, "ns", tt.expected, tt.want)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestBuildHPADeployment pins the accelerator request shape per allocation
// mode: exactly one device per pod, through a ResourceClaimTemplate under
// DRA or nvidia.com/gpu: 1 pinned to the device-plugin nodes otherwise.
func TestBuildHPADeployment(t *testing.T) {
	t.Parallel()
	t.Run("DRA", func(t *testing.T) {
		t.Parallel()
		d := buildHPADeployment("ns", true, nil, nil, nil)
		spec := d.Spec.Template.Spec
		if len(spec.ResourceClaims) != 1 || ptr.Deref(spec.ResourceClaims[0].ResourceClaimTemplateName, "") != hpaWorkloadName {
			t.Fatalf("resourceClaims = %+v, want one claim from template %s", spec.ResourceClaims, hpaWorkloadName)
		}
		c := spec.Containers[0]
		if len(c.Resources.Claims) != 1 || c.Resources.Claims[0].Name != gpuClaimName {
			t.Errorf("container claims = %+v, want %s", c.Resources.Claims, gpuClaimName)
		}
		if _, has := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; has {
			t.Error("DRA pod must not also request nvidia.com/gpu")
		}
		if spec.Affinity != nil {
			t.Error("DRA pod must not pin nodes; the scheduler places it by claim")
		}
		if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != corev1.TolerationOpExists {
			t.Errorf("tolerations = %+v, want tolerate-all default", spec.Tolerations)
		}
	})
	t.Run("device plugin", func(t *testing.T) {
		t.Parallel()
		tol := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
		d := buildHPADeployment("ns", false, []string{"node-a", "node-b"}, tol, nil)
		spec := d.Spec.Template.Spec
		if len(spec.ResourceClaims) != 0 {
			t.Errorf("device-plugin pod must not carry resourceClaims: %+v", spec.ResourceClaims)
		}
		q := spec.Containers[0].Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]
		if q.Value() != 1 {
			t.Errorf("nvidia.com/gpu limit = %s, want 1", q.String())
		}
		terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 2 {
			t.Errorf("node affinity terms = %d, want one per device-plugin node", len(terms))
		}
		if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "dedicated" {
			t.Errorf("tolerations = %+v, want the caller's", spec.Tolerations)
		}
	})
	t.Run("common shape", func(t *testing.T) {
		t.Parallel()
		d := buildHPADeployment("ns", false, nil, nil, nil)
		if ptr.Deref(d.Spec.Replicas, 0) != hpaMinReplicas {
			t.Errorf("replicas = %d, want %d", ptr.Deref(d.Spec.Replicas, 0), hpaMinReplicas)
		}
		if d.Spec.Template.Labels[hpaMetricSourceLabel] != "true" {
			t.Errorf("pod template lacks the %s label the PodMonitor selects", hpaMetricSourceLabel)
		}
		c := d.Spec.Template.Spec.Containers[0]
		if c.Image != "docker.io/library/python:3.13-alpine" {
			t.Errorf("image = %s", c.Image)
		}
		if len(c.Ports) != 1 || c.Ports[0].Name != hpaMetricsPortName || c.Ports[0].ContainerPort != hpaMetricsPort {
			t.Errorf("ports = %+v, want %s/%d", c.Ports, hpaMetricsPortName, hpaMetricsPort)
		}
		if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
			t.Errorf("readiness probe = %+v, want HTTP /healthz", c.ReadinessProbe)
		}
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].SubPath != "" {
			t.Errorf("volume mounts = %+v, want the whole ConfigMap directory (subPath mounts never update)", c.VolumeMounts)
		}
		if !strings.Contains(c.Args[0], "/dev/nvidia[0-9]*") || !strings.Contains(c.Args[0], `-ne 1`) {
			t.Errorf("command does not require exactly one accelerator device: %s", c.Args[0])
		}
	})
}

func TestBuildHPA(t *testing.T) {
	t.Parallel()
	h := buildHPA("ns", time.Minute)
	if ptr.Deref(h.Spec.MinReplicas, 0) != 1 || h.Spec.MaxReplicas != 2 {
		t.Errorf("replicas min=%d max=%d, want 1 and 2", ptr.Deref(h.Spec.MinReplicas, 0), h.Spec.MaxReplicas)
	}
	m := h.Spec.Metrics[0]
	if m.Type != autoscalingv2.PodsMetricSourceType || m.Pods.Metric.Name != hpaMetricSeries || m.Pods.Target.AverageValue.Value() != hpaTargetValue {
		t.Errorf("metric = %+v, want Pods %s averageValue %d", m, hpaMetricSeries, hpaTargetValue)
	}
	if got := ptr.Deref(h.Spec.Behavior.ScaleDown.StabilizationWindowSeconds, -1); got != 60 {
		t.Errorf("scale-down stabilization = %d, want 60", got)
	}
	if got := ptr.Deref(h.Spec.Behavior.ScaleUp.StabilizationWindowSeconds, -1); got != 0 {
		t.Errorf("scale-up stabilization = %d, want 0", got)
	}
}

func TestBuildHPAClaimTemplate(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v1", "v1beta2", "v1beta1"} {
		tpl := buildHPAClaimTemplate("ns", version)
		if tpl.GetAPIVersion() != apiGroupResourceK8sIO+"/"+version || tpl.GetKind() != "ResourceClaimTemplate" {
			t.Errorf("%s: apiVersion/kind = %s/%s", version, tpl.GetAPIVersion(), tpl.GetKind())
		}
		requests, _, _ := unstructured.NestedSlice(tpl.Object, "spec", "spec", "devices", "requests")
		if len(requests) != 1 {
			t.Fatalf("%s: requests = %v, want one", version, requests)
		}
		req := requests[0].(map[string]any)
		if version == versionV1beta1 {
			if req["deviceClassName"] != draDriverGPU || req["count"] != int64(1) {
				t.Errorf("v1beta1 flat request = %v", req)
			}
		} else if exactly, ok := req["exactly"].(map[string]any); !ok || exactly["deviceClassName"] != draDriverGPU || exactly["count"] != int64(1) {
			t.Errorf("%s exactly request = %v", version, req)
		}
	}
}

func TestPhaseErrorKeepsCode(t *testing.T) {
	t.Parallel()
	err := phaseError(hpaPhaseScaleUp, errors.New(errors.ErrCodeTimeout, "slow"))
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if !strings.Contains(err.Error(), "phase scale-up") {
		t.Errorf("error %q does not name the phase", err.Error())
	}
	err = phaseError(hpaPhaseBaseline, stderrors.New("plain"))
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

// fakeHPAControlPlane stands in for the Deployment and HPA controllers against
// the fake clientset: it keeps the pods in step with spec.replicas, and once
// the HPA exists it decides replicas from the ConfigMap value exactly as a
// correct HPA would (max above target, min below), unless a behavior knob
// says otherwise.
type fakeHPAControlPlane struct {
	c  *k8sfake.Clientset
	ns string

	// neverScaleUp keeps deciding on one replica whatever the metric.
	neverScaleUp bool
	// neverScaleDown keeps the maximum once reached.
	neverScaleDown bool
	// flipAtBaselineAfter makes the HPA decide on two replicas while the
	// metric is still at baseline, once the HPA has existed this long.
	flipAtBaselineAfter time.Duration
	// adapterBroken makes the custom metrics API answer NotFound.
	adapterBroken bool
	// replacePodDuringNegativeControl evicts the baseline pod once the HPA
	// has held the baseline for a while and lets a differently named pod
	// replace it, as a Deployment does after an eviction.
	replacePodDuringNegativeControl bool
	// holdNewPodPendingFor keeps every pod the HPA adds Pending, with no
	// Ready condition and no sample, for this long before it becomes Ready.
	holdNewPodPendingFor time.Duration
	// poolLoadedAfter delays the PodMonitor's scrape pool from the fake's
	// creation, standing in for prometheus-operator's reconcile gap, and
	// poolNever withholds it; the adapter serves no sample before the pool
	// is loaded, as no scrape can happen before the configuration is.
	// noPrometheus leaves the scenario without a scrape pool lister, the
	// shape a recipe without kube-prometheus-stack produces.
	poolLoadedAfter time.Duration
	poolNever       bool
	noPrometheus    bool
	created         time.Time

	mu           sync.Mutex
	hpaSeen      time.Time
	scaledUp     bool
	replaced     bool
	heldPending  bool
	pendingSince map[string]time.Time
	stop         chan struct{}
	stopped      chan struct{}
	stopOnce     sync.Once
	// lastMetric is the value the fake adapter serves: the ConfigMap value as
	// of the last reconcile, so a sample trails the reconcile the way a scrape
	// trails the kubelet, and a poll that sees a new value also sees the pods
	// the HPA added for it. scraped is false until the first reconcile.
	lastMetric int
	scraped    bool
}

func newFakeHPAControlPlane(c *k8sfake.Clientset, ns string) *fakeHPAControlPlane {
	return &fakeHPAControlPlane{c: c, ns: ns, created: time.Now(), stop: make(chan struct{}), stopped: make(chan struct{}),
		pendingSince: map[string]time.Time{}}
}

// poolLoaded reports whether Prometheus has the PodMonitor's scrape pool.
func (f *fakeHPAControlPlane) poolLoaded() bool {
	return !f.poolNever && time.Since(f.created) >= f.poolLoadedAfter
}

// listScrapePools answers as Prometheus's scrape_pools API would: an
// unrelated pool always, the PodMonitor's once loaded.
func (f *fakeHPAControlPlane) listScrapePools(context.Context) ([]byte, error) {
	pools := []string{"serviceMonitor/monitoring/kube-prometheus-stack-kubelet/0"}
	if f.poolLoaded() {
		pools = append(pools, scrapePoolPrefix(monitorKindPod, f.ns, hpaWorkloadName)+"0")
	}
	return promScrapePools(pools...), nil
}

func (f *fakeHPAControlPlane) start() {
	go func() {
		defer close(f.stopped)
		for {
			select {
			case <-f.stop:
				return
			case <-time.After(2 * time.Millisecond):
				f.reconcile()
			}
		}
	}()
}

func (f *fakeHPAControlPlane) shutdown() {
	f.stopOnce.Do(func() { close(f.stop) })
	<-f.stopped
}

func (f *fakeHPAControlPlane) metricValue(ctx context.Context) (int, bool) {
	cm, err := f.c.CoreV1().ConfigMaps(f.ns).Get(ctx, hpaWorkloadName, metav1.GetOptions{})
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(cm.Data[hpaMetricValueKey])
	return v, err == nil
}

func (f *fakeHPAControlPlane) reconcile() {
	ctx := context.Background()
	value, ok := f.metricValue(ctx)
	if !ok {
		return
	}
	deploy, err := f.c.AppsV1().Deployments(f.ns).Get(ctx, hpaWorkloadName, metav1.GetOptions{})
	if err != nil {
		return
	}
	if hpa, err := f.c.AutoscalingV2().HorizontalPodAutoscalers(f.ns).Get(ctx, hpaWorkloadName, metav1.GetOptions{}); err == nil {
		f.reconcileHPA(ctx, hpa, deploy, value)
		f.maybeReplacePod(ctx, value)
		deploy, err = f.c.AppsV1().Deployments(f.ns).Get(ctx, hpaWorkloadName, metav1.GetOptions{})
		if err != nil {
			return
		}
	}
	f.reconcilePods(ctx, deploy)
	f.mu.Lock()
	f.lastMetric, f.scraped = value, true
	f.mu.Unlock()
}

func (f *fakeHPAControlPlane) didHoldPodPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heldPending
}

func (f *fakeHPAControlPlane) decide(value int) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hpaSeen.IsZero() {
		f.hpaSeen = time.Now()
	}
	switch {
	case f.neverScaleUp:
		return hpaMinReplicas
	case f.flipAtBaselineAfter > 0 && time.Since(f.hpaSeen) > f.flipAtBaselineAfter:
		return hpaMaxReplicas
	case value > hpaTargetValue:
		f.scaledUp = true
		return hpaMaxReplicas
	case f.neverScaleDown && f.scaledUp:
		return hpaMaxReplicas
	default:
		return hpaMinReplicas
	}
}

func (f *fakeHPAControlPlane) reconcileHPA(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, deploy *appsv1.Deployment, value int) {
	desired := f.decide(value)
	avg := resource.MustParse(strconv.Itoa(value))
	hpa.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
		DesiredReplicas: desired,
		CurrentReplicas: ptr.Deref(deploy.Spec.Replicas, 0),
		Conditions:      []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue, Reason: "ValidMetricFound"}},
		CurrentMetrics: []autoscalingv2.MetricStatus{{
			Type: autoscalingv2.PodsMetricSourceType,
			Pods: &autoscalingv2.PodsMetricStatus{Metric: autoscalingv2.MetricIdentifier{Name: hpaMetricSeries},
				Current: autoscalingv2.MetricValueStatus{AverageValue: &avg}},
		}},
	}
	_, _ = f.c.AutoscalingV2().HorizontalPodAutoscalers(f.ns).Update(ctx, hpa, metav1.UpdateOptions{})
	if ptr.Deref(deploy.Spec.Replicas, 0) != desired {
		deploy.Spec.Replicas = ptr.To(desired)
		_, _ = f.c.AppsV1().Deployments(f.ns).Update(ctx, deploy, metav1.UpdateOptions{})
	}
}

// maybeReplacePod performs the one-off eviction replacePodDuringNegativeControl
// asks for: 150ms after the HPA appeared, with the metric still at baseline,
// the current pods are deleted and a pod under a new name takes their place.
func (f *fakeHPAControlPlane) maybeReplacePod(ctx context.Context, value int) {
	f.mu.Lock()
	due := f.replacePodDuringNegativeControl && !f.replaced && value == hpaBaselineValue &&
		!f.hpaSeen.IsZero() && time.Since(f.hpaSeen) > 150*time.Millisecond
	if due {
		f.replaced = true
	}
	f.mu.Unlock()
	if !due {
		return
	}
	list, err := f.c.CoreV1().Pods(f.ns).List(ctx, metav1.ListOptions{LabelSelector: hpaWorkloadSelector()})
	if err != nil {
		return
	}
	for _, p := range list.Items {
		_ = f.c.CoreV1().Pods(f.ns).Delete(ctx, p.Name, metav1.DeleteOptions{})
	}
	pod := readyHPAPod(hpaWorkloadName + "-replacement")
	pod.Namespace = f.ns
	_, _ = f.c.CoreV1().Pods(f.ns).Create(ctx, &pod, metav1.CreateOptions{})
}

func (f *fakeHPAControlPlane) didReplacePod() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaced
}

func (f *fakeHPAControlPlane) reconcilePods(ctx context.Context, deploy *appsv1.Deployment) {
	want := int(ptr.Deref(deploy.Spec.Replicas, 0))
	list, err := f.c.CoreV1().Pods(f.ns).List(ctx, metav1.ListOptions{LabelSelector: hpaWorkloadSelector()})
	if err != nil {
		return
	}
	ready := 0
	for i := range list.Items {
		pod := &list.Items[i]
		if podIsReady(pod) {
			ready++
			continue
		}
		f.mu.Lock()
		since, held := f.pendingSince[pod.Name]
		f.mu.Unlock()
		if held && time.Since(since) >= f.holdNewPodPendingFor {
			pod.Status = readyHPAPod(pod.Name).Status
			_, _ = f.c.CoreV1().Pods(f.ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
			ready++
		}
	}
	for i := len(list.Items); i < want; i++ {
		pod := readyHPAPod(fmt.Sprintf("%s-%d", hpaWorkloadName, i))
		pod.Namespace = f.ns
		if f.holdNewPodPendingFor > 0 && i > 0 { // the first pod is the baseline pod
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
			f.mu.Lock()
			f.pendingSince[pod.Name] = time.Now()
			f.heldPending = true
			f.mu.Unlock()
		} else {
			ready++
		}
		_, _ = f.c.CoreV1().Pods(f.ns).Create(ctx, &pod, metav1.CreateOptions{})
	}
	for i := want; i < len(list.Items); i++ {
		_ = f.c.CoreV1().Pods(f.ns).Delete(ctx, list.Items[i].Name, metav1.DeleteOptions{})
	}
	n := int32(want)             //nolint:gosec // G115: tiny
	r := int32(min(ready, want)) //nolint:gosec // G115: tiny
	deploy.Status = appsv1.DeploymentStatus{Replicas: n, UpdatedReplicas: n, ReadyReplicas: r, ObservedGeneration: deploy.Generation}
	_, _ = f.c.AppsV1().Deployments(f.ns).Update(ctx, deploy, metav1.UpdateOptions{})
}

// readMetrics serves the last reconciled ConfigMap value for every Ready
// pod, as the adapter would once Prometheus has scraped them; a pod that is
// not Ready has no series yet.
func (f *fakeHPAControlPlane) readMetrics(ctx context.Context) ([]byte, error) {
	if f.adapterBroken || !f.poolLoaded() {
		return nil, k8serrors.NewNotFound(schema.GroupResource{Group: hpaCustomMetricsGroup, Resource: "pods"}, hpaMetricSeries)
	}
	f.mu.Lock()
	value, ok := f.lastMetric, f.scraped
	f.mu.Unlock()
	if !ok {
		return nil, k8serrors.NewNotFound(schema.GroupResource{Group: hpaCustomMetricsGroup, Resource: "pods"}, hpaMetricSeries)
	}
	list, err := f.c.CoreV1().Pods(f.ns).List(ctx, metav1.ListOptions{LabelSelector: hpaWorkloadSelector()})
	if err != nil {
		return nil, err
	}
	values := map[string]int{}
	for i := range list.Items {
		if podIsReady(&list.Items[i]) {
			values[list.Items[i].Name] = value
		}
	}
	return metricSamples(f.ns, values, ""), nil
}

func newTestHPAScenario(t *testing.T, cp *fakeHPAControlPlane, c *k8sfake.Clientset) *hpaScenario {
	t.Helper()
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatalf("newGPUTestRun: %v", err)
	}
	run.namespace = cp.ns
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podMonitorGVR: "PodMonitorList"})
	s := &hpaScenario{
		clientset:             c,
		dynClient:             dyn,
		testRun:               run,
		metricsVersion:        "v1beta1",
		readMetrics:           cp.readMetrics,
		promURL:               "http://prometheus.monitoring.svc:9090",
		listScrapePools:       cp.listScrapePools,
		newScrapePoolLister:   func(string) func(context.Context) ([]byte, error) { return cp.listScrapePools },
		pollInterval:          2 * time.Millisecond,
		workloadReadyTimeout:  2 * time.Second,
		monitorLoadTimeout:    3 * time.Second,
		metricTimeout:         time.Second,
		scaleTimeout:          time.Second,
		scaleDownTimeout:      time.Second,
		negativeControlWindow: 400 * time.Millisecond,
		stabilization:         time.Minute,
		timings:               map[hpaPhase]time.Duration{},
	}
	if cp.noPrometheus {
		s.promURL, s.listScrapePools = "", nil
	}
	return s
}

// TestHPAScenarioPhases drives the full cycle against the fake control plane
// and pins which phase produces each verdict: a correct HPA passes; an HPA
// that scales during the negative control, never scales up, or never scales
// down fails in that phase; an adapter that does not serve the metric fails
// at baseline with a pipeline (not an HPA) error; a baseline pod replaced
// during the negative control does not fail scale-up; nor does a pod the HPA
// adds that stays Pending longer than the metric wait allows. A PodMonitor
// Prometheus loads only after the metric timeout would have expired still
// passes (#3671), one never loaded fails at baseline naming the load, and a
// recipe without a Prometheus to ask skips that wait.
func TestHPAScenarioPhases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*fakeHPAControlPlane)
		wantPhase hpaPhase
		wantCode  errors.ErrorCode
		wantMsg   string
	}{
		{name: "correct HPA passes", configure: func(*fakeHPAControlPlane) {}, wantPhase: hpaPhaseScaleDown},
		{
			name:      "baseline pod replaced during the negative control still passes",
			configure: func(f *fakeHPAControlPlane) { f.replacePodDuringNegativeControl = true },
			wantPhase: hpaPhaseScaleDown,
		},
		{
			// Longer than the scenario's metricTimeout: awaiting the raised
			// metric on the Pending pod too would exhaust the wait.
			name:      "pod added Pending during scale-up still passes",
			configure: func(f *fakeHPAControlPlane) { f.holdNewPodPendingFor = 1200 * time.Millisecond },
			wantPhase: hpaPhaseScaleDown,
		},
		{
			// Longer than the scenario's metricTimeout: judged from the
			// PodMonitor's creation, the metric wait would expire first.
			name:      "PodMonitor loaded after the metric timeout still passes",
			configure: func(f *fakeHPAControlPlane) { f.poolLoadedAfter = 1500 * time.Millisecond },
			wantPhase: hpaPhaseScaleDown,
		},
		{
			name:      "no Prometheus in the recipe skips the load wait",
			configure: func(f *fakeHPAControlPlane) { f.noPrometheus = true },
			wantPhase: hpaPhaseScaleDown,
		},
		{
			name:      "PodMonitor never loaded",
			configure: func(f *fakeHPAControlPlane) { f.poolNever = true },
			wantPhase: hpaPhaseBaseline, wantCode: errors.ErrCodeNotFound,
			wantMsg: "Prometheus did not load the PodMonitor",
		},
		{
			name:      "adapter does not serve the metric",
			configure: func(f *fakeHPAControlPlane) { f.adapterBroken = true },
			wantPhase: hpaPhaseBaseline, wantCode: errors.ErrCodeNotFound,
			wantMsg: "Prometheus loaded the PodMonitor, but the custom metrics pipeline does not serve the workload metric",
		},
		{
			name:      "adapter does not serve the metric and no Prometheus in the recipe",
			configure: func(f *fakeHPAControlPlane) { f.adapterBroken, f.noPrometheus = true, true },
			wantPhase: hpaPhaseBaseline, wantCode: errors.ErrCodeNotFound,
			wantMsg: "phase baseline: [NOT_FOUND] custom metrics pipeline does not serve the workload metric",
		},
		{
			name:      "HPA scales while the metric is below target",
			configure: func(f *fakeHPAControlPlane) { f.flipAtBaselineAfter = 80 * time.Millisecond },
			wantPhase: hpaPhaseNegativeControl, wantCode: errors.ErrCodeInternal,
			wantMsg: "did not hold 1 replica",
		},
		{
			name:      "HPA never scales up",
			configure: func(f *fakeHPAControlPlane) { f.neverScaleUp = true },
			wantPhase: hpaPhaseScaleUp, wantCode: errors.ErrCodeInternal,
			wantMsg: "did not scale the Deployment to 2 replicas",
		},
		{
			name:      "HPA never scales down",
			configure: func(f *fakeHPAControlPlane) { f.neverScaleDown = true },
			wantPhase: hpaPhaseScaleDown, wantCode: errors.ErrCodeInternal,
			wantMsg: "did not scale back down to 1 replica",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := k8sfake.NewClientset()
			cp := newFakeHPAControlPlane(c, hpaTestPrefix+"unit")
			tt.configure(cp)
			s := newTestHPAScenario(t, cp, c)
			cp.start()
			defer cp.shutdown()

			ctx := &validators.Context{Ctx: context.Background(), Clientset: c}
			err := s.runWithCleanup(ctx)
			if s.phase != tt.wantPhase {
				t.Errorf("verdict phase = %s, want %s (err=%v)", s.phase, tt.wantPhase, err)
			}
			if cp.replacePodDuringNegativeControl && !cp.didReplacePod() {
				t.Error("the fake never replaced the baseline pod, so the case did not exercise pod churn")
			}
			if cp.holdNewPodPendingFor > 0 && !cp.didHoldPodPending() {
				t.Error("the fake never held a new pod Pending, so the case did not exercise the scale-up wait set")
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				for _, phase := range []hpaPhase{hpaPhaseDeploy, hpaPhaseBaseline, hpaPhaseNegativeControl, hpaPhaseScaleUp, hpaPhaseScaleDown} {
					if _, ok := s.timings[phase]; !ok {
						t.Errorf("no timing recorded for phase %s", phase)
					}
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error %q lacks %q", err.Error(), tt.wantMsg)
				}
				if !strings.Contains(err.Error(), "phase "+string(tt.wantPhase)) {
					t.Errorf("error %q does not name phase %s", err.Error(), tt.wantPhase)
				}
			}
			if _, getErr := c.CoreV1().Namespaces().Get(context.Background(), cp.ns, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
				t.Errorf("namespace %s not deleted after the run: %v", cp.ns, getErr)
			}
			if _, getErr := s.dynClient.Resource(podMonitorGVR).Namespace(cp.ns).Get(context.Background(), hpaWorkloadName, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
				t.Errorf("PodMonitor not deleted after the run: %v", getErr)
			}
		})
	}
}

// TestReadyLivePodsExcludesPending pins the scale-up wait set: a pod the HPA
// has added that is not yet Ready is not awaited for the raised metric,
// while livePods still counts it.
func TestReadyLivePodsExcludesPending(t *testing.T) {
	t.Parallel()
	const ns = hpaTestPrefix + "ready-unit"
	ready := readyHPAPod(hpaWorkloadName + "-0")
	ready.Namespace = ns
	pending := readyHPAPod(hpaWorkloadName + "-1")
	pending.Namespace = ns
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending}
	c := k8sfake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: hpaWorkloadName, Namespace: ns}}, &ready, &pending)
	s := newTestHPAScenario(t, newFakeHPAControlPlane(c, ns), c)

	got, err := s.readyLivePods(context.Background())
	if err != nil {
		t.Fatalf("readyLivePods: %v", err)
	}
	if want := ready.Name; strings.Join(got, ",") != want {
		t.Errorf("readyLivePods = %v, want [%s]", got, want)
	}
	all, err := s.livePods(context.Background())
	if err != nil {
		t.Fatalf("livePods: %v", err)
	}
	if want := ready.Name + "," + pending.Name; strings.Join(all, ",") != want {
		t.Errorf("livePods = %v, want [%s]", all, want)
	}
}

// TestHPAScenarioAttachPrometheus pins when the baseline waits for the
// PodMonitor to be loaded: a recipe that declares kube-prometheus-stack
// (enabled) with a reachable Prometheus Service turns the wait on, pointed
// at the discovered URL; one declaring it without the Service fails closed;
// one without it, or with it disabled as the OpenShift overlay does, skips
// the wait. The first case is what fails if the one line enabling the wait
// is dropped.
func TestHPAScenarioAttachPrometheus(t *testing.T) {
	t.Parallel()
	declared := []recipe.ComponentRef{{Name: prometheusComponentName, Namespace: "monitoring"}}
	promSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-prometheus-prometheus", Namespace: "monitoring", Labels: map[string]string{"self-monitor": "true"}},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 9090}}},
	}
	tests := []struct {
		name       string
		refs       []recipe.ComponentRef
		objects    []runtime.Object
		wantCode   errors.ErrorCode
		wantWait   bool
		wantURL    string
		wantPolled bool
	}{
		{name: "declared and reachable: wait on", refs: declared, objects: []runtime.Object{promSvc},
			wantWait: true, wantURL: "http://kube-prometheus-prometheus.monitoring.svc:9090", wantPolled: true},
		{name: "declared without a Prometheus Service", refs: declared, wantCode: errors.ErrCodeNotFound},
		{name: "not declared: wait skipped", refs: []recipe.ComponentRef{enabledRef("prometheus-adapter-ocp")}},
		{name: "declared disabled: wait skipped", refs: []recipe.ComponentRef{disabledRef(prometheusComponentName)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := k8sfake.NewClientset(tt.objects...)
			cp := newFakeHPAControlPlane(c, hpaTestPrefix+"attach")
			cp.noPrometheus = true
			s := newTestHPAScenario(t, cp, c)
			var gotURL string
			var polled atomic.Int32
			s.newScrapePoolLister = func(promURL string) func(context.Context) ([]byte, error) {
				gotURL = promURL
				return func(ctx context.Context) ([]byte, error) { polled.Add(1); return cp.listScrapePools(ctx) }
			}
			err := s.attachPrometheus(&validators.Context{Ctx: context.Background(), Clientset: c,
				ValidationInput: &v1.ValidationInput{ComponentRefs: tt.refs}})
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("attachPrometheus: %v", err)
			}
			if (s.listScrapePools != nil) != tt.wantWait {
				t.Errorf("listScrapePools set = %t, want %t", s.listScrapePools != nil, tt.wantWait)
			}
			if gotURL != tt.wantURL || s.promURL != tt.wantURL {
				t.Errorf("lister built for %q, scenario promURL %q, want %q", gotURL, s.promURL, tt.wantURL)
			}
			if (polled.Load() > 0) != tt.wantPolled {
				t.Errorf("preflight listed scrape pools %d times, want polled=%t", polled.Load(), tt.wantPolled)
			}
		})
	}
}

// TestHPAScenarioDeployCreatesScrapeConfig pins that the check itself makes
// the recipe's Prometheus scrape its pods: a PodMonitor selecting the
// metric-source label on the metrics port, carrying the release label.
func TestHPAScenarioDeployCreatesScrapeConfig(t *testing.T) {
	t.Parallel()
	c := k8sfake.NewClientset()
	cp := newFakeHPAControlPlane(c, hpaTestPrefix+"scrape")
	s := newTestHPAScenario(t, cp, c)
	if err := s.deploy(&validators.Context{Ctx: context.Background(), Clientset: c}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	pm, err := s.dynClient.Resource(podMonitorGVR).Namespace(cp.ns).Get(context.Background(), hpaWorkloadName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PodMonitor not created: %v", err)
	}
	if pm.GetLabels()[prometheusReleaseLabel] != prometheusComponentName {
		t.Errorf("PodMonitor labels = %v, want %s=%s", pm.GetLabels(), prometheusReleaseLabel, prometheusComponentName)
	}
	sel, _, _ := unstructured.NestedStringMap(pm.Object, "spec", "selector", "matchLabels")
	if sel[hpaMetricSourceLabel] != "true" {
		t.Errorf("PodMonitor selector = %v, want %s=true", sel, hpaMetricSourceLabel)
	}
	endpoints, _, _ := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	if len(endpoints) != 1 || endpoints[0].(map[string]any)["port"] != hpaMetricsPortName {
		t.Errorf("PodMonitor endpoints = %v, want port %s", endpoints, hpaMetricsPortName)
	}
	if _, err := c.CoreV1().ConfigMaps(cp.ns).Get(context.Background(), hpaWorkloadName, metav1.GetOptions{}); err != nil {
		t.Errorf("metric ConfigMap not created: %v", err)
	}
	if _, err := c.AppsV1().Deployments(cp.ns).Get(context.Background(), hpaWorkloadName, metav1.GetOptions{}); err != nil {
		t.Errorf("Deployment not created: %v", err)
	}
	if _, err := c.AutoscalingV2().HorizontalPodAutoscalers(cp.ns).Get(context.Background(), hpaWorkloadName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("HPA must not exist before the baseline is established: %v", err)
	}
}

// namespaceCreateFailureCases drives the shared ambiguous-create contract for
// every GPU check that registers its teardown before createGPUTestNamespace
// (#3648): an AMBIGUOUS failure — the apiserver persisted the namespace, then
// the response was lost — must still issue the namespace delete and leave
// nothing behind, while a DEFINITIVE rejection must issue no delete at all.
// Either way the "Delete test namespace" artifact is recorded with the
// matching cleanup status (wantStatus). persists installs the namespace in
// the tracker before the error returns; surfacesLate installs it only after
// cleanup's first lookup has already answered NotFound (the server was still
// applying the Create when the client timed out), which a single Delete
// would miss; deleteErr, when set, makes every namespace Delete fail so the
// case proves the create error still wins over the cleanup error (the
// named-return guard).
var namespaceCreateFailureCases = []struct {
	name         string
	persists     bool
	surfacesLate bool
	createErr    func(name string) error
	deleteErr    error
	wantDeleted  bool
	wantStatus   string
}{
	{
		name:     "ambiguous create that persisted the namespace is cleaned up",
		persists: true,
		createErr: func(string) error {
			return k8serrors.NewTimeoutError("request timed out; object may have been persisted", 1)
		},
		wantDeleted: true,
		wantStatus:  "confirmed absent",
	},
	{
		name:         "ambiguous create that surfaces after the first NotFound is cleaned up",
		surfacesLate: true,
		createErr: func(string) error {
			return k8serrors.NewTimeoutError("request timed out; object may have been persisted", 1)
		},
		wantDeleted: true,
		wantStatus:  "confirmed absent",
	},
	{
		name: "definitively rejected create issues no delete",
		createErr: func(name string) error {
			return k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, name,
				stderrors.New("namespaces is forbidden"))
		},
		wantStatus: "never created",
	},
	{
		name:     "failing delete after an ambiguous create keeps the create error",
		persists: true,
		createErr: func(string) error {
			return k8serrors.NewTimeoutError("request timed out; object may have been persisted", 1)
		},
		deleteErr: k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "",
			stderrors.New("namespaces is forbidden")),
		wantDeleted: true,
		wantStatus:  "CLEANUP FAILED",
	},
}

// failNamespaceDelete makes every namespace Delete return deleteErr.
func failNamespaceDelete(c *k8sfake.Clientset, deleteErr error) {
	c.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, deleteErr
	})
}

// shortenCleanupTimeout bounds the cleanup settle loop for the duration of a
// SEQUENTIAL test (cleanupTimeout is package state). It stays above the poll
// interval so a namespace that surfaces after the first lookup still gets a
// second one before the budget expires.
func shortenCleanupTimeout(t *testing.T) {
	t.Helper()
	old := cleanupTimeout
	cleanupTimeout = 3 * defaults.PodPollInterval
	t.Cleanup(func() { cleanupTimeout = old })
}

// failNamespaceCreate makes the fake apiserver answer the namespace Create
// with createErr. With persists the object is committed before the error
// returns; with surfacesLate it is committed only when cleanup first looks
// the namespace up, and that lookup still answers NotFound — the "server
// still applying the Create" shape a single Delete would miss. The returned
// function reports the name the Create used.
func failNamespaceCreate(c *k8sfake.Clientset, persists, surfacesLate bool, createErr func(name string) error) func() string {
	var pending *corev1.Namespace
	c.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace).DeepCopy()
		ns.UID = "uid-ambiguous-create"
		pending = ns
		if persists {
			if err := c.Tracker().Add(ns); err != nil {
				return true, nil, err
			}
		}
		return true, nil, createErr(ns.Name)
	})
	if surfacesLate {
		// Whichever of Get or Delete reaches the namespace first answers
		// NotFound and commits the object behind that answer: a settle loop
		// sees it on its next lookup, a single Delete never does.
		fired := false
		late := func(name string) (handled bool, err error) {
			if fired || pending == nil || name != pending.Name {
				return false, nil // fall through to the tracker
			}
			fired = true
			if err := c.Tracker().Add(pending); err != nil {
				return true, err
			}
			return true, k8serrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, pending.Name)
		}
		c.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
			handled, err := late(action.(k8stesting.GetAction).GetName())
			return handled, nil, err
		})
		c.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
			handled, err := late(action.(k8stesting.DeleteAction).GetName())
			return handled, nil, err
		})
	}
	return func() string {
		if pending == nil {
			return ""
		}
		return pending.Name
	}
}

// namespaceDeleteIssued reports whether the fake recorded a Delete of the
// named namespace.
func namespaceDeleteIssued(actions []k8stesting.Action, name string) bool {
	for _, action := range actions {
		del, ok := action.(k8stesting.DeleteAction)
		if ok && del.GetResource().Resource == "namespaces" && del.GetName() == name {
			return true
		}
	}
	return false
}

// assertNamespaceCreateFailureCleanup checks the outcome shared by every
// check's ambiguous-create case: the create error is the check's result, the
// cleanup artifact was recorded with the expected status (out is the captured
// stdout), the delete was issued exactly when the namespace may exist, a
// namespace that ever existed is gone unless the delete was made to fail,
// and a failed cleanup never replaces the create error.
func assertNamespaceCreateFailureCleanup(t *testing.T, c *k8sfake.Clientset, ns, out string, err error, existed bool, deleteErr error, wantDeleted bool, wantStatus string) {
	t.Helper()
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	if err == nil || !strings.Contains(err.Error(), "failed to create test namespace") {
		t.Errorf("error = %v, want the namespace create failure as the check's result", err)
	}
	if err != nil && strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Errorf("error = %v, want the create error to win over the cleanup error", err)
	}
	if !strings.Contains(out, "--- Delete test namespace ---") {
		t.Error("evidence missing the \"Delete test namespace\" artifact")
	}
	if !strings.Contains(out, wantStatus) {
		t.Errorf("cleanup artifact lacks status %q in output:\n%s", wantStatus, out)
	}
	if got := namespaceDeleteIssued(c.Actions(), ns); got != wantDeleted {
		t.Errorf("namespace delete issued = %t, want %t", got, wantDeleted)
	}
	if existed && deleteErr == nil {
		if _, getErr := c.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
			t.Errorf("namespace %s leaked after an ambiguous create (get err = %v)", ns, getErr)
		}
	}
}

// TestHPAScenarioRunWithCleanup_NamespaceCreateFailure pins that the HPA
// check's teardown runs even when createGPUTestNamespace fails, and never
// starts the cycle on that path. Sequential: captureStdout swaps os.Stdout.
func TestHPAScenarioRunWithCleanup_NamespaceCreateFailure(t *testing.T) {
	shortenCleanupTimeout(t)
	for _, tt := range namespaceCreateFailureCases {
		t.Run(tt.name, func(t *testing.T) {
			c := k8sfake.NewClientset()
			failNamespaceCreate(c, tt.persists, tt.surfacesLate, tt.createErr)
			if tt.deleteErr != nil {
				failNamespaceDelete(c, tt.deleteErr)
			}
			run, err := newGPUTestRun()
			if err != nil {
				t.Fatalf("newGPUTestRun: %v", err)
			}
			run.namespace = hpaTestPrefix + run.token
			s := &hpaScenario{
				clientset: c,
				dynClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
					map[schema.GroupVersionResource]string{podMonitorGVR: "PodMonitorList"}),
				testRun: run,
				timings: map[hpaPhase]time.Duration{},
			}

			out := captureStdout(t, func() {
				err = s.runWithCleanup(&validators.Context{Ctx: context.Background(), Clientset: c})
			})
			assertNamespaceCreateFailureCleanup(t, c, run.namespace, out, err, tt.persists || tt.surfacesLate, tt.deleteErr, tt.wantDeleted, tt.wantStatus)
			if s.phase != "" {
				t.Errorf("phase = %q, want none — the cycle must not start without a namespace", s.phase)
			}
		})
	}
}

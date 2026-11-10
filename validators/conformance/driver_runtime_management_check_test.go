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
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testObservedDriver   = "580.65.06"
	testAdvertisedDriver = "580.65.06"
)

// probeLogs renders the RESULT lines the probe prints on a healthy node.
func probeLogs(count int, runtimeOK, driverOK bool, version string) string {
	return strings.Join([]string{
		"some unrelated prologue",
		fmt.Sprintf("%s%d", driverRuntimeCountPrefix, count),
		fmt.Sprintf("%s%t", driverRuntimeConfigOKPrefix, runtimeOK),
		fmt.Sprintf("%s%t", driverRuntimeFunctionalPrefix, driverOK),
		driverRuntimeActualDriverPrefix + version,
		"",
	}, "\n")
}

// serveProbeLogs makes the fake clientset answer every pods/log read with logs.
func serveProbeLogs(client *k8sfake.Clientset, logs string) {
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: []byte(logs)}, nil
	})
}

func withNodeLabels(labels map[string]string) nodeOpt {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for k, v := range labels {
			n.Labels[k] = v
		}
	}
}

// driverAttrDevice is a gpu.nvidia.com device advertising driverVersion as a
// DRA version attribute (the NVIDIA DRA driver's shape).
func driverAttrDevice(name, version string) map[string]any {
	return map[string]any{
		"name": name,
		"attributes": map[string]any{
			draDriverVersionAttribute: map[string]any{"version": version},
		},
	}
}

func TestVersionsCompatible(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"580.65.06", "580.65.06", true},
		{"580.65.6", "580.65.06", true}, // DRA SemVer normalization vs nvidia-smi
		{"580.65", "580.65.06", true},   // a shorter version is a less precise statement of the same version
		{"580", "580.65.06", true},
		{"v580.65.06", "580.65.06", true},
		{"580.65.06-rc1", "580.65.06", true},
		{"550.54.14", "580.65.06", false},
		{"580.65.07", "580.65.06", false},
		{"N/A", "580.65.06", false},
		{"N/A", "n/a", true},
		{"", "", false},
		{"", "580.65.06", false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q_vs_%q", tc.a, tc.b), func(t *testing.T) {
			if got := versionsCompatible(tc.a, tc.b); got != tc.want {
				t.Errorf("versionsCompatible(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
			}
			if got := versionsCompatible(tc.b, tc.a); got != tc.want {
				t.Errorf("versionsCompatible(%q, %q) = %t, want %t (must be symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestParseDriverRuntimeProbeLogs(t *testing.T) {
	t.Run("complete output", func(t *testing.T) {
		got := parseDriverRuntimeProbeLogs(probeLogs(1, true, true, testObservedDriver))
		want := driverRuntimeProbeResults{AcceleratorCount: 1, HasAcceleratorCount: true, RuntimeConfigOK: true, DriverFunctional: true, ActualDriverVersion: testObservedDriver, Complete: true}
		if got != want {
			t.Errorf("parse = %+v, want %+v", got, want)
		}
	})
	t.Run("empty version line is complete but not functional", func(t *testing.T) {
		got := parseDriverRuntimeProbeLogs(probeLogs(1, true, false, ""))
		if !got.Complete || got.DriverFunctional || got.ActualDriverVersion != "" {
			t.Errorf("parse = %+v, want complete, not functional, empty version", got)
		}
	})
	t.Run("missing RESULT line is incomplete", func(t *testing.T) {
		logs := driverRuntimeCountPrefix + "1\n" + driverRuntimeConfigOKPrefix + "true\n"
		if got := parseDriverRuntimeProbeLogs(logs); got.Complete {
			t.Errorf("parse = %+v, want incomplete", got)
		}
	})
	t.Run("non-numeric count is not a count", func(t *testing.T) {
		got := parseDriverRuntimeProbeLogs(driverRuntimeCountPrefix + "many\n")
		if got.HasAcceleratorCount || got.Complete {
			t.Errorf("parse = %+v, want no count and incomplete", got)
		}
	})
	t.Run("default fake logs are incomplete", func(t *testing.T) {
		if got := parseDriverRuntimeProbeLogs("fake logs"); got.Complete {
			t.Errorf("parse = %+v, want incomplete", got)
		}
	})
}

func TestNodeDriverVersion(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		wantKey     string
		wantVersion string
	}{
		{
			name:        "full GFD label",
			labels:      map[string]string{gfdDriverVersionLabel: " 580.65.06 ", gfdLegacyDriverMajorLabel: "580"},
			wantKey:     gfdDriverVersionLabel,
			wantVersion: "580.65.06",
		},
		{
			name:        "legacy split labels compose",
			labels:      map[string]string{gfdLegacyDriverMajorLabel: "580", gfdLegacyDriverMinorLabel: "65", gfdLegacyDriverRevLabel: "06"},
			wantKey:     gfdLegacyDriverMajorLabel + "+" + gfdLegacyDriverMinorLabel + "+" + gfdLegacyDriverRevLabel,
			wantVersion: "580.65.06",
		},
		{
			name:   "partial legacy labels advertise nothing",
			labels: map[string]string{gfdLegacyDriverMajorLabel: "580", gfdLegacyDriverMinorLabel: "65"},
		},
		{
			name:        "annotation fallback",
			annotations: map[string]string{gfdDriverVersionLabel: "580.65.06"},
			wantKey:     gfdDriverVersionLabel,
			wantVersion: "580.65.06",
		},
		{
			name:   "blank label advertises nothing",
			labels: map[string]string{gfdDriverVersionLabel: "  "},
		},
		{name: "no labels"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: tt.labels, Annotations: tt.annotations}}
			key, version := nodeDriverVersion(node)
			if key != tt.wantKey || version != tt.wantVersion {
				t.Errorf("nodeDriverVersion() = (%q, %q), want (%q, %q)", key, version, tt.wantKey, tt.wantVersion)
			}
		})
	}
}

func TestVerifyDriverRuntime(t *testing.T) {
	advertised := func(version string) *advertisedDriverVersion {
		return &advertisedDriverVersion{Mechanism: driverVersionMechanismNodeMetadata, NodeName: "node1", Key: gfdDriverVersionLabel, Version: version}
	}
	complete := func(count int64, runtimeOK, driverOK bool, version string) driverRuntimeProbeResults {
		return driverRuntimeProbeResults{AcceleratorCount: count, HasAcceleratorCount: true, RuntimeConfigOK: runtimeOK, DriverFunctional: driverOK, ActualDriverVersion: version, Complete: true}
	}
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}

	tests := []struct {
		name       string
		advertised *advertisedDriverVersion
		probe      driverRuntimeProbeResults
		wantErr    string
	}{
		{"matching versions pass", advertised("580.65.06"), complete(1, true, true, "580.65.06"), ""},
		{"shared precision passes", advertised("580.65"), complete(1, true, true, "580.65.06"), ""},
		{"unadvertised version passes on the probe alone", advertised(""), complete(1, true, true, "580.65.06"), ""},
		{"incomplete probe output fails", advertised(""), driverRuntimeProbeResults{HasAcceleratorCount: true, AcceleratorCount: 1}, "did not report every RESULT line"},
		{"no device seen fails", advertised(""), complete(0, false, false, ""), "saw 0 accelerator device(s), expected 1"},
		{"two devices seen fails", advertised(""), complete(2, true, true, "580.65.06"), "saw 2 accelerator device(s), expected 1"},
		{"tools not injected fails", advertised(""), complete(1, false, false, ""), "did not inject the accelerator driver tools"},
		{"driver not functional fails", advertised(""), complete(1, true, false, ""), "failed to initialize or report a numeric version"},
		{"functional without a version fails", advertised(""), complete(1, true, true, ""), "failed to initialize or report a numeric version"},
		{"version mismatch fails", advertised("550.54.14"), complete(1, true, true, "580.65.06"), "does not match the driver version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyDriverRuntime(tt.advertised, tt.probe, pod)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyDriverRuntime() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyDriverRuntime() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tt.wantErr)
			}
			if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
				t.Errorf("error code = %v, want ErrCodeInternal", err)
			}
		})
	}
}

func TestResolveDriverRuntimeProbeImage(t *testing.T) {
	tests := []struct {
		name     string
		override string
		registry string
		want     string
	}{
		{name: "default", want: driverRuntimeProbeImage},
		// The validator registry override names where validator images live
		// (dev lanes point it at a namespace holding only those); the probe
		// image must not follow it (seen live: ErrImagePull on
		// ghcr.io/<user>/ubuntu:22.04).
		{name: "validator registry override does not apply", registry: "registry.example.com/mirror", want: driverRuntimeProbeImage},
		{name: "full reference override wins", override: "mirror.example.com/os/ubuntu:22.04", registry: "registry.example.com/mirror", want: "mirror.example.com/os/ubuntu:22.04"},
		{name: "blank override is unset", override: "  ", want: driverRuntimeProbeImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(driverRuntimeImageEnv, tt.override)
			t.Setenv(validatorImageRegistryEnv, tt.registry)
			if got := resolveDriverRuntimeProbeImage(); got != tt.want {
				t.Errorf("resolveDriverRuntimeProbeImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildDriverRuntimeProbePod(t *testing.T) {
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatal(err)
	}
	if len(run.driverProbePodName) > 63 || !strings.HasSuffix(run.driverProbePodName, run.token) {
		t.Errorf("driverProbePodName %q must be a DNS-1123 name carrying the run token", run.driverProbePodName)
	}

	t.Run("device plugin", func(t *testing.T) {
		pod := buildDriverRuntimeProbePod(run, "img:1", nil, false, []string{"node1", "node2"})
		if pod.Name != run.driverProbePodName || pod.Namespace != run.namespace {
			t.Errorf("pod %s/%s, want %s/%s", pod.Namespace, pod.Name, run.namespace, run.driverProbePodName)
		}
		requireNodeNameAffinity(t, pod, []string{"node1", "node2"})
		if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "img:1" {
			t.Fatalf("containers = %+v, want one container with image img:1", pod.Spec.Containers)
		}
		c := pod.Spec.Containers[0]
		if qty, ok := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; !ok || qty.Cmp(resource.MustParse("1")) != 0 {
			t.Errorf("limits = %v, want %s: 1", c.Resources.Limits, resourceNVIDIAGPU)
		}
		if len(pod.Spec.ResourceClaims) != 0 || len(c.Resources.Claims) != 0 {
			t.Error("device plugin probe must not reference resource claims")
		}
		if c.Command[2] != driverRuntimeProbeScript {
			t.Error("probe command must be driverRuntimeProbeScript")
		}
		for _, want := range []string{"--query-gpu=driver_version --format=csv,noheader,nounits", "/usr/local/nvidia/bin/nvidia-smi", "/usr/local/nvidia/lib64", driverRuntimeCountPrefix, driverRuntimeActualDriverPrefix, "exit 0"} {
			if !strings.Contains(c.Command[2], want) {
				t.Errorf("probe script lacks %q", want)
			}
		}
		if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
			t.Errorf("tolerations = %v, want tolerate-all default", pod.Spec.Tolerations)
		}
		if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("restartPolicy = %s, want Never", pod.Spec.RestartPolicy)
		}
	})
	t.Run("DRA", func(t *testing.T) {
		tol := []corev1.Toleration{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
		pod := buildDriverRuntimeProbePod(run, "img:1", tol, true, nil)
		if pod.Spec.Affinity != nil || pod.Spec.NodeName != "" {
			t.Error("DRA probe must leave placement to the scheduler (no affinity, no nodeName)")
		}
		if len(pod.Spec.ResourceClaims) != 1 || *pod.Spec.ResourceClaims[0].ResourceClaimName != run.claimName {
			t.Errorf("resourceClaims = %+v, want the run's claim", pod.Spec.ResourceClaims)
		}
		c := pod.Spec.Containers[0]
		if len(c.Resources.Claims) != 1 || c.Resources.Claims[0].Name != gpuClaimName {
			t.Errorf("container claims = %+v, want %s", c.Resources.Claims, gpuClaimName)
		}
		if _, hasGPU := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
			t.Error("DRA probe must not request nvidia.com/gpu limits")
		}
		if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "dedicated" {
			t.Errorf("tolerations = %v, want the context tolerations", pod.Spec.Tolerations)
		}
	})
}

// driverRuntimeDevicePluginCluster is a Ready device-plugin node (optionally
// labeled) with pods terminal on create and the DRA API served without a
// gpu.nvidia.com DeviceClass, so the check runs the device-plugin path.
func driverRuntimeDevicePluginCluster(t *testing.T, labels map[string]string) (*k8sfake.Clientset, *[]*corev1.Pod) {
	t.Helper()
	// The probe image must come from the code under test, not from a
	// developer or CI shell that set the override.
	t.Setenv(driverRuntimeImageEnv, "")
	t.Setenv(validatorImageRegistryEnv, "")
	clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8"), withNodeLabels(labels)))
	withDRAAPIDiscovery(t, clientset)
	created := markPodsSucceededOnCreate(clientset)
	return clientset, created
}

func requireNoLeakedNamespace(t *testing.T, clientset *k8sfake.Clientset) {
	t.Helper()
	namespaces, err := clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list namespaces: %v", err)
	}
	for _, ns := range namespaces.Items {
		if strings.HasPrefix(ns.Name, gpuTestNamespacePrefix) {
			t.Errorf("per-run test namespace %s leaked", ns.Name)
		}
	}
}

func TestCheckDriverRuntimeManagement_DevicePluginPathPasses(t *testing.T) {
	clientset, created := driverRuntimeDevicePluginCluster(t, map[string]string{gfdDriverVersionLabel: testAdvertisedDriver})
	serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

	var err error
	out := captureStdout(t, func() { err = CheckDriverRuntimeManagement(ctx) })
	if err != nil {
		t.Fatalf("CheckDriverRuntimeManagement() error = %v\n%s", err, out)
	}
	pod := findPodByPrefix(*created, driverRuntimeProbePodPrefix)
	if pod == nil {
		t.Fatalf("probe pod (prefix %q) was not created; created=%d", driverRuntimeProbePodPrefix, len(*created))
	}
	requireNodeNameAffinity(t, pod, []string{"node1"})
	if pod.Spec.Containers[0].Image != driverRuntimeProbeImage {
		t.Errorf("probe image = %q, want %q", pod.Spec.Containers[0].Image, driverRuntimeProbeImage)
	}
	for _, want := range []string{
		"Mechanism: " + driverVersionMechanismNodeMetadata,
		"Version:   " + testAdvertisedDriver,
		"Key:       " + gfdDriverVersionLabel,
		"verdict:            PASS",
		"--- Driver runtime probe pod logs ---",
		"mode exercised: device plugin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("an advertised version must not record the warning:\n%s", out)
	}
	requireNoLeakedNamespace(t, clientset)
}

func TestCheckDriverRuntimeManagement_LegacyLabelsAndSharedPrecision(t *testing.T) {
	clientset, _ := driverRuntimeDevicePluginCluster(t, map[string]string{
		gfdLegacyDriverMajorLabel: "580", gfdLegacyDriverMinorLabel: "65", gfdLegacyDriverRevLabel: "6",
	})
	serveProbeLogs(clientset, probeLogs(1, true, true, "580.65.06"))
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

	var err error
	out := captureStdout(t, func() { err = CheckDriverRuntimeManagement(ctx) })
	if err != nil {
		t.Fatalf("CheckDriverRuntimeManagement() error = %v\n%s", err, out)
	}
	if !strings.Contains(out, "composed from the legacy GFD") || !strings.Contains(out, "Version:   580.65.6") {
		t.Errorf("stdout must record the legacy composition 580.65.6:\n%s", out)
	}
}

func TestCheckDriverRuntimeManagement_UnadvertisedVersionWarnsAndPasses(t *testing.T) {
	clientset, _ := driverRuntimeDevicePluginCluster(t, nil)
	serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

	var err error
	out := captureStdout(t, func() { err = CheckDriverRuntimeManagement(ctx) })
	if err != nil {
		t.Fatalf("CheckDriverRuntimeManagement() error = %v (an unadvertised version is a warning, not a failure)\n%s", err, out)
	}
	for _, want := range []string{"WARNING: node node1 advertises no accelerator driver version", "Version:   <none advertised>", "verdict:            PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestCheckDriverRuntimeManagement_FailurePaths(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		logs    string
		wantErr string
	}{
		{
			name:    "version mismatch",
			labels:  map[string]string{gfdDriverVersionLabel: "550.54.14"},
			logs:    probeLogs(1, true, true, testObservedDriver),
			wantErr: "does not match the driver version",
		},
		{
			name:    "tools not injected",
			labels:  map[string]string{gfdDriverVersionLabel: testAdvertisedDriver},
			logs:    probeLogs(1, false, false, ""),
			wantErr: "did not inject the accelerator driver tools",
		},
		{
			name:    "driver not functional",
			labels:  map[string]string{gfdDriverVersionLabel: testAdvertisedDriver},
			logs:    probeLogs(1, true, false, ""),
			wantErr: "failed to initialize or report a numeric version",
		},
		{
			name:    "no device visible",
			labels:  nil,
			logs:    probeLogs(0, false, false, ""),
			wantErr: "saw 0 accelerator device(s), expected 1",
		},
		{
			name:    "probe output incomplete",
			labels:  map[string]string{gfdDriverVersionLabel: testAdvertisedDriver},
			logs:    "fake logs",
			wantErr: "did not report every RESULT line",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset, _ := driverRuntimeDevicePluginCluster(t, tt.labels)
			serveProbeLogs(clientset, tt.logs)
			ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

			var err error
			out := captureStdout(t, func() { err = CheckDriverRuntimeManagement(ctx) })
			if err == nil {
				t.Fatalf("CheckDriverRuntimeManagement() = nil, want error containing %q\n%s", tt.wantErr, out)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tt.wantErr)
			}
			if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
				t.Errorf("error code = %v, want ErrCodeInternal", err)
			}
			// Diagnostics ship on failure: logs, the advertised version and
			// the verdict artifact precede the error.
			for _, want := range []string{"--- Driver runtime probe pod logs ---", "--- Advertised driver version ---", "verdict:            FAIL"} {
				if !strings.Contains(out, want) {
					t.Errorf("stdout missing %q on failure:\n%s", want, out)
				}
			}
			requireNoLeakedNamespace(t, clientset)
		})
	}
}

func TestCheckDriverRuntimeManagement_DRAPath(t *testing.T) {
	tests := []struct {
		name          string
		version       string
		devices       []any
		wantMechanism string
		wantVersion   string
	}{
		{
			name:          "v1 slice attribute preferred over the node label",
			version:       "v1",
			devices:       []any{driverAttrDevice("gpu-0", "580.65.6")},
			wantMechanism: driverVersionMechanismDRA,
			wantVersion:   "580.65.6",
		},
		{
			name:    "v1beta1 basic-wrapped attribute",
			version: versionV1beta1,
			devices: []any{basicWrappedDevice("gpu-0", map[string]any{
				"attributes": map[string]any{draDriverVersionAttribute: map[string]any{"string": "580.65.06"}},
			})},
			wantMechanism: driverVersionMechanismDRA,
			wantVersion:   "580.65.06",
		},
		{
			name:          "slice without the attribute falls back to the node label",
			version:       "v1",
			devices:       []any{plainDevice("gpu-0")},
			wantMechanism: driverVersionMechanismNodeMetadata,
			wantVersion:   testAdvertisedDriver,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiVersion := apiGroupResourceK8sIO + "/" + tt.version
			// No allocatable GPUs: DRA is the only usable mechanism.
			clientset := k8sfake.NewClientset(testNode("node1", withNodeLabels(map[string]string{gfdDriverVersionLabel: testAdvertisedDriver})))
			withDRAAPIDiscoveryAt(t, clientset, apiVersion)
			created := markPodsSucceededOnCreate(clientset)
			serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
			dynClient := newDRAFakeDynamicClientAt(tt.version,
				testDeviceClassAt(apiVersion, draDriverGPU),
				testResourceSliceAt(apiVersion, "s1", draDriverGPU, "node1", 1, 1, map[string]any{"nodeName": "node1"}, tt.devices),
			)
			ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: dynClient}

			var err error
			out := captureStdout(t, func() { err = CheckDriverRuntimeManagement(ctx) })
			if err != nil {
				t.Fatalf("CheckDriverRuntimeManagement() error = %v\n%s", err, out)
			}
			pod := findPodByPrefix(*created, driverRuntimeProbePodPrefix)
			if pod == nil {
				t.Fatal("probe pod was not created")
			}
			if len(pod.Spec.ResourceClaims) != 1 || pod.Spec.Affinity != nil {
				t.Errorf("DRA probe pod must carry one ResourceClaim and no affinity: claims=%d affinity=%v", len(pod.Spec.ResourceClaims), pod.Spec.Affinity)
			}
			if claims, listErr := dynClient.Resource(draGVRAt(tt.version, "resourceclaims")).Namespace(pod.Namespace).List(context.Background(), metav1.ListOptions{}); listErr != nil || len(claims.Items) != 1 {
				t.Errorf("ResourceClaims in %s = %d (err %v), want 1", pod.Namespace, len(claims.Items), listErr)
			}
			for _, want := range []string{"Mechanism: " + tt.wantMechanism, "Version:   " + tt.wantVersion, "mode exercised: DRA", "verdict:            PASS"} {
				if !strings.Contains(out, want) {
					t.Errorf("stdout missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestCheckDriverRuntimeManagement_DRAModeWithoutNodeSliceFails(t *testing.T) {
	// The claim was allocated from node1's slices, yet the probe landed on a
	// node no gpu.nvidia.com slice names: fail rather than fall back.
	clientset := k8sfake.NewClientset(testNode("node1"), testNode("node-elsewhere"))
	withDRAAPIDiscovery(t, clientset)
	markPodsTerminalOnCreate(clientset, func(string) (corev1.PodPhase, int32) { return corev1.PodSucceeded, 0 })
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok {
			pod.Spec.NodeName = "node-elsewhere"
		}
		return false, nil, nil
	})
	serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
	ctx := &validators.Context{
		Ctx: context.Background(), Clientset: clientset,
		DynamicClient: newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("s1", draDriverGPU, "node1", 1, 1, map[string]any{"nodeName": "node1"}, []any{driverAttrDevice("gpu-0", testAdvertisedDriver)}),
		),
	}
	err := CheckDriverRuntimeManagement(ctx)
	if err == nil {
		t.Fatal("expected an error: no gpu.nvidia.com slice names the node the probe landed on")
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
		t.Errorf("error code = %v, want ErrCodeInternal", err)
	}
	if !strings.Contains(err.Error(), "no gpu.nvidia.com ResourceSlice device for node node-elsewhere") {
		t.Errorf("error = %v, want the missing-slice failure", err)
	}
}

// TestCheckDriverRuntimeManagement_ResourceSliceListFailure covers the DRA
// listing error branches of inspectAdvertisedDriverVersion: in DRA mode the
// read failure is the verdict (the claim came from those slices); in
// device-plugin mode it is a recorded note and the node label decides.
func TestCheckDriverRuntimeManagement_ResourceSliceListFailure(t *testing.T) {
	listForbidden := func(dynClient *dynamicfake.FakeDynamicClient) {
		dynClient.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, k8serrors.NewForbidden(schema.GroupResource{Group: apiGroupResourceK8sIO, Resource: "resourceslices"}, "", stderrors.New("RBAC denies resourceslices list"))
		})
	}

	t.Run("DRA mode fails on the read", func(t *testing.T) {
		clientset := k8sfake.NewClientset(testNode("node1", withNodeLabels(map[string]string{gfdDriverVersionLabel: testAdvertisedDriver})))
		withDRAAPIDiscovery(t, clientset)
		markPodsSucceededOnCreate(clientset)
		serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
		dynClient := newDRAFakeDynamicClient(
			testDeviceClass(draDriverGPU),
			testResourceSlice("s1", draDriverGPU, "node1", 1, 1, map[string]any{"nodeName": "node1"}, []any{driverAttrDevice("gpu-0", testAdvertisedDriver)}),
		)
		// The allocation-mode probe lists slices first and must still see
		// them; only the advertisement read (after the pod ran) is denied.
		pods := 0
		clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			pods++
			listForbidden(dynClient)
			return false, nil, nil
		})
		ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: dynClient}
		err := CheckDriverRuntimeManagement(ctx)
		if err == nil || !strings.Contains(err.Error(), "DRA ResourceSlices") {
			t.Fatalf("error = %v, want the ResourceSlice read failure", err)
		}
		if pods != 1 {
			t.Errorf("probe pods created = %d, want 1", pods)
		}
	})
	t.Run("device plugin mode notes the read and falls back to the label", func(t *testing.T) {
		clientset, _ := driverRuntimeDevicePluginCluster(t, map[string]string{gfdDriverVersionLabel: testAdvertisedDriver})
		serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
		dynClient := newDRAFakeDynamicClient() // no DeviceClass: device plugin mode
		clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			listForbidden(dynClient)
			return false, nil, nil
		})
		ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: dynClient}
		var err error
		out := captureStdout(t, func() { err = CheckDriverRuntimeManagement(ctx) })
		if err != nil {
			t.Fatalf("CheckDriverRuntimeManagement() error = %v\n%s", err, out)
		}
		for _, want := range []string{"DRA ResourceSlices unread", "falling back to node metadata", "Key:       " + gfdDriverVersionLabel, "verdict:            PASS"} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout missing %q:\n%s", want, out)
			}
		}
	})
}

func TestCheckDriverRuntimeManagement_NeitherUsable(t *testing.T) {
	clientset := k8sfake.NewClientset(testNode("node1")) // Ready node, no GPUs
	withDRAAPIDiscovery(t, clientset)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}
	err := CheckDriverRuntimeManagement(ctx)
	if err == nil {
		t.Fatal("expected an error when neither allocation mechanism is usable")
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeUnavailable {
		t.Errorf("error code = %v, want ErrCodeUnavailable", err)
	}
	if validators.IsSkip(err) {
		t.Errorf("neither-usable must be an environment failure, not a skip: %v", err)
	}
}

func TestCheckDriverRuntimeManagement_SkipsForSlinkySlurm(t *testing.T) {
	validation := v1.ToValidationInput(&recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}, {Name: "slinky-slurm"}},
	})
	ctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(), ValidationInput: validation}
	err := CheckDriverRuntimeManagement(ctx)
	if err == nil || !validators.IsSkip(err) {
		t.Fatalf("expected a skip error, got %v", err)
	}
	if !strings.Contains(err.Error(), "slinky-slurm") {
		t.Errorf("skip reason = %v, want it to name slinky-slurm", err)
	}

	noCluster := &validators.Context{Ctx: context.Background()}
	if err := CheckDriverRuntimeManagement(noCluster); errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Errorf("nil clientset error = %v, want ErrCodeInvalidRequest", err)
	}
}

func TestCheckDriverRuntimeManagement_FailsFastWhenDeadlineTooShortForCleanup(t *testing.T) {
	clientset, created := driverRuntimeDevicePluginCluster(t, nil)
	deadlineCtx, cancel := context.WithTimeout(context.Background(), gpuCheckMinWorkBudget)
	defer cancel()
	ctx := &validators.Context{Ctx: deadlineCtx, Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

	err := CheckDriverRuntimeManagement(ctx)
	if errorstest.ReportedCode(err) != errors.ErrCodeTimeout {
		t.Fatalf("error = %v, want ErrCodeTimeout fail-fast", err)
	}
	if len(*created) != 0 {
		t.Errorf("fail-fast path created %d pod(s)", len(*created))
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" {
			t.Errorf("fail-fast path issued create on %s", action.GetResource().Resource)
		}
	}
}

func TestCheckDriverRuntimeManagement_CleanupFailureFailsPassingCheck(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	clientset, _ := driverRuntimeDevicePluginCluster(t, map[string]string{gfdDriverVersionLabel: testAdvertisedDriver})
	serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", stderrors.New("RBAC denies namespace deletion"))
	})
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

	err := CheckDriverRuntimeManagement(ctx)
	if err == nil || !strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Fatalf("error = %v, want the terminal cleanup failure to fail the passing check", err)
	}
}

func TestCheckDriverRuntimeManagement_PrimaryErrorNotMaskedByCleanupError(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	clientset, _ := driverRuntimeDevicePluginCluster(t, map[string]string{gfdDriverVersionLabel: "550.54.14"})
	serveProbeLogs(clientset, probeLogs(1, true, true, testObservedDriver))
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", stderrors.New("RBAC denies namespace deletion"))
	})
	ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

	err := CheckDriverRuntimeManagement(ctx)
	if err == nil || !strings.Contains(err.Error(), "does not match the driver version") {
		t.Fatalf("error = %v, want the primary mismatch error, not the cleanup error", err)
	}
}

// TestCheckDriverRuntimeManagement_ProbePodFailedPhaseJudgedFromLogs pins the
// upstream rule that the verdict comes from the RESULT lines, not the pod
// phase: a Failed pod (killed after printing) whose logs are complete is
// judged on what it printed, and a Failed pod with partial output fails
// naming the phase and exit code so the operator sees why the output stopped.
func TestCheckDriverRuntimeManagement_ProbePodFailedPhaseJudgedFromLogs(t *testing.T) {
	tests := []struct {
		name    string
		logs    string
		wantErr string
	}{
		{name: "complete logs pass whatever the phase", logs: probeLogs(1, true, true, testObservedDriver)},
		{name: "partial logs fail naming phase and exit code", logs: driverRuntimeCountPrefix + "1\n", wantErr: "pod phase Failed, exit code 137"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset(testNode("node1", withGPUAllocatable("8"), withNodeLabels(map[string]string{gfdDriverVersionLabel: testAdvertisedDriver})))
			withDRAAPIDiscovery(t, clientset)
			markPodsTerminalOnCreate(clientset, func(string) (corev1.PodPhase, int32) { return corev1.PodFailed, 137 })
			serveProbeLogs(clientset, tt.logs)
			ctx := &validators.Context{Ctx: context.Background(), Clientset: clientset, DynamicClient: newDRAFakeDynamicClient()}

			err := CheckDriverRuntimeManagement(ctx)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckDriverRuntimeManagement() error = %v, want nil: complete logs decide, not the phase", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

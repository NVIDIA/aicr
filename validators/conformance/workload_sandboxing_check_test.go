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
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Kernel identities a gVisor sentry and its host would report.
var (
	testGuestKernel = sandboxKernelIdentity{Release: "4.4.0", Version: "Linux version 4.4.0 #1 SMP Sun Jan 10 15:06:54 PST 2016", BootID: "3f0c6a36-1c1e-4b7e-9a2b-8f4b0a4b2c11"}
	testHostKernel  = sandboxKernelIdentity{Release: "6.8.0-1021-azure", Version: "Linux version 6.8.0-1021-azure (buildd@lcy02) #25-Ubuntu SMP", BootID: "9d3b2e1a-7c4f-4e8a-b2d6-1f0e9c8a7b65"}
)

// sandboxProbeOutcome selects which probes a rendered log passes.
type sandboxProbeOutcome struct {
	pid, kernel, fs, net bool
	completed            bool
	kernelID             sandboxKernelIdentity
	interfaces           string
	pidCount             int
}

func allProbesPass(k sandboxKernelIdentity) sandboxProbeOutcome {
	return sandboxProbeOutcome{pid: true, kernel: true, fs: true, net: true, completed: true, kernelID: k, interfaces: "eth0 lo", pidCount: 4}
}

// sandboxLogs renders the probe script's output for one pod.
func sandboxLogs(o sandboxProbeOutcome) string {
	verdict := func(prefix string, ok bool) string {
		if ok {
			return prefix + sandboxProbePass
		}
		return prefix + sandboxProbeFail
	}
	info := func(prefix, v string) string {
		if v == "" {
			return prefix + sandboxInfoUnknown
		}
		return prefix + v
	}
	lines := []string{
		"some unrelated prologue",
		info(sandboxInfoKernelReleasePrefix, o.kernelID.Release),
		info(sandboxInfoKernelVersionPrefix, o.kernelID.Version),
		info(sandboxInfoBootIDPrefix, o.kernelID.BootID),
		fmt.Sprintf("%s%d", sandboxInfoPIDCountPrefix, o.pidCount),
		verdict(sandboxProbePIDIsolationPrefix, o.pid),
		verdict(sandboxProbeKernelIsolationPrefix, o.kernel),
		verdict(sandboxProbeFSIsolationPrefix, o.fs),
		sandboxInfoInterfacesPrefix + o.interfaces,
		verdict(sandboxProbeNetIsolationPrefix, o.net),
	}
	if o.completed {
		lines = append(lines, sandboxProbeCompletedMarker)
	}
	return strings.Join(lines, "\n") + "\n"
}

// serveSandboxProbeLogs answers successive pods/log reads with logs in order
// (the fake GetLogs action carries no pod name): the check reads the sandboxed
// pod first, then the control pod. The last entry answers any further read.
func serveSandboxProbeLogs(client *k8sfake.Clientset, logs ...string) {
	next := 0
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		l := logs[min(next, len(logs)-1)]
		next++
		return true, &runtime.Unknown{Raw: []byte(l)}, nil
	})
}

func testRuntimeClass(name, handler string) *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: handler}
}

// sandboxTestCluster is a Ready node, the given RuntimeClasses and pods
// terminal on create; the shell's override variables are cleared so the code
// under test resolves its own defaults.
func sandboxTestCluster(t *testing.T, classes ...runtime.Object) (*k8sfake.Clientset, *[]*corev1.Pod) {
	t.Helper()
	t.Setenv(sandboxRuntimeClassEnv, "")
	t.Setenv(sandboxProbeImageEnv, "")
	t.Setenv(validatorImageRegistryEnv, "")
	objects := append([]runtime.Object{testNode("node1")}, classes...)
	clientset := k8sfake.NewClientset(objects...)
	created := markPodsSucceededOnCreate(clientset)
	return clientset, created
}

func sandboxContext(clientset *k8sfake.Clientset) *validators.Context {
	return &validators.Context{Ctx: context.Background(), Clientset: clientset}
}

func requireNoSandboxNamespaceLeak(t *testing.T, clientset *k8sfake.Clientset) {
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

func TestParseSandboxProbeLogs(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want sandboxProbeResults
	}{
		{
			name: "full pass",
			logs: sandboxLogs(allProbesPass(testGuestKernel)),
			want: sandboxProbeResults{PIDIsolation: true, KernelIsolation: true, FSIsolation: true, NetIsolation: true, Completed: true, Kernel: testGuestKernel, Interfaces: "eth0 lo", PIDCount: 4},
		},
		{
			name: "pid isolation failure and unreadable boot_id",
			logs: sandboxLogs(sandboxProbeOutcome{kernel: true, fs: true, net: true, completed: true, kernelID: sandboxKernelIdentity{Release: "6.6.0"}, pidCount: 120}),
			want: sandboxProbeResults{KernelIsolation: true, FSIsolation: true, NetIsolation: true, Completed: true, Kernel: sandboxKernelIdentity{Release: "6.6.0"}, PIDCount: 120},
		},
		{
			name: "network, kernel and fs failure",
			logs: sandboxLogs(sandboxProbeOutcome{pid: true, completed: true, interfaces: "docker0 eth0 lo"}),
			want: sandboxProbeResults{PIDIsolation: true, Completed: true, Interfaces: "docker0 eth0 lo"},
		},
		{
			name: "probe died before completing",
			logs: sandboxInfoKernelReleasePrefix + "6.6.0\n",
			want: sandboxProbeResults{Kernel: sandboxKernelIdentity{Release: "6.6.0"}},
		},
		{
			name: "a FAIL line never passes and a non-numeric count is ignored",
			logs: sandboxProbePIDIsolationPrefix + "FAIL\n" + sandboxInfoPIDCountPrefix + "many\n" + sandboxProbeCompletedMarker + "\n",
			want: sandboxProbeResults{Completed: true},
		},
		{name: "default fake logs", logs: "fake logs", want: sandboxProbeResults{}},
		{name: "empty logs", logs: "", want: sandboxProbeResults{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSandboxProbeLogs(tt.logs); got != tt.want {
				t.Errorf("parseSandboxProbeLogs() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestKernelIdentitiesDiffer(t *testing.T) {
	tests := []struct {
		name       string
		sandboxed  sandboxKernelIdentity
		control    sandboxKernelIdentity
		want       bool
		wantFields []string
	}{
		{"all fields differ", testGuestKernel, testHostKernel, true, []string{"release", "version", "boot_id"}},
		{"identical", testHostKernel, testHostKernel, false, nil},
		{"only boot_id differs", sandboxKernelIdentity{Release: "6.8.0", Version: "v", BootID: "a"}, sandboxKernelIdentity{Release: "6.8.0", Version: "v", BootID: "b"}, true, []string{"boot_id"}},
		{"unreadable boot_id is ignored", sandboxKernelIdentity{Release: "4.4.0", Version: "guest"}, sandboxKernelIdentity{Release: "6.8.0", Version: "host", BootID: "b"}, true, []string{"release", "version"}},
		{"nothing readable on one side never differs", sandboxKernelIdentity{}, testHostKernel, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, fields := kernelIdentitiesDiffer(tt.sandboxed, tt.control)
			if got != tt.want || strings.Join(fields, ",") != strings.Join(tt.wantFields, ",") {
				t.Errorf("kernelIdentitiesDiffer() = (%t, %v), want (%t, %v)", got, fields, tt.want, tt.wantFields)
			}
		})
	}
}

func TestSandboxRuntimeKeyword(t *testing.T) {
	tests := []struct {
		name, handler string
		wantKeyword   string
		wantField     string
	}{
		{"gvisor", "runsc", "gvisor", "name"},
		{"sandbox", "runsc", "runsc", "handler"},
		{"kata-qemu", "kata-qemu", "kata", "name"},
		{"kata-mshv-vm-isolation", "kata-mshv-vm-isolation", "kata", "name"}, // AKS
		{"Isolated", "KATA", "kata", "handler"},                              // case-insensitive
		{"secure", "quark", "quark", "handler"},
		{"krun", "", "krun", "name"},
		{"nvidia", "nvidia", "", ""},
		{"runc", "runc", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.handler, func(t *testing.T) {
			keyword, field := sandboxRuntimeKeyword(testRuntimeClass(tt.name, tt.handler))
			if keyword != tt.wantKeyword || field != tt.wantField {
				t.Errorf("sandboxRuntimeKeyword() = (%q, %q), want (%q, %q)", keyword, field, tt.wantKeyword, tt.wantField)
			}
		})
	}
}

func TestResolveSandboxedRuntimeClass(t *testing.T) {
	tests := []struct {
		name         string
		classes      []runtime.Object
		override     string
		listErr      error
		wantClass    string
		wantSource   string
		wantReason   string
		wantCode     errors.ErrorCode
		wantErr      string
		wantConsults int
	}{
		{
			name:         "found by name",
			classes:      []runtime.Object{testRuntimeClass("nvidia", "nvidia"), testRuntimeClass("gvisor", "runsc")},
			wantClass:    "gvisor",
			wantSource:   sandboxSelectionDetected,
			wantReason:   `name contains the known sandboxed runtime keyword "gvisor"`,
			wantConsults: 2,
		},
		{
			name:         "found by handler",
			classes:      []runtime.Object{testRuntimeClass("isolated", "kata-qemu")},
			wantClass:    "isolated",
			wantSource:   sandboxSelectionDetected,
			wantReason:   `handler contains the known sandboxed runtime keyword "kata"`,
			wantConsults: 1,
		},
		{
			name:         "first match by name order",
			classes:      []runtime.Object{testRuntimeClass("kata-qemu", "kata-qemu"), testRuntimeClass("gvisor", "runsc")},
			wantClass:    "gvisor",
			wantSource:   sandboxSelectionDetected,
			wantConsults: 2,
		},
		{
			name:         "override names an existing class",
			classes:      []runtime.Object{testRuntimeClass("gvisor", "runsc"), testRuntimeClass("custom-vm", "my-vmm")},
			override:     "custom-vm",
			wantClass:    "custom-vm",
			wantSource:   sandboxSelectionOverride,
			wantReason:   "neither name nor handler matches a known sandboxed runtime keyword; accepted as named",
			wantConsults: 1,
		},
		{
			name:         "override names a keyword class",
			classes:      []runtime.Object{testRuntimeClass("kata-mshv-vm-isolation", "kata-mshv-vm-isolation")},
			override:     " kata-mshv-vm-isolation ",
			wantClass:    "kata-mshv-vm-isolation",
			wantSource:   sandboxSelectionOverride,
			wantReason:   `name contains the known sandboxed runtime keyword "kata"`,
			wantConsults: 1,
		},
		{
			name:     "override names a missing class",
			classes:  []runtime.Object{testRuntimeClass("gvisor", "runsc")},
			override: "kata",
			wantCode: errors.ErrCodeInvalidRequest,
			wantErr:  `AICR_SANDBOX_RUNTIME_CLASS names RuntimeClass "kata", which does not exist`,
		},
		{
			name:         "none found",
			classes:      []runtime.Object{testRuntimeClass("nvidia", "nvidia"), testRuntimeClass("runc", "runc")},
			wantCode:     errors.ErrCodeUnavailable,
			wantErr:      sandboxMissingPrerequisite + ": 2 RuntimeClass(es) exist and none names or handles a known sandboxed runtime (gvisor, runsc, kata, sandboxed, quark, krun)",
			wantConsults: 2,
		},
		{
			name:     "no RuntimeClass at all",
			wantCode: errors.ErrCodeUnavailable,
			wantErr:  sandboxMissingPrerequisite + ": 0 RuntimeClass(es) exist",
		},
		{
			name:     "list forbidden is a read failure, not a missing prerequisite",
			listErr:  k8serrors.NewForbidden(schema.GroupResource{Group: "node.k8s.io", Resource: "runtimeclasses"}, "", stderrors.New("RBAC denies runtimeclasses list")),
			wantCode: errors.ErrCodeInternal,
			wantErr:  "failed to read RuntimeClasses",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(sandboxRuntimeClassEnv, tt.override)
			clientset := k8sfake.NewClientset(tt.classes...)
			if tt.listErr != nil {
				clientset.PrependReactor("list", "runtimeclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listErr
				})
			}
			selection, err := resolveSandboxedRuntimeClass(context.Background(), clientset)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if selection.Class != nil {
					t.Errorf("selection.Class = %s, want none on error", selection.Class.Name)
				}
				if len(selection.Candidates) != tt.wantConsults {
					t.Errorf("candidates = %v, want %d consulted", selection.Candidates, tt.wantConsults)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSandboxedRuntimeClass() error = %v", err)
			}
			if selection.Class == nil || selection.Class.Name != tt.wantClass {
				t.Fatalf("selected %+v, want %s", selection.Class, tt.wantClass)
			}
			if selection.Source != tt.wantSource {
				t.Errorf("source = %q, want %q", selection.Source, tt.wantSource)
			}
			if !strings.Contains(selection.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want containing %q", selection.Reason, tt.wantReason)
			}
			if len(selection.Candidates) != tt.wantConsults {
				t.Errorf("candidates = %v, want %d consulted", selection.Candidates, tt.wantConsults)
			}
			if !strings.Contains(selection.describe(), "Selected:  "+tt.wantClass) {
				t.Errorf("describe() lacks the selection:\n%s", selection.describe())
			}
		})
	}
}

func TestResolveSandboxProbeImage(t *testing.T) {
	tests := []struct {
		name     string
		override string
		registry string
		want     string
	}{
		{name: "default", want: sandboxProbeImage},
		// The validator registry override names where validator images live;
		// the probe image must not follow it.
		{name: "validator registry override does not apply", registry: "registry.example.com/mirror", want: sandboxProbeImage},
		{name: "full reference override wins", override: "mirror.example.com/os/ubuntu:22.04", registry: "registry.example.com/mirror", want: "mirror.example.com/os/ubuntu:22.04"},
		{name: "blank override is unset", override: "  ", want: sandboxProbeImage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(sandboxProbeImageEnv, tt.override)
			t.Setenv(validatorImageRegistryEnv, tt.registry)
			if got := resolveSandboxProbeImage(); got != tt.want {
				t.Errorf("resolveSandboxProbeImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildSandboxProbePod(t *testing.T) {
	run, err := newGPUTestRun()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{run.sandboxProbePodName, run.sandboxControlPodName} {
		if len(name) > 63 || !strings.HasSuffix(name, run.token) {
			t.Errorf("pod name %q must be a DNS-1123 name carrying the run token", name)
		}
	}
	wantResources := func(t *testing.T, c corev1.Container) {
		t.Helper()
		for res, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: sandboxProbeCPURequest, corev1.ResourceMemory: sandboxProbeMemoryRequest} {
			if qty, ok := c.Resources.Requests[res]; !ok || qty.Cmp(resource.MustParse(want)) != 0 {
				t.Errorf("requests[%s] = %v, want %s", res, qty, want)
			}
		}
		for res, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: sandboxProbeCPULimit, corev1.ResourceMemory: sandboxProbeMemoryLimit} {
			if qty, ok := c.Resources.Limits[res]; !ok || qty.Cmp(resource.MustParse(want)) != 0 {
				t.Errorf("limits[%s] = %v, want %s", res, qty, want)
			}
		}
		if _, hasGPU := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
			t.Error("the sandbox probe must not request an accelerator")
		}
	}

	t.Run("sandboxed", func(t *testing.T) {
		tol := []corev1.Toleration{{Key: "dedicated", Value: "sandbox", Effect: corev1.TaintEffectNoSchedule}}
		pod := buildSandboxProbePod(run, run.sandboxProbePodName, "img:1", "gvisor", "", tol)
		if pod.Name != run.sandboxProbePodName || pod.Namespace != run.namespace {
			t.Errorf("pod %s/%s, want %s/%s", pod.Namespace, pod.Name, run.namespace, run.sandboxProbePodName)
		}
		if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
			t.Errorf("runtimeClassName = %v, want gvisor", pod.Spec.RuntimeClassName)
		}
		if pod.Spec.NodeName != "" || pod.Spec.Affinity != nil {
			t.Error("the sandboxed pod must go through the scheduler so RuntimeClass scheduling applies")
		}
		if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "dedicated" {
			t.Errorf("tolerations = %v, want the context tolerations", pod.Spec.Tolerations)
		}
		if pod.Labels[sandboxProbeTestLabel] != sandboxProbeTestValue {
			t.Errorf("labels = %v, want %s=%s", pod.Labels, sandboxProbeTestLabel, sandboxProbeTestValue)
		}
		if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("restartPolicy = %s, want Never", pod.Spec.RestartPolicy)
		}
		if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "img:1" || pod.Spec.Containers[0].Name != sandboxProbeContainer {
			t.Fatalf("containers = %+v, want one %s container with image img:1", pod.Spec.Containers, sandboxProbeContainer)
		}
		c := pod.Spec.Containers[0]
		if c.Command[0] != posixShell || c.Command[2] != sandboxProbeScript {
			t.Error("probe command must run sandboxProbeScript under the POSIX shell")
		}
		for _, want := range []string{"/dev/mem", "/dev/kmem", "/etc/kubernetes /var/lib/kubelet /var/log/pods", "/proc/net/dev", "docker0|cbr0|flannel*|cni0|br-*|bond*|dummy*", "kubelet|containerd|containerd-shim*|dockerd|crio|systemd-journal", sandboxProbeCompletedMarker, "exit 0"} {
			if !strings.Contains(c.Command[2], want) {
				t.Errorf("probe script lacks %q", want)
			}
		}
		wantResources(t, c)
	})
	t.Run("control", func(t *testing.T) {
		pod := buildSandboxProbePod(run, run.sandboxControlPodName, "img:1", "", "node1", nil)
		if pod.Spec.RuntimeClassName != nil {
			t.Errorf("runtimeClassName = %q, want unset", *pod.Spec.RuntimeClassName)
		}
		if pod.Spec.NodeName != "node1" {
			t.Errorf("nodeName = %q, want node1 (pinned beside the sandboxed pod)", pod.Spec.NodeName)
		}
		if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
			t.Errorf("tolerations = %v, want tolerate-all", pod.Spec.Tolerations)
		}
		wantResources(t, pod.Spec.Containers[0])
	})
}

func TestVerifyWorkloadSandboxing(t *testing.T) {
	rc := testRuntimeClass("gvisor", "runsc")
	succeeded := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	failedPod := &corev1.Pod{Status: corev1.PodStatus{
		Phase:             corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}}},
	}}
	guest := allProbesPass(testGuestKernel)
	host := allProbesPass(testHostKernel)
	with := func(o sandboxProbeOutcome, mutate func(*sandboxProbeOutcome)) sandboxProbeResults {
		mutate(&o)
		return parseSandboxProbeLogs(sandboxLogs(o))
	}
	same := func(*sandboxProbeOutcome) {}

	tests := []struct {
		name       string
		sandboxed  sandboxProbeResults
		control    sandboxProbeResults
		sandboxPod *corev1.Pod
		wantErr    []string
		absent     []string
	}{
		{name: "all probes pass and the kernel differs", sandboxed: with(guest, same), control: with(host, same)},
		{
			name:      "a control pod that fails its own probes does not matter",
			sandboxed: with(guest, same),
			control:   with(host, func(o *sandboxProbeOutcome) { o.fs = false; o.net = false }),
		},
		{
			name:      "process isolation fails",
			sandboxed: with(guest, func(o *sandboxProbeOutcome) { o.pid = false; o.pidCount = 120 }),
			control:   with(host, same),
			wantErr:   []string{"failed 1 isolation probe(s)", "process isolation:", "120 PIDs visible"},
			absent:    []string{"kernel and memory isolation", "filesystem isolation", "network isolation", "kernel identity"},
		},
		{
			name:      "kernel and memory isolation fails",
			sandboxed: with(guest, func(o *sandboxProbeOutcome) { o.kernel = false }),
			control:   with(host, same),
			wantErr:   []string{"kernel and memory isolation: /dev/mem or /dev/kmem is readable"},
		},
		{
			name:      "filesystem isolation fails",
			sandboxed: with(guest, func(o *sandboxProbeOutcome) { o.fs = false }),
			control:   with(host, same),
			wantErr:   []string{"filesystem isolation: a host path"},
		},
		{
			name:      "network isolation fails",
			sandboxed: with(guest, func(o *sandboxProbeOutcome) { o.net = false; o.interfaces = "docker0 eth0 lo" }),
			control:   with(host, same),
			wantErr:   []string{"network isolation: a host bridge", "interfaces: docker0 eth0 lo"},
		},
		{
			name:      "same kernel identity as the control pod",
			sandboxed: with(host, same),
			control:   with(host, same),
			wantErr:   []string{"kernel identity: the sandboxed pod observes the same kernel identity", "node node1", `handler "runsc" provides no kernel boundary`},
		},
		{
			name:      "every probe fails and each is named",
			sandboxed: with(host, func(o *sandboxProbeOutcome) { o.pid, o.kernel, o.fs, o.net = false, false, false, false }),
			control:   with(host, same),
			wantErr:   []string{"failed 5 isolation probe(s)", "process isolation:", "kernel and memory isolation:", "kernel identity:", "filesystem isolation:", "network isolation:"},
		},
		{
			name:       "sandboxed probe incomplete",
			sandboxed:  with(guest, func(o *sandboxProbeOutcome) { o.completed = false }),
			control:    with(host, same),
			sandboxPod: failedPod,
			wantErr:    []string{"sandboxed probe pod did not print", "pod phase Failed, exit code 137"},
		},
		{
			name:      "control probe incomplete",
			sandboxed: with(guest, same),
			control:   with(host, func(o *sandboxProbeOutcome) { o.completed = false }),
			wantErr:   []string{"unsandboxed control pod did not print", "kernel identity comparison cannot be judged"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxPod := tt.sandboxPod
			if sandboxPod == nil {
				sandboxPod = succeeded
			}
			err := verifyWorkloadSandboxing(rc, "node1", tt.sandboxed, tt.control, sandboxPod, succeeded)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("verifyWorkloadSandboxing() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyWorkloadSandboxing() = nil, want error containing %q", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want containing %q", err, want)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error = %v, must not name %q", err, bad)
				}
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		})
	}
}

// TestSandboxProbeScriptRuns executes the probe script on the local host and
// checks that it runs to completion and reports every probe (PASS or FAIL;
// the host is not expected to be a sandbox). This catches shell syntax errors
// and missing markers without a cluster.
func TestSandboxProbeScriptRuns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found; skipping probe script smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, sh, "-c", sandboxProbeScript).CombinedOutput()
	if err != nil {
		t.Fatalf("probe script exited with %v:\n%s", err, out)
	}
	got := parseSandboxProbeLogs(string(out))
	if !got.Completed {
		t.Errorf("probe script did not print %q:\n%s", sandboxProbeCompletedMarker, out)
	}
	for _, prefix := range []string{sandboxProbePIDIsolationPrefix, sandboxProbeKernelIsolationPrefix, sandboxProbeFSIsolationPrefix, sandboxProbeNetIsolationPrefix, sandboxInfoKernelReleasePrefix, sandboxInfoPIDCountPrefix, sandboxInfoInterfacesPrefix} {
		if !strings.Contains(string(out), prefix) {
			t.Errorf("probe script output lacks %q:\n%s", prefix, out)
		}
	}
}

func TestCheckWorkloadSandboxing_PassesWithSandboxedRuntimeClass(t *testing.T) {
	clientset, created := sandboxTestCluster(t, testRuntimeClass("nvidia", "nvidia"), testRuntimeClass("gvisor", "runsc"))
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)), sandboxLogs(allProbesPass(testHostKernel)))
	ctx := sandboxContext(clientset)

	var err error
	out := captureStdout(t, func() { err = CheckWorkloadSandboxing(ctx) })
	if err != nil {
		t.Fatalf("CheckWorkloadSandboxing() error = %v\n%s", err, out)
	}
	sandboxed := findPodByPrefix(*created, sandboxProbePodPrefix)
	control := findPodByPrefix(*created, sandboxControlPodPrefix)
	if sandboxed == nil || control == nil {
		t.Fatalf("both probe pods must be created; created=%d", len(*created))
	}
	if sandboxed.Spec.RuntimeClassName == nil || *sandboxed.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("sandboxed pod runtimeClassName = %v, want gvisor", sandboxed.Spec.RuntimeClassName)
	}
	if sandboxed.Spec.NodeName != "" {
		t.Errorf("sandboxed pod nodeName = %q, want scheduler placement", sandboxed.Spec.NodeName)
	}
	if control.Spec.RuntimeClassName != nil {
		t.Errorf("control pod runtimeClassName = %q, want unset", *control.Spec.RuntimeClassName)
	}
	if control.Spec.NodeName != "node1" {
		t.Errorf("control pod nodeName = %q, want node1 (the sandboxed pod's node)", control.Spec.NodeName)
	}
	for _, pod := range []*corev1.Pod{sandboxed, control} {
		if pod.Spec.Containers[0].Image != sandboxProbeImage {
			t.Errorf("probe image = %q, want %q", pod.Spec.Containers[0].Image, sandboxProbeImage)
		}
	}
	for _, want := range []string{
		"--- Sandboxed RuntimeClass ---",
		"Selected:  gvisor",
		"Handler:   runsc",
		"Source:    " + sandboxSelectionDetected,
		"nvidia (handler: nvidia)",
		"--- Sandboxed probe pod logs ---",
		"--- Unsandboxed control pod logs ---",
		"RuntimeClass:  gvisor",
		"RuntimeClass:  none",
		"kernel identity:           differs from the control pod in release, version, boot_id",
		"verdict:                   PASS",
		"--- Cleanup test namespace ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	requireNoSandboxNamespaceLeak(t, clientset)
}

func TestCheckWorkloadSandboxing_OverrideSelectsTheNamedClass(t *testing.T) {
	clientset, created := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"), testRuntimeClass("custom-vm", "my-vmm"))
	t.Setenv(sandboxRuntimeClassEnv, "custom-vm")
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)), sandboxLogs(allProbesPass(testHostKernel)))

	var err error
	out := captureStdout(t, func() { err = CheckWorkloadSandboxing(sandboxContext(clientset)) })
	if err != nil {
		t.Fatalf("CheckWorkloadSandboxing() error = %v\n%s", err, out)
	}
	sandboxed := findPodByPrefix(*created, sandboxProbePodPrefix)
	if sandboxed == nil || sandboxed.Spec.RuntimeClassName == nil || *sandboxed.Spec.RuntimeClassName != "custom-vm" {
		t.Fatalf("sandboxed pod must run on the named class, got %+v", sandboxed)
	}
	for _, want := range []string{"Selected:  custom-vm", "Source:    " + sandboxSelectionOverride, "accepted as named"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestCheckWorkloadSandboxing_NoSandboxedRuntimeClassFails(t *testing.T) {
	clientset, created := sandboxTestCluster(t, testRuntimeClass("nvidia", "nvidia"))

	var err error
	out := captureStdout(t, func() { err = CheckWorkloadSandboxing(sandboxContext(clientset)) })
	if err == nil {
		t.Fatalf("CheckWorkloadSandboxing() = nil, want the missing-prerequisite failure\n%s", out)
	}
	if !strings.Contains(err.Error(), sandboxMissingPrerequisite) {
		t.Errorf("error = %v, want containing %q", err, sandboxMissingPrerequisite)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
	if validators.IsSkip(err) {
		t.Errorf("a missing sandboxed RuntimeClass must fail, never skip: %v", err)
	}
	if len(*created) != 0 {
		t.Errorf("no pod may be created without a RuntimeClass to test, created %d", len(*created))
	}
	for _, want := range []string{"--- Sandboxed RuntimeClass ---", "Selected:  <none>", "nvidia (handler: nvidia)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" {
			t.Errorf("missing-prerequisite path issued create on %s", action.GetResource().Resource)
		}
	}
}

func TestCheckWorkloadSandboxing_OverrideNamesMissingClass(t *testing.T) {
	clientset, created := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"))
	t.Setenv(sandboxRuntimeClassEnv, "kata-qemu")

	err := CheckWorkloadSandboxing(sandboxContext(clientset))
	if err == nil {
		t.Fatal("CheckWorkloadSandboxing() = nil, want an error naming the missing class")
	}
	for _, want := range []string{sandboxRuntimeClassEnv, `"kata-qemu"`, "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want containing %q", err, want)
		}
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if len(*created) != 0 {
		t.Errorf("no pod may be created when the named class is missing, created %d", len(*created))
	}
}

func TestCheckWorkloadSandboxing_FailurePaths(t *testing.T) {
	guest := allProbesPass(testGuestKernel)
	host := allProbesPass(testHostKernel)
	tests := []struct {
		name      string
		sandboxed sandboxProbeOutcome
		control   sandboxProbeOutcome
		wantErr   string
	}{
		{"process isolation", func() sandboxProbeOutcome { o := guest; o.pid = false; return o }(), host, "process isolation:"},
		{"kernel and memory isolation", func() sandboxProbeOutcome { o := guest; o.kernel = false; return o }(), host, "kernel and memory isolation:"},
		{"filesystem isolation", func() sandboxProbeOutcome { o := guest; o.fs = false; return o }(), host, "filesystem isolation:"},
		{"network isolation", func() sandboxProbeOutcome { o := guest; o.net = false; return o }(), host, "network isolation:"},
		{"same kernel identity", host, host, "kernel identity: the sandboxed pod observes the same kernel identity"},
		{"sandboxed probe incomplete", func() sandboxProbeOutcome { o := guest; o.completed = false; return o }(), host, "sandboxed probe pod did not print"},
		{"control probe incomplete", guest, func() sandboxProbeOutcome { o := host; o.completed = false; return o }(), "unsandboxed control pod did not print"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset, created := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"))
			serveSandboxProbeLogs(clientset, sandboxLogs(tt.sandboxed), sandboxLogs(tt.control))

			var err error
			out := captureStdout(t, func() { err = CheckWorkloadSandboxing(sandboxContext(clientset)) })
			if err == nil {
				t.Fatalf("CheckWorkloadSandboxing() = nil, want error containing %q\n%s", tt.wantErr, out)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want containing %q", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if len(*created) != 2 {
				t.Errorf("created %d pods, want both probe pods", len(*created))
			}
			// Diagnostics ship on failure: both logs and the verdict artifact
			// precede the error.
			for _, want := range []string{"--- Sandboxed probe pod logs ---", "--- Unsandboxed control pod logs ---", "--- Workload sandboxing verdict ---", "verdict:                   FAIL"} {
				if !strings.Contains(out, want) {
					t.Errorf("stdout missing %q on failure:\n%s", want, out)
				}
			}
			requireNoSandboxNamespaceLeak(t, clientset)
		})
	}
}

// TestCheckWorkloadSandboxing_ProbePodFailedPhaseJudgedFromLogs pins the rule
// that the verdict comes from the probe lines, not the pod phase: a Failed
// pod (killed after printing) whose logs are complete is judged on what it
// printed.
func TestCheckWorkloadSandboxing_ProbePodFailedPhaseJudgedFromLogs(t *testing.T) {
	t.Setenv(sandboxRuntimeClassEnv, "")
	t.Setenv(sandboxProbeImageEnv, "")
	clientset := k8sfake.NewClientset(testNode("node1"), testRuntimeClass("gvisor", "runsc"))
	markPodsTerminalOnCreate(clientset, func(string) (corev1.PodPhase, int32) { return corev1.PodFailed, 137 })
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)), sandboxLogs(allProbesPass(testHostKernel)))

	if err := CheckWorkloadSandboxing(sandboxContext(clientset)); err != nil {
		t.Fatalf("CheckWorkloadSandboxing() error = %v, want nil: complete logs decide, not the phase", err)
	}
}

func TestCheckWorkloadSandboxing_ControlPodUnschedulableFailsFast(t *testing.T) {
	t.Setenv(sandboxRuntimeClassEnv, "")
	t.Setenv(sandboxProbeImageEnv, "")
	clientset := k8sfake.NewClientset(testNode("node1"), testRuntimeClass("gvisor", "runsc"))
	// The sandboxed pod succeeds on node1; the control pod stays Pending with
	// the scheduler's Unschedulable verdict, which waitForTerminalPod treats
	// as stuck. handled=false lets the tracker store both.
	var created []string
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		created = append(created, pod.Name)
		if strings.HasPrefix(pod.Name, sandboxControlPodPrefix) {
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: string(corev1.PodReasonUnschedulable), Message: "node(s) had untolerated taint",
			}}
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodSucceeded
		pod.Spec.NodeName = "node1"
		return false, nil, nil
	})
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)))

	err := CheckWorkloadSandboxing(sandboxContext(clientset))
	if err == nil {
		t.Fatal("CheckWorkloadSandboxing() = nil, want the stuck control pod failure")
	}
	for _, want := range []string{"unsandboxed control pod stuck", "Unschedulable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want containing %q", err, want)
		}
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	if len(created) != 2 {
		t.Errorf("created pods = %v, want the sandboxed probe and the control pod", created)
	}
	requireNoSandboxNamespaceLeak(t, clientset)
}

// TestCheckWorkloadSandboxing_ControlPodRejectedByKubeletNamesReason pins the
// diagnostic for a spec.nodeName-pinned control pod the kubelet rejects at
// admission: the pod is Failed with a pod-level reason and no container, so
// the log read fails and the error must carry that reason.
func TestCheckWorkloadSandboxing_ControlPodRejectedByKubeletNamesReason(t *testing.T) {
	t.Setenv(sandboxRuntimeClassEnv, "")
	t.Setenv(sandboxProbeImageEnv, "")
	clientset := k8sfake.NewClientset(testNode("node1"), testRuntimeClass("gvisor", "runsc"))
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		if strings.HasPrefix(pod.Name, sandboxControlPodPrefix) {
			pod.Status.Phase = corev1.PodFailed
			pod.Status.Reason = "OutOfcpu"
			pod.Status.Message = "Pod was rejected: Node didn't have enough resource: cpu, requested: 50, used: 3950, capacity: 4000"
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodSucceeded
		pod.Spec.NodeName = "node1"
		return false, nil, nil
	})
	// The sandboxed pod's logs read fine; the control pod has no container
	// to read from.
	reads := 0
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		reads++
		if reads == 1 {
			return true, &runtime.Unknown{Raw: []byte(sandboxLogs(allProbesPass(testGuestKernel)))}, nil
		}
		return true, nil, k8serrors.NewBadRequest("container \"prober\" in pod is waiting to start: ContainerCreating")
	})

	var err error
	out := captureStdout(t, func() { err = CheckWorkloadSandboxing(sandboxContext(clientset)) })
	if err == nil {
		t.Fatalf("CheckWorkloadSandboxing() = nil, want the rejected control pod failure\n%s", out)
	}
	for _, want := range []string{"unsandboxed control pod failed", "reason=OutOfcpu", "didn't have enough resource"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want containing %q", err, want)
		}
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	for _, want := range []string{"Reason:        OutOfcpu", "--- Unsandboxed control pod status ---"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	requireNoSandboxNamespaceLeak(t, clientset)
}

func TestCheckWorkloadSandboxing_SandboxedPodWithoutNodeNameFails(t *testing.T) {
	t.Setenv(sandboxRuntimeClassEnv, "")
	t.Setenv(sandboxProbeImageEnv, "")
	clientset := k8sfake.NewClientset(testNode("node1"), testRuntimeClass("gvisor", "runsc"))
	clientset.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodSucceeded // terminal, yet no node recorded
		return false, nil, nil
	})
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)))

	err := CheckWorkloadSandboxing(sandboxContext(clientset))
	if err == nil || !strings.Contains(err.Error(), "without a node name") {
		t.Fatalf("error = %v, want the missing node name failure", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

func TestCheckWorkloadSandboxing_DoesNotSkipOnSlinkySlurm(t *testing.T) {
	clientset, created := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"))
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)), sandboxLogs(allProbesPass(testHostKernel)))
	ctx := sandboxContext(clientset)
	ctx.ValidationInput = v1.ToValidationInput(&recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}, {Name: "slinky-slurm"}},
	})

	err := CheckWorkloadSandboxing(ctx)
	if err != nil {
		t.Fatalf("CheckWorkloadSandboxing() error = %v; the CPU-only probe needs no GPU, so slinky-slurm is no reason to skip", err)
	}
	if len(*created) != 2 {
		t.Errorf("created %d pods, want both probe pods to run on a Slurm recipe", len(*created))
	}
}

func TestCheckWorkloadSandboxing_NilClientset(t *testing.T) {
	err := CheckWorkloadSandboxing(&validators.Context{Ctx: context.Background()})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestCheckWorkloadSandboxing_FailsFastWhenDeadlineTooShortForCleanup(t *testing.T) {
	clientset, created := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"))
	deadlineCtx, cancel := context.WithTimeout(context.Background(), gpuCheckMinWorkBudget)
	defer cancel()
	ctx := &validators.Context{Ctx: deadlineCtx, Clientset: clientset}

	err := CheckWorkloadSandboxing(ctx)
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if len(*created) != 0 {
		t.Errorf("fail-fast path created %d pod(s)", len(*created))
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "list" || action.GetVerb() == "get" {
			t.Errorf("fail-fast path issued %s on %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestCheckWorkloadSandboxing_CleanupFailureFailsPassingCheck(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	clientset, _ := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"))
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testGuestKernel)), sandboxLogs(allProbesPass(testHostKernel)))
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", stderrors.New("RBAC denies namespace deletion"))
	})

	err := CheckWorkloadSandboxing(sandboxContext(clientset))
	if err == nil || !strings.Contains(err.Error(), "CLEANUP FAILED") {
		t.Fatalf("error = %v, want the terminal cleanup failure to fail the passing check", err)
	}
}

func TestCheckWorkloadSandboxing_PrimaryErrorNotMaskedByCleanupError(t *testing.T) {
	oldTimeout := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = oldTimeout })

	clientset, _ := sandboxTestCluster(t, testRuntimeClass("gvisor", "runsc"))
	serveSandboxProbeLogs(clientset, sandboxLogs(allProbesPass(testHostKernel)), sandboxLogs(allProbesPass(testHostKernel)))
	clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", stderrors.New("RBAC denies namespace deletion"))
	})

	err := CheckWorkloadSandboxing(sandboxContext(clientset))
	if err == nil || !strings.Contains(err.Error(), "kernel identity:") {
		t.Fatalf("error = %v, want the primary kernel identity failure, not the cleanup error", err)
	}
}

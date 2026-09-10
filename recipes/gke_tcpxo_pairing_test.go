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

package recipes

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

// Google ships the GPUDirect-TCPXO NCCL plugin installer and the workload-side
// tcpxo-daemon as a coupled release pair; running a mismatched pair is
// unsupported. This table is the in-repo record of that pairing, per Google's
// release notes:
//
//	https://github.com/GoogleCloudPlatform/container-engine-accelerators/blob/master/gpudirect-tcpxo/README.md
//
// A bump of either side must move both and update this table in the same
// change — that is the coupling this test exists to hold.
var gkeTCXOPluginToDaemonPair = map[string]string{
	"v1.0.15": "v1.0.21",
}

var (
	gkeTCXOPluginImageRe = regexp.MustCompile(`nccl-plugin-gpudirecttcpx-dev:(v[0-9.]+)`)
	gkeTCXODaemonImageRe = regexp.MustCompile(`tcpgpudmarxd-dev:(v[0-9.]+)`)
)

func pluginTagFromInstaller(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(FS, "components/gke-nccl-tcpxo/manifests/nccl-tcpxo-installer.yaml")
	if err != nil {
		t.Fatalf("read installer manifest: %v", err)
	}
	match := gkeTCXOPluginImageRe.FindSubmatch(data)
	if match == nil {
		t.Fatal("installer manifest carries no nccl-plugin-gpudirecttcpx-dev image tag")
	}
	return string(match[1])
}

func daemonTagFrom(t *testing.T, label string, data []byte) string {
	t.Helper()
	match := gkeTCXODaemonImageRe.FindSubmatch(data)
	if match == nil {
		t.Fatalf("%s carries no tcpgpudmarxd-dev image tag", label)
	}
	return string(match[1])
}

// TestGKETCPXODaemonPluginPairing asserts every workload-side tcpxo-daemon pin
// in the repo matches the documented pair of the plugin the installer ships.
// Without it the coupling is a comment and drifts — the validator pinned
// v1.0.20 against a plugin that pairs with v1.0.21 until #2379 corrected it.
func TestGKETCPXODaemonPluginPairing(t *testing.T) {
	pluginTag := pluginTagFromInstaller(t)
	wantDaemon, known := gkeTCXOPluginToDaemonPair[pluginTag]
	if !known {
		t.Fatalf("installer ships plugin %s, which has no entry in the pairing table; "+
			"add the documented pair from Google's release notes", pluginTag)
	}

	// The shipped runtime's daemon pin.
	runtimeData, err := fs.ReadFile(FS,
		"components/kubeflow-trainer/manifests/torch-distributed-tcpxo-cluster-training-runtime.yaml")
	if err != nil {
		t.Fatalf("read torch-distributed-tcpxo manifest: %v", err)
	}
	if got := daemonTagFrom(t, "torch-distributed-tcpxo manifest", runtimeData); got != wantDaemon {
		t.Errorf("torch-distributed-tcpxo daemon pin = %s, want %s (pairs with plugin %s)",
			got, wantDaemon, pluginTag)
	}

	// The validator's testdata runtime and the demo workload — outside the
	// recipes embed, so read from the module tree. Go tests run with the
	// package directory as CWD.
	for _, site := range []struct {
		label string
		path  string
	}{
		{"validator testdata runtime", "../validators/performance/testdata/h100/gke/runtime.yaml"},
		{"demo workload", "../demos/workloads/training/gke-nccl-test-tcpxo.yaml"},
	} {
		data, err := os.ReadFile(site.path)
		if err != nil {
			t.Fatalf("read %s: %v", site.label, err)
		}
		for _, match := range gkeTCXODaemonImageRe.FindAllSubmatch(data, -1) {
			if got := string(match[1]); got != wantDaemon {
				t.Errorf("%s daemon pin = %s, want %s (pairs with plugin %s)",
					site.label, got, wantDaemon, pluginTag)
			}
		}
	}
}

// TestTCPIXORuntimeRendersRecordedMapping proves the recipe-recorded interface
// mapping is what lands in the rendered annotation — in order, behind the
// fixed eth0 → default entry — and that the fabric wiring the runtime
// promises is actually present in the output.
func TestTCPIXORuntimeRendersRecordedMapping(t *testing.T) {
	content, err := fs.ReadFile(FS,
		"components/kubeflow-trainer/manifests/torch-distributed-tcpxo-cluster-training-runtime.yaml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	interfaces := make([]any, 0, 8)
	networks := []string{"gpu-nic-0", "gpu-nic-1", "gpu-nic-2", "gpu-nic-3",
		"gpu-nic-4", "gpu-nic-5", "gpu-nic-6", "gpu-nic-7"}
	for i, network := range networks {
		interfaces = append(interfaces, map[string]any{
			"interfaceName": fmt.Sprintf("eth%d", i+1),
			"network":       network,
		})
	}

	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "kubeflow-trainer",
		Namespace:     "kubeflow",
		ChartName:     "kubeflow-trainer",
		ChartVersion:  "2.2.0",
		Values: map[string]any{
			"tcpxoInterfaces": interfaces,
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	out := string(rendered)

	wantAnnotation := `networking.gke.io/interfaces: '[{"interfaceName":"eth0","network":"default"}`
	for i, network := range networks {
		iface := fmt.Sprintf("eth%d", i+1)
		wantAnnotation += `,{"interfaceName":"` + iface + `","network":"` + network + `"}`
	}
	wantAnnotation += `]'`
	if !strings.Contains(out, wantAnnotation) {
		t.Errorf("rendered manifest missing ordered interfaces annotation\nwant: %s", wantAnnotation)
	}

	for _, want := range []string{
		"name: torch-distributed-tcpxo",
		"trainer.kubeflow.org/framework: torch",
		"numNodes: 2",
		"devices.gke.io/container.tcpxo-daemon:",
		"networking.gke.io/default-interface: eth0",
		"name: tcpxo-daemon",
		"tcpgpudmarxd-dev:v1.0.21",
		"restartPolicy: Always",
		"NET_ADMIN", "NET_BIND_SERVICE", "IPC_LOCK",
		"NCCL_FASTRAK_IFNAME",
		"/dev/aperture_devices",
		"nvidia.com/gpu",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifest missing %q", want)
		}
	}
	if strings.Contains(out, "{{") {
		t.Error("rendered manifest contains unresolved template actions")
	}
}

// TestTCPIXORuntimeFailsVisibleWithoutMapping pins the deliberate no-guard
// contract: with the mapping absent the annotation renders visibly
// incomplete (eth0 only) rather than silently omitting the interfaces block —
// the Go gates (recipe generation fail-closed, bundle ownership) are the
// enforcement, and a template that hides the omission would mask their
// bypass. See issue #2296.
func TestTCPIXORuntimeFailsVisibleWithoutMapping(t *testing.T) {
	content, err := fs.ReadFile(FS,
		"components/kubeflow-trainer/manifests/torch-distributed-tcpxo-cluster-training-runtime.yaml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "kubeflow-trainer",
		Namespace:     "kubeflow",
		ChartName:     "kubeflow-trainer",
		ChartVersion:  "2.2.0",
		Values:        map[string]any{},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	out := string(rendered)
	if !strings.Contains(out, `networking.gke.io/interfaces: '[{"interfaceName":"eth0","network":"default"}]'`) {
		t.Error("without the mapping the annotation should render eth0-only (visibly broken), not be omitted")
	}
}

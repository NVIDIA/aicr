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

package plugin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

// The commands end to end against a real API server, as a certificate user
// whose name the server reports: no username is ever typed.
func TestRunAgainstAnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set (run `make test`)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "charts", "aicr-operator", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	if err = admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "aicr-system"}}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-plugin-run", Labels: map[string]string{v1alpha1.LabelRuntime: v1alpha1.SingletonName}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion, RuntimeUID: "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
			ToPlanDigest: digest, StartingInventoryDigest: digest, ApprovalRequired: true,
			Deadline:      metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second)),
			ActionTimeout: metav1.Duration{Duration: time.Minute},
			Steps:         []v1alpha1.RolloutStep{{Release: "nfd", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable}},
		},
	}
	if err = admin.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	user, err := env.AddUser(envtest.User{Name: "carol", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err = os.WriteFile(kubeconfig, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"describe", ro.Name, "--kubeconfig", kubeconfig}, "required, pending"},
		{[]string{"approve", ro.Name, "--kubeconfig", kubeconfig, "--reason", "window", "--expires", "30m"}, "approved by carol"},
		{[]string{"revoke", ro.Name, "--reason", "incident", "--kubeconfig", kubeconfig}, "revoked by carol"},
		{[]string{"describe", "--kubeconfig", kubeconfig, ro.Name}, "revoked by carol"},
	} {
		var out bytes.Buffer
		if err = Run(ctx, c.args, &out); err != nil || !strings.Contains(out.String(), c.want) {
			t.Fatalf("Run(%q) = %v\n%s", c.args, err, out.String())
		}
	}
	if err = Run(ctx, []string{"approve", "rollout-missing", "--kubeconfig", kubeconfig, "--reason", "x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("approving a missing rollout succeeded")
	}
}

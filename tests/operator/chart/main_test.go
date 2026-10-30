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

// Package chart tests the installation chart with the pinned helm
// against a real API server: rendering, the admission policies, RBAC, Pod
// Security, upgrade and uninstall. envtest's API server
// enforces RBAC and ValidatingAdmissionPolicy; it runs no controllers, so the
// operator never starts.
package chart

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

const (
	release      = "aicr-operator"
	operatorNS   = "aicr-system"
	engineNS     = "argocd"
	jobsNS       = "aicr-jobs"
	discoveryNS  = "aicr-discovery"
	validationNS = "aicr-validation"
	operatorUser = "system:serviceaccount:" + operatorNS + ":aicr-operator"
	aicrImage    = "ghcr.io/nvidia/aicr:v1.0.0@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433"
	registry     = "oci://123456789012.dkr.ecr.us-west-2.amazonaws.com/aicr/cluster"
	helmTimeout  = 2 * time.Minute
)

var (
	chartDir   = filepath.Join("..", "..", "..", "charts", "aicr-operator")
	valuesFile = filepath.Join("testdata", "values.yaml")
	// inClusterValuesFile switches valuesFile to the in-cluster registry.
	inClusterValuesFile = filepath.Join("testdata", "in-cluster.yaml")

	helmBin    string
	helmHome   string
	kubeconfig string
	restConfig *rest.Config
	testEnv    *envtest.Environment
	scheme     = runtime.NewScheme()
	admin      client.Client
)

// TestMain starts envtest with the Argo CD CRDs the chart needs. Without
// envtest assets or helm the suite skips locally and fails in CI.
func TestMain(m *testing.M) {
	missing := ""
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		missing = "KUBEBUILDER_ASSETS is not set"
	} else if path, err := exec.LookPath("helm"); err != nil {
		missing = "helm is not installed"
	} else {
		helmBin = path
	}
	if missing != "" {
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, missing+"; chart tests cannot run")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping chart tests: "+missing)
		os.Exit(0)
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctrllog.SetLogger(logr.Discard())
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata", "argocd-crds.yaml")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	restConfig, testEnv = cfg, env

	tmp, err := os.MkdirTemp("", "aicr-chart-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	helmHome = tmp
	user, err := env.AddUser(envtest.User{Name: "chart-admin", Groups: []string{"system:masters"}}, nil)
	if err == nil {
		var raw []byte
		if raw, err = user.KubeConfig(); err == nil {
			kubeconfig = filepath.Join(tmp, "kubeconfig")
			err = os.WriteFile(kubeconfig, raw, 0o600)
		}
	}
	if err == nil {
		err = clientgoscheme.AddToScheme(scheme)
	}
	if err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err == nil {
		err = apiextensionsv1.AddToScheme(scheme)
	}
	if err == nil {
		admin, err = client.New(cfg, client.Options{Scheme: scheme})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "set up clients: %v\n", err)
		return 1
	}
	return m.Run()
}

// helm runs the pinned helm against envtest with an isolated home.
func helm(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, helmTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, helmBin, append(args, "--kubeconfig", kubeconfig)...)
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(helmHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(helmHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(helmHome, "data"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// render runs helm template in the operator namespace with the test values
// and extra --set pairs.
func render(ctx context.Context, set ...string) ([]unstructured.Unstructured, string, error) {
	return renderIn(ctx, operatorNS, set...)
}

// renderIn renders as render does, with ns as the release namespace.
func renderIn(ctx context.Context, ns string, set ...string) ([]unstructured.Unstructured, string, error) {
	return renderFiles(ctx, ns, []string{valuesFile}, set...)
}

// renderFiles renders as renderIn does, with the given values files.
func renderFiles(ctx context.Context, ns string, files []string, set ...string) ([]unstructured.Unstructured, string, error) {
	args := make([]string, 0, 5+2*len(files)+2*len(set))
	args = append(args, "template", release, chartDir, "--namespace", ns)
	for _, f := range files {
		args = append(args, "-f", f)
	}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out, err := helm(ctx, args...)
	if err != nil {
		return nil, out, err
	}
	var objs []unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(out), 4096)
	for {
		obj := map[string]any{}
		if err = dec.Decode(&obj); err != nil {
			break
		}
		if len(obj) > 0 {
			objs = append(objs, unstructured.Unstructured{Object: obj})
		}
	}
	return objs, out, nil
}

// find returns the rendered object of a kind and name.
func find(t *testing.T, objs []unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i]
		}
	}
	t.Fatalf("no %s %s rendered", kind, name)
	return nil
}

// as returns a client that impersonates a user.
func as(t *testing.T, user string, groups ...string) client.Client {
	t.Helper()
	cfg := rest.CopyConfig(restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: append(groups, "system:authenticated")}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// operator impersonates the operator's ServiceAccount.
func operator(t *testing.T) client.Client {
	return as(t, operatorUser, "system:serviceaccounts", "system:serviceaccounts:"+operatorNS)
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// denied asserts that err is an admission denial carrying want.
func denied(t *testing.T, what string, err error, want string) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s: admitted", what)
	case !strings.Contains(err.Error(), want):
		t.Errorf("%s: denied with %v, want %q", what, err, want)
	}
}

// kubeconfigFor writes a kubeconfig for a user the API server authenticates
// by client certificate, as a real approver's kubectl would.
func kubeconfigFor(t *testing.T, name string, groups ...string) string {
	t.Helper()
	user, err := testEnv.AddUser(envtest.User{Name: name, Groups: groups}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".kubeconfig")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ensureNamespace creates a namespace with labels unless it exists.
func ensureNamespace(ctx context.Context, t *testing.T, name string, labels map[string]string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if err := admin.Create(ctx, ns); client.IgnoreAlreadyExists(err) != nil {
		t.Fatal(err)
	}
}

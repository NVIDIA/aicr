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

// Package qualify is the fault-injection and isolation qualification suite:
// a candidate (`make operator-candidate`), installed from its own chart and
// image digest with the RBAC it ships, on kind with the pinned Argo CD, the
// in-cluster registry and the pinned AICR image. Each scenario injects a
// fault, asserts the prohibited behavior does not happen, reads the cause
// from conditions, Events and rollout status alone, and recovers. Run with
// `make operator-qualify`.
package qualify

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

const (
	operatorNamespace = "aicr-system"
	// approverGroup is bound to the approver roles in testdata/values.yaml.
	approverGroup = "aicr-qualify-approvers"
	approverUser  = "qualify-approver"
)

// candidateManifest is the part of candidate.json the suite checks the
// running operator against.
type candidateManifest struct {
	Version  string `json:"version"`
	Digest   string `json:"digest"`
	Operator struct {
		Repository string            `json:"repository"`
		Digest     string            `json:"digest"`
		Platforms  map[string]string `json:"platforms"`
	} `json:"operator"`
}

var (
	k8s         client.Client
	approver    client.Client
	clientset   kubernetes.Interface
	restConfig  *rest.Config
	aicrVersion string
	candidate   candidateManifest
	next        candidateManifest
)

func TestMain(m *testing.M) {
	path := os.Getenv("AICR_OPERATOR_E2E_KUBECONFIG")
	aicrVersion = os.Getenv("AICR_OPERATOR_E2E_AICR_VERSION")
	if path == "" || aicrVersion == "" || os.Getenv("AICR_OPERATOR_QUALIFY_CANDIDATE") == "" {
		msg := "AICR_OPERATOR_E2E_KUBECONFIG, AICR_OPERATOR_E2E_AICR_VERSION and AICR_OPERATOR_QUALIFY_CANDIDATE are required (run `make operator-qualify`)"
		if os.Getenv("OPERATOR_E2E_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, msg)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "skipping qualification: "+msg)
		os.Exit(0)
	}
	if err := setup(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setup(path string) error {
	for file, into := range map[string]*candidateManifest{
		os.Getenv("AICR_OPERATOR_QUALIFY_CANDIDATE"): &candidate,
		os.Getenv("AICR_OPERATOR_QUALIFY_NEXT"):      &next,
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read candidate %s: %w (build both with `CANDIDATE_VERSION=v0.0.0-rc.0 make operator-candidate` and `CANDIDATE_DIR=dist/candidate-next CANDIDATE_VERSION=v0.0.0-rc.1 make operator-candidate`)", file, err)
		}
		if err = json.Unmarshal(data, into); err != nil {
			return fmt.Errorf("parse candidate %s: %w", file, err)
		}
	}
	if candidate.Operator.Digest == next.Operator.Digest {
		return fmt.Errorf("the upgrade candidate %s is the installed candidate %s", next.Version, candidate.Version)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return fmt.Errorf("load kubeconfig %s: %w", path, err)
	}
	restConfig = cfg
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err == nil {
		err = v1alpha1.AddToScheme(scheme)
	}
	if err != nil {
		return err
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		return err
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{UserName: approverUser, Groups: []string{approverGroup, "system:authenticated"}}
	approver, err = client.New(as, client.Options{Scheme: scheme})
	return err
}

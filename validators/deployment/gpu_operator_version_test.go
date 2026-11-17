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
	"fmt"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	v1 "aicr.run/pkg/validator/v1"
	"aicr.run/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestExtractVersionFromImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "standard image with tag",
			image: "nvcr.io/nvidia/gpu-operator:v24.9.0",
			want:  "v24.9.0",
		},
		{
			name:  "tag without v prefix",
			image: "nvcr.io/nvidia/gpu-operator:24.9.0",
			want:  "v24.9.0",
		},
		{
			name:  "tag with suffix after dash",
			image: "nvcr.io/nvidia/gpu-operator:v24.9.0-ubi8",
			want:  "v24.9.0",
		},
		{
			name:  "tag with suffix no v prefix",
			image: "nvcr.io/nvidia/gpu-operator:24.9.0-ubi8",
			want:  "v24.9.0",
		},
		{
			name:  "no tag returns empty",
			image: "nvcr.io/nvidia/gpu-operator",
			want:  "",
		},
		{
			name:  "empty string returns empty",
			image: "",
			want:  "",
		},
		{
			name:  "registry with port",
			image: "host:5000/nvidia/gpu-operator:v1.0.0",
			want:  "v1.0.0",
		},
		{
			name:  "registry with port and no tag",
			image: "host:5000/nvidia/gpu-operator",
			want:  "",
		},
		{
			name:  "digest only",
			image: "nvcr.io/nvidia/gpu-operator@sha256:" + testDigest,
			want:  "",
		},
		{
			name:  "tag and digest",
			image: "nvcr.io/nvidia/gpu-operator:v25.10.1@sha256:" + testDigest,
			want:  "v25.10.1",
		},
		{
			name:  "registry with port, tag and digest",
			image: "host:5000/nvidia/gpu-operator:v25.10.1-ubi9@sha256:" + testDigest,
			want:  "v25.10.1",
		},
		{
			name:  "registry with port and digest only",
			image: "host:5000/nvidia/gpu-operator@sha256:" + testDigest,
			want:  "",
		},
		{
			name:  "non-sha256 digest only",
			image: "nvcr.io/nvidia/gpu-operator@sha512:" + testDigest + testDigest,
			want:  "",
		},
		{
			name:  "tag and non-sha256 digest",
			image: "nvcr.io/nvidia/gpu-operator:v25.10.1@sha512:" + testDigest + testDigest,
			want:  "v25.10.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractVersionFromImage(tt.image)
			if got != tt.want {
				t.Errorf("extractVersionFromImage(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{
			name:    "already has v prefix",
			version: "v24.9.0",
			want:    "v24.9.0",
		},
		{
			name:    "no v prefix",
			version: "24.9.0",
			want:    "v24.9.0",
		},
		{
			name:    "empty string",
			version: "",
			want:    "",
		},
		{
			name:    "whitespace only",
			version: "   ",
			want:    "",
		},
		{
			name:    "whitespace around version",
			version: "  24.9.0  ",
			want:    "v24.9.0",
		},
		{
			name:    "v prefix with whitespace",
			version: "  v1.2.3  ",
			want:    "v1.2.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeVersion(tt.version)
			if got != tt.want {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestParseImageReference(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		wantTag    string
		wantDigest string
	}{
		{name: "tag", image: "nvcr.io/nvidia/gpu-operator:v25.10.1", wantTag: "v25.10.1"},
		{name: "digest", image: "nvcr.io/nvidia/gpu-operator@sha256:" + testDigest, wantDigest: "sha256:" + testDigest},
		{name: "tag and digest", image: "nvcr.io/nvidia/gpu-operator:v25.10.1@sha256:" + testDigest, wantTag: "v25.10.1", wantDigest: "sha256:" + testDigest},
		{name: "port, no tag", image: "host:5000/gpu-operator"},
		{name: "port and digest", image: "host:5000/gpu-operator@sha256:" + testDigest, wantDigest: "sha256:" + testDigest},
		{name: "non-sha256 digest", image: "nvcr.io/nvidia/gpu-operator@sha512:" + testDigest, wantDigest: "sha512:" + testDigest},
		{name: "bare name", image: "gpu-operator"},
		{name: "empty", image: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tag, digest := parseImageReference(tt.image)
			if tag != tt.wantTag || digest != tt.wantDigest {
				t.Errorf("parseImageReference(%q) = (%q, %q), want (%q, %q)", tt.image, tag, digest, tt.wantTag, tt.wantDigest)
			}
		})
	}
}

// deploymentIn returns a deployment name in namespace ns whose pod runs the
// given images, in order.
func deploymentIn(ns, name string, labels, annotations map[string]string, images ...string) *appsv1.Deployment {
	containers := make([]corev1.Container, 0, len(images))
	for i, image := range images {
		containers = append(containers, corev1.Container{Name: fmt.Sprintf("c%d", i), Image: image})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}},
		},
	}
}

func operatorDeployment(image string, labels, annotations map[string]string) *appsv1.Deployment {
	return deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, labels, annotations, image)
}

func TestGetGPUOperatorVersion(t *testing.T) {
	digestOnly := "nvcr.io/nvidia/gpu-operator@sha256:" + testDigest
	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		wantVer    string
		wantPinned string
		wantErr    bool
	}{
		{
			name:       "tagged image",
			deployment: operatorDeployment("nvcr.io/nvidia/gpu-operator:v25.10.1", nil, nil),
			wantVer:    "v25.10.1",
		},
		{
			name:       "tag and digest reads the tag",
			deployment: operatorDeployment("nvcr.io/nvidia/gpu-operator:v25.10.1@sha256:"+testDigest, nil, nil),
			wantVer:    "v25.10.1",
		},
		{
			name:       "digest only is reported as pinned, not as a version",
			deployment: operatorDeployment(digestOnly, nil, nil),
			wantPinned: digestOnly,
		},
		{
			name:       "digest only with version label uses the label",
			deployment: operatorDeployment(digestOnly, map[string]string{"app.kubernetes.io/version": "v26.3.0"}, nil),
			wantVer:    "v26.3.0",
		},
		{
			name:       "digest only with version annotation uses the annotation",
			deployment: operatorDeployment(digestOnly, nil, map[string]string{"nvidia.com/gpu-operator-version": "26.3.0"}),
			wantVer:    "v26.3.0",
		},
		{
			name:       "untagged image without digest is an error",
			deployment: operatorDeployment("nvcr.io/nvidia/gpu-operator", nil, nil),
			wantErr:    true,
		},
		{
			name:       "untagged image behind a registry port is an error, not a version",
			deployment: operatorDeployment("host:5000/nvidia/gpu-operator", nil, nil),
			wantErr:    true,
		},
		{
			name:    "no deployment is an error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset()
			if tt.deployment != nil {
				clientset = k8sfake.NewClientset(tt.deployment)
			}
			version, pinned, err := getGPUOperatorVersion(context.Background(), clientset)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getGPUOperatorVersion() error = %v, wantErr %v", err, tt.wantErr)
			}
			if version != tt.wantVer || pinned != tt.wantPinned {
				t.Errorf("getGPUOperatorVersion() = (%q, %q), want (%q, %q)", version, pinned, tt.wantVer, tt.wantPinned)
			}
		})
	}
}

// TestGetGPUOperatorVersionSelectionOrder pins which deployment and which
// container decide when several qualify. Deployments are tried in
// namespace-then-name order and the first one with a version source decides;
// within it the label wins, then the first gpu-operator image that carries a
// tag or a digest, then the annotation. A digest-only image decides too, so
// the search does not fall through to a later deployment or container.
func TestGetGPUOperatorVersionSelectionOrder(t *testing.T) {
	digestOnly := "nvcr.io/nvidia/gpu-operator@sha256:" + testDigest
	tagged := "nvcr.io/nvidia/gpu-operator:v25.10.1"
	tests := []struct {
		name        string
		deployments []runtime.Object
		wantVer     string
		wantPinned  string
	}{
		{
			name: "a digest-only first deployment decides over a tagged later one",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, nil, nil, digestOnly),
				deploymentIn("nvidia-gpu-operator", gpuOperatorNamespace, nil, nil, tagged),
			},
			wantPinned: digestOnly,
		},
		{
			name: "a deployment with no version source is passed over",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, nil, nil, "nvcr.io/nvidia/gpu-operator"),
				deploymentIn("nvidia-gpu-operator", gpuOperatorNamespace, nil, nil, tagged),
			},
			wantVer: "v25.10.1",
		},
		{
			name: "deployment names are tried in order within a namespace",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, "nvidia-gpu-operator", nil, nil, "nvcr.io/nvidia/gpu-operator:v24.9.0"),
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, nil, nil, tagged),
			},
			wantVer: "v25.10.1",
		},
		{
			name: "the first gpu-operator image decides even when digest-only",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, nil, nil,
					"nvcr.io/nvidia/k8s-sidecar:v1.0.0", digestOnly, tagged),
			},
			wantPinned: digestOnly,
		},
		{
			name: "the annotation applies when the deciding image is digest-only",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, nil,
					map[string]string{"nvidia.com/gpu-operator-version": "26.3.0"}, digestOnly, tagged),
			},
			wantVer: "v26.3.0",
		},
		{
			name: "an untagged gpu-operator image is passed over for a later tagged one",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace, nil, nil, "host:5000/nvidia/gpu-operator", tagged),
			},
			wantVer: "v25.10.1",
		},
		{
			name: "the label wins over every image",
			deployments: []runtime.Object{
				deploymentIn(gpuOperatorNamespace, gpuOperatorNamespace,
					map[string]string{"app.kubernetes.io/version": "v26.7.0"}, nil, digestOnly, tagged),
			},
			wantVer: "v26.7.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, pinned, err := getGPUOperatorVersion(context.Background(), k8sfake.NewClientset(tt.deployments...))
			if err != nil {
				t.Fatalf("getGPUOperatorVersion() error = %v", err)
			}
			if version != tt.wantVer || pinned != tt.wantPinned {
				t.Errorf("getGPUOperatorVersion() = (%q, %q), want (%q, %q)", version, pinned, tt.wantVer, tt.wantPinned)
			}
		})
	}
}

func checkContext(deploy *appsv1.Deployment, constraint string) *validators.Context {
	ctx := &validators.Context{
		Ctx:             context.Background(),
		Clientset:       k8sfake.NewClientset(deploy),
		ValidationInput: &v1.ValidationInput{},
	}
	if constraint != "" {
		ctx.ValidationInput.Config.Deployment = &v1.ValidationPhase{
			Constraints: []recipe.Constraint{{Name: gpuOperatorVersionConstraint, Value: constraint}},
		}
	}
	return ctx
}

func assertNoVersionExtras(t *testing.T, extra map[string]string) {
	t.Helper()
	for _, key := range []string{validators.ExtraKeyDetectedVersion, validators.ExtraKeyVersionSatisfied} {
		if v, ok := extra[key]; ok {
			t.Errorf("%s = %q, want absent when no version was read", key, v)
		}
	}
}

// TestCheckGPUOperatorVersion_DigestPinned covers an operator image pinned by
// digest alone, with no version label or annotation: a declared constraint
// fails closed, no constraint keeps the no-constraint skip, and neither
// reports a version.
func TestCheckGPUOperatorVersion_DigestPinned(t *testing.T) {
	digestOnly := "nvcr.io/nvidia/gpu-operator@sha256:" + testDigest

	t.Run("declared constraint fails closed", func(t *testing.T) {
		ctx := checkContext(operatorDeployment(digestOnly, nil, nil), ">= v25.10.0")
		extra, err := captureExtras(t, func() error { return checkGPUOperatorVersion(ctx) })
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("checkGPUOperatorVersion() error = %v, want a failure", err)
		}
		if got := errorstest.ReportedCode(err); got != errors.ErrCodeNotFound {
			t.Errorf("error code = %v, want %v", got, errors.ErrCodeNotFound)
		}
		for _, want := range []string{digestOnly, "pinned by digest only", "app.kubernetes.io/version", `">= v25.10.0"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err.Error(), want)
			}
		}
		assertNoVersionExtras(t, extra)
		if v, ok := extra[validators.ExtraKeySkipReason]; ok {
			t.Errorf("skipReason = %q on a failure", v)
		}
	})

	t.Run("no constraint skips", func(t *testing.T) {
		ctx := checkContext(operatorDeployment(digestOnly, nil, nil), "")
		extra, err := captureExtras(t, func() error { return checkGPUOperatorVersion(ctx) })
		if !validators.IsSkip(err) {
			t.Fatalf("checkGPUOperatorVersion() error = %v, want a skip", err)
		}
		assertExtraSubset(t, extra, map[string]string{validators.ExtraKeySkipReason: validators.SkipReasonNoConstraint})
		assertNoVersionExtras(t, extra)
	})
}

// TestCheckGPUOperatorVersion_ImageForms covers the image references the
// parser used to misread.
func TestCheckGPUOperatorVersion_ImageForms(t *testing.T) {
	t.Run("tag and digest is evaluated by its tag", func(t *testing.T) {
		image := "nvcr.io/nvidia/gpu-operator:v25.10.1@sha256:" + testDigest
		ctx := checkContext(operatorDeployment(image, nil, nil), ">= v25.10.0")
		extra, err := captureExtras(t, func() error { return checkGPUOperatorVersion(ctx) })
		if err != nil {
			t.Fatalf("checkGPUOperatorVersion() error = %v, want a pass", err)
		}
		assertExtraSubset(t, extra, map[string]string{
			validators.ExtraKeyDetectedVersion:  "v25.10.1",
			validators.ExtraKeyVersionSatisfied: "true",
		})
	})

	t.Run("untagged image behind a registry port reports no version", func(t *testing.T) {
		ctx := checkContext(operatorDeployment("host:5000/nvidia/gpu-operator", nil, nil), "")
		extra, err := captureExtras(t, func() error { return checkGPUOperatorVersion(ctx) })
		if err == nil || validators.IsSkip(err) {
			t.Fatalf("checkGPUOperatorVersion() error = %v, want the untagged-image failure", err)
		}
		assertNoVersionExtras(t, extra)
	})
}

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

package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

const ecrHost = "123456789012.dkr.ecr.us-west-2.amazonaws.com"

type fakeECR struct {
	token   string
	expires time.Time
	calls   int
	// fail fails the first fail calls, as an STS or ECR outage does.
	fail int
}

func (f *fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	f.calls++
	if f.calls <= f.fail {
		return nil, errors.New("sts: connection reset by peer")
	}
	t := base64.StdEncoding.EncodeToString([]byte("AWS:" + f.token))
	return &ecr.GetAuthorizationTokenOutput{AuthorizationData: []ecrtypes.AuthorizationData{{AuthorizationToken: &t, ExpiresAt: &f.expires}}}, nil
}

// A failed refresh is retried with backoff instead of waiting out the
// interval: one failed mint on a fresh install no longer leaves Argo CD
// without registry credentials for hours.
func TestRepoCredsRefresherRetriesAFailure(t *testing.T) {
	ctx := testContext(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "aicr-registry-retry", Labels: map[string]string{"argocd.argoproj.io/secret-type": "repo-creds"}},
		StringData: map[string]string{"type": "oci", "url": "oci://" + ecrHost + "/aicr/cluster"},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	api := &fakeECR{token: "token-after-retry", expires: time.Now().Add(12 * time.Hour), fail: 1}
	r := &RepoCredsRefresher{Client: k8s, Reader: k8s, Provider: &registry.ECRProvider{Client: api, Host: ecrHost, Now: time.Now},
		Namespace: "argocd", Name: "aicr-registry-retry", Interval: time.Hour, RetryBackoff: 20 * time.Millisecond, Now: time.Now}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Start(runCtx) }()
	refreshed := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !refreshed; time.Sleep(20 * time.Millisecond) {
		got := &corev1.Secret{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), got); err == nil {
			refreshed = string(got.Data["password"]) == "token-after-retry"
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Fatal("a failed refresh was not retried within its backoff; it waited out the 1h interval")
	}
}

// The wait before the next refresh: the interval after a success; after
// failures, a backoff that doubles to its cap and never exceeds the interval.
func TestRefreshWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		failures int
		interval time.Duration
		want     time.Duration
	}{
		{0, 6 * time.Hour, 6 * time.Hour},
		{1, 6 * time.Hour, defaults.RepoCredsRetryBackoff},
		{2, 6 * time.Hour, 2 * defaults.RepoCredsRetryBackoff},
		{40, 6 * time.Hour, defaults.RepoCredsRetryMaxBackoff},
		{1, time.Second, time.Second},
	} {
		if got := refreshWait(tc.failures, tc.interval, defaults.RepoCredsRetryBackoff); got != tc.want {
			t.Errorf("refreshWait(%d, %s) = %s, want %s", tc.failures, tc.interval, got, tc.want)
		}
	}
}

// The refresher writes a current token into repo-creds and records its
// expiry there; Check reports staleness from the Secret, so it holds across
// an operator restart.
func TestRepoCredsRefresher(t *testing.T) {
	ctx := testContext(t)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	api := &fakeECR{token: "token-1", expires: now.Add(12 * time.Hour)}
	provider := &registry.ECRProvider{Client: api, Host: ecrHost, Now: func() time.Time { return clock }}
	r := &RepoCredsRefresher{Client: k8s, Reader: k8s, Provider: provider, Namespace: "argocd", Name: "aicr-registry",
		Now: func() time.Time { return clock }}

	if err := r.Refresh(ctx); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Fatalf("Refresh() without the pre-created Secret = %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "aicr-registry", Labels: map[string]string{"argocd.argoproj.io/secret-type": "repo-creds"}},
		StringData: map[string]string{"type": "oci", "url": "oci://" + ecrHost + "/aicr/cluster"},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(ctx); operrors.ReasonOf(err) != ReasonRegistryCredentialsStale {
		t.Fatalf("Check() before any refresh = %v, want stale", err)
	}
	if expiry, refresh := gauges(r); expiry != 0 || refresh != 0 {
		t.Errorf("before any refresh: expiry %v, refresh %v; want 0, unknown", expiry, refresh)
	}
	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("Refresh() = %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["password"]) != "token-1" || string(secret.Data["username"]) != "AWS" || string(secret.Data["url"]) == "" ||
		secret.Annotations[v1alpha1.AnnotationTokenExpiresAt] != api.expires.Format(time.RFC3339) {

		t.Fatalf("repo-creds = %v %v", secret.Annotations, secret.Data)
	}
	if err := r.Check(ctx); err != nil {
		t.Fatalf("Check() after a refresh = %v", err)
	}
	clock = now.Add(2 * time.Hour)
	if expiry, refresh := gauges(r); expiry != float64(api.expires.Unix()) || refresh != float64(now.Unix()) {
		t.Errorf("expiry %v, refresh %v; want %d, %d", expiry, refresh, api.expires.Unix(), now.Unix())
	}

	// The operator was down past the token's life: stale until the restart's
	// first refresh mints a new token.
	clock = now.Add(13 * time.Hour)
	if err := r.Check(ctx); operrors.ReasonOf(err) != ReasonRegistryCredentialsStale || strings.Contains(err.Error(), "token-1") {
		t.Fatalf("Check() after expiry = %v", err)
	}
	api.token, api.expires = "token-2", clock.Add(12*time.Hour)
	restarted := &RepoCredsRefresher{Client: k8s, Reader: k8s, Provider: provider, Namespace: "argocd", Name: "aicr-registry",
		Now: func() time.Time { return clock }}
	if err := restarted.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Check(ctx); err != nil || api.calls != 2 {
		t.Fatalf("after restart: Check() = %v, %d token calls", err, api.calls)
	}
	if expiry, refresh := gauges(restarted); expiry != float64(api.expires.Unix()) || refresh != float64(clock.Unix()) {
		t.Errorf("after restart: expiry %v, refresh %v; want %d, %d", expiry, refresh, api.expires.Unix(), clock.Unix())
	}
	// A refresher that has only read the Secret knows the expiry, but no
	// refresh of its own: that is unknown, not fresh.
	checked := &RepoCredsRefresher{Client: k8s, Reader: k8s, Provider: provider, Namespace: "argocd", Name: "aicr-registry",
		Now: func() time.Time { return clock }}
	if err := checked.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if expiry, refresh := gauges(checked); expiry != float64(api.expires.Unix()) || refresh != 0 {
		t.Errorf("after a check only: expiry %v, refresh %v; want %d, 0", expiry, refresh, api.expires.Unix())
	}
}

func gauges(r *RepoCredsRefresher) (expiry, refresh float64) {
	c := r.Collectors()
	return testutil.ToFloat64(c[0]), testutil.ToFloat64(c[1])
}

// Pushing Jobs mount the administrator's static Secret as it is; with brokered
// credentials each run gets a fresh, Job-owned Secret, also when the
// operator stopped between creating the Job and resuming it.
func TestPushingJobCredentials(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	st := &stages{client: k8s, reader: k8s, config: testConfig(), creds: &registry.StaticProvider{SecretName: "registry-auth"}}
	run := &stageRun{stage: aicrjob.StageRender, input: "0123456789abcdef",
		args: func(*aicrjob.Request) ([]string, error) { return []string{"--output", "oci://r/x:t"}, nil }}
	if _, _, err := st.drive(ctx, rt, run); err != nil {
		t.Fatal(err)
	}
	job := liveJobs(t, ctx, "render")[0]
	if credentialSecretName(&job) != "registry-auth" {
		t.Fatalf("static: Job mounts %q", credentialSecretName(&job))
	}
	secrets := &corev1.SecretList{}
	if err := k8s.List(ctx, secrets, client.InNamespace(jobsNS), client.MatchingLabels{v1alpha1.LabelRunID: job.Labels[v1alpha1.LabelRunID]}); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("static: the operator wrote %d credential Secrets", len(secrets.Items))
	}

	freshRuntime(t, ctx, nil)
	rt = getRuntime(t, ctx)
	api := &fakeECR{token: "per-run-token", expires: time.Now().Add(12 * time.Hour)}
	st.creds = &registry.ECRProvider{Client: api, Host: ecrHost, Now: time.Now}
	// Simulate a crash after the Job was created, before its run objects:
	// build and create the suspended Job directly.
	req := &aicrjob.Request{Stage: aicrjob.StageRender, RunID: "0123456789ab", RuntimeName: rt.Name, RuntimeUID: string(rt.UID),
		Image: aicrImage, Namespaces: testConfig().Namespaces(), ServiceAccount: "aicr-jobs", Input: run.input, Attempt: 1,
		CredentialSecret: perRunSecretName(aicrjob.JobName(aicrjob.StageRender, "0123456789ab")), Args: []string{"--output", "oci://r/x:t"}}
	suspendedJob, err := aicrjob.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, suspendedJob); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.drive(ctx, rt, run); err != nil {
		t.Fatalf("drive() after a crash = %v", err)
	}
	if err = k8s.Get(ctx, client.ObjectKeyFromObject(suspendedJob), suspendedJob); err != nil || suspended(suspendedJob) {
		t.Fatalf("the suspended Job was not resumed: %v", err)
	}
	secret := &corev1.Secret{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: jobsNS, Name: req.CredentialSecret}, secret); err != nil {
		t.Fatalf("per-run Secret: %v", err)
	}
	if ref := secret.OwnerReferences; len(ref) != 1 || ref[0].UID != suspendedJob.UID ||
		!strings.Contains(string(secret.Data[corev1.DockerConfigJsonKey]), base64.StdEncoding.EncodeToString([]byte("AWS:per-run-token"))) {

		t.Fatalf("per-run Secret = %+v", secret.ObjectMeta)
	}
	if len(liveJobs(t, ctx, "render")) != 1 {
		t.Fatal("recovery started a second run")
	}
}

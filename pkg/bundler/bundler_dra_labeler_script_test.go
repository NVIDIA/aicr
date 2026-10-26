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

//go:build unix

package bundler

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/k8s/k8stest"
)

const (
	draLabelerScriptTimeout = 60 * time.Second
	draLabelerPollInterval  = 100 * time.Millisecond
	draLabelerKillDelay     = 5 * time.Second
)

// TestDRANodeLabelerScript_ReadyOnlyOnceLabeled runs the rendered labeler
// script against a real API server, authenticated as the DaemonSet's
// ServiceAccount and authorized only by the rendered ClusterRole and binding.
// The readiness probe's marker must not exist until the node carries the
// eviction label key.
func TestDRANodeLabelerScript_ReadyOnlyOnceLabeled(t *testing.T) {
	label := config.NodeLabel{Key: "example.com/dra-ready", Value: "enabled"}
	objs := renderDRALabelerObjects(t, label)
	env, cfg := k8stest.Start(t)
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), draLabelerScriptTimeout)
	defer cancel()
	if _, err = client.RbacV1().ClusterRoles().Create(ctx, objs.clusterRole, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ClusterRole: %v", err)
	}
	getOnly := objs.clusterRole.DeepCopy()
	getOnly.Name += "-get-only"
	for i := range getOnly.Rules {
		getOnly.Rules[i].Verbs = []string{"get"}
	}
	if _, err = client.RbacV1().ClusterRoles().Create(ctx, getOnly, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create get-only ClusterRole: %v", err)
	}

	tests := []struct {
		name   string
		labels map[string]string
		// deniedVerb, when set, withholds the rendered role's binding until
		// the API server has refused that verb on nodes, so the marker can be
		// checked while it is refused. "get" binds nothing. "patch" binds a
		// get-only role, so the script reads the node and is refused the
		// label write.
		deniedVerb string
		want       string
	}{
		{name: "labels a node without the key", want: label.Value},
		{name: "keeps another writer's value", labels: map[string]string{label.Key: "false"}, want: "false"},
		{name: "stays unready while node API access is denied", deniedVerb: "get", want: label.Value},
		{name: "stays unready while the label write is denied", deniedVerb: "patch", want: label.Value},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), draLabelerScriptTimeout)
			defer cancel()

			namespace := fmt.Sprintf("dra-labeler-%d", i)
			nodeName := fmt.Sprintf("gpu-node-%d", i)
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: tt.labels}}
			if _, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
				t.Fatalf("create Node: %v", err)
			}
			// Each case binds its own namespace's ServiceAccount, so a case
			// running unbound cannot inherit another case's grant.
			binding := objs.binding.DeepCopy()
			binding.Name = fmt.Sprintf("%s-%d", binding.Name, i)
			for j := range binding.Subjects {
				if binding.Subjects[j].Kind == rbacv1.ServiceAccountKind {
					binding.Subjects[j].Namespace = namespace
				}
			}
			bind := func(b *rbacv1.ClusterRoleBinding) {
				t.Helper()
				if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, b, metav1.CreateOptions{}); err != nil {
					t.Fatalf("create ClusterRoleBinding %s: %v", b.Name, err)
				}
			}
			switch tt.deniedVerb {
			case "":
				bind(binding)
			case "patch":
				readOnly := binding.DeepCopy()
				readOnly.Name += "-get-only"
				readOnly.RoleRef.Name = getOnly.Name
				bind(readOnly)
			}

			run := startDRALabeler(t, env, cfg, objs.daemonSet, namespace, nodeName)
			if tt.deniedVerb != "" {
				denial := fmt.Sprintf("cannot %s resource \"nodes\"", tt.deniedVerb)
				run.waitFor(t, "a denied "+tt.deniedVerb, func() bool {
					out := run.output()
					return strings.Contains(out, denial) && (strings.Contains(out, "retrying") || run.markerExists())
				})
				if run.markerExists() {
					t.Fatalf("readiness marker written while %s on nodes was denied; output:\n%s", tt.deniedVerb, run.output())
				}
				bind(binding)
			}
			run.waitFor(t, "the readiness marker", run.markerExists)

			got, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get Node: %v", err)
			}
			if v, ok := got.Labels[label.Key]; !ok || v != tt.want {
				t.Errorf("node label %s = %q (present %v) once ready, want %q; output:\n%s", label.Key, v, ok, tt.want, run.output())
			}
		})
	}
}

// draLabelerRun is the labeler container's command running on the host, with
// the readiness probe's marker redirected into a temp dir.
type draLabelerRun struct {
	marker  string
	outPath string
	exited  chan struct{}
}

// startDRALabeler runs the DaemonSet's container command as its
// ServiceAccount in namespace, on nodeName, with envtest's kubectl on PATH.
// The script never exits on its own, so cleanup kills its process group.
func startDRALabeler(t *testing.T, env *envtest.Environment, cfg *rest.Config, ds *appsv1.DaemonSet, namespace, nodeName string) *draLabelerRun {
	t.Helper()
	spec := ds.Spec.Template.Spec
	if len(spec.Containers) != 1 {
		t.Fatalf("labeler DaemonSet has %d containers, want 1", len(spec.Containers))
	}
	c := spec.Containers[0]
	if len(c.Command) == 0 {
		t.Fatal("labeler container has no command")
	}
	probe := c.ReadinessProbe
	if probe == nil || probe.Exec == nil || len(probe.Exec.Command) != 3 ||
		probe.Exec.Command[0] != "test" || probe.Exec.Command[1] != "-f" {

		t.Fatalf("labeler readiness probe = %+v, want test -f <marker>", probe)
	}
	podEnv := containerEnv(t, c, nodeName)
	if podEnv["READY"] != probe.Exec.Command[2] {
		t.Fatalf("labeler READY env = %q, want the readiness probe's marker %q", podEnv["READY"], probe.Exec.Command[2])
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "labeled")
	podEnv["READY"] = marker
	shell := k8stest.ShellEnv(t, env, cfg, envtest.User{
		Name: fmt.Sprintf("system:serviceaccount:%s:%s", namespace, spec.ServiceAccountName),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	cmd.Env = append(append(os.Environ(), shell...), "HOME="+dir)
	for name, value := range podEnv {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = draLabelerKillDelay
	outPath := filepath.Join(dir, "output")
	out, err := os.Create(outPath)
	if err != nil {
		cancel()
		t.Fatalf("create output file: %v", err)
	}
	cmd.Stdout, cmd.Stderr = out, out
	startErr := cmd.Start()
	if err = out.Close(); err != nil {
		t.Errorf("close output file: %v", err)
	}
	if startErr != nil {
		cancel()
		t.Fatalf("start labeler: %v", startErr)
	}

	run := &draLabelerRun{marker: marker, outPath: outPath, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(run.exited)
	}()
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		cancel()
		<-run.exited
		// A child forked while the first kill was in flight escapes it. The
		// shell is gone now, so nothing new can join the group.
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !stderrors.Is(err, syscall.ESRCH) {
			t.Errorf("kill labeler process group: %v", err)
		}
	})
	return run
}

// output returns what the script has written so far. The child writes
// straight to the file, so reading it needs no synchronization.
func (r *draLabelerRun) output() string {
	data, err := os.ReadFile(r.outPath)
	if err != nil {
		return fmt.Sprintf("(read labeler output: %v)", err)
	}
	return string(data)
}

func (r *draLabelerRun) markerExists() bool {
	_, err := os.Stat(r.marker)
	return err == nil
}

// waitFor polls cond until it holds. The script exiting fails the wait,
// because it is meant to sleep forever once labeled.
func (r *draLabelerRun) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	err := wait.PollUntilContextTimeout(context.Background(), draLabelerPollInterval, draLabelerScriptTimeout, true,
		func(context.Context) (bool, error) {
			if cond() {
				return true, nil
			}
			select {
			case <-r.exited:
				return false, stderrors.New("labeler exited")
			default:
				return false, nil
			}
		})
	if err != nil {
		t.Fatalf("waiting for %s: %v; labeler output:\n%s", what, err, r.output())
	}
}

// containerEnv resolves c's env the way kubelet would for a pod on nodeName.
// Only the downward-API field the labeler reads is supported.
func containerEnv(t *testing.T, c corev1.Container, nodeName string) map[string]string {
	t.Helper()
	env := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		switch {
		case e.ValueFrom == nil:
			env[e.Name] = e.Value
		case e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "spec.nodeName":
			env[e.Name] = nodeName
		default:
			t.Fatalf("labeler env %s has an unsupported source %+v", e.Name, e.ValueFrom)
		}
	}
	return env
}

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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Files a reproduction writes and renders into, relative to its directory.
const (
	RecipeFile = "recipe.json"
	CLIBundle  = "cli-bundle"
)

// The chart's in-cluster registry (charts/aicr-operator/templates/registry.yaml):
// its Service and login Secret in the operator namespace, and the local port
// a reproduction forwards to it, away from 5000, which macOS AirPlay holds.
const (
	inClusterService = "aicr-registry"
	inClusterLogin   = "aicr-registry-login"
	forwardPort      = "15000"
)

// Reproduction is what the AICR CLI needs to render a plan's bundle again:
// the recipe the operator's render read, the flags it ran with and the AICR
// image that ran it, with the bundle the operator pushed to compare against.
type Reproduction struct {
	Plan   string
	Digest string
	Image  string
	Bundle string
	Recipe []byte
	Flags  []string
}

// LoadReproduction reads a plan and its recipe. ref is a plan ConfigMap name
// or desiredStateDigest; empty is the runtime's active plan, or its candidate
// before any rollout.
func LoadReproduction(ctx context.Context, c client.Reader, namespace, ref string) (*Reproduction, error) {
	name, err := planName(ctx, c, ref)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{}
	if err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeNotFound, "read plan "+name, err)
	}
	doc, err := plan.FromConfigMap(cm)
	if err != nil {
		return nil, err
	}
	switch {
	case doc.Provenance.Source != plan.SourceResolved || doc.Recipe == nil:
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("plan %s was rebuilt from engine objects; it has no recipe to render", name))
	case len(doc.Provenance.RenderFlags) == 0 || doc.Provenance.Bundle == "" || doc.Provenance.AICRImage == "":
		return nil, operrors.New(operrors.ErrCodeInvalidRequest,
			fmt.Sprintf("plan %s does not record its render; a plan resolved by this operator version does", name))
	}
	recipeCM := &corev1.ConfigMap{}
	if err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: doc.Recipe.ConfigMapRef.ConfigMap}, recipeCM); err != nil {
		return nil, operrors.Wrap(operrors.ErrCodeNotFound, "read recipe "+doc.Recipe.ConfigMapRef.ConfigMap, err)
	}
	recipe, err := plan.ReadContent(recipeCM, plan.RecipeDataKey)
	if err != nil {
		return nil, err
	}
	if plan.DigestOf(recipe) != doc.Recipe.ConfigMapRef.Digest {
		return nil, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("recipe %s is not the recipe plan %s records", recipeCM.Name, name))
	}
	return &Reproduction{
		Plan: name, Digest: doc.DesiredStateDigest, Image: doc.Provenance.AICRImage, Bundle: doc.Provenance.Bundle,
		Recipe: recipe, Flags: doc.Provenance.RenderFlags,
	}, nil
}

func planName(ctx context.Context, c client.Reader, ref string) (string, error) {
	if ref == "" {
		rt := &v1alpha1.ClusterRuntime{}
		if err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, rt); err != nil {
			return "", operrors.Wrap(operrors.ErrCodeNotFound, "read ClusterRuntime "+v1alpha1.SingletonName, err)
		}
		if ref = rt.Status.ActivePlanDigest; ref == "" {
			ref = rt.Status.CandidatePlanDigest
		}
		if ref == "" {
			return "", operrors.New(operrors.ErrCodeNotFound, "the runtime has no plan yet")
		}
	}
	if strings.HasPrefix(ref, "sha256:") {
		return plan.Name(ref)
	}
	return ref, nil
}

// ContainerArgs are the `docker run` arguments, after the user, that render
// the bundle with the plan's AICR image from dir/recipe.json into
// dir/cli-bundle.
func (r *Reproduction) ContainerArgs(dir string) []string {
	args := []string{"-v", dir + ":/work", "-w", "/work", r.Image, "bundle", "--recipe", RecipeFile}
	args = append(args, r.Flags...)
	return append(args, "--output", CLIBundle)
}

// BundleRef is the operator's bundle as a registry reference, without the
// oci:// scheme.
func (r *Reproduction) BundleRef() string { return strings.TrimPrefix(r.Bundle, "oci://") }

// reproduce writes the plan's recipe into dir and prints the commands that
// render the same bundle with the AICR CLI and compare it with the
// operator's.
func reproduce(ctx context.Context, c client.Reader, namespace, ref, dir string, out io.Writer) error {
	r, err := LoadReproduction(ctx, c, namespace, ref)
	if err != nil {
		return err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return operrors.Wrap(operrors.ErrCodeInvalidRequest, "resolve --dir", err)
	}
	if err = os.MkdirAll(dir, 0o750); err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "create "+dir, err)
	}
	if err = os.WriteFile(filepath.Join(dir, RecipeFile), r.Recipe, 0o600); err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "write the recipe", err)
	}
	operatorBundle := shellQuote(filepath.Join(dir, "operator-bundle"))
	pull := fmt.Sprintf("Pull the operator's bundle (log in to the registry first) and compare:\n\n  oras pull %s -o %s\n",
		shellQuote(r.BundleRef()), operatorBundle)
	if port, ok := chartRegistryPort(r.BundleRef(), namespace); ok {
		_, path, _ := strings.Cut(r.BundleRef(), "/")
		ns := shellQuote(namespace)
		secret := fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath=", ns, inClusterLogin)
		pull = fmt.Sprintf(`Pull the operator's bundle from the in-cluster registry through a port-forward,
with the login the chart generated (this needs get on Secret %s and
create on pods/portforward in %s), and compare:

  kubectl -n %s port-forward service/%s %s &
  %s'{.data.password}' | base64 -d |
    oras pull --plain-http -u "$(%s'{.data.username}' | base64 -d)" --password-stdin %s -o %s
  kill %%1
`, inClusterLogin, namespace, ns, inClusterService, shellQuote(forwardPort+":"+port), secret, secret,
			shellQuote("localhost:"+forwardPort+"/"+path), operatorBundle)
	}
	_, err = fmt.Fprintf(out, `Plan %s (%s)
AICR image: %s
The operator's bundle: %s
Recipe: %s

Render the same bundle with the AICR image the operator ran:

  docker run --rm --user "$(id -u):$(id -g)" %s

%s  diff -r %s %s && echo "zero diff"
`, r.Plan, r.Digest, r.Image, r.Bundle, filepath.Join(dir, RecipeFile),
		shellJoin(r.ContainerArgs(dir)), pull, operatorBundle, shellQuote(filepath.Join(dir, CLIBundle)))
	return err
}

// chartRegistryPort returns the Service port of a registry reference whose
// host is the chart's in-cluster registry in namespace,
// aicr-registry.<namespace>.svc[.<cluster domain>]:<port>, which a client
// outside the cluster reaches only through a port-forward. Any other
// registry, a Service another registry runs included, is not the chart's.
func chartRegistryPort(ref, namespace string) (string, bool) {
	host, _, _ := strings.Cut(ref, "/")
	name, port, hasPort := strings.Cut(host, ":")
	labels := strings.Split(name, ".")
	if !hasPort || port == "" || len(labels) < 3 ||
		labels[0] != inClusterService || labels[1] != namespace || labels[2] != "svc" {

		return "", false
	}
	return port, true
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s for a POSIX shell when it needs quoting.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

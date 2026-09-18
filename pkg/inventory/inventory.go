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

package inventory

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Keys the package publishes in a structured error's context, alongside the
// release key releaseContext carries.
const (
	ctxKeyObject    = "object"
	ctxKeyNamespace = "namespace"
	ctxKeyResource  = "resource"
)

// What a canceled read was reading, as ctxErr names it.
const (
	helmSubject = "Helm release records"
	argoSubject = "Argo CD Applications"
)

// releaseSource names the reader an installedRelease came from. It is what
// makes the zero fields readable: a consumer cannot otherwise tell a chart
// whose metadata carried no annotations from an Argo CD Application, which has
// no annotations to carry. A version rule that treats a present annotation as
// authoritative needs that distinction, because the absence means a different
// thing on each side.
type releaseSource string

const (
	sourceHelm releaseSource = "helm"
	sourceArgo releaseSource = "argocd"
)

// installedRelease is one installed component as the cluster holds it: its
// chart identity, and the namespace it was installed into. A Helm release
// fills every field, from the newest revision of its storage record. An Argo
// CD Application fills only those Argo has an answer for, because a field it
// leaves zero is one the cluster genuinely does not record; Source says which
// of the two is being read.
type installedRelease struct {
	Source       releaseSource
	Name         string
	Namespace    string
	Revision     int
	Status       string
	ChartName    string
	ChartVersion string
	AppVersion   string
	Annotations  map[string]string
}

// inventoryRead is one reader's answer: what it read, and what it did not.
//
// The two counts are different facts and a report names them differently.
// Unattributed is a record that carried no name at all, so not even a guess
// can be made about it. Unreadable is a release, not a record: one that some
// component's name matched only loosely and that could not then be read. It
// may be a foreign workload drawn in by a shared token, or a real component
// whose storage is damaged, and this package cannot tell which. Summing them
// would merge "we know nothing about this" with "we could not read this".
//
// Neither counts the ordinary case of a record that names something perfectly
// well and matches no component. Those are the overwhelming majority of a real
// cluster and are dropped uncounted by design: reporting them would be a tally
// of everything else installed anywhere, which is noise rather than a finding.
//
// Records is the denominator the two are read against: every object the
// reader examined, in-scope or not. Its unit differs per reader — Helm storage
// records, of which one release contributes one per retained revision, against
// Argo Applications, of which a component has one — so the two are never
// summed.
//
// A reader returns the zero value alongside an error; the counts describe a
// completed read.
type inventoryRead struct {
	Releases     []installedRelease
	Records      int
	Unattributed int
	Unreadable   int
}

// compareInstallOrder orders two installed things by name, then by namespace.
//
// The namespace is not decoration: a release name is unique within a namespace
// and not across the cluster, so two tenants' installs of one chart differ only
// there. Both readers sort on it, and it is named rather than inlined so the
// tiebreak can be asserted directly: through a sort it cannot be, because the
// comparator is a total order over unique identities and dropping the
// tiebreak leaves an order that depends on Go map iteration.
func compareInstallOrder(nameA, namespaceA, nameB, namespaceB string) int {
	if nameA != nameB {
		return strings.Compare(nameA, nameB)
	}

	return strings.Compare(namespaceA, namespaceB)
}

// advance moves the cursor to the next page, refusing a continue token the
// server has already handed back.
//
// Without that refusal the loop is unbounded: a proxy or aggregation layer
// that echoes a stale token turns one cluster-wide read into an open request
// flood against the apiserver. Each caller's deadline is the backstop for a
// server that cycles through several tokens rather than repeating one; this
// catches the repeat immediately, before thousands of requests have been
// issued.
func advance(opts *metav1.ListOptions, next, resource string) error {
	if next == opts.Continue {
		return errors.NewWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("the apiserver returned the same continue token twice while listing %s, "+
				"so paging cannot make progress", resource),
			map[string]any{ctxKeyResource: resource})
	}
	opts.Continue = next

	return nil
}

// ctxErr reports cancellation as a structured error, naming the subject the
// caller was reading. Paging and decoding both outlive a single round trip,
// and a canceled run must return an error rather than the short list it had
// reached.
func ctxErr(ctx context.Context, subject string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeTimeout, "canceled while reading "+subject, err)
	}

	return nil
}

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

// Package inventory reads the installed component inventory from a live
// cluster and returns it as the component-to-version table an upgrade
// comparison consumes. It is the input side of `aicr upgrade-check --from
// cluster`, whose other half is the at-risk scan, an advisory pass over live
// resources that reports what an upgrade might disturb.
//
// Read is the entry point for the version table. It builds both clients from
// one resolved kubeconfig — a second authentication path could land on a
// different context, and an inventory assembled from two clusters is a
// confident wrong answer — then reads two sources, each answering for
// deployers the other cannot see, and both projected onto one shape so the
// results merge. Helm's release records: one paged List per storage driver,
// reduced to the newest revision of each release, decoded from the stored
// payload. And Argo CD's Applications: one paged List of the CRD. The
// comparison that consumes the table lands with the rest of the command.
//
// ScanAtRisk is the other half, and it answers a different question about the
// same cluster. See "The at-risk scan" below.
//
// # What it answers for
//
// Both readers list cluster-wide and are given a scope: the set of component
// names the caller is comparing. Scope is decided per record, from the name,
// before anything about that record is validated.
//
// This is what keeps a cluster's unrelated workloads out of the answer, and it
// is a correctness property rather than a performance one. Strict validation
// is owed to records that map to a component being reported on, because there
// a record that goes missing reads as a component being installed for the
// first time. It is not owed to the rest of the cluster: another team's Argo
// Application with no destination namespace, or a Helm record written in a
// storage format this build cannot read, are well-formed for their own
// purposes, and failing an upgrade check on them would be this package's bug.
// The rule is that a record which cannot affect the comparison cannot fail it
// either.
//
// A component's name is not the name the cluster stores it under. Only helm
// and helmfile install under the component's own name; Flux composes
// "<targetNamespace>-<name>", Argo CD prepends a user-settable prefix, and the
// bundle writer's injected folders append a phase. So matching is loose, and
// what a record's tier buys is not inclusion but strictness:
//
//   - Confident, a name this project itself writes. Every check is strict and
//     a malformed record fails the run, because it names a component being
//     reported on and reading it wrong is worse than not running.
//   - Possible, a name some deployer's naming could have produced from a
//     component's. A record that cannot be read is skipped and counted rather
//     than fatal, because it is as likely to be a foreign workload sharing a
//     token as a component's own, and this package cannot tell which.
//   - Out of scope, skipped before anything about it is examined.
//
// Tying strictness to the tier is what makes the loose tier safe: widening the
// match can no longer make a run fail, only draw in a foreign record that
// reads perfectly well, which the comparison can still reject.
// Under-matching has no such safety valve, since a component whose record is
// never seen reports as newly installed.
//
// Each reader accounts for itself separately in the result, and the two sets
// of counts are never summed: their units differ, since the Helm reader counts
// storage records and one release contributes one per retained revision, while
// the Argo reader counts Applications, of which a component has one. A scope
// naming no components is refused rather than answered, because an empty
// inventory is indistinguishable from a cluster with nothing installed and
// reads as every component being new.
//
// # From records to a version table
//
// Scope decides what is worth reading. Attribution decides what a record
// actually is, and it is the stricter question: the caller names the deployer,
// because the cluster does not record it and the transform is not invertible.
// A release called gpu-operator-gpu-operator is flux's gpu-operator installed
// into namespace gpu-operator, or helm's component of that literal name, and
// nothing on the record tells them apart.
//
// Each deployer's rule inverts what that deployer writes: the component's own
// name for helm and helmfile, "<namespace>-<name>" for flux, and for Argo CD a
// raw suffix — its namePrefix is user-settable and need not end in a
// separator, so it cannot be enumerated — anchored by requiring the
// Application's destination namespace to be the component's. An injected
// -pre/-post/-readiness folder folds into its parent, and a component whose
// own name ends in one of those phases claims a record of that name rather
// than ceding it to a parent that does not exist.
//
// A record matching no component is dropped, because on a real cluster that is
// almost everything installed. One carrying an AICR stamp annotation and
// matching no component is counted, and that count is the detector for a
// broken mapping: AICR wrote the record, so anything above zero means this
// package no longer recognizes its own output. It is a Helm-side signal only,
// since a generated Argo Application carries no such annotation.
//
// The version comes from the stamp when it is present and from the release's
// chart version otherwise, the latter only for a component the registry pins
// an upstream chart for (ADR-021 Decision 5, plus that guard). The guard is
// what keeps a wrapper's version from being reported as a payload's: a
// manifest-only or Kustomize component is carried by an AICR-authored chart
// whose version is AICR's. An injected folder never supplies a version at all,
// for the same reason. No version is a legitimate answer and needs no
// sentinel, because upgrade.Match already reports an unparseable side as
// VerdictUnversioned.
//
// Two exclusions are deliberate and narrow. A release is excluded only when
// its status is uninstalled, because `helm uninstall --keep-history` leaves a
// complete record and reporting it would manufacture a verdict for an upgrade
// nobody is making; failed, uninstalling and the pending-* statuses all
// describe resources that are really on the cluster, and a failed release at a
// known version is when the question matters most. And when several releases
// map to one component, the component's namespace decides which; failing to
// decide is an error rather than a pick, because guessing which tenant's
// install was meant is not this package's decision. A single unambiguous
// install answers whatever namespace it is in, since a recipe may legitimately
// deploy outside the registry default.
//
// Helm answers first and Argo fills only what it left unanswered: argocd-helm
// produces both an app-of-apps Helm release and per-component Applications,
// and the Applications are the per-component truth.
//
// # Why Argo CD needs a reader of its own
//
// Argo CD creates no Helm release. A generated Application with a helm source
// is rendered with `helm template` and its manifests applied directly, so no
// release record is ever written and the Helm reader returns nothing for a
// cluster deployed with the argocd or argocd-helm deployer. The Applications
// are the only evidence those components are installed. (argocd-helm
// delegates to the argocd deployer and emits the same Applications, so one
// reader covers both.)
//
// What Argo can be asked is narrower. It keeps no counterpart to a chart's
// annotations and no revision in Helm's sense, and its status describes sync
// and health rather than which version is installed, so those fields are left
// zero rather than approximated. A path-based Application, which is the shape
// of every generated wrapper and every Kustomize component, reports no
// version at all: the only revision it carries is the bundle repository's git
// branch, and reporting that would claim each such component sits at "main".
//
// # Why it reads Kubernetes rather than Helm
//
// Helm's CLI and SDK both read release records from the Kubernetes API. This
// package reads the same records directly, which buys three things. One
// authentication path, since the at-risk scan needs a dynamic client anyway
// and a second path through the helm binary could resolve a different context.
// One paged List per storage driver instead of the N+1 subprocesses
// `helm list` plus `helm get metadata` per release would cost. And no
// dependency on helm.sh/helm/v4, whose linked module graph adds 38
// permanently scanned modules (a WebAssembly runtime and a Postgres driver
// among them, retained because Helm's storage backend blank-imports them for
// their init side effects). That dependency was assessed and declined in #2530.
//
// The cost is coupling to a storage encoding rather than an API. Helm's
// default Secret driver versions that encoding explicitly in the Secret's Type
// field ("helm.sh/release.v1"), unchanged across the Helm 3 to 4 major
// boundary, so a record written in a format this package has not been taught
// is identifiable as such and is refused rather than guessed at.
//
// The ConfigMap driver writes no counterpart. Its records carry the same
// labels and the same encoded payload and nothing that names the format, so
// there is nothing to check before decoding: an unreadable ConfigMap record
// surfaces only as the read failing closed, on an absent or empty release key,
// or on base64, gzip, JSON or absent chart metadata. Reading the format out of
// the storage key ("sh.helm.release.v1.<name>.v<revision>") would appear to
// close that gap and is deliberately not done, here or anywhere else in the
// package: the labels
// are how these records are meant to be read, and an object name is not a
// format declaration.
//
// # The at-risk scan
//
// ScanAtRisk is advisory and asks what an upgrade might destroy rather than
// what is installed. Given the group/kind pairs the crossed transition records
// name, it resolves each through a RESTMapper and lists it cluster-wide,
// reporting every object carrying neither Helm ownership (the managed-by label
// together with a release-name annotation, because the label alone is written
// by anything) nor Argo CD's tracking id.
//
// The check is positive, so an unrecognized object is at risk by default. The
// failure it guards is an operator's own custom resources being
// cascade-deleted when a CRD is removed, and over-warning about an object some
// fourth tool owns costs a line of output while under-warning costs the
// object. A kind the cluster does not serve is skipped rather than raised: the
// CRD a record names may simply not be installed, which is the common case for
// a component the operator does not run. A discovery failure is not skipped,
// because it has not established that.
//
// Nothing it returns reaches the exit code, per ADR-021 Decision 3. AICR
// blocking an upgrade over resources it does not own is a claim it has not
// earned.
//
// # What it cannot tell you
//
// Helm records what it last applied, and an Argo Application records what it
// is configured to deploy. Both are declarations rather than observations: a
// hand-edited Deployment leaves either unchanged, and an Application that has
// never synced looks no different from one that has. This answers the version
// question authoritatively and nothing else. Live resource state is the
// at-risk scan's job, and that scan in turn reports only what carries an
// ownership marker, not whether an object is in use.
package inventory

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
	stderrors "errors"
	"fmt"
	"maps"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Helm's storage contract, shared by the drivers that keep release records in
// Kubernetes. The labels carry everything the storage key
// ("sh.helm.release.v1.<name>.v<revision>") encodes, which is why nothing here
// parses that name. helmReleaseSecretType is the format marker Helm stamps on
// the Secret, versioned as <domain>/<object>.v<n> and incremented only when
// the record's metadata changes incompatibly, so a value this build does not
// know names an encoding it cannot read rather than one it may guess at. The
// ConfigMap driver writes no counterpart, so that marker is the Secret path's
// alone; see the package doc for what catches an unreadable ConfigMap record.
const (
	helmOwnerSelector     = "owner=helm"
	helmNameLabel         = "name"
	helmVersionLabel      = "version"
	helmReleaseDataKey    = "release"
	helmReleaseSecretType = "helm.sh/release.v1" //nolint:gosec // G101 false positive: storage format marker, not a credential
)

// releaseID identifies a release. A Helm release name is unique within its
// namespace, not across the cluster, so the namespace is part of the identity:
// keying on the name alone would collapse two tenants' installs of the same
// chart into one and drop whichever carried the lower revision.
type releaseID struct {
	namespace string
	name      string
}

// storedRecord is one storage object before decoding. Revisions are compared
// on their labels alone so only the newest is ever decompressed: every record
// embeds the release's full rendered manifest, so a long history would
// otherwise cost one inflate per dead revision.
//
// The payload stays in the form its driver delivered, bytes from a Secret and
// text from a ConfigMap, because converting between them at collection time
// copies every superseded record that keepNewest is about to discard. Exactly
// one of the two fields is set.
type storedRecord struct {
	release      string
	namespace    string
	revision     int
	object       string
	confidence   confidence
	payloadBytes []byte
	payloadText  string
}

// payload returns the record in the form decodeRelease reads, converting a
// Secret's bytes here, once, for the one revision that is decoded.
func (r storedRecord) payload() string {
	if r.payloadText != "" {
		return r.payloadText
	}

	return string(r.payloadBytes)
}

// empty reports a record that carries nothing under its release key.
func (r storedRecord) empty() bool {
	return r.payloadText == "" && len(r.payloadBytes) == 0
}

// helmWalk is the state the two drivers' paged reads fold into: which
// components the read answers for, the newest record seen per release, the
// releases no revision of which can be trusted, how many storage objects were
// examined in total, and how many of them named no release at all.
type helmWalk struct {
	within       scope
	newest       map[releaseID]storedRecord
	poisoned     map[releaseID]struct{}
	records      int
	unattributed int
	unreadable   int
}

// tolerate reports whether a record that cannot be read may be skipped, and
// poisons the release it belongs to when it may.
//
// A confident record is one this project installed under a name it chose, so
// failing to read it is a component the comparison would silently omit, and
// that is worse than not running at all. A possible record was matched
// loosely: it is as likely to be a foreign workload sharing a token as a real
// component, so it is skipped rather than allowed to fail a run it may have
// nothing to do with.
//
// Skipping the record alone would be wrong, which is why the release is
// poisoned instead. Revisions of one release compete: dropping an unreadable
// revision 2 leaves a readable, superseded revision 1 to win keepNewest, and
// the release is then reported at a version it was upgraded away from. That is
// worse than omitting it, because a stale baseline is a confident wrong answer
// rather than a visible gap. Poisoning withholds the release entirely, and it
// is counted once however many of its revisions were unreadable, so the count
// is releases and not records.
func (w *helmWalk) tolerate(id releaseID, c confidence) bool {
	if c == confident {
		return false
	}
	if _, already := w.poisoned[id]; !already {
		w.poisoned[id] = struct{}{}
		w.unreadable++
	}

	return true
}

// helmReleases reads the newest revision of every in-scope Helm release in the
// cluster: one paged List loop per storage driver, then a decode of each
// release's winning revision. It returns the releases and the number of
// records it could not attribute to any release.
//
// Every failure is fatal rather than skipped, for records the read answers
// for. The inventory feeds an upgrade comparison, where a release that goes
// missing reads as a component being installed for the first time. Records
// outside the scope are a different matter: the cluster is full of releases
// this project knows nothing about, and one of them being malformed is not a
// reason to fail. See the scope type.
//
// The two drivers are read in sequence rather than through an errgroup. They
// are independent reads and would fan out cleanly, but keepNewest breaks a
// revision tie in favor of whichever record arrived first, and under a
// scheduler that answer stops being reproducible.
func helmReleases(ctx context.Context, client kubernetes.Interface, within scope) (inventoryRead, error) {
	if err := within.validate(); err != nil {
		return inventoryRead{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.HelmInventoryTimeout)
	defer cancel()

	walk := &helmWalk{
		within:   within,
		newest:   make(map[releaseID]storedRecord),
		poisoned: make(map[releaseID]struct{}),
	}

	if err := collectSecretRecords(ctx, client, walk); err != nil {
		return inventoryRead{}, err
	}
	if err := collectConfigMapRecords(ctx, client, walk); err != nil {
		return inventoryRead{}, err
	}

	releases, err := decodeNewest(ctx, walk)
	if err != nil {
		return inventoryRead{}, err
	}

	return inventoryRead{
		Releases:     releases,
		Records:      walk.records,
		Unattributed: walk.unattributed,
		Unreadable:   walk.unreadable,
	}, nil
}

// attribute reports the release a storage object belongs to, and whether it is
// one this read answers for.
//
// It runs before every strict check, which is the point: the format marker,
// the revision label and the payload are all validated only for records that
// can reach the comparison. A record that names no release is counted rather
// than dropped, so an exclusion is shown rather than silent.
func (w *helmWalk) attribute(labels map[string]string) (string, confidence) {
	w.records++
	release := labels[helmNameLabel]
	if release == "" {
		w.unattributed++

		return "", outOfScope
	}

	return release, w.within.covers(release)
}

// collectSecretRecords reads the records written by Helm's default driver.
//
// The List selects on the owner label rather than on the Secret type, even
// though a type field selector would push the filter server-side: a record
// written in a format this build has not been taught would then simply not be
// returned, and the release would vanish from the inventory instead of
// failing the run.
func collectSecretRecords(ctx context.Context, client kubernetes.Interface, walk *helmWalk) error {
	opts := listOptions()
	for {
		if err := ctxErr(ctx, helmSubject); err != nil {
			return err
		}
		secrets, err := client.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return listError(err, "secrets")
		}

		for i := range secrets.Items {
			secret := &secrets.Items[i]
			release, conf := walk.attribute(secret.Labels)
			if conf == outOfScope {
				continue
			}
			if secret.Type != helmReleaseSecretType {
				if walk.tolerate(releaseID{namespace: secret.Namespace, name: release}, conf) {
					continue
				}

				return errors.NewWithContext(errors.ErrCodeInternal,
					fmt.Sprintf("Secret %q in namespace %q is owned by Helm but stores its release in format %q, "+
						"which this build cannot read; it understands %q",
						secret.Name, secret.Namespace, secret.Type, helmReleaseSecretType),
					storageContext(secret.Name, secret.Namespace))
			}
			record, err := storedRecordFrom(release, secret.Name, secret.Namespace, secret.Labels)
			if err != nil {
				if walk.tolerate(releaseID{namespace: secret.Namespace, name: release}, conf) {
					continue
				}

				return err
			}
			record.confidence = conf
			record.payloadBytes = secret.Data[helmReleaseDataKey]
			keepNewest(walk.newest, record)
		}

		if secrets.Continue == "" {
			return nil
		}
		if err := advance(&opts, secrets.Continue, "secrets"); err != nil {
			return err
		}
	}
}

// collectConfigMapRecords reads the records written under HELM_DRIVER=configmap,
// which carry the same labels and the same encoded payload. Both drivers are
// read on every run because which one a cluster uses is a client-side setting
// that leaves no trace to query.
func collectConfigMapRecords(ctx context.Context, client kubernetes.Interface, walk *helmWalk) error {
	opts := listOptions()
	for {
		if err := ctxErr(ctx, helmSubject); err != nil {
			return err
		}
		configMaps, err := client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return listError(err, "configmaps")
		}

		for i := range configMaps.Items {
			configMap := &configMaps.Items[i]
			release, conf := walk.attribute(configMap.Labels)
			if conf == outOfScope {
				continue
			}
			record, err := storedRecordFrom(release, configMap.Name, configMap.Namespace, configMap.Labels)
			if err != nil {
				if walk.tolerate(releaseID{namespace: configMap.Namespace, name: release}, conf) {
					continue
				}

				return err
			}
			record.confidence = conf
			record.payloadText = configMap.Data[helmReleaseDataKey]
			keepNewest(walk.newest, record)
		}

		if configMaps.Continue == "" {
			return nil
		}
		if err := advance(&opts, configMaps.Continue, "configmaps"); err != nil {
			return err
		}
	}
}

// listOptions is the first page's request. Both drivers page because the
// selector matches every retained revision of every release, and keepNewest
// reduces each page as it arrives, so the peak held at once is one page plus
// the winners rather than the whole history.
//
// The page loop is the one that can run long, so that is where cancellation is
// checked; a page is bounded work over data already in hand.
func listOptions() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: helmOwnerSelector,
		Limit:         defaults.HelmReleaseListPageSize,
	}
}

// storedRecordFrom reads the revision off a storage object's labels. The
// release is already known, because attribute reads it first to decide scope,
// and the caller stamps the tier that decision produced.
// A malformed revision on an in-scope record is fatal: it would otherwise cost
// the release its place in the comparison, where an absent release reads as a
// component being installed for the first time.
func storedRecordFrom(release, object, namespace string, labels map[string]string) (storedRecord, error) {
	revision, err := strconv.Atoi(labels[helmVersionLabel])
	if err != nil {
		errCtx := storageContext(object, namespace)
		maps.Copy(errCtx, releaseContext(release))

		return storedRecord{}, errors.WrapWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("the storage record %q for Helm release %q in namespace %q carries a %q label of %q, "+
				"which is not a revision number", object, release, namespace, helmVersionLabel,
				labels[helmVersionLabel]),
			err, errCtx)
	}

	return storedRecord{
		release:   release,
		namespace: namespace,
		revision:  revision,
		object:    object,
	}, nil
}

// keepNewest retains the highest revision seen for a release. Ties keep the
// incumbent, which makes the secret driver win over a configmap record of the
// same revision; the two encode the same release, so either answer is right.
func keepNewest(newest map[releaseID]storedRecord, record storedRecord) {
	id := releaseID{namespace: record.namespace, name: record.release}
	if existing, ok := newest[id]; ok && existing.revision >= record.revision {
		return
	}
	newest[id] = record
}

// decodeNewest decodes the selected revisions, sorted by release name so a
// caller's output and its golden files do not depend on List order. The
// namespace breaks ties, since one release name can appear in several.
func decodeNewest(ctx context.Context, walk *helmWalk) ([]installedRelease, error) {
	newest := walk.newest
	ids := make([]releaseID, 0, len(newest))
	for id := range newest {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return compareInstallOrder(ids[i].name, ids[i].namespace, ids[j].name, ids[j].namespace) < 0
	})

	var releases []installedRelease
	for _, id := range ids {
		if err := ctxErr(ctx, helmSubject); err != nil {
			return nil, err
		}
		// A release poisoned while its records were collected is withheld
		// whatever survived into newest, since what survived is by definition
		// not its newest revision.
		if _, bad := walk.poisoned[id]; bad {
			continue
		}
		record := newest[id]
		if record.empty() {
			if walk.tolerate(id, record.confidence) {
				continue
			}
			errCtx := storageContext(record.object, record.namespace)
			maps.Copy(errCtx, releaseContext(record.release))

			return nil, errors.NewWithContext(errors.ErrCodeInternal,
				fmt.Sprintf("the storage record %q for Helm release %q in namespace %q carries nothing under "+
					"its %q key", record.object, record.release, record.namespace, helmReleaseDataKey),
				errCtx)
		}

		release, err := decodeRelease(record.release, record.payload())
		if err != nil {
			if walk.tolerate(id, record.confidence) {
				continue
			}

			return nil, err
		}

		releases = append(releases, installedRelease{
			Source:       sourceHelm,
			Name:         record.release,
			Namespace:    record.namespace,
			Revision:     record.revision,
			Status:       release.Info.Status,
			ChartName:    release.Chart.Metadata.Name,
			ChartVersion: release.Chart.Metadata.Version,
			AppVersion:   release.Chart.Metadata.AppVersion,
			Annotations:  release.Chart.Metadata.Annotations,
		})
	}

	return releases, nil
}

// releaseContext builds the structured-error context for a release, which
// carries the identifying name and nothing drawn from the payload.
func releaseContext(name string) map[string]any {
	return map[string]any{"release": name}
}

// storageContext is the error context for one storage object, identified by
// the object rather than by the release, which a malformed record may not name.
func storageContext(object, namespace string) map[string]any {
	return map[string]any{ctxKeyObject: object, ctxKeyNamespace: namespace}
}

// listError classifies a failed List. An RBAC denial names the permission to
// grant, because the operator hitting it is the one who can fix it.
//
// Cancellation wins wherever it occurs, so it is tested first: a caller who
// canceled or ran out of deadline needs to see that, not a classification of
// whatever the cancellation happened to interrupt. The Lists are where the
// wall clock goes, which makes this the timeout an operator meets most often.
func listError(err error, resource string) error {
	if stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded) {
		return errors.WrapWithContext(errors.ErrCodeTimeout,
			fmt.Sprintf("canceled while listing %s for Helm release records", resource),
			err, map[string]any{ctxKeyResource: resource})
	}

	// A continue token outlives the apiserver's watch-cache window on a large
	// cluster, which is transient and an operator's cue to re-run rather than
	// to file a bug. k8s.io/client-go/tools/pager answers this by restarting
	// the list, and is deliberately not used here: the restart stitches pages
	// from before and after into one result, so the inventory would describe
	// two different clusters. An incomplete inventory produces a confident
	// wrong upgrade verdict, so a consistent failure beats an inconsistent
	// success.
	if apierrors.IsResourceExpired(err) {
		return errors.WrapWithContext(errors.ErrCodeUnavailable,
			fmt.Sprintf("the paged list of %s outlived the apiserver's window for it, so the Helm release "+
				"records were read only in part; re-run", resource),
			err, map[string]any{ctxKeyResource: resource})
	}

	if apierrors.IsForbidden(err) {
		return errors.WrapWithContext(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot list %s across all namespaces, so the installed Helm releases cannot be read; "+
				"grant 'list %s' at cluster scope and re-run", resource, resource),
			err, map[string]any{ctxKeyResource: resource})
	}

	return errors.WrapWithContext(errors.ErrCodeInternal,
		fmt.Sprintf("failed to list %s holding Helm release records", resource),
		err, map[string]any{ctxKeyResource: resource})
}

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

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"golang.org/x/sync/errgroup"
	authv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels used when rendering a permission failure for an operator. The
// caller label names the kubeconfig identity running aicr; the
// ServiceAccount label is filled in with the subject's
// "system:serviceaccount:<ns>:<name>" username.
const (
	callerSubjectLabel = "the caller (your kubeconfig identity)"

	// serviceAccountUserPrefix is the username the apiserver authenticates
	// a ServiceAccount as, and therefore the SubjectAccessReview subject
	// that answers for the agent pod rather than for the caller.
	serviceAccountUserPrefix = "system:serviceaccount:"

	// Virtual groups every ServiceAccount is a member of. RBAC bindings
	// routinely target them instead of an individual ServiceAccount, so a
	// SubjectAccessReview that omits them under-reports the subject's
	// real permissions and would fail a correctly-provisioned operator.
	groupServiceAccounts       = "system:serviceaccounts"
	groupServiceAccountsPrefix = "system:serviceaccounts:"
	groupAuthenticated         = "system:authenticated"
)

// Sizing hints for the two slices this file builds. Neither is a bound.
const (
	// rbacKindCount is the number of RBAC kinds a prefix-mode run creates
	// and later deletes: ServiceAccount, Role, RoleBinding, ClusterRole,
	// ClusterRoleBinding.
	rbacKindCount = 5

	// rulesPerPolicyHint is the average number of (group, resource, verb)
	// triples one PolicyRule expands into across namespacedRules and
	// clusterRules.
	rulesPerPolicyHint = 4
)

// accessCheck is one authorization question: may `subject` perform `verb` on
// `group/resource[/subresource]` at `namespace` (empty means cluster scope)?
//
// subject is "" for the caller — answered with a SelfSubjectAccessReview —
// or a "system:serviceaccount:<ns>:<name>" username for the agent's
// ServiceAccount, answered with a SubjectAccessReview. The struct is
// deliberately all-comparable so dedupeChecks can key a map on it.
type accessCheck struct {
	group       string
	resource    string
	subresource string
	verb        string
	namespace   string
	subject     string
}

// permissionCheck is one answered accessCheck, returned to callers so they
// can render the full pre-flight rather than only its failures.
type permissionCheck struct {
	Group       string
	Resource    string
	Subresource string
	Verb        string
	Namespace   string

	// Subject is "" for the caller, or the ServiceAccount's
	// "system:serviceaccount:<ns>:<name>" username.
	Subject string

	Allowed bool
	Reason  string

	// Unverified marks a ServiceAccount check the apiserver refused to
	// answer because the caller may not create a SubjectAccessReview.
	// Allowed is false on such an entry, but it is NOT counted as a
	// missing permission: nothing was learned either way. CheckPermissions
	// says so out loud instead of silently dropping the check.
	Unverified bool
}

// CheckPermissions is the authoritative pre-flight gate for an entire agent
// run. It verifies every permission the run will actually exercise, for both
// identities involved — the caller (the kubeconfig identity running aicr)
// and the ServiceAccount the agent pod runs as — and fails before anything
// is written to the cluster when any of them is missing.
//
// # Ordering, and why it is still fail-before-mutate
//
// The required verb set depends on which ServiceAccount mode this run is in
// (see resolveServiceAccount), and the mode cannot be known without reading
// ServiceAccounts. The gate therefore runs in two phases:
//
//  1. Check the caller permissions every run needs in either mode,
//     `serviceaccounts: get` among them.
//  2. Resolve the ServiceAccount (a read-only Get), then check the
//     mode-specific set.
//
// Every step up to the point the gate closes is a read: a
// Self/SubjectAccessReview is a non-persisted authorization query, and the
// resolution Get creates nothing. The fail-before-mutate guarantee is about
// not WRITING before validation, and no write is issued until Deploy's
// ensure* chain, which runs only after this returns nil.
//
// # Mode-specific verbs
//
// Prefix mode (aicr creates its own run-scoped ServiceAccount) needs create
// AND delete on all five RBAC kinds: the deferred Cleanup is registered
// before Deploy and always runs, so an identity that can create but not
// delete would pass a green pre-flight and then leak a full run-scoped RBAC
// set — cluster-scoped objects included — on every run.
//
// Exact-ServiceAccount mode creates and deletes no RBAC at all, so demanding
// those verbs would block operators who legitimately hold none. It instead
// verifies that the operator actually provisioned the ServiceAccount, which
// aicr does not do for them.
//
// # Reporting
//
// Every check is evaluated before any failure is reported, so an operator
// fixing permissions gets the complete list in one run. Each failure names
// the verb, the resource, the scope, and which subject lacked it.
func (d *Deployer) CheckPermissions(ctx context.Context) ([]permissionCheck, error) {
	// Phase 1: the caller-side set every run needs regardless of mode.
	results, err := d.runAccessChecks(ctx, dedupeChecks(d.callerCommonChecks()))
	if err != nil {
		return nil, err
	}

	// `serviceaccounts: get` gates the rest of the pre-flight rather than
	// merely joining it. Without it the mode is unknowable, and the
	// historical behavior — treat an unreadable ServiceAccount as a
	// prefix — silently ran an operator who named their IRSA / Workload
	// Identity ServiceAccount under a fresh, un-annotated one instead.
	// Fail here with everything phase 1 found, and say why the rest was
	// not evaluated.
	if !callerMayReadServiceAccounts(results) {
		return results, missingPermissionsError(results, unresolvableModeHint(d.config.Namespace))
	}

	// Read-only: decides which verb set phase 2 demands, and which
	// ServiceAccount the agent pod will run as.
	if err = d.resolveServiceAccount(ctx); err != nil {
		return results, err
	}

	modeResults, err := d.runAccessChecks(ctx, dedupeChecks(d.modeSpecificChecks()))
	if err != nil {
		return nil, err
	}
	results = append(results, modeResults...)

	// The meta-permission is handled out loud, never silently: a caller who
	// cannot create a SubjectAccessReview learns that the ServiceAccount's
	// own permissions went unverified and that the agent will surface any
	// gap in-pod instead.
	d.warnUnverified(results)

	if hasMissing(results) {
		return results, missingPermissionsError(results, d.remediationHints(results))
	}
	return results, nil
}

// callerCommonChecks returns the permissions the caller needs in either
// ServiceAccount mode: everything Deploy, WaitForCompletion, GetSnapshot and
// Cleanup issue that is not RBAC creation or deletion.
//
// Deliberately absent: Namespace create/get/patch. ensureNamespace's three
// branches need a different verb depending on whether the namespace already
// exists and is already labeled, and the documented path — an operator's
// pre-existing, previously-labeled namespace such as `gpu-operator` —
// needs none of them. Demanding the cluster-scoped `namespaces: create`
// that only the fresh-namespace branch uses would fail the majority case.
//
// Also absent: `get` on Role, RoleBinding, ClusterRole and ClusterRoleBinding.
// Cleanup reads those only to re-establish ownership of an entry whose
// Create response was lost (resolveIntentUID), and that path already fails
// closed with a warning rather than a blind delete, so a missing `get`
// degrades to a logged orphan instead of a wrong deletion. `serviceaccounts:
// get` is required anyway, for the mode resolution above.
func (d *Deployer) callerCommonChecks() []accessCheck {
	ns := d.config.Namespace
	checks := []accessCheck{
		// Mode resolution (resolveServiceAccount) and Cleanup's
		// ownership re-check (resolveIntentUID).
		{resource: resourceServiceAccounts, verb: verbGet, namespace: ns},

		// ensureJob, waitForJobCompletion (Get + Watch, List on watch
		// resume) and Cleanup's UID-pinned delete.
		{group: batchAPIGroup, resource: resourceJobs, verb: verbCreate, namespace: ns},
		{group: batchAPIGroup, resource: resourceJobs, verb: verbGet, namespace: ns},
		{group: batchAPIGroup, resource: resourceJobs, verb: verbList, namespace: ns},
		{group: batchAPIGroup, resource: resourceJobs, verb: verbWatch, namespace: ns},
		{group: batchAPIGroup, resource: resourceJobs, verb: verbDelete, namespace: ns},

		// Pod discovery (findPodName / findOrWatchPodName), readiness
		// waiting, and log streaming back to the operator's terminal.
		{resource: resourcePods, verb: verbGet, namespace: ns},
		{resource: resourcePods, verb: verbList, namespace: ns},
		{resource: resourcePods, verb: verbWatch, namespace: ns},
		{resource: resourcePods, subresource: subresourceLog, verb: verbGet, namespace: ns},

		// Reading the snapshot the agent staged.
		{resource: resourceCM, verb: verbGet, namespace: ns},
		{resource: resourceCM, verb: verbList, namespace: ns},
	}

	// A caller-supplied `cm://<other-namespace>/<name>` Output is read
	// where it points, not in the agent's namespace.
	if outputNS, _, parseErr := pod.ParseConfigMapURI(d.config.Output); parseErr == nil && outputNS != "" {
		checks = append(checks, accessCheck{resource: resourceCM, verb: verbGet, namespace: outputNS})
	}

	// `configmaps: delete` is required only when this Deployer owns the
	// output ConfigMap. Cleanup's staging-ConfigMap sweep and
	// getSnapshotFromConfigMap's created-set record are both gated on
	// Config.OwnsOutputConfigMap, so a caller who supplied their own
	// `cm://` output URI never has a ConfigMap deleted on their behalf.
	// This gate fails closed, so demanding an unconditional delete grant
	// would block deployment for identities perfectly capable of the run
	// they actually asked for.
	if d.config.OwnsOutputConfigMap {
		checks = append(checks, accessCheck{resource: resourceCM, verb: verbDelete, namespace: ns})
	}
	return checks
}

// modeSpecificChecks returns the half of the gate that depends on which
// ServiceAccount mode resolveServiceAccount settled on.
//
// Prefix mode: the caller creates and later deletes the full run-scoped RBAC
// set, so both verbs are required on all five kinds.
//
// Exact-ServiceAccount mode: aicr creates and deletes nothing, so none of
// those verbs is required of the caller. What IS required is that the
// operator already granted the agent's rules to the ServiceAccount they
// named — `aicr snapshot --add-roles-to-service-account` renders those
// manifests but applies nothing, so "you never applied them" is the
// failure this catches, at the gate rather than minutes later inside a pod.
func (d *Deployer) modeSpecificChecks() []accessCheck {
	if !d.managesRBAC() {
		return d.serviceAccountChecks(serviceAccountUsername(d.config.Namespace, d.existingServiceAccount()))
	}

	ns := d.config.Namespace
	verbs := []string{verbCreate, verbDelete}
	checks := make([]accessCheck, 0, len(verbs)*rbacKindCount)
	for _, verb := range verbs {
		checks = append(checks,
			accessCheck{resource: resourceServiceAccounts, verb: verb, namespace: ns},
			accessCheck{group: rbacAPIGroup, resource: resourceRoles, verb: verb, namespace: ns},
			accessCheck{group: rbacAPIGroup, resource: resourceRoleBindings, verb: verb, namespace: ns},
			accessCheck{group: rbacAPIGroup, resource: resourceClusterRoles, verb: verb},
			accessCheck{group: rbacAPIGroup, resource: resourceClusterRoleBindings, verb: verb},
		)
	}
	return checks
}

// serviceAccountChecks returns the questions asked of the agent's own
// ServiceAccount: exactly the rules ensureRole and ensureClusterRole grant
// in prefix mode, expanded to one check per (group, resource, verb).
//
// Deriving them from namespacedRules and clusterRules — the same two
// definitions the run-scoped Role and ClusterRole are built from, and the
// same ones BuildServiceAccountRoleManifests renders — is what keeps the
// gate from drifting away from what the agent actually needs.
//
// Issued only in exact-ServiceAccount mode. In prefix mode the
// ServiceAccount does not exist yet and its RBAC has not been created, so
// every answer would be a truthful "denied" about an identity that is about
// to be granted those very rules.
func (d *Deployer) serviceAccountChecks(subject string) []accessCheck {
	checks := checksFromRules(namespacedRules(), d.config.Namespace, subject)
	return append(checks, checksFromRules(clusterRules(d.config.DiscoverNetwork), "", subject)...)
}

// checksFromRules expands PolicyRules into one accessCheck per
// (apiGroup, resource, verb) triple at the given scope. A "resource/sub"
// entry such as "pods/exec" is split so the review carries Subresource,
// which is how the apiserver evaluates it.
func checksFromRules(rules []rbacv1.PolicyRule, namespace, subject string) []accessCheck {
	// Capacity is a hint, not a bound: most rules name one API group and a
	// handful of verbs, so this lands close without a counting pass.
	checks := make([]accessCheck, 0, len(rules)*rulesPerPolicyHint)
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, res := range rule.Resources {
				resource, subresource, _ := strings.Cut(res, "/")
				for _, verb := range rule.Verbs {
					checks = append(checks, accessCheck{
						group:       group,
						resource:    resource,
						subresource: subresource,
						verb:        verb,
						namespace:   namespace,
						subject:     subject,
					})
				}
			}
		}
	}
	return checks
}

// dedupeChecks drops exact duplicates while preserving first-seen order, so
// the same question is never asked of the apiserver twice and never reported
// twice. Near-duplicates that differ only in scope (namespaced `pods: list`
// vs cluster-wide `pods: list`) are deliberately kept: an operator who
// applied the Role but not the ClusterRole fails exactly one of them.
func dedupeChecks(checks []accessCheck) []accessCheck {
	seen := make(map[accessCheck]struct{}, len(checks))
	out := make([]accessCheck, 0, len(checks))
	for _, c := range checks {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// runAccessChecks answers every check and returns the results in input
// order. An access review is a read-only query, so they fan out
// concurrently: N sequential reviews cost N round trips, one batch costs
// one. Concurrency is bounded because the expanded rule set can reach
// several dozen checks on a --discover-network run and there is no reason
// to open that many connections at once.
//
// A ServiceAccount check the apiserver declines to answer is recorded as
// Unverified rather than failing the run; see CheckPermissions.
func (d *Deployer) runAccessChecks(ctx context.Context, checks []accessCheck) ([]permissionCheck, error) {
	results := make([]permissionCheck, len(checks))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(defaults.K8sAccessReviewConcurrency)
	for i := range checks {
		check := checks[i]
		g.Go(func() error {
			allowed, reason, reviewErr := d.reviewAccess(gctx, check)
			if reviewErr != nil {
				if check.subject != "" && isUnanswerable(reviewErr) {
					results[i] = check.result(false, reviewErr.Error())
					results[i].Unverified = true
					return nil
				}
				code := errors.ErrCodeInternal
				if errors.IsNetworkError(reviewErr) {
					code = errors.ErrCodeUnavailable
				}
				return errors.Wrap(code,
					fmt.Sprintf("failed to check whether %s may %q %s (%s)",
						check.subjectLabel(), check.verb, check.resourceLabel(), check.scopeLabel()),
					reviewErr)
			}
			results[i] = check.result(allowed, reason)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// reviewAccess asks the apiserver one authorization question.
//
// A caller check uses SelfSubjectAccessReview, which answers only for the
// identity in the kubeconfig. A ServiceAccount check must use
// SubjectAccessReview with that ServiceAccount as the subject — the agent
// pod runs as it, and a SelfSubjectAccessReview cannot answer for anyone but
// the caller.
//
// The apiserver error is returned unwrapped so the caller can classify it
// (Forbidden / not served / network) before deciding whether it is fatal.
func (d *Deployer) reviewAccess(ctx context.Context, check accessCheck) (bool, string, error) {
	attrs := &authv1.ResourceAttributes{
		Verb:        check.verb,
		Group:       check.group,
		Resource:    check.resource,
		Subresource: check.subresource,
		Namespace:   check.namespace,
	}

	if check.subject == "" {
		review := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs},
		}
		result, err := d.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return false, "", err
		}
		return result.Status.Allowed, result.Status.Reason, nil
	}

	review := &authv1.SubjectAccessReview{
		Spec: authv1.SubjectAccessReviewSpec{
			ResourceAttributes: attrs,
			User:               check.subject,
			Groups:             serviceAccountGroups(d.config.Namespace),
		},
	}
	result, err := d.clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	return result.Status.Allowed, result.Status.Reason, nil
}

// isUnanswerable reports whether err means the apiserver would not answer a
// SubjectAccessReview at all, as opposed to answering "denied".
//
// Creating a SubjectAccessReview is itself a privilege, and some clusters do
// not serve the endpoint. Neither says anything about the ServiceAccount's
// permissions, so neither may be read as a pass OR as a failure — the run
// continues with the gap reported (see warnUnverified).
func isUnanswerable(err error) bool {
	return apierrors.IsForbidden(err) ||
		apierrors.IsUnauthorized(err) ||
		apierrors.IsNotFound(err) ||
		apierrors.IsMethodNotSupported(err)
}

// serviceAccountUsername returns the username the apiserver authenticates a
// ServiceAccount as, which is the SubjectAccessReview subject that answers
// for the agent pod.
func serviceAccountUsername(namespace, name string) string {
	return serviceAccountUserPrefix + namespace + ":" + name
}

// serviceAccountGroups returns the virtual groups every ServiceAccount in
// namespace belongs to, so a SubjectAccessReview reflects grants made to
// those groups rather than only to the individual subject.
func serviceAccountGroups(namespace string) []string {
	return []string{groupServiceAccounts, groupServiceAccountsPrefix + namespace, groupAuthenticated}
}

// result converts an answered check into the reportable form.
func (c accessCheck) result(allowed bool, reason string) permissionCheck {
	return permissionCheck{
		Group:       c.group,
		Resource:    c.resource,
		Subresource: c.subresource,
		Verb:        c.verb,
		Namespace:   c.namespace,
		Subject:     c.subject,
		Allowed:     allowed,
		Reason:      reason,
	}
}

// subjectLabel names whose permissions this check answers for.
func (c accessCheck) subjectLabel() string {
	return subjectLabel(c.subject)
}

// resourceLabel renders "<resource>[/<subresource>][.<group>]", the spelling
// an operator would use in a PolicyRule.
func (c accessCheck) resourceLabel() string {
	return resourceLabel(c.resource, c.subresource, c.group)
}

// scopeLabel renders the scope the check was evaluated at.
func (c accessCheck) scopeLabel() string {
	return scopeLabel(c.namespace)
}

func subjectLabel(subject string) string {
	if subject == "" {
		return callerSubjectLabel
	}
	return fmt.Sprintf("agent ServiceAccount %q", subject)
}

func resourceLabel(resource, subresource, group string) string {
	name := resource
	if subresource != "" {
		name += "/" + subresource
	}
	if group != "" {
		name += "." + group
	}
	return name
}

func scopeLabel(namespace string) string {
	if namespace == "" {
		return "cluster-scoped"
	}
	return fmt.Sprintf("namespace %q", namespace)
}

// callerMayReadServiceAccounts reports whether the caller's
// `serviceaccounts: get` check came back allowed. It is the one check that
// gates the rest of the pre-flight rather than merely joining it: without it
// the ServiceAccount mode is unknowable.
func callerMayReadServiceAccounts(results []permissionCheck) bool {
	for _, r := range results {
		if r.Subject == "" && r.Resource == resourceServiceAccounts && r.Verb == verbGet && r.Allowed {
			return true
		}
	}
	return false
}

// hasMissing reports whether any check came back denied. An Unverified entry
// does not count: nothing was learned about it either way.
func hasMissing(results []permissionCheck) bool {
	for _, r := range results {
		if !r.Allowed && !r.Unverified {
			return true
		}
	}
	return false
}

// missingPermissionsError renders every denied check as one actionable line
// — subject, verb, resource, scope, and the authorizer's reason when it gave
// one — followed by hint. Reporting all of them together is the point: an
// operator fixing permissions should need one run, not one run per verb.
func missingPermissionsError(results []permissionCheck, hint string) error {
	var missing []string
	for _, r := range results {
		if r.Allowed || r.Unverified {
			continue
		}
		line := fmt.Sprintf("%s cannot %q %s (%s)",
			subjectLabel(r.Subject), r.Verb,
			resourceLabel(r.Resource, r.Subresource, r.Group), scopeLabel(r.Namespace))
		if r.Reason != "" {
			line += ": " + r.Reason
		}
		missing = append(missing, line)
	}
	msg := fmt.Sprintf("missing required permissions:\n  - %s", strings.Join(missing, "\n  - "))
	if hint != "" {
		msg += "\n\n" + hint
	}
	return errors.New(errors.ErrCodeUnauthorized, msg)
}

// unresolvableModeHint explains why the pre-flight stopped early when the
// caller cannot read ServiceAccounts, and why that is not something aicr
// works around.
func unresolvableModeHint(namespace string) string {
	return fmt.Sprintf(
		"Reading ServiceAccounts in namespace %q is required before anything else: it is what\n"+
			"decides whether --service-account-name names an existing ServiceAccount to run as\n"+
			"verbatim, or is a prefix for one this run creates. Without it, a ServiceAccount you\n"+
			"named explicitly would be silently replaced by a generated one carrying none of its\n"+
			"cloud credentials (IRSA / Workload Identity), so the run stops instead.\n"+
			"The remaining, mode-specific permissions were not evaluated; fix the above and re-run.",
		namespace)
}

// remediationHints returns the operator-facing next step for whichever kind
// of failure occurred, so the message can be acted on without reading the
// source.
func (d *Deployer) remediationHints(results []permissionCheck) string {
	var callerMissing, subjectMissing bool
	for _, r := range results {
		if r.Allowed || r.Unverified {
			continue
		}
		if r.Subject == "" {
			callerMissing = true
		} else {
			subjectMissing = true
		}
	}

	var hints []string
	if subjectMissing {
		hints = append(hints, fmt.Sprintf(
			"The agent pod runs as ServiceAccount %q, and aicr manages no permissions for a\n"+
				"ServiceAccount it did not create. Generate the RBAC that grants it what the agent\n"+
				"needs, review it, and apply it yourself:\n"+
				"  aicr snapshot --add-roles-to-service-account %s --namespace %s\n"+
				"  kubectl apply -f snapshot-rbac-<run-id>/",
			d.existingServiceAccount(), d.existingServiceAccount(), d.config.Namespace))
	}
	if callerMissing && d.managesRBAC() {
		hints = append(hints, ""+
			"This run creates and deletes its own run-scoped RBAC, which is why both create and\n"+
			"delete are required: cleanup always runs, and an identity that can create but not\n"+
			"delete leaks a ServiceAccount, Role, RoleBinding, ClusterRole and ClusterRoleBinding\n"+
			"on every run. To need none of those verbs, pre-create a ServiceAccount and pass its\n"+
			"exact name with --service-account-name.")
	}
	return strings.Join(hints, "\n\n")
}

// warnUnverified reports, once, that the agent ServiceAccount's own
// permissions could not be checked.
//
// Creating a SubjectAccessReview is itself a privilege. A caller who lacks
// it learns nothing about the ServiceAccount, and silently dropping the
// check would be the same defect class the `serviceaccounts: get` gate above
// closes — a missing answer read as a passing one. The run continues,
// because the agent still fails visibly inside the pod, but the operator is
// told the gate did not cover it and why.
func (d *Deployer) warnUnverified(results []permissionCheck) {
	var count int
	var reason string
	for _, r := range results {
		if !r.Unverified {
			continue
		}
		count++
		if reason == "" {
			reason = r.Reason
		}
	}
	if count == 0 {
		return
	}
	slog.Warn("could not verify the agent ServiceAccount's own permissions; continuing, but a missing rule will surface as an in-pod failure minutes from now instead of here",
		slog.String(attrServiceAccount, d.existingServiceAccount()),
		slog.String(attrNamespace, d.config.Namespace),
		slog.String(attrRunID, d.config.RunID),
		slog.Int("uncheckedRules", count),
		slog.String("cause", reason),
		slog.String("remedy", "grant the caller 'create subjectaccessreviews.authorization.k8s.io', or verify by hand with: kubectl auth can-i --list --as "+serviceAccountUsername(d.config.Namespace, d.existingServiceAccount())))
}

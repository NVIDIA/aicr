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

// Package redact minimizes the sensitive operational detail an evidence
// bundle physically ships, while leaving the cryptographic verification
// story intact.
//
// The signed predicate commits to artifacts by hash and carries the derived
// fingerprint / criteria-match / per-phase counts — that is the conformance
// signal. The snapshot and CTRF payloads are the *backing content* those
// digests point at, not the signal itself, so they can be shrunk without
// weakening the binding.
//
// Two transforms are applied by the minimal policy:
//
//   - Snapshot: a fail-closed allowlist that is enforced at every level —
//     measurement type, subtype, AND data key. Only enumerated types,
//     subtypes, and keys survive; a new type, subtype, or key a future
//     collector adds is dropped until explicitly allowlisted (there is no
//     keep-all subtype). Node names, provider instance IDs, the raw node
//     label/taint set, kernel/sysctl tuning, loaded modules, and systemd
//     service config are dropped.
//   - CTRF: per-test Stdout and Message (free-form log text that can leak IPs,
//     DNS names, secret/cert names, internal URLs) are omitted; the pass/fail
//     signal (name, status, duration, suite, summary counts) is preserved. The
//     structured per-test Extra map is rebuilt against a fail-closed key AND
//     value allowlist (ctrfExtraAllowlist): only enumerated low-cardinality
//     keys survive, and each surviving value must additionally pass its key's
//     validator (non-negative decimal count, or a closed set of known skip
//     codes) — so an identifier smuggled under an allowed key (e.g. an IP in
//     nodesTotal, a hostname or unminted code in skipReason) is dropped, not
//     published. This is
//     the publication boundary: emission-side validation alone is bypassable
//     via raw prefixed stdout. A signed bundle can still distinguish a
//     1-of-2-node pass from 2-of-2 and preserve a skip reason without shipping
//     free-form log text. An Extra map that retains nothing is dropped rather
//     than shipped as an empty object.
//
// Both functions are pure and non-mutating: they build fresh structures and
// never alter their inputs, so the full (unredacted) artifacts remain
// available for the --full emit path and for computing the predicate
// fingerprint from the raw snapshot.
package redact

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

const (
	// PolicyName identifies the redaction policy recorded in the predicate.
	PolicyName = "minimal"

	// PolicyVersion is the allowlist/scrub-rule version. Bump on any change
	// to what survives redaction so verifiers can tell which rules ran.
	//
	// v2 added the per-test CTRF Extra allowlist (ctrfExtraAllowlist):
	// allowlisted structured keys whose values match the key's canonical shape
	// (count / enum code) now survive minimal redaction.
	// v3 (#2297): the per-test Extra allowlist admits the NCCL runtime-
	// provenance key runtimeSource (closed set), and the new bounded
	// TestResult.RuntimeProvenance carrier survives under the rules in
	// boundRuntimeProvenance. Nothing previously published changed shape;
	// verifiers on v2 will see fields they do not expect, which is exactly why
	// the version moves.
	//
	// v4 added the `named-in-skip-checks` skipReason code, so a check the
	// CALLER withheld (--skip-check) reaches the bundle with its reason and not
	// only its name; the message that used to carry it is blanked here.
	PolicyVersion = "v4"
)

// headerMetadataAllowlist is the fail-closed set of snapshot header metadata
// keys safe to publish. The collecting node's name (`source-node`) and any key
// a future writer adds are dropped unless listed here.
var headerMetadataAllowlist = map[string]struct{}{
	"timestamp": {},
	"version":   {},
}

// subtypePolicy is the allowlist of data keys that survive within a kept
// subtype. Every kept subtype is key-constrained — there is no keep-all
// escape hatch — so a key a future collector attaches to an allowlisted
// subtype is dropped by default until added here (fail-closed at key level).
type subtypePolicy struct {
	keys map[string]struct{}
}

func keep(keys ...string) subtypePolicy {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return subtypePolicy{keys: set}
}

// snapshotAllowlist is the fail-closed allowlist: only the measurement types,
// subtypes, and data keys listed here survive. A type not present is dropped
// entirely; a subtype not present for a kept type is dropped entirely; and a
// data key not present in its subtype's policy is dropped. Every subtype is
// key-constrained (no keep-all), so the guarantee holds at the key level too.
//
// The subtype names and data keys mirror the literals the collectors author
// (pkg/collector/{k8s,os,gpu,topology}) — the same names pkg/fingerprint keys
// off. They are not shared constants, so a collector that renames a subtype or
// key must update this table too; otherwise the renamed field is silently
// dropped from the minimal snapshot (fail-closed, but lossy). Bump PolicyVersion
// when the allowlist changes.
var snapshotAllowlist = map[measurement.Type]map[string]subtypePolicy{
	measurement.TypeK8s: {
		"server": keep("version", "platform", "goVersion"),
		"node": keep(
			"provider",
			"kubelet-version",
			"kernel-version",
			"operating-system",
			"os-image",
			"container-runtime-name",
			"container-runtime-version",
		), // drops source-node, provider-id, container-runtime-id
	},
	measurement.TypeGPU: {
		"hardware": keep(
			"gpu-present",
			"gpu-count",
			"driver-loaded",
			"detection-source",
			"model",
		),
	},
	measurement.TypeOS: {
		// /etc/os-release distro identity. Key-constrained because the
		// collector ships the whole file verbatim, so a non-standard distro
		// could inject arbitrary (operator-influenced) keys otherwise.
		"release": keep(
			"ID",
			"ID_LIKE",
			"NAME",
			"PRETTY_NAME",
			"VERSION",
			"VERSION_ID",
			"VERSION_CODENAME",
		),
		// grub, sysctl, kmod intentionally absent → dropped (tuning/hardening posture)
	},
	measurement.TypeNodeTopology: {
		"summary": keep("node-count", "taint-count", "label-count"), // counts, not identifiers
		// label, taint intentionally absent → dropped (node names + custom labels)
	},
	// TypeSystemD intentionally absent → entire measurement dropped.
}

// snapshotAppliedRules is the static, sorted description of what the minimal
// policy removes from a snapshot. Static (rather than input-derived) so the
// recorded redaction provenance stays byte-stable across runs.
var snapshotAppliedRules = []string{
	"snapshot.header.allowlist",
	"snapshot.measurements.allowlist",
}

// ctrfExtraValidator reports whether v is a safe published value for its key.
// It is the value half of the fail-closed contract: an allowlisted key alone is
// not enough — the value must also match the key's canonical shape, so a value
// that structurally looks like an identifier (IP, hostname, FQDN, free text)
// never survives under an allowed key.
type ctrfExtraValidator func(v string) bool

// maxCountDigits bounds a published count value. Five digits (up to 99,999)
// comfortably covers realistic node/GPU counts while keeping the attacker-
// chosen numeric channel narrow — the premise is that only LOW-cardinality
// counts cross the publication boundary, and EmitExtra itself does no value
// validation, so this allowlist regex is the defense.
const maxCountDigits = 5

// ctrfCountValue matches a bare non-negative decimal count — no sign, dot, or
// separator — so an IP or instance id smuggled into a count key (e.g.
// "10.0.0.5") fails closed. The digit bound (maxCountDigits) caps the value.
var ctrfCountValue = regexp.MustCompile(`^[0-9]{1,` + strconv.Itoa(maxCountDigits) + `}$`)

func isCountValue(v string) bool { return ctrfCountValue.MatchString(v) }

// ctrfSkipReasons is the CLOSED set of skipReason codes safe to publish. A
// regex on kebab-case shape is not enough — it would still pass an arbitrary
// low-cardinality identifier like "customer-prod-cluster". Only codes minted by
// a check (see validators/deployment/nvidia_smi.go's skipReason* constants) are
// listed, plus the one pkg/validator mints for a caller-declared skip
// (skipCheckReasonCode); any other value, including a well-formed but unlisted code, is dropped
// fail-closed. A new skip code must be added here in the same change that emits
// it — same discipline as the key allowlist.
var ctrfSkipReasons = map[string]struct{}{
	"no-gpu-nodes":             {}, // cluster has no GPU nodes at all
	"no-schedulable-gpu-nodes": {}, // GPU nodes exist but all cordoned/unschedulable
	"nodes-busy":               {}, // schedulable GPU nodes exist but are busy with workloads
	"named-in-skip-checks":     {}, // the CALLER withheld the check (--skip-check / spec.validate.execution.skipChecks)
}

func isSkipReason(v string) bool { _, ok := ctrfSkipReasons[v]; return ok }

// ctrfRuntimeSources is the CLOSED set of NCCL benchmark runtime-provenance
// codes (validators/performance, #2297). The value names WHERE the measured
// runtime came from — not whether the bandwidth passed — so a reader can tell a
// number that describes the artifact the recipe ships from one that describes
// a validator fixture. As with skip reasons, only codes the check mints are
// listed; a new code must be added here in the same change that emits it.
var ctrfRuntimeSources = map[string]struct{}{
	"delivered-artifact":      {}, // derived from the ClusterTrainingRuntime the recipe ships
	"recipe-supplied-runtime": {}, // nccl-benchmark-runtime(-ref): the recipe supplied the runtime itself
	"cluster-capability":      {}, // the validator's embedded fixture: proves the fabric, not the shipped artifact
}

func isRuntimeSource(v string) bool { _, ok := ctrfRuntimeSources[v]; return ok }

// ctrfExtraAllowlist is the fail-closed set of TestResult.Extra keys safe to
// publish in a minimal (default) evidence bundle, each paired with the
// validator its value must pass. Every key carries only low-cardinality counts
// or a closed-set enum code — never node names, IPs, or other operator-
// identifying text — because unlike Stdout/Message these values are NOT stripped
// by default. A key a future check adds is dropped until listed here; a value
// that fails its validator is dropped even under an allowed key. Keep the map
// keys mirrored in the ctrf godoc and docs/contributor/validator.md.
var ctrfExtraAllowlist = map[string]ctrfExtraValidator{
	"nodesValidated": isCountValue, // count of nodes a coverage check actually verified
	"nodesTotal":     isCountValue, // count of candidate nodes (validated + skipped/cordoned)
	"skipReason":     isSkipReason, // closed-set code for why a check skipped
	// NCCL benchmark runtime provenance (#2297): which artifact the bandwidth
	// number describes. The template digests and path diff that back the claim
	// are stdout (--full) evidence, not Extra — see validators/performance.
	"runtimeSource": isRuntimeSource, // closed-set code: delivered-artifact | recipe-supplied-runtime | cluster-capability
}

// ctrfSHA256Value matches a bare lowercase sha256 hex digest and nothing else.
var ctrfSHA256Value = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ctrfProvenancePath bounds a RuntimeProvenance path: the dotted template-key
// grammar (map keys, [name]-addressed list elements, label/annotation keys with
// their "/" and "-"), and nothing that could carry free text or an address.
var ctrfProvenancePath = regexp.MustCompile(`^[A-Za-z0-9._/*\-\[\]]{1,256}$`)

// ctrfListSelector matches a named-list selector segment ("[name]") in a path.
var ctrfListSelector = regexp.MustCompile(`\[([^\[\]]*)\]`)

// ctrfFabricEnvNames is the EXACT set of env names an env selector may keep:
// the GPUDirect-TCPXO NCCL configuration the shipped torch-distributed-tcpxo
// runtime declares (Google's v1.0.15 plugin set) plus the two loader/device
// variables — the variables the inherited inventory exists to prove were
// carried. A prefix rule (NCCL_*) was rejected because an operator can name a
// variable NCCL_CUSTOMER_ACME_PROD; only names on this list are vendor-defined.
// Every other list element name (containers, volumes, mounts, other env) is
// operator- or vendor-chosen text and collapses to "[*]". Adding a variable to
// the shipped runtime that the inventory should name means adding it here in
// the same change.
var ctrfFabricEnvNames = map[string]struct{}{
	"CUDA_VISIBLE_DEVICES":                          {},
	"LD_LIBRARY_PATH":                               {},
	"NCCL_BUFFSIZE":                                 {},
	"NCCL_CROSS_NIC":                                {},
	"NCCL_DEBUG":                                    {},
	"NCCL_DEBUG_SUBSYS":                             {},
	"NCCL_FASTRAK_CTRL_DEV":                         {},
	"NCCL_FASTRAK_ENABLE_CONTROL_CHANNEL":           {},
	"NCCL_FASTRAK_ENABLE_HOTPATH_LOGGING":           {},
	"NCCL_FASTRAK_IFNAME":                           {},
	"NCCL_FASTRAK_LLCM_DEVICE_DIRECTORY":            {},
	"NCCL_FASTRAK_NUM_FLOWS":                        {},
	"NCCL_FASTRAK_PLUGIN_ACCEPT_TIMEOUT_MS":         {},
	"NCCL_FASTRAK_USE_LLCM":                         {},
	"NCCL_FASTRAK_USE_SNAP":                         {},
	"NCCL_MIN_NCHANNELS":                            {},
	"NCCL_NET_GDR_LEVEL":                            {},
	"NCCL_NVLS_ENABLE":                              {},
	"NCCL_NVLSTREE_MAX_CHUNKSIZE":                   {},
	"NCCL_P2P_NET_CHUNKSIZE":                        {},
	"NCCL_P2P_NVL_CHUNKSIZE":                        {},
	"NCCL_P2P_PCI_CHUNKSIZE":                        {},
	"NCCL_PROTO":                                    {},
	"NCCL_SHIMNET_GUEST_CONFIG_CHECKER_CONFIG_FILE": {},
	"NCCL_SOCKET_IFNAME":                            {},
	"NCCL_TUNER_CONFIG_PATH":                        {},
	"NCCL_TUNER_PLUGIN":                             {},
}

// ctrfProvenanceMaxPaths caps each path list; a derived PodTemplateSpec has a
// few hundred leaves, so a longer list is not a template inventory.
const ctrfProvenanceMaxPaths = 1024

// ctrfFreeKeyMaps are the PodTemplateSpec field names whose value is a map
// with USER-DEFINED keys (map[string]string / map[string]Quantity in the
// core/v1 API): labels, annotations, nodeSelector, resource limits/requests
// and overhead (extended-resource names such as acme.internal/x), label
// selectors' matchLabels, CSI volumeAttributes and flexVolume options. Every
// other segment of a template path is a Kubernetes API field name. A key under
// one of these maps is operator text wherever the map sits in the template —
// a sidecar's resources as much as the pod's labels — so it is collapsed to
// the map unless the whole key is in ctrfVendorKeys. The maps are leaves in
// the API (their values are scalars), so the key is always the final segment.
var ctrfFreeKeyMaps = []string{
	"annotations", "labels", "nodeSelector", "limits", "requests", "overhead",
	"matchLabels", "volumeAttributes", "options", "userAnnotations",
}

// ctrfPodTemplateFields is the EXACT set of JSON field names reachable from a
// core/v1 PodTemplateSpec (k8s.io/api v0.37.0), generated by reflection over
// the API types and pinned by TestPodTemplateFieldsMatchAPI. Every structural
// segment of a provenance path must be one of these — a path is dropped
// otherwise — so the carrier can only ever describe the Kubernetes schema:
// an operator cannot smuggle a name through a segment position even if a
// non-schema key survived CRD pruning or the sentinel line were forged. Keys of
// free-key maps and [name] selectors are governed by the exact allowlists
// above, not by this set.
var ctrfPodTemplateFields = setOf(
	"accessModes", "action", "activeDeadlineSeconds", "add", "affinity", "allowPrivilegeEscalation",
	"annotations", "apiGroup", "apiVersion", "appArmorProfile", "args", "audience", "automountServiceAccountToken",
	"awsElasticBlockStore", "azureDisk", "azureFile", "bindMountOptions", "blockOwnerDeletion",
	"cachingMode", "capabilities", "cephfs", "certificateChainPath", "chapAuthDiscovery",
	"chapAuthSession", "cinder", "claimName", "claims", "clusterTrustBundle", "command",
	"conditionType", "configMap", "configMapKeyRef", "configMapRef", "containerName", "containerPort",
	"containers", "controller", "creationTimestamp", "credentialBundlePath", "csi", "dataSource",
	"dataSourceRef", "datasetName", "datasetUUID", "defaultMode", "defaultUser", "deletionGracePeriodSeconds",
	"deletionTimestamp", "devicePath", "directory", "diskName", "diskURI", "divisor", "dnsConfig",
	"dnsPolicy", "downwardAPI", "driver", "drop", "effect", "emptyDir", "enableServiceLinks",
	"endpoints", "env", "envFrom", "ephemeral", "ephemeralContainers", "evictionResponders",
	"exec", "exitCodes", "expirationSeconds", "failureThreshold", "fc", "fieldPath", "fieldRef",
	"fieldsType", "fieldsV1", "fileKeyRef", "finalizers", "flexVolume", "flocker", "fsGroup",
	"fsGroupChangePolicy", "fsType", "gateway", "gcePersistentDisk", "generateName", "generation",
	"gitRepo", "glusterfs", "gmsaCredentialSpec", "gmsaCredentialSpecName", "group", "grpc",
	"host", "hostAliases", "hostIP", "hostIPC", "hostNetwork", "hostPID", "hostPath", "hostPort",
	"hostProcess", "hostUsers", "hostname", "hostnameOverride", "hostnames", "httpGet",
	"httpHeaders", "image", "imagePullPolicy", "imagePullSecrets", "initContainers", "initialDelaySeconds",
	"initiatorName", "ip", "iqn", "iscsi", "iscsiInterface", "items", "key", "keyPath",
	"keyType", "keyring", "kind", "labelSelector", "labels", "level", "lifecycle", "limits",
	"livenessProbe", "localhostProfile", "lun", "managedFields", "manager", "matchExpressions",
	"matchFields", "matchLabelKeys", "matchLabels", "maxExpirationSeconds", "maxSkew",
	"medium", "metadata", "minDomains", "mismatchLabelKeys", "mode", "monitors", "mountPath",
	"mountPropagation", "name", "nameservers", "namespace", "namespaceSelector", "namespaces",
	"nfs", "nodeAffinity", "nodeAffinityPolicy", "nodeName", "nodePublishSecretRef", "nodeSelector",
	"nodeSelectorTerms", "nodeTaintsPolicy", "operation", "operator", "optional", "options",
	"os", "overhead", "ownerReferences", "partition", "path", "pdID", "pdName", "periodSeconds",
	"persistentVolumeClaim", "photonPersistentDisk", "podAffinity", "podAffinityTerm",
	"podAntiAffinity", "podCertificate", "podGroupName", "pool", "port", "portals", "ports",
	"portworxVolume", "postStart", "preStop", "preemptionPolicy", "preference", "preferredDuringSchedulingIgnoredDuringExecution",
	"prefix", "priority", "priorityClassName", "privileged", "procMount", "projected",
	"protectionDomain", "protocol", "pullPolicy", "quobyte", "rbd", "readOnly", "readOnlyRootFilesystem",
	"readinessGates", "readinessProbe", "recursiveReadOnly", "reference", "registry", "repository",
	"request", "requests", "requiredDuringSchedulingIgnoredDuringExecution", "resizePolicy",
	"resource", "resourceClaimName", "resourceClaimTemplateName", "resourceClaims", "resourceFieldRef",
	"resourceName", "resourceVersion", "resources", "restartPolicy", "restartPolicyRules",
	"revision", "role", "runAsGroup", "runAsNonRoot", "runAsUser", "runAsUserName", "runtimeClassName",
	"scaleIO", "schedulerName", "schedulingGates", "schedulingGroup", "scheme", "seLinuxChangePolicy",
	"seLinuxOptions", "searches", "seccompProfile", "seconds", "secret", "secretFile",
	"secretKeyRef", "secretName", "secretRef", "securityContext", "selector", "selfLink",
	"server", "service", "serviceAccount", "serviceAccountName", "serviceAccountToken",
	"setHostnameAsFQDN", "shareName", "shareProcessNamespace", "signerName", "sizeLimit",
	"sleep", "sources", "spec", "sslEnabled", "startupProbe", "stdin", "stdinOnce", "stopSignal",
	"storageClassName", "storageMode", "storagePolicyID", "storagePolicyName", "storagePool",
	"storageos", "subPath", "subPathExpr", "subdomain", "subresource", "successThreshold",
	"supplementalGroups", "supplementalGroupsPolicy", "sysctls", "system", "targetContainerName",
	"targetPortal", "targetWWNs", "tcpSocket", "tenant", "terminationGracePeriodSeconds",
	"terminationMessagePath", "terminationMessagePolicy", "time", "timeoutSeconds", "tolerationSeconds",
	"tolerations", "topologyKey", "topologySpreadConstraints", "tty", "type", "uid", "user",
	"userAnnotations", "value", "valueFrom", "values", "volume", "volumeAttributes", "volumeAttributesClassName",
	"volumeClaimTemplate", "volumeDevices", "volumeID", "volumeMode", "volumeMounts", "volumeName",
	"volumeNamespace", "volumePath", "volumes", "vsphereVolume", "weight", "whenUnsatisfiable",
	"windowsOptions", "workingDir", "wwids",
)

func setOf(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

// ctrfVendorKeys is the EXACT set of full label/annotation/nodeSelector keys
// that are vendor-defined API surface, not operator text, and stay in minimal
// evidence: the two GKE fabric annotations this carrier exists to prove were
// inherited, the NRI device annotation, the GKE accelerator selector, the
// Trainer ancestry label, and the two scheduling keys AICR itself stamps. A
// domain rule was rejected because the local part is operator-writable
// (networking.gke.io/customer-prod is a legal key); only whole keys on this
// list survive, every other key collapses to its map.
var ctrfVendorKeys = map[string]struct{}{
	"networking.gke.io/interfaces":                {},
	"networking.gke.io/default-interface":         {},
	"devices.gke.io/container.tcpxo-daemon":       {},
	"cloud.google.com/gke-accelerator":            {},
	"trainer.kubeflow.org/trainjob-ancestor-step": {},
	"nvidia.com/gpu":                              {}, // the worker's GPU request — the count the measurement ran on
	"nvidia.com/gpu.present":                      {},
	"node.kubernetes.io/instance-type":            {},
}

// boundRuntimeProvenance applies the minimal-evidence policy to a derived
// runtime's provenance record: both digests must be lowercase sha256 hex or
// the whole record is dropped (fail-closed — a record that cannot bind is not
// evidence); each path must match the template-key grammar; named-list
// selectors collapse to "[*]" unless they name a variable in the exact fabric
// env set; keys under operator-keyed maps collapse to the parent unless the
// whole key is in the exact vendor-key set; lists are deduplicated, sorted and
// capped. The live runtime is operator-modifiable, so any name it carries is
// treated as operator text unless an exact allowlist keeps it — there is no
// prefix or domain rule anywhere in this policy. Returns a fresh record; never
// mutates in.
func boundRuntimeProvenance(in *ctrf.RuntimeProvenance) *ctrf.RuntimeProvenance {
	if in == nil || !ctrfSHA256Value.MatchString(in.ShippedDigest) || !ctrfSHA256Value.MatchString(in.DerivedDigest) {
		return nil
	}
	return &ctrf.RuntimeProvenance{
		ShippedDigest:   in.ShippedDigest,
		DerivedDigest:   in.DerivedDigest,
		OverriddenPaths: boundProvenancePaths(in.OverriddenPaths),
		InheritedPaths:  boundProvenancePaths(in.InheritedPaths),
	}
}

func boundProvenancePaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if !ctrfProvenancePath.MatchString(p) {
			continue
		}
		p = collapseOperatorKey(collapseListSelectors(p))
		if !structuralSegmentsKnown(p) {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	if len(out) > ctrfProvenanceMaxPaths {
		out = out[:ctrfProvenanceMaxPaths]
	}
	return out
}

// collapseOperatorKey returns p unchanged unless it addresses a key under a
// free-key map (ctrfFreeKeyMaps) anywhere in the template, in which case the
// whole key must be in ctrfVendorKeys; otherwise the path collapses to the map
// itself. Keys may contain "." (annotation domains), so the map is located by
// field-name segment, not by splitting the key.
func collapseOperatorKey(p string) string {
	best := -1
	for _, field := range ctrfFreeKeyMaps {
		for _, marker := range []string{"." + field + ".", field + "."} {
			idx := strings.Index(p, marker)
			if idx < 0 || (marker[0] != '.' && idx != 0) {
				continue
			}
			end := idx + len(marker) // first byte of the key
			if best < 0 || end < best {
				best = end
			}
		}
	}
	if best < 0 {
		return p
	}
	if _, vendor := ctrfVendorKeys[p[best:]]; vendor {
		return p
	}
	return p[:best-1]
}

// structuralSegmentsKnown reports whether every structural segment of an
// already-collapsed path is a PodTemplateSpec field name. Segments are the
// dot-separated tokens up to and including the first free-key map field; what
// follows that field is the map key, which collapseOperatorKey has already
// reduced to an exact vendor key (and which may itself contain dots). A
// "[...]" selector suffix is not part of the field name.
func structuralSegmentsKnown(p string) bool {
	rest := p
	for rest != "" {
		seg, tail, _ := strings.Cut(rest, ".")
		if i := strings.IndexByte(seg, '['); i >= 0 {
			seg = seg[:i]
		}
		if _, ok := ctrfPodTemplateFields[seg]; !ok {
			return false
		}
		if slices.Contains(ctrfFreeKeyMaps, seg) {
			return true // the remainder is the (already vetted) map key
		}
		rest = tail
	}
	return true
}

// collapseListSelectors rewrites every "[name]" selector in p to "[*]" except
// an env selector naming a variable in ctrfFabricEnvNames, which is kept
// because it is the evidence. Container, volume, mount and arbitrary env names
// are whatever the live runtime carries and are not published.
func collapseListSelectors(p string) string {
	return ctrfListSelector.ReplaceAllStringFunc(p, func(sel string) string {
		name := sel[1 : len(sel)-1]
		start := strings.Index(p, sel)
		if _, fabric := ctrfFabricEnvNames[name]; fabric && start >= 0 && strings.HasSuffix(p[:start], "env") {
			return sel
		}
		return "[*]"
	})
}

// ctrfAppliedRules is the static, sorted description of the CTRF scrub.
var ctrfAppliedRules = []string{
	"ctrf.tests.extra.allowlist",
	"ctrf.tests.omit:message",
	"ctrf.tests.omit:stdout",
	"ctrf.tests.runtimeProvenance.bound",
}

// Snapshot returns a redacted deep copy of in and the sorted list of applied
// rule identifiers. It never mutates in. Returns (nil, nil) when in is nil.
func Snapshot(in *snapshotter.Snapshot) (*snapshotter.Snapshot, []string) {
	if in == nil {
		return nil, nil
	}

	out := &snapshotter.Snapshot{
		Header: redactHeader(in.Header),
		// The advisory fingerprint is kept as-is: the same structured
		// fingerprint is already computed and signed into the predicate, so
		// retaining it here is not new disclosure. It is an immutable value,
		// safe to share with the input.
		Fingerprint: in.Fingerprint,
	}

	for _, m := range in.Measurements {
		if rm := redactMeasurement(m); rm != nil {
			out.Measurements = append(out.Measurements, rm)
		}
	}

	return out, append([]string(nil), snapshotAppliedRules...)
}

// redactMeasurement returns an allowlisted copy of m, or nil if the whole
// measurement is dropped (unlisted type, or no subtype survives).
func redactMeasurement(m *measurement.Measurement) *measurement.Measurement {
	if m == nil {
		return nil
	}
	subPolicies, ok := snapshotAllowlist[m.Type]
	if !ok {
		return nil
	}
	out := &measurement.Measurement{Type: m.Type}
	for i := range m.Subtypes {
		st := &m.Subtypes[i]
		pol, ok := subPolicies[st.Name]
		if !ok {
			continue
		}
		cp := copySubtype(st, pol)
		if len(cp.Data) == 0 {
			// A key-constrained subtype that retained nothing is dropped
			// rather than shipped as an empty `data: {}` — that would be a
			// fail-open hole and would also fail measurement.Subtype.Validate
			// for any downstream consumer that re-reads the snapshot.
			continue
		}
		out.Subtypes = append(out.Subtypes, cp)
	}
	if len(out.Subtypes) == 0 {
		return nil
	}
	return out
}

// copySubtype copies st, retaining only the data keys in pol — every other
// key (including any a future collector adds to this subtype) is dropped,
// fail-closed at the key level. Reading values are immutable wrappers, so
// sharing them is safe. The subtype's Context is intentionally NOT carried
// over: it is not allowlisted and carries no conformance signal, so passing
// it through would be a fail-open path as collectors start attaching context.
func copySubtype(st *measurement.Subtype, pol subtypePolicy) measurement.Subtype {
	data := make(map[string]measurement.Reading, len(pol.keys)) // upper bound on survivors
	for k, v := range st.Data {
		if _, allowed := pol.keys[k]; allowed {
			data[k] = v
		}
	}
	return measurement.Subtype{Name: st.Name, Data: data}
}

func redactHeader(h header.Header) header.Header {
	out := header.Header{Kind: h.Kind, APIVersion: h.APIVersion}
	md := make(map[string]string, len(headerMetadataAllowlist))
	for k, v := range h.Metadata {
		if _, ok := headerMetadataAllowlist[k]; ok {
			md[k] = v
		}
	}
	if len(md) > 0 {
		out.Metadata = md
	}
	return out
}

// CTRF returns a redacted deep copy of in with per-test Stdout and Message
// omitted and each per-test Extra map rebuilt against ctrfExtraAllowlist, plus
// the sorted list of applied rule identifiers. It never mutates in. Returns
// (nil, nil) when in is nil.
func CTRF(in *ctrf.Report) (*ctrf.Report, []string) {
	if in == nil {
		return nil, nil
	}

	// out := *in copies every field by value, including the Results struct
	// (Tool, Summary, and the shared Environment pointer — none sensitive).
	// Only Results.Tests is rebuilt below so the input is never mutated.
	out := *in

	if in.Results.Tests != nil {
		tests := make([]ctrf.TestResult, len(in.Results.Tests))
		for i, tr := range in.Results.Tests {
			tr.Stdout = nil
			tr.Message = ""
			tr.Extra = allowlistExtra(tr.Extra)
			tr.RuntimeProvenance = boundRuntimeProvenance(tr.RuntimeProvenance)
			tests[i] = tr
		}
		out.Results.Tests = tests
	}

	return &out, append([]string(nil), ctrfAppliedRules...)
}

// allowlistExtra returns a fresh map containing only the ctrfExtraAllowlist
// entries of in whose value also passes the key's validator — every other key
// (including any a future check adds) and every ill-shaped value (an identifier
// smuggled under an allowed key) is dropped fail-closed. Returns nil when in is
// empty or nothing survives, so no empty `extra: {}` ships. It never mutates in.
func allowlistExtra(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for k, v := range in {
		validate, ok := ctrfExtraAllowlist[k]
		if !ok || !validate(v) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(ctrfExtraAllowlist))
		}
		out[k] = v
	}
	return out
}

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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
)

// ncclRuntimeSource says where the benchmark runtime came from. It is the
// provenance half of a result (#2297): bandwidth still decides pass/fail, but
// the number means something different depending on the artifact it measured.
// The codes are the closed set pkg/evidence/redact publishes under the
// runtimeSource Extra key; adding one here requires adding it there.
type ncclRuntimeSource string

const (
	// runtimeSourceCapability is the validator's embedded fixture: the runtime
	// was built for the test and deleted. It proves the fabric can reach the
	// floor; it says nothing about what the recipe ships.
	runtimeSourceCapability ncclRuntimeSource = "cluster-capability"
	// runtimeSourceRecipeSupplied is nccl-benchmark-runtime(-ref): the recipe
	// supplied the runtime itself and owns its wiring.
	runtimeSourceRecipeSupplied ncclRuntimeSource = "recipe-supplied-runtime"
	// runtimeSourceDelivered is a benchmark runtime DERIVED from the
	// ClusterTrainingRuntime the recipe ships, so the number attests to the
	// delivered wiring.
	runtimeSourceDelivered ncclRuntimeSource = "delivered-artifact"

	extraKeyRuntimeSource = "runtimeSource"
)

// runsGKETCPXOChecks reports whether the GKE preflight and the per-worker
// transport watcher apply. Both the capability fixture and a derived runtime
// are TCPXO-wired by AICR and must be checked; a recipe-supplied runtime owns
// its own fabric and is left alone (the existing contract for #1792).
func (s ncclRuntimeSource) runsGKETCPXOChecks() bool { return s != runtimeSourceRecipeSupplied }

// benchmarkRuntimePlan is the outcome of resolveBenchmarkRuntimeSource: the
// carrier fed to the existing custom-runtime plumbing (empty for the embedded
// fixture), the provenance class, and — for a derived runtime — the shipped
// object the final runtime is derived from at apply time, plus the provenance
// record that makes the claim auditable.
//
// For a delivered artifact the carrier is a gating/sizing stand-in only: it
// lets every customRuntime != "" branch (fabric injection, scheduling defaults,
// the node-selector sizing lookup) treat the derived runtime like a
// recipe-supplied one. The object that is actually applied is re-derived in
// buildNCCLRuntimeObject from the skeleton rendered with real templateData, so
// placeholder types survive (a quoted "${GPU_COUNT}" stays a string, a bare
// ${GPU_COUNT_PER_NODE} stays a number) instead of round-tripping through a
// serialized carrier that re-parses "16" as an integer.
type benchmarkRuntimePlan struct {
	carrier    string
	source     ncclRuntimeSource
	shipped    *unstructured.Unstructured // deployed ClusterTrainingRuntime; nil unless delivered
	provenance *derivedRuntimeProvenance
}

// derived reports whether the plan re-derives the applied runtime from a
// shipped object. Nil-safe so test callers that exercise the baked-in path can
// pass no plan.
func (p *benchmarkRuntimePlan) derived() bool { return p != nil && p.shipped != nil }

// derivedRuntimeProvenance is the audit record for a derived runtime, computed
// against the object that was actually applied (after scheduling was stamped)
// so it describes what ran, not an intermediate. It is published two ways: the
// human-readable listing on stdout (--full evidence), and the bounded
// ctrf.RuntimeProvenance carrier via EmitRuntimeProvenance, which survives
// minimal redaction — digests and template KEYS only, operator-keyed map keys
// collapsed by pkg/evidence/redact — so a default attestation still binds the
// number to the exact templates compared (#2297's evidence carrier).
type derivedRuntimeProvenance struct {
	shippedDigest  string
	derivedDigest  string
	overridePaths  []string // paths at which applied != shipped (owned overrides + scheduling)
	inheritedPaths []string // shipped leaf paths carried into the applied runtime unchanged
}

// resolveBenchmarkRuntimeSource decides which of the three runtime sources this
// run uses and, for a delivered artifact, performs the recipe -> deployed ->
// cluster verification and the derivation. It runs BEFORE any cluster mutation.
//
// Precedence: a recipe-supplied runtime and a delivered fabric runtime are
// mutually exclusive — the recipe would be claiming two different runtimes
// own the same benchmark — mirroring the existing runtime/profile exclusivity.
// The delivered predicate is recipe-derived (gkenet.FabricRuntimeDelivered) and
// never inferred from a live object, so a stray runtime cannot change who owns
// the evidence.
func resolveBenchmarkRuntimeSource(ctx *validators.Context, customRuntime string, profiled bool,
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant,
	fabric ncclFabricType) (*benchmarkRuntimePlan, error) {

	var refs []recipe.ComponentRef
	if ctx.ValidationInput != nil {
		refs = ctx.ValidationInput.ComponentRefs
	}
	recorded, delivered, err := gkenet.FabricRuntimeDelivered(refs)
	if err != nil {
		return nil, err
	}
	if customRuntime != "" {
		if delivered {
			return nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("%s (or %s) cannot be combined with a recipe that ships %s: the benchmark would have two owners; drop the supplied runtime to measure the delivered artifact, or remove the shipped runtime's %s override to supply your own",
					perfConstraintNCCLBenchmarkRuntime, perfConstraintNCCLBenchmarkRuntimeRef,
					gkenet.TCPXORuntimeName, recipe.GKETCPXOInterfacesOverrideKey))
		}
		emitRuntimeSource(runtimeSourceRecipeSupplied)
		return &benchmarkRuntimePlan{carrier: customRuntime, source: runtimeSourceRecipeSupplied}, nil
	}
	if !delivered {
		emitRuntimeSource(runtimeSourceCapability)
		return &benchmarkRuntimePlan{source: runtimeSourceCapability}, nil
	}
	// A benchmark profile retargets the skeleton, the preflights and the
	// watcher to another platform; a delivered runtime is wired for the one
	// the recipe ships on. Combining them mirrors the supplied-runtime case —
	// two owners of the benchmark's platform — and is rejected the same way.
	if profiled {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s cannot be combined with a recipe that ships %s: the shipped runtime fixes the benchmark's platform; drop the profile to measure the delivered artifact",
				perfConstraintNCCLBenchmarkProfile, gkenet.TCPXORuntimeName))
	}
	// The class is recipe-determined and is settled here, BEFORE the live
	// verification below: a missing runtime, a mapping drift, or an absent
	// network fails the run, and that failure must still say it was a
	// delivered-artifact measurement that failed, not a fixture that never ran.
	emitRuntimeSource(runtimeSourceDelivered)
	if ctx.DynamicClient == nil {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, "dynamic client is not available")
	}
	shipped, err := verifyDeliveredTCPXORuntime(ctx, recorded)
	if err != nil {
		return nil, err
	}
	// The skeleton is the platform's own MPI benchmark template. The fabric
	// argument selects between per-fabric template trees (the RoCE tree lives
	// under testdata/roce/); a DELIVERED runtime already carries its fabric
	// wiring from the shipped object, so the only thing the skeleton must match
	// is the platform — never the AICR_NCCL_FABRIC environment, which describes
	// the validator's own fixture and has no bearing on what the recipe ships.
	// Pinning the platform default here keeps an operator-set fabric override
	// from redirecting a delivered derivation to a template tree that does not
	// exist for this platform.
	_ = fabric
	skeletonPath := templatePath(accelerator, service, variant, fabricEFA, "runtime.yaml")
	skeleton, err := parseYAMLTemplate(skeletonPath, nil)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to load benchmark runtime skeleton "+skeletonPath, err)
	}
	derived, prov, err := deriveBenchmarkRuntime(skeleton, shipped)
	if err != nil {
		return nil, err
	}
	// The carrier is the gating stand-in described on benchmarkRuntimePlan; it
	// still holds skeleton placeholders and is never applied. It rides the same
	// shape gate as a recipe-supplied runtime so every downstream branch that
	// already knows to leave a self-wired runtime's fabric alone
	// (customRuntime != "") applies to the derived one.
	raw, err := yaml.Marshal(derived.Object)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to serialize derived benchmark runtime", err)
	}
	if err := v1.ValidateBenchmarkRuntime(string(raw)); err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "derived benchmark runtime failed the shape gate", err)
	}
	slog.Info("Derived NCCL benchmark runtime from the shipped ClusterTrainingRuntime",
		"shipped", gkenet.TCPXORuntimeName, "shippedDigest", prov.shippedDigest, "overriddenPaths", len(prov.overridePaths))
	// provenance stays nil here on purpose: the record that is published
	// describes the object that was APPLIED, and nothing has been yet. The
	// derivation-time record above is logging only.
	return &benchmarkRuntimePlan{carrier: string(raw), source: runtimeSourceDelivered, shipped: shipped}, nil
}

// verifyDeliveredTCPXORuntime is the shared three-way verifier: the recipe's
// recorded mapping must equal the deployed runtime's mapping exactly and in
// order, and every network the deployed runtime selects must exist on the
// cluster. It lives here so `--phase performance` compares recipe -> runtime ->
// cluster itself instead of assuming the deployment phase ran; the deployment
// check calls the same gkenet primitives. It verifies only — a missing or
// divergent wiring is a finding, never something to inject.
func verifyDeliveredTCPXORuntime(ctx *validators.Context, recorded []recipe.NetworkInterfaceMapping) (*unstructured.Unstructured, error) {
	shipped, err := gkenet.ReadDeployedTCPXORuntime(ctx.Ctx, ctx.DynamicClient)
	if err != nil {
		return nil, err
	}
	deployed, err := gkenet.DeployedTCPXOMapping(shipped)
	if err != nil {
		return nil, err
	}
	if vErr := gkenet.VerifyMappingMatchesRecipe(recorded, deployed); vErr != nil {
		return nil, vErr
	}
	discovered, err := gkenet.DiscoverGPUNICNetworks(ctx.Ctx, ctx.DynamicClient)
	if err != nil {
		// The raw API error is deliberately unwrapped by the discoverer; give a
		// stalled or canceled read its own code rather than an internal fault.
		return nil, aicrErrors.Wrap(gkenet.ReadErrorCode(err), "failed to discover GKE GPU NIC networks", err)
	}
	if err := gkenet.VerifyNetworksExist(deployed, discovered); err != nil {
		return nil, err
	}
	return shipped, nil
}

// benchmarkOwnedNodePaths enumerates every path under the worker
// PodTemplateSpec that the benchmark is allowed to change when deriving from
// the shipped runtime. Everything else is copied wholesale, so a fabric field
// added to the shipped runtime later is carried without a code change. The
// guard below fails loudly if the derived template differs from the shipped one
// anywhere outside this list — that is the boundary where copy-by-construction
// stops providing equivalence and has to be checked instead.
//
// Paths are dotted, with the worker container addressed by name rather than
// index so a reordering in the shipped spec cannot silently move the boundary.
var benchmarkOwnedNodePaths = ownedWorkerPaths(benchmarkOwnedWorkerFields)

// benchmarkOwnedWorkerFields is the single list of worker-container fields the
// derivation re-applies from the skeleton. Both the override loop and the
// guard allowlist are derived from it, so a field cannot be added to one and
// forgotten in the other — which would let a differing skeleton value pass the
// guard while the derived template silently kept the shipped value.
var benchmarkOwnedWorkerFields = []string{"image", "command", "args", "resources", "terminationMessagePolicy"}

func ownedWorkerPaths(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, "spec.containers["+benchmarkWorkerContainer+"]."+f)
	}
	return out
}

// deriveBenchmarkRuntime builds the benchmark TrainingRuntime from the
// embedded MPI skeleton and the shipped ClusterTrainingRuntime: the skeleton
// supplies everything benchmark-owned above the worker template (kind and
// scope, the framework label, mlPolicy.mpi, the network block, the launcher
// job, successPolicy); the shipped runtime supplies the worker template —
// metadata AND spec — wholesale; then the skeleton's worker container fields on
// the owned-path list are re-applied over the copy. Volumes and volumeMounts the
// skeleton needs beyond the shipped ones are merged additively by name rather
// than replaced.
//
// It returns the provenance record alongside: content identities of both
// normalized worker templates and the owned paths at which they differ.
func deriveBenchmarkRuntime(skeleton, shipped *unstructured.Unstructured) (*unstructured.Unstructured, *derivedRuntimeProvenance, error) {
	shippedTmpl, err := gkenet.NodeTemplateOf(shipped)
	if err != nil {
		return nil, nil, err
	}
	if bErr := checkShippedWorkerBaseline(shippedTmpl); bErr != nil {
		return nil, nil, bErr
	}
	skelTmpl, err := gkenet.NodeTemplateOf(skeleton)
	if err != nil {
		return nil, nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "benchmark skeleton has no node template", err)
	}

	derivedTmpl := serializer.DeepCopyAnyMap(shippedTmpl)
	skelWorker := workerContainer(skelTmpl)
	derWorker := workerContainer(derivedTmpl)
	if skelWorker == nil || derWorker == nil {
		return nil, nil, aicrErrors.New(aicrErrors.ErrCodeInternal,
			"both the skeleton and the shipped runtime must define a worker container named \"node\"")
	}
	for _, field := range benchmarkOwnedWorkerFields {
		if v, ok := skelWorker[field]; ok {
			derWorker[field] = serializer.DeepCopyAny(v)
		} else {
			delete(derWorker, field)
		}
	}
	// The measurement must run under the env the shipped worker declares, so
	// the fixture's bootstrap — which re-sources the host nccl-env-profile.sh
	// over it — is replaced by one that only exports what the container already
	// has, and the launcher's NCCL/CUDA tuning exports (which mpirun -x would
	// push over the shipped values on every rank) are dropped.
	derWorker["args"] = []any{deliveredWorkerBootstrap}
	mergeNamedList(derWorker, skelWorker, "volumeMounts")
	mergeNamedListAt(derivedTmpl, skelTmpl, []string{"spec", "volumes"})

	out := skeleton.DeepCopy()
	if err := setNodeTemplate(out, derivedTmpl); err != nil {
		return nil, nil, err
	}
	if err := stripLauncherNCCLTuning(out); err != nil {
		return nil, nil, err
	}

	diff := diffTemplatePaths(shippedTmpl, derivedTmpl)
	var outside []string
	for _, p := range diff {
		if !overlapsAny(p, benchmarkOwnedNodePaths) && !isAdditiveMergePath(p) {
			outside = append(outside, p)
		}
	}
	if len(outside) > 0 {
		return nil, nil, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("derived benchmark runtime diverges from the shipped %s outside the benchmark-owned paths at: %s — the shipped runtime changed shape in a way the derivation does not carry; update benchmarkOwnedNodePaths deliberately or fix the copy",
				gkenet.TCPXORuntimeName, strings.Join(outside, ", ")))
	}
	prov := &derivedRuntimeProvenance{
		shippedDigest: digestOf(shippedTmpl),
		derivedDigest: digestOf(derivedTmpl),
		overridePaths: diff,
	}
	return out, prov, nil
}

// checkShippedWorkerBaseline pins the semantic preconditions the derivation
// relies on for every path it overrides. Overridden paths are invisible to the
// diff — command/args/image/resources differ on every run by construction — so
// a fabric-sensitive change hiding inside one of them (the fixture activates
// TCPXO by sourcing nccl-env-profile.sh from the worker command; a shipped
// runtime doing the same would lose that under the override) can only be caught by checking the source still looks the way the override
// assumes. Each precondition names the assumption so a future shipped-runtime
// change fails here with a reason, not silently under the override.
func checkShippedWorkerBaseline(tmpl map[string]any) error {
	worker := workerContainer(tmpl)
	if worker == nil {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("shipped %s has no worker container named \"node\"", gkenet.TCPXORuntimeName))
	}
	// The benchmark replaces the worker entrypoint with its sshd bootstrap. That
	// is only equivalent if the shipped runtime carries no entrypoint of its own
	// — Trainer injects torchrun at TrainJob time — because anything a shipped
	// command did (env activation, wrapper scripts) would be discarded.
	for _, f := range []string{"command", "args"} {
		if _, ok := worker[f]; ok {
			return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("shipped %s worker sets %s, which the benchmark overrides; whatever that entrypoint does is not carried into the measurement — move it into env/volumes or teach the derivation about it deliberately",
					gkenet.TCPXORuntimeName, f))
		}
	}
	// The fabric env must be declared as env, not applied by an entrypoint, for
	// the override above to be lossless. Require the two variables that make the
	// wiring take effect at all.
	envNames := map[string]struct{}{}
	if envs, ok := worker["env"].([]any); ok {
		for _, e := range envs {
			if m, ok := e.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					envNames[n] = struct{}{}
				}
			}
		}
	}
	for _, need := range []string{"NCCL_FASTRAK_IFNAME", "NCCL_SOCKET_IFNAME"} {
		if _, ok := envNames[need]; !ok {
			return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("shipped %s worker does not declare %s as env; the derivation carries fabric configuration only through env, so the benchmark would run without it", gkenet.TCPXORuntimeName, need))
		}
	}
	// resources: the skeleton's block is the GPU request/limit alone, so the
	// override is lossless only while the shipped block asks for nothing else.
	// A fabric device or an extended resource added under the shipped
	// resources later would be dropped silently under the override — fail here
	// instead, naming the key.
	// The check is two-sided: extra keys would be dropped, and a MISSING GPU
	// request would be silently repaired by the skeleton's — a deployed runtime
	// whose GPU request was removed must fail, not be measured as delivered.
	// The quantity itself is checked against the discovered node GPU count at
	// apply time (checkShippedWorkerGPUCount), where that count is known.
	res, _ := worker["resources"].(map[string]any)
	for section, raw := range res {
		if section != resourcesLimits && section != resourcesRequests {
			return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("shipped %s worker resources carry %q, which the benchmark override would drop", gkenet.TCPXORuntimeName, section))
		}
		m, _ := raw.(map[string]any)
		for name := range m {
			if name != shippedWorkerGPUResource {
				return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
					fmt.Sprintf("shipped %s worker resources.%s carry %q beyond %s, which the benchmark override would drop", gkenet.TCPXORuntimeName, section, name, shippedWorkerGPUResource))
			}
		}
	}
	for _, section := range []string{resourcesLimits, resourcesRequests} {
		m, _ := res[section].(map[string]any)
		if _, ok := m[shippedWorkerGPUResource]; !ok {
			return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("shipped %s worker resources.%s do not request %s; the benchmark would substitute its own GPU request for a runtime that no longer schedules on GPUs", gkenet.TCPXORuntimeName, section, shippedWorkerGPUResource))
		}
	}
	// terminationMessagePolicy: the skeleton's worker sets none (only its
	// launcher does), so the owned-field override CLEARS whatever the shipped
	// worker carries. A shipped value would be a diagnostics choice for the
	// training workload with no bearing on the measurement, but one exists only
	// if the shipped runtime changed shape, which must be seen rather than
	// silently cleared.
	if _, ok := worker["terminationMessagePolicy"]; ok {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("shipped %s worker sets terminationMessagePolicy, which the benchmark overrides; confirm the derivation still measures what the recipe ships and update the baseline deliberately", gkenet.TCPXORuntimeName))
	}
	// image is the one override with no precondition by design: the benchmark
	// binary (nccl-tests under MPI) lives in the fixture image, and the shipped
	// training image carries neither. Replacing it is inherent to measuring at
	// all; the fabric plugin is mounted from the host, not the image.
	return nil
}

// shippedWorkerGPUResource is the only resource the shipped worker may request
// for the derivation's resources override to be lossless.
const shippedWorkerGPUResource = "nvidia.com/gpu"

// The two resource sections the shipped worker may carry.
const (
	resourcesLimits   = "limits"
	resourcesRequests = "requests"
)

// checkShippedWorkerGPUCount completes the resources baseline where the node
// GPU count is known: the shipped worker's nvidia.com/gpu request (limits and
// requests, already proven present) must equal the per-node count the
// benchmark is about to substitute. A deployed runtime asking for a different
// count would not schedule the way the derived one does, so measuring it as
// delivered-artifact would misattribute the number.
func checkShippedWorkerGPUCount(shipped *unstructured.Unstructured, gpusPerNode string) error {
	tmpl, err := gkenet.NodeTemplateOf(shipped)
	if err != nil {
		return err
	}
	worker := workerContainer(tmpl)
	for _, section := range []string{resourcesLimits, resourcesRequests} {
		got, _, _ := unstructured.NestedFieldNoCopy(worker, "resources", section, shippedWorkerGPUResource)
		if fmt.Sprint(got) != gpusPerNode {
			return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("shipped %s worker resources.%s request %s=%v but the target nodes carry %s GPUs each; the deployed runtime does not schedule the way the benchmark would, so it is not measured as the delivered artifact",
					gkenet.TCPXORuntimeName, section, shippedWorkerGPUResource, got, gpusPerNode))
		}
	}
	return nil
}

// deliveredWorkerBootstrap is the worker entrypoint for a DERIVED runtime. It
// is the fixture's sshd bootstrap minus the one line that made the fixture
// self-wiring: sourcing the host nccl-env-profile.sh, which would overwrite
// the NCCL env the shipped worker declares and turn the measurement back into
// a fixture measurement. The shipped env (NCCL_*, CUDA_*, LD_LIBRARY_PATH) is
// exported to the ssh sessions mpirun opens so each rank runs under exactly
// what the recipe ships; checkShippedWorkerBaseline guarantees the grep is
// non-empty.
const deliveredWorkerBootstrap = `set -x &&
apt-get update &&
apt-get install -y --no-install-recommends openssh-server &&
mkdir -p /var/run/sshd &&
chmod 0755 /var/run/sshd &&
mkdir -p /root/.ssh &&
chmod 700 /root/.ssh &&
cp /tmp/mpi-keys/* /root/.ssh/ &&
chmod 600 /root/.ssh/id_rsa &&
chmod 644 /root/.ssh/id_rsa.pub /root/.ssh/authorized_keys &&
env | grep -E '^NCCL_|^CUDA_|^LD_LIBRARY_PATH=' > /root/.ssh/environment &&
echo "PermitUserEnvironment yes" >> /etc/ssh/sshd_config &&
/usr/sbin/sshd -De
`

// launcherOwnedExportPrefixes are the mpirun -x exports the benchmark keeps on
// a derived runtime: mpirun's own control-plane pinning and NCCL_DEBUG, which
// is log verbosity the fixture owns (#1712), not fabric configuration.
var launcherOwnedExportPrefixes = []string{"UCX_", "NCCL_DEBUG="}

// stripLauncherNCCLTuning removes every `-x NCCL_*`, `-x CUDA_*` and
// `-x LD_LIBRARY_PATH=` pair from the launcher's mpirun args. mpirun -x pushes
// the value into every rank's environment ahead of what the worker's ssh
// session exported, so a fixture tuning value left here would silently win over
// the shipped one and the number would no longer describe the delivered
// artifact. NCCL_DEBUG is kept: it is output volume, not wiring.
func stripLauncherNCCLTuning(rt *unstructured.Unstructured) error {
	jobs, found, err := unstructured.NestedSlice(rt.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, "benchmark skeleton has no replicatedJobs")
	}
	for i, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(job, "name"); name != benchmarkLauncherJob {
			continue
		}
		tmpl, _, _ := unstructured.NestedMap(job, "template", "spec", "template")
		launcher := workerContainer(tmpl)
		if launcher == nil {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, "benchmark skeleton launcher has no container named \"node\"")
		}
		args, _ := launcher["args"].([]any)
		kept := make([]any, 0, len(args))
		for j := 0; j < len(args); j++ {
			if args[j] == "-x" && j+1 < len(args) {
				if v, ok := args[j+1].(string); ok && isFabricExport(v) {
					j++
					continue
				}
			}
			kept = append(kept, args[j])
		}
		launcher["args"] = kept
		if err := unstructured.SetNestedMap(job, tmpl, "template", "spec", "template"); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to set derived launcher template", err)
		}
		jobs[i] = job
		return unstructured.SetNestedSlice(rt.Object, jobs, "spec", "template", "spec", "replicatedJobs")
	}
	return aicrErrors.New(aicrErrors.ErrCodeInternal, "benchmark skeleton declares no \"launcher\" replicatedJob")
}

// isFabricExport reports whether an mpirun -x value is fabric configuration a
// derived runtime must take from the shipped worker instead.
func isFabricExport(v string) bool {
	for _, keep := range launcherOwnedExportPrefixes {
		if strings.HasPrefix(v, keep) {
			return false
		}
	}
	return strings.HasPrefix(v, "NCCL_") || strings.HasPrefix(v, "CUDA_") || strings.HasPrefix(v, "LD_LIBRARY_PATH=")
}

// finalizeRuntimeProvenance computes the provenance record against the
// runtime object that is about to be applied — after scheduling was stamped —
// so the digests and path lists describe what actually ran. It RETURNS the
// record rather than storing it: the caller assigns plan.provenance only once
// the create succeeded, so a run that fails before or at application (namespace
// or lock failure, a declared-but-incomplete Trainer, an admission rejection)
// publishes no record claiming an object was applied. The derivation-time
// guard already proved the derived template stays inside the owned paths; this
// only records.
func finalizeRuntimeProvenance(plan *benchmarkRuntimePlan, applied *unstructured.Unstructured) (*derivedRuntimeProvenance, error) {
	shippedTmpl, err := gkenet.NodeTemplateOf(plan.shipped)
	if err != nil {
		return nil, err
	}
	appliedTmpl, err := gkenet.NodeTemplateOf(applied)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "applied benchmark runtime has no node template", err)
	}
	diff := diffTemplatePaths(shippedTmpl, appliedTmpl)
	var inherited []string
	for _, p := range leafPaths("", shippedTmpl) {
		if !overlapsAny(p, diff) {
			inherited = append(inherited, p)
		}
	}
	sort.Strings(inherited)
	return &derivedRuntimeProvenance{
		shippedDigest:  digestOf(shippedTmpl),
		derivedDigest:  digestOf(appliedTmpl),
		overridePaths:  diff,
		inheritedPaths: inherited,
	}, nil
}

// emitRuntimeSource publishes the provenance class the moment it is decided —
// inside resolveBenchmarkRuntimeSource, before the delivered path's live
// verification and before any cluster mutation — so a run that fails at any
// later point still records which artifact it set out to measure. The class
// rides EmitExtra and survives minimal redaction (redact allowlist key
// runtimeSource).
func emitRuntimeSource(source ncclRuntimeSource) {
	fmt.Printf("Benchmark runtime source: %s\n", source)
	if err := validators.EmitExtra(runtimeProvenanceExtra(&benchmarkRuntimePlan{source: source})); err != nil {
		slog.Warn("failed to emit runtime source extra", "error", err)
	}
}

// emitRuntimeProvenance publishes the derived-runtime audit record after the
// run: the human-readable listing on stdout (--full evidence) and the bounded
// ctrf.RuntimeProvenance carrier, which survives minimal redaction. No-op for
// the other two sources.
func emitRuntimeProvenance(plan *benchmarkRuntimePlan) {
	prov := plan.provenance
	if prov == nil {
		return
	}
	if err := validators.EmitRuntimeProvenance(runtimeProvenanceCarrier(prov)); err != nil {
		slog.Warn("failed to emit runtime provenance carrier", "error", err)
	}
	fmt.Printf("Benchmark runtime derived from %s: shipped node template sha256 %s, applied sha256 %s\n",
		gkenet.TCPXORuntimeName, prov.shippedDigest, prov.derivedDigest)
	fmt.Printf("Overridden by the benchmark (%d paths):\n", len(prov.overridePaths))
	for _, p := range prov.overridePaths {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("Inherited from the shipped runtime unchanged (%d paths):\n", len(prov.inheritedPaths))
	for _, p := range prov.inheritedPaths {
		fmt.Printf("  %s\n", p)
	}
}

// runtimeProvenanceExtra is the allowlisted Extra payload for a plan: the
// provenance class only. Digests and paths ride their own bounded carrier
// (runtimeProvenanceCarrier), never Extra, whose values must be counts or
// codes. Pure so it can be unit-tested without capturing the stdout sentinel.
func runtimeProvenanceExtra(plan *benchmarkRuntimePlan) map[string]string {
	return map[string]string{extraKeyRuntimeSource: string(plan.source)}
}

// runtimeProvenanceCarrier maps the audit record onto the ctrf carrier.
func runtimeProvenanceCarrier(prov *derivedRuntimeProvenance) *ctrf.RuntimeProvenance {
	return &ctrf.RuntimeProvenance{
		ShippedDigest:   prov.shippedDigest,
		DerivedDigest:   prov.derivedDigest,
		OverriddenPaths: append([]string(nil), prov.overridePaths...),
		InheritedPaths:  append([]string(nil), prov.inheritedPaths...),
	}
}

// --- template plumbing -------------------------------------------------------

// benchmarkWorkerContainer is the worker container name both the shipped
// runtime and the MPI skeleton use; the derivation keys its overrides on it.
// The skeleton's launcher job uses the same container name.
const benchmarkWorkerContainer = "node"

// benchmarkLauncherJob is the skeleton's mpirun replicatedJob.
const benchmarkLauncherJob = "launcher"

// workerContainer returns the worker container map of a PodTemplateSpec map,
// or nil when absent. The returned map aliases tmpl so callers can override in
// place on a template they own (the derivation works on a deep copy).
func workerContainer(tmpl map[string]any) map[string]any {
	cs, _ := tmpl["spec"].(map[string]any)
	list, _ := cs["containers"].([]any)
	for _, c := range list {
		if m, ok := c.(map[string]any); ok && m["name"] == benchmarkWorkerContainer {
			return m
		}
	}
	return nil
}

// mergeNamedList adds entries of src[key] whose "name" is absent from dst[key].
// Existing entries win: the shipped runtime's mounts/volumes are the contract,
// the skeleton only contributes what the benchmark additionally needs.
func mergeNamedList(dst, src map[string]any, key string) {
	srcList, _ := src[key].([]any)
	if len(srcList) == 0 {
		return
	}
	dstList, _ := dst[key].([]any)
	have := map[string]struct{}{}
	for _, e := range dstList {
		if m, ok := e.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				have[n] = struct{}{}
			}
		}
	}
	for _, e := range srcList {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := m["name"].(string); ok {
			if _, dup := have[n]; dup {
				continue
			}
		}
		dstList = append(dstList, serializer.DeepCopyAny(m))
	}
	dst[key] = dstList
}

func mergeNamedListAt(dst, src map[string]any, path []string) {
	d, found, err := unstructured.NestedMap(dst, path[:len(path)-1]...)
	if err != nil || !found {
		return
	}
	s, _, _ := unstructured.NestedMap(src, path[:len(path)-1]...)
	mergeNamedList(d, s, path[len(path)-1])
	_ = unstructured.SetNestedMap(dst, d, path[:len(path)-1]...)
}

// isAdditiveMergePath accepts diffs produced by the additive volume/mount merge.
func isAdditiveMergePath(p string) bool {
	return strings.HasPrefix(p, "spec.volumes") || strings.Contains(p, "].volumeMounts")
}

func setNodeTemplate(rt *unstructured.Unstructured, tmpl map[string]any) error {
	jobs, found, err := unstructured.NestedSlice(rt.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, "benchmark skeleton has no replicatedJobs")
	}
	for i, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _, _ := unstructured.NestedString(job, "name"); name != gkenet.TCPXONodeJob {
			continue
		}
		if err := unstructured.SetNestedMap(job, tmpl, "template", "spec", "template"); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to set derived node template", err)
		}
		jobs[i] = job
		return unstructured.SetNestedSlice(rt.Object, jobs, "spec", "template", "spec", "replicatedJobs")
	}
	return aicrErrors.New(aicrErrors.ErrCodeInternal, "benchmark skeleton declares no \"node\" replicatedJob")
}

// diffTemplatePaths returns the sorted dotted paths at which a and b differ,
// descending into maps and into lists of named objects by name (containers,
// volumes, env, mounts) so a reorder is not a diff and a change is attributed
// to the element, not its index.
func diffTemplatePaths(a, b map[string]any) []string {
	var out []string
	diffInto(&out, "", a, b)
	sort.Strings(out)
	return out
}

func diffInto(out *[]string, prefix string, a, b any) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]struct{}{}
		for k := range am {
			keys[k] = struct{}{}
		}
		for k := range bm {
			keys[k] = struct{}{}
		}
		for k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			av, ain := am[k]
			bv, bin := bm[k]
			if !ain || !bin {
				*out = append(*out, p)
				continue
			}
			diffInto(out, p, av, bv)
		}
		return
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok && allNamed(al) && allNamed(bl) {
		an, bn := byName(al), byName(bl)
		for n, av := range an {
			p := prefix + "[" + n + "]"
			if bv, ok := bn[n]; ok {
				diffInto(out, p, av, bv)
			} else {
				*out = append(*out, p)
			}
		}
		for n := range bn {
			if _, ok := an[n]; !ok {
				*out = append(*out, prefix+"["+n+"]")
			}
		}
		return
	}
	if fmt.Sprintf("%#v", a) != fmt.Sprintf("%#v", b) {
		*out = append(*out, prefix)
	}
}

func allNamed(l []any) bool {
	if len(l) == 0 {
		return false
	}
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["name"].(string); !ok {
			return false
		}
	}
	return true
}

func byName(l []any) map[string]any {
	out := make(map[string]any, len(l))
	for _, e := range l {
		m := e.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

// leafPaths enumerates the dotted leaf paths of a template using the same
// addressing as diffTemplatePaths (maps by key, named lists by [name]), so the
// two can be set-compared to inventory what a derived runtime inherited.
func leafPaths(prefix string, v any) []string {
	switch t := v.(type) {
	case map[string]any:
		out := make([]string, 0, len(t))
		for k, cv := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out = append(out, leafPaths(p, cv)...)
		}
		return out
	case []any:
		if allNamed(t) {
			var out []string
			for n, cv := range byName(t) {
				out = append(out, leafPaths(prefix+"["+n+"]", cv)...)
			}
			return out
		}
	}
	return []string{prefix}
}

// overlapsAny applies the profile lock's overlap rule (argocdhelm: a path
// "equals, contains, or is contained by" an owned path) to dotted paths:
// segment-wise, one must be a prefix of the other.
func overlapsAny(p string, owned []string) bool {
	ps := strings.Split(p, ".")
	for _, o := range owned {
		os := strings.Split(o, ".")
		n := len(ps)
		if len(os) < n {
			n = len(os)
		}
		match := true
		for i := 0; i < n; i++ {
			if ps[i] != os[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// digestOf is the content identity of a normalized template: canonical JSON
// (sorted keys, via sigs.k8s.io/yaml's JSON path) hashed with sha256.
func digestOf(m map[string]any) string {
	b, err := yaml.Marshal(m) // yaml.Marshal sorts map keys deterministically
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

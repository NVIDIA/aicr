// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// topologyProbeCmd runs in the nic-configuration-daemon pod. It emits three
// blocks (some may be empty):
//  1. nvidia-smi --query-gpu CSV: "<index>, <pci.bus_id>"
//     --- TOPO --- marker
//  2. nvidia-smi topo -mp output (matrix + NIC legend)
//     --- NUMA --- marker
//  3. Per-Mellanox-PF lines: "<pci>|<numa_node>"
//
// The shell is wrapped so that missing/crashing nvidia-smi keeps the overall
// exit code at 0 (stdout simply lacks the nvidia-smi blocks). The sysfs NUMA
// walk always runs independently of nvidia-smi.
const topologyProbeCmd = `if [ -x /host/usr/bin/nvidia-smi ]; then
  export LD_LIBRARY_PATH=/host/usr/lib/x86_64-linux-gnu:/host/usr/lib/aarch64-linux-gnu:$LD_LIBRARY_PATH
  /host/usr/bin/nvidia-smi --query-gpu=index,pci.bus_id --format=csv,noheader 2>/dev/null || true
  echo '---TOPO---'
  /host/usr/bin/nvidia-smi topo -mp 2>/dev/null || true
fi
echo '---NUMA---'
for d in /sys/bus/pci/devices/*; do
  v=$(cat "$d/vendor" 2>/dev/null) || continue
  [ "$v" = "0x15b3" ] || continue
  echo "$(basename "$d")|$(cat "$d/numa_node" 2>/dev/null || echo -1)"
done`

// proximity represents one cell of the nvidia-smi topology matrix.
// Ordering: lower value = closer. self is a sentinel for the diagonal.
type proximity int

const (
	proxPIX proximity = iota
	proxPXB
	proxPHB
	proxNODE
	proxSYS
	proxUnknown
)

func parseProximity(s string) proximity {
	switch strings.TrimSpace(s) {
	case "PIX":
		return proxPIX
	case "PXB":
		return proxPXB
	case "PHB":
		return proxPHB
	case "NODE":
		return proxNODE
	case "SYS":
		return proxSYS
	default:
		return proxUnknown
	}
}

func (p proximity) String() string {
	switch p {
	case proxPIX:
		return "PIX"
	case proxPXB:
		return "PXB"
	case proxPHB:
		return "PHB"
	case proxNODE:
		return "NODE"
	case proxSYS:
		return "SYS"
	default:
		return ""
	}
}

// topoData holds the parsed probe output. Any field may be zero-valued if the
// corresponding block was missing or empty; callers handle partial data.
type topoData struct {
	// gpuPCI maps nvidia-smi GPU index -> normalized PCI address
	// (e.g. 0 -> "0000:18:00.0").
	gpuPCI map[int]string
	// matrix maps RDMA device name -> (GPU index -> proximity label).
	matrix map[string]map[int]proximity
	// numa maps normalized PCI address -> numa_node value as read from sysfs
	// (-1 meaning "no NUMA locality").
	numa map[string]int
}

// normalizePCI converts PCI addresses to a canonical lowercase 4-digit-domain
// form used throughout our config (e.g. "00000000:19:00.0" -> "0000:19:00.0",
// "0000:19:00.0" -> "0000:19:00.0"). Returns "" if input doesn't parse.
func normalizePCI(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	domain := strings.TrimLeft(parts[0], "0")
	if domain == "" {
		domain = "0"
	}
	// Pad back to 4 hex digits.
	if len(domain) < 4 {
		domain = strings.Repeat("0", 4-len(domain)) + domain
	}
	return domain + ":" + parts[1]
}

// busOf extracts the hex bus number from a normalized PCI BDF
// ("0000:19:00.0" -> 0x19). Returns -1 on parse failure.
func busOf(pciAddr string) int {
	parts := strings.Split(pciAddr, ":")
	if len(parts) < 3 {
		return -1
	}
	bus, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return -1
	}
	return int(bus)
}

// parseTopologyProbe parses the three-block output emitted by topologyProbeCmd.
// All blocks are optional; a missing block produces an empty map on the
// corresponding field rather than an error. The only hard error is a totally
// unparseable input (which we treat as empty).
func parseTopologyProbe(output string) topoData {
	data := topoData{
		gpuPCI: map[int]string{},
		matrix: map[string]map[int]proximity{},
		numa:   map[string]int{},
	}

	const (
		topoMarker = "---TOPO---"
		numaMarker = "---NUMA---"
	)

	// Segment the output by markers.
	var queryBlock, topoBlock, numaBlock string
	s := output
	if i := strings.Index(s, numaMarker); i >= 0 {
		numaBlock = s[i+len(numaMarker):]
		s = s[:i]
	}
	if i := strings.Index(s, topoMarker); i >= 0 {
		topoBlock = s[i+len(topoMarker):]
		queryBlock = s[:i]
	} else {
		// No TOPO marker — whatever remains before NUMA is either pure query
		// (rare) or garbage. Treat it as query for robustness.
		queryBlock = s
	}

	parseGPUQuery(queryBlock, data.gpuPCI)
	parseTopoMatrix(topoBlock, data.matrix)
	parseNUMABlock(numaBlock, data.numa)
	return data
}

// parseGPUQuery parses lines like "0, 00000000:18:00.0" (CSV, whitespace-padded).
func parseGPUQuery(block string, out map[int]string) {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		pci := normalizePCI(parts[1])
		if pci == "" {
			continue
		}
		out[idx] = pci
	}
}

// parseTopoMatrix parses the matrix emitted by `nvidia-smi topo -mp` plus its
// trailing "NIC Legend:" mapping. Fills `out` keyed by RDMA device name.
func parseTopoMatrix(block string, out map[string]map[int]proximity) {
	// Normalize ANSI escape codes away (nvidia-smi sometimes wraps the header
	// row with \x1b[4m…\x1b[0m for underline).
	block = stripANSI(block)

	lines := strings.Split(block, "\n")

	// Find the header row: starts with tabs/spaces then "GPU0" as first column.
	headerIdx := -1
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "GPU0" {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return
	}
	headerFields := strings.Fields(lines[headerIdx])

	// Identify which columns are GPU columns. The tail columns
	// ("CPU Affinity", "NUMA Affinity", "GPU NUMA ID") are not proximity cells;
	// we only care about GPU<N> columns here.
	gpuCol := map[int]int{} // column index (in row after row-label) -> GPU index
	for i, h := range headerFields {
		if strings.HasPrefix(h, "GPU") {
			if idx, err := strconv.Atoi(strings.TrimPrefix(h, "GPU")); err == nil {
				gpuCol[i] = idx
			}
		}
	}
	if len(gpuCol) == 0 {
		return
	}

	// Matrix rows start right after the header. Row format:
	//   <rowLabel>\t<cell0>\t<cell1>\t...<trailing-affinity-columns>
	// rowLabel looks like GPU0..7 or NIC0..N. For NIC rows we record cells.
	nicRows := map[string]map[int]proximity{} // key: "NIC<n>" -> gpuIdx -> prox

	// Also need the NIC Legend trailer to convert "NIC<n>" labels to RDMA
	// device names. We'll parse it below and re-key nicRows at the end.
	legendIdx := -1
	for i := headerIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "NIC Legend:") {
			legendIdx = i
			break
		}
		fields := strings.Fields(lines[i])
		if len(fields) == 0 {
			continue
		}
		rowLabel := fields[0]
		if !strings.HasPrefix(rowLabel, "NIC") {
			continue
		}
		// After the row label, fields are the proximity cells aligned to
		// headerFields positions. Some cells are " X " which Fields collapses.
		row := map[int]proximity{}
		// fields[0] = rowLabel, fields[1..] = cells + trailing affinity cols
		cells := fields[1:]
		for pos, gpuIdx := range gpuCol {
			if pos >= len(cells) {
				continue
			}
			p := parseProximity(cells[pos])
			if p == proxUnknown {
				// "X" self-cell or empty — skip.
				continue
			}
			row[gpuIdx] = p
		}
		if len(row) > 0 {
			nicRows[rowLabel] = row
		}
	}

	if legendIdx < 0 {
		return // No legend → can't rekey to RDMA names
	}

	// Parse legend lines: "  NIC0: rocep25s0f0"
	for i := legendIdx + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "NIC") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		label := strings.TrimSpace(parts[0])
		rdma := strings.TrimSpace(parts[1])
		if rdma == "" {
			continue
		}
		if row, ok := nicRows[label]; ok {
			out[rdma] = row
		}
	}
}

// stripANSI removes ANSI escape sequences (ESC [ ... m) from s. nvidia-smi
// emits \x1b[4m before the header row on some versions.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// Skip until the terminating letter.
			j := i + 2
			for j < len(s) {
				c := s[j]
				if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
					break
				}
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseNUMABlock parses lines like "0000:19:00.0|0" emitted by the sysfs walk.
func parseNUMABlock(block string, out map[string]int) {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		pci := normalizePCI(parts[0])
		if pci == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		out[pci] = n
	}
}

// pickConnectedGPU selects one GPU from equal-proximity candidates using a
// two-layer tiebreaker: usage (round-robin for load spreading), then bus
// adjacency (physical proximity for final disambiguation).
//
// Priority order matters. Round-robin first ensures dual-port cards with two
// equally-PIX GPUs (e.g. NIC4 + NIC5 both on the same card, both PIX to GPU4
// and GPU5) spread across the GPU pool — the second NIC finds its usual
// tie-partner already "used" and picks the other. Bus-adjacency as the
// second layer handles the flat-topology case where many candidates tie on
// usage: the first NIC picks the physically closest GPU, subsequent NICs see
// their closest GPU already used and fall through to the next-closest.
//
// Returns -1 only on empty input.
func pickConnectedGPU(candidates []int, nicPCI string,
	gpuPCI map[int]string, usage map[int]int) int {
	if len(candidates) == 0 {
		return -1
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	// Layer 1: find the lowest-usage count among candidates.
	minUsage := -1
	for _, g := range candidates {
		if minUsage < 0 || usage[g] < minUsage {
			minUsage = usage[g]
		}
	}
	var byUsage []int
	for _, g := range candidates {
		if usage[g] == minUsage {
			byUsage = append(byUsage, g)
		}
	}
	if len(byUsage) == 1 {
		return byUsage[0]
	}

	// Layer 2: among the usage-tied winners, pick the one closest by PCI bus.
	nicBus := busOf(nicPCI)
	minDist := -1
	for _, g := range byUsage {
		d := abs(busOf(gpuPCI[g]) - nicBus)
		if minDist < 0 || d < minDist {
			minDist = d
		}
	}
	var finalTied []int
	for _, g := range byUsage {
		if abs(busOf(gpuPCI[g])-nicBus) == minDist {
			finalTied = append(finalTied, g)
		}
	}
	sort.Ints(finalTied)
	return finalTied[0]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// reclassifyAndReassignRails applies the PIX-gate rule for E/W-vs-N/S and then
// renumbers rails sequentially over the E/W set.
//
// PIX-gate: if any PF has GPUProximity == "PIX", topology is discriminating —
// rewrite every PF's Traffic based on whether it has PIX (PIX => E/W,
// otherwise => N/S). When no PF has PIX (flat-topology box or nvidia-smi
// absent), Traffic is left intact so the upstream part-number heuristic still
// governs.
//
// Rails: after the Traffic decision, iterate in slice order and assign
// 0..N-1 to E/W PFs; N/S PFs get Rail cleared.
//
// Idempotent: safe to call multiple times.
func reclassifyAndReassignRails(pfs []config.PFConfig) {
	hasPIX := false
	for i := range pfs {
		if pfs[i].GPUProximity == "PIX" {
			hasPIX = true
			break
		}
	}
	if hasPIX {
		for i := range pfs {
			if pfs[i].GPUProximity == "PIX" {
				pfs[i].Traffic = "east-west"
			} else {
				pfs[i].Traffic = "north-south"
			}
		}
	}
	rail := 0
	for i := range pfs {
		if pfs[i].Traffic == "east-west" {
			r := rail
			pfs[i].Rail = &r
			rail++
		} else {
			pfs[i].Rail = nil
		}
	}
}

// discoverGPUTopology execs a single probe into the nic-configuration-daemon
// pod for one representative node of the group, parses the output, and
// enriches the group's PFs with NumaNode, ConnectedGPU, GPUProximity. If the
// PIX-gate fires it additionally rewrites Traffic and re-runs rail numbering.
//
// All failures are non-fatal: logged, the corresponding fields stay empty,
// and Traffic/Rail continue to reflect whatever the part-number heuristic
// assigned in buildClusterConfig.
func discoverGPUTopology(ctx context.Context, restConfig *rest.Config,
	namespace string, group *config.ClusterConfig, dsPods []corev1.Pod) {

	if group == nil || len(group.PFs) == 0 {
		return
	}

	targetPod := findDaemonPod(group.WorkerNodes, dsPods)
	if targetPod == nil {
		log.Log.Info("No nic-configuration-daemon pod found on group nodes; skipping GPU topology probing",
			"group", group.Identifier)
		return
	}
	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
		[]string{"/bin/sh", "-c", topologyProbeCmd})
	if err != nil {
		log.Log.Error(err, "failed to exec GPU topology probe", "pod", targetPod.Name)
		return
	}

	data := parseTopologyProbe(output)

	// Step 1: NumaNode enrichment. Works with or without nvidia-smi.
	for i := range group.PFs {
		pci := normalizePCI(group.PFs[i].PciAddress)
		if n, ok := data.numa[pci]; ok && n >= 0 {
			nn := n
			group.PFs[i].NumaNode = &nn
		}
	}

	// Step 2: if no matrix or no GPUs, topology enrichment stops here.
	if len(data.matrix) == 0 || len(data.gpuPCI) == 0 {
		log.Log.V(1).Info("GPU topology: nvidia-smi produced no matrix; preserving heuristic classification",
			"group", group.Identifier)
		return
	}

	// Step 3: per-PF proximity + GPU pairing. PFs are already PCI-sorted
	// (see buildClusterConfig), so iteration order is deterministic.
	usage := map[int]int{}
	for i := range group.PFs {
		pf := &group.PFs[i]
		if pf.RdmaDevice == "" {
			log.Log.V(1).Info("GPU topology: skipping PF with no RDMA device",
				"group", group.Identifier, "pci", pf.PciAddress)
			continue
		}
		row, ok := data.matrix[pf.RdmaDevice]
		if !ok {
			log.Log.V(1).Info("GPU topology: PF's RDMA device not present in nvidia-smi matrix (port likely down)",
				"group", group.Identifier, "pci", pf.PciAddress, "rdma", pf.RdmaDevice)
			continue
		}

		best := proxUnknown
		var candidates []int
		for gpuIdx, p := range row {
			if p == proxUnknown {
				continue
			}
			if best == proxUnknown || p < best {
				best = p
				candidates = []int{gpuIdx}
			} else if p == best {
				candidates = append(candidates, gpuIdx)
			}
		}
		if best == proxUnknown || len(candidates) == 0 {
			log.Log.V(1).Info("GPU topology: no proximity data for PF",
				"group", group.Identifier, "pci", pf.PciAddress, "rdma", pf.RdmaDevice)
			continue
		}

		chosen := pickConnectedGPU(candidates, pf.PciAddress,
			data.gpuPCI, usage)
		if chosen < 0 {
			continue
		}
		usage[chosen]++

		pf.ConnectedGPU = fmt.Sprintf("GPU%d", chosen)
		pf.GPUProximity = best.String()

		numaStr := "unknown"
		if pf.NumaNode != nil {
			numaStr = fmt.Sprintf("%d", *pf.NumaNode)
		}
		log.Log.V(1).Info("GPU topology: PF connectivity discovered",
			"group", group.Identifier,
			"pci", pf.PciAddress,
			"rdma", pf.RdmaDevice,
			"connectedGPU", pf.ConnectedGPU,
			"proximity", pf.GPUProximity,
			"numa", numaStr,
			"tiedCandidates", len(candidates))
	}

	// Step 4: PIX-gate reclassification (Traffic + Rails together).
	// Log the decision and each PF's final rail reason at debug level.
	hasPIX := false
	pixCount := 0
	for i := range group.PFs {
		if group.PFs[i].GPUProximity == "PIX" {
			hasPIX = true
			pixCount++
		}
	}
	if hasPIX {
		log.Log.V(1).Info("GPU topology: PIX detected — overriding part-number traffic classification",
			"group", group.Identifier, "pixPFs", pixCount, "totalPFs", len(group.PFs))
	} else {
		log.Log.V(1).Info("GPU topology: no PIX detected — preserving part-number traffic classification",
			"group", group.Identifier, "totalPFs", len(group.PFs))
	}

	reclassifyAndReassignRails(group.PFs)

	// Explain each PF's final rail decision. The "reason" lets operators see
	// at a glance why a device got a rail (or didn't).
	for i := range group.PFs {
		pf := &group.PFs[i]
		var reason string
		switch {
		case hasPIX && pf.GPUProximity == "PIX":
			reason = "PIX to " + pf.ConnectedGPU
		case hasPIX && pf.GPUProximity != "PIX":
			reason = "no PIX to any GPU (topology override)"
		default:
			reason = "part-number heuristic"
		}
		if pf.Rail != nil {
			log.Log.V(1).Info("Rail assigned",
				"group", group.Identifier,
				"pci", pf.PciAddress,
				"rdma", pf.RdmaDevice,
				"traffic", pf.Traffic,
				"rail", *pf.Rail,
				"reason", reason)
		} else {
			log.Log.V(1).Info("No rail (north-south)",
				"group", group.Identifier,
				"pci", pf.PciAddress,
				"rdma", pf.RdmaDevice,
				"reason", reason)
		}
	}
}

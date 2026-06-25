/*
 Copyright 2026, NVIDIA CORPORATION & AFFILIATES
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at
     http://www.apache.org/licenses/LICENSE-2.0
 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

// Package mofedmodules exposes the canonical default lists of kernel modules
// that the MOFED driver container unloads before reloading the OFED driver.
// This package is the single source of truth shared with other repositories
// (notably network-operator-init-container) so the pre-flight checker and the
// driver container agree on what counts as a known third-party RDMA or
// storage-over-RDMA module.
package mofedmodules

// Separator is the character used to join module names when serialized into
// env var values (e.g. STORAGE_MODULES, THIRD_PARTY_RDMA_MODULES). A single
// space keeps the bash- and Go-side representations identical.
const Separator = " "

// DefaultStorageModules is the list of storage-over-RDMA kernel modules that
// the driver container unloads when UNLOAD_STORAGE_MODULES=true. Includes
// both initiator (ib_iser, ib_srp, nvme_rdma, rpcrdma/xprtrdma) and target
// (ib_isert, ib_srpt, nvmet_rdma) sides of iSCSI, SRP, NVMe and NFS over RDMA.
var DefaultStorageModules = []string{
	"ib_iser", "ib_isert", "ib_srp", "ib_srpt",
	"nvme_rdma", "nvmet_rdma", "rpcrdma", "xprtrdma",
}

// DefaultThirdPartyRDMAModules is the list of non-NVIDIA NIC-vendor RDMA
// provider modules (from rdma-core) that the driver container unloads when
// UNLOAD_THIRD_PARTY_RDMA_MODULES=true.
//
// Do NOT add core RDMA infrastructure modules (iw_cm, ib_cm, rdma_cm,
// rdma_ucm, ib_core, ib_uverbs, etc.) — MOFED manages those in its own
// openibd unload sequence. Do NOT add storage-over-RDMA modules — those
// belong in DefaultStorageModules.
var DefaultThirdPartyRDMAModules = []string{
	"bnxt_re", "efa", "erdma", "iw_cxgb4",
	"hfi1", "hns_roce", "ionic_rdma", "irdma",
	"ib_qib", "mana_ib", "ocrdma", "qedr",
	"rdma_rxe", "siw", "vmw_pvrdma",
}

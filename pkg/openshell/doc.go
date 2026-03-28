// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package openshell provides caller-side policy enforcement for DNS-AID
// agent discovery.
//
// OpenShell secures the agent boundary; DNS-AID resolves what's inside it.
//
// When an agent discovers peers via DNS-AID SVCB records, the target may
// publish a policy document at its policy URI (SvcParamKey 65403). OpenShell
// fetches and evaluates that document as Layer 1 (caller-side) enforcement,
// determining whether the calling agent is permitted to reach the target.
//
// The policy document schema matches the dns-aid-core PolicyDocument
// (JSON over HTTPS). OpenShell implements the same 16 native rules
// and respects the same enforcement layer annotations.
//
// Usage:
//
//	guard := openshell.NewGuard(
//	    openshell.WithCallerID("aicrd"),
//	    openshell.WithMode(openshell.ModeStrict),
//	)
//
//	result, err := guard.Check(ctx, agentRecord)
//	if err != nil {
//	    // fetch or evaluation error (fail-open by default)
//	}
//	if !result.Allowed {
//	    // policy denied the connection
//	}
package openshell

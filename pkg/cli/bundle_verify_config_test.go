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

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// verifyConfig builds an AICRConfig document carrying only a spec.verify
// section, which is the shape a consumer-side config file takes.
func verifyConfig(t *testing.T, body string) string {
	t.Helper()
	return writeYAML(t, "aicr-config.yaml",
		"kind: AICRConfig\napiVersion: aicr.run/v1alpha2\nspec:\n  verify:\n"+body)
}

// runVerify runs the real verify command and returns stdout plus any error
// text, so a test can assert on whichever surface the message lands in.
func runVerify(t *testing.T, args ...string) string {
	t.Helper()
	cmd := bundleVerifyCmd()
	var buf bytes.Buffer
	cmd.Writer = &buf

	err := cmd.Run(context.Background(), args)

	// A flag-level failure means the command never reached its action, which
	// would make any assertion below meaningless.
	if err != nil {
		for _, bad := range []string{"flag provided but not defined", "no such flag", "flag needs an argument"} {
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("got flag-level error %q for args %v", err.Error(), args)
			}
		}
	}
	combined := buf.String()
	if err != nil {
		combined += " " + err.Error()
	}
	return combined
}

// TestBundleVerifyCmd_ConfigKeyReachesKeyVerification proves
// spec.verify.trust.key is plumbed into VerifyOptions.Key. It reuses the
// --key plumbing signal: a nonexistent PEM path drives the key branch to a
// public-key resolution error that the keyless path does not produce.
func TestBundleVerifyCmd_ConfigKeyReachesKeyVerification(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    trust:\n      key: /nonexistent/from-config.pem\n")

	out := runVerify(t, "verify", dir, "--config", cfg)

	// Assert on the config-supplied path name rather than the generic
	// "public key" phrase: if the keyless path ever emits that phrase, a
	// substring check on it would pass vacuously.
	if !strings.Contains(out, "from-config.pem") {
		t.Errorf("config key did not route to the key-verification path; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_MinTrustLevelFlagOverridesConfig covers the override
// branch of stringFlagOrConfig for the policy fields. Config demands
// "verified", which this unsigned fixture cannot reach, while the flag asks for
// "unknown", which it does. The run must therefore not fail the trust check.
func TestBundleVerifyCmd_MinTrustLevelFlagOverridesConfig(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    policy:\n      minTrustLevel: verified\n")

	out := runVerify(t, "verify", dir, "--config", cfg, "--min-trust-level", "unknown")

	if strings.Contains(out, `does not meet minimum "verified"`) {
		t.Errorf("config minTrustLevel won over an explicit --min-trust-level; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_PolicylessConfigKeepsMaxDefault guards against the
// wiring regressing to pass an empty MinTrustLevel. CheckPolicy skips the trust
// check entirely when the field is "", so a fail-open regression would be
// silent. A config with no policy section must leave the flag's "max" default
// intact, which this unsigned fixture fails.
func TestBundleVerifyCmd_PolicylessConfigKeepsMaxDefault(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    trust:\n      trustRoot: \"\"\n")

	out := runVerify(t, "verify", dir, "--config", cfg)

	if !strings.Contains(out, "maximum achievable") {
		t.Errorf("policy-less config did not preserve the \"max\" default floor; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_ConfigKeyOverriddenByFlag locks in the documented
// precedence: an explicit --key wins over spec.verify.trust.key. The config
// points at a path whose name would appear in the error if it were used.
func TestBundleVerifyCmd_ConfigKeyOverriddenByFlag(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    trust:\n      key: /nonexistent/from-config.pem\n")

	out := runVerify(t, "verify", dir, "--config", cfg, "--key", "/nonexistent/from-flag.pem")

	if strings.Contains(out, "from-config.pem") {
		t.Errorf("config key was used despite an explicit --key; got:\n%s", out)
	}
	if !strings.Contains(out, "from-flag.pem") {
		t.Errorf("explicit --key did not reach verification; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_ConfigMinTrustLevelEnforced proves
// spec.verify.policy.minTrustLevel reaches verifier.Policy. The fixture has no
// verifiable attestation, so demanding "verified" must fail the policy check.
//
// The assertion targets the explicit-level branch of CheckPolicy ("does not
// meet minimum") rather than the substring "verified", which the default "max"
// branch also emits via the word "unverified".
func TestBundleVerifyCmd_ConfigMinTrustLevelEnforced(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    policy:\n      minTrustLevel: verified\n")

	out := runVerify(t, "verify", dir, "--config", cfg)

	if !strings.Contains(out, `does not meet minimum "verified"`) {
		t.Errorf("config minTrustLevel was not enforced; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_ConfigRequireCreatorEnforced proves
// spec.verify.policy.requireCreator reaches verifier.Policy: the fixture has no
// bundle creator, so a pinned creator must be reported as unmet.
//
// minTrustLevel is pinned to "unknown" (the level this fixture reaches)
// deliberately. CheckPolicy evaluates the trust floor first and returns on the
// first failure, so under the default "max" the creator check is never reached
// and this test would assert nothing.
func TestBundleVerifyCmd_ConfigRequireCreatorEnforced(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t,
		"    policy:\n      minTrustLevel: unknown\n      requireCreator: ci@example.com\n")

	out := runVerify(t, "verify", dir, "--config", cfg)

	if !strings.Contains(out, "ci@example.com") {
		t.Errorf("config requireCreator was not enforced; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_IgnoreTLogSatisfiedByConfigKey guards the interaction
// between the --insecure-ignore-tlog precondition and a config-supplied key.
// The guard must run against the resolved key, otherwise a perfectly valid
// config-driven offline verify is rejected as if --key were missing.
func TestBundleVerifyCmd_IgnoreTLogSatisfiedByConfigKey(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    trust:\n      key: /nonexistent/from-config.pem\n")

	out := runVerify(t, "verify", dir, "--config", cfg, "--insecure-ignore-tlog")

	if strings.Contains(out, "--insecure-ignore-tlog requires --key") {
		t.Errorf("guard ignored the config-supplied key; got:\n%s", out)
	}
}

// TestBundleVerifyCmd_InvalidConfigRejected proves the config is validated
// before verification runs, so a typo fails with spec-path attribution rather
// than after a full verification pass.
func TestBundleVerifyCmd_InvalidConfigRejected(t *testing.T) {
	dir := writeVerifiableKeyFixture(t)
	cfg := verifyConfig(t, "    policy:\n      minTrustLevel: totally-bogus\n")

	out := runVerify(t, "verify", dir, "--config", cfg)

	if !strings.Contains(out, "spec.verify.policy.minTrustLevel") {
		t.Errorf("invalid minTrustLevel was not rejected with spec-path attribution; got:\n%s", out)
	}
}

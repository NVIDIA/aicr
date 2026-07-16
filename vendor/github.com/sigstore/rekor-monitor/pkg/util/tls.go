//
// Copyright 2026 The Sigstore Authors.
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

package util

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// TLSConfigForCA returns a tls.Config for an HTTP client to connect to a server using the given certificate chain file.
func TLSConfigForCA(chain string) (*tls.Config, error) {
	caCert, err := os.ReadFile(chain)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	return &tls.Config{
		RootCAs:    caCertPool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// ConfigureTrustedCAs configures the root/intermediate CAs for the monitor, by either
// using the configured CAs or, if they were not explicitly defined, using the
// default ones from the TUF data.
func ConfigureTrustedCAs(caRootsFile string, caIntermediatesFile string, trustedRoot *root.TrustedRoot) (string, string, func(), error) {
	if caRootsFile != "" {
		return caRootsFile, caIntermediatesFile, func() {}, nil
	}

	var fulcioRootFile, fulcioIntermediateFile *os.File
	var err error

	closeFiles := func() {
		if fulcioRootFile != nil {
			fulcioRootFile.Close()
		}
		if fulcioIntermediateFile != nil {
			fulcioIntermediateFile.Close()
		}
	}
	cleanupFiles := func() {
		if fulcioRootFile != nil {
			os.Remove(fulcioRootFile.Name())
		}
		if fulcioIntermediateFile != nil {
			os.Remove(fulcioIntermediateFile.Name())
		}
	}

	fulcioRootFile, err = os.CreateTemp("", "fulcio-root-*.pem")
	if err != nil {
		return "", "", func() {}, fmt.Errorf("failed to create temp file for Fulcio CA: %w", err)
	}

	fulcioIntermediateFile, err = os.CreateTemp("", "fulcio-intermediate-*.pem")
	if err != nil {
		closeFiles()
		cleanupFiles()
		return "", "", func() {}, fmt.Errorf("failed to create temp file for Fulcio CA intermediate: %w", err)
	}

	for _, ca := range trustedRoot.FulcioCertificateAuthorities() {
		fulcioCA := ca.(*root.FulcioCertificateAuthority)

		// Get the root certificate from TUF
		if err := pem.Encode(fulcioRootFile, &pem.Block{Type: "CERTIFICATE", Bytes: fulcioCA.Root.Raw}); err != nil {
			closeFiles()
			cleanupFiles()
			return "", "", func() {}, fmt.Errorf("failed to write Fulcio CA root to temp file: %w", err)
		}

		// Get the intermediate certificates from TUF
		for _, intermediate := range fulcioCA.Intermediates {
			if err := pem.Encode(fulcioIntermediateFile, &pem.Block{Type: "CERTIFICATE", Bytes: intermediate.Raw}); err != nil {
				closeFiles()
				cleanupFiles()
				return "", "", func() {}, fmt.Errorf("failed to write Fulcio CA intermediate to temp file: %w", err)
			}
		}
	}
	closeFiles()
	return fulcioRootFile.Name(), fulcioIntermediateFile.Name(), cleanupFiles, nil
}

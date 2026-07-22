// Copyright 2025 The Sigstore Authors.
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

package v2

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sigstore/rekor-tiles/v2/pkg/client"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore/pkg/signature"
	tclient "github.com/transparency-dev/tessera/client"
	"golang.org/x/mod/sumdb/note"
)

// GetCheckpointKeyIDUnverified fetches the latest checkpoint from the server at baseURL
// and extracts the key ID from it.
//
// No verification of the checkpoint is performed, since this function is meant
// to be called before we have a public key to verify against.
func GetCheckpointKeyIDUnverified(ctx context.Context, baseURL *url.URL, userAgent string, tlsConfig *tls.Config) ([]byte, error) {
	transport := http.DefaultTransport
	if tlsConfig != nil {
		transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
	}
	httpClient := &http.Client{
		Transport: client.CreateRoundTripper(transport, userAgent),
	}
	tileClient, err := tclient.NewHTTPFetcher(baseURL, httpClient)
	if err != nil {
		return nil, fmt.Errorf("creating tile client: %v", err)
	}
	cpRaw, err := tileClient.ReadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("error fetching checkpoint: %v", err)
	}

	// The sumdb/note API requires verification to happen while
	// opening/parsing a note. Since we don't have a public key
	// at this point in time, we force the parsing by passing an
	// empty list of verifiers and extracting the (unverified)
	// parsed note from the returned error.
	var checkpointNote *note.Note
	var unverifiedErr *note.UnverifiedNoteError
	_, err = note.Open(cpRaw, note.VerifierList())
	if errors.As(err, &unverifiedErr) {
		checkpointNote = unverifiedErr.Note
	} else {
		return nil, fmt.Errorf("error parsing checkpoint: %v", err)
	}

	if len(checkpointNote.UnverifiedSigs) == 0 {
		return nil, fmt.Errorf("no signatures found in checkpoint: %v", checkpointNote)
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(checkpointNote.UnverifiedSigs[0].Base64)
	if err != nil {
		return nil, fmt.Errorf("error decoding checkpoint signature: %v", err)
	}
	if len(signatureBytes) < 4 {
		return nil, fmt.Errorf("signature too short, expected >=4 bytes: %v", signatureBytes)
	}
	return signatureBytes[:4], nil
}

func GetLogVerifier(ctx context.Context, baseURL *url.URL, trustedRoot root.TrustedMaterial, userAgent string, tlsConfig *tls.Config) (signature.Verifier, error) {
	checkpointKeyID, err := GetCheckpointKeyIDUnverified(ctx, baseURL, userAgent, tlsConfig)
	if err != nil {
		return nil, err
	}

	var matchingLogInstance *root.TransparencyLog
	rekorLogs := trustedRoot.RekorLogs()
	for k, v := range rekorLogs {
		logID, err := hex.DecodeString(k)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(logID[:4], checkpointKeyID) {
			matchingLogInstance = v
		}
	}

	if matchingLogInstance == nil {
		return nil, fmt.Errorf("couldn't find matching log instance with baseURL %v", baseURL)
	}

	verifier, err := signature.LoadVerifier(matchingLogInstance.PublicKey, matchingLogInstance.HashFunc)
	if err != nil {
		return nil, err
	}
	return verifier, nil
}

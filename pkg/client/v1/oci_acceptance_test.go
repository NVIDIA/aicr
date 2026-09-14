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

package aicr_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	stderrors "errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
)

const (
	ociAcceptanceChildEnv     = "AICR_OCI_ACCEPTANCE_CHILD"
	ociAcceptanceConfigEnv    = "AICR_OCI_ACCEPTANCE_CONFIG"
	ociAcceptanceArtifactType = "application/vnd.nvidia.aicr.artifact"
	ociAcceptanceSentinel     = "caller-owned.txt"
	ociAcceptanceRepository   = "acceptance/recipes"
	ociAcceptanceBlockTimeout = 2 * time.Second
)

type ociAcceptanceConfig struct {
	Registry        string   `json:"registry"`
	Repository      string   `json:"repository"`
	TempParent      string   `json:"tempParent"`
	CAPEM           []byte   `json:"caPEM"`
	FirstDigest     string   `json:"firstDigest"`
	SecondDigest    string   `json:"secondDigest"`
	FrozenTag       string   `json:"frozenTag"`
	FrozenDigest    string   `json:"frozenDigest"`
	BlockedTag      string   `json:"blockedTag"`
	UnauthorizedTag string   `json:"unauthorizedTag"`
	HostileDigests  []string `json:"hostileDigests"`
}

// TestOCIRecipeSourceAcceptance runs its real OCI client half in a fresh copy
// of this race-instrumented test binary. The child installs the generated CA as
// its fallback root before constructing a client. This keeps the public
// NewClient path unchanged while the parent retains an in-memory HTTPS registry
// and can inspect its request counts after the child exits.
func TestOCIRecipeSourceAcceptance(t *testing.T) {
	if os.Getenv(ociAcceptanceChildEnv) == "1" {
		runOCIRecipeSourceAcceptanceChild(t)
		return
	}
	if os.Getenv(ociAcceptanceChildEnv) != "" {
		t.Fatalf("%s has unsupported value %q", ociAcceptanceChildEnv,
			os.Getenv(ociAcceptanceChildEnv))
	}

	registry := newOCIInMemoryRegistry(t)
	repository := ociAcceptanceRepository
	tempParent := t.TempDir()
	writeAcceptanceFile(t, filepath.Join(tempParent, ociAcceptanceSentinel), []byte("keep"))

	first := packageAndPushAcceptanceRecipe(t, registry, repository, "first", "first")
	second := packageAndPushAcceptanceRecipe(t, registry, repository, "second", "second")
	frozenFirst := registry.addArtifact(
		t, "frozen-a", buildValidOCIRecipeArchive(t, "frozen-a"))
	frozenSecond := registry.addArtifact(
		t, "frozen-b", buildValidOCIRecipeArchive(t, "frozen-b"))
	registry.moveTag(repository, "frozen", frozenFirst.descriptor.Digest)
	registry.failBlobOnceAndMoveTag(
		frozenFirst.layer.Digest, repository, "frozen", frozenSecond.descriptor.Digest)

	manifestTamper := registry.addArtifact(
		t, "manifest-tamper", buildValidOCIRecipeArchive(t, "manifest-tamper"))
	registry.tamperManifest(manifestTamper.descriptor.Digest)
	layerTamper := registry.addArtifact(
		t, "layer-tamper", buildValidOCIRecipeArchive(t, "layer-tamper"))
	registry.tamperBlob(layerTamper.layer.Digest)
	selectorMismatch := registry.addArtifact(
		t, "selector-mismatch", buildValidOCIRecipeArchive(t, "selector-mismatch"))
	registry.mismatchManifestDigest(selectorMismatch.descriptor.Digest, frozenSecond.descriptor.Digest)

	traversal := registry.addArtifact(t, "traversal",
		buildOCIRecipeArchive(t, []ociArchiveEntry{
			{name: "registry.yaml", body: []byte(acceptanceRegistryYAML)},
			{name: "../escape", body: []byte("escape")},
		}, 0))
	symlink := registry.addArtifact(t, "symlink",
		buildOCIRecipeArchive(t, []ociArchiveEntry{
			{name: "registry.yaml", body: []byte(acceptanceRegistryYAML)},
			{name: "link", typeflag: tar.TypeSymlink, linkname: "/tmp"},
		}, 0))
	expanded := registry.addArtifact(t, "expanded",
		buildOCIRecipeArchive(t, []ociArchiveEntry{
			{name: "registry.yaml", body: []byte(acceptanceRegistryYAML)},
		}, defaults.MaxOCIRecipeExtractedBytes+1))

	registry.blockManifest(repository, "blocked")
	registry.rejectManifest(repository, "unauthorized", http.StatusUnauthorized)

	config := ociAcceptanceConfig{
		Registry:        registry.host,
		Repository:      repository,
		TempParent:      tempParent,
		CAPEM:           registry.caPEM,
		FirstDigest:     first.Digest,
		SecondDigest:    second.Digest,
		FrozenTag:       "frozen",
		FrozenDigest:    frozenFirst.descriptor.Digest.String(),
		BlockedTag:      "blocked",
		UnauthorizedTag: "unauthorized",
		HostileDigests: []string{
			manifestTamper.descriptor.Digest.String(),
			layerTamper.descriptor.Digest.String(),
			selectorMismatch.descriptor.Digest.String(),
			traversal.descriptor.Digest.String(),
			symlink.descriptor.Digest.String(),
			expanded.descriptor.Digest.String(),
		},
	}
	configPath := filepath.Join(t.TempDir(), "acceptance.json")
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal child config: %v", err)
	}
	writeAcceptanceFile(t, configPath, configBytes)

	childCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0],
		"-test.run=^TestOCIRecipeSourceAcceptance$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(),
		ociAcceptanceChildEnv+"=1",
		ociAcceptanceConfigEnv+"="+configPath,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"HTTP_PROXY=http://127.0.0.1:1",
		"ALL_PROXY=http://127.0.0.1:1",
	)
	output, childErr := cmd.CombinedOutput()
	if childCtx.Err() != nil {
		t.Fatalf("acceptance child exceeded deadline: %v\n%s", childCtx.Err(), output)
	}
	if childErr != nil {
		t.Fatalf("acceptance child failed: %v\n%s", childErr, output)
	}

	if got := registry.manifestRequestCount(repository, "frozen"); got != 1 {
		t.Errorf("frozen tag requests = %d, want 1", got)
	}
	if got := registry.blobGETCount(frozenFirst.layer.Digest); got != 2 {
		t.Errorf("frozen layer GET requests = %d, want 2", got)
	}
	if got := registry.manifestRequestCount(repository, "unauthorized"); got != 1 {
		t.Errorf("unauthorized manifest requests = %d, want 1", got)
	}
	if got := registry.manifestRequestCount(repository, "blocked"); got != 1 {
		t.Errorf("blocked manifest requests = %d, want 1", got)
	}
}

func runOCIRecipeSourceAcceptanceChild(t *testing.T) {
	configPath := os.Getenv(ociAcceptanceConfigEnv)
	if configPath == "" {
		t.Fatalf("%s is required in child mode", ociAcceptanceConfigEnv)
	}
	configFile, err := os.Open(configPath)
	if err != nil {
		t.Fatalf("open acceptance config: %v", err)
	}
	var config ociAcceptanceConfig
	decoder := json.NewDecoder(io.LimitReader(configFile, defaults.MaxOCIRecipeManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		_ = configFile.Close()
		t.Fatalf("decode acceptance config: %v", err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatalf("close acceptance config: %v", err)
	}
	installAcceptanceFallbackRoots(t, config.CAPEM)
	repository := config.Registry + "/" + config.Repository

	t.Run("immutable lifecycle and isolation", func(t *testing.T) {
		first, err := aicr.NewClient(
			aicr.WithRecipeSource(aicr.OCISource(repository, config.FirstDigest)),
			aicr.WithOCISourceTempDir(config.TempParent),
		)
		if err != nil {
			t.Fatalf("NewClient(first) error = %v", err)
		}
		second, err := aicr.NewClient(
			aicr.WithRecipeSource(aicr.OCISource(repository, config.SecondDigest)),
			aicr.WithOCISourceTempDir(config.TempParent),
		)
		if err != nil {
			_ = first.Close()
			t.Fatalf("NewClient(second) error = %v", err)
		}

		assertAcceptanceParentEntries(t, config.TempParent, 3)
		firstRecipe := resolveAcceptanceRecipe(t, first)
		secondRecipe := resolveAcceptanceRecipe(t, second)
		assertAcceptanceMarker(t, firstRecipe, "first")
		assertAcceptanceMarker(t, secondRecipe, "second")

		start := make(chan struct{})
		var operations sync.WaitGroup
		operations.Add(24)
		for i := range 24 {
			go func(index int) {
				defer operations.Done()
				<-start
				if index%2 == 0 {
					_, _ = first.ResolveRecipe(t.Context(), acceptanceRecipeRequest())
					return
				}
				_, _ = aicr.SelectFromRecipeWithContext(
					t.Context(), firstRecipe, "components.gpu-operator.values.acceptanceMarker")
			}(i)
		}
		close(start)
		if err := first.Close(); err != nil {
			t.Fatalf("first Close() error = %v", err)
		}
		operations.Wait()
		if err := first.Close(); err != nil {
			t.Fatalf("repeated first Close() error = %v", err)
		}
		assertAcceptanceParentEntries(t, config.TempParent, 2)
		assertAcceptanceMarker(t, secondRecipe, "second")

		if err := second.Close(); err != nil {
			t.Fatalf("second Close() error = %v", err)
		}
		if err := second.Close(); err != nil {
			t.Fatalf("repeated second Close() error = %v", err)
		}
		assertAcceptanceParentEntries(t, config.TempParent, 1)
	})

	t.Run("tag is frozen across transient retry", func(t *testing.T) {
		staged, err := oci.StageRecipeArtifact(t.Context(), oci.RecipePullOptions{
			Repository: repository,
			Selector:   config.FrozenTag,
			TempDir:    config.TempParent,
		})
		if err != nil {
			t.Fatalf("StageRecipeArtifact() error = %v", err)
		}
		if got := staged.Descriptor().Digest.String(); got != config.FrozenDigest {
			t.Errorf("frozen digest = %q, want %q", got, config.FrozenDigest)
		}
		if got := staged.Reference(); got != repository+"@"+config.FrozenDigest {
			t.Errorf("frozen reference = %q", got)
		}
		if err := staged.Close(); err != nil {
			t.Fatalf("staged Close() error = %v", err)
		}
		assertAcceptanceParentEntries(t, config.TempParent, 1)
	})

	t.Run("cancellation and terminal classification clean workspaces", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), ociAcceptanceBlockTimeout)
		defer cancel()
		staged, err := oci.StageRecipeArtifact(ctx, oci.RecipePullOptions{
			Repository: repository,
			Selector:   config.BlockedTag,
			TempDir:    config.TempParent,
		})
		if staged != nil {
			_ = staged.Close()
			t.Fatal("blocked staging returned an artifact")
		}
		assertAcceptanceErrorCode(t, err, apperrors.ErrCodeTimeout)
		assertAcceptanceParentEntries(t, config.TempParent, 1)

		staged, err = oci.StageRecipeArtifact(t.Context(), oci.RecipePullOptions{
			Repository: repository,
			Selector:   config.UnauthorizedTag,
			TempDir:    config.TempParent,
		})
		if staged != nil {
			_ = staged.Close()
			t.Fatal("unauthorized staging returned an artifact")
		}
		assertAcceptanceErrorCode(t, err, apperrors.ErrCodeUnauthorized)
		assertAcceptanceParentEntries(t, config.TempParent, 1)
	})

	hostileNames := []string{
		"manifest digest mismatch",
		"layer digest mismatch",
		"selector digest mismatch",
		"archive traversal",
		"archive symlink",
		"expanded stream limit",
	}
	if len(config.HostileDigests) != len(hostileNames) {
		t.Fatalf("hostile selectors = %d, want %d", len(config.HostileDigests), len(hostileNames))
	}
	for i, name := range hostileNames {
		t.Run(name, func(t *testing.T) {
			client, err := aicr.NewClient(
				aicr.WithRecipeSource(aicr.OCISource(repository, config.HostileDigests[i])),
				aicr.WithOCISourceTempDir(config.TempParent),
			)
			if client != nil {
				_ = client.Close()
				t.Fatal("hostile artifact constructed a Client")
			}
			assertAcceptanceErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			assertAcceptanceParentEntries(t, config.TempParent, 1)
		})
	}
}

func installAcceptanceFallbackRoots(t *testing.T, caPEM []byte) {
	t.Helper()
	roots := x509.NewCertPool()
	if ok := roots.AppendCertsFromPEM(caPEM); !ok {
		t.Fatal("acceptance config does not contain a valid CA certificate")
	}
	godebug := os.Getenv("GODEBUG")
	if godebug != "" {
		godebug += ","
	}
	t.Setenv("GODEBUG", godebug+"x509usefallbackroots=1")
	x509.SetFallbackRoots(roots)
}

func acceptanceRecipeRequest() aicr.RecipeRequest {
	return aicr.RecipeRequest{
		Service: "eks", Accelerator: "h100", Intent: "training", OS: "ubuntu", Platform: "slurm",
	}
}

func resolveAcceptanceRecipe(t *testing.T, client *aicr.Client) *aicr.RecipeResult {
	t.Helper()
	result, err := client.ResolveRecipe(t.Context(), acceptanceRecipeRequest())
	if err != nil {
		t.Fatalf("ResolveRecipe() error = %v", err)
	}
	return result
}

func assertAcceptanceMarker(t *testing.T, result *aicr.RecipeResult, want string) {
	t.Helper()
	got, err := aicr.SelectFromRecipeWithContext(
		t.Context(), result, "components.gpu-operator.values.acceptanceMarker")
	if err != nil {
		t.Fatalf("select acceptance marker: %v", err)
	}
	if got != want {
		t.Fatalf("acceptance marker = %#v, want %q", got, want)
	}
}

func assertAcceptanceErrorCode(t *testing.T, err error, code apperrors.ErrorCode) {
	t.Helper()
	if !stderrors.Is(err, apperrors.New(code, "")) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func assertAcceptanceParentEntries(t *testing.T, parent string, want int) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", parent, err)
	}
	if len(entries) != want {
		t.Fatalf("temp parent entries = %v, want %d", entryNames(entries), want)
	}
	if data, err := os.ReadFile(filepath.Join(parent, ociAcceptanceSentinel)); err != nil || string(data) != "keep" {
		t.Fatalf("caller sentinel = (%q, %v), want keep,nil", data, err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = entries[i].Name()
	}
	return names
}

const acceptanceRegistryYAML = `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`

func packageAndPushAcceptanceRecipe(
	t *testing.T,
	registry *ociInMemoryRegistry,
	repository, tag, marker string,
) *oci.PackageResult {

	t.Helper()
	source := t.TempDir()
	writeAcceptanceFile(t, filepath.Join(source, "registry.yaml"), []byte(acceptanceRegistryYAML))
	writeAcceptanceFile(t, filepath.Join(source, "components", "gpu-operator", "values.yaml"),
		[]byte("acceptanceMarker: "+marker+"\n"))
	result, err := oci.Package(t.Context(), oci.PackageOptions{
		SourceDir: source, OutputDir: t.TempDir(), Registry: registry.host,
		Repository: repository, Tag: tag,
	})
	if err != nil {
		t.Fatalf("Package(%s) error = %v", tag, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(result.StorePath); err != nil {
			t.Errorf("remove package layout: %v", err)
		}
	})
	if _, err := oci.PushFromStore(t.Context(), result.StorePath, result.Descriptor, oci.PushOptions{
		Registry: registry.host, Repository: repository, Tag: tag, InsecureTLS: true,
	}); err != nil {
		t.Fatalf("PushFromStore(%s) error = %v", tag, err)
	}
	return result
}

type ociArchiveEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func buildValidOCIRecipeArchive(t *testing.T, identity string) []byte {
	t.Helper()
	return buildOCIRecipeArchive(t, []ociArchiveEntry{
		{name: "registry.yaml", body: []byte(acceptanceRegistryYAML)},
		{name: "identity", body: []byte(identity)},
	}, 0)
}

// buildOCIRecipeArchive streams any expanded tail through gzip in fixed-size
// chunks; even the >128 MiB case retains only the compressed bytes and a 32 KiB
// scratch buffer in memory.
func buildOCIRecipeArchive(t *testing.T, entries []ociArchiveEntry, expandedTail int64) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name: entry.name, Mode: 0o600, Typeflag: typeflag, Linkname: entry.linkname,
		}
		if typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if len(entry.body) != 0 {
			if _, err := tarWriter.Write(entry.body); err != nil {
				t.Fatalf("write tar body: %v", err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	zeros := make([]byte, 32*1024)
	for expandedTail > 0 {
		chunk := min(int64(len(zeros)), expandedTail)
		if _, err := gzipWriter.Write(zeros[:chunk]); err != nil {
			t.Fatalf("write expanded tail: %v", err)
		}
		expandedTail -= chunk
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return output.Bytes()
}

func writeAcceptanceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

type registryArtifact struct {
	descriptor ociv1.Descriptor
	layer      ociv1.Descriptor
}

type registryUpload struct {
	repository string
	data       []byte
}

type ociInMemoryRegistry struct {
	t      *testing.T
	host   string
	caPEM  []byte
	server *httptest.Server

	mu                sync.Mutex
	blobs             map[digest.Digest][]byte
	manifests         map[string]map[digest.Digest][]byte
	tags              map[string]map[string]digest.Digest
	uploads           map[string]registryUpload
	nextUpload        int
	manifestRequests  map[string]int
	blobGETs          map[digest.Digest]int
	tamperedManifests map[digest.Digest]bool
	tamperedBlobs     map[digest.Digest]bool
	mismatchedHeaders map[digest.Digest]digest.Digest
	blockedManifests  map[string]bool
	rejectedManifests map[string]int
	failBlobOnce      map[digest.Digest]func()
}

func newOCIInMemoryRegistry(t *testing.T) *ociInMemoryRegistry {
	t.Helper()
	registry := &ociInMemoryRegistry{
		t:                 t,
		blobs:             make(map[digest.Digest][]byte),
		manifests:         make(map[string]map[digest.Digest][]byte),
		tags:              make(map[string]map[string]digest.Digest),
		uploads:           make(map[string]registryUpload),
		manifestRequests:  make(map[string]int),
		blobGETs:          make(map[digest.Digest]int),
		tamperedManifests: make(map[digest.Digest]bool),
		tamperedBlobs:     make(map[digest.Digest]bool),
		mismatchedHeaders: make(map[digest.Digest]digest.Digest),
		blockedManifests:  make(map[string]bool),
		rejectedManifests: make(map[string]int),
		failBlobOnce:      make(map[digest.Digest]func()),
	}

	certificate, caPEM := newAcceptanceTLSCertificate(t)
	server := httptest.NewUnstartedServer(registry)
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}
	server.StartTLS()
	registry.server = server
	registry.host = server.Listener.Addr().String()
	registry.caPEM = caPEM
	t.Cleanup(server.Close)
	return registry
}

func newAcceptanceTLSCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate registry CA key: %v", err)
	}
	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate registry CA serial: %v", err)
	}
	ca := &x509.Certificate{
		SerialNumber: caSerial,
		Subject:      pkix.Name{CommonName: "AICR OCI acceptance CA"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create registry CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse registry CA: %v", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate registry server key: %v", err)
	}
	serverSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate registry server serial: %v", err)
	}
	serverCert := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(
		rand.Reader, serverCert, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create registry server certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal registry server key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load registry server key pair: %v", err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func (r *ociInMemoryRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/v2" || req.URL.Path == "/v2/" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}
	if repository, tail, ok := splitOCIRegistryPath(req.URL.Path, "/blobs/uploads/"); ok {
		r.serveUpload(w, req, repository, tail)
		return
	}
	if repository, reference, ok := splitOCIRegistryPath(req.URL.Path, "/manifests/"); ok {
		r.serveManifest(w, req, repository, reference)
		return
	}
	if repository, reference, ok := splitOCIRegistryPath(req.URL.Path, "/referrers/"); ok {
		r.serveReferrers(w, req, repository, reference)
		return
	}
	if _, reference, ok := splitOCIRegistryPath(req.URL.Path, "/blobs/"); ok {
		r.serveBlob(w, req, reference)
		return
	}
	writeOCIRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "route not found")
}

func splitOCIRegistryPath(requestPath, marker string) (string, string, bool) {
	trimmed := strings.TrimPrefix(requestPath, "/v2/")
	if trimmed == requestPath {
		return "", "", false
	}
	index := strings.Index(trimmed, marker)
	if index <= 0 {
		return "", "", false
	}
	return trimmed[:index], trimmed[index+len(marker):], true
}

func (r *ociInMemoryRegistry) serveUpload(
	w http.ResponseWriter,
	req *http.Request,
	repository, session string,
) {

	switch req.Method {
	case http.MethodPost:
		r.mu.Lock()
		r.nextUpload++
		session = strconv.Itoa(r.nextUpload)
		r.uploads[session] = registryUpload{repository: repository}
		r.mu.Unlock()
		w.Header().Set("Location", req.URL.Path+session)
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut, http.MethodPatch:
		body, ok := readOCIRegistryBody(w, req, defaults.MaxOCIRecipeLayerBytes)
		if !ok {
			return
		}
		r.mu.Lock()
		upload, exists := r.uploads[session]
		if !exists || upload.repository != repository {
			r.mu.Unlock()
			writeOCIRegistryError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload missing")
			return
		}
		if int64(len(upload.data)+len(body)) > defaults.MaxOCIRecipeLayerBytes {
			r.mu.Unlock()
			writeOCIRegistryError(w, http.StatusRequestEntityTooLarge, "BLOB_UPLOAD_INVALID", "upload too large")
			return
		}
		upload.data = append(upload.data, body...)
		r.uploads[session] = upload
		if req.Method == http.MethodPatch {
			r.mu.Unlock()
			w.Header().Set("Location", req.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		got := digest.FromBytes(upload.data)
		want := digest.Digest(req.URL.Query().Get("digest"))
		if want != got {
			r.mu.Unlock()
			writeOCIRegistryError(w, http.StatusBadRequest, "DIGEST_INVALID", "blob digest mismatch")
			return
		}
		r.blobs[got] = append([]byte(nil), upload.data...)
		delete(r.uploads, session)
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", got.String())
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *ociInMemoryRegistry) serveBlob(w http.ResponseWriter, req *http.Request, reference string) {
	desc := digest.Digest(reference)
	r.mu.Lock()
	data, ok := r.blobs[desc]
	if req.Method == http.MethodGet {
		r.blobGETs[desc]++
		if callback := r.failBlobOnce[desc]; callback != nil {
			delete(r.failBlobOnce, desc)
			callback()
			r.mu.Unlock()
			writeOCIRegistryError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "retry blob")
			return
		}
	}
	tampered := r.tamperedBlobs[desc]
	r.mu.Unlock()
	if !ok {
		writeOCIRegistryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
		return
	}
	response := append([]byte(nil), data...)
	if tampered && len(response) != 0 {
		response[0] ^= 0xff
	}
	w.Header().Set("Docker-Content-Digest", desc.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(response)))
	switch req.Method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		_, _ = w.Write(response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *ociInMemoryRegistry) serveManifest(
	w http.ResponseWriter,
	req *http.Request,
	repository, reference string,
) {

	requestKey := repository + "\x00" + reference
	r.mu.Lock()
	r.manifestRequests[requestKey]++
	blocked := r.blockedManifests[requestKey]
	rejected := r.rejectedManifests[requestKey]
	r.mu.Unlock()
	if blocked {
		<-req.Context().Done()
		return
	}
	if rejected != 0 {
		writeOCIRegistryError(w, rejected, "UNAUTHORIZED", "request rejected")
		return
	}
	if req.Method == http.MethodPut {
		r.putManifest(w, req, repository, reference)
		return
	}
	if req.Method != http.MethodHead && req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	r.mu.Lock()
	manifestDigest, data, ok := r.resolveManifestLocked(repository, reference)
	tampered := r.tamperedManifests[manifestDigest]
	headerDigest := manifestDigest
	if mismatch := r.mismatchedHeaders[manifestDigest]; mismatch != "" {
		headerDigest = mismatch
	}
	r.mu.Unlock()
	if !ok {
		writeOCIRegistryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found")
		return
	}
	response := append([]byte(nil), data...)
	if tampered && len(response) != 0 {
		response[0] ^= 0xff
	}
	w.Header().Set("Content-Type", ociv1.MediaTypeImageManifest)
	w.Header().Set("Content-Length", strconv.Itoa(len(response)))
	w.Header().Set("Docker-Content-Digest", headerDigest.String())
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(response)
}

func (r *ociInMemoryRegistry) putManifest(
	w http.ResponseWriter,
	req *http.Request,
	repository, reference string,
) {

	data, ok := readOCIRegistryBody(w, req, defaults.MaxOCIRecipeManifestBytes)
	if !ok {
		return
	}
	desc := digest.FromBytes(data)
	if strings.Contains(reference, ":") && digest.Digest(reference) != desc {
		writeOCIRegistryError(w, http.StatusBadRequest, "DIGEST_INVALID", "manifest digest mismatch")
		return
	}
	var manifest ociv1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		writeOCIRegistryError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid manifest")
		return
	}
	r.mu.Lock()
	r.storeManifestLocked(repository, reference, desc, data)
	r.mu.Unlock()
	if manifest.Subject != nil {
		w.Header().Set("OCI-Subject", manifest.Subject.Digest.String())
	}
	w.Header().Set("Docker-Content-Digest", desc.String())
	w.WriteHeader(http.StatusCreated)
}

func (r *ociInMemoryRegistry) serveReferrers(
	w http.ResponseWriter,
	req *http.Request,
	repository, reference string,
) {

	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	subjectDigest := digest.Digest(reference)
	artifactType := req.URL.Query().Get("artifactType")
	r.mu.Lock()
	manifests := make([]ociv1.Descriptor, 0)
	for manifestDigest, data := range r.manifests[repository] {
		var manifest ociv1.Manifest
		if json.Unmarshal(data, &manifest) != nil || manifest.Subject == nil ||
			manifest.Subject.Digest != subjectDigest ||
			artifactType != "" && manifest.ArtifactType != artifactType {

			continue
		}
		manifests = append(manifests, ociv1.Descriptor{
			MediaType:    ociv1.MediaTypeImageManifest,
			Digest:       manifestDigest,
			Size:         int64(len(data)),
			ArtifactType: manifest.ArtifactType,
		})
	}
	r.mu.Unlock()
	index := ociv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ociv1.MediaTypeImageIndex,
		Manifests: manifests,
	}
	data, err := json.Marshal(index)
	if err != nil {
		writeOCIRegistryError(
			w, http.StatusInternalServerError, "UNKNOWN", "marshal referrers index failed")
		return
	}
	w.Header().Set("Content-Type", ociv1.MediaTypeImageIndex)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func readOCIRegistryBody(
	w http.ResponseWriter,
	req *http.Request,
	limit int64,
) ([]byte, bool) {

	if req.ContentLength > limit {
		writeOCIRegistryError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "body too large")
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil {
		writeOCIRegistryError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "body read failed")
		return nil, false
	}
	if int64(len(data)) > limit {
		writeOCIRegistryError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "body too large")
		return nil, false
	}
	return data, true
}

func writeOCIRegistryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

func (r *ociInMemoryRegistry) addArtifact(
	t *testing.T,
	tag string,
	layerBytes []byte,
) registryArtifact {

	t.Helper()
	layer := ociv1.Descriptor{
		MediaType: ociv1.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(layerBytes),
		Size:      int64(len(layerBytes)),
	}
	manifest := ociv1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ociv1.MediaTypeImageManifest,
		ArtifactType: ociAcceptanceArtifactType,
		Config:       ociv1.DescriptorEmptyJSON,
		Layers:       []ociv1.Descriptor{layer},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal direct artifact: %v", err)
	}
	descriptor := ociv1.Descriptor{
		MediaType: ociv1.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
	}
	r.mu.Lock()
	r.blobs[ociv1.DescriptorEmptyJSON.Digest] =
		append([]byte(nil), ociv1.DescriptorEmptyJSON.Data...)
	r.blobs[layer.Digest] = append([]byte(nil), layerBytes...)
	r.storeManifestLocked(ociAcceptanceRepository, tag, descriptor.Digest, manifestBytes)
	r.mu.Unlock()
	return registryArtifact{descriptor: descriptor, layer: layer}
}

func (r *ociInMemoryRegistry) storeManifestLocked(
	repository, reference string,
	manifestDigest digest.Digest,
	data []byte,
) {

	if r.manifests[repository] == nil {
		r.manifests[repository] = make(map[digest.Digest][]byte)
	}
	if r.tags[repository] == nil {
		r.tags[repository] = make(map[string]digest.Digest)
	}
	r.manifests[repository][manifestDigest] = append([]byte(nil), data...)
	r.tags[repository][reference] = manifestDigest
	r.tags[repository][manifestDigest.String()] = manifestDigest
}

func (r *ociInMemoryRegistry) resolveManifestLocked(
	repository, reference string,
) (digest.Digest, []byte, bool) {

	manifestDigest := r.tags[repository][reference]
	if manifestDigest == "" {
		manifestDigest = digest.Digest(reference)
	}
	data, ok := r.manifests[repository][manifestDigest]
	return manifestDigest, data, ok
}

func (r *ociInMemoryRegistry) moveTag(
	repository, tag string,
	manifestDigest digest.Digest,
) {

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tags[repository] == nil {
		r.tags[repository] = make(map[string]digest.Digest)
	}
	r.tags[repository][tag] = manifestDigest
}

func (r *ociInMemoryRegistry) failBlobOnceAndMoveTag(
	blobDigest digest.Digest,
	repository, tag string,
	manifestDigest digest.Digest,
) {

	r.mu.Lock()
	defer r.mu.Unlock()
	r.failBlobOnce[blobDigest] = func() {
		if r.tags[repository] == nil {
			r.tags[repository] = make(map[string]digest.Digest)
		}
		r.tags[repository][tag] = manifestDigest
	}
}

func (r *ociInMemoryRegistry) tamperManifest(manifestDigest digest.Digest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tamperedManifests[manifestDigest] = true
}

func (r *ociInMemoryRegistry) tamperBlob(blobDigest digest.Digest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tamperedBlobs[blobDigest] = true
}

func (r *ociInMemoryRegistry) mismatchManifestDigest(
	manifestDigest, responseDigest digest.Digest,
) {

	r.mu.Lock()
	defer r.mu.Unlock()
	r.mismatchedHeaders[manifestDigest] = responseDigest
}

func (r *ociInMemoryRegistry) blockManifest(repository, reference string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockedManifests[repository+"\x00"+reference] = true
}

func (r *ociInMemoryRegistry) rejectManifest(repository, reference string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectedManifests[repository+"\x00"+reference] = status
}

func (r *ociInMemoryRegistry) manifestRequestCount(repository, reference string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manifestRequests[repository+"\x00"+reference]
}

func (r *ociInMemoryRegistry) blobGETCount(blobDigest digest.Digest) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.blobGETs[blobDigest]
}

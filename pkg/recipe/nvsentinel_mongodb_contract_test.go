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

package recipe

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The names the step mixins, nvsentinel-mongodb's values and its manifests
// must agree on. Each side is read from its own file; nothing here is a copy
// of the value under test except these expectations.
const (
	contractAppDN      = "CN=mongo-user-client,OU=DGXC,O=Nvidia,L=SantaClara,ST=California,C=US"
	contractURISecret  = "nvsentinel-mongodb-uri"
	contractClientCert = "nvsentinel-mongodb-app-client"
	contractDatabase   = "HealthEventsDatabase"
	contractTTLRole    = "nvsentinelTTLIndex"
)

func readComponentYAML(t *testing.T, path string, out any) {
	t.Helper()
	data, err := defaultEmbeddedProvider.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	// Manifests are Helm templates; blank the directives before parsing.
	body := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(string(data), "NS")
	if err := yaml.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}

// assertPerconaDatastore checks the global.datastore block a step sets.
func assertPerconaDatastore(t *testing.T, mixin string, global map[string]any) {
	t.Helper()
	if global["certificateRotationEnabled"] != true {
		t.Errorf("%s: global.certificateRotationEnabled = %v, want true", mixin, global["certificateRotationEnabled"])
	}
	ds, _ := global["datastore"].(map[string]any)
	creds, _ := ds["credentialsFromSecret"].(map[string]any)
	conn, _ := ds["connection"].(map[string]any)
	tls, _ := ds["tls"].(map[string]any)
	auth, _ := ds["auth"].(map[string]any)
	want := []struct {
		path string
		got  any
		want any
	}{
		{"datastore.provider", ds["provider"], "mongodb"},
		{"datastore.credentialsFromSecret.name", creds["name"], contractURISecret},
		{"datastore.connection.database", conn["database"], contractDatabase},
		{"datastore.tls.enabled", tls["enabled"], true},
		{"datastore.auth.mechanism", auth["mechanism"], "x509"},
		{"datastore.auth.clientCertSecretName", auth["clientCertSecretName"], contractClientCert},
		{"datastore.auth.x509ApplicationUserDN", auth["x509ApplicationUserDN"], contractAppDN},
		// Equal to the app DN so the setup Job's lookup finds the user the
		// operator created instead of trying to create a second one.
		{"datastore.auth.x509DgxcopsUserDN", auth["x509DgxcopsUserDN"], contractAppDN},
	}
	for _, w := range want {
		if w.got != w.want {
			t.Errorf("%s: global.%s = %v, want %v", mixin, w.path, w.got, w.want)
		}
	}
}

// assertPerconaComponents checks that a step adds both Percona components
// and orders nvsentinel after the datastore.
func assertPerconaComponents(t *testing.T, spec RecipeMetadataSpec, nvsentinel ComponentRef) {
	t.Helper()
	if !slices.Contains(nvsentinel.DependencyRefs, "nvsentinel-mongodb") {
		t.Errorf("nvsentinel dependencyRefs = %v, want nvsentinel-mongodb", nvsentinel.DependencyRefs)
	}
	op, ok := findComponentRefByName(spec.ComponentRefs, "psmdb-operator")
	if !ok || op.ValuesFile != "components/psmdb-operator/values.yaml" {
		t.Errorf("psmdb-operator componentRef = %+v, want it with its values file", op)
	}
	db, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel-mongodb")
	if !ok || db.ValuesFile != "components/nvsentinel-mongodb/values.yaml" {
		t.Errorf("nvsentinel-mongodb componentRef = %+v, want it with its values file", db)
	}
	for _, dep := range []string{"psmdb-operator", "cert-manager"} {
		if !slices.Contains(db.DependencyRefs, dep) {
			t.Errorf("nvsentinel-mongodb dependencyRefs = %v, want %s", db.DependencyRefs, dep)
		}
	}
}

// TestNVSentinelMongoDBContract pins every name the Percona datastore and
// NVSentinel share across files, so a rename on one side fails here instead
// of as an authentication or connection error at runtime.
func TestNVSentinelMongoDBContract(t *testing.T) {
	var db struct {
		FullnameOverride string `yaml:"fullnameOverride"`
		CRVersion        string `yaml:"crVersion"`
		Image            struct{ Repository, Tag string }
		InitImage        struct{ Repository, Tag string } `yaml:"initImage"`
		Users            []struct {
			Name  string
			DB    string                      `yaml:"db"`
			Roles []struct{ Name, DB string } `yaml:"roles"`
		}
		CustomRoles []struct {
			Role       string
			DB         string `yaml:"db"`
			Privileges []struct {
				Resource struct{ DB, Collection string } `yaml:"resource"`
				Actions  []string                        `yaml:"actions"`
			} `yaml:"privileges"`
		} `yaml:"roles"`
		Replsets map[string]struct {
			Name string
			Size int
		}
	}
	readComponentYAML(t, "components/nvsentinel-mongodb/values.yaml", &db)
	var op struct {
		Image            struct{ Repository, Tag string }
		DisableTelemetry bool `yaml:"disableTelemetry"`
	}
	readComponentYAML(t, "components/psmdb-operator/values.yaml", &op)

	var cert struct {
		Spec struct {
			SecretName string `yaml:"secretName"`
			CommonName string `yaml:"commonName"`
			Subject    struct {
				Organizations       []string `yaml:"organizations"`
				OrganizationalUnits []string `yaml:"organizationalUnits"`
				Localities          []string `yaml:"localities"`
				Provinces           []string `yaml:"provinces"`
				Countries           []string `yaml:"countries"`
			}
			IssuerRef struct{ Name string } `yaml:"issuerRef"`
		}
	}
	readComponentYAML(t, "components/nvsentinel-mongodb/manifests/client-certificate.yaml", &cert)
	var conn struct {
		Metadata   struct{ Name string }
		StringData map[string]string `yaml:"stringData"`
	}
	readComponentYAML(t, "components/nvsentinel-mongodb/manifests/connection.yaml", &conn)

	s := cert.Spec.Subject
	if len(s.Organizations) != 1 || len(s.OrganizationalUnits) != 1 || len(s.Localities) != 1 || len(s.Provinces) != 1 || len(s.Countries) != 1 {
		t.Fatalf("client certificate subject %+v must have exactly one value per field", s)
	}
	// RFC 2253 lists the subject most-specific first, which is how MongoDB
	// names an x509 user.
	certDN := "CN=" + cert.Spec.CommonName + ",OU=" + s.OrganizationalUnits[0] + ",O=" + s.Organizations[0] +
		",L=" + s.Localities[0] + ",ST=" + s.Provinces[0] + ",C=" + s.Countries[0]
	if certDN != contractAppDN {
		t.Errorf("client certificate subject is %q, want %q", certDN, contractAppDN)
	}
	if cert.Spec.SecretName != contractClientCert {
		t.Errorf("client certificate secretName = %q, want %q", cert.Spec.SecretName, contractClientCert)
	}
	if want := db.FullnameOverride + "-psmdb-issuer"; cert.Spec.IssuerRef.Name != want {
		t.Errorf("client certificate issuer = %q, want the operator's %q", cert.Spec.IssuerRef.Name, want)
	}

	// readWrite for the pipeline, and collMod only, which the setup Job
	// needs to change an existing TTL index.
	hasUser := false
	for _, u := range db.Users {
		if u.Name == contractAppDN && u.DB == "$external" && len(u.Roles) == 2 &&
			u.Roles[0].Name == "readWrite" && u.Roles[0].DB == contractDatabase &&
			u.Roles[1].Name == contractTTLRole && u.Roles[1].DB == "admin" {

			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("nvsentinel-mongodb users %+v lack the $external user %q with readWrite on %s and %s",
			db.Users, contractAppDN, contractDatabase, contractTTLRole)
	}
	hasRole := false
	for _, r := range db.CustomRoles {
		if r.Role == contractTTLRole && r.DB == "admin" && len(r.Privileges) == 1 &&
			r.Privileges[0].Resource.DB == contractDatabase && r.Privileges[0].Resource.Collection == "" &&
			slices.Equal(r.Privileges[0].Actions, []string{"collMod"}) {

			hasRole = true
		}
	}
	if !hasRole {
		t.Errorf("nvsentinel-mongodb roles %+v lack %s granting only collMod on %s", db.CustomRoles, contractTTLRole, contractDatabase)
	}

	if conn.Metadata.Name != contractURISecret {
		t.Errorf("connection Secret = %q, want %q", conn.Metadata.Name, contractURISecret)
	}
	uri := conn.StringData["MONGODB_URI"]
	rs, ok := db.Replsets["rs0"]
	if !ok || rs.Size < 3 {
		t.Errorf("replsets.rs0 = %+v, want a replica set of at least 3", rs)
	}
	for _, part := range []string{db.FullnameOverride + "-rs0.", "replicaSet=" + rs.Name} {
		if !strings.Contains(uri, part) {
			t.Errorf("MONGODB_URI %q does not contain %q", uri, part)
		}
	}
	if strings.Contains(uri, "@") {
		t.Errorf("MONGODB_URI %q carries credentials; NVSentinel authenticates with its client certificate", uri)
	}

	if op.Image.Repository != db.InitImage.Repository || op.Image.Tag != db.InitImage.Tag {
		t.Errorf("initImage %s:%s differs from the operator image %s:%s", db.InitImage.Repository, db.InitImage.Tag, op.Image.Repository, op.Image.Tag)
	}
	minor := func(v string) string {
		v, _, _ = strings.Cut(v, "@")
		parts := strings.SplitN(v, ".", 3)
		if len(parts) < 2 {
			return v
		}
		return parts[0] + "." + parts[1]
	}
	if minor(db.CRVersion) != minor(op.Image.Tag) {
		t.Errorf("crVersion %s is not at the operator's minor (%s)", db.CRVersion, op.Image.Tag)
	}
	if !op.DisableTelemetry {
		t.Error("psmdb-operator disableTelemetry must be true")
	}

	_, store := remediationStore(t)
	for _, step := range remediationSteps {
		for _, c := range store.Mixins[step.mixin].Spec.ComponentRefs {
			if c.Name != "nvsentinel" {
				continue
			}
			global, _ := c.Overrides["global"].(map[string]any)
			ds, _ := global["datastore"].(map[string]any)
			setup, _ := ds["setupJob"].(map[string]any)
			image, _ := setup["image"].(map[string]any)
			if image["repository"] != db.Image.Repository || image["tag"] != db.Image.Tag {
				t.Errorf("%s: setup Job image %v:%v differs from the mongod image %s:%s", step.mixin,
					image["repository"], image["tag"], db.Image.Repository, db.Image.Tag)
			}
		}
	}
}

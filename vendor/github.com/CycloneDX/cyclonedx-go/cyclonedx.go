// This file is part of CycloneDX Go
//
// Licensed under the Apache License, Version 2.0 (the “License”);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an “AS IS” BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0
// Copyright (c) OWASP Foundation. All Rights Reserved.

package cyclonedx

import (
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
)

//go:generate stringer -linecomment -output cyclonedx_string.go -type MediaType,SpecVersion

const (
	BOMFormat = "CycloneDX"
)

var ErrInvalidSpecVersion = errors.New("invalid specification version")

type Advisory struct {
	Title string `json:"title,omitempty" xml:"title,omitempty"`
	URL   string `json:"url" xml:"url"`
}

type AffectedVersions struct {
	Version string              `json:"version,omitempty" xml:"version,omitempty"`
	Range   string              `json:"range,omitempty" xml:"range,omitempty"`
	Status  VulnerabilityStatus `json:"status" xml:"status"`
}

type Affects struct {
	Ref   string              `json:"ref" xml:"ref"`
	Range *[]AffectedVersions `json:"versions,omitempty" xml:"versions>version,omitempty"`
}

type Annotation struct {
	BOMRef    string          `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Subjects  *[]BOMReference `json:"subjects,omitempty" xml:"subjects>subject,omitempty"`
	Annotator *Annotator      `json:"annotator,omitempty" xml:"annotator,omitempty"`
	Timestamp string          `json:"timestamp,omitempty" xml:"timestamp,omitempty"`
	Text      string          `json:"text,omitempty" xml:"text,omitempty"`
	Signature *JSFSignature   `json:"signature,omitempty" xml:"-"`
}

type Annotator struct {
	Organization *OrganizationalEntity  `json:"organization,omitempty" xml:"organization,omitempty"`
	Individual   *OrganizationalContact `json:"individual,omitempty" xml:"individual,omitempty"`
	Component    *Component             `json:"component,omitempty" xml:"component,omitempty"`
	Service      *Service               `json:"service,omitempty" xml:"service,omitempty"`
}

type Assessor struct {
	BOMRef       BOMReference          `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	ThirdParty   bool                  `json:"thirdParty,omitempty" xml:"thirdParty,omitempty"`
	Organization *OrganizationalEntity `json:"organization,omitempty" xml:"organization,omitempty"`
}

type AttachedText struct {
	Content     string `json:"content" xml:",chardata"`
	ContentType string `json:"contentType,omitempty" xml:"content-type,attr,omitempty"`
	Encoding    string `json:"encoding,omitempty" xml:"encoding,attr,omitempty"`
}

type Attestation struct {
	Summary   string            `json:"summary,omitempty" xml:"summary,omitempty"`
	Assessor  BOMReference      `json:"assessor,omitempty" xml:"assessor,omitempty"`
	Map       *[]AttestationMap `json:"map,omitempty" xml:"map,omitempty"`
	Signature *JSFSignature     `json:"signature,omitempty" xml:"-"`
}

type AttestationMap struct {
	Requirement   string                  `json:"requirement,omitempty" xml:"requirement,omitempty"`
	Claims        *[]BOMReference         `json:"claims,omitempty" xml:"claims>claim,omitempty"`
	CounterClaims *[]BOMReference         `json:"counterClaims,omitempty" xml:"counterClaims>counterClaim,omitempty"`
	Conformance   *AttestationConformance `json:"conformance,omitempty" xml:"conformance,omitempty"`
	Confidence    *AttestationConfidence  `json:"confidence,omitempty" xml:"confidence,omitempty"`
}

type AttestationConformance struct {
	Score                *float64        `json:"score,omitempty" xml:"score,omitempty"`
	Rationale            string          `json:"rationale,omitempty" xml:"rationale,omitempty"`
	MitigationStrategies *[]BOMReference `json:"mitigationStrategies,omitempty" xml:"mitigationStrategies>mitigationStrategy,omitempty"`
}

type AttestationConfidence struct {
	Score     *float64 `json:"score,omitempty" xml:"score,omitempty"`
	Rationale string   `json:"rationale,omitempty" xml:"rationale,omitempty"`
}

type BOM struct {
	// XML specific fields
	XMLName xml.Name `json:"-" xml:"bom"`
	XMLNS   string   `json:"-" xml:"xmlns,attr"`

	// JSON specific fields
	JSONSchema  string      `json:"$schema,omitempty" xml:"-"`
	BOMFormat   string      `json:"bomFormat" xml:"-"`
	SpecVersion SpecVersion `json:"specVersion" xml:"-"`

	SerialNumber       string               `json:"serialNumber,omitempty" xml:"serialNumber,attr,omitempty"`
	Version            int                  `json:"version" xml:"version,attr"`
	Metadata           *Metadata            `json:"metadata,omitempty" xml:"metadata,omitempty"`
	Components         *[]Component         `json:"components,omitempty" xml:"components>component,omitempty"`
	Services           *[]Service           `json:"services,omitempty" xml:"services>service,omitempty"`
	ExternalReferences *[]ExternalReference `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
	Dependencies       *[]Dependency        `json:"dependencies,omitempty" xml:"dependencies>dependency,omitempty"`
	Compositions       *[]Composition       `json:"compositions,omitempty" xml:"compositions>composition,omitempty"`
	Properties         *[]Property          `json:"properties,omitempty" xml:"properties>property,omitempty"`
	Vulnerabilities    *[]Vulnerability     `json:"vulnerabilities,omitempty" xml:"vulnerabilities>vulnerability,omitempty"`
	Annotations        *[]Annotation        `json:"annotations,omitempty" xml:"annotations>annotation,omitempty"`
	Formulation        *[]Formula           `json:"formulation,omitempty" xml:"formulation>formula,omitempty"`
	Declarations       *Declarations        `json:"declarations,omitempty" xml:"declarations,omitempty"`
	Definitions        *Definitions         `json:"definitions,omitempty" xml:"definitions,omitempty"`
	Signature          *JSFSignature        `json:"signature,omitempty" xml:"-"`
}

func NewBOM() *BOM {
	return &BOM{
		JSONSchema:  jsonSchemas[SpecVersion1_6],
		XMLNS:       xmlNamespaces[SpecVersion1_6],
		BOMFormat:   BOMFormat,
		SpecVersion: SpecVersion1_6,
		Version:     1,
	}
}

type BOMFileFormat int

const (
	BOMFileFormatXML BOMFileFormat = iota
	BOMFileFormatJSON
)

// Bool is a convenience function to transform a value of the primitive type bool to a pointer of bool
func Bool(value bool) *bool {
	return &value
}

type BOMReference string

type Callstack struct {
	Frames *[]CallstackFrame `json:"frames,omitempty" xml:"frames>frame,omitempty"`
}

type CallstackFrame struct {
	Package      string    `json:"package,omitempty" xml:"package,omitempty"`
	Module       string    `json:"module,omitempty" xml:"module,omitempty"`
	Function     string    `json:"function,omitempty" xml:"function,omitempty"`
	Parameters   *[]string `json:"parameters,omitempty" xml:"parameters>parameter,omitempty"`
	Line         *int      `json:"line,omitempty" xml:"line,omitempty"`
	Column       *int      `json:"column,omitempty" xml:"column,omitempty"`
	FullFilename string    `json:"fullFilename,omitempty" xml:"fullFilename,omitempty"`
}

type CertificateProperties struct {
	SubjectName           string       `json:"subjectName,omitempty" xml:"subjectName,omitempty"`
	IssuerName            string       `json:"issuerName,omitempty" xml:"issuerName,omitempty"`
	NotValidBefore        string       `json:"notValidBefore,omitempty" xml:"notValidBefore,omitempty"`
	NotValidAfter         string       `json:"notValidAfter,omitempty" xml:"notValidAfter,omitempty"`
	SignatureAlgorithmRef BOMReference `json:"signatureAlgorithmRef,omitempty" xml:"signatureAlgorithmRef,omitempty"`
	SubjectPublicKeyRef   BOMReference `json:"subjectPublicKeyRef,omitempty" xml:"subjectPublicKeyRef,omitempty"`
	CertificateFormat     string       `json:"certificateFormat,omitempty" xml:"certificateFormat,omitempty"`
	CertificateExtension  string       `json:"certificateExtension,omitempty" xml:"certificateExtension,omitempty"`
}

type Claim struct {
	BOMRef               string               `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Target               BOMReference         `json:"target,omitempty" xml:"target,omitempty"`
	Predicate            string               `json:"predicate,omitempty" xml:"predicate,omitempty"`
	MitigationStrategies *[]BOMReference      `json:"mitigationStrategies,omitempty" xml:"mitigationStrategies>mitigationStrategy,omitempty"`
	Reasoning            string               `json:"reasoning,omitempty" xml:"reasoning,omitempty"`
	Evidence             *[]BOMReference      `json:"evidence,omitempty" xml:"evidence,omitempty"`
	CounterEvidence      *[]BOMReference      `json:"counterEvidence,omitempty" xml:"counterEvidence,omitempty"`
	ExternalReferences   *[]ExternalReference `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
	Signature            *JSFSignature        `json:"signature,omitempty" xml:"-"`
}

type CipherSuite struct {
	Name        string          `json:"name,omitempty" xml:"name,omitempty"`
	Algorithms  *[]BOMReference `json:"algorithms,omitempty" xml:"algorithms,omitempty"`
	Identifiers *[]string       `json:"identifiers,omitempty" xml:"identifiers,omitempty"`
}

type ComponentType string

const (
	ComponentTypeApplication          ComponentType = "application"
	ComponentTypeContainer            ComponentType = "container"
	ComponentTypeCryptographicAsset   ComponentType = "cryptographic-asset"
	ComponentTypeData                 ComponentType = "data"
	ComponentTypeDevice               ComponentType = "device"
	ComponentTypeDeviceDriver         ComponentType = "device-driver"
	ComponentTypeFile                 ComponentType = "file"
	ComponentTypeFirmware             ComponentType = "firmware"
	ComponentTypeFramework            ComponentType = "framework"
	ComponentTypeLibrary              ComponentType = "library"
	ComponentTypeMachineLearningModel ComponentType = "machine-learning-model"
	ComponentTypeOS                   ComponentType = "operating-system"
	ComponentTypePlatform             ComponentType = "platform"
)

type Commit struct {
	UID       string              `json:"uid,omitempty" xml:"uid,omitempty"`
	URL       string              `json:"url,omitempty" xml:"url,omitempty"`
	Author    *IdentifiableAction `json:"author,omitempty" xml:"author,omitempty"`
	Committer *IdentifiableAction `json:"committer,omitempty" xml:"committer,omitempty"`
	Message   string              `json:"message,omitempty" xml:"message,omitempty"`
}

type Component struct {
	BOMRef             string                   `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	MIMEType           string                   `json:"mime-type,omitempty" xml:"mime-type,attr,omitempty"`
	Type               ComponentType            `json:"type" xml:"type,attr"`
	Supplier           *OrganizationalEntity    `json:"supplier,omitempty" xml:"supplier,omitempty"`
	Manufacturer       *OrganizationalEntity    `json:"manufacturer,omitempty" xml:"manufacturer,omitempty"`
	Author             string                   `json:"author,omitempty" xml:"author,omitempty"` // Deprecated: Use authors or manufacturer instead.
	Authors            *[]OrganizationalContact `json:"authors,omitempty" xml:"authors>author,omitempty"`
	Publisher          string                   `json:"publisher,omitempty" xml:"publisher,omitempty"`
	Group              string                   `json:"group,omitempty" xml:"group,omitempty"`
	Name               string                   `json:"name" xml:"name"`
	Version            string                   `json:"version,omitempty" xml:"version,omitempty"`
	Description        string                   `json:"description,omitempty" xml:"description,omitempty"`
	Scope              Scope                    `json:"scope,omitempty" xml:"scope,omitempty"`
	Hashes             *[]Hash                  `json:"hashes,omitempty" xml:"hashes>hash,omitempty"`
	Licenses           *Licenses                `json:"licenses,omitempty" xml:"licenses,omitempty"`
	Copyright          string                   `json:"copyright,omitempty" xml:"copyright,omitempty"`
	CPE                string                   `json:"cpe,omitempty" xml:"cpe,omitempty"`
	PackageURL         string                   `json:"purl,omitempty" xml:"purl,omitempty"`
	OmniborID          *[]string                `json:"omniborId,omitempty" xml:"omniborId,omitempty"`
	SWHID              *[]string                `json:"swhid,omitempty" xml:"swhid,omitempty"`
	SWID               *SWID                    `json:"swid,omitempty" xml:"swid,omitempty"`
	Modified           *bool                    `json:"modified,omitempty" xml:"modified,omitempty"`
	Pedigree           *Pedigree                `json:"pedigree,omitempty" xml:"pedigree,omitempty"`
	ExternalReferences *[]ExternalReference     `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
	Properties         *[]Property              `json:"properties,omitempty" xml:"properties>property,omitempty"`
	Components         *[]Component             `json:"components,omitempty" xml:"components>component,omitempty"`
	Evidence           *Evidence                `json:"evidence,omitempty" xml:"evidence,omitempty"`
	ReleaseNotes       *ReleaseNotes            `json:"releaseNotes,omitempty" xml:"releaseNotes,omitempty"`
	ModelCard          *MLModelCard             `json:"modelCard,omitempty" xml:"modelCard,omitempty"`
	Data               *[]ComponentData         `json:"data,omitempty" xml:"data,omitempty"`
	CryptoProperties   *CryptoProperties        `json:"cryptoProperties,omitempty" xml:"cryptoProperties,omitempty"`
	Tags               *[]string                `json:"tags,omitempty" xml:"tags>tag,omitempty"`
	Signature          *JSFSignature            `json:"signature,omitempty" xml:"-"`
}

type ComponentData struct {
	BOMRef         string                 `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Type           ComponentDataType      `json:"type,omitempty" xml:"type,omitempty"`
	Name           string                 `json:"name,omitempty" xml:"name,omitempty"`
	Contents       *ComponentDataContents `json:"contents,omitempty" xml:"contents,omitempty"`
	Classification string                 `json:"classification,omitempty" xml:"classification,omitempty"`
	SensitiveData  *[]string              `json:"sensitiveData,omitempty" xml:"sensitiveData,omitempty"`
	Graphics       *ComponentDataGraphics `json:"graphics,omitempty" xml:"graphics,omitempty"`
	Description    string                 `json:"description,omitempty" xml:"description,omitempty"`
	Governance     *DataGovernance        `json:"governance,omitempty" xml:"governance,omitempty"`
}

type ComponentDataContents struct {
	Attachment *AttachedText `json:"attachment,omitempty" xml:"attachment,omitempty"`
	URL        string        `json:"url,omitempty" xml:"url,omitempty"`
	Properties *[]Property   `json:"properties,omitempty" xml:"properties,omitempty"`
}

type ComponentDataGovernanceResponsibleParty struct {
	Organization *OrganizationalEntity  `json:"organization,omitempty" xml:"organization,omitempty"`
	Contact      *OrganizationalContact `json:"contact,omitempty" xml:"contact,omitempty"`
}

type ComponentDataGraphic struct {
	Name  string        `json:"name,omitempty" xml:"name,omitempty"`
	Image *AttachedText `json:"image,omitempty" xml:"image,omitempty"`
}

type ComponentDataGraphics struct {
	Description string                  `json:"description,omitempty" xml:"description,omitempty"`
	Collection  *[]ComponentDataGraphic `json:"collection,omitempty" xml:"collection>graphic,omitempty"`
}

type ComponentDataType string

const (
	ComponentDataTypeConfiguration ComponentDataType = "configuration"
	ComponentDataTypeDataset       ComponentDataType = "dataset"
	ComponentDataTypeDefinition    ComponentDataType = "definition"
	ComponentDataTypeOther         ComponentDataType = "other"
	ComponentDataTypeSourceCode    ComponentDataType = "source-code"
)

type Composition struct {
	BOMRef          string               `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Aggregate       CompositionAggregate `json:"aggregate" xml:"aggregate"`
	Assemblies      *[]BOMReference      `json:"assemblies,omitempty" xml:"assemblies>assembly,omitempty"`
	Dependencies    *[]BOMReference      `json:"dependencies,omitempty" xml:"dependencies>dependency,omitempty"`
	Vulnerabilities *[]BOMReference      `json:"vulnerabilities,omitempty" xml:"vulnerabilities>vulnerability,omitempty"`
	Signature       *JSFSignature        `json:"signature,omitempty" xml:"-"`
}

type CompositionAggregate string

const (
	CompositionAggregateComplete                            CompositionAggregate = "complete"
	CompositionAggregateIncomplete                          CompositionAggregate = "incomplete"
	CompositionAggregateIncompleteFirstPartyOnly            CompositionAggregate = "incomplete_first_party_only"
	CompositionAggregateIncompleteFirstPartyOpenSourceOnly  CompositionAggregate = "incomplete_first_party_opensource_only"
	CompositionAggregateIncompleteFirstPartyProprietaryOnly CompositionAggregate = "incomplete_first_party_proprietary_only"
	CompositionAggregateIncompleteThirdPartyOnly            CompositionAggregate = "incomplete_third_party_only"
	CompositionAggregateIncompleteThirdPartyOpenSourceOnly  CompositionAggregate = "incomplete_third_party_opensource_only"
	CompositionAggregateIncompleteThirdPartyProprietaryOnly CompositionAggregate = "incomplete_third_party_proprietary_only"
	CompositionAggregateNotSpecified                        CompositionAggregate = "not_specified"
	CompositionAggregateUnknown                             CompositionAggregate = "unknown"
)

type Copyright struct {
	Text string `json:"text" xml:"-"`
}

type Credits struct {
	Organizations *[]OrganizationalEntity  `json:"organizations,omitempty" xml:"organizations>organization,omitempty"`
	Individuals   *[]OrganizationalContact `json:"individuals,omitempty" xml:"individuals>individual,omitempty"`
}

type CryptoAlgorithmMode string

const (
	CryptoAlgorithmModeCBC     CryptoAlgorithmMode = "cbc"
	CryptoAlgorithmModeECB     CryptoAlgorithmMode = "ecb"
	CryptoAlgorithmModeCCM     CryptoAlgorithmMode = "ccm"
	CryptoAlgorithmModeGCM     CryptoAlgorithmMode = "gcm"
	CryptoAlgorithmModeCFB     CryptoAlgorithmMode = "cfb"
	CryptoAlgorithmModeOFB     CryptoAlgorithmMode = "ofb"
	CryptoAlgorithmModeCTR     CryptoAlgorithmMode = "ctr"
	CryptoAlgorithmModeOther   CryptoAlgorithmMode = "other"
	CryptoAlgorithmModeUnknown CryptoAlgorithmMode = "unknown"
)

type CryptoAlgorithmProperties struct {
	Primitive                CryptoPrimitive             `json:"primitive,omitempty" xml:"primitive,omitempty"`
	ParameterSetIdentifier   string                      `json:"parameterSetIdentifier,omitempty" xml:"parameterSetIdentifier,omitempty"`
	Curve                    string                      `json:"curve,omitempty" xml:"curve,omitempty"`
	ExecutionEnvironment     CryptoExecutionEnvironment  `json:"executionEnvironment,omitempty" xml:"executionEnvironment,omitempty"`
	ImplementationPlatform   ImplementationPlatform      `json:"implementationPlatform,omitempty" xml:"implementationPlatform,omitempty"`
	CertificationLevel       *[]CryptoCertificationLevel `json:"certificationLevel,omitempty" xml:"certificationLevel,omitempty"`
	Mode                     CryptoAlgorithmMode         `json:"mode,omitempty" xml:"mode,omitempty"`
	Padding                  CryptoPadding               `json:"padding,omitempty" xml:"padding,omitempty"`
	CryptoFunctions          *[]CryptoFunction           `json:"cryptoFunctions,omitempty" xml:"cryptoFunctions>cryptoFunction,omitempty"`
	ClassicalSecurityLevel   *int                        `json:"classicalSecurityLevel,omitempty" xml:"classicalSecurityLevel,omitempty"`
	NistQuantumSecurityLevel *int                        `json:"nistQuantumSecurityLevel,omitempty" xml:"nistQuantumSecurityLevel,omitempty"`
}

type CryptoAssetType string

const (
	CryptoAssetTypeAlgorithm             CryptoAssetType = "algorithm"
	CryptoAssetTypeCertificate           CryptoAssetType = "certificate"
	CryptoAssetTypeProtocol              CryptoAssetType = "protocol"
	CryptoAssetTypeRelatedCryptoMaterial CryptoAssetType = "related-crypto-material"
)

type CryptoCertificationLevel string

const (
	CryptoCertificationLevelNone         CryptoCertificationLevel = "none"
	CryptoCertificationLevelFIPS140_1_L1 CryptoCertificationLevel = "fips140-1-l1"
	CryptoCertificationLevelFIPS140_1_L2 CryptoCertificationLevel = "fips140-1-l2"
	CryptoCertificationLevelFIPS140_1_L3 CryptoCertificationLevel = "fips140-1-l3"
	CryptoCertificationLevelFIPS140_1_L4 CryptoCertificationLevel = "fips140-1-l4"
	CryptoCertificationLevelFIPS140_2_L1 CryptoCertificationLevel = "fips140-2-l1"
	CryptoCertificationLevelFIPS140_2_L2 CryptoCertificationLevel = "fips140-2-l2"
	CryptoCertificationLevelFIPS140_2_L3 CryptoCertificationLevel = "fips140-2-l3"
	CryptoCertificationLevelFIPS140_2_L4 CryptoCertificationLevel = "fips140-2-l4"
	CryptoCertificationLevelFIPS140_3_L1 CryptoCertificationLevel = "fips140-3-l1"
	CryptoCertificationLevelFIPS140_3_L2 CryptoCertificationLevel = "fips140-3-l2"
	CryptoCertificationLevelFIPS140_3_L3 CryptoCertificationLevel = "fips140-3-l3"
	CryptoCertificationLevelFIPS140_3_L4 CryptoCertificationLevel = "fips140-3-l4"
	CryptoCertificationLevelCCEAL1       CryptoCertificationLevel = "cc-eal1"
	CryptoCertificationLevelCCEAL1Plus   CryptoCertificationLevel = "cc-eal1+"
	CryptoCertificationLevelCCEAL2       CryptoCertificationLevel = "cc-eal2"
	CryptoCertificationLevelCCEAL2Plus   CryptoCertificationLevel = "cc-eal2+"
	CryptoCertificationLevelCCEAL3       CryptoCertificationLevel = "cc-eal3"
	CryptoCertificationLevelCCEAL3Plus   CryptoCertificationLevel = "cc-eal3+"
	CryptoCertificationLevelCCEAL4       CryptoCertificationLevel = "cc-eal4"
	CryptoCertificationLevelCCEAL4Plus   CryptoCertificationLevel = "cc-eal4+"
	CryptoCertificationLevelCCEAL5       CryptoCertificationLevel = "cc-eal5"
	CryptoCertificationLevelCCEAL5Plus   CryptoCertificationLevel = "cc-eal5+"
	CryptoCertificationLevelCCEAL6       CryptoCertificationLevel = "cc-eal6"
	CryptoCertificationLevelCCEAL6Plus   CryptoCertificationLevel = "cc-eal6+"
	CryptoCertificationLevelCCEAL7       CryptoCertificationLevel = "cc-eal7"
	CryptoCertificationLevelCCEAL7Plus   CryptoCertificationLevel = "cc-eal7+"
	CryptoCertificationLevelOther        CryptoCertificationLevel = "other"
	CryptoCertificationLevelUnknown      CryptoCertificationLevel = "unknown"
)

type CryptoExecutionEnvironment string

const (
	CryptoExecutionEnvironmentSoftwarePlainRAM     CryptoExecutionEnvironment = "software-plain-ram"
	CryptoExecutionEnvironmentSoftwareEncryptedRAM CryptoExecutionEnvironment = "software-encrypted-ram"
	CryptoExecutionEnvironmentSoftwareTEE          CryptoExecutionEnvironment = "software-tee"
	CryptoExecutionEnvironmentHardware             CryptoExecutionEnvironment = "hardware"
	CryptoExecutionEnvironmentOther                CryptoExecutionEnvironment = "other"
	CryptoExecutionEnvironmentUnknown              CryptoExecutionEnvironment = "unknown"
)

type CryptoFunction string

const (
	CryptoFunctionGenerate    CryptoFunction = "generate"
	CryptoFunctionKeygen      CryptoFunction = "keygen"
	CryptoFunctionEncrypt     CryptoFunction = "encrypt"
	CryptoFunctionDecrypt     CryptoFunction = "decrypt"
	CryptoFunctionDigest      CryptoFunction = "digest"
	CryptoFunctionTag         CryptoFunction = "tag"
	CryptoFunctionKeyderive   CryptoFunction = "keyderive"
	CryptoFunctionSign        CryptoFunction = "sign"
	CryptoFunctionVerify      CryptoFunction = "verify"
	CryptoFunctionEncapsulate CryptoFunction = "encapsulate"
	CryptoFunctionDecapsulate CryptoFunction = "decapsulate"
	CryptoFunctionOther       CryptoFunction = "other"
	CryptoFunctionUnknown     CryptoFunction = "unknown"
)

type CryptoKeyState string

const (
	CryptoKeyStatePreActivation CryptoKeyState = "pre-activation"
	CryptoKeyStateActive        CryptoKeyState = "active"
	CryptoKeyStateSuspended     CryptoKeyState = "suspended"
	CryptoKeyStateDeactivated   CryptoKeyState = "deactivated"
	CryptoKeyStateCompromised   CryptoKeyState = "compromised"
	CryptoKeyStateDestroyed     CryptoKeyState = "destroyed"
)

type CryptoPadding string

const (
	CryptoPaddingPKCS5    CryptoPadding = "pkcs5"
	CryptoPaddingPKCS7    CryptoPadding = "pkcs7"
	CryptoPaddingPKCS1v15 CryptoPadding = "pkcs1v15"
	CryptoPaddingOAEP     CryptoPadding = "oaep"
	CryptoPaddingRaw      CryptoPadding = "raw"
	CryptoPaddingOther    CryptoPadding = "other"
	CryptoPaddingUnknown  CryptoPadding = "unknown"
)

type CryptoPrimitive string

const (
	CryptoPrimitiveDRBG         CryptoPrimitive = "drbg"
	CryptoPrimitiveMAC          CryptoPrimitive = "mac"
	CryptoPrimitiveBlockCipher  CryptoPrimitive = "block-cipher"
	CryptoPrimitiveStreamCipher CryptoPrimitive = "stream-cipher"
	CryptoPrimitiveSignature    CryptoPrimitive = "signature"
	CryptoPrimitiveHash         CryptoPrimitive = "hash"
	CryptoPrimitivePKE          CryptoPrimitive = "pke"
	CryptoPrimitiveXOF          CryptoPrimitive = "xof"
	CryptoPrimitiveKDF          CryptoPrimitive = "kdf"
	CryptoPrimitiveKeyAgree     CryptoPrimitive = "key-agree"
	CryptoPrimitiveKEM          CryptoPrimitive = "kem"
	CryptoPrimitiveAE           CryptoPrimitive = "ae"
	CryptoPrimitiveCombiner     CryptoPrimitive = "combiner"
	CryptoPrimitiveOther        CryptoPrimitive = "other"
	CryptoPrimitiveUnknown      CryptoPrimitive = "unknown"
)

type CryptoProperties struct {
	AssetType                       CryptoAssetType                  `json:"assetType" xml:"assetType"`
	AlgorithmProperties             *CryptoAlgorithmProperties       `json:"algorithmProperties,omitempty" xml:"algorithmProperties,omitempty"`
	CertificateProperties           *CertificateProperties           `json:"certificateProperties,omitempty" xml:"certificateProperties,omitempty"`
	RelatedCryptoMaterialProperties *RelatedCryptoMaterialProperties `json:"relatedCryptoMaterialProperties,omitempty" xml:"relatedCryptoMaterialProperties,omitempty"`
	ProtocolProperties              *CryptoProtocolProperties        `json:"protocolProperties,omitempty" xml:"protocolProperties,omitempty"`
	OID                             string                           `json:"oid,omitempty" xml:"oid,omitempty"`
}

type CryptoProtocolProperties struct {
	Type                CryptoProtocolType   `json:"type,omitempty" xml:"type,omitempty"`
	Version             string               `json:"version,omitempty" xml:"version,omitempty"`
	CipherSuites        *[]CipherSuite       `json:"cipherSuites,omitempty" xml:"cipherSuites,omitempty"`
	IKEv2TransformTypes *IKEv2TransformTypes `json:"ikev2TransformTypes,omitempty" xml:"ikev2TransformTypes,omitempty"`
	CryptoRefArray      *[]BOMReference      `json:"cryptoRefArray,omitempty" xml:"cryptoRefArray,omitempty"`
}

type CryptoProtocolType string

const (
	CryptoProtocolTypeTLS     CryptoProtocolType = "tls"
	CryptoProtocolTypeSSH     CryptoProtocolType = "ssh"
	CryptoProtocolTypeIPSec   CryptoProtocolType = "ipsec"
	CryptoProtocolTypeIKE     CryptoProtocolType = "ike"
	CryptoProtocolTypeSSTP    CryptoProtocolType = "sstp"
	CryptoProtocolTypeWPA     CryptoProtocolType = "wpa"
	CryptoProtocolTypeOther   CryptoProtocolType = "other"
	CryptoProtocolTypeUnknown CryptoProtocolType = "unknown"
)

type IKEv2TransformTypes struct {
	Encr  *[]BOMReference `json:"encr,omitempty" xml:"encr,omitempty"`
	PRF   *[]BOMReference `json:"prf,omitempty" xml:"prf,omitempty"`
	Integ *[]BOMReference `json:"integ,omitempty" xml:"integ,omitempty"`
	KE    *[]BOMReference `json:"ke,omitempty" xml:"ke,omitempty"`
	ESN   bool            `json:"esn" xml:"esn"`
	Auth  *[]BOMReference `json:"auth,omitempty" xml:"auth,omitempty"`
}

type SecuredBy struct {
	Mechanism    string       `json:"mechanism,omitempty" xml:"mechanism,omitempty"`
	AlgorithmRef BOMReference `json:"algorithmRef,omitempty" xml:"algorithmRef,omitempty"`
}

type DataClassification struct {
	Flow           DataFlow `json:"flow" xml:"flow,attr"`
	Classification string   `json:"classification" xml:",chardata"`
}

type DataFlow string

const (
	DataFlowBidirectional DataFlow = "bi-directional"
	DataFlowInbound       DataFlow = "inbound"
	DataFlowOutbound      DataFlow = "outbound"
	DataFlowUnknown       DataFlow = "unknown"
)

type DataGovernance struct {
	Custodians *[]ComponentDataGovernanceResponsibleParty `json:"custodians,omitempty" xml:"custodians>custodian,omitempty"`
	Stewards   *[]ComponentDataGovernanceResponsibleParty `json:"stewards,omitempty" xml:"stewards>steward,omitempty"`
	Owners     *[]ComponentDataGovernanceResponsibleParty `json:"owners,omitempty" xml:"owners>owner,omitempty"`
}

type Declarations struct {
	Assessors    *[]Assessor            `json:"assessors,omitempty" xml:"assessors>assessor,omitempty"`
	Attestations *[]Attestation         `json:"attestations,omitempty" xml:"attestations>attestation,omitempty"`
	Claims       *[]Claim               `json:"claims,omitempty" xml:"claims>claim,omitempty"`
	Evidence     *[]DeclarationEvidence `json:"evidence,omitempty" xml:"evidence>evidence,omitempty"`
	Targets      *Targets               `json:"targets,omitempty" xml:"targets,omitempty"`
	Affirmation  *Affirmation           `json:"affirmation,omitempty" xml:"affirmation,omitempty"`
	Signature    *JSFSignature          `json:"signature,omitempty" xml:"-"`
}

type DeclarationEvidence struct {
	BOMRef       string                 `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	PropertyName string                 `json:"propertyName,omitempty" xml:"propertyName,omitempty"`
	Description  string                 `json:"description,omitempty" xml:"description,omitempty"`
	Data         *[]EvidenceData        `json:"data,omitempty" xml:"data,omitempty"`
	Created      string                 `json:"created,omitempty" xml:"created,omitempty"`
	Expires      string                 `json:"expires,omitempty" xml:"expires,omitempty"`
	Author       *OrganizationalContact `json:"author,omitempty" xml:"author,omitempty"`
	Reviewer     *OrganizationalContact `json:"reviewer,omitempty" xml:"reviewer,omitempty"`
	Signature    *JSFSignature          `json:"signature,omitempty" xml:"-"`
}

type Definitions struct {
	Standards *[]StandardDefinition `json:"standards,omitempty" xml:"standards>standard,omitempty"`
}

type EvidenceData struct {
	Name           string                `json:"name,omitempty" xml:"name,omitempty"`
	Contents       *EvidenceDataContents `json:"contents,omitempty" xml:"contents,omitempty"`
	Classification *DataClassification   `json:"classification,omitempty" xml:"data>classification,omitempty"`
	SensitiveData  *[]string             `json:"sensitiveData,omitempty" xml:"sensitiveData,omitempty"`
	Governance     *DataGovernance       `json:"governance,omitempty" xml:"governance,omitempty"`
}

type EvidenceDataContents struct {
	Attachment *AttachedText `json:"attachment,omitempty" xml:"attachment,omitempty"`
	URL        string        `json:"url,omitempty" xml:"url,omitempty"`
}

type Targets struct {
	Organizations *[]OrganizationalEntity `json:"organizations,omitempty" xml:"organizations>organization,omitempty"`
	Components    *[]Component            `json:"components,omitempty" xml:"components>component,omitempty"`
	Services      *[]Service              `json:"services,omitempty" xml:"services>service,omitempty"`
}

type Affirmation struct {
	Statement   string        `json:"statement,omitempty" xml:"statement,omitempty"`
	Signatories *[]Signatory  `json:"signatories,omitempty" xml:"signatories>signatory,omitempty"`
	Signature   *JSFSignature `json:"signature,omitempty" xml:"-"`
}

type Signatory struct {
	Name              string                `json:"name,omitempty" xml:"name,omitempty"`
	Role              string                `json:"role,omitempty" xml:"role,omitempty"`
	Signature         *JSFSignature         `json:"signature,omitempty" xml:"-"`
	Organization      *OrganizationalEntity `json:"organization,omitempty" xml:"organization,omitempty"`
	ExternalReference *ExternalReference    `json:"externalReference,omitempty" xml:"externalReference,omitempty"`
}

type Dependency struct {
	Ref          string    `json:"ref"`
	Dependencies *[]string `json:"dependsOn,omitempty"`
	Provides     *[]string `json:"provides,omitempty"`
}

type Diff struct {
	Text *AttachedText `json:"text,omitempty" xml:"text,omitempty"`
	URL  string        `json:"url,omitempty" xml:"url,omitempty"`
}

type EnvironmentVariables []EnvironmentVariableChoice

type EnvironmentVariableChoice struct {
	Property *Property `json:"-" xml:"-"`
	Value    string    `json:"-" xml:"-"`
}

type Event struct {
	UID          string                   `json:"uid,omitempty" xml:"uid,omitempty"`
	Description  string                   `json:"description,omitempty" xml:"description,omitempty"`
	TimeReceived string                   `json:"timeReceived,omitempty" xml:"timeReceived,omitempty"`
	Data         *AttachedText            `json:"data,omitempty" xml:"data,omitempty"`
	Source       *ResourceReferenceChoice `json:"source,omitempty" xml:"source,omitempty"`
	Target       *ResourceReferenceChoice `json:"target,omitempty" xml:"target,omitempty"`
	Properties   *[]Property              `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type Evidence struct {
	Identity    *[]EvidenceIdentity   `json:"identity,omitempty" xml:"-"`
	Occurrences *[]EvidenceOccurrence `json:"occurrences,omitempty" xml:"-"`
	Callstack   *Callstack            `json:"callstack,omitempty" xml:"-"`
	Licenses    *Licenses             `json:"licenses,omitempty" xml:"-"`
	Copyright   *[]Copyright          `json:"copyright,omitempty" xml:"-"`
}

type EvidenceIdentity struct {
	Field      EvidenceIdentityFieldType `json:"field,omitempty" xml:"field,omitempty"`
	Confidence *float32                  `json:"confidence,omitempty" xml:"confidence,omitempty"`
	Methods    *[]EvidenceIdentityMethod `json:"methods,omitempty" xml:"methods>method,omitempty"`
	Tools      *[]BOMReference           `json:"tools,omitempty" xml:"tools>tool,omitempty"`
}

type EvidenceIdentityFieldType string

const (
	EvidenceIdentityFieldTypeCPE       EvidenceIdentityFieldType = "cpe"
	EvidenceIdentityFieldTypeGroup     EvidenceIdentityFieldType = "group"
	EvidenceIdentityFieldTypeHash      EvidenceIdentityFieldType = "hash"
	EvidenceIdentityFieldTypeName      EvidenceIdentityFieldType = "name"
	EvidenceIdentityFieldTypePURL      EvidenceIdentityFieldType = "purl"
	EvidenceIdentityFieldTypeOmniborID EvidenceIdentityFieldType = "omniborId"
	EvidenceIdentityFieldTypeSWHID     EvidenceIdentityFieldType = "swhid"
	EvidenceIdentityFieldTypeSWID      EvidenceIdentityFieldType = "swid"
	EvidenceIdentityFieldTypeVersion   EvidenceIdentityFieldType = "version"
)

type EvidenceIdentityMethod struct {
	Technique  EvidenceIdentityTechnique `json:"technique,omitempty" xml:"technique,omitempty"`
	Confidence *float32                  `json:"confidence,omitempty" xml:"confidence,omitempty"`
	Value      string                    `json:"value,omitempty" xml:"value,omitempty"`
}

type EvidenceIdentityTechnique string

const (
	EvidenceIdentityTechniqueASTFingerprint     EvidenceIdentityTechnique = "ast-fingerprint"
	EvidenceIdentityTechniqueAttestation        EvidenceIdentityTechnique = "attestation"
	EvidenceIdentityTechniqueBinaryAnalysis     EvidenceIdentityTechnique = "binary-analysis"
	EvidenceIdentityTechniqueDynamicAnalysis    EvidenceIdentityTechnique = "dynamic-analysis"
	EvidenceIdentityTechniqueFilename           EvidenceIdentityTechnique = "filename"
	EvidenceIdentityTechniqueHashComparison     EvidenceIdentityTechnique = "hash-comparison"
	EvidenceIdentityTechniqueInstrumentation    EvidenceIdentityTechnique = "instrumentation"
	EvidenceIdentityTechniqueManifestAnalysis   EvidenceIdentityTechnique = "manifest-analysis"
	EvidenceIdentityTechniqueOther              EvidenceIdentityTechnique = "other"
	EvidenceIdentityTechniqueSourceCodeAnalysis EvidenceIdentityTechnique = "source-code-analysis"
)

type EvidenceOccurrence struct {
	BOMRef            string `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Location          string `json:"location,omitempty" xml:"location,omitempty"`
	Line              *int   `json:"line,omitempty" xml:"line,attr,omitempty"`
	Offset            *int   `json:"offset,omitempty" xml:"offset,attr,omitempty"`
	Symbol            string `json:"symbol,omitempty" xml:"symbol,attr,omitempty"`
	AdditionalContext string `json:"additionalContext,omitempty" xml:"additionalContext,attr,omitempty"`
}

type ExternalReference struct {
	URL     string                `json:"url" xml:"url"`
	Comment string                `json:"comment,omitempty" xml:"comment,omitempty"`
	Hashes  *[]Hash               `json:"hashes,omitempty" xml:"hashes>hash,omitempty"`
	Type    ExternalReferenceType `json:"type" xml:"type,attr"`
}

type ExternalReferenceType string

const (
	ERTypeAdversaryModel          ExternalReferenceType = "adversary-model"
	ERTypeAdvisories              ExternalReferenceType = "advisories"
	ERTypeAttestation             ExternalReferenceType = "attestation"
	ERTypeBOM                     ExternalReferenceType = "bom"
	ERTypeBuildMeta               ExternalReferenceType = "build-meta"
	ERTypeBuildSystem             ExternalReferenceType = "build-system"
	ERTypeCertificationReport     ExternalReferenceType = "certification-report"
	ERTypeChat                    ExternalReferenceType = "chat"
	ERTypeConfiguration           ExternalReferenceType = "configuration"
	ERTypeCodifiedInfrastructure  ExternalReferenceType = "codified-infrastructure"
	ERTypeComponentAnalysisReport ExternalReferenceType = "component-analysis-report"
	ERTypeDistribution            ExternalReferenceType = "distribution"
	ERTypeDistributionIntake      ExternalReferenceType = "distribution-intake"
	ERTypeDocumentation           ExternalReferenceType = "documentation"
	ERTypeDynamicAnalysisReport   ExternalReferenceType = "dynamic-analysis-report"
	ERTypeDigitalSignature        ExternalReferenceType = "digital-signature"
	ERTypeElectronicSignature     ExternalReferenceType = "electronic-signature"
	ERTypeEvidence                ExternalReferenceType = "evidence"
	ERTypeExploitabilityStatement ExternalReferenceType = "exploitability-statement"
	ERTypeFormulation             ExternalReferenceType = "formulation"
	ERTypeIssueTracker            ExternalReferenceType = "issue-tracker"
	ERTypeLicense                 ExternalReferenceType = "license"
	ERTypeLog                     ExternalReferenceType = "log"
	ERTypeMailingList             ExternalReferenceType = "mailing-list"
	ERTypeMaturityReport          ExternalReferenceType = "maturity-report"
	ERTypeModelCard               ExternalReferenceType = "model-card"
	ERTypeOther                   ExternalReferenceType = "other"
	ERTypePentestReport           ExternalReferenceType = "pentest-report"
	ERTypePOAM                    ExternalReferenceType = "poam"
	ERTypeQualityMetrics          ExternalReferenceType = "quality-metrics"
	ERTypeReleaseNotes            ExternalReferenceType = "release-notes"
	ERTypeRiskAssessment          ExternalReferenceType = "risk-assessment"
	ERTypeRFC9116                 ExternalReferenceType = "rfc-9116"
	ERTypeRuntimeAnalysisReport   ExternalReferenceType = "runtime-analysis-report"
	ERTypeSecurityContact         ExternalReferenceType = "security-contact"
	ERTypeSocial                  ExternalReferenceType = "social"
	ERTypeSourceDistribution      ExternalReferenceType = "source-distribution"
	ERTypeStaticAnalysisReport    ExternalReferenceType = "static-analysis-report"
	ERTypeSupport                 ExternalReferenceType = "support"
	ERTypeThreatModel             ExternalReferenceType = "threat-model"
	ERTypeVCS                     ExternalReferenceType = "vcs"
	ERTypeVulnerabilityAssertion  ExternalReferenceType = "vulnerability-assertion"
	ERTypeWebsite                 ExternalReferenceType = "website"
)

type Formula struct {
	BOMRef     string       `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Components *[]Component `json:"components,omitempty" xml:"components>component,omitempty"`
	Services   *[]Service   `json:"services,omitempty" xml:"services>service,omitempty"`
	Workflows  *[]Workflow  `json:"workflows,omitempty" xml:"workflows>workflow,omitempty"`
	Properties *[]Property  `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type Hash struct {
	Algorithm HashAlgorithm `json:"alg" xml:"alg,attr"`
	Value     string        `json:"content" xml:",chardata"`
}

type HashAlgorithm string

const (
	HashAlgoMD5         HashAlgorithm = "MD5"
	HashAlgoSHA1        HashAlgorithm = "SHA-1"
	HashAlgoSHA256      HashAlgorithm = "SHA-256"
	HashAlgoSHA384      HashAlgorithm = "SHA-384"
	HashAlgoSHA512      HashAlgorithm = "SHA-512"
	HashAlgoSHA3_256    HashAlgorithm = "SHA3-256"
	HashAlgoSHA3_384    HashAlgorithm = "SHA3-384"
	HashAlgoSHA3_512    HashAlgorithm = "SHA3-512"
	HashAlgoBlake2b_256 HashAlgorithm = "BLAKE2b-256"
	HashAlgoBlake2b_384 HashAlgorithm = "BLAKE2b-384"
	HashAlgoBlake2b_512 HashAlgorithm = "BLAKE2b-512"
	HashAlgoBlake3      HashAlgorithm = "BLAKE3"
)

type IdentifiableAction struct {
	Timestamp string `json:"timestamp,omitempty" xml:"timestamp,omitempty"`
	Name      string `json:"name,omitempty" xml:"name,omitempty"`
	Email     string `json:"email,omitempty" xml:"email,omitempty"`
}

type ImpactAnalysisJustification string

const (
	IAJCodeNotPresent               ImpactAnalysisJustification = "code_not_present"
	IAJCodeNotReachable             ImpactAnalysisJustification = "code_not_reachable"
	IAJRequiresConfiguration        ImpactAnalysisJustification = "requires_configuration"
	IAJRequiresDependency           ImpactAnalysisJustification = "requires_dependency"
	IAJRequiresEnvironment          ImpactAnalysisJustification = "requires_environment"
	IAJProtectedByCompiler          ImpactAnalysisJustification = "protected_by_compiler"
	IAJProtectedAtRuntime           ImpactAnalysisJustification = "protected_at_runtime"
	IAJProtectedAtPerimeter         ImpactAnalysisJustification = "protected_at_perimeter"
	IAJProtectedByMitigatingControl ImpactAnalysisJustification = "protected_by_mitigating_control"
)

type ImpactAnalysisResponse string

const (
	IARCanNotFix           ImpactAnalysisResponse = "can_not_fix"
	IARWillNotFix          ImpactAnalysisResponse = "will_not_fix"
	IARUpdate              ImpactAnalysisResponse = "update"
	IARRollback            ImpactAnalysisResponse = "rollback"
	IARWorkaroundAvailable ImpactAnalysisResponse = "workaround_available"
)

type ImpactAnalysisState string

const (
	IASResolved             ImpactAnalysisState = "resolved"
	IASResolvedWithPedigree ImpactAnalysisState = "resolved_with_pedigree"
	IASExploitable          ImpactAnalysisState = "exploitable"
	IASInTriage             ImpactAnalysisState = "in_triage"
	IASFalsePositive        ImpactAnalysisState = "false_positive"
	IASNotAffected          ImpactAnalysisState = "not_affected"
)

type ImplementationPlatform string

const (
	ImplementationPlatformGeneric ImplementationPlatform = "generic"
	ImplementationPlatformX86_32  ImplementationPlatform = "x86_32"
	ImplementationPlatformX86_64  ImplementationPlatform = "x86_64"
	ImplementationPlatformARMv7A  ImplementationPlatform = "armv7-a"
	ImplementationPlatformARMv7M  ImplementationPlatform = "armv7-m"
	ImplementationPlatformARMv8A  ImplementationPlatform = "armv8-a"
	ImplementationPlatformARMv8M  ImplementationPlatform = "armv8-m"
	ImplementationPlatformARMv9A  ImplementationPlatform = "armv9-a"
	ImplementationPlatformARMv9M  ImplementationPlatform = "armv9-m"
	ImplementationPlatformS390x   ImplementationPlatform = "s390x"
	ImplementationPlatformPPC64   ImplementationPlatform = "ppc64"
	ImplementationPlatformPPC64LE ImplementationPlatform = "ppc64le"
	ImplementationPlatformOther   ImplementationPlatform = "other"
	ImplementationPlatformUnknown ImplementationPlatform = "unknown"
)

type Issue struct {
	ID          string    `json:"id" xml:"id"`
	Name        string    `json:"name,omitempty" xml:"name,omitempty"`
	Description string    `json:"description" xml:"description"`
	Source      *Source   `json:"source,omitempty" xml:"source,omitempty"`
	References  *[]string `json:"references,omitempty" xml:"references>url,omitempty"`
	Type        IssueType `json:"type" xml:"type,attr"`
}

type IssueType string

const (
	IssueTypeDefect      IssueType = "defect"
	IssueTypeEnhancement IssueType = "enhancement"
	IssueTypeSecurity    IssueType = "security"
)

type JSFSignature struct {
	*JSFSigner `json:"-" xml:"-"`

	Signers *[]JSFSigner `json:"signers,omitempty" xml:"-"`
	Chain   *[]JSFSigner `json:"chain,omitempty" xml:"-"`
}

type JSFSigner struct {
	Algorithm       string       `json:"algorithm" xml:"-"`
	KeyID           string       `json:"keyId,omitempty" xml:"-"`
	PublicKey       JSFPublicKey `json:"publicKey,omitempty" xml:"-"`
	CertificatePath *[]string    `json:"certificatePath,omitempty" xml:"-"`
	Excludes        *[]string    `json:"excludes,omitempty" xml:"-"`
	Value           string       `json:"value" xml:"-"`
}

type JSFPublicKey struct {
	KTY string `json:"kty,omitempty" xml:"-"`

	CRV string `json:"crv,omitempty" xml:"-"`
	X   string `json:"x,omitempty" xml:"-"`
	Y   string `json:"y,omitempty" xml:"-"`

	N string `json:"n,omitempty" xml:"-"`
	E string `json:"e,omitempty" xml:"-"`
}

type License struct {
	BOMRef          string                 `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	ID              string                 `json:"id,omitempty" xml:"id,omitempty"`
	Name            string                 `json:"name,omitempty" xml:"name,omitempty"`
	Acknowledgement LicenseAcknowledgement `json:"acknowledgement,omitempty" xml:"acknowledgement,attr,omitempty"`
	Text            *AttachedText          `json:"text,omitempty" xml:"text,omitempty"`
	URL             string                 `json:"url,omitempty" xml:"url,omitempty"`
	Licensing       *Licensing             `json:"licensing,omitempty" xml:"licensing,omitempty"`
	Properties      *[]Property            `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type LicenseAcknowledgement string

const (
	LicenseAcknowledgementDeclared  LicenseAcknowledgement = "declared"
	LicenseAcknowledgementConcluded LicenseAcknowledgement = "concluded"
)

type Licenses []LicenseChoice

type LicenseChoice struct {
	License    *License `json:"license,omitempty" xml:"-"`
	Expression string   `json:"expression,omitempty" xml:"-"`
}

type LicenseType string

const (
	LicenseTypeAcademic        LicenseType = "academic"
	LicenseTypeAppliance       LicenseType = "appliance"
	LicenseTypeClientAccess    LicenseType = "client-access"
	LicenseTypeConcurrentUser  LicenseType = "concurrent-user"
	LicenseTypeCorePoints      LicenseType = "core-points"
	LicenseTypeCustomMetric    LicenseType = "custom-metric"
	LicenseTypeDevice          LicenseType = "device"
	LicenseTypeEvaluation      LicenseType = "evaluation"
	LicenseTypeNamedUser       LicenseType = "named-user"
	LicenseTypeNodeLocked      LicenseType = "node-locked"
	LicenseTypeOEM             LicenseType = "oem"
	LicenseTypeOther           LicenseType = "other"
	LicenseTypePerpetual       LicenseType = "perpetual"
	LicenseTypeProcessorPoints LicenseType = "processor-points"
	LicenseTypeSubscription    LicenseType = "subscription"
	LicenseTypeUser            LicenseType = "user"
)

type Licensing struct {
	AltIDs        *[]string                      `json:"altIds,omitempty" xml:"altIds>altId,omitempty"`
	Licensor      *OrganizationalEntityOrContact `json:"licensor,omitempty" xml:"licensor,omitempty"`
	Licensee      *OrganizationalEntityOrContact `json:"licensee,omitempty" xml:"licensee,omitempty"`
	Purchaser     *OrganizationalEntityOrContact `json:"purchaser,omitempty" xml:"purchaser,omitempty"`
	PurchaseOrder string                         `json:"purchaseOrder,omitempty" xml:"purchaseOrder,omitempty"`
	LicenseTypes  *[]LicenseType                 `json:"licenseTypes,omitempty" xml:"licenseTypes>licenseType,omitempty"`
	LastRenewal   string                         `json:"lastRenewal,omitempty" xml:"lastRenewal,omitempty"`
	Expiration    string                         `json:"expiration,omitempty" xml:"expiration,omitempty"`
}

type Lifecycle struct {
	Name        string         `json:"name,omitempty" xml:"name,omitempty"`
	Phase       LifecyclePhase `json:"phase,omitempty" xml:"phase,omitempty"`
	Description string         `json:"description,omitempty" xml:"description,omitempty"`
}

type LifecyclePhase string

const (
	LifecyclePhaseBuild        LifecyclePhase = "build"
	LifecyclePhaseDecommission LifecyclePhase = "decommission"
	LifecyclePhaseDesign       LifecyclePhase = "design"
	LifecyclePhaseDiscovery    LifecyclePhase = "discovery"
	LifecyclePhaseOperations   LifecyclePhase = "operations"
	LifecyclePhasePostBuild    LifecyclePhase = "post-build"
	LifecyclePhasePreBuild     LifecyclePhase = "pre-build"
)

// MediaType defines the official media types for CycloneDX BOMs.
// See https://cyclonedx.org/specification/overview/#registered-media-types
type MediaType int

const (
	MediaTypeJSON     MediaType = iota + 1 // application/vnd.cyclonedx+json
	MediaTypeXML                           // application/vnd.cyclonedx+xml
	MediaTypeProtobuf                      // application/x.vnd.cyclonedx+protobuf
)

func (mt MediaType) WithVersion(specVersion SpecVersion) (string, error) {
	if mt == MediaTypeJSON && specVersion < SpecVersion1_2 {
		return "", fmt.Errorf("json format is not supported for specification versions lower than %s", SpecVersion1_2)
	}

	return fmt.Sprintf("%s; version=%s", mt, specVersion), nil
}

type Metadata struct {
	Timestamp    string                   `json:"timestamp,omitempty" xml:"timestamp,omitempty"`
	Lifecycles   *[]Lifecycle             `json:"lifecycles,omitempty" xml:"lifecycles>lifecycle,omitempty"`
	Tools        *ToolsChoice             `json:"tools,omitempty" xml:"tools,omitempty"`
	Authors      *[]OrganizationalContact `json:"authors,omitempty" xml:"authors>author,omitempty"`
	Component    *Component               `json:"component,omitempty" xml:"component,omitempty"`
	Manufacture  *OrganizationalEntity    `json:"manufacture,omitempty" xml:"manufacture,omitempty"` // Deprecated: Use Component Manufacturer instead.
	Manufacturer *OrganizationalEntity    `json:"manufacturer,omitempty" xml:"manufacturer,omitempty"`
	Supplier     *OrganizationalEntity    `json:"supplier,omitempty" xml:"supplier,omitempty"`
	Licenses     *Licenses                `json:"licenses,omitempty" xml:"licenses,omitempty"`
	Properties   *[]Property              `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type MLDatasetChoice struct {
	Ref           string         `json:"-" xml:"-"`
	ComponentData *ComponentData `json:"-" xml:"-"`
}

type MLInputOutputParameters struct {
	Format string `json:"format,omitempty" xml:"format,omitempty"`
}

type MLModelCard struct {
	BOMRef               string                     `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	ModelParameters      *MLModelParameters         `json:"modelParameters,omitempty" xml:"modelParameters,omitempty"`
	QuantitativeAnalysis *MLQuantitativeAnalysis    `json:"quantitativeAnalysis,omitempty" xml:"quantitativeAnalysis,omitempty"`
	Considerations       *MLModelCardConsiderations `json:"considerations,omitempty" xml:"considerations,omitempty"`
}

type MLModelCardConsiderations struct {
	Users                       *[]string                               `json:"users,omitempty" xml:"users>user,omitempty"`
	UseCases                    *[]string                               `json:"useCases,omitempty" xml:"useCases>useCase,omitempty"`
	TechnicalLimitations        *[]string                               `json:"technicalLimitations,omitempty" xml:"technicalLimitations>technicalLimitation,omitempty"`
	PerformanceTradeoffs        *[]string                               `json:"performanceTradeoffs,omitempty" xml:"performanceTradeoffs>performanceTradeoff,omitempty"`
	EthicalConsiderations       *[]MLModelCardEthicalConsideration      `json:"ethicalConsiderations,omitempty" xml:"ethicalConsiderations>ethicalConsideration,omitempty"`
	EnvironmentalConsiderations *MLModelCardEnvironmentalConsiderations `json:"environmentalConsiderations,omitempty" xml:"environmentalConsiderations,omitempty"`
	FairnessAssessments         *[]MLModelCardFairnessAssessment        `json:"fairnessAssessments,omitempty" xml:"fairnessAssessments>fairnessAssessment,omitempty"`
}

type MLModelCardEnvironmentalConsiderations struct {
	EnergyConsumptions *[]MLModelEnergyConsumption `json:"energyConsumptions,omitempty" xml:"energyConsumptions>energyConsumption,omitempty"`
	Properties         *[]Property                 `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type MLModelCardEthicalConsideration struct {
	Name               string `json:"name,omitempty" xml:"name,omitempty"`
	MitigationStrategy string `json:"mitigationStrategy,omitempty" xml:"mitigationStrategy,omitempty"`
}

type MLModelCardFairnessAssessment struct {
	GroupAtRisk        string `json:"groupAtRisk,omitempty" xml:"groupAtRisk,omitempty"`
	Benefits           string `json:"benefits,omitempty" xml:"benefits,omitempty"`
	Harms              string `json:"harms,omitempty" xml:"harms,omitempty"`
	MitigationStrategy string `json:"mitigationStrategy,omitempty" xml:"mitigationStrategy,omitempty"`
}

type MLModelCO2Measure struct {
	Value float32        `json:"value" xml:"value"`
	Unit  MLModelCO2Unit `json:"unit" xml:"unit"`
}

type MLModelCO2Unit string

const MLModelCO2UnitTCO2Eq MLModelCO2Unit = "tCO2eq"

type MLModelEnergyConsumption struct {
	Activity           MLModelEnergyConsumptionActivity `json:"activity" xml:"activity"`
	EnergyProviders    *[]MLModelEnergyProvider         `json:"energyProviders" xml:"energyProviders"`
	ActivityEnergyCost MLModelEnergyMeasure             `json:"activityEnergyCost" xml:"activityEnergyCost"`
	CO2CostEquivalent  *MLModelCO2Measure               `json:"co2CostEquivalent,omitempty" xml:"co2CostEquivalent,omitempty"`
	CO2CostOffset      *MLModelCO2Measure               `json:"co2CostOffset,omitempty" xml:"co2CostOffset,omitempty"`
	Properties         *[]Property                      `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type MLModelEnergyConsumptionActivity string

const (
	MLModelEnergyConsumptionActivityDesign          MLModelEnergyConsumptionActivity = "design"
	MLModelEnergyConsumptionActivityDataCollection  MLModelEnergyConsumptionActivity = "data-collection"
	MLModelEnergyConsumptionActivityDataPreparation MLModelEnergyConsumptionActivity = "data-preparation"
	MLModelEnergyConsumptionActivityTraining        MLModelEnergyConsumptionActivity = "training"
	MLModelEnergyConsumptionActivityFineTuning      MLModelEnergyConsumptionActivity = "fine-tuning"
	MLModelEnergyConsumptionActivityValidation      MLModelEnergyConsumptionActivity = "validation"
	MLModelEnergyConsumptionActivityDeployment      MLModelEnergyConsumptionActivity = "deployment"
	MLModelEnergyConsumptionActivityInference       MLModelEnergyConsumptionActivity = "inference"
	MLModelEnergyConsumptionActivityOther           MLModelEnergyConsumptionActivity = "other"
)

type MLModelEnergyMeasure struct {
	Value float32           `json:"value" xml:"value"`
	Unit  MLModelEnergyUnit `json:"unit" xml:"unit"`
}

type MLModelEnergyProvider struct {
	BOMRef             string                `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Description        string                `json:"description,omitempty" xml:"description,omitempty"`
	Organization       *OrganizationalEntity `json:"organization" xml:"organization"`
	EnergySource       MLModelEnergySource   `json:"energySource" xml:"energySource"`
	EnergyProvided     *MLModelEnergyMeasure `json:"energyProvided" xml:"energyProvided"`
	ExternalReferences *[]ExternalReference  `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
}

type MLModelEnergySource string

const (
	MLModelEnergySourceCoal       MLModelEnergySource = "coal"
	MLModelEnergySourceOil        MLModelEnergySource = "oil"
	MLModelEnergySourceNaturalGas MLModelEnergySource = "natural-gas"
	MLModelEnergySourceNuclear    MLModelEnergySource = "nuclear"
	MLModelEnergySourceWind       MLModelEnergySource = "wind"
	MLModelEnergySourceSolar      MLModelEnergySource = "solar"
	MLModelEnergySourceGeothermal MLModelEnergySource = "geothermal"
	MLModelEnergySourceHydropower MLModelEnergySource = "hydropower"
	MLModelEnergySourceBiofuel    MLModelEnergySource = "biofuel"
	MLModelEnergySourceUnknown    MLModelEnergySource = "unknown"
	MLModelEnergySourceOther      MLModelEnergySource = "other"
)

type MLModelEnergyUnit string

const MLModelEnergyUnitKWH MLModelEnergyUnit = "kWh"

type MLModelParameters struct {
	Approach           *MLModelParametersApproach `json:"approach,omitempty" xml:"approach,omitempty"`
	Task               string                     `json:"task,omitempty" xml:"task,omitempty"`
	ArchitectureFamily string                     `json:"architectureFamily,omitempty" xml:"architectureFamily,omitempty"`
	ModelArchitecture  string                     `json:"modelArchitecture,omitempty" xml:"modelArchitecture,omitempty"`
	Datasets           *[]MLDatasetChoice         `json:"datasets,omitempty" xml:"datasets>dataset,omitempty"`
	Inputs             *[]MLInputOutputParameters `json:"inputs,omitempty" xml:"inputs>input,omitempty"`
	Outputs            *[]MLInputOutputParameters `json:"outputs,omitempty" xml:"outputs>output,omitempty"`
}

type MLModelParametersApproach struct {
	Type MLModelParametersApproachType `json:"type,omitempty" xml:"type,omitempty"`
}

type MLModelParametersApproachType string

const (
	MLModelParametersApproachTypeSupervised            MLModelParametersApproachType = "supervised"
	MLModelParametersApproachTypeUnsupervised          MLModelParametersApproachType = "unsupervised"
	MLModelParametersApproachTypeReinforcementLearning MLModelParametersApproachType = "reinforcement-learning"
	MLModelParametersApproachTypeSemiSupervised        MLModelParametersApproachType = "semi-supervised"
	MLModelParametersApproachTypeSelfSupervised        MLModelParametersApproachType = "self-supervised"
)

type MLQuantitativeAnalysis struct {
	PerformanceMetrics *[]MLPerformanceMetric `json:"performanceMetrics,omitempty" xml:"performanceMetrics>performanceMetric,omitempty"`
	Graphics           *ComponentDataGraphics `json:"graphics,omitempty" xml:"graphics,omitempty"`
}

type MLPerformanceMetric struct {
	Type               string                                 `json:"type,omitempty" xml:"type,omitempty"`
	Value              string                                 `json:"value,omitempty" xml:"value,omitempty"`
	Slice              string                                 `json:"slice,omitempty" xml:"slice,omitempty"`
	ConfidenceInterval *MLPerformanceMetricConfidenceInterval `json:"confidenceInterval,omitempty" xml:"confidenceInterval,omitempty"`
}

type MLPerformanceMetricConfidenceInterval struct {
	LowerBound string `json:"lowerBound,omitempty" xml:"lowerBound,omitempty"`
	UpperBound string `json:"upperBound,omitempty" xml:"upperBound,omitempty"`
}

type Note struct {
	Locale string       `json:"locale,omitempty" xml:"locale,omitempty"`
	Text   AttachedText `json:"text" xml:"text"`
}

type OrganizationalContact struct {
	BOMRef string `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Name   string `json:"name,omitempty" xml:"name,omitempty"`
	Email  string `json:"email,omitempty" xml:"email,omitempty"`
	Phone  string `json:"phone,omitempty" xml:"phone,omitempty"`
}

type OrganizationalEntity struct {
	BOMRef  string                   `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Name    string                   `json:"name" xml:"name"`
	Address *PostalAddress           `json:"address,omitempty" xml:"address,omitempty"`
	URL     *[]string                `json:"url,omitempty" xml:"url,omitempty"`
	Contact *[]OrganizationalContact `json:"contact,omitempty" xml:"contact,omitempty"`
}

type OrganizationalEntityOrContact struct {
	Organization *OrganizationalEntity  `json:"organization,omitempty" xml:"organization,omitempty"`
	Individual   *OrganizationalContact `json:"individual,omitempty" xml:"individual,omitempty"`
}

type Parameter struct {
	Name     string `json:"name,omitempty" xml:"name,omitempty"`
	Value    string `json:"value,omitempty" xml:"value,omitempty"`
	DataType string `json:"dataType,omitempty" xml:"dataType,omitempty"`
}

type Patch struct {
	Diff     *Diff     `json:"diff,omitempty" xml:"diff,omitempty"`
	Resolves *[]Issue  `json:"resolves,omitempty" xml:"resolves>issue,omitempty"`
	Type     PatchType `json:"type" xml:"type,attr"`
}

type PatchType string

const (
	PatchTypeBackport   PatchType = "backport"
	PatchTypeCherryPick PatchType = "cherry-pick"
	PatchTypeMonkey     PatchType = "monkey"
	PatchTypeUnofficial PatchType = "unofficial"
)

type Pedigree struct {
	Ancestors   *[]Component `json:"ancestors,omitempty" xml:"ancestors>component,omitempty"`
	Descendants *[]Component `json:"descendants,omitempty" xml:"descendants>component,omitempty"`
	Variants    *[]Component `json:"variants,omitempty" xml:"variants>component,omitempty"`
	Commits     *[]Commit    `json:"commits,omitempty" xml:"commits>commit,omitempty"`
	Patches     *[]Patch     `json:"patches,omitempty" xml:"patches>patch,omitempty"`
	Notes       string       `json:"notes,omitempty" xml:"notes,omitempty"`
}

type PostalAddress struct {
	BOMRef              string `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Country             string `json:"country,omitempty" xml:"country,omitempty"`
	Region              string `json:"region,omitempty" xml:"region,omitempty"`
	Locality            string `json:"locality,omitempty" xml:"locality,omitempty"`
	PostOfficeBoxNumber string `json:"postOfficeBoxNumber,omitempty" xml:"postOfficeBoxNumber,omitempty"`
	PostalCode          string `json:"postalCode,omitempty" xml:"postalCode,omitempty"`
	StreetAddress       string `json:"streetAddress,omitempty" xml:"streetAddress,omitempty"`
}

type ProofOfConcept struct {
	ReproductionSteps  string          `json:"reproductionSteps,omitempty" xml:"reproductionSteps,omitempty"`
	Environment        string          `json:"environment,omitempty" xml:"environment,omitempty"`
	SupportingMaterial *[]AttachedText `json:"supportingMaterial,omitempty" xml:"supportingMaterial>attachment,omitempty"`
}

type Property struct {
	Name  string `json:"name" xml:"name,attr"`
	Value string `json:"value" xml:",chardata"`
}

type RelatedCryptoMaterialProperties struct {
	Type           RelatedCryptoMaterialType `json:"type,omitempty" xml:"type,omitempty"`
	ID             string                    `json:"id,omitempty" xml:"id,omitempty"`
	State          CryptoKeyState            `json:"state,omitempty" xml:"state,omitempty"`
	AlgorithmRef   BOMReference              `json:"algorithmRef,omitempty" xml:"algorithmRef,omitempty"`
	CreationDate   string                    `json:"creationDate,omitempty" xml:"creationDate,omitempty"`
	ActivationDate string                    `json:"activationDate,omitempty" xml:"activationDate,omitempty"`
	UpdateDate     string                    `json:"updateDate,omitempty" xml:"updateDate,omitempty"`
	ExpirationDate string                    `json:"expirationDate,omitempty" xml:"expirationDate,omitempty"`
	Value          string                    `json:"value,omitempty" xml:"value,omitempty"`
	Size           *int                      `json:"size,omitempty" xml:"size,omitempty"`
	Format         string                    `json:"format,omitempty" xml:"format,omitempty"`
	SecuredBy      *SecuredBy                `json:"securedBy,omitempty" xml:"securedBy,omitempty"`
}

type RelatedCryptoMaterialType string

const (
	RelatedCryptoMaterialTypePrivateKey           RelatedCryptoMaterialType = "private-key"
	RelatedCryptoMaterialTypePublicKey            RelatedCryptoMaterialType = "public-key"
	RelatedCryptoMaterialTypeSecretKey            RelatedCryptoMaterialType = "secret-key"
	RelatedCryptoMaterialTypeKey                  RelatedCryptoMaterialType = "key"
	RelatedCryptoMaterialTypeCiphertext           RelatedCryptoMaterialType = "ciphertext"
	RelatedCryptoMaterialTypeSignature            RelatedCryptoMaterialType = "signature"
	RelatedCryptoMaterialTypeDigest               RelatedCryptoMaterialType = "digest"
	RelatedCryptoMaterialTypeInitializationVector RelatedCryptoMaterialType = "initialization-vector"
	RelatedCryptoMaterialTypeNonce                RelatedCryptoMaterialType = "nonce"
	RelatedCryptoMaterialTypeSeed                 RelatedCryptoMaterialType = "seed"
	RelatedCryptoMaterialTypeSalt                 RelatedCryptoMaterialType = "salt"
	RelatedCryptoMaterialTypeSharedSecret         RelatedCryptoMaterialType = "shared-secret"
	RelatedCryptoMaterialTypeTag                  RelatedCryptoMaterialType = "tag"
	RelatedCryptoMaterialTypeAdditionalData       RelatedCryptoMaterialType = "additional-data"
	RelatedCryptoMaterialTypePassword             RelatedCryptoMaterialType = "password"
	RelatedCryptoMaterialTypeCredential           RelatedCryptoMaterialType = "credential"
	RelatedCryptoMaterialTypeToken                RelatedCryptoMaterialType = "token"
	RelatedCryptoMaterialTypeOther                RelatedCryptoMaterialType = "other"
	RelatedCryptoMaterialTypeUnknown              RelatedCryptoMaterialType = "unknown"
)

type ReleaseNotes struct {
	Type          string      `json:"type" xml:"type"`
	Title         string      `json:"title,omitempty" xml:"title,omitempty"`
	FeaturedImage string      `json:"featuredImage,omitempty" xml:"featuredImage,omitempty"`
	SocialImage   string      `json:"socialImage,omitempty" xml:"socialImage,omitempty"`
	Description   string      `json:"description,omitempty" xml:"description,omitempty"`
	Timestamp     string      `json:"timestamp,omitempty" xml:"timestamp,omitempty"`
	Aliases       *[]string   `json:"aliases,omitempty" xml:"aliases>alias,omitempty"`
	Tags          *[]string   `json:"tags,omitempty" xml:"tags>tag,omitempty"`
	Resolves      *[]Issue    `json:"resolves,omitempty" xml:"resolves>issue,omitempty"`
	Notes         *[]Note     `json:"notes,omitempty" xml:"notes>note,omitempty"`
	Properties    *[]Property `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type ResourceReferenceChoice struct {
	Ref               string             `json:"ref,omitempty" xml:"ref,omitempty"`
	ExternalReference *ExternalReference `json:"externalReference,omitempty" xml:"externalReference,omitempty"`
}

type Scope string

const (
	ScopeExcluded Scope = "excluded"
	ScopeOptional Scope = "optional"
	ScopeRequired Scope = "required"
)

type ScoringMethod string

const (
	ScoringMethodOther   ScoringMethod = "other"
	ScoringMethodCVSSv2  ScoringMethod = "CVSSv2"
	ScoringMethodCVSSv3  ScoringMethod = "CVSSv3"
	ScoringMethodCVSSv31 ScoringMethod = "CVSSv31"
	ScoringMethodCVSSv4  ScoringMethod = "CVSSv4"
	ScoringMethodOWASP   ScoringMethod = "OWASP"
	ScoringMethodSSVC    ScoringMethod = "SSVC"
)

type Service struct {
	BOMRef               string                `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Provider             *OrganizationalEntity `json:"provider,omitempty" xml:"provider,omitempty"`
	Group                string                `json:"group,omitempty" xml:"group,omitempty"`
	Name                 string                `json:"name" xml:"name"`
	Version              string                `json:"version,omitempty" xml:"version,omitempty"`
	Description          string                `json:"description,omitempty" xml:"description,omitempty"`
	Endpoints            *[]string             `json:"endpoints,omitempty" xml:"endpoints>endpoint,omitempty"`
	Authenticated        *bool                 `json:"authenticated,omitempty" xml:"authenticated,omitempty"`
	CrossesTrustBoundary *bool                 `json:"x-trust-boundary,omitempty" xml:"x-trust-boundary,omitempty"`
	Data                 *[]DataClassification `json:"data,omitempty" xml:"data>classification,omitempty"`
	Licenses             *Licenses             `json:"licenses,omitempty" xml:"licenses,omitempty"`
	ExternalReferences   *[]ExternalReference  `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
	Properties           *[]Property           `json:"properties,omitempty" xml:"properties>property,omitempty"`
	Services             *[]Service            `json:"services,omitempty" xml:"services>service,omitempty"`
	ReleaseNotes         *ReleaseNotes         `json:"releaseNotes,omitempty" xml:"releaseNotes,omitempty"`
	Tags                 *[]string             `json:"tags,omitempty" xml:"tags>tag,omitempty"`
	TrustZone            string                `json:"trustZone,omitempty" xml:"trustZone,omitempty"`
	Signature            *JSFSignature         `json:"signature,omitempty" xml:"-"`
}

type Severity string

const (
	SeverityUnknown  Severity = "unknown"
	SeverityNone     Severity = "none"
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var serialNumberRegex = regexp.MustCompile(`^urn:uuid:[\da-f]{8}-[\da-f]{4}-[\da-f]{4}-[\da-f]{4}-[\da-f]{12}$`)

type Source struct {
	Name string `json:"name,omitempty" xml:"name,omitempty"`
	URL  string `json:"url,omitempty" xml:"url,omitempty"`
}

type SpecVersion int

const (
	SpecVersion1_0 SpecVersion = iota + 1 // 1.0
	SpecVersion1_1                        // 1.1
	SpecVersion1_2                        // 1.2
	SpecVersion1_3                        // 1.3
	SpecVersion1_4                        // 1.4
	SpecVersion1_5                        // 1.5
	SpecVersion1_6                        // 1.6
)

type StandardDefinition struct {
	BOMRef      string `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Name        string `json:"name,omitempty" xml:"name,omitempty"`
	Version     string `json:"version,omitempty" xml:"version,omitempty"`
	Description string `json:"description,omitempty" xml:"description,omitempty"`
	Owner       string `json:"owner,omitempty" xml:"owner,omitempty"`

	Requirements       *[]StandardRequirement `json:"requirements,omitempty" xml:"requirements>requirement,omitempty"`
	Levels             *[]StandardLevel       `json:"levels,omitempty" xml:"levels>level,omitempty"`
	ExternalReferences *[]ExternalReference   `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
	Signature          *JSFSignature          `json:"signature,omitempty" xml:"-"`
}

type StandardRequirement struct {
	BOMRef             string               `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Identifier         string               `json:"identifier,omitempty" xml:"identifier,omitempty"`
	Title              string               `json:"title,omitempty" xml:"title,omitempty"`
	Text               string               `json:"text,omitempty" xml:"text,omitempty"`
	Descriptions       *[]string            `json:"descriptions,omitempty" xml:"descriptions>description,omitempty"`
	OpenCRE            *[]string            `json:"openCre,omitempty" xml:"openCre,omitempty"`
	Parent             string               `json:"parent,omitempty" xml:"parent,omitempty"`
	Properties         *[]Property          `json:"properties,omitempty" xml:"properties>property,omitempty"`
	ExternalReferences *[]ExternalReference `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
}

type StandardLevel struct {
	BOMRef       string    `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	Identifier   string    `json:"identifier,omitempty" xml:"identifier,omitempty"`
	Title        string    `json:"title,omitempty" xml:"title,omitempty"`
	Description  string    `json:"description,omitempty" xml:"description,omitempty"`
	Requirements *[]string `json:"requirements,omitempty" xml:"requirements>requirement,omitempty"`
}

type SWID struct {
	Text       *AttachedText `json:"text,omitempty" xml:"text,omitempty"`
	URL        string        `json:"url,omitempty" xml:"url,attr,omitempty"`
	TagID      string        `json:"tagId" xml:"tagId,attr"`
	Name       string        `json:"name" xml:"name,attr"`
	Version    string        `json:"version,omitempty" xml:"version,attr,omitempty"`
	TagVersion *int          `json:"tagVersion,omitempty" xml:"tagVersion,attr,omitempty"`
	Patch      *bool         `json:"patch,omitempty" xml:"patch,attr,omitempty"`
}

type Task struct {
	BOMRef             string                     `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	UID                string                     `json:"uid,omitempty" xml:"uid,omitempty"`
	Name               string                     `json:"name,omitempty" xml:"name,omitempty"`
	Description        string                     `json:"description,omitempty" xml:"description,omitempty"`
	Properties         *[]Property                `json:"properties,omitempty" xml:"properties>property,omitempty"`
	ResourceReferences *[]ResourceReferenceChoice `json:"resourceReferences,omitempty" xml:"resourceReferences>resourceReference,omitempty"`
	TaskTypes          *[]TaskType                `json:"taskTypes,omitempty" xml:"taskTypes>taskType,omitempty"`
	Trigger            *TaskTrigger               `json:"trigger,omitempty" xml:"trigger,omitempty"`
	Steps              *[]TaskStep                `json:"steps,omitempty" xml:"steps>step,omitempty"`
	Inputs             *[]TaskInput               `json:"inputs,omitempty" xml:"inputs>input,omitempty"`
	Outputs            *[]TaskOutput              `json:"outputs,omitempty" xml:"outputs>output,omitempty"`
	TimeStart          string                     `json:"timeStart,omitempty" xml:"timeStart,omitempty"`
	TimeEnd            string                     `json:"timeEnd,omitempty" xml:"timeEnd,omitempty"`
	Workspaces         *[]TaskWorkspace           `json:"workspaces,omitempty" xml:"workspaces>workspace,omitempty"`
	RuntimeTopology    *[]Dependency              `json:"runtimeTopology,omitempty" xml:"runtimeTopology>dependency,omitempty"`
}

type TaskCommand struct {
	Executed   string      `json:"executed,omitempty" xml:"executed,omitempty"`
	Properties *[]Property `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskInput struct {
	Resource        *ResourceReferenceChoice `json:"resource,omitempty" xml:"resource,omitempty"`
	Parameters      *[]Parameter             `json:"parameters,omitempty" xml:"parameters>parameter,omitempty"`
	EnvironmentVars *EnvironmentVariables    `json:"environmentVars,omitempty" xml:"environmentVars,omitempty"`
	Data            *AttachedText            `json:"data,omitempty" xml:"data,omitempty"`
	Source          *ResourceReferenceChoice `json:"source,omitempty" xml:"source,omitempty"`
	Target          *ResourceReferenceChoice `json:"target,omitempty" xml:"target,omitempty"`
	Properties      *[]Property              `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskOutput struct {
	Resource        *ResourceReferenceChoice `json:"resource,omitempty" xml:"resource,omitempty"`
	Parameters      *[]Parameter             `json:"parameters,omitempty" xml:"parameters>parameter,omitempty"`
	EnvironmentVars *EnvironmentVariables    `json:"environmentVars,omitempty" xml:"environmentVars,omitempty"`
	Data            *AttachedText            `json:"data,omitempty" xml:"data,omitempty"`
	Type            TaskOutputType           `json:"type,omitempty" xml:"type,omitempty"`
	Source          *ResourceReferenceChoice `json:"source,omitempty" xml:"source,omitempty"`
	Target          *ResourceReferenceChoice `json:"target,omitempty" xml:"target,omitempty"`
	Properties      *[]Property              `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskOutputType string

const (
	TaskOutputTypeArtifact    TaskOutputType = "artifact"
	TaskOutputTypeAttestation TaskOutputType = "attestation"
	TaskOutputTypeEvidence    TaskOutputType = "evidence"
	TaskOutputTypeLog         TaskOutputType = "log"
	TaskOutputTypeMetrics     TaskOutputType = "metrics"
	TaskOutputTypeOther       TaskOutputType = "other"
)

type TaskStep struct {
	Name        string         `json:"name,omitempty" xml:"name,omitempty"`
	Description string         `json:"description,omitempty" xml:"description,omitempty"`
	Commands    *[]TaskCommand `json:"commands,omitempty" xml:"commands>command,omitempty"`
	Properties  *[]Property    `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskTrigger struct {
	BOMRef             string                     `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	UID                string                     `json:"uid,omitempty" xml:"uid,omitempty"`
	Name               string                     `json:"name,omitempty" xml:"name,omitempty"`
	Description        string                     `json:"description,omitempty" xml:"description,omitempty"`
	ResourceReferences *[]ResourceReferenceChoice `json:"resourceReferences,omitempty" xml:"resourceReferences>resourceReference,omitempty"`
	Type               TaskTriggerType            `json:"type,omitempty" xml:"type,omitempty"`
	Event              *Event                     `json:"event,omitempty" xml:"event,omitempty"`
	Conditions         *[]TaskTriggerCondition    `json:"conditions,omitempty" xml:"conditions>condition,omitempty"`
	TimeActivated      string                     `json:"timeActivated,omitempty" xml:"timeActivated,omitempty"`
	Inputs             *[]TaskInput               `json:"inputs,omitempty" xml:"inputs>input,omitempty"`
	Outputs            *[]TaskOutput              `json:"outputs,omitempty" xml:"outputs>output,omitempty"`
	Properties         *[]Property                `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskTriggerCondition struct {
	Description string      `json:"description,omitempty" xml:"description,omitempty"`
	Expression  string      `json:"expression,omitempty" xml:"expression,omitempty"`
	Properties  *[]Property `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskTriggerType string

const (
	TaskTriggerTypeAPI       TaskTriggerType = "api"
	TaskTriggerTypeManual    TaskTriggerType = "manual"
	TaskTriggerTypeScheduled TaskTriggerType = "scheduled"
	TaskTriggerTypeWebhook   TaskTriggerType = "webhook"
)

type TaskType string

const (
	TaskTypeBuild   TaskType = "build"
	TaskTypeClean   TaskType = "clean"
	TaskTypeClone   TaskType = "clone"
	TaskTypeCopy    TaskType = "copy"
	TaskTypeDeliver TaskType = "deliver"
	TaskTypeDeploy  TaskType = "deploy"
	TaskTypeLint    TaskType = "lint"
	TaskTypeMerge   TaskType = "merge"
	TaskTypeOther   TaskType = "other"
	TaskTypeRelease TaskType = "release"
	TaskTypeScan    TaskType = "scan"
	TaskTypeTest    TaskType = "test"
)

type TaskWorkspace struct {
	BOMRef             string                     `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	UID                string                     `json:"uid,omitempty" xml:"uid,omitempty"`
	Name               string                     `json:"name,omitempty" xml:"name,omitempty"`
	Aliases            *[]string                  `json:"aliases,omitempty" xml:"aliases>alias,omitempty"`
	Description        string                     `json:"description,omitempty" xml:"description,omitempty"`
	ResourceReferences *[]ResourceReferenceChoice `json:"resourceReferences,omitempty" xml:"resourceReferences>resourceReference,omitempty"`
	AccessMode         TaskWorkspaceAccessMode    `json:"accessMode,omitempty" xml:"accessMode,omitempty"`
	MountPath          string                     `json:"mountPath,omitempty" xml:"mountPath,omitempty"`
	ManagedDataType    string                     `json:"managedDataType,omitempty" xml:"managedDataType,omitempty"`
	VolumeRequest      string                     `json:"volumeRequest,omitempty" xml:"volumeRequest,omitempty"`
	Volume             *Volume                    `json:"volume,omitempty" xml:"volume,omitempty"`
	Properties         *[]Property                `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type TaskWorkspaceAccessMode string

const (
	TaskWorkspaceAccessModeReadOnly      TaskWorkspaceAccessMode = "read-only"
	TaskWorkspaceAccessModeReadWrite     TaskWorkspaceAccessMode = "read-write"
	TaskWorkspaceAccessModeReadWriteOnce TaskWorkspaceAccessMode = "read-write-once"
	TaskWorkspaceAccessModeWriteOnce     TaskWorkspaceAccessMode = "write-once"
	TaskWorkspaceAccessModeWriteOnly     TaskWorkspaceAccessMode = "write-only"
)

// Deprecated: Use Component or Service instead.
type Tool struct {
	Vendor             string               `json:"vendor,omitempty" xml:"vendor,omitempty"`
	Name               string               `json:"name" xml:"name"`
	Version            string               `json:"version,omitempty" xml:"version,omitempty"`
	Hashes             *[]Hash              `json:"hashes,omitempty" xml:"hashes>hash,omitempty"`
	ExternalReferences *[]ExternalReference `json:"externalReferences,omitempty" xml:"externalReferences>reference,omitempty"`
}

// ToolsChoice represents a union of either Tools (deprecated as of CycloneDX v1.5), and Components or Services.
//
// Encoding or decoding a ToolsChoice with both options present will raise an error.
// When encoding to a SpecVersion lower than SpecVersion1_5, and Components or Services are set,
// they will be automatically converted to legacy Tools.
//
// It is strongly recommended to use Components and Services. However, when consuming BOMs,
// applications should still expect legacy Tools to be present, and handle them accordingly.
type ToolsChoice struct {
	Tools      *[]Tool      `json:"-" xml:"-"` // Deprecated: Use Components and Services instead.
	Components *[]Component `json:"-" xml:"-"`
	Services   *[]Service   `json:"-" xml:"-"`
}

type Volume struct {
	UID           string      `json:"uid,omitempty" xml:"uid,omitempty"`
	Name          string      `json:"name,omitempty" xml:"name,omitempty"`
	Mode          VolumeMode  `json:"mode,omitempty" xml:"mode,omitempty"`
	Path          string      `json:"path,omitempty" xml:"path,omitempty"`
	SizeAllocated string      `json:"sizeAllocated,omitempty" xml:"sizeAllocated,omitempty"`
	Persistent    *bool       `json:"persistent,omitempty" xml:"persistent,omitempty"`
	Remote        *bool       `json:"remote,omitempty" xml:"remote,omitempty"`
	Properties    *[]Property `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type VolumeMode string

const (
	VolumeModeBlock      VolumeMode = "block"
	VolumeModeFilesystem VolumeMode = "file-system"
)

type Vulnerability struct {
	BOMRef         string                    `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	ID             string                    `json:"id" xml:"id"`
	Source         *Source                   `json:"source,omitempty" xml:"source,omitempty"`
	References     *[]VulnerabilityReference `json:"references,omitempty" xml:"references>reference,omitempty"`
	Ratings        *[]VulnerabilityRating    `json:"ratings,omitempty" xml:"ratings>rating,omitempty"`
	CWEs           *[]int                    `json:"cwes,omitempty" xml:"cwes>cwe,omitempty"`
	Description    string                    `json:"description,omitempty" xml:"description,omitempty"`
	Detail         string                    `json:"detail,omitempty" xml:"detail,omitempty"`
	Recommendation string                    `json:"recommendation,omitempty" xml:"recommendation,omitempty"`
	Workaround     string                    `json:"workaround,omitempty" xml:"workaround,omitempty"`
	ProofOfConcept *ProofOfConcept           `json:"proofOfConcept,omitempty" xml:"proofOfConcept,omitempty"`
	Advisories     *[]Advisory               `json:"advisories,omitempty" xml:"advisories>advisory,omitempty"`
	Created        string                    `json:"created,omitempty" xml:"created,omitempty"`
	Published      string                    `json:"published,omitempty" xml:"published,omitempty"`
	Updated        string                    `json:"updated,omitempty" xml:"updated,omitempty"`
	Rejected       string                    `json:"rejected,omitempty" xml:"rejected,omitempty"`
	Credits        *Credits                  `json:"credits,omitempty" xml:"credits,omitempty"`
	Tools          *ToolsChoice              `json:"tools,omitempty" xml:"tools,omitempty"`
	Analysis       *VulnerabilityAnalysis    `json:"analysis,omitempty" xml:"analysis,omitempty"`
	Affects        *[]Affects                `json:"affects,omitempty" xml:"affects>target,omitempty"`
	Properties     *[]Property               `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

type VulnerabilityAnalysis struct {
	State         ImpactAnalysisState         `json:"state,omitempty" xml:"state,omitempty"`
	Justification ImpactAnalysisJustification `json:"justification,omitempty" xml:"justification,omitempty"`
	Response      *[]ImpactAnalysisResponse   `json:"response,omitempty" xml:"responses>response,omitempty"`
	Detail        string                      `json:"detail,omitempty" xml:"detail,omitempty"`
	FirstIssued   string                      `json:"firstIssued,omitempty" xml:"firstIssued,omitempty"`
	LastUpdated   string                      `json:"lastUpdated,omitempty" xml:"lastUpdated,omitempty"`
}

type VulnerabilityRating struct {
	Source        *Source       `json:"source,omitempty" xml:"source,omitempty"`
	Score         *float64      `json:"score,omitempty" xml:"score,omitempty"`
	Severity      Severity      `json:"severity,omitempty" xml:"severity,omitempty"`
	Method        ScoringMethod `json:"method,omitempty" xml:"method,omitempty"`
	Vector        string        `json:"vector,omitempty" xml:"vector,omitempty"`
	Justification string        `json:"justification,omitempty" xml:"justification,omitempty"`
}

type VulnerabilityReference struct {
	ID     string  `json:"id,omitempty" xml:"id,omitempty"`
	Source *Source `json:"source,omitempty" xml:"source,omitempty"`
}

type VulnerabilityStatus string

const (
	VulnerabilityStatusUnknown     VulnerabilityStatus = "unknown"
	VulnerabilityStatusAffected    VulnerabilityStatus = "affected"
	VulnerabilityStatusNotAffected VulnerabilityStatus = "unaffected"
)

type Workflow struct {
	BOMRef             string                     `json:"bom-ref,omitempty" xml:"bom-ref,attr,omitempty"`
	UID                string                     `json:"uid,omitempty" xml:"uid,omitempty"`
	Name               string                     `json:"name,omitempty" xml:"name,omitempty"`
	Description        string                     `json:"description,omitempty" xml:"description,omitempty"`
	ResourceReferences *[]ResourceReferenceChoice `json:"resourceReferences,omitempty" xml:"resourceReferences>resourceReference,omitempty"`
	Tasks              *[]Task                    `json:"tasks,omitempty" xml:"tasks>task,omitempty"`
	TaskDependencies   *[]Dependency              `json:"taskDependencies,omitempty" xml:"taskDependencies>dependency"`
	TaskTypes          *[]TaskType                `json:"taskTypes,omitempty" xml:"taskTypes>taskType,omitempty"`
	Trigger            *TaskTrigger               `json:"trigger,omitempty" xml:"trigger,omitempty"`
	Steps              *[]TaskStep                `json:"steps,omitempty" xml:"steps>step,omitempty"`
	Inputs             *[]TaskInput               `json:"inputs,omitempty" xml:"inputs>input,omitempty"`
	Outputs            *[]TaskOutput              `json:"outputs,omitempty" xml:"outputs>output,omitempty"`
	TimeStart          string                     `json:"timeStart,omitempty" xml:"timeStart,omitempty"`
	TimeEnd            string                     `json:"timeEnd,omitempty" xml:"timeEnd,omitempty"`
	Workspaces         *[]TaskWorkspace           `json:"workspaces,omitempty" xml:"workspaces>workspace,omitempty"`
	RuntimeTopology    *[]Dependency              `json:"runtimeTopology,omitempty" xml:"runtimeTopology>dependency,omitempty"`
	Properties         *[]Property                `json:"properties,omitempty" xml:"properties>property,omitempty"`
}

// Package definition contains shared types used across the provider definition.
//
// +k8s:openapi-gen=true
package definition

// Parameters defines instance-wide structured parameters that apply to the
// entire cluster (Instance.spec.parameters).
type Parameters struct {
	// TLS configures encryption in transit between clients and the instance.
	// +optional
	TLS *TLSParameters `json:"tls,omitempty"`
}

// TLSMode controls which client connections must use TLS. CloudNativePG
// always serves TLS and replicates over it, so TLS cannot be turned off.
// +enum
type TLSMode string

const (
	// TLSModePrefer accepts clients with or without TLS. The default.
	TLSModePrefer TLSMode = "preferTLS"
	// TLSModeRequire rejects clients without TLS, at PostgreSQL and at the
	// proxy component's PgBouncer.
	TLSModeRequire TLSMode = "requireTLS"
)

// TLSParameters configures TLS for the instance. The server certificate comes
// from, in order of precedence: certificateSecretName, the cert-manager
// issuer, or else a CA CloudNativePG creates for the instance. Certificates
// and issuer can only be set when the instance is created; the certificates
// in the Secrets can be renewed at any time, without a restart.
type TLSParameters struct {
	// Mode controls which client connections must use TLS: preferTLS or
	// requireTLS. Changing it is applied without a restart.
	// +default="preferTLS"
	// +optional
	Mode TLSMode `json:"mode,omitempty"`

	// CertificateSecretName names a Secret in the instance namespace with the
	// server certificate, followed by any intermediate CA certificates
	// (tls.crt), its private key (tls.key) and the CA certificate clients
	// verify it with (ca.crt). The certificate must be valid for the names
	// clients connect to, such as <instance>-rw.<namespace>.svc. Cannot be
	// combined with issuer or additionalNames.
	// +optional
	CertificateSecretName string `json:"certificateSecretName,omitempty"`

	// InternalCertificateSecretName names a Secret in the instance namespace
	// with the client certificate replicas authenticate to the primary with,
	// in the same format: issued to the streaming_replica user (common name)
	// for client authentication, and signed by the CA in its ca.crt. Without
	// it CloudNativePG issues the replication certificate. Cannot be combined
	// with the proxy component, whose client certificate CloudNativePG issues
	// with the private key of that CA.
	// +optional
	InternalCertificateSecretName string `json:"internalCertificateSecretName,omitempty"`

	// Issuer is the cert-manager issuer that signs the server certificate,
	// instead of the CA CloudNativePG creates. The provider requests it as
	// the Certificate <instance>-tls and cert-manager renews it. The issuer
	// must write its CA certificate to ca.crt, as the CA and Vault issuers do.
	// +optional
	Issuer *IssuerReference `json:"issuer,omitempty"`

	// AdditionalNames are DNS names the server certificate is valid for
	// besides those of the instance's Services, such as an external name
	// clients connect to. Used with issuer or the certificate CloudNativePG
	// issues; cannot be combined with certificateSecretName, whose
	// certificate carries its own names. Changing them reissues the
	// certificate without a restart.
	// +optional
	AdditionalNames []string `json:"additionalNames,omitempty"`
}

// IssuerKind is the kind of a cert-manager issuer.
// +enum
type IssuerKind string

const (
	// IssuerKindIssuer is an Issuer in the instance namespace.
	IssuerKindIssuer IssuerKind = "Issuer"
	// IssuerKindClusterIssuer is a cluster-wide ClusterIssuer.
	IssuerKindClusterIssuer IssuerKind = "ClusterIssuer"
)

// IssuerReference refers to a cert-manager issuer.
type IssuerReference struct {
	// Name of the issuer.
	Name string `json:"name"`

	// Kind of the issuer: Issuer or ClusterIssuer, or the kind of an
	// external issuer of group.
	// +default="Issuer"
	// +optional
	Kind IssuerKind `json:"kind,omitempty"`

	// Group is the API group of an external issuer, such as
	// awspca.cert-manager.io.
	// +default="cert-manager.io"
	// +optional
	Group string `json:"group,omitempty"`
}

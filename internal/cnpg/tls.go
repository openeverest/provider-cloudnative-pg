package cnpg

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

// RejectNonTLSRule refuses every TCP connection that is not encrypted. CNPG
// evaluates user rules after its fixed rules (local peer access and
// certificate-authenticated replication, both unaffected) and before its
// default "host all all all" rule, which this rule therefore shadows.
const RejectNonTLSRule = "hostnossl all all all reject"

// ApplyRequireTLS places RejectNonTLSRule ahead of the user's pg_hba rules
// when TLS is required, so no user rule can admit an unencrypted client.
func ApplyRequireTLS(pg *cnpgv1.Cluster, custom *components.CNPGCustomSpec) {
	if custom.RequireTLS == nil || !*custom.RequireTLS {
		return
	}
	pg.Spec.PostgresConfiguration.PgHBA = append([]string{RejectNonTLSRule}, pg.Spec.PostgresConfiguration.PgHBA...)
}

// certManagerGroup is the API group of cert-manager's own issuers.
const certManagerGroup = "cert-manager.io"

// ServerCertificateName names the cert-manager Certificate the provider
// requests for tls.issuer, and the Secret cert-manager writes it to. CNPG's
// own Secrets end in -ca, -server, -replication, -app and -superuser.
func ServerCertificateName(instance string) string {
	return instance + "-tls"
}

// ServerCertificateSecret is the Secret holding the server certificate tls
// sets, or "" when CNPG issues it.
func ServerCertificateSecret(tls *components.TLSSpec, instance string) string {
	switch {
	case tls == nil:
		return ""
	case tls.Issuer != nil:
		return ServerCertificateName(instance)
	default:
		return tls.CertificateSecretName
	}
}

// ApplyTLS points CNPG at the server certificate tls sets. The Secret is both
// the server certificate and the CA CNPG checks it against, since it carries
// ca.crt. Without a Secret, extraDNSNames go to the certificate CNPG issues.
func ApplyTLS(pg *cnpgv1.Cluster, tls *components.TLSSpec, instance string) {
	if tls == nil {
		return
	}
	certificates := &cnpgv1.CertificatesConfiguration{}
	if pg.Spec.Certificates != nil {
		certificates = pg.Spec.Certificates.DeepCopy()
	}
	if secret := ServerCertificateSecret(tls, instance); secret != "" {
		certificates.ServerTLSSecret = secret
		certificates.ServerCASecret = secret
	} else if len(tls.ExtraDNSNames) > 0 {
		certificates.ServerAltDNSNames = slices.Clone(tls.ExtraDNSNames)
	} else {
		return
	}
	pg.Spec.Certificates = certificates
}

// IssuerKind is the kind of the issuer, Issuer when unset.
func IssuerKind(issuer *components.IssuerReference) string {
	return cmp.Or(issuer.Kind, "Issuer")
}

// IssuerGroup is the API group of the issuer, cert-manager.io when unset.
func IssuerGroup(issuer *components.IssuerReference) string {
	return cmp.Or(issuer.Group, certManagerGroup)
}

// ValidateTLS checks tls alone and against the server fields of certificates,
// which name the same certificate.
func ValidateTLS(tls *components.TLSSpec, certificates *cnpgv1.CertificatesConfiguration) error {
	if tls == nil {
		return nil
	}
	if tls.Issuer != nil && tls.CertificateSecretName != "" {
		return fmt.Errorf("tls: set issuer or certificateSecretName, not both")
	}
	if issuer := tls.Issuer; issuer != nil {
		if issuer.Name == "" {
			return fmt.Errorf("tls.issuer.name is required")
		}
		if errs := validation.IsDNS1123Subdomain(issuer.Name); len(errs) > 0 {
			return fmt.Errorf("tls.issuer.name %q is invalid: %s", issuer.Name, strings.Join(errs, "; "))
		}
		if kind := IssuerKind(issuer); IssuerGroup(issuer) == certManagerGroup && kind != "Issuer" && kind != "ClusterIssuer" {
			return fmt.Errorf("tls.issuer.kind %q is not a cert-manager issuer: use Issuer or ClusterIssuer, or set group for an external issuer", kind)
		}
	}
	if name := tls.CertificateSecretName; name != "" {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return fmt.Errorf("tls.certificateSecretName %q is invalid: %s", name, strings.Join(errs, "; "))
		}
		if len(tls.ExtraDNSNames) > 0 {
			return fmt.Errorf("tls.extraDNSNames cannot be combined with tls.certificateSecretName: the certificate in the Secret carries its own names")
		}
	}
	for _, name := range tls.ExtraDNSNames {
		if errs := validation.IsDNS1123Subdomain(strings.TrimPrefix(name, "*.")); len(errs) > 0 {
			return fmt.Errorf("tls.extraDNSNames: %q is not a DNS name: %s", name, strings.Join(errs, "; "))
		}
	}
	if certificates == nil {
		return nil
	}
	if (tls.Issuer != nil || tls.CertificateSecretName != "") &&
		(certificates.ServerTLSSecret != "" || certificates.ServerCASecret != "" || len(certificates.ServerAltDNSNames) > 0) {
		return fmt.Errorf("tls.issuer and tls.certificateSecretName cannot be combined with certificates.serverTLSSecret, serverCASecret or serverAltDNSNames: they set the same certificate")
	}
	if len(tls.ExtraDNSNames) > 0 && (certificates.ServerTLSSecret != "" || len(certificates.ServerAltDNSNames) > 0) {
		return fmt.Errorf("tls.extraDNSNames cannot be combined with certificates.serverTLSSecret or serverAltDNSNames; use one of them")
	}
	return nil
}

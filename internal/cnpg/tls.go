package cnpg

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-cloudnative-pg/definition"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// RejectNonTLSRule refuses every TCP connection that is not encrypted. CNPG
// evaluates user rules after its fixed rules (local peer access and
// certificate-authenticated replication, both unaffected) and before its
// default "host all all all" rule, which this rule therefore shadows.
const RejectNonTLSRule = "hostnossl all all all reject"

// CertManagerGroup is the API group of cert-manager's own issuers.
const CertManagerGroup = "cert-manager.io"

// TLSPath is the field path of the TLS parameters.
var TLSPath = field.NewPath("spec", "parameters", "tls")

// TLS decodes the TLS parameters of in, with the defaults applied: preferTLS,
// and an Issuer of cert-manager.io.
func TLS(in *corev1alpha1.Instance) (definition.TLSParameters, error) {
	var params definition.Parameters
	if raw := in.Spec.Parameters; raw != nil && len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &params); err != nil {
			return definition.TLSParameters{}, fmt.Errorf("decode spec.parameters: %w", err)
		}
	}
	var tls definition.TLSParameters
	if params.TLS != nil {
		tls = *params.TLS
	}
	if tls.Mode == "" {
		tls.Mode = definition.TLSModePrefer
	}
	if tls.Issuer != nil {
		issuer := *tls.Issuer
		if issuer.Kind == "" {
			issuer.Kind = definition.IssuerKindIssuer
		}
		if issuer.Group == "" {
			issuer.Group = CertManagerGroup
		}
		tls.Issuer = &issuer
	}
	return tls, nil
}

// RequireTLS says whether clients must use TLS.
func RequireTLS(tls definition.TLSParameters) bool {
	return tls.Mode == definition.TLSModeRequire
}

// ServerCertificateName names the cert-manager Certificate the provider
// requests for tls.issuer, and the Secret cert-manager writes it to. CNPG's
// own Secrets end in -ca, -server, -replication, -app and -superuser.
func ServerCertificateName(instance string) string {
	return instance + "-tls"
}

// ServerCertificateSecret is the Secret holding the server certificate tls
// sets, or "" when CNPG issues it.
func ServerCertificateSecret(tls definition.TLSParameters, instance string) string {
	switch {
	case tls.CertificateSecretName != "":
		return tls.CertificateSecretName
	case tls.Issuer != nil:
		return ServerCertificateName(instance)
	}
	return ""
}

// ApplyTLS configures the Cluster for tls. requireTLS places RejectNonTLSRule
// ahead of the user's pg_hba rules, so no user rule can admit an unencrypted
// client. A server certificate Secret is both the certificate and the CA CNPG
// checks it against, since it carries ca.crt. The client CA is
// clientCASecretName when set, otherwise the replication Secret's ca.crt when
// that is brought; without either CNPG issues both. Without a server Secret,
// additionalNames go to the certificate CNPG issues.
func ApplyTLS(pg *cnpgv1.Cluster, tls definition.TLSParameters, instance string) {
	if RequireTLS(tls) {
		pg.Spec.PostgresConfiguration.PgHBA = append([]string{RejectNonTLSRule}, pg.Spec.PostgresConfiguration.PgHBA...)
	}

	certificates := cnpgv1.CertificatesConfiguration{}
	if secret := ServerCertificateSecret(tls, instance); secret != "" {
		certificates.ServerTLSSecret = secret
		certificates.ServerCASecret = secret
	} else if len(tls.AdditionalNames) > 0 {
		certificates.ServerAltDNSNames = slices.Clone(tls.AdditionalNames)
	}
	switch {
	case tls.ClientCASecretName != "" && tls.InternalCertificateSecretName != "":
		certificates.ClientCASecret = tls.ClientCASecretName
		certificates.ReplicationTLSSecret = tls.InternalCertificateSecretName
	case tls.ClientCASecretName != "":
		// CNPG signs streaming_replica with ca.key of this Secret.
		certificates.ClientCASecret = tls.ClientCASecretName
	case tls.InternalCertificateSecretName != "":
		// One Secret holds the replication certificate and the CA it chains to.
		certificates.ReplicationTLSSecret = tls.InternalCertificateSecretName
		certificates.ClientCASecret = tls.InternalCertificateSecretName
	}
	if !equality.Semantic.DeepEqual(certificates, cnpgv1.CertificatesConfiguration{}) {
		pg.Spec.Certificates = &certificates
	}
}

var (
	tlsModes    = []definition.TLSMode{definition.TLSModePrefer, definition.TLSModeRequire}
	issuerKinds = []definition.IssuerKind{definition.IssuerKindIssuer, definition.IssuerKindClusterIssuer}
)

// ValidateTLS checks the TLS parameters of in on their own. proxy says
// whether the Instance has the proxy component.
func ValidateTLS(in *corev1alpha1.Instance, proxy bool) field.ErrorList {
	tls, err := TLS(in)
	if err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "parameters"), field.OmitValueType{}, err.Error())}
	}

	var errs field.ErrorList
	switch {
	case tls.Mode == "disabled":
		errs = append(errs, field.Invalid(TLSPath.Child("mode"), tls.Mode,
			"CloudNativePG always serves TLS and replicates over it, so TLS cannot be turned off; use preferTLS or requireTLS"))
	case !slices.Contains(tlsModes, tls.Mode):
		errs = append(errs, field.NotSupported(TLSPath.Child("mode"), tls.Mode, tlsModes))
	}

	for _, ref := range []struct{ name, value string }{
		{"certificateSecretName", tls.CertificateSecretName},
		{"internalCertificateSecretName", tls.InternalCertificateSecretName},
		{"clientCASecretName", tls.ClientCASecretName},
	} {
		if ref.value == "" {
			continue
		}
		if msgs := validation.IsDNS1123Subdomain(ref.value); len(msgs) > 0 {
			errs = append(errs, field.Invalid(TLSPath.Child(ref.name), ref.value, strings.Join(msgs, "; ")))
		}
		for _, suffix := range cnpgCertificateSecretSuffixes {
			if ref.value == in.Name+suffix {
				errs = append(errs, field.Invalid(TLSPath.Child(ref.name), ref.value,
					"the name of a Secret CloudNativePG writes its own certificate to, and it would overwrite yours; name the Secret differently"))
			}
		}
	}

	if issuer := tls.Issuer; issuer != nil {
		path := TLSPath.Child("issuer")
		if tls.CertificateSecretName != "" {
			errs = append(errs, field.Forbidden(path, "use either certificateSecretName or an issuer, not both"))
		}
		if issuer.Name == "" {
			errs = append(errs, field.Required(path.Child("name"), ""))
		} else if msgs := validation.IsDNS1123Subdomain(issuer.Name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(path.Child("name"), issuer.Name, strings.Join(msgs, "; ")))
		}
		if issuer.Group == CertManagerGroup && !slices.Contains(issuerKinds, issuer.Kind) {
			errs = append(errs, field.NotSupported(path.Child("kind"), issuer.Kind, issuerKinds))
		}
	}

	// Without a client CA Secret that holds ca.key, CNPG cannot issue
	// PgBouncer's client certificate when the replication certificate is
	// user-provided (no CA private key in that Secret).
	if tls.InternalCertificateSecretName != "" && tls.ClientCASecretName == "" && proxy {
		errs = append(errs, field.Forbidden(TLSPath.Child("internalCertificateSecretName"),
			"cannot be combined with the proxy component: CloudNativePG issues the client certificate PgBouncer authenticates to PostgreSQL with "+
				"from the private key of the CA that signs the replication certificate; set clientCASecretName to a Secret that holds ca.key, "+
				"or omit the replication certificate Secret so CloudNativePG issues it"))
	}

	if len(tls.AdditionalNames) > 0 && tls.CertificateSecretName != "" {
		errs = append(errs, field.Forbidden(TLSPath.Child("additionalNames"),
			"cannot be combined with certificateSecretName: the certificate in the Secret carries its own names"))
	}
	for i, name := range tls.AdditionalNames {
		if net.ParseIP(name) != nil {
			errs = append(errs, field.Invalid(TLSPath.Child("additionalNames").Index(i), name, "must be a DNS name, not an IP address"))
			continue
		}
		if msgs := validation.IsDNS1123Subdomain(strings.TrimPrefix(name, "*.")); len(msgs) > 0 {
			errs = append(errs, field.Invalid(TLSPath.Child("additionalNames").Index(i), name, "must be a DNS name: "+strings.Join(msgs, "; ")))
		}
	}
	return errs
}

// ValidateTLSTransition rejects, on an update of an Instance the provider has
// reconciled before, a change of where its certificates come from: they can
// only be set at creation. The certificate fields an Instance of an earlier
// release set in components.engine.parameters.certificates count as the
// same source, so moving them to spec.parameters.tls is not a change.
func ValidateTLSTransition(in, old *corev1alpha1.Instance) field.ErrorList {
	if old == nil || old.Status.Version == "" {
		return nil
	}
	before, err := TLS(old)
	if err != nil {
		return nil
	}
	after, err := TLS(in)
	if err != nil {
		return nil
	}
	if legacy := legacyCertificates(old); legacy != nil && before.CertificateSecretName == "" && before.InternalCertificateSecretName == "" && before.ClientCASecretName == "" && before.Issuer == nil {
		before.CertificateSecretName = legacy.ServerTLSSecret
		before.InternalCertificateSecretName = legacy.ReplicationTLSSecret
		// Same Secret for both maps to internalCertificateSecretName alone.
		if legacy.ClientCASecret != "" && legacy.ClientCASecret != legacy.ReplicationTLSSecret {
			before.ClientCASecretName = legacy.ClientCASecret
		}
	}
	if CertificateSource(before) != CertificateSource(after) {
		return field.ErrorList{field.Forbidden(TLSPath,
			"certificateSecretName, internalCertificateSecretName, clientCASecretName and issuer can only be set when the instance is created")}
	}
	return nil
}

// CertificateSource identifies where an instance's certificates come from.
func CertificateSource(tls definition.TLSParameters) string {
	issuer := "/"
	if tls.Issuer != nil {
		issuer = tls.Issuer.Group + "/" + string(tls.Issuer.Kind) + "/" + tls.Issuer.Name
	}
	return issuer + " " + tls.CertificateSecretName + " " + tls.InternalCertificateSecretName + " " + tls.ClientCASecretName
}

// legacyCertificates are the CNPG certificates an Instance of an earlier
// release set in its engine parameters, or nil.
func legacyCertificates(in *corev1alpha1.Instance) *cnpgv1.CertificatesConfiguration {
	engine, ok := in.Spec.Components[common.ComponentEngine]
	if !ok || engine.Parameters == nil {
		return nil
	}
	var legacy struct {
		Certificates *cnpgv1.CertificatesConfiguration `json:"certificates"`
	}
	if json.Unmarshal(engine.Parameters.Raw, &legacy) != nil {
		return nil
	}
	return legacy.Certificates
}

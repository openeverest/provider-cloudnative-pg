package cnpg

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// Keys of the certificate Secrets CloudNativePG reads.
const (
	tlsCertificateKey = "tls.crt"
	tlsPrivateKeyKey  = "tls.key"
	caCertificateKey  = "ca.crt"
	caPrivateKeyKey   = "ca.key"
)

// cnpgCertificateSecretSuffixes end the names of the Secrets CloudNativePG
// writes its own certificates to. A Secret of the user's with such a name
// would be overwritten by CloudNativePG.
var cnpgCertificateSecretSuffixes = []string{"-ca", "-server", "-replication"}

// ValidateServerCertificate checks the server certificate in server (tls.crt
// and tls.key) against the CA certificates in ca (ca.crt) the way
// CloudNativePG and libpq use them: the key matches, the certificate chains to
// ca.crt for server authentication and is valid now, ca.crt holds the root of
// that chain, and the certificate is valid for one of hosts.
func ValidateServerCertificate(server, ca *corev1.Secret, hosts []string) error {
	leaf, err := verifyCertificate(server, ca, x509.ExtKeyUsageServerAuth, "server")
	if err != nil {
		return err
	}
	if len(hosts) > 0 && !slices.ContainsFunc(hosts, func(host string) bool { return leaf.VerifyHostname(host) == nil }) {
		return fmt.Errorf("the certificate in Secret %s is valid for none of the names of the read-write Service clients connect to: %s",
			server.Name, strings.Join(hosts, ", "))
	}
	return nil
}

// ValidateReplicationCertificate checks the replication certificate in
// secret the way CloudNativePG and PostgreSQL use it when the Secret is also
// the client CA: replicas present tls.crt and tls.key as the streaming_replica
// user, and the primary verifies the certificate against ca.crt of the same
// Secret.
func ValidateReplicationCertificate(secret *corev1.Secret) error {
	return ValidateReplicationCertificateAgainst(secret, secret)
}

// ValidateReplicationCertificateAgainst checks the replication certificate in
// secret against the CA certificates in ca (ca.crt), the way CloudNativePG
// does when clientCASecret and replicationTLSSecret are separate Secrets.
func ValidateReplicationCertificateAgainst(secret, ca *corev1.Secret) error {
	leaf, err := verifyCertificate(secret, ca, x509.ExtKeyUsageClientAuth, "replication")
	if err != nil {
		return err
	}
	if leaf.Subject.CommonName != cnpgv1.StreamingReplicationUser {
		return fmt.Errorf("the certificate in Secret %s is issued to %q, and replicas authenticate with it as the %s user: "+
			"issue it with the common name %s", secret.Name, leaf.Subject.CommonName, cnpgv1.StreamingReplicationUser, cnpgv1.StreamingReplicationUser)
	}
	return nil
}

// ValidateClientCASecret checks that secret holds a CA certificate in ca.crt
// (PostgreSQL's ssl_ca_file). When requireKey is set, ca.key must also be
// present and match ca.crt: CNPG needs it to sign the streaming_replica
// certificate (and PgBouncer's when the proxy component is used).
func ValidateClientCASecret(secret *corev1.Secret, requireKey bool) error {
	if _, err := parseCertificates(secret.Data[caCertificateKey]); err != nil {
		return fmt.Errorf("%s of the Secret %s: %w", caCertificateKey, secret.Name, err)
	}
	if !requireKey {
		return nil
	}
	key := secret.Data[caPrivateKeyKey]
	if len(key) == 0 {
		return fmt.Errorf("the Secret %s needs the CA private key in %s so CloudNativePG can sign the streaming_replica certificate "+
			"(or set internalCertificateSecretName to a certificate signed by this CA)", secret.Name, caPrivateKeyKey)
	}
	if _, err := tls.X509KeyPair(secret.Data[caCertificateKey], key); err != nil {
		return fmt.Errorf("%s and %s of the Secret %s are not a CA certificate and its private key: %w",
			caCertificateKey, caPrivateKeyKey, secret.Name, err)
	}
	return nil
}

// verifyCertificate checks the certificate in tls.crt and tls.key of leaf
// against the CA certificates in ca.crt of ca: the key matches, the
// certificate chains to ca.crt for usage and is valid now, and ca.crt holds
// the root of that chain. kind names the certificate in errors.
func verifyCertificate(leafSecret, ca *corev1.Secret, usage x509.ExtKeyUsage, kind string) (*x509.Certificate, error) {
	certificate, key := leafSecret.Data[tlsCertificateKey], leafSecret.Data[tlsPrivateKeyKey]
	if len(certificate) == 0 || len(key) == 0 {
		return nil, fmt.Errorf("the Secret %s needs the %s certificate in %s and its private key in %s", leafSecret.Name, kind, tlsCertificateKey, tlsPrivateKeyKey)
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return nil, fmt.Errorf("%s and %s of the Secret %s are not a certificate and its private key: %w", tlsCertificateKey, tlsPrivateKeyKey, leafSecret.Name, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse %s of the Secret %s: %w", tlsCertificateKey, leafSecret.Name, err)
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		if cert, err := x509.ParseCertificate(der); err == nil {
			intermediates.AddCert(cert)
		}
	}
	cas, err := parseCertificates(ca.Data[caCertificateKey])
	if err != nil {
		return nil, fmt.Errorf("%s of the Secret %s: %w", caCertificateKey, ca.Name, err)
	}
	roots := x509.NewCertPool()
	for _, cert := range cas {
		roots.AddCert(cert)
	}

	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{usage},
	})
	if err != nil {
		return nil, explainVerifyError(err, leaf, leafSecret.Name, ca.Name, usage)
	}
	// Go ends a chain at any certificate of the pool; libpq goes on to a
	// self-signed root, which must then be in ca.crt as well.
	if !slices.ContainsFunc(chains, func(chain []*x509.Certificate) bool { return chainsToRootIn(chain[len(chain)-1], cas) }) {
		return nil, fmt.Errorf("%s of Secret %s holds no root CA certificate of the chain of the %s certificate, only %q: "+
			"libpq, which replicas and most clients use, trusts only a chain that ends at a self-signed root; add the root CA certificate to %s",
			caCertificateKey, ca.Name, kind, chains[0][len(chains[0])-1].Subject.String(), caCertificateKey)
	}
	return leaf, nil
}

// explainVerifyError says why the server certificate does not verify, in the
// terms of the Secrets it comes from.
func explainVerifyError(err error, leaf *x509.Certificate, serverSecret, caSecret string, usage x509.ExtKeyUsage) error {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknown):
		return fmt.Errorf("the certificate in Secret %s (%s), issued by %q, is not signed by a CA certificate in %s of Secret %s, "+
			"directly or through intermediate CA certificates following it in %s",
			serverSecret, tlsCertificateKey, leaf.Issuer.String(), caCertificateKey, caSecret, tlsCertificateKey)
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return fmt.Errorf("a certificate of Secret %s is not valid at this time: %s, valid from %s until %s",
			serverSecret, invalid.Cert.Subject.String(), invalid.Cert.NotBefore.Format(time.RFC3339), invalid.Cert.NotAfter.Format(time.RFC3339))
	case errors.As(err, &invalid) && invalid.Reason == x509.IncompatibleUsage:
		if usage == x509.ExtKeyUsageClientAuth {
			return fmt.Errorf("the certificate in Secret %s is not for client authentication: add clientAuth to its extended key usage", serverSecret)
		}
		return fmt.Errorf("the certificate in Secret %s is not for server authentication: add serverAuth to its extended key usage", serverSecret)
	default:
		return fmt.Errorf("the certificate in Secret %s does not verify against %s of Secret %s: %w", serverSecret, caCertificateKey, caSecret, err)
	}
}

func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	for rest := data; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, cert)
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return certificates, nil
}

func isSelfSigned(cert *x509.Certificate) bool {
	return bytes.Equal(cert.RawSubject, cert.RawIssuer) && cert.CheckSignatureFrom(cert) == nil
}

// chainsToRootIn says whether cert chains to a self-signed certificate of
// bundle, through the other certificates of bundle.
func chainsToRootIn(cert *x509.Certificate, bundle []*x509.Certificate) bool {
	if isSelfSigned(cert) {
		return true
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	for _, c := range bundle {
		if isSelfSigned(c) {
			roots.AddCert(c)
		} else {
			intermediates.AddCert(c)
		}
	}
	_, err := cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

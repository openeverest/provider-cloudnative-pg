package cnpg

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

// Keys of the certificate Secrets CloudNativePG reads.
const (
	tlsCertificateKey = "tls.crt"
	tlsPrivateKeyKey  = "tls.key"
	caCertificateKey  = "ca.crt"
	caPrivateKeyKey   = "ca.key"
)

// cnpgCertificateSecretSuffixes end the names of the Secrets CloudNativePG
// writes its own certificates to. A Secret of the user's with such a name is
// taken over by CloudNativePG once the Instance stops naming it.
var cnpgCertificateSecretSuffixes = []string{"-ca", "-server", "-replication"}

// ValidateCertificateSecretNames refuses certificate Secrets named like the
// ones CloudNativePG writes for the instance.
func ValidateCertificateSecretNames(custom *components.CNPGCustomSpec, instance string) error {
	named := map[string]string{}
	if custom.TLS != nil {
		named["tls.certificateSecretName"] = custom.TLS.CertificateSecretName
	}
	if certs := custom.Certificates; certs != nil {
		named["certificates.serverTLSSecret"] = certs.ServerTLSSecret
		named["certificates.serverCASecret"] = certs.ServerCASecret
		named["certificates.clientCASecret"] = certs.ClientCASecret
		named["certificates.replicationTLSSecret"] = certs.ReplicationTLSSecret
	}
	for _, field := range slices.Sorted(maps.Keys(named)) {
		name := named[field]
		for _, suffix := range cnpgCertificateSecretSuffixes {
			if name == instance+suffix {
				return fmt.Errorf("%s: %q is the name of a Secret CloudNativePG writes its own certificate to, "+
					"and it would overwrite yours once the instance no longer names it; name the Secret differently",
					field, name)
			}
		}
	}
	return nil
}

// ValidateServerCertificate checks the server certificate in server (tls.crt
// and tls.key) against the CA certificates in ca (ca.crt) the way
// CloudNativePG and libpq use them: the key matches, the certificate chains to
// ca.crt for server authentication and is valid now, ca.crt holds the root of
// that chain, and the certificate is valid for one of hosts.
func ValidateServerCertificate(server, ca *corev1.Secret, hosts []string) error {
	certificate, key := server.Data[tlsCertificateKey], server.Data[tlsPrivateKeyKey]
	if len(certificate) == 0 || len(key) == 0 {
		return fmt.Errorf("the Secret %s needs the server certificate in %s and its private key in %s", server.Name, tlsCertificateKey, tlsPrivateKeyKey)
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return fmt.Errorf("%s and %s of the Secret %s are not a certificate and its private key: %w", tlsCertificateKey, tlsPrivateKeyKey, server.Name, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse %s of the Secret %s: %w", tlsCertificateKey, server.Name, err)
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		if cert, err := x509.ParseCertificate(der); err == nil {
			intermediates.AddCert(cert)
		}
	}
	cas, err := parseCertificates(ca.Data[caCertificateKey])
	if err != nil {
		return fmt.Errorf("%s of the Secret %s: %w", caCertificateKey, ca.Name, err)
	}
	roots := x509.NewCertPool()
	for _, cert := range cas {
		roots.AddCert(cert)
	}

	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return explainVerifyError(err, leaf, server.Name, ca.Name)
	}
	// Go ends a chain at any certificate of the pool; libpq goes on to a
	// self-signed root, which must then be in ca.crt as well.
	if !slices.ContainsFunc(chains, func(chain []*x509.Certificate) bool { return chainsToRootIn(chain[len(chain)-1], cas) }) {
		return fmt.Errorf("%s of Secret %s holds no root CA certificate of the chain of the server certificate, only %q: "+
			"libpq, which replicas and most clients use, trusts only a chain that ends at a self-signed root; add the root CA certificate to %s",
			caCertificateKey, ca.Name, chains[0][len(chains[0])-1].Subject.String(), caCertificateKey)
	}
	if len(hosts) > 0 && !slices.ContainsFunc(hosts, func(host string) bool { return leaf.VerifyHostname(host) == nil }) {
		return fmt.Errorf("the certificate in Secret %s is valid for none of the names of the read-write Service clients connect to: %s",
			server.Name, strings.Join(hosts, ", "))
	}
	return nil
}

// explainVerifyError says why the server certificate does not verify, in the
// terms of the Secrets it comes from.
func explainVerifyError(err error, leaf *x509.Certificate, serverSecret, caSecret string) error {
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
		return fmt.Errorf("the certificate in Secret %s is not for server authentication: add serverAuth to its extended key usage", serverSecret)
	default:
		return fmt.Errorf("the certificate in Secret %s does not verify against %s of Secret %s: %w", serverSecret, caCertificateKey, caSecret, err)
	}
}

// ValidateSigningCA checks a CA Secret CloudNativePG issues certificates with:
// ca.key is the private key of the first certificate of ca.crt in the one
// form CloudNativePG reads, and ca.crt holds the root CA of that certificate.
func ValidateSigningCA(secret *corev1.Secret, field string) error {
	cas, err := parseCertificates(secret.Data[caCertificateKey])
	if err != nil {
		return fmt.Errorf("%s: Secret %s: %s: %w", field, secret.Name, caCertificateKey, err)
	}
	key := secret.Data[caPrivateKeyKey]
	if len(key) == 0 {
		return fmt.Errorf("%s: Secret %s has no %s, the private key CloudNativePG issues certificates with", field, secret.Name, caPrivateKeyKey)
	}
	block, _ := pem.Decode(key)
	switch {
	case block == nil:
		return fmt.Errorf("%s: %s of Secret %s is not a PEM private key", field, caPrivateKeyKey, secret.Name)
	case block.Type == "RSA PRIVATE KEY" || isPKCS8Of[*rsa.PrivateKey](block):
		return fmt.Errorf("%s: %s of Secret %s is an RSA key, and CloudNativePG issues certificates only with an ECDSA CA: "+
			"have a certificate issued by your CA with tls.issuer or tls.certificateSecretName instead", field, caPrivateKeyKey, secret.Name)
	case isPKCS8Of[*ecdsa.PrivateKey](block):
		return fmt.Errorf("%s: %s of Secret %s is a PKCS #8 key (BEGIN PRIVATE KEY), and CloudNativePG reads only the SEC 1 form (BEGIN EC PRIVATE KEY): "+
			"convert it with openssl ec -in ca.key -out ca-ec.key", field, caPrivateKeyKey, secret.Name)
	case block.Type != "EC PRIVATE KEY":
		return fmt.Errorf("%s: %s of Secret %s holds a %q PEM block, not an ECDSA key in the SEC 1 form CloudNativePG reads (BEGIN EC PRIVATE KEY)",
			field, caPrivateKeyKey, secret.Name, block.Type)
	}
	if _, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cas[0].Raw}), key); err != nil {
		return fmt.Errorf("%s: %s of Secret %s is not the private key of the first certificate of %s, %q: %w",
			field, caPrivateKeyKey, secret.Name, caCertificateKey, cas[0].Subject.String(), err)
	}
	if !chainsToRootIn(cas[0], cas) {
		return fmt.Errorf("%s: %s of Secret %s holds %q but not its root CA certificate: "+
			"libpq, which replicas and most clients use, trusts only a chain that ends at a self-signed root; add the intermediate and root CA certificates after it",
			field, caCertificateKey, secret.Name, cas[0].Subject.String())
	}
	return nil
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

func isPKCS8Of[K any](block *pem.Block) bool {
	if block.Type != "PRIVATE KEY" {
		return false
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	_, ok := key.(K)
	return err == nil && ok
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

// SignedBy says whether the certificate in tls.crt of leaf is signed by a
// certificate in ca.crt of ca. It is true when either cannot be read, so the
// caller leaves a certificate it cannot judge alone.
func SignedBy(leaf, ca *corev1.Secret) bool {
	block, _ := pem.Decode(leaf.Data[tlsCertificateKey])
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	cas, err := parseCertificates(ca.Data[caCertificateKey])
	if err != nil {
		return true
	}
	return slices.ContainsFunc(cas, func(ca *x509.Certificate) bool { return cert.CheckSignatureFrom(ca) == nil })
}

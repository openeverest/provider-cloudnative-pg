package cnpg

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

type testCert struct {
	cert *x509.Certificate
	key  any
}

func (tc testCert) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tc.cert.Raw})
}

func (tc testCert) secKey(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(tc.key.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func (tc testCert) pkcs8Key(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(tc.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

var serial int64

// issue makes a certificate signed by parent, or self-signed without one.
func issue(t *testing.T, template *x509.Certificate, parent *testCert, rsaKey bool) testCert {
	t.Helper()
	var key any
	var public any
	if rsaKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		key, public = k, &k.PublicKey
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, public = k, &k.PublicKey
	}
	serial++
	template.SerialNumber = big.NewInt(serial)
	if template.NotBefore.IsZero() {
		template.NotBefore = time.Now().Add(-time.Hour)
		template.NotAfter = time.Now().Add(24 * time.Hour)
	}
	signer, signerKey := template, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, public, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCert{cert: cert, key: key}
}

func caTemplate(name string) *x509.Certificate {
	return &x509.Certificate{
		Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
}

func serverTemplate(usage x509.ExtKeyUsage, names ...string) *x509.Certificate {
	return &x509.Certificate{
		Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
}

func secret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: data}
}

func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestValidateServerCertificate(t *testing.T) {
	hosts := []string{"pg-rw", "pg-rw.ns", "pg-rw.ns.svc", "pg-rw.ns.svc.cluster.local"}
	root := issue(t, caTemplate("Root CA"), nil, false)
	intermediate := issue(t, caTemplate("Intermediate CA"), &root, false)
	leaf := issue(t, serverTemplate(x509.ExtKeyUsageServerAuth, "pg-rw.ns.svc"), &intermediate, true)
	direct := issue(t, serverTemplate(x509.ExtKeyUsageServerAuth, "*.ns.svc"), &root, false)
	clientOnly := issue(t, serverTemplate(x509.ExtKeyUsageClientAuth, "pg-rw.ns.svc"), &root, false)
	otherNames := issue(t, serverTemplate(x509.ExtKeyUsageServerAuth, "db.example.com"), &root, false)
	expiredTemplate := serverTemplate(x509.ExtKeyUsageServerAuth, "pg-rw.ns.svc")
	expiredTemplate.NotBefore, expiredTemplate.NotAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)
	expired := issue(t, expiredTemplate, &root, false)

	server := func(c testCert, chain ...[]byte) *corev1.Secret {
		return secret("server", map[string][]byte{"tls.crt": join(append([][]byte{c.pem()}, chain...)...), "tls.key": c.pkcs8Key(t)})
	}
	ca := func(certs ...[]byte) *corev1.Secret { return secret("ca", map[string][]byte{"ca.crt": join(certs...)}) }

	tests := []struct {
		name    string
		server  *corev1.Secret
		ca      *corev1.Secret
		wantErr string
	}{
		{name: "chain to the root", server: server(leaf, intermediate.pem()), ca: ca(root.pem())},
		{name: "signed by the root, wildcard name", server: server(direct), ca: ca(root.pem())},
		{name: "intermediate in ca.crt with its root", server: server(leaf), ca: ca(intermediate.pem(), root.pem())},
		{
			name: "key of another certificate", ca: ca(root.pem()),
			server:  secret("server", map[string][]byte{"tls.crt": leaf.pem(), "tls.key": direct.pkcs8Key(t)}),
			wantErr: "not a certificate and its private key",
		},
		{name: "intermediate missing from tls.crt", server: server(leaf), ca: ca(root.pem()), wantErr: `issued by "CN=Intermediate CA", is not signed by a CA certificate in ca.crt`},
		{name: "only the intermediate in ca.crt", server: server(leaf, intermediate.pem(), root.pem()), ca: ca(intermediate.pem()), wantErr: "holds no root CA certificate"},
		{name: "client certificate", server: server(clientOnly), ca: ca(root.pem()), wantErr: "not for server authentication"},
		{name: "expired", server: server(expired), ca: ca(root.pem()), wantErr: "not valid at this time"},
		{name: "other names", server: server(otherNames), ca: ca(root.pem()), wantErr: "valid for none of the names of the read-write Service"},
		{name: "no ca.crt", server: server(direct), ca: secret("ca", nil), wantErr: "ca.crt of the Secret ca: no PEM certificate"},
		{name: "no key", server: secret("server", map[string][]byte{"tls.crt": direct.pem()}), ca: ca(root.pem()), wantErr: "needs the server certificate in tls.crt and its private key in tls.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, ValidateServerCertificate(tt.server, tt.ca, hosts), tt.wantErr)
		})
	}
}

func TestValidateSigningCA(t *testing.T) {
	root := issue(t, caTemplate("Root CA"), nil, false)
	intermediate := issue(t, caTemplate("Intermediate CA"), &root, false)
	rsaRoot := issue(t, caTemplate("RSA Root CA"), nil, true)

	tests := []struct {
		name    string
		data    map[string][]byte
		wantErr string
	}{
		{name: "root", data: map[string][]byte{"ca.crt": root.pem(), "ca.key": root.secKey(t)}},
		{name: "intermediate with its root", data: map[string][]byte{"ca.crt": join(intermediate.pem(), root.pem()), "ca.key": intermediate.secKey(t)}},
		{name: "no key", data: map[string][]byte{"ca.crt": root.pem()}, wantErr: "has no ca.key"},
		{name: "PKCS #8 key", data: map[string][]byte{"ca.crt": root.pem(), "ca.key": root.pkcs8Key(t)}, wantErr: "openssl ec -in ca.key -out ca-ec.key"},
		{name: "RSA key", data: map[string][]byte{"ca.crt": rsaRoot.pem(), "ca.key": rsaRoot.pkcs8Key(t)}, wantErr: "is an RSA key"},
		{name: "key of another CA", data: map[string][]byte{"ca.crt": root.pem(), "ca.key": intermediate.secKey(t)}, wantErr: "is not the private key of the first certificate"},
		{name: "intermediate alone", data: map[string][]byte{"ca.crt": intermediate.pem(), "ca.key": intermediate.secKey(t)}, wantErr: "but not its root CA certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, ValidateSigningCA(secret("ca", tt.data), "certificates.serverCASecret"), tt.wantErr)
		})
	}
}

func TestValidateCertificateSecretNames(t *testing.T) {
	tests := []struct {
		name    string
		custom  components.CNPGCustomSpec
		wantErr string
	}{
		{name: "none"},
		{name: "own names", custom: components.CNPGCustomSpec{
			TLS:          &components.TLSSpec{CertificateSecretName: "pg-tls"},
			Certificates: &cnpgv1.CertificatesConfiguration{ClientCASecret: "pg-client-ca", ReplicationTLSSecret: "pg-repl"},
		}},
		{name: "server certificate named like CloudNativePG's", custom: components.CNPGCustomSpec{TLS: &components.TLSSpec{CertificateSecretName: "pg-server"}}, wantErr: `tls.certificateSecretName: "pg-server"`},
		{
			name: "replication certificate named like CloudNativePG's", wantErr: `certificates.replicationTLSSecret: "pg-replication"`,
			custom: components.CNPGCustomSpec{Certificates: &cnpgv1.CertificatesConfiguration{ClientCASecret: "ca", ReplicationTLSSecret: "pg-replication"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, ValidateCertificateSecretNames(&tt.custom, "pg"), tt.wantErr)
		})
	}
}

func assertError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one containing %q", err, want)
	}
}

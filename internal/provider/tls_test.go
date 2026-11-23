package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

func tlsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, cnpgv1.AddToScheme, corev1alpha1.AddToScheme, cmv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func tlsContext(t *testing.T, objects ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace, UID: "instance-uid"}}
	cl := fake.NewClientBuilder().WithScheme(tlsScheme(t)).WithObjects(append([]client.Object{instance}, objects...)...).Build()
	return controller.NewContext(context.Background(), cl, instance, "provider-cloudnative-pg"), cl
}

func testCluster() *cnpgv1.Cluster {
	return &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace}}
}

func TestServerCertificate(t *testing.T) {
	c, _ := tlsContext(t)
	tls := &components.TLSSpec{
		Issuer:        &components.IssuerReference{Name: "acme"},
		ExtraDNSNames: []string{"db.example.com"},
	}
	certificate := serverCertificate(c, testCluster(), tls)

	if certificate.Name != "pg-tls" || certificate.Spec.SecretName != "pg-tls" {
		t.Fatalf("name %q, secret %q, want pg-tls", certificate.Name, certificate.Spec.SecretName)
	}
	if ref := certificate.Spec.IssuerRef; ref.Name != "acme" || ref.Kind != "Issuer" || ref.Group != "cert-manager.io" {
		t.Fatalf("issuerRef = %+v, want the Issuer acme of cert-manager.io", ref)
	}
	if _, ok := certificate.Spec.SecretTemplate.Labels[utils.WatchedLabelName]; !ok {
		t.Fatalf("secret labels = %v, want %s so CloudNativePG reloads renewals", certificate.Spec.SecretTemplate.Labels, utils.WatchedLabelName)
	}
	if !slices.Contains(certificate.Spec.Usages, cmv1.UsageServerAuth) {
		t.Fatalf("usages = %v, want server auth", certificate.Spec.Usages)
	}
	for _, name := range []string{
		"pg-rw", "pg-rw.ns", "pg-rw.ns.svc", "pg-rw.ns.svc.cluster.local",
		"pg-r.ns.svc", "pg-ro.ns.svc",
		"pg-pooler", "pg-pooler.ns.svc", "pg-pooler.ns.svc.cluster.local", "pg-pooler-ro.ns.svc",
		"db.example.com",
	} {
		if !slices.Contains(certificate.Spec.DNSNames, name) {
			t.Errorf("dnsNames lack %q: %v", name, certificate.Spec.DNSNames)
		}
	}
}

func TestSyncIssuedCertificate(t *testing.T) {
	tls := &components.TLSSpec{Issuer: &components.IssuerReference{Name: "acme", Kind: "ClusterIssuer"}}
	issued := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pg-tls", Namespace: testNamespace}, Data: data}
	}

	t.Run("waits for cert-manager to issue it", func(t *testing.T) {
		c, cl := tlsContext(t)
		err := syncServerCertificate(c, testCluster(), tls)
		if !controller.IsWaitError(err) || !strings.Contains(err.Error(), "Certificate pg-tls") {
			t.Fatalf("error = %v, want a wait for the Certificate pg-tls", err)
		}
		certificate := &cmv1.Certificate{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-tls"}, certificate); err != nil {
			t.Fatalf("the Certificate was not created: %v", err)
		}
		if !metav1.IsControlledBy(certificate, c.Instance()) {
			t.Fatalf("owners = %v, want the Instance", certificate.OwnerReferences)
		}
	})

	t.Run("refuses an issuer that writes no CA certificate", func(t *testing.T) {
		c, _ := tlsContext(t, issued(map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k")}))
		err := syncServerCertificate(c, testCluster(), tls)
		if err == nil || controller.IsWaitError(err) || !strings.Contains(err.Error(), "writes no CA certificate") {
			t.Fatalf("error = %v, want one about the missing ca.crt", err)
		}
	})

	t.Run("makes the Certificate own its Secret", func(t *testing.T) {
		c, cl := tlsContext(t, issued(map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k"), "ca.crt": []byte("ca")}))
		if err := syncServerCertificate(c, testCluster(), tls); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-tls"}, secret); err != nil {
			t.Fatal(err)
		}
		if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != cmv1.CertificateKind {
			t.Fatalf("owners = %v, want the Certificate", secret.OwnerReferences)
		}
	})
}

func TestLabelCertificateSecret(t *testing.T) {
	tls := &components.TLSSpec{CertificateSecretName: "mine"}

	t.Run("waits for the Secret", func(t *testing.T) {
		c, _ := tlsContext(t)
		err := syncServerCertificate(c, testCluster(), tls)
		if !controller.IsWaitError(err) || !strings.Contains(err.Error(), "Secret mine") {
			t.Fatalf("error = %v, want a wait for the Secret mine", err)
		}
	})

	t.Run("labels it for reload", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: testNamespace, Labels: map[string]string{"team": "db"}}}
		c, cl := tlsContext(t, secret)
		if err := syncServerCertificate(c, testCluster(), tls); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
			t.Fatal(err)
		}
		if _, ok := secret.Labels[utils.WatchedLabelName]; !ok || secret.Labels["team"] != "db" {
			t.Fatalf("labels = %v, want %s added to the user's", secret.Labels, utils.WatchedLabelName)
		}
	})
}

func TestDeleteUnusedServerCertificate(t *testing.T) {
	certificate := func(owner *corev1alpha1.Instance) *cmv1.Certificate {
		cert := &cmv1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "pg-tls", Namespace: testNamespace}}
		if owner != nil {
			cert.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1alpha1.GroupVersion.WithKind("Instance"))}
		}
		return cert
	}
	usedBy := func(secret string) *cnpgv1.Cluster {
		pg := testCluster()
		pg.Status.Certificates.ServerTLSSecret = secret
		return pg
	}
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace, UID: "instance-uid"}}

	tests := []struct {
		name        string
		tls         *components.TLSSpec
		certificate *cmv1.Certificate
		cluster     *cnpgv1.Cluster
		wantDeleted bool
	}{
		{name: "none requested"},
		{
			name: "still set", tls: &components.TLSSpec{Issuer: &components.IssuerReference{Name: "acme"}},
			certificate: certificate(instance), cluster: usedBy("pg-tls"),
		},
		{name: "still used by the Cluster", certificate: certificate(instance), cluster: usedBy("pg-tls")},
		{name: "not the provider's", certificate: certificate(nil), cluster: usedBy("pg-server")},
		{name: "unused", certificate: certificate(instance), cluster: usedBy("pg-server"), wantDeleted: true},
		{
			name: "replaced by a certificate secret", tls: &components.TLSSpec{CertificateSecretName: "mine"},
			certificate: certificate(instance), cluster: usedBy("mine"), wantDeleted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.certificate != nil {
				objects = append(objects, tt.certificate)
			}
			if tt.cluster != nil {
				objects = append(objects, tt.cluster)
			}
			c, cl := tlsContext(t, objects...)
			if err := deleteUnusedServerCertificate(c, tt.tls); err != nil {
				t.Fatal(err)
			}
			if tt.certificate == nil {
				return
			}
			err := cl.Get(context.Background(), client.ObjectKeyFromObject(tt.certificate), &cmv1.Certificate{})
			if deleted := apierrors.IsNotFound(err); deleted != tt.wantDeleted {
				t.Fatalf("deleted = %t (err %v), want %t", deleted, err, tt.wantDeleted)
			}
		})
	}
}

func TestCertificateWaitNamesTheIssuerProblem(t *testing.T) {
	notReady := &cmv1.Issuer{
		ObjectMeta: metav1.ObjectMeta{Name: "databases-ca", Namespace: testNamespace},
		Status: cmv1.IssuerStatus{Conditions: []cmv1.IssuerCondition{{
			Type: cmv1.IssuerConditionReady, Status: cmmeta.ConditionFalse, Message: "Error getting keypair for CA issuer",
		}}},
	}
	tests := []struct {
		name    string
		issuer  components.IssuerReference
		objects []client.Object
		want    string
	}{
		{name: "missing ClusterIssuer", issuer: components.IssuerReference{Name: "nope", Kind: "ClusterIssuer"}, want: "the ClusterIssuer nope does not exist"},
		{name: "missing Issuer", issuer: components.IssuerReference{Name: "nope"}, want: "the Issuer nope does not exist in namespace ns"},
		{
			name: "issuer not ready", issuer: components.IssuerReference{Name: "databases-ca"}, objects: []client.Object{notReady},
			want: "the Issuer databases-ca is not ready: Error getting keypair for CA issuer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := tlsContext(t, tt.objects...)
			err := syncServerCertificate(c, testCluster(), &components.TLSSpec{Issuer: &tt.issuer})
			if !controller.IsWaitError(err) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want a wait saying %q", err, tt.want)
			}
		})
	}
}

func TestValidateCertificatesSecrets(t *testing.T) {
	withoutKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "client-ca", Namespace: testNamespace},
		Data:       map[string][]byte{"ca.crt": selfSignedCA(t)},
	}
	certificates := &cnpgv1.CertificatesConfiguration{ClientCASecret: "client-ca", ReplicationTLSSecret: "replication"}

	t.Run("client CA without key and no proxy", func(t *testing.T) {
		c, _ := tlsContext(t, withoutKey)
		if err := validateCertificatesSecrets(c, &components.CNPGCustomSpec{Certificates: certificates}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("client CA without key and a proxy", func(t *testing.T) {
		c, _ := tlsContext(t, withoutKey)
		c.Instance().Spec.Components = map[string]corev1alpha1.ComponentSpec{"proxy": {}}
		err := validateCertificatesSecrets(c, &components.CNPGCustomSpec{Certificates: certificates})
		if err == nil || !strings.Contains(err.Error(), "has no ca.key") || !strings.Contains(err.Error(), "client certificate of the proxy component's PgBouncer") {
			t.Fatalf("error = %v, want one about PgBouncer's client certificate", err)
		}
	})

	t.Run("missing Secrets are left to CloudNativePG", func(t *testing.T) {
		c, _ := tlsContext(t)
		c.Instance().Spec.Components = map[string]corev1alpha1.ComponentSpec{"proxy": {}}
		custom := &components.CNPGCustomSpec{Certificates: &cnpgv1.CertificatesConfiguration{
			ServerTLSSecret: "server", ServerCASecret: "server-ca", ClientCASecret: "client-ca",
		}}
		if err := validateCertificatesSecrets(c, custom); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := validateTLSSecret(c, &components.TLSSpec{CertificateSecretName: "mine"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

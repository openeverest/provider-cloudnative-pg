package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition"
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

func issuerTLS(name, kind string) definition.TLSParameters {
	return definition.TLSParameters{Issuer: &definition.IssuerReference{Name: name, Kind: definition.IssuerKind(kind), Group: "cert-manager.io"}}
}

func TestServerCertificate(t *testing.T) {
	c, _ := tlsContext(t)
	tls := issuerTLS("acme", "Issuer")
	tls.AdditionalNames = []string{"db.example.com"}
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
	tls := issuerTLS("acme", "ClusterIssuer")
	issued := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pg-tls", Namespace: testNamespace}, Data: data}
	}

	t.Run("waits for cert-manager to issue it", func(t *testing.T) {
		c, cl := tlsContext(t)
		err := syncCertificates(c, testCluster(), tls)
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
		err := syncCertificates(c, testCluster(), tls)
		if err == nil || controller.IsWaitError(err) || !strings.Contains(err.Error(), "writes no CA certificate") {
			t.Fatalf("error = %v, want one about the missing ca.crt", err)
		}
	})

	t.Run("makes the Certificate own its Secret", func(t *testing.T) {
		c, cl := tlsContext(t, issued(map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k"), "ca.crt": []byte("ca")}))
		if err := syncCertificates(c, testCluster(), tls); err != nil {
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
	tls := definition.TLSParameters{CertificateSecretName: "mine", InternalCertificateSecretName: "repl"}

	t.Run("waits for the Secret", func(t *testing.T) {
		c, _ := tlsContext(t)
		err := syncCertificates(c, testCluster(), tls)
		if !controller.IsWaitError(err) || !strings.Contains(err.Error(), "Secret mine") {
			t.Fatalf("error = %v, want a wait for the Secret mine", err)
		}
	})

	t.Run("labels both for reload", func(t *testing.T) {
		server := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: testNamespace, Labels: map[string]string{"team": "db"}}}
		replication := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "repl", Namespace: testNamespace}}
		c, cl := tlsContext(t, server, replication)
		if err := syncCertificates(c, testCluster(), tls); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []*corev1.Secret{server, replication} {
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
				t.Fatal(err)
			}
			if _, ok := secret.Labels[utils.WatchedLabelName]; !ok {
				t.Fatalf("labels of %s = %v, want %s", secret.Name, secret.Labels, utils.WatchedLabelName)
			}
		}
		if server.Labels["team"] != "db" {
			t.Fatalf("labels = %v, want the user's kept", server.Labels)
		}
	})

	t.Run("waits for the internal Secret", func(t *testing.T) {
		c, _ := tlsContext(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: testNamespace}})
		err := syncCertificates(c, testCluster(), tls)
		if !controller.IsWaitError(err) || !strings.Contains(err.Error(), "Secret repl of spec.parameters.tls.internalCertificateSecretName") {
			t.Fatalf("error = %v, want a wait for the Secret repl", err)
		}
	})

	t.Run("labels the client CA Secret", func(t *testing.T) {
		ca := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "client-ca", Namespace: testNamespace}}
		c, cl := tlsContext(t, ca)
		if err := syncCertificates(c, testCluster(), definition.TLSParameters{ClientCASecretName: "client-ca"}); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(context.Background(), client.ObjectKeyFromObject(ca), ca); err != nil {
			t.Fatal(err)
		}
		if _, ok := ca.Labels[utils.WatchedLabelName]; !ok {
			t.Fatalf("labels = %v, want %s", ca.Labels, utils.WatchedLabelName)
		}
	})
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
		issuer  definition.TLSParameters
		objects []client.Object
		want    string
	}{
		{name: "missing ClusterIssuer", issuer: issuerTLS("nope", "ClusterIssuer"), want: "the ClusterIssuer nope does not exist"},
		{name: "missing Issuer", issuer: issuerTLS("nope", "Issuer"), want: "the Issuer nope does not exist in namespace ns"},
		{
			name: "issuer not ready", issuer: issuerTLS("databases-ca", "Issuer"), objects: []client.Object{notReady},
			want: "the Issuer databases-ca is not ready: Error getting keypair for CA issuer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := tlsContext(t, tt.objects...)
			err := syncCertificates(c, testCluster(), tt.issuer)
			if !controller.IsWaitError(err) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want a wait saying %q", err, tt.want)
			}
		})
	}
}

func TestValidateTLSChange(t *testing.T) {
	cluster := func(certificates *cnpgv1.CertificatesConfiguration) *cnpgv1.Cluster {
		pg := testCluster()
		pg.Spec.Certificates = certificates
		return pg
	}
	issued := func(kind string) *cmv1.Certificate {
		return &cmv1.Certificate{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-tls", Namespace: testNamespace},
			Spec:       cmv1.CertificateSpec{IssuerRef: cmmeta.IssuerReference{Name: "acme", Kind: kind}},
		}
	}
	issuerCertificates := &cnpgv1.CertificatesConfiguration{ServerTLSSecret: "pg-tls", ServerCASecret: "pg-tls"}
	tests := []struct {
		name    string
		tls     definition.TLSParameters
		objects []client.Object
		wantErr string
	}{
		{name: "no Cluster yet", tls: definition.TLSParameters{CertificateSecretName: "mine"}},
		{name: "CloudNativePG's with more names", tls: definition.TLSParameters{AdditionalNames: []string{"db.example.com"}},
			objects: []client.Object{cluster(nil)}},
		{name: "same issuer", tls: issuerTLS("acme", "Issuer"), objects: []client.Object{cluster(issuerCertificates), issued("")}},
		{name: "the certificates of an earlier release",
			tls: definition.TLSParameters{CertificateSecretName: "mine", InternalCertificateSecretName: "repl"},
			objects: []client.Object{cluster(&cnpgv1.CertificatesConfiguration{
				ServerTLSSecret: "mine", ServerCASecret: "mine-ca", ReplicationTLSSecret: "repl", ClientCASecret: "repl-ca",
			})}},
		{name: "a secret on CloudNativePG's", tls: definition.TLSParameters{CertificateSecretName: "mine"},
			objects: []client.Object{cluster(nil)}, wantErr: "the CA CloudNativePG created for it"},
		{name: "a secret instead of the issuer", tls: definition.TLSParameters{CertificateSecretName: "mine"},
			objects: []client.Object{cluster(issuerCertificates)}, wantErr: "its cert-manager issuer"},
		{name: "another issuer kind", tls: issuerTLS("acme", "ClusterIssuer"),
			objects: []client.Object{cluster(issuerCertificates), issued("Issuer")}, wantErr: "issued by the Issuer acme of cert-manager.io"},
		{name: "a replication certificate", tls: definition.TLSParameters{InternalCertificateSecretName: "repl"},
			objects: []client.Object{cluster(nil)}, wantErr: "replicas authenticate with"},
		{name: "off the CA of an earlier release's serverCASecret", tls: definition.TLSParameters{},
			objects: []client.Object{cluster(&cnpgv1.CertificatesConfiguration{ServerCASecret: "own-ca"})}, wantErr: "from the CA of the Secret own-ca"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := tlsContext(t, tt.objects...)
			errs, err := validateTLSChange(c, tt.tls)
			if err != nil {
				t.Fatal(err)
			}
			got := errs.ToAggregate()
			if tt.wantErr == "" {
				if got != nil {
					t.Fatalf("unexpected error: %v", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", got, tt.wantErr)
			}
		})
	}
}

func TestValidateCertificateSecretsLeavesMissingOnesToSync(t *testing.T) {
	c, _ := tlsContext(t)
	errs, err := validateCertificateSecrets(c, definition.TLSParameters{CertificateSecretName: "mine", InternalCertificateSecretName: "repl"})
	if err != nil || len(errs) > 0 {
		t.Fatalf("errs = %v, err = %v", errs, err)
	}
}

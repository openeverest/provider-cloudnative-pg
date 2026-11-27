package cnpg

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-cloudnative-pg/definition"
)

// tlsInstance is an Instance named pg with spec.parameters set to params and
// the engine parameters to engine, either "" for none.
func tlsInstance(params, engine string) *corev1alpha1.Instance {
	in := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "pg"}}
	in.Spec.Components = map[string]corev1alpha1.ComponentSpec{"engine": {}}
	if params != "" {
		in.Spec.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}
	if engine != "" {
		in.Spec.Components["engine"] = corev1alpha1.ComponentSpec{Parameters: &runtime.RawExtension{Raw: []byte(engine)}}
	}
	return in
}

func mustTLS(t *testing.T, params string) definition.TLSParameters {
	t.Helper()
	tls, err := TLS(tlsInstance(params, ""))
	if err != nil {
		t.Fatal(err)
	}
	return tls
}

func TestTLSDefaults(t *testing.T) {
	tls := mustTLS(t, `{"tls":{"issuer":{"name":"acme"}}}`)
	if tls.Mode != definition.TLSModePrefer || tls.Issuer.Kind != definition.IssuerKindIssuer || tls.Issuer.Group != CertManagerGroup {
		t.Fatalf("defaults = %+v, issuer %+v", tls, tls.Issuer)
	}
}

func TestApplyTLS(t *testing.T) {
	userRules := []string{"host app app 10.0.0.0/8 scram-sha-256"}
	tests := []struct {
		name   string
		params string
		hba    []string
		want   *cnpgv1.CertificatesConfiguration
	}{
		{name: "unset keeps CNPG's certificates and rules", hba: userRules},
		{name: "preferTLS keeps the rules", params: `{"tls":{"mode":"preferTLS"}}`, hba: userRules},
		{name: "requireTLS goes before user rules", params: `{"tls":{"mode":"requireTLS"}}`, hba: append([]string{RejectNonTLSRule}, userRules...)},
		{
			name: "certificate secret is the server certificate and its CA", params: `{"tls":{"certificateSecretName":"mine"}}`, hba: userRules,
			want: &cnpgv1.CertificatesConfiguration{ServerTLSSecret: "mine", ServerCASecret: "mine"},
		},
		{
			name: "issuer uses the Certificate's Secret, its names go to the Certificate", hba: userRules,
			params: `{"tls":{"issuer":{"name":"acme"},"additionalNames":["db.example.com"]}}`,
			want:   &cnpgv1.CertificatesConfiguration{ServerTLSSecret: "pg-tls", ServerCASecret: "pg-tls"},
		},
		{
			name: "additional names alone go to the certificate CNPG issues", params: `{"tls":{"additionalNames":["db.example.com"]}}`, hba: userRules,
			want: &cnpgv1.CertificatesConfiguration{ServerAltDNSNames: []string{"db.example.com"}},
		},
		{
			name: "internal certificate is the replication certificate and its CA", params: `{"tls":{"internalCertificateSecretName":"repl"}}`, hba: userRules,
			want: &cnpgv1.CertificatesConfiguration{ReplicationTLSSecret: "repl", ClientCASecret: "repl"},
		},
		{
			name: "client CA alone: CNPG signs replication with its ca.key", params: `{"tls":{"clientCASecretName":"client-ca"}}`, hba: userRules,
			want: &cnpgv1.CertificatesConfiguration{ClientCASecret: "client-ca"},
		},
		{
			name: "client CA and replication certificate are separate Secrets", params: `{"tls":{"clientCASecretName":"client-ca","internalCertificateSecretName":"repl"}}`, hba: userRules,
			want: &cnpgv1.CertificatesConfiguration{ClientCASecret: "client-ca", ReplicationTLSSecret: "repl"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg := &cnpgv1.Cluster{}
			pg.Spec.PostgresConfiguration.PgHBA = slices.Clone(userRules)
			ApplyTLS(pg, mustTLS(t, tt.params), "pg")
			if !reflect.DeepEqual(pg.Spec.Certificates, tt.want) {
				t.Errorf("certificates = %+v, want %+v", pg.Spec.Certificates, tt.want)
			}
			if !slices.Equal(pg.Spec.PostgresConfiguration.PgHBA, tt.hba) {
				t.Errorf("pg_hba = %q, want %q", pg.Spec.PostgresConfiguration.PgHBA, tt.hba)
			}
		})
	}
}

func TestValidateTLS(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		proxy   bool
		wantErr string
	}{
		{name: "unset"},
		{name: "requireTLS", params: `{"tls":{"mode":"requireTLS"}}`},
		{name: "issuer with names", params: `{"tls":{"issuer":{"name":"acme","kind":"ClusterIssuer"},"additionalNames":["db.example.com","*.db.example.com"]}}`},
		{name: "external issuer", params: `{"tls":{"issuer":{"name":"pca","kind":"AWSPCAClusterIssuer","group":"awspca.cert-manager.io"}}}`},
		{name: "certificate secrets", params: `{"tls":{"certificateSecretName":"pg-cert","internalCertificateSecretName":"pg-repl"}}`},
		{name: "internal certificate with issuer", params: `{"tls":{"issuer":{"name":"acme"},"internalCertificateSecretName":"pg-repl"}}`},
		{name: "client CA", params: `{"tls":{"clientCASecretName":"client-ca"}}`},
		{name: "client CA with replication certificate", params: `{"tls":{"clientCASecretName":"client-ca","internalCertificateSecretName":"pg-repl"}}`},
		{name: "client CA with proxy", params: `{"tls":{"clientCASecretName":"client-ca"}}`, proxy: true},
		{name: "client CA, replication certificate and proxy", params: `{"tls":{"clientCASecretName":"client-ca","internalCertificateSecretName":"pg-repl"}}`, proxy: true},
		{name: "disabled", params: `{"tls":{"mode":"disabled"}}`, wantErr: "TLS cannot be turned off"},
		{name: "unknown mode", params: `{"tls":{"mode":"allowTLS"}}`, wantErr: `Unsupported value: "allowTLS"`},
		{name: "issuer and secret", params: `{"tls":{"certificateSecretName":"pg-cert","issuer":{"name":"acme"}}}`, wantErr: "not both"},
		{name: "issuer without name", params: `{"tls":{"issuer":{"kind":"Issuer"}}}`, wantErr: "spec.parameters.tls.issuer.name: Required"},
		{name: "unknown cert-manager kind", params: `{"tls":{"issuer":{"name":"acme","kind":"VaultIssuer"}}}`, wantErr: `Unsupported value: "VaultIssuer"`},
		{name: "invalid secret name", params: `{"tls":{"certificateSecretName":"PG_cert"}}`, wantErr: "spec.parameters.tls.certificateSecretName: Invalid"},
		{name: "secret named like CloudNativePG's", params: `{"tls":{"internalCertificateSecretName":"pg-replication"}}`, wantErr: "CloudNativePG writes its own certificate to"},
		{name: "client CA named like CloudNativePG's", params: `{"tls":{"clientCASecretName":"pg-ca"}}`, wantErr: "CloudNativePG writes its own certificate to"},
		{name: "names with secret", params: `{"tls":{"certificateSecretName":"pg-cert","additionalNames":["db.example.com"]}}`, wantErr: "carries its own names"},
		{name: "IP address as name", params: `{"tls":{"additionalNames":["10.0.0.1"]}}`, wantErr: "not an IP address"},
		{name: "invalid name", params: `{"tls":{"additionalNames":["db_example"]}}`, wantErr: "additionalNames[0]: Invalid"},
		{name: "internal certificate with proxy", params: `{"tls":{"internalCertificateSecretName":"pg-repl"}}`, proxy: true, wantErr: "cannot be combined with the proxy component"},
		{name: "not JSON", params: `{"tls":[]}`, wantErr: "decode spec.parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, ValidateTLS(tlsInstance(tt.params, ""), tt.proxy).ToAggregate(), tt.wantErr)
		})
	}
}

func TestValidateTLSTransition(t *testing.T) {
	reconciled := func(in *corev1alpha1.Instance) *corev1alpha1.Instance {
		in.Status.Version = "18.7"
		return in
	}
	tests := []struct {
		name     string
		old, new *corev1alpha1.Instance
		wantErr  string
	}{
		{name: "create", new: tlsInstance(`{"tls":{"certificateSecretName":"mine"}}`, "")},
		{name: "not reconciled yet", old: tlsInstance("", ""), new: tlsInstance(`{"tls":{"certificateSecretName":"mine"}}`, "")},
		{name: "mode and names change", old: reconciled(tlsInstance(`{"tls":{"issuer":{"name":"acme"}}}`, "")),
			new: tlsInstance(`{"tls":{"mode":"requireTLS","issuer":{"name":"acme","kind":"Issuer"},"additionalNames":["db.example.com"]}}`, "")},
		{name: "set a secret", old: reconciled(tlsInstance("", "")), new: tlsInstance(`{"tls":{"certificateSecretName":"mine"}}`, ""), wantErr: "only be set when the instance is created"},
		{name: "set a client CA", old: reconciled(tlsInstance("", "")), new: tlsInstance(`{"tls":{"clientCASecretName":"client-ca"}}`, ""), wantErr: "only be set when the instance is created"},
		{name: "remove the issuer", old: reconciled(tlsInstance(`{"tls":{"issuer":{"name":"acme"}}}`, "")), new: tlsInstance("", ""), wantErr: "only be set when the instance is created"},
		{name: "change the issuer kind", old: reconciled(tlsInstance(`{"tls":{"issuer":{"name":"acme"}}}`, "")),
			new: tlsInstance(`{"tls":{"issuer":{"name":"acme","kind":"ClusterIssuer"}}}`, ""), wantErr: "only be set when the instance is created"},
		{name: "move the certificates of an earlier release",
			old: reconciled(tlsInstance("", `{"certificates":{"serverTLSSecret":"mine","serverCASecret":"mine","replicationTLSSecret":"repl","clientCASecret":"repl"}}`)),
			new: tlsInstance(`{"tls":{"certificateSecretName":"mine","internalCertificateSecretName":"repl"}}`, "")},
		{name: "move separate client CA of an earlier release",
			old: reconciled(tlsInstance("", `{"certificates":{"replicationTLSSecret":"repl","clientCASecret":"client-ca"}}`)),
			new: tlsInstance(`{"tls":{"internalCertificateSecretName":"repl","clientCASecretName":"client-ca"}}`, "")},
		{name: "change the certificates of an earlier release",
			old: reconciled(tlsInstance("", `{"certificates":{"serverTLSSecret":"mine","serverCASecret":"mine"}}`)),
			new: tlsInstance(`{"tls":{"certificateSecretName":"other"}}`, ""), wantErr: "only be set when the instance is created"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, ValidateTLSTransition(tt.new, tt.old).ToAggregate(), tt.wantErr)
		})
	}
}

func TestCertificateSourceIgnoresNames(t *testing.T) {
	a := mustTLS(t, `{"tls":{"issuer":{"name":"acme"}}}`)
	b := mustTLS(t, `{"tls":{"mode":"requireTLS","issuer":{"name":"acme"},"additionalNames":["db.example.com"]}}`)
	if CertificateSource(a) != CertificateSource(b) || !strings.Contains(CertificateSource(a), "acme") {
		t.Fatalf("sources %q and %q", CertificateSource(a), CertificateSource(b))
	}
}

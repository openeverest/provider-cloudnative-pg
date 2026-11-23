package cnpg

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/AlekSi/pointer"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

func TestApplyRequireTLS(t *testing.T) {
	userRules := []string{"host app app 10.0.0.0/8 scram-sha-256", "hostssl all all all cert"}
	tests := []struct {
		name       string
		requireTLS *bool
		userRules  []string
		want       []string
	}{
		{name: "unset keeps CNPG defaults", userRules: userRules, want: userRules},
		{name: "false keeps CNPG defaults", requireTLS: pointer.To(false), userRules: userRules, want: userRules},
		{name: "required without user rules", requireTLS: pointer.To(true), want: []string{RejectNonTLSRule}},
		{
			name: "required goes before user rules", requireTLS: pointer.To(true), userRules: userRules,
			want: append([]string{RejectNonTLSRule}, userRules...),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg := &cnpgv1.Cluster{}
			pg.Spec.PostgresConfiguration.PgHBA = slices.Clone(tt.userRules)
			ApplyRequireTLS(pg, &components.CNPGCustomSpec{RequireTLS: tt.requireTLS})
			if !slices.Equal(pg.Spec.PostgresConfiguration.PgHBA, tt.want) {
				t.Fatalf("pg_hba = %q, want %q", pg.Spec.PostgresConfiguration.PgHBA, tt.want)
			}
		})
	}
}

func TestApplyTLS(t *testing.T) {
	replication := cnpgv1.CertificatesConfiguration{ClientCASecret: "client-ca", ReplicationTLSSecret: "client-ca"}
	tests := []struct {
		name         string
		tls          *components.TLSSpec
		certificates *cnpgv1.CertificatesConfiguration
		want         *cnpgv1.CertificatesConfiguration
	}{
		{name: "unset keeps the certificates", certificates: &replication, want: &replication},
		{name: "empty keeps CNPG's own", tls: &components.TLSSpec{}},
		{
			name: "issuer uses the Certificate's Secret for server and CA",
			tls:  &components.TLSSpec{Issuer: &components.IssuerReference{Name: "acme"}, ExtraDNSNames: []string{"db.example.com"}},
			want: &cnpgv1.CertificatesConfiguration{ServerTLSSecret: "pg-tls", ServerCASecret: "pg-tls"},
		},
		{
			name:         "certificate secret keeps the replication certificates",
			tls:          &components.TLSSpec{CertificateSecretName: "mine"},
			certificates: &replication,
			want: &cnpgv1.CertificatesConfiguration{
				ServerTLSSecret: "mine", ServerCASecret: "mine", ClientCASecret: "client-ca", ReplicationTLSSecret: "client-ca",
			},
		},
		{
			name: "extra names alone go to the certificate CNPG issues",
			tls:  &components.TLSSpec{ExtraDNSNames: []string{"db.example.com"}},
			want: &cnpgv1.CertificatesConfiguration{ServerAltDNSNames: []string{"db.example.com"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pg := &cnpgv1.Cluster{}
			pg.Spec.Certificates = tt.certificates
			ApplyTLS(pg, tt.tls, "pg")
			if !reflect.DeepEqual(pg.Spec.Certificates, tt.want) {
				t.Fatalf("certificates = %+v, want %+v", pg.Spec.Certificates, tt.want)
			}
		})
	}

	t.Run("does not change the certificates of the custom spec", func(t *testing.T) {
		custom := replication
		pg := &cnpgv1.Cluster{}
		pg.Spec.Certificates = &custom
		ApplyTLS(pg, &components.TLSSpec{CertificateSecretName: "mine"}, "pg")
		if !reflect.DeepEqual(custom, replication) {
			t.Fatalf("custom certificates changed to %+v", custom)
		}
	})
}

func TestValidateTLS(t *testing.T) {
	issuer := &components.IssuerReference{Name: "acme", Kind: "ClusterIssuer"}
	tests := []struct {
		name         string
		tls          *components.TLSSpec
		certificates *cnpgv1.CertificatesConfiguration
		wantErr      string
	}{
		{name: "unset"},
		{name: "issuer", tls: &components.TLSSpec{Issuer: issuer, ExtraDNSNames: []string{"db.example.com", "*.db.example.com"}}},
		{name: "issuer kind defaults", tls: &components.TLSSpec{Issuer: &components.IssuerReference{Name: "acme"}}},
		{name: "external issuer", tls: &components.TLSSpec{Issuer: &components.IssuerReference{Name: "pca", Kind: "AWSPCAClusterIssuer", Group: "awspca.cert-manager.io"}}},
		{name: "certificate secret", tls: &components.TLSSpec{CertificateSecretName: "pg-cert"}},
		{name: "extra names alone", tls: &components.TLSSpec{ExtraDNSNames: []string{"db.example.com"}}},
		{
			name: "issuer with replication certificates", tls: &components.TLSSpec{Issuer: issuer},
			certificates: &cnpgv1.CertificatesConfiguration{ClientCASecret: "ca", ReplicationTLSSecret: "repl"},
		},
		{
			name: "issuer and secret", tls: &components.TLSSpec{Issuer: issuer, CertificateSecretName: "pg-cert"},
			wantErr: "not both",
		},
		{name: "issuer without name", tls: &components.TLSSpec{Issuer: &components.IssuerReference{Kind: "Issuer"}}, wantErr: "tls.issuer.name is required"},
		{name: "invalid issuer name", tls: &components.TLSSpec{Issuer: &components.IssuerReference{Name: "Acme"}}, wantErr: "tls.issuer.name"},
		{
			name: "unknown cert-manager kind", tls: &components.TLSSpec{Issuer: &components.IssuerReference{Name: "acme", Kind: "VaultIssuer"}},
			wantErr: "not a cert-manager issuer",
		},
		{name: "invalid secret name", tls: &components.TLSSpec{CertificateSecretName: "PG_cert"}, wantErr: "tls.certificateSecretName"},
		{
			name: "extra names with secret", tls: &components.TLSSpec{CertificateSecretName: "pg-cert", ExtraDNSNames: []string{"db.example.com"}},
			wantErr: "carries its own names",
		},
		{name: "invalid extra name", tls: &components.TLSSpec{ExtraDNSNames: []string{"db_example"}}, wantErr: "not a DNS name"},
		{
			name: "issuer with server certificate", tls: &components.TLSSpec{Issuer: issuer},
			certificates: &cnpgv1.CertificatesConfiguration{ServerTLSSecret: "s", ServerCASecret: "ca"},
			wantErr:      "they set the same certificate",
		},
		{
			name: "secret with server CA", tls: &components.TLSSpec{CertificateSecretName: "pg-cert"},
			certificates: &cnpgv1.CertificatesConfiguration{ServerCASecret: "ca"},
			wantErr:      "they set the same certificate",
		},
		{
			name: "extra names with server alt names", tls: &components.TLSSpec{ExtraDNSNames: []string{"db.example.com"}},
			certificates: &cnpgv1.CertificatesConfiguration{ServerAltDNSNames: []string{"other.example.com"}},
			wantErr:      "use one of them",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTLS(tt.tls, tt.certificates)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

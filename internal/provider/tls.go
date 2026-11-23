package provider

import (
	"fmt"
	"slices"
	"strings"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// caCertificateKey is the Secret key of the CA certificate, in the Secrets
// cert-manager writes and in those CloudNativePG reads.
const caCertificateKey = "ca.crt"

// syncServerCertificate makes the server certificate tls sets available
// before the Cluster refers to it. With an issuer it requests the
// Certificate and waits for cert-manager to issue it; with a Secret it waits
// for the Secret and labels it, so PostgreSQL reloads a renewed certificate.
func syncServerCertificate(c *controller.Context, pg *cnpgv1.Cluster, tls *components.TLSSpec) error {
	switch {
	case tls == nil:
		return nil
	case tls.Issuer != nil:
		return syncIssuedCertificate(c, pg, tls)
	case tls.CertificateSecretName != "":
		return labelCertificateSecret(c, tls.CertificateSecretName)
	}
	return nil
}

func syncIssuedCertificate(c *controller.Context, pg *cnpgv1.Cluster, tls *components.TLSSpec) error {
	name := cnpg.ServerCertificateName(c.Name())
	if err := c.Apply(serverCertificate(c, pg, tls)); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("tls.issuer needs cert-manager, and the cluster does not serve its Certificate kind: %w", err)
		}
		return fmt.Errorf("apply cert-manager Certificate %q: %w", name, err)
	}
	certificate, err := uncachedCertificate(c, name)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{}
	if err := c.Get(secret, name); apierrors.IsNotFound(err) {
		return controller.WaitFor(certificateWait(certificate, issuerProblem(c, tls.Issuer)))
	} else if err != nil {
		return fmt.Errorf("get Secret %q: %w", name, err)
	}
	if len(secret.Data[caCertificateKey]) == 0 {
		return fmt.Errorf("the issuer of the cert-manager Certificate %q writes no CA certificate (%s) to the Secret, "+
			"and clients and CloudNativePG verify the server certificate with it: use an issuer that does, such as a CA or Vault issuer",
			name, caCertificateKey)
	}
	return ownCertificateSecret(c, secret, certificate)
}

// serverCertificate is the cert-manager Certificate for the names clients
// reach the instance at: its Services, its poolers' and extraDNSNames.
func serverCertificate(c *controller.Context, pg *cnpgv1.Cluster, tls *components.TLSSpec) *cmv1.Certificate {
	name := cnpg.ServerCertificateName(c.Name())
	return &cmv1.Certificate{
		ObjectMeta: c.ObjectMeta(name),
		Spec: cmv1.CertificateSpec{
			SecretName: name,
			// CloudNativePG reloads a renewed certificate only from a Secret with this label.
			SecretTemplate: &cmv1.CertificateSecretTemplate{
				Labels: map[string]string{utils.WatchedLabelName: "true"},
			},
			DNSNames: slices.Concat(serviceDNSNames(pg, c.Name()), tls.ExtraDNSNames),
			Usages:   []cmv1.KeyUsage{cmv1.UsageDigitalSignature, cmv1.UsageKeyEncipherment, cmv1.UsageServerAuth},
			IssuerRef: cmmeta.IssuerReference{
				Name:  tls.Issuer.Name,
				Kind:  cnpg.IssuerKind(tls.Issuer),
				Group: cnpg.IssuerGroup(tls.Issuer),
			},
		},
	}
}

// serviceDNSNames are the names of the Cluster's Services and of the
// instance's pooler Services, in the forms CloudNativePG gives its own
// certificate: name, name.namespace, name.namespace.svc and the cluster
// domain one. The pooler names are always there, so turning the proxy on
// does not reissue the certificate.
func serviceDNSNames(pg *cnpgv1.Cluster, instance string) []string {
	withPoolers := pg.DeepCopy()
	withPoolers.Spec.Certificates = nil
	if withPoolers.Spec.Managed == nil {
		withPoolers.Spec.Managed = &cnpgv1.ManagedConfiguration{}
	}
	if withPoolers.Spec.Managed.Services == nil {
		withPoolers.Spec.Managed.Services = &cnpgv1.ManagedServices{}
	}
	for _, pooler := range []string{poolerName(instance), poolerROName(instance)} {
		withPoolers.Spec.Managed.Services.Additional = append(withPoolers.Spec.Managed.Services.Additional,
			cnpgv1.ManagedService{ServiceTemplate: cnpgv1.ServiceTemplateSpec{ObjectMeta: cnpgv1.Metadata{Name: pooler}}})
	}
	return withPoolers.GetClusterAltDNSNames()
}

// uncachedCertificate reads the Certificate from the API server: the client
// caches no Certificates, which may not even be served in a cluster without
// cert-manager.
func uncachedCertificate(c *controller.Context, name string) (*cmv1.Certificate, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cmv1.SchemeGroupVersion.WithKind(cmv1.CertificateKind))
	if err := c.Get(obj, name); err != nil {
		return nil, fmt.Errorf("get cert-manager Certificate %q: %w", name, err)
	}
	certificate := &cmv1.Certificate{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, certificate); err != nil {
		return nil, fmt.Errorf("decode cert-manager Certificate %q: %w", name, err)
	}
	return certificate, nil
}

// certificateWait says what an unissued Certificate waits for: what is wrong
// with its issuer, or else what cert-manager reports about it.
func certificateWait(certificate *cmv1.Certificate, issuerProblem string) string {
	wait := fmt.Sprintf("Waiting for cert-manager to issue the Certificate %s", certificate.Name)
	if issuerProblem != "" {
		return wait + ": " + issuerProblem
	}
	for _, condition := range certificate.Status.Conditions {
		if condition.Type == cmv1.CertificateConditionReady && condition.Message != "" {
			return wait + ": " + condition.Message
		}
	}
	return wait
}

// ownCertificateSecret makes the Certificate own the Secret cert-manager
// wrote, which cert-manager leaves without an owner. Deleting the Instance
// then deletes the private key with the Certificate, and orphaning the
// Instance's objects keeps both.
func ownCertificateSecret(c *controller.Context, secret *corev1.Secret, certificate *cmv1.Certificate) error {
	if slices.ContainsFunc(secret.OwnerReferences, func(ref metav1.OwnerReference) bool { return ref.UID == certificate.UID }) {
		return nil
	}
	patch := client.MergeFrom(secret.DeepCopy())
	secret.OwnerReferences = append(secret.OwnerReferences, metav1.OwnerReference{
		APIVersion: cmv1.SchemeGroupVersion.String(),
		Kind:       cmv1.CertificateKind,
		Name:       certificate.Name,
		UID:        certificate.UID,
	})
	if err := c.Client().Patch(c.Context(), secret, patch); err != nil {
		return fmt.Errorf("set the owner of Secret %q: %w", secret.Name, err)
	}
	return nil
}

// labelCertificateSecret waits for the Secret of tls.certificateSecretName
// and labels it, so CloudNativePG reloads the certificate when it changes.
func labelCertificateSecret(c *controller.Context, name string) error {
	secret := &corev1.Secret{}
	if err := c.Get(secret, name); apierrors.IsNotFound(err) {
		return controller.WaitFor(fmt.Sprintf("Waiting for the Secret %s with the server certificate (tls.certificateSecretName)", name))
	} else if err != nil {
		return fmt.Errorf("get Secret %q: %w", name, err)
	}
	if _, labelled := secret.Labels[utils.WatchedLabelName]; labelled {
		return nil
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[utils.WatchedLabelName] = "true"
	if err := c.Client().Patch(c.Context(), secret, patch); err != nil {
		return fmt.Errorf("label Secret %q for reload: %w", name, err)
	}
	return nil
}

// deleteUnusedServerCertificate deletes the Certificate requested for an
// issuer the Instance no longer sets, with the Secret it owns, once the
// Cluster has stopped using it.
func deleteUnusedServerCertificate(c *controller.Context, tls *components.TLSSpec) error {
	if tls != nil && tls.Issuer != nil {
		return nil
	}
	name := cnpg.ServerCertificateName(c.Name())
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(cmv1.SchemeGroupVersion.WithKind(cmv1.CertificateKind))
	if err := c.Get(certificate, name); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("get cert-manager Certificate %q: %w", name, err)
	}
	if !metav1.IsControlledBy(certificate, c.Instance()) {
		return nil
	}

	pg := &cnpgv1.Cluster{}
	if err := c.Get(pg, c.Name()); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pg.Status.Certificates.ServerTLSSecret == name {
		// The Cluster's next status change starts another Sync.
		return nil
	}
	return client.IgnoreNotFound(c.Delete(certificate))
}

// validateTLSSecret checks the Secret of the server certificate tls sets,
// once it exists: Sync waits for a missing one.
func validateTLSSecret(c *controller.Context, tls *components.TLSSpec) error {
	name := cnpg.ServerCertificateSecret(tls, c.Name())
	if name == "" {
		return nil
	}
	secret, err := optionalSecret(c, name)
	if secret == nil {
		return err
	}
	return cnpg.ValidateServerCertificate(secret, secret, readWriteHosts(c))
}

// validateCertificatesSecrets checks the Secrets of the certificates
// passthrough that exist; CloudNativePG reports a missing one.
func validateCertificatesSecrets(c *controller.Context, custom *components.CNPGCustomSpec) error {
	certificates := custom.Certificates
	if certificates == nil {
		return nil
	}
	switch {
	case certificates.ServerTLSSecret != "" && certificates.ServerCASecret != "":
		server, err := optionalSecret(c, certificates.ServerTLSSecret)
		if server == nil {
			return err
		}
		ca, err := optionalSecret(c, certificates.ServerCASecret)
		if ca == nil {
			return err
		}
		if err := cnpg.ValidateServerCertificate(server, ca, readWriteHosts(c)); err != nil {
			return fmt.Errorf("serverTLSSecret: %w", err)
		}
	case certificates.ServerCASecret != "":
		if err := validateSigningCA(c, certificates.ServerCASecret, "serverCASecret"); err != nil {
			return err
		}
	}

	// CloudNativePG issues the replication certificate with the client CA,
	// unless it is given one, and always the client certificate of PgBouncer.
	_, proxy := c.Instance().Spec.Components[common.ComponentProxy]
	if certificates.ClientCASecret == "" || (certificates.ReplicationTLSSecret != "" && !proxy) {
		return nil
	}
	err := validateSigningCA(c, certificates.ClientCASecret, "clientCASecret")
	if err != nil && certificates.ReplicationTLSSecret != "" {
		return fmt.Errorf("%w; CloudNativePG issues the client certificate of the proxy component's PgBouncer with it, "+
			"so leave clientCASecret and replicationTLSSecret unset to have it manage the replication certificates", err)
	}
	return err
}

func validateSigningCA(c *controller.Context, name, field string) error {
	secret, err := optionalSecret(c, name)
	if secret == nil {
		return err
	}
	return cnpg.ValidateSigningCA(secret, field)
}

// optionalSecret reads a Secret, nil when it does not exist.
func optionalSecret(c *controller.Context, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.Get(secret, name); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return secret, nil
}

// readWriteHosts are the names of the read-write Service clients connect to.
func readWriteHosts(c *controller.Context) []string {
	pg := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: c.Name(), Namespace: c.Namespace()}}
	return slices.DeleteFunc(pg.GetClusterAltDNSNames(), func(name string) bool {
		return !strings.HasPrefix(name, pg.GetServiceReadWriteName())
	})
}

// issuerProblem says why the issuer cannot issue the Certificate, or "" when
// it is ready or its readiness cannot be told.
func issuerProblem(c *controller.Context, issuer *components.IssuerReference) string {
	kind := cnpg.IssuerKind(issuer)
	if cnpg.IssuerGroup(issuer) != cmv1.SchemeGroupVersion.Group || (kind != cmv1.IssuerKind && kind != cmv1.ClusterIssuerKind) {
		return ""
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cmv1.SchemeGroupVersion.WithKind(kind))
	if err := c.Get(obj, issuer.Name); apierrors.IsNotFound(err) {
		if kind == cmv1.IssuerKind {
			return fmt.Sprintf("the Issuer %s does not exist in namespace %s", issuer.Name, c.Namespace())
		}
		return fmt.Sprintf("the ClusterIssuer %s does not exist", issuer.Name)
	} else if err != nil {
		return ""
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, condition := range conditions {
		condition, _ := condition.(map[string]any)
		if condition["type"] == string(cmv1.IssuerConditionReady) && condition["status"] == string(cmmeta.ConditionFalse) {
			return fmt.Sprintf("the %s %s is not ready: %v", kind, issuer.Name, condition["message"])
		}
	}
	return ""
}

// reissueStaleCertificates deletes the server and replication certificates
// CloudNativePG issued when they are no longer signed by the CA it now issues
// them with, so it issues them again. CloudNativePG renews its certificates
// only when they expire: after a change of certificates.serverCASecret or
// clientCASecret it would keep presenting the old server certificate, which
// clients and replicas no longer trust, and replicas would keep presenting a
// replication certificate the primary no longer accepts. It waits until
// CloudNativePG has seen the new CA, so it does not issue from the old one.
func reissueStaleCertificates(c *controller.Context, desired *cnpgv1.Cluster) error {
	pg := &cnpgv1.Cluster{}
	if err := c.Get(pg, c.Name()); err != nil {
		return client.IgnoreNotFound(err)
	}
	certificates := desired.Spec.Certificates
	if certificates == nil {
		certificates = &cnpgv1.CertificatesConfiguration{}
	}
	observed := pg.Status.Certificates
	for _, issued := range []struct {
		supplied, secret, ca, observedCA string
	}{
		{certificates.ServerTLSSecret, desired.GetServerTLSSecretName(), desired.GetServerCASecretName(), observed.ServerCASecret},
		{certificates.ReplicationTLSSecret, desired.GetReplicationSecretName(), desired.GetClientCASecretName(), observed.ClientCASecret},
	} {
		if issued.supplied != "" || issued.observedCA != issued.ca {
			continue
		}
		if err := deleteIfNotSignedBy(c, pg, issued.secret, issued.ca); err != nil {
			return err
		}
	}
	return nil
}

func deleteIfNotSignedBy(c *controller.Context, pg *cnpgv1.Cluster, secretName, caName string) error {
	secret, err := optionalSecret(c, secretName)
	if secret == nil || !metav1.IsControlledBy(secret, pg) {
		return err
	}
	ca, err := optionalSecret(c, caName)
	if ca == nil || cnpg.SignedBy(secret, ca) {
		return err
	}
	log.FromContext(c.Context()).Info("Deleting a certificate CloudNativePG issued with another CA, to have it issued again",
		"secret", secretName, "ca", caName)
	return client.IgnoreNotFound(c.Delete(secret))
}

package provider

import (
	"cmp"
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
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
)

// caCertificateKey is the Secret key of the CA certificate, in the Secrets
// cert-manager writes and in those CloudNativePG reads.
const caCertificateKey = "ca.crt"

// syncCertificates makes the certificates tls sets available before the
// Cluster refers to them. With an issuer it requests the server Certificate
// and waits for cert-manager to issue it; with Secrets it waits for them and
// labels them, so PostgreSQL reloads a renewed certificate.
func syncCertificates(c *controller.Context, pg *cnpgv1.Cluster, tls definition.TLSParameters) error {
	switch {
	case tls.CertificateSecretName != "":
		if err := labelCertificateSecret(c, tls.CertificateSecretName, "certificateSecretName"); err != nil {
			return err
		}
	case tls.Issuer != nil:
		if err := syncIssuedCertificate(c, pg, tls); err != nil {
			return err
		}
	}
	if tls.InternalCertificateSecretName != "" {
		return labelCertificateSecret(c, tls.InternalCertificateSecretName, "internalCertificateSecretName")
	}
	return nil
}

func syncIssuedCertificate(c *controller.Context, pg *cnpgv1.Cluster, tls definition.TLSParameters) error {
	name := cnpg.ServerCertificateName(c.Name())
	if err := c.Apply(serverCertificate(c, pg, tls)); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("spec.parameters.tls.issuer needs cert-manager, and the cluster does not serve its Certificate kind: %w", err)
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
// reach the instance at: its Services, its poolers' and additionalNames.
func serverCertificate(c *controller.Context, pg *cnpgv1.Cluster, tls definition.TLSParameters) *cmv1.Certificate {
	name := cnpg.ServerCertificateName(c.Name())
	return &cmv1.Certificate{
		ObjectMeta: c.ObjectMeta(name),
		Spec: cmv1.CertificateSpec{
			SecretName: name,
			// CloudNativePG reloads a renewed certificate only from a Secret with this label.
			SecretTemplate: &cmv1.CertificateSecretTemplate{
				Labels: map[string]string{utils.WatchedLabelName: "true"},
			},
			DNSNames: slices.Concat(serviceDNSNames(pg, c.Name()), tls.AdditionalNames),
			Usages:   []cmv1.KeyUsage{cmv1.UsageDigitalSignature, cmv1.UsageKeyEncipherment, cmv1.UsageServerAuth},
			IssuerRef: cmmeta.IssuerReference{
				Name:  tls.Issuer.Name,
				Kind:  string(tls.Issuer.Kind),
				Group: tls.Issuer.Group,
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

// labelCertificateSecret waits for the certificate Secret the TLS parameter
// field names and labels it, so CloudNativePG reloads the certificate when
// it changes.
func labelCertificateSecret(c *controller.Context, name, field string) error {
	secret := &corev1.Secret{}
	if err := c.Get(secret, name); apierrors.IsNotFound(err) {
		return controller.WaitFor(fmt.Sprintf("Waiting for the Secret %s of spec.parameters.tls.%s", name, field))
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

// validateCertificateSecrets checks the certificate Secrets tls sets, once
// they exist: Sync waits for a missing one.
func validateCertificateSecrets(c *controller.Context, tls definition.TLSParameters) (field.ErrorList, error) {
	var errs field.ErrorList
	for _, check := range []struct {
		path     *field.Path
		name     string
		validate func(*corev1.Secret) error
	}{
		{serverCertificatePath(tls), cnpg.ServerCertificateSecret(tls, c.Name()), func(secret *corev1.Secret) error {
			return cnpg.ValidateServerCertificate(secret, secret, readWriteHosts(c))
		}},
		{cnpg.TLSPath.Child("internalCertificateSecretName"), tls.InternalCertificateSecretName, cnpg.ValidateReplicationCertificate},
	} {
		if check.name == "" {
			continue
		}
		secret, err := optionalSecret(c, check.name)
		if err != nil {
			return nil, err
		}
		if secret == nil {
			continue
		}
		if err := check.validate(secret); err != nil {
			errs = append(errs, field.Invalid(check.path, check.name, err.Error()))
		}
	}
	return errs, nil
}

// serverCertificatePath is the field that sets the server certificate.
func serverCertificatePath(tls definition.TLSParameters) *field.Path {
	if tls.CertificateSecretName == "" {
		return cnpg.TLSPath.Child("issuer")
	}
	return cnpg.TLSPath.Child("certificateSecretName")
}

// validateTLSChange is cnpg.ValidateTLSTransition against the running
// Cluster and the server Certificate, for reconcile time, when the previous
// Instance is unknown: the certificates the Cluster uses, and the issuer of
// the Certificate, can't change.
func validateTLSChange(c *controller.Context, tls definition.TLSParameters) (field.ErrorList, error) {
	live := &cnpgv1.Cluster{}
	if err := c.Get(live, c.Name()); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	current := cnpgv1.CertificatesConfiguration{}
	if live.Spec.Certificates != nil {
		current = *live.Spec.Certificates
	}
	desired := &cnpgv1.Cluster{}
	cnpg.ApplyTLS(desired, tls, c.Name())
	want := cnpgv1.CertificatesConfiguration{}
	if desired.Spec.Certificates != nil {
		want = *desired.Spec.Certificates
	}

	forbidden := func(detail string) field.ErrorList {
		return field.ErrorList{field.Forbidden(cnpg.TLSPath,
			"certificateSecretName, internalCertificateSecretName and issuer can only be set when the instance is created: "+detail)}
	}
	if current.ServerTLSSecret != want.ServerTLSSecret {
		return forbidden(fmt.Sprintf("the instance presents the server certificate of %s", describeServerSecret(current.ServerTLSSecret, c.Name()))), nil
	}
	if current.ReplicationTLSSecret != want.ReplicationTLSSecret {
		return forbidden(fmt.Sprintf("replicas authenticate with the certificate of %s", describeSecret(current.ReplicationTLSSecret))), nil
	}
	// CloudNativePG issues its certificates from a CA of an earlier release's
	// certificates.serverCASecret or clientCASecret, which the instance can't move off.
	if current.ServerTLSSecret == "" && current.ServerCASecret != want.ServerCASecret {
		return forbidden("CloudNativePG issues the server certificate from the CA of the Secret " + current.ServerCASecret), nil
	}
	if current.ReplicationTLSSecret == "" && current.ClientCASecret != want.ClientCASecret {
		return forbidden("CloudNativePG issues the replication certificate from the CA of the Secret " + current.ClientCASecret), nil
	}
	if tls.Issuer == nil {
		return nil, nil
	}
	certificate, err := uncachedCertificate(c, cnpg.ServerCertificateName(c.Name()))
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	ref := certificate.Spec.IssuerRef
	if ref.Name != tls.Issuer.Name || cmp.Or(ref.Kind, cmv1.IssuerKind) != string(tls.Issuer.Kind) || cmp.Or(ref.Group, cnpg.CertManagerGroup) != tls.Issuer.Group {
		return forbidden(fmt.Sprintf("the server certificate is issued by the %s %s of %s", cmp.Or(ref.Kind, cmv1.IssuerKind), ref.Name, cmp.Or(ref.Group, cnpg.CertManagerGroup))), nil
	}
	return nil, nil
}

func describeServerSecret(name, instance string) string {
	switch name {
	case "":
		return "the CA CloudNativePG created for it"
	case cnpg.ServerCertificateName(instance):
		return "its cert-manager issuer"
	}
	return "the Secret " + name
}

func describeSecret(name string) string {
	if name == "" {
		return "the CA CloudNativePG created for the instance"
	}
	return "the Secret " + name
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
func issuerProblem(c *controller.Context, issuer *definition.IssuerReference) string {
	kind := string(issuer.Kind)
	if issuer.Group != cmv1.SchemeGroupVersion.Group || (kind != cmv1.IssuerKind && kind != cmv1.ClusterIssuerKind) {
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

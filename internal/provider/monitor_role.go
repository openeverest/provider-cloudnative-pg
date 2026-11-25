package provider

import (
	"crypto/rand"
	"fmt"
	"slices"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/certs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

const (
	// monitorRoleName is the read-only role monitoring agents log in with,
	// a member of pg_monitor and nothing else.
	monitorRoleName = "openeverest_monitor"
	// pgMonitorRole is PostgreSQL's built-in monitoring role: every pg_stat_*
	// view, pg_settings and the monitoring functions, no table data.
	pgMonitorRole = "pg_monitor"
	// postgresContainerName and postgresPortName are the PostgreSQL
	// container and port CloudNativePG gives every instance pod.
	postgresContainerName = "postgres"
	postgresPortName      = "postgresql"

	monitorPasswordLength = 32
)

// monitorSecretName is the Instance-owned Secret of the monitor role, with
// the username and password keys the monitoring contract fixes. CloudNativePG
// reads the same keys for a managed role's passwordSecret.
func monitorSecretName(instance string) string {
	return instance + "-monitoring-monitor"
}

// syncMonitorRole keeps the monitor role in line with what bound monitoring
// classes need: while one requires connections it creates the role's Secret
// and renders the role into pg; once none does it renders the role absent
// and deletes the Secret. While the requirement is unresolved, whatever is
// live is held.
func syncMonitorRole(c *controller.Context, pg *cnpgv1.Cluster) error {
	req, err := c.RequiredSources()
	if err != nil {
		return err
	}
	live := &cnpgv1.Cluster{}
	if _, err := c.Exists(live, c.Name()); err != nil {
		return fmt.Errorf("get CloudNativePG cluster: %w", err)
	}
	secret := &corev1.Secret{}
	hasSecret, err := c.Exists(secret, monitorSecretName(c.Name()))
	if err != nil {
		return fmt.Errorf("get monitor Secret: %w", err)
	}
	liveRole := managedRole(live, monitorRoleName)
	livePresent := hasSecret && liveRole != nil && liveRole.Ensure != cnpgv1.EnsureAbsent

	if req.Connections.Else(livePresent) {
		if err := applyMonitorSecret(c, secret, hasSecret); err != nil {
			return err
		}
		appendManagedRole(pg, monitorRole(c.Name(), cnpgv1.EnsurePresent))
		return nil
	}
	if liveRole == nil && !hasSecret {
		return nil
	}
	// Once the role existed it stays declared absent, so CloudNativePG
	// drops it whenever it gets to it; an absent role needs no Secret.
	appendManagedRole(pg, monitorRole(c.Name(), cnpgv1.EnsureAbsent))
	if hasSecret {
		if err := c.Delete(secret); err != nil {
			return fmt.Errorf("delete monitor Secret: %w", err)
		}
	}
	return nil
}

// applyMonitorSecret writes the monitor Secret, keeping the password of an
// existing one.
func applyMonitorSecret(c *controller.Context, live *corev1.Secret, exists bool) error {
	password := ""
	if exists {
		password = string(live.Data[corev1alpha1.CredentialSecretKeyPassword])
	}
	if password == "" {
		password = rand.Text() + rand.Text()
		password = password[:monitorPasswordLength]
	}
	secret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(monitorSecretName(c.Name())),
		Type:       corev1.SecretTypeBasicAuth,
		StringData: map[string]string{
			corev1alpha1.CredentialSecretKeyUsername: monitorRoleName,
			corev1alpha1.CredentialSecretKeyPassword: password,
		},
	}
	// CloudNativePG reloads a managed role's password when its Secret changes.
	secret.Labels["cnpg.io/reload"] = "true"
	if err := c.Apply(secret); err != nil {
		return fmt.Errorf("apply monitor Secret: %w", err)
	}
	return nil
}

func monitorRole(instance string, ensure cnpgv1.EnsureOption) cnpgv1.RoleConfiguration {
	role := cnpgv1.RoleConfiguration{
		Name:    monitorRoleName,
		Comment: "OpenEverest monitoring agents (read-only)",
		Ensure:  ensure,
	}
	if ensure == cnpgv1.EnsurePresent {
		role.Login = true
		role.InRoles = []string{pgMonitorRole}
		role.ConnectionLimit = 5
		role.PasswordSecret = &cnpgv1.LocalObjectReference{Name: monitorSecretName(instance)}
	}
	return role
}

// appendManagedRole adds role to pg's managed roles, replacing one the
// Instance declares under the same name.
func appendManagedRole(pg *cnpgv1.Cluster, role cnpgv1.RoleConfiguration) {
	if pg.Spec.Managed == nil {
		pg.Spec.Managed = &cnpgv1.ManagedConfiguration{}
	} else {
		pg.Spec.Managed = pg.Spec.Managed.DeepCopy()
	}
	pg.Spec.Managed.Roles = slices.DeleteFunc(pg.Spec.Managed.Roles, func(r cnpgv1.RoleConfiguration) bool {
		return r.Name == role.Name
	})
	pg.Spec.Managed.Roles = append(pg.Spec.Managed.Roles, role)
}

func managedRole(pg *cnpgv1.Cluster, name string) *cnpgv1.RoleConfiguration {
	if pg.Spec.Managed == nil {
		return nil
	}
	i := slices.IndexFunc(pg.Spec.Managed.Roles, func(r cnpgv1.RoleConfiguration) bool { return r.Name == name })
	if i < 0 {
		return nil
	}
	return &pg.Spec.Managed.Roles[i]
}

func roleReconciled(pg *cnpgv1.Cluster, name string) bool {
	return slices.Contains(pg.Status.ManagedRolesStatus.ByStatus[cnpgv1.RoleStatusReconciled], name)
}

// monitorConnectionSources returns the monitor credential and the engine's
// connection endpoint once CloudNativePG has created the monitor role.
func monitorConnectionSources(c *controller.Context, pg *cnpgv1.Cluster) ([]corev1alpha1.MonitoringCredential, []corev1alpha1.ConnectionEndpoint, error) {
	role := managedRole(pg, monitorRoleName)
	if role == nil || role.Ensure == cnpgv1.EnsureAbsent || !roleReconciled(pg, monitorRoleName) {
		return nil, nil, nil
	}
	found, err := c.Exists(&corev1.Secret{}, monitorSecretName(c.Name()))
	if err != nil || !found {
		return nil, nil, err
	}
	tls, err := cnpg.TLS(c.Instance())
	if err != nil {
		return nil, nil, err
	}
	credentials := []corev1alpha1.MonitoringCredential{{
		Profile:   corev1alpha1.CredentialProfileMonitor,
		SecretRef: commonv1alpha1.SecretRef{Name: monitorSecretName(c.Name())},
	}}
	connections := []corev1alpha1.ConnectionEndpoint{{
		Component: common.ComponentEngine,
		PodSelector: map[string]string{
			controller.InstanceLabel:  c.Name(),
			controller.ComponentLabel: common.ComponentEngine,
		},
		Container: postgresContainerName,
		Port:      intstr.FromString(postgresPortName),
		// CloudNativePG always serves TLS; the server certificates name the
		// Services, so agents verify the chain only.
		TLS: &corev1alpha1.ConnectionTLS{
			Required: cnpg.RequireTLS(tls),
			CA: &commonv1alpha1.CABundleSource{SecretKeyRef: &commonv1alpha1.SecretKeyRef{
				Name: pg.GetServerCASecretName(),
				Key:  certs.CACertKey,
			}},
		},
		Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: corev1alpha1.CredentialProfileMonitor},
	}}
	return credentials, connections, nil
}

package provider

import (
	"context"
	"os"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// connectionContext is the context of Instance "pg" listing one destination
// whose class requires connections when withEntry is set; objs are the live
// Cluster and Secrets.
func connectionContext(t *testing.T, withEntry bool, parameters string, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, monitoringv1alpha1.AddToScheme, cnpgv1.AddToScheme, corev1.AddToScheme,
	} {
		require.NoError(t, add(scheme))
	}
	data, err := os.ReadFile(providerSpecPath)
	require.NoError(t, err)
	provider := &corev1alpha1.Provider{}
	require.NoError(t, yaml.Unmarshal(data, provider))

	in := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace, UID: "uid"},
		Spec:       corev1alpha1.InstanceSpec{ProviderRef: commonv1alpha1.ObjectRef{Name: provider.Name}},
	}
	if parameters != "" {
		in.Spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	objs = append(objs, in, provider, &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: "releem"},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			AuthModes: []commonv1alpha1.AuthMode{{Name: commonv1alpha1.AuthModeAnonymous}},
			ExtensionManaged: &monitoringv1alpha1.ExtensionManagedFulfilment{
				Requires: monitoringv1alpha1.SourceRequirements{Connection: true},
			},
		},
	})
	if withEntry {
		in.Spec.Monitoring = &corev1alpha1.InstanceMonitoringSpec{Destinations: []corev1alpha1.InstanceMonitoringDestination{{
			Name: "releem", DestinationRef: commonv1alpha1.MonitoringDestinationRef{Name: "releem"},
		}}}
		objs = append(objs, &monitoringv1alpha1.MonitoringBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pg-releem", Namespace: testNamespace,
				Labels: map[string]string{
					monitoringv1alpha1.BindingInstanceLabel: "pg",
					monitoringv1alpha1.BindingEntryLabel:    "releem",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Instance", Name: "pg", UID: "uid", Controller: new(true),
				}},
			},
			Spec: monitoringv1alpha1.MonitoringBindingSpec{
				InstanceRef:    commonv1alpha1.ObjectRef{Name: "pg"},
				DestinationRef: commonv1alpha1.MonitoringDestinationRef{Name: "releem"},
				ClassRef:       &commonv1alpha1.ObjectRef{Name: "releem"},
				ExecutionMode:  corev1alpha1.MonitoringExecutionModeExtensionManaged,
			},
		})
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, in, provider.Name), cl
}

func monitorSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-monitoring-monitor", Namespace: testNamespace},
		Data: map[string][]byte{
			corev1alpha1.CredentialSecretKeyUsername: []byte(monitorRoleName),
			corev1alpha1.CredentialSecretKeyPassword: []byte(password),
		},
	}
}

func clusterWithRole(ensure cnpgv1.EnsureOption, reconciled bool) *cnpgv1.Cluster {
	pg := monitoredCluster(true, 1, 1, false)
	appendManagedRole(pg, monitorRole("pg", ensure))
	if reconciled {
		pg.Status.ManagedRolesStatus.ByStatus = map[cnpgv1.RoleStatus][]string{cnpgv1.RoleStatusReconciled: {monitorRoleName}}
	}
	return pg
}

func TestSyncMonitorRoleCreatesTheRoleWhileAClassRequiresConnections(t *testing.T) {
	c, cl := connectionContext(t, true, "")
	pg := &cnpgv1.Cluster{}
	pg.Spec.Managed = &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{{Name: "app_reader"}}}

	require.NoError(t, syncMonitorRole(c, pg))

	secret := &corev1.Secret{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "pg-monitoring-monitor"}, secret))
	assert.Equal(t, corev1.SecretTypeBasicAuth, secret.Type)
	assert.Equal(t, monitorRoleName, secret.StringData[corev1alpha1.CredentialSecretKeyUsername])
	assert.Len(t, secret.StringData[corev1alpha1.CredentialSecretKeyPassword], monitorPasswordLength)
	assert.True(t, metav1.IsControlledBy(secret, c.Instance()), "the runtime publishes only Instance-owned credentials")

	require.Len(t, pg.Spec.Managed.Roles, 2, "roles the Instance declares are kept")
	role := pg.Spec.Managed.Roles[1]
	assert.Equal(t, monitorRoleName, role.Name)
	assert.Equal(t, cnpgv1.EnsurePresent, role.Ensure)
	assert.True(t, role.Login)
	assert.Equal(t, []string{"pg_monitor", "pg_read_all_data"}, role.InRoles)
	assert.Equal(t, int64(monitorConnectionLimit), role.ConnectionLimit)
	assert.Equal(t, "pg-monitoring-monitor", role.PasswordSecret.Name)
}

func TestSyncMonitorRoleKeepsThePassword(t *testing.T) {
	c, cl := connectionContext(t, true, "", monitorSecret("kept-password"), clusterWithRole(cnpgv1.EnsurePresent, true))

	require.NoError(t, syncMonitorRole(c, &cnpgv1.Cluster{}))

	secret := &corev1.Secret{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "pg-monitoring-monitor"}, secret))
	assert.Equal(t, "kept-password", secret.StringData[corev1alpha1.CredentialSecretKeyPassword])
}

func TestSyncMonitorRoleDropsTheRoleOnceNoClassRequiresIt(t *testing.T) {
	c, cl := connectionContext(t, false, "", monitorSecret("p"), clusterWithRole(cnpgv1.EnsurePresent, true))
	pg := &cnpgv1.Cluster{}

	require.NoError(t, syncMonitorRole(c, pg))

	assert.Equal(t, cnpgv1.EnsureAbsent, managedRole(pg, monitorRoleName).Ensure)
	assert.Nil(t, managedRole(pg, monitorRoleName).PasswordSecret)
	err := cl.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "pg-monitoring-monitor"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "an absent role needs no Secret")

	c, _ = connectionContext(t, false, "", clusterWithRole(cnpgv1.EnsureAbsent, true))
	pg = &cnpgv1.Cluster{}
	require.NoError(t, syncMonitorRole(c, pg))
	assert.Equal(t, cnpgv1.EnsureAbsent, managedRole(pg, monitorRoleName).Ensure, "the role stays declared absent")
}

func TestSyncMonitorRoleWithoutAnyMonitoringLeavesTheClusterAlone(t *testing.T) {
	c, _ := connectionContext(t, false, "")
	pg := &cnpgv1.Cluster{}

	require.NoError(t, syncMonitorRole(c, pg))

	assert.Nil(t, pg.Spec.Managed)
}

func TestSyncMonitorRoleDropsTheRoleARestoreInherits(t *testing.T) {
	c, _ := connectionContext(t, false, "")
	c.Instance().Spec.DataSource = &backupv1alpha1.DataSource{}
	pg := &cnpgv1.Cluster{}

	require.NoError(t, syncMonitorRole(c, pg))

	assert.Equal(t, cnpgv1.EnsureAbsent, managedRole(pg, monitorRoleName).Ensure,
		"the source's role and password must not stay usable")
}

func TestValidateReservedRoles(t *testing.T) {
	ok := &components.CNPGCustomSpec{Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{{Name: "app_reader"}}}}
	require.NoError(t, validateReservedRoles(ok))
	require.NoError(t, validateReservedRoles(&components.CNPGCustomSpec{}))
	reserved := &components.CNPGCustomSpec{Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{{Name: monitorRoleName}}}}
	assert.ErrorContains(t, validateReservedRoles(reserved), "reserved")
}

func TestMonitorConnectionSources(t *testing.T) {
	t.Run("published once the role is reconciled", func(t *testing.T) {
		c, _ := connectionContext(t, true, `{"tls":{"mode":"requireTLS"}}`, monitorSecret("p"))
		pg := clusterWithRole(cnpgv1.EnsurePresent, true)

		credentials, connections, err := monitorConnectionSources(c, pg)
		require.NoError(t, err)

		assert.Equal(t, []corev1alpha1.MonitoringCredential{{
			Profile: corev1alpha1.CredentialProfileMonitor, SecretRef: commonv1alpha1.SecretRef{Name: "pg-monitoring-monitor"},
		}}, credentials)
		require.Len(t, connections, 1)
		ep := connections[0]
		assert.Equal(t, common.ComponentEngine, ep.Component)
		assert.Equal(t, map[string]string{controller.InstanceLabel: "pg", controller.ComponentLabel: common.ComponentEngine}, ep.PodSelector)
		assert.Equal(t, "postgres", ep.Container)
		assert.Equal(t, intstr.FromString("postgresql"), ep.Port)
		assert.True(t, ep.TLS.Required)
		assert.Equal(t, "pg-ca", ep.TLS.CA.SecretKeyRef.Name)
		assert.Equal(t, corev1alpha1.CredentialProfileMonitor, ep.Auth.CredentialProfile)
	})
	t.Run("TLS accepted but not required by default", func(t *testing.T) {
		c, _ := connectionContext(t, true, "", monitorSecret("p"))
		_, connections, err := monitorConnectionSources(c, clusterWithRole(cnpgv1.EnsurePresent, true))
		require.NoError(t, err)
		require.Len(t, connections, 1)
		assert.False(t, connections[0].TLS.Required)
	})
	t.Run("kept while a rotated password is applied", func(t *testing.T) {
		c, _ := connectionContext(t, true, "", monitorSecret("p"))
		pg := clusterWithRole(cnpgv1.EnsurePresent, false)
		pg.Status.ManagedRolesStatus.ByStatus = map[cnpgv1.RoleStatus][]string{cnpgv1.RoleStatusPendingReconciliation: {monitorRoleName}}
		pg.Status.ManagedRolesStatus.PasswordStatus = map[string]cnpgv1.PasswordState{monitorRoleName: {TransactionID: 1}}
		_, connections, err := monitorConnectionSources(c, pg)
		require.NoError(t, err)
		assert.Len(t, connections, 1, "withdrawing the endpoint would delete every agent")
	})
	for name, pg := range map[string]*cnpgv1.Cluster{
		"no role":             monitoredCluster(true, 1, 1, false),
		"role not reconciled": clusterWithRole(cnpgv1.EnsurePresent, false),
		"role being dropped":  clusterWithRole(cnpgv1.EnsureAbsent, true),
	} {
		t.Run("nothing while "+name, func(t *testing.T) {
			c, _ := connectionContext(t, true, "", monitorSecret("p"))
			credentials, connections, err := monitorConnectionSources(c, pg)
			require.NoError(t, err)
			assert.Empty(t, credentials)
			assert.Empty(t, connections)
		})
	}
}

func TestProviderDeclaresEngineConnections(t *testing.T) {
	data, err := os.ReadFile(providerSpecPath)
	require.NoError(t, err)
	var provider corev1alpha1.Provider
	require.NoError(t, yaml.Unmarshal(data, &provider))
	engineType := provider.Spec.Components[common.ComponentEngine].Type
	assert.Equal(t, corev1alpha1.ConnectionSupported, provider.Spec.Monitoring.ComponentTypes[engineType].Connection)
}

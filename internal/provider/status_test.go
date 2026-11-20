package provider

import (
	"context"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

const (
	imageOld = "ghcr.io/cloudnative-pg/postgresql:18.6-standard-trixie"
	imageNew = "ghcr.io/cloudnative-pg/postgresql:18.7-standard-trixie"
)

// healthyCluster is a two-instance cluster CloudNativePG reports healthy,
// changed by mutate.
func healthyCluster(mutate ...func(*cnpgv1.Cluster)) *cnpgv1.Cluster {
	pg := readyCluster(&cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{
		ImageName: imageNew,
		Bootstrap: &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{
			Database: "app", Owner: "app", Secret: &cnpgv1.LocalObjectReference{Name: "pg-app"},
		}},
	}})
	pg.Status.Image = imageNew
	pg.Status.PGDataImageInfo = &cnpgv1.ImageInfo{Image: imageNew, MajorVersion: 18}
	for _, m := range mutate {
		m(pg)
	}
	return pg
}

func phase(phase, reason string) func(*cnpgv1.Cluster) {
	return func(pg *cnpgv1.Cluster) {
		pg.Status.Phase, pg.Status.PhaseReason = phase, reason
		pg.Status.Conditions[0].Status = metav1.ConditionFalse
	}
}

func TestObserveCluster(t *testing.T) {
	t.Parallel()
	type operation struct{ conditionType, reason string }

	tests := []struct {
		name          string
		cluster       *cnpgv1.Cluster
		seeding       string
		wantMessage   string
		wantAvailable controller.Availability
		wantProblem   string
		wantTerminal  bool
		wantOperation operation
	}{
		{
			name:          "healthy",
			cluster:       healthyCluster(),
			wantAvailable: controller.Available,
		},
		{
			name: "not initialized, in CloudNativePG's words",
			cluster: healthyCluster(func(pg *cnpgv1.Cluster) {
				pg.Status = cnpgv1.ClusterStatus{Conditions: []metav1.Condition{{
					Type:    string(cnpgv1.ConditionInitialized),
					Status:  metav1.ConditionFalse,
					Message: "Cluster has not been bootstrapped yet",
				}}}
			}),
			wantMessage:   "Cluster has not been bootstrapped yet",
			wantAvailable: controller.Unavailable,
		},
		{
			name:          "seeding from a data source",
			cluster:       healthyCluster(phase(cnpgv1.PhaseFirstPrimary, "")),
			seeding:       "Restoring the backup of the data source",
			wantMessage:   "Restoring the backup of the data source",
			wantAvailable: controller.Available,
		},
		{
			// CloudNativePG has not seen the new image yet: its status still
			// reads healthy for the previous one.
			name: "image not picked up",
			cluster: healthyCluster(func(pg *cnpgv1.Cluster) {
				pg.Status.Image = imageOld
				pg.Status.PGDataImageInfo.Image = imageOld
			}),
			wantMessage:   "Waiting for CloudNativePG to roll out image " + imageNew,
			wantAvailable: controller.Available,
			wantOperation: operation{corev1alpha1.ConditionUpgrading, reasonRollingOutImage},
		},
		{
			name: "instances restarted for a new image",
			cluster: healthyCluster(phase(cnpgv1.PhaseUpgrade,
				"Restarting instance pg-2, because: the instance is using a different image: "+imageOld+" -> "+imageNew)),
			wantMessage: "Waiting for CloudNativePG: Upgrading cluster: Restarting instance pg-2, because: " +
				"the instance is using a different image: " + imageOld + " -> " + imageNew,
			wantAvailable: controller.Available,
			wantOperation: operation{corev1alpha1.ConditionUpgrading, reasonRollingOutImage},
		},
		{
			// CloudNativePG uses the same phase for every rolling restart.
			name:          "instances restarted for another change is no upgrade",
			cluster:       healthyCluster(phase(cnpgv1.PhaseUpgrade, "Restarting instance pg-2, because: resources changed")),
			wantMessage:   "Waiting for CloudNativePG: Upgrading cluster: Restarting instance pg-2, because: resources changed",
			wantAvailable: controller.Available,
		},
		{
			name: "major version upgrade",
			cluster: healthyCluster(phase(cnpgv1.PhaseMajorUpgrade, ""), func(pg *cnpgv1.Cluster) {
				pg.Status.PGDataImageInfo = &cnpgv1.ImageInfo{Image: "ghcr.io/cloudnative-pg/postgresql:17.9-standard-trixie", MajorVersion: 17}
			}),
			wantMessage:   "Waiting for CloudNativePG: " + cnpgv1.PhaseMajorUpgrade,
			wantAvailable: controller.Available,
			wantOperation: operation{corev1alpha1.ConditionUpgrading, reasonUpgradingMajorVersion},
		},
		{
			name:          "scaling out",
			cluster:       healthyCluster(func(pg *cnpgv1.Cluster) { pg.Spec.Instances = 3 }),
			wantMessage:   "Waiting for CloudNativePG to scale to 3 instances (it has 2)",
			wantAvailable: controller.Available,
		},
		{
			name: "an instance is not ready",
			cluster: healthyCluster(phase(cnpgv1.PhaseWaitingForInstancesToBeActive, ""), func(pg *cnpgv1.Cluster) {
				pg.Status.ReadyInstances = 1
				pg.Status.InstancesStatus = map[cnpgv1.PodStatus][]string{cnpgv1.PodHealthy: {"pg-1"}, cnpgv1.PodReplicating: {"pg-2"}}
			}),
			wantMessage:   "Waiting for the instances to become ready (1 of 2 ready)",
			wantAvailable: controller.Available,
		},
		{
			name:          "volumes being resized",
			cluster:       healthyCluster(func(pg *cnpgv1.Cluster) { pg.Status.ResizingPVC = []string{"pg-1", "pg-2"} }),
			wantMessage:   "Waiting for volumes to be resized: pg-1, pg-2",
			wantAvailable: controller.Available,
			wantOperation: operation{corev1alpha1.ConditionStorageResizing, reasonResizingVolumes},
		},
		{
			name: "hibernated outside OpenEverest",
			cluster: healthyCluster(func(pg *cnpgv1.Cluster) {
				pg.Annotations = map[string]string{utils.HibernationAnnotationName: string(utils.HibernationAnnotationValueOn)}
			}),
			wantMessage:   "The cluster is hibernated on its CloudNativePG Cluster object",
			wantAvailable: controller.Unavailable,
			wantProblem:   reasonHibernatedOutsideOpenEverest,
		},
		{
			name:          "unrecoverable is terminal",
			cluster:       healthyCluster(phase(cnpgv1.PhaseUnrecoverable, "")),
			wantMessage:   "CloudNativePG reports: " + cnpgv1.PhaseUnrecoverable,
			wantAvailable: controller.Available,
			wantProblem:   reasonUnrecoverable,
			wantTerminal:  true,
		},
		{
			// CloudNativePG reports reconcile-time validation failures this way.
			name:          "definition rejected",
			cluster:       healthyCluster(phase(cnpgv1.PhaseDefinitionInvalid, "synchronous.number must be lower than the number of instances")),
			wantMessage:   "CloudNativePG reports: " + cnpgv1.PhaseDefinitionInvalid + ": synchronous.number must be lower than the number of instances",
			wantAvailable: controller.Available,
			wantProblem:   reasonConfigurationRejected,
		},
		{
			name:          "image catalog error",
			cluster:       healthyCluster(phase(cnpgv1.PhaseImageCatalogError, "missing")),
			wantMessage:   "CloudNativePG reports: " + cnpgv1.PhaseImageCatalogError + ": missing",
			wantAvailable: controller.Available,
			wantProblem:   reasonImageCatalogInvalid,
		},
		{
			name:          "plugin failure",
			cluster:       healthyCluster(phase(cnpgv1.PhaseFailurePlugin, "barman-cloud.cloudnative-pg.io")),
			wantMessage:   "CloudNativePG reports: " + cnpgv1.PhaseFailurePlugin + ": barman-cloud.cloudnative-pg.io",
			wantAvailable: controller.Available,
			wantProblem:   reasonPluginFailed,
		},
		{
			name:          "a phase the provider gives no reason of its own",
			cluster:       healthyCluster(phase(cnpgv1.PhaseArchitectureBinaryMissing, "")),
			wantMessage:   "CloudNativePG reports: " + cnpgv1.PhaseArchitectureBinaryMissing,
			wantAvailable: controller.Available,
			wantProblem:   corev1alpha1.ReasonEngineError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeCluster(tt.cluster, tt.seeding)

			assert.Equal(t, tt.wantMessage, got.Message)
			assert.Equal(t, tt.wantAvailable, got.Available, "available: %s %s", got.AvailableReason, got.AvailableMessage)

			if tt.wantProblem == "" {
				assert.Empty(t, got.Problems)
			} else if assert.Len(t, got.Problems, 1) {
				assert.Equal(t, tt.wantProblem, got.Problems[0].Reason)
				assert.Equal(t, tt.wantTerminal, got.Problems[0].Terminal)
				assert.NotEmpty(t, got.Problems[0].Message)
			}

			if tt.wantOperation == (operation{}) {
				assert.Empty(t, got.Operations)
			} else if assert.Len(t, got.Operations, 1) {
				assert.Equal(t, tt.wantOperation, operation{got.Operations[0].Type, got.Operations[0].Reason})
			}
		})
	}
}

func TestAvailability(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		cluster    *cnpgv1.Cluster
		want       controller.Availability
		wantReason string
	}{
		{name: "primary ready", cluster: healthyCluster(), want: controller.Available},
		{
			// A rolling restart of the replicas leaves the primary serving.
			name: "replica restarting",
			cluster: healthyCluster(func(pg *cnpgv1.Cluster) {
				pg.Status.InstancesStatus = map[cnpgv1.PodStatus][]string{cnpgv1.PodHealthy: {"pg-1"}}
			}),
			want: controller.Available,
		},
		{
			name:       "no primary yet",
			cluster:    healthyCluster(func(pg *cnpgv1.Cluster) { pg.Status.CurrentPrimary, pg.Status.TargetPrimary = "", "" }),
			want:       controller.Unavailable,
			wantReason: reasonNoPrimary,
		},
		{
			name:       "failover pending",
			cluster:    healthyCluster(func(pg *cnpgv1.Cluster) { pg.Status.TargetPrimary = cnpgv1.PendingFailoverMarker }),
			want:       controller.Unavailable,
			wantReason: reasonNoPrimary,
		},
		{
			name:       "switchover",
			cluster:    healthyCluster(func(pg *cnpgv1.Cluster) { pg.Status.TargetPrimary = "pg-2" }),
			want:       controller.Unavailable,
			wantReason: reasonSwitchingPrimary,
		},
		{
			name: "primary not ready",
			cluster: healthyCluster(func(pg *cnpgv1.Cluster) {
				pg.Status.InstancesStatus = map[cnpgv1.PodStatus][]string{cnpgv1.PodHealthy: {"pg-2"}, cnpgv1.PodFailed: {"pg-1"}}
			}),
			want:       controller.Unavailable,
			wantReason: reasonPrimaryNotReady,
		},
		{
			// Ready pods do not make a primary: without the instance states
			// the answer is unknown, not guessed.
			name:       "instances not reported",
			cluster:    healthyCluster(func(pg *cnpgv1.Cluster) { pg.Status.InstancesStatus = nil }),
			want:       controller.AvailabilityUnknown,
			wantReason: reasonInstancesNotReported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason, message := availability(tt.cluster)
			assert.Equal(t, tt.want, got, message)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestStatusWaitsForTheClusterObject(t *testing.T) {
	t.Parallel()
	got, err := statusOf(t, engineInstance(3))
	require.NoError(t, err)

	assert.False(t, got.Converged)
	assert.Equal(t, "Waiting for the CloudNativePG Cluster object to be created", got.Message)
	assert.Equal(t, []controller.ComponentReplicas{{Component: common.ComponentEngine, Desired: 3}}, got.DesiredReplicas)
}

func TestStatusConverged(t *testing.T) {
	t.Parallel()
	got, err := statusOf(t, engineInstance(2), healthyCluster(), credentialSecret("pg-app", "app", "secret", "5432"))
	require.NoError(t, err)

	assert.True(t, got.Converged, got.Message)
	assert.Equal(t, controller.Available, got.Available)
	assert.Empty(t, got.Problems)
	assert.Empty(t, got.Operations)
	assert.Equal(t, "pg-rw.ns.svc", got.ConnectionDetails.Host)
}

// Clients can connect while a change rolls out, so the connection details
// stay published although the cluster has not converged.
func TestStatusPublishesConnectionDetailsWhileRollingOut(t *testing.T) {
	t.Parallel()
	rolling := healthyCluster(phase(cnpgv1.PhaseUpgrade, "Restarting instance pg-2, because: resources changed"))
	got, err := statusOf(t, engineInstance(2), rolling, credentialSecret("pg-app", "app", "secret", "5432"))
	require.NoError(t, err)

	assert.False(t, got.Converged)
	assert.Equal(t, controller.Available, got.Available)
	assert.Equal(t, "pg-rw.ns.svc", got.ConnectionDetails.Host)
}

func TestStatusReportsManagedRoleFailure(t *testing.T) {
	t.Parallel()
	pg := healthyCluster(func(pg *cnpgv1.Cluster) {
		pg.Spec.Managed = &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{{Name: "reporting"}}}
		pg.Status.ManagedRolesStatus.CannotReconcile = map[string][]string{"reporting": {"role owns objects"}}
	})
	got, err := statusOf(t, engineInstance(2), pg, credentialSecret("pg-app", "app", "secret", "5432"))
	require.NoError(t, err)

	assert.False(t, got.Converged)
	require.Len(t, got.Problems, 1)
	assert.Equal(t, reasonManagedRoleFailed, got.Problems[0].Reason)
	assert.False(t, got.Problems[0].Terminal)
	assert.Equal(t, got.Problems[0].Message, got.Message)
}

func TestWithDesiredReplicas(t *testing.T) {
	t.Parallel()
	instance := engineInstance(3)
	instance.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{
		Replicas:   ptr.To(int32(2)),
		Parameters: &runtime.RawExtension{Raw: []byte(`{"readOnly":true}`)},
	}
	c := controller.NewContext(context.Background(), fake.NewClientBuilder().Build(), instance, common.ProviderName)

	got := withDesiredReplicas(c, controller.Status{})

	// The read-only Pooler runs as many PgBouncer pods as the read-write one.
	assert.ElementsMatch(t, []controller.ComponentReplicas{
		{Component: common.ComponentEngine, Desired: 3},
		{Component: common.ComponentProxy, Desired: 4},
	}, got.DesiredReplicas)
}

func engineInstance(replicas int32) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace},
		Spec: corev1alpha1.InstanceSpec{Components: map[string]corev1alpha1.ComponentSpec{
			common.ComponentEngine: {Replicas: ptr.To(replicas)},
		}},
	}
}

func statusOf(t *testing.T, instance *corev1alpha1.Instance, objects ...client.Object) (controller.Status, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{cnpgv1.AddToScheme, corev1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)
	return (&Provider{}).Status(c)
}

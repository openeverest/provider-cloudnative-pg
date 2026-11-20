package provider

import (
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	barmancloudv1 "github.com/cloudnative-pg/plugin-barman-cloud/api/v1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/backupclasses"
	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	barman "github.com/openeverest/provider-cloudnative-pg/internal/cnpg/barman"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// Compile-time check that Provider implements the required interface.
var _ controller.ProviderInterface = (*Provider)(nil)

// Provider implements controller.ProviderInterface for the provider-cloudnative-pg provider.
type Provider struct {
	controller.BaseProvider

	// extensionCatalog is the ClusterImageCatalog with the extension images.
	extensionCatalog string
}

// New creates a new Provider instance.
func New() *Provider {
	extensionCatalog := os.Getenv("EXTENSION_CATALOG")
	if extensionCatalog == "" {
		extensionCatalog = common.DefaultExtensionCatalog
	}
	p := &Provider{
		extensionCatalog: extensionCatalog,
		BaseProvider: controller.BaseProvider{
			ProviderName: common.ProviderName,
			WatchConfigs: []controller.WatchConfig{
				controller.WatchOwned(&cnpgv1.Cluster{}),
				controller.WatchOwned(&cnpgv1.Database{}),
				controller.WatchOwned(&cnpgv1.Pooler{}),
				controller.WatchExternal(&appsv1.Deployment{},
					handler.EnqueueRequestsFromMapFunc(instanceOfPoolerDeployment)),
				// Connection details publish the LoadBalancer address or NodePort
				// once the managed exposure Service is assigned.
				controller.WatchExternal(&corev1.Service{},
					handler.EnqueueRequestsFromMapFunc(instanceOfManagedService)),
				controller.WatchOwned(&barmancloudv1.ObjectStore{}),
				controller.WatchOwned(&cnpgv1.ScheduledBackup{}),
			},
			SchemeFuncs: []func(*runtime.Scheme) error{
				cnpgv1.SchemeBuilder.AddToScheme,
				addBarmanCloudScheme,
				batchv1.AddToScheme,
				appsv1.AddToScheme,
			},
		},
	}
	p.SetShippedBackupClasses(backupclasses.Shipped())
	return p
}

func addBarmanCloudScheme(scheme *runtime.Scheme) error {
	barmancloudv1.AddKnownTypes(scheme)
	return nil
}

// Sync ensures all required resources exist and are configured correctly.
func (p *Provider) Sync(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Syncing instance", "name", c.Name())
	dataSource, err := c.ReconcileDataSource()
	if err != nil {
		return fmt.Errorf("reconcile data source: %w", err)
	}

	engine := c.Instance().Spec.Components[common.ComponentEngine]

	var custom components.CNPGCustomSpec
	if c.TryDecodeComponentParameters(engine, &custom) {
		if err := c.DecodeComponentParameters(engine, &custom); err != nil {
			return fmt.Errorf("failed to decode component custom spec: %w", err)
		}
	}
	if err := cnpg.DefaultPgStatStatements(c, &custom); err != nil {
		return err
	}

	pg := &cnpgv1.Cluster{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       buildClusterSpec(engine, custom),
	}

	if custom.PostgresConfiguration != nil {
		pg.Spec.PostgresConfiguration = *custom.PostgresConfiguration
	}
	if err := cnpg.ApplyPgHBA(pg, &custom); err != nil {
		return err
	}
	cnpg.ApplyRequireTLS(pg, &custom)
	cnpg.ApplyReplicationSlots(pg, &custom, engine.Version, c.Instance().Spec.Version)

	if custom.Managed != nil {
		pg.Spec.Managed = custom.Managed
	}
	applyService(pg, engine.Service)

	if custom.Affinity != nil {
		pg.Spec.Affinity = *custom.Affinity
	}
	applyScheduling(pg, engine.SchedulingPolicy)

	if custom.Bootstrap != nil {
		pg.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{
			InitDB: custom.Bootstrap.InitDB,
		}
	}

	var providerSpec *corev1alpha1.ProviderSpec
	if engine.Image == "" {
		providerSpec, err = c.ProviderSpec()
		if err != nil {
			return err
		}
	}
	pg.Spec.ImageName = resolveEngineImage(engine, providerSpec)

	if pg.Spec.ImagePullPolicy == "" {
		pg.Spec.ImagePullPolicy = corev1.PullIfNotPresent
	}

	if custom.Certificates != nil {
		pg.Spec.Certificates = custom.Certificates
	}

	if custom.Monitoring != nil {
		pg.Spec.Monitoring = custom.Monitoring
	}

	// CNPG has no pod template; inheritedMetadata reaches the instance pods
	// (not the bootstrap job pods), so the runtime counts them in status.components.
	pg.Spec.InheritedMetadata = &cnpgv1.EmbeddedObjectMetadata{
		Labels: c.PodLabels(common.ComponentEngine),
	}

	extensionImages, err := cnpg.ResolveExtensionImages(c, p.extensionCatalog, &custom, engine.Version, c.Instance().Spec.Version)
	if err != nil {
		return err
	}
	cnpg.ApplyExtensions(pg, &custom, extensionImages)
	cnpg.ApplyQueryStatisticsMonitoring(pg, &custom)
	disableOperatorPodMonitor(pg)

	if c.Instance().Spec.DataSource != nil {
		recovery, externalCluster, err := barman.BuildRecoveryConfig(c, custom)
		if err != nil {
			return err
		}

		pg.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{
			Recovery: recovery,
		}

		pg.Spec.ExternalClusters = []cnpgv1.ExternalCluster{externalCluster}
	}

	plugins, err := barman.SyncBackupInfrastructure(c)
	if err != nil {
		return err
	}
	if len(plugins) > 0 {
		pg.Spec.Plugins = plugins
	}

	if err := cnpg.ApplyQueryStatisticsConfigMap(c, &custom); err != nil {
		return err
	}

	if err := c.Apply(pg); err != nil {
		return err
	}

	if err := cnpg.DeleteQueryStatisticsConfigMap(c, &custom); err != nil {
		return err
	}

	// Releases before the monitoring contract created a PodMonitor.
	if err := deleteLegacyPodMonitor(c); err != nil {
		return err
	}

	if err := syncPooler(c, custom.RequireTLS != nil && *custom.RequireTLS); err != nil {
		return err
	}

	// The application database and its backups only exist once a restore is done.
	if !dataSource.Done {
		return nil
	}

	if err := cnpg.SyncExtensionsDatabase(c, &custom, extensionImages); err != nil {
		return err
	}

	return barman.SyncScheduledBackups(c)
}

// Cleanup handles deletion of provider-managed resources.
//
// Resources created via c.Apply() carry owner references and are garbage
// collected with the Instance. The PodMonitor of earlier releases is deleted
// explicitly, in case it never got another Sync after the upgrade.
func (p *Provider) Cleanup(c *controller.Context) error {
	l := log.FromContext(c.Context())
	l.Info("Cleaning up instance", "name", c.Name())
	return deleteLegacyPodMonitor(c)
}

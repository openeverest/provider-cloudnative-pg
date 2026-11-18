// Package barman holds shared constants for the CNPG-I Barman Cloud Plugin
// backup path. See https://cloudnative-pg.github.io/plugin-barman-cloud/.
package barman

const (
	// KindBackup is the kind of the backup resource.
	KindBackup = "Backup"

	// PluginName is the CNPG-I plugin identifier referenced on Cluster.spec.plugins,
	// Backup.spec.pluginConfiguration, and ScheduledBackup.spec.pluginConfiguration.
	PluginName = "barman-cloud.cloudnative-pg.io"

	// PluginParameterObjectStore is the Cluster plugin parameter that names the
	// barmancloud.cnpg.io ObjectStore resource in the same namespace.
	PluginParameterObjectStore = "barmanObjectName"

	// BackupClassName is the BackupClass the provider ships, defined in
	// definition/backupclasses.
	BackupClassName = "cnpg-barman-plugin"

	// StorageLabel names the BackupStorage an archive ObjectStore of an
	// Instance renders.
	StorageLabel = "backup.openeverest.io/storage"

	// RestoreSourceRole is the ObjectStoreName role of the ObjectStore an
	// Instance bootstraps from.
	RestoreSourceRole = "restore-source"

	// BackupDeletionImage contains the Barman Cloud CLI version pinned by this
	// provider's plugin-barman-cloud dependency.
	BackupDeletionImage = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.15.0"

	// ReasonBarmanPluginMissing is published on BackupConfigured / Backup status
	// when the ObjectStore CRD (Barman Cloud Plugin) is absent from the cluster.
	ReasonBarmanPluginMissing = "BarmanPluginMissing"
)

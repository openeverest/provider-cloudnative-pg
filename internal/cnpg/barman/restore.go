package barman

import (
	"fmt"
	"time"

	"github.com/AlekSi/pointer"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/provider-cloudnative-pg/definition/components"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const recoveryExternalClusterName = "origin"

func ValidateDataSource(ds *backupv1alpha1.DataSource) error {
	if ds == nil {
		return nil
	}

	switch ds.Type {
	case backupv1alpha1.DataSourceTypeBackup:
		if ds.Backup == nil || ds.Backup.BackupRef.Name == "" {
			return fmt.Errorf("spec.dataSource.backup.backupRef.name is required")
		}
	case backupv1alpha1.DataSourceTypePointInTime:
		if ds.PointInTime == nil || ds.PointInTime.Source.StorageRef.Name == "" {
			return fmt.Errorf("spec.dataSource.pointInTime.source.storageRef.name is required")
		}
		if ds.PointInTime.RecoveryTarget == backupv1alpha1.RecoveryTargetDate && ds.PointInTime.Date == nil {
			return fmt.Errorf("spec.dataSource.pointInTime.date is required when recoveryTarget is date")
		}
	default:
		return fmt.Errorf("unsupported dataSource.type %q", ds.Type)
	}

	return nil
}

func BuildRecoveryConfig(c *controller.Context, custom components.CNPGCustomSpec) (*cnpgv1.BootstrapRecovery, cnpgv1.ExternalCluster, error) {
	ds := c.Instance().Spec.DataSource

	var storageName, location, serverName string
	var recoveryTarget *cnpgv1.RecoveryTarget

	if ds.Type == backupv1alpha1.DataSourceTypePointInTime {
		pitr := ds.PointInTime
		storageName = pitr.Source.StorageRef.Name
		serverName = c.Instance().Name
		if ref := pitr.Source.InstanceRef; ref != nil && ref.Name != "" {
			serverName = ref.Name
		}
		var err error
		if location, err = sourceInstanceLocation(c, serverName, storageName); err != nil {
			return nil, cnpgv1.ExternalCluster{}, err
		}
		if pitr.RecoveryTarget == backupv1alpha1.RecoveryTargetDate {
			recoveryTarget = &cnpgv1.RecoveryTarget{
				TargetTime: pitr.Date.UTC().Format(time.RFC3339),
			}
		}
	} else {
		cnpgBackup, oeBackup, err := resolveSourceBackup(c, ds)
		if err != nil {
			return nil, cnpgv1.ExternalCluster{}, err
		}
		storageName = oeBackup.Spec.StorageRef.Name
		if location, err = sourceBackupLocation(c, oeBackup); err != nil {
			return nil, cnpgv1.ExternalCluster{}, err
		}

		serverName = cnpgBackup.Status.ServerName
		if serverName == "" {
			serverName = cnpgBackup.Spec.Cluster.Name
		}
		if serverName == "" {
			return nil, cnpgv1.ExternalCluster{}, fmt.Errorf(
				"CloudNativePG Backup %q has no server name (status.serverName / spec.cluster.name)",
				cnpgBackup.Name)
		}

		recoveryTarget = &cnpgv1.RecoveryTarget{
			BackupID:        cnpgBackup.Status.BackupID,
			TargetImmediate: pointer.To(true),
		}
	}

	recovery := &cnpgv1.BootstrapRecovery{
		Source:         recoveryExternalClusterName,
		RecoveryTarget: recoveryTarget,
	}

	if custom.Bootstrap != nil && custom.Bootstrap.InitDB != nil {
		initdb := custom.Bootstrap.InitDB
		recovery.Database = initdb.Database
		recovery.Owner = initdb.Owner
		recovery.Secret = initdb.Secret
	}

	objectStoreName, err := syncRestoreSource(c, storageName, location)
	if err != nil {
		return nil, cnpgv1.ExternalCluster{}, err
	}

	externalCluster := cnpgv1.ExternalCluster{
		Name: recoveryExternalClusterName,
		PluginConfiguration: &cnpgv1.PluginConfiguration{
			Name: PluginName,
			Parameters: map[string]string{
				PluginParameterObjectStore: objectStoreName,
				"serverName":               serverName,
			},
		},
	}

	return recovery, externalCluster, nil
}

func resolveSourceBackup(c *controller.Context, ds *backupv1alpha1.DataSource) (*cnpgv1.Backup, *backupv1alpha1.Backup, error) {
	backupName := ds.Backup.BackupRef.Name

	oeBackup := &backupv1alpha1.Backup{}
	if err := c.Get(oeBackup, backupName); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, controller.WaitFor(fmt.Sprintf(
				"source Backup %q not yet present", backupName))
		}
		return nil, nil, &controller.BackupConfigError{
			Reason:  "BackupResolutionFailed",
			Message: err.Error(),
		}
	}

	if oeBackup.Status.State != backupv1alpha1.BackupStateSucceeded {
		return nil, nil, controller.WaitFor(fmt.Sprintf(
			"source Backup %q is in state %q, waiting for Succeeded",
			backupName, oeBackup.Status.State))
	}

	if oeBackup.Spec.StorageRef.Name == "" {
		return nil, nil, &controller.BackupConfigError{
			Reason:  "StorageRefMissing",
			Message: fmt.Sprintf("source Backup %q has no storageRef.name", backupName),
		}
	}

	cnpgBackupName := backupName
	if ref := oeBackup.Status.OperatorBackupRef; ref != nil && ref.Name != "" {
		cnpgBackupName = ref.Name
	}

	cnpgBackup := &cnpgv1.Backup{}
	if err := c.Get(cnpgBackup, cnpgBackupName); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, controller.WaitFor(fmt.Sprintf(
				"CloudNativePG Backup %q not yet present", cnpgBackupName))
		}
		return nil, nil, fmt.Errorf("get CloudNativePG Backup %q: %w", cnpgBackupName, err)
	}

	if cnpgBackup.Status.Phase != cnpgv1.BackupPhaseCompleted {
		return nil, nil, controller.WaitFor(fmt.Sprintf(
			"CloudNativePG Backup %q is not completed (phase=%q)",
			cnpgBackupName, cnpgBackup.Status.Phase))
	}

	if cnpgBackup.Status.BackupID == "" {
		return nil, nil, fmt.Errorf("CloudNativePG Backup %q has no Barman backup ID", cnpgBackup.Name)
	}

	return cnpgBackup, oeBackup, nil
}

// sourceBackupLocation returns where the source Backup's data lives in its
// storage.
func sourceBackupLocation(c *controller.Context, backup *backupv1alpha1.Backup) (string, error) {
	bs, err := c.BackupStorage(backup.Spec.StorageRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", controller.WaitFor(fmt.Sprintf(
				"BackupStorage %q of source Backup %q not yet present", backup.Spec.StorageRef.Name, backup.Name))
		}
		return "", fmt.Errorf("get BackupStorage %q: %w", backup.Spec.StorageRef.Name, err)
	}
	location := c.BackupLocation(backup, bs)
	if location == "" {
		return "", &controller.BackupConfigError{
			Reason:  "BackupLocationUnknown",
			Message: fmt.Sprintf("source Backup %q has no recorded path in storage %q", backup.Name, bs.Name),
		}
	}
	return location, nil
}

// sourceInstanceLocation returns where the Instance named instance writes
// the backups and WALs of its storage entry storage, as the runtime records
// it in its status.
func sourceInstanceLocation(c *controller.Context, instance, storage string) (string, error) {
	src := c.Instance()
	if instance != src.Name {
		src = &corev1alpha1.Instance{}
		if err := c.Get(src, instance); err != nil {
			if apierrors.IsNotFound(err) {
				return "", controller.WaitFor(fmt.Sprintf("source Instance %q not yet present", instance))
			}
			return "", fmt.Errorf("get source Instance %q: %w", instance, err)
		}
	}
	if src.Status.Backup != nil {
		for _, s := range src.Status.Backup.Storages {
			if s.Name == storage && s.Location != "" {
				return s.Location, nil
			}
		}
	}
	return "", controller.WaitFor(fmt.Sprintf(
		"source Instance %q has no recorded location in storage %q", instance, storage))
}

// syncRestoreSource renders the ObjectStore the Instance bootstraps from:
// location in the BackupStorage named storage.
func syncRestoreSource(c *controller.Context, storage, location string) (string, error) {
	if err := EnsureObjectStoreAvailable(c); err != nil {
		return "", err
	}
	bs, err := c.BackupStorage(storage)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", controller.WaitFor(fmt.Sprintf("BackupStorage %q not yet present", storage))
		}
		return "", fmt.Errorf("get BackupStorage %q: %w", storage, err)
	}
	name := ObjectStoreName(c.Name(), RestoreSourceRole)
	store, err := BuildObjectStore(c, name, bs, location)
	if err != nil {
		if apiStatus(err) != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("render BackupStorage %q: %w", storage, err)
		}
		return "", &controller.BackupConfigError{
			Reason:  "RestoreSourceUnavailable",
			Message: fmt.Sprintf("backup storage %s cannot be read: %v", storage, err),
		}
	}
	// Recovery reads only, so the source keeps no WAL settings.
	store.Spec.Configuration.Wal = nil
	if err := c.Apply(store); err != nil {
		if bce := ObjectStoreCRDMissing(err); bce != nil {
			return "", bce
		}
		return "", fmt.Errorf("apply ObjectStore %q: %w", name, err)
	}
	return name, nil
}

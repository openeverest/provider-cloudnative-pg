package cnpg

import (
	"fmt"
	"strconv"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

// Destructive starts the detail of every error and warning about a change
// that loses data or cannot be undone, so tooling can tell them from invalid
// values: a rejected one is a Forbidden error, an admitted one a warning.
const Destructive = "destructive change: "

// minMajorUpgradeTarget is the oldest major CloudNativePG upgrades to.
const minMajorUpgradeTarget = 16

// ValidateStorageChanges checks an update of the data and WAL volumes against
// the stored Instance: volumes cannot shrink, the WAL volume cannot be
// removed, and neither storage class can change, as CloudNativePG would apply
// a new one only to volumes it creates later. enginePath is the engine
// component, paramsPath its parameters.
func ValidateStorageChanges(
	enginePath, paramsPath *field.Path,
	old, updated corev1alpha1.ComponentSpec,
	oldCustom, custom *components.CNPGCustomSpec,
) field.ErrorList {
	var errs field.ErrorList
	if old.Storage != nil && updated.Storage != nil {
		storagePath := enginePath.Child("storage")
		if updated.Storage.Size.Sign() > 0 && updated.Storage.Size.Cmp(old.Storage.Size) < 0 {
			errs = append(errs, shrink(storagePath.Child("size"), old.Storage.Size.String(), updated.Storage.Size.String()))
		}
		if deref(old.Storage.StorageClass) != deref(updated.Storage.StorageClass) {
			errs = append(errs, classChange(storagePath.Child("storageClass")))
		}
	}

	if oldCustom.WALStorage == nil {
		return errs
	}
	walPath := paramsPath.Child("walStorage")
	if custom.WALStorage == nil {
		return append(errs, field.Forbidden(walPath, Destructive+"cannot be removed once configured"))
	}
	oldSize, size := oldCustom.WALStorage.GetSizeOrNil(), custom.WALStorage.GetSizeOrNil()
	if oldSize != nil && size != nil && size.Sign() > 0 && size.Cmp(*oldSize) < 0 {
		errs = append(errs, shrink(walPath.Child("size"), oldSize.String(), size.String()))
	}
	if storageClass(oldCustom.WALStorage) != storageClass(custom.WALStorage) {
		errs = append(errs, classChange(walPath.Child("storageClass")))
	}
	return errs
}

// ValidateMajorVersionChange checks a change of the PostgreSQL release, as
// "<major>.<minor>", at path. A downgrade of the major, and an upgrade to a
// major CloudNativePG cannot upgrade to, are errors. An upgrade is admitted
// with a warning: it cannot be undone.
func ValidateMajorVersionChange(path *field.Path, oldVersion, version string) (field.ErrorList, []string) {
	oldMajor, okOld := major(oldVersion)
	newMajor, ok := major(version)
	if !okOld || !ok || oldMajor == newMajor {
		return nil, nil
	}
	if newMajor < oldMajor {
		return field.ErrorList{field.Forbidden(path,
			fmt.Sprintf("cannot downgrade PostgreSQL from major %d to %d", oldMajor, newMajor))}, nil
	}
	if newMajor < minMajorUpgradeTarget {
		return field.ErrorList{field.Forbidden(path,
			fmt.Sprintf("CloudNativePG upgrades to PostgreSQL %d or later only, not to %d", minMajorUpgradeTarget, newMajor))}, nil
	}
	return nil, []string{fmt.Sprintf("%s: %supgrading PostgreSQL from major %d to %d cannot be undone: "+
		"CloudNativePG stops every instance, runs pg_upgrade on the primary and recreates the replicas",
		path, Destructive, oldMajor, newMajor)}
}

// BackupChangeWarnings warns when an update turns backups off.
func BackupChangeWarnings(old, updated *corev1alpha1.InstanceBackupSpec) []string {
	if old == nil || !old.Enabled || (updated != nil && updated.Enabled) {
		return nil
	}
	return []string{field.NewPath("spec", "backup", "enabled").String() + ": " + Destructive +
		"turning backups off stops WAL archiving and the scheduled backups; " +
		"the instance cannot be recovered to a point in time while they are off"}
}

func shrink(path *field.Path, from, to string) *field.Error {
	return field.Forbidden(path, fmt.Sprintf("%scannot shrink from %s to %s; volumes can only grow", Destructive, from, to))
}

func classChange(path *field.Path) *field.Error {
	return field.Forbidden(path, "cannot be changed once the volumes exist: CloudNativePG would apply it only to volumes it creates later")
}

func storageClass(storage *cnpgv1.StorageConfiguration) string {
	if storage.StorageClass != nil && *storage.StorageClass != "" {
		return *storage.StorageClass
	}
	if storage.PersistentVolumeClaimTemplate != nil {
		return deref(storage.PersistentVolumeClaimTemplate.StorageClassName)
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func major(version string) (int, bool) {
	m, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(m)
	return n, err == nil
}

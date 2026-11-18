package barman

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AlekSi/pointer"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
	barmancloudv1 "github.com/cloudnative-pg/plugin-barman-cloud/api/v1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/provider-cloudnative-pg/definition/backupclasses"
	cnpgbarmanplugin "github.com/openeverest/provider-cloudnative-pg/definition/backupclasses/cnpg-barman-plugin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// SyncBackupInfrastructure renders an ObjectStore for every backup storage of
// the Instance and returns the Cluster plugin configuration archiving to the
// first one. Each ObjectStore belongs to the Instance and writes under the
// Instance's own path in its storage, so Instances sharing a bucket never
// share an archive. An entry that must not or cannot be rendered keeps the
// ObjectStore it already has (hold rule), and gets none while it has none.
func SyncBackupInfrastructure(c *controller.Context) ([]cnpgv1.PluginConfiguration, error) {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled || len(backupCfg.Storages) == 0 {
		return nil, deleteStaleObjectStores(c, nil)
	}

	if err := EnsureObjectStoreAvailable(c); err != nil {
		return nil, err
	}

	resolved, err := c.BackupStorages()
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(resolved))
	for _, s := range resolved {
		keep[s.Name()] = true
		if !s.Apply() {
			continue
		}
		retention, err := storageRetentionPolicy(*s.Entry)
		if err != nil {
			return nil, &controller.BackupConfigError{Reason: "InvalidBackupRetention", Message: err.Error()}
		}
		objStore, err := BuildObjectStore(c, ObjectStoreName(c.Name(), s.Name()), s.Storage, s.Path)
		if err != nil {
			if apiErr := apiStatus(err); apiErr != nil {
				return nil, fmt.Errorf("render BackupStorage %q: %w", s.Name(), err)
			}
			c.RejectBackupStorage(s.Name(), err)
			continue
		}
		objStore.Labels[StorageLabel] = s.Name()
		objStore.Spec.RetentionPolicy = retention
		if err := c.Apply(objStore); err != nil {
			if bce := ObjectStoreCRDMissing(err); bce != nil {
				return nil, bce
			}
			return nil, fmt.Errorf("apply ObjectStore %q: %w", objStore.Name, err)
		}
	}
	if err := deleteStaleObjectStores(c, keep); err != nil {
		return nil, err
	}

	stores, err := RenderedObjectStores(c)
	if err != nil {
		return nil, err
	}
	for _, s := range resolved {
		name, ok := stores[s.Name()]
		if !ok {
			continue
		}
		return []cnpgv1.PluginConfiguration{{
			Name:          PluginName,
			Enabled:       pointer.To(true),
			IsWALArchiver: pointer.To(true),
			Parameters: map[string]string{
				PluginParameterObjectStore: name,
			},
		}}, nil
	}
	return nil, nil
}

// RenderedObjectStores returns the ObjectStore of every backup storage of the
// Instance the engine gets, keyed by BackupStorage name: the entries rendered
// in this reconcile, and the ones held at the ObjectStore they already have.
// Schedules and backups only reach these.
func RenderedObjectStores(c *controller.Context) (map[string]string, error) {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled {
		return nil, nil
	}
	resolved, err := c.BackupStorages()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(resolved))
	for _, s := range resolved {
		name := ObjectStoreName(c.Name(), s.Name())
		if s.Apply() {
			out[s.Name()] = name
			continue
		}
		err := c.Get(&barmancloudv1.ObjectStore{}, name)
		switch {
		case err == nil:
			out[s.Name()] = name
		case apierrors.IsNotFound(err):
		default:
			if bce := ObjectStoreCRDMissing(err); bce != nil {
				return nil, bce
			}
			return nil, fmt.Errorf("get ObjectStore %q: %w", name, err)
		}
	}
	return out, nil
}

// deleteStaleObjectStores deletes the archive ObjectStores of the Instance
// whose storage is no longer in spec.backup.storages.
func deleteStaleObjectStores(c *controller.Context, keep map[string]bool) error {
	stores := &barmancloudv1.ObjectStoreList{}
	if err := c.List(stores); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list ObjectStores: %w", err)
	}
	for i := range stores.Items {
		store := &stores.Items[i]
		storage, ok := store.Labels[StorageLabel]
		if !ok || keep[storage] || !metav1.IsControlledBy(store, c.Instance()) {
			continue
		}
		if err := c.Delete(store); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ObjectStore %q: %w", store.Name, err)
		}
	}
	return nil
}

// ObjectStoreName returns the name of the ObjectStore the Instance uses for
// role, a BackupStorage name or RestoreSourceRole. The hash keeps names of
// different Instances and roles apart, and long names within a label value.
func ObjectStoreName(instance, role string) string {
	const hashLength = 8
	sum := sha256.Sum256([]byte(instance + "/" + role))
	hash := hex.EncodeToString(sum[:])[:hashLength]
	name := instance + "-" + role
	if maxPrefix := validation.LabelValueMaxLength - hashLength - 1; len(name) > maxPrefix {
		name = strings.TrimRight(name[:maxPrefix], "-.")
	}
	return name + "-" + hash
}

// BuildObjectStore renders an s3 BackupStorage as an ObjectStore named name
// whose WALs and base backups go under location in the storage's bucket.
func BuildObjectStore(
	c *controller.Context,
	name string,
	bs *backupv1alpha1.BackupStorage,
	location string,
) (*barmancloudv1.ObjectStore, error) {
	config, err := ObjectStoreConfiguration(c, bs, DestinationPath(bs, location))
	if err != nil {
		return nil, err
	}
	config.Wal = &barmanapi.WalBackupConfiguration{
		Compression: barmanapi.CompressionTypeGzip,
	}
	return &barmancloudv1.ObjectStore{
		ObjectMeta: c.ObjectMeta(name),
		Spec: barmancloudv1.ObjectStoreSpec{
			Configuration: config,
		},
	}, nil
}

// DestinationPath is the Barman destination path for location in bs's
// bucket. Barman writes each server under it, in a folder named after it.
func DestinationPath(bs *backupv1alpha1.BackupStorage, location string) string {
	var bucket string
	if store := bs.Spec.Location.ObjectStore; store != nil {
		bucket = store.Bucket
	}
	location = strings.Trim(location, "/")
	if location == "" {
		return "s3://" + bucket + "/"
	}
	return "s3://" + bucket + "/" + location + "/"
}

// ObjectStoreConfiguration renders the connection to an s3 BackupStorage
// writing to destination. Barman reads the AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY of the storage's credentials Secret.
func ObjectStoreConfiguration(
	c *controller.Context,
	bs *backupv1alpha1.BackupStorage,
	destination string,
) (barmanapi.BarmanObjectStoreConfiguration, error) {
	s3, _, err := c.BackupStorageS3(bs)
	if err != nil {
		return barmanapi.BarmanObjectStoreConfiguration{}, err
	}
	if s3.EffectiveImplementation() == s3class.ImplementationGCS {
		return barmanapi.BarmanObjectStoreConfiguration{}, fmt.Errorf(
			"barman cloud cannot use GCS through its S3 client: %w", controller.ErrImplementationUnsupported)
	}
	tls, err := c.BackupStorageTLS(bs, backupclasses.TLSSupport(BackupClassName, s3class.ClassName))
	if err != nil {
		return barmanapi.BarmanObjectStoreConfiguration{}, err
	}
	secretName := bs.Spec.Authentication.CredentialsSecretRef.Name

	config := barmanapi.BarmanObjectStoreConfiguration{
		EndpointURL:     s3.EndpointURL,
		DestinationPath: destination,
		BarmanCredentials: barmanapi.BarmanCredentials{
			AWS: &barmanapi.S3Credentials{
				AccessKeyIDReference: &machineryapi.SecretKeySelector{
					LocalObjectReference: machineryapi.LocalObjectReference{Name: secretName},
					Key:                  s3class.KeyAWSAccessKeyID,
				},
				SecretAccessKeyReference: &machineryapi.SecretKeySelector{
					LocalObjectReference: machineryapi.LocalObjectReference{Name: secretName},
					Key:                  s3class.KeyAWSSecretAccessKey,
				},
			},
		},
	}
	if ref := tls.CASecretKeyRef; ref != nil {
		config.EndpointCA = &machineryapi.SecretKeySelector{
			LocalObjectReference: machineryapi.LocalObjectReference{Name: ref.Name},
			Key:                  ref.Key,
		}
	}
	return config, nil
}

// apiStatus returns err as an API error, nil when it is not one: a failure
// to read the cluster, not a storage the engine cannot use.
func apiStatus(err error) apierrors.APIStatus {
	var apiErr apierrors.APIStatus
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

func DecodeBackupConfig(backup *backupv1alpha1.Backup) (cnpgbarmanplugin.CnpgBarmanPluginBackupConfig, error) {
	return decodeBackupParameters(backup.Spec.Parameters)
}

func decodeBackupParameters(raw *runtime.RawExtension) (cnpgbarmanplugin.CnpgBarmanPluginBackupConfig, error) {
	var cfg cnpgbarmanplugin.CnpgBarmanPluginBackupConfig
	if raw == nil || len(raw.Raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("decode backup config: %w", err)
	}
	return cfg, nil
}

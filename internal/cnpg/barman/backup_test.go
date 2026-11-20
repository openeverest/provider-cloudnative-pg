package barman

import (
	"context"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	barmancloudv1 "github.com/cloudnative-pg/plugin-barman-cloud/api/v1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNativeRetention(t *testing.T) {
	timePolicy := func(duration string) *corev1alpha1.BackupScheduleRetention {
		return &corev1alpha1.BackupScheduleRetention{
			Type: corev1alpha1.BackupScheduleRetentionTypeTime, Duration: duration,
		}
	}
	withRetention := func(name string, policy *corev1alpha1.BackupScheduleRetention) corev1alpha1.InstanceBackupSchedule {
		s := schedule(name, "0 2 * * *", true)
		s.Retention = policy
		return s
	}
	disabled := withRetention("paused", timePolicy("7d"))
	disabled.Enabled = false
	tests := []struct {
		name      string
		schedules []corev1alpha1.InstanceBackupSchedule
		want      string
		wantErr   string
	}{
		{name: "no schedules"},
		{name: "keep all", schedules: []corev1alpha1.InstanceBackupSchedule{withRetention("daily", nil)}},
		{name: "days", schedules: []corev1alpha1.InstanceBackupSchedule{withRetention("daily", timePolicy("30d"))}, want: "30d"},
		{name: "weeks", schedules: []corev1alpha1.InstanceBackupSchedule{withRetention("daily", timePolicy("4w"))}, want: "4w"},
		{name: "months", schedules: []corev1alpha1.InstanceBackupSchedule{withRetention("daily", timePolicy("2m"))}, want: "2m"},
		{name: "matching schedules", schedules: []corev1alpha1.InstanceBackupSchedule{
			withRetention("daily", timePolicy("30d")), withRetention("weekly", timePolicy("30d")),
		}, want: "30d"},
		{name: "conflicting schedules", schedules: []corev1alpha1.InstanceBackupSchedule{
			withRetention("daily", timePolicy("30d")), withRetention("weekly", timePolicy("7d")),
		}, wantErr: "same time retention"},
		{name: "keep all conflicts", schedules: []corev1alpha1.InstanceBackupSchedule{
			withRetention("daily", nil), withRetention("weekly", timePolicy("7d")),
		}, wantErr: "same time retention"},
		{name: "disabled schedule conflicts", schedules: []corev1alpha1.InstanceBackupSchedule{
			withRetention("daily", timePolicy("30d")), disabled,
		}, wantErr: "same time retention"},
		{name: "count rejected", schedules: []corev1alpha1.InstanceBackupSchedule{
			withRetention("daily", &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount}),
		}, wantErr: "only time retention"},
		{name: "invalid duration", schedules: []corev1alpha1.InstanceBackupSchedule{withRetention("daily", timePolicy("0d"))}, wantErr: "invalid retention duration"},
		{name: "empty duration", schedules: []corev1alpha1.InstanceBackupSchedule{withRetention("daily", timePolicy(""))}, wantErr: "invalid retention duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := storageWithSchedules("s3", tt.schedules...)
			cfg := &corev1alpha1.InstanceBackupSpec{
				Enabled: true, Storages: []corev1alpha1.InstanceBackupStorage{storage},
			}
			err := ValidateSchedules(cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ValidateSchedules error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			instance := &corev1alpha1.Instance{
				ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace, UID: "uid-1"},
				Spec:       corev1alpha1.InstanceSpec{Backup: cfg},
			}
			ctx, cl := storageTestContext(t, instance, testBackupStorage("s3", minioParameters, nil))
			if _, err := SyncBackupInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			got := &barmancloudv1.ObjectStore{}
			key := client.ObjectKey{Namespace: testNamespace, Name: ObjectStoreName("pg", "s3")}
			if err := cl.Get(context.Background(), key, got); err != nil {
				t.Fatal(err)
			}
			if got.Spec.RetentionPolicy != tt.want {
				t.Fatalf("ObjectStore retention = %q, want %q", got.Spec.RetentionPolicy, tt.want)
			}
			for i := range cfg.Storages[0].Schedules {
				cfg.Storages[0].Schedules[i].Retention = nil
			}
			ctx, _ = storageTestContextWithClient(cl, instance)
			if _, err := SyncBackupInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(context.Background(), key, got); err != nil {
				t.Fatal(err)
			}
			if got.Spec.RetentionPolicy != "" {
				t.Fatalf("removed retention is still configured: %q", got.Spec.RetentionPolicy)
			}
		})
	}
}

const (
	testStorageCredentials = "storage-credentials"
	minioParameters        = `{"region":"us-east-1","endpointURL":"https://minio.minio.svc:9000","forcePathStyle":true,"implementation":"minio"}`
	gcsParameters          = `{"region":"auto","endpointURL":"https://storage.googleapis.com","implementation":"gcs"}`
)

func testBackupStorage(name, parameters string, tls *commonv1alpha1.TLSConfig) *backupv1alpha1.BackupStorage {
	return &backupv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: backupv1alpha1.BackupStorageSpec{
			BackupStorageClassRef: commonv1alpha1.ObjectRef{Name: s3class.ClassName},
			Location: backupv1alpha1.BackupStorageLocation{
				ObjectStore: &backupv1alpha1.ObjectStoreLocation{Bucket: "backups", Prefix: "team"},
			},
			Parameters: &runtime.RawExtension{Raw: []byte(parameters)},
			Authentication: commonv1alpha1.Authentication{
				Mode:                 s3class.AuthModeAccessKey,
				CredentialsSecretRef: &commonv1alpha1.SecretRef{Name: testStorageCredentials},
			},
			TLS: tls,
		},
		Status: backupv1alpha1.BackupStorageStatus{Conditions: []metav1.Condition{{
			Type:               backupv1alpha1.BackupStorageConditionAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             backupv1alpha1.ReasonAccepted,
			ObservedGeneration: 1,
		}}},
	}
}

func storageTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	barmancloudv1.AddKnownTypes(scheme)
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, corev1alpha1.AddToScheme, backupv1alpha1.AddToScheme, cnpgv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// storageTestContext returns a Context for instance over a fake client
// holding objs and the storage credentials and CA Secrets.
func storageTestContext(t *testing.T, instance *corev1alpha1.Instance, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	objs = append(objs, instance,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testStorageCredentials, Namespace: testNamespace},
			Data: map[string][]byte{
				s3class.KeyAWSAccessKeyID:     []byte("id"),
				s3class.KeyAWSSecretAccessKey: []byte("secret"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "storage-ca", Namespace: testNamespace},
			Data:       map[string][]byte{"ca.crt": []byte("-----BEGIN CERTIFICATE-----")},
		},
	)
	cl := fake.NewClientBuilder().WithScheme(storageTestScheme(t)).WithObjects(objs...).Build()
	return storageTestContextWithClient(cl, instance)
}

// storageTestContextWithClient starts a new reconcile of instance: the
// Context caches resolved storages, so a changed spec needs a new one.
func storageTestContextWithClient(cl client.Client, instance *corev1alpha1.Instance) (*controller.Context, client.Client) {
	return controller.NewContext(context.Background(), cl, instance, "provider-cloudnative-pg"), cl
}

func backupInstance(name string, uid types.UID, storages ...string) *corev1alpha1.Instance {
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: uid},
		Spec:       corev1alpha1.InstanceSpec{Backup: &corev1alpha1.InstanceBackupSpec{Enabled: true}},
	}
	for _, s := range storages {
		instance.Spec.Backup.Storages = append(instance.Spec.Backup.Storages, storageWithSchedules(s))
	}
	return instance
}

func TestObjectStorePerInstance(t *testing.T) {
	storage := testBackupStorage("s3", minioParameters, nil)
	first := backupInstance("pg", "uid-1", "s3")
	ctx, cl := storageTestContext(t, first, storage)
	plugins, err := SyncBackupInfrastructure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := backupInstance("other", "uid-2", "s3")
	if err := cl.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	ctx, _ = storageTestContextWithClient(cl, second)
	if _, err := SyncBackupInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}

	name := ObjectStoreName("pg", "s3")
	if len(plugins) != 1 || plugins[0].Parameters[PluginParameterObjectStore] != name {
		t.Fatalf("plugin configuration = %+v, want archiving to %s", plugins, name)
	}
	for instance, want := range map[string]string{
		"pg":    "s3://backups/team/backups/ns/pg/uid-1/s3/",
		"other": "s3://backups/team/backups/ns/other/uid-2/s3/",
	} {
		got := &barmancloudv1.ObjectStore{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ObjectStoreName(instance, "s3")}, got); err != nil {
			t.Fatal(err)
		}
		cfg := got.Spec.Configuration
		if cfg.DestinationPath != want {
			t.Errorf("%s destinationPath = %q, want %q", instance, cfg.DestinationPath, want)
		}
		if cfg.EndpointURL != "https://minio.minio.svc:9000" || cfg.EndpointCA != nil {
			t.Errorf("%s endpoint = %q, CA = %+v", instance, cfg.EndpointURL, cfg.EndpointCA)
		}
		if cfg.AWS == nil || cfg.AWS.AccessKeyIDReference.Name != testStorageCredentials ||
			cfg.AWS.AccessKeyIDReference.Key != s3class.KeyAWSAccessKeyID {
			t.Errorf("%s credentials = %+v", instance, cfg.AWS)
		}
		if got.Labels[StorageLabel] != "s3" {
			t.Errorf("%s labels = %v", instance, got.Labels)
		}
	}
}

func TestObjectStoreTLS(t *testing.T) {
	ca := &commonv1alpha1.TLSConfig{CA: &commonv1alpha1.CABundleSource{
		SecretKeyRef: &commonv1alpha1.SecretKeyRef{Name: "storage-ca", Key: "ca.crt"},
	}}
	tests := map[string]struct {
		parameters string
		tls        *commonv1alpha1.TLSConfig
		wantCA     bool
		wantReason string
	}{
		"system trust store":       {parameters: minioParameters},
		"custom CA":                {parameters: minioParameters, tls: ca, wantCA: true},
		"insecureSkipVerify":       {parameters: minioParameters, tls: &commonv1alpha1.TLSConfig{InsecureSkipVerify: true}, wantReason: corev1alpha1.ReasonTLSOptionUnsupported},
		"gcs through the S3 class": {parameters: gcsParameters, wantReason: corev1alpha1.ReasonImplementationUnsupported},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			instance := backupInstance("pg", "uid-1", "s3")
			ctx, cl := storageTestContext(t, instance, testBackupStorage("s3", tt.parameters, tt.tls))
			plugins, err := SyncBackupInfrastructure(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := &barmancloudv1.ObjectStore{}
			getErr := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ObjectStoreName("pg", "s3")}, got)
			if tt.wantReason != "" {
				resolved, err := ctx.BackupStorages()
				if err != nil {
					t.Fatal(err)
				}
				if resolved[0].Reason != tt.wantReason {
					t.Fatalf("reason = %q, want %q", resolved[0].Reason, tt.wantReason)
				}
				if !apierrors.IsNotFound(getErr) || plugins != nil {
					t.Fatalf("rejected storage was rendered: %v, plugins %+v", getErr, plugins)
				}
				return
			}
			if getErr != nil {
				t.Fatal(getErr)
			}
			if gotCA := got.Spec.Configuration.EndpointCA; (gotCA != nil) != tt.wantCA ||
				(gotCA != nil && (gotCA.Name != "storage-ca" || gotCA.Key != "ca.crt")) {
				t.Fatalf("endpointCA = %+v, want CA %t", gotCA, tt.wantCA)
			}
		})
	}
}

func TestStaleObjectStoresDeleted(t *testing.T) {
	instance := backupInstance("pg", "uid-1", "s3", "s3-b")
	ctx, cl := storageTestContext(t, instance,
		testBackupStorage("s3", minioParameters, nil), testBackupStorage("s3-b", minioParameters, nil))
	if _, err := SyncBackupInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	unrelated := &barmancloudv1.ObjectStore{ObjectMeta: metav1.ObjectMeta{
		Name: "manual", Namespace: testNamespace, Labels: map[string]string{StorageLabel: "s3-b"},
	}}
	if err := cl.Create(context.Background(), unrelated); err != nil {
		t.Fatal(err)
	}

	instance.Spec.Backup.Storages = instance.Spec.Backup.Storages[:1]
	ctx, _ = storageTestContextWithClient(cl, instance)
	if _, err := SyncBackupInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	exists := func(name string) bool {
		err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &barmancloudv1.ObjectStore{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	if !exists(ObjectStoreName("pg", "s3")) || exists(ObjectStoreName("pg", "s3-b")) || !exists("manual") {
		t.Fatal("only the removed storage's ObjectStore owned by the Instance must be deleted")
	}

	instance.Spec.Backup.Enabled = false
	ctx, _ = storageTestContextWithClient(cl, instance)
	if _, err := SyncBackupInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(ObjectStoreName("pg", "s3")) {
		t.Fatal("ObjectStore survived disabling backups")
	}
}

func TestObjectStoreName(t *testing.T) {
	long := ObjectStoreName(strings.Repeat("a", 60), "storage")
	if len(long) > 63 {
		t.Fatalf("name length = %d", len(long))
	}
	if ObjectStoreName("a-b", "c") == ObjectStoreName("a", "b-c") {
		t.Fatal("names collide across instance and storage boundaries")
	}
}

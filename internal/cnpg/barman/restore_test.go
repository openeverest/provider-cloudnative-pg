package barman

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBuildRecoveryConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, backupv1alpha1.AddToScheme, cnpgv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	targetTime := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	tests := []struct {
		name       string
		dataSource *backupv1alpha1.DataSource
		objects    []client.Object
		custom     components.CNPGCustomSpec
		wantWait   string
		wantErr    string
		wantBackup string
		wantStore  string
		wantServer string
		wantTime   string
	}{
		{
			name: "point-in-time recovery",
			dataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypePointInTime,
				PointInTime: &backupv1alpha1.DataSourcePointInTime{
					Source: backupv1alpha1.StreamSource{
						StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
					},
					RecoveryTarget: backupv1alpha1.RecoveryTargetDate,
					Date:           &targetTime,
				},
			},
			wantStore:  "s3",
			wantServer: "pg",
			wantTime:   "2026-01-02T03:04:05Z",
		},
		{
			name: "waits for missing OpenEverest backup",
			dataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{
					BackupRef: commonv1alpha1.ObjectRef{Name: "missing"},
				},
			},
			wantWait: `source Backup "missing" not yet present`,
		},
		{
			name: "waits for backup not yet succeeded",
			dataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{
					BackupRef: commonv1alpha1.ObjectRef{Name: "b"},
				},
			},
			objects: []client.Object{
				&backupv1alpha1.Backup{
					ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNamespace},
					Spec:       backupv1alpha1.BackupSpec{StorageRef: commonv1alpha1.ObjectRef{Name: "s3"}},
					Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateRunning},
				},
			},
			wantWait: `source Backup "b" is in state "Running"`,
		},
		{
			name: "errors when storageRef missing",
			dataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{
					BackupRef: commonv1alpha1.ObjectRef{Name: "b"},
				},
			},
			objects: []client.Object{
				&backupv1alpha1.Backup{
					ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNamespace},
					Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
				},
			},
			wantErr: "StorageRefMissing",
		},
		{
			name: "waits for CloudNativePG backup",
			dataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{
					BackupRef: commonv1alpha1.ObjectRef{Name: "b"},
				},
			},
			objects: []client.Object{
				&backupv1alpha1.Backup{
					ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNamespace},
					Spec:       backupv1alpha1.BackupSpec{StorageRef: commonv1alpha1.ObjectRef{Name: "s3"}},
					Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
				},
			},
			wantWait: `CloudNativePG Backup "b" not yet present`,
		},
		{
			name: "successful backup recovery",
			dataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{
					BackupRef: commonv1alpha1.ObjectRef{Name: "b"},
				},
			},
			objects: []client.Object{
				&backupv1alpha1.Backup{
					ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNamespace},
					Spec:       backupv1alpha1.BackupSpec{StorageRef: commonv1alpha1.ObjectRef{Name: "s3"}},
					Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
				},
				&cnpgv1.Backup{
					ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNamespace},
					Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "origin-cluster"}},
					Status: cnpgv1.BackupStatus{
						Phase:      cnpgv1.BackupPhaseCompleted,
						BackupID:   "20260102T030405",
						ServerName: "origin-cluster",
					},
				},
			},
			wantBackup: "20260102T030405",
			wantStore:  "s3",
			wantServer: "origin-cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &corev1alpha1.Instance{
				ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace},
				Spec:       corev1alpha1.InstanceSpec{DataSource: tt.dataSource},
			}
			objects := append([]client.Object{instance}, tt.objects...)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			c := controller.NewContext(context.Background(), cl, instance, "provider-cloudnative-pg")

			recovery, external, err := BuildRecoveryConfig(c, tt.custom)
			if tt.wantWait != "" {
				if !controller.IsWaitError(err) || !strings.Contains(err.Error(), tt.wantWait) {
					t.Fatalf("error = %v, want WaitError containing %q", err, tt.wantWait)
				}
				return
			}
			if tt.wantErr != "" {
				bce := controller.AsBackupConfigError(err)
				if bce == nil {
					t.Fatalf("error = %v, want BackupConfigError", err)
				}
				if bce.Reason != tt.wantErr && !strings.Contains(bce.Message, tt.wantErr) && !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want reason/message %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildRecoveryConfig: %v", err)
			}
			if recovery.Source != recoveryExternalClusterName {
				t.Fatalf("recovery source = %q", recovery.Source)
			}
			if tt.wantBackup != "" {
				if recovery.RecoveryTarget == nil || recovery.RecoveryTarget.BackupID != tt.wantBackup {
					t.Fatalf("BackupID = %+v, want %q", recovery.RecoveryTarget, tt.wantBackup)
				}
			}
			if tt.wantTime != "" {
				if recovery.RecoveryTarget == nil || recovery.RecoveryTarget.TargetTime != tt.wantTime {
					t.Fatalf("TargetTime = %+v, want %q", recovery.RecoveryTarget, tt.wantTime)
				}
			}
			if external.PluginConfiguration == nil {
				t.Fatal("expected plugin configuration on external cluster")
			}
			if got := external.PluginConfiguration.Parameters[PluginParameterObjectStore]; got != tt.wantStore {
				t.Fatalf("objectStore = %q, want %q", got, tt.wantStore)
			}
			if got := external.PluginConfiguration.Parameters["serverName"]; got != tt.wantServer {
				t.Fatalf("serverName = %q, want %q", got, tt.wantServer)
			}
		})
	}
}

func TestBuildRecoveryConfigPropagatesInitDB(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, backupv1alpha1.AddToScheme, cnpgv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace},
		Spec: corev1alpha1.InstanceSpec{
			DataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypePointInTime,
				PointInTime: &backupv1alpha1.DataSourcePointInTime{
					Source: backupv1alpha1.StreamSource{
						StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
					},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	c := controller.NewContext(context.Background(), cl, instance, "provider-cloudnative-pg")
	custom := components.CNPGCustomSpec{
		Bootstrap: &components.BootstrapConfiguration{
			InitDB: &cnpgv1.BootstrapInitDB{Database: "appdb", Owner: "appuser"},
		},
	}

	recovery, _, err := BuildRecoveryConfig(c, custom)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Database != "appdb" || recovery.Owner != "appuser" {
		t.Fatalf("database/owner = %q/%q", recovery.Database, recovery.Owner)
	}
}

func TestBuildRecoveryConfigWrapsGetErrors(t *testing.T) {
	// Ensure non-NotFound Get failures become BackupConfigError.
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = backupv1alpha1.AddToScheme(scheme)
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace},
		Spec: corev1alpha1.InstanceSpec{
			DataSource: &backupv1alpha1.DataSource{
				Type: backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{
					BackupRef: commonv1alpha1.ObjectRef{Name: "b"},
				},
			},
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	c := controller.NewContext(context.Background(), errClient{Client: base, err: errors.New("boom")}, instance, "provider-cloudnative-pg")
	_, _, err := BuildRecoveryConfig(c, components.CNPGCustomSpec{})
	bce := controller.AsBackupConfigError(err)
	if bce == nil || bce.Reason != "BackupResolutionFailed" {
		t.Fatalf("error = %v, want BackupConfigError BackupResolutionFailed", err)
	}
}

type errClient struct {
	client.Client
	err error
}

func (c errClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*backupv1alpha1.Backup); ok {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

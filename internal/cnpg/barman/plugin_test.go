package barman

import (
	"context"
	"testing"

	"github.com/AlekSi/pointer"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	barmancloudv1 "github.com/cloudnative-pg/plugin-barman-cloud/api/v1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestArchiveObjectStoreName(t *testing.T) {
	barmanPlugin := func(enabled *bool) cnpgv1.PluginConfiguration {
		return cnpgv1.PluginConfiguration{
			Name:       PluginName,
			Enabled:    enabled,
			Parameters: map[string]string{PluginParameterObjectStore: "s3"},
		}
	}

	tests := []struct {
		name    string
		plugins []cnpgv1.PluginConfiguration
		want    string
	}{
		{name: "enabled plugin", plugins: []cnpgv1.PluginConfiguration{barmanPlugin(pointer.To(true))}, want: "s3"},
		{name: "plugin enabled by default", plugins: []cnpgv1.PluginConfiguration{barmanPlugin(nil)}, want: "s3"},
		{name: "disabled plugin", plugins: []cnpgv1.PluginConfiguration{barmanPlugin(pointer.To(false))}},
		{name: "no plugins"},
		{
			name: "only other plugins",
			plugins: []cnpgv1.PluginConfiguration{{
				Name:       "other.example.com",
				Parameters: map[string]string{PluginParameterObjectStore: "s3"},
			}},
		},
		{
			name: "barman plugin after another plugin",
			plugins: []cnpgv1.PluginConfiguration{
				{Name: "other.example.com"},
				barmanPlugin(pointer.To(true)),
			},
			want: "s3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: tt.plugins}}
			if got := ArchiveObjectStoreName(cluster); got != tt.want {
				t.Fatalf("ArchiveObjectStoreName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestObjectStoreCRDMissing(t *testing.T) {
	if got := ObjectStoreCRDMissing(nil); got != nil {
		t.Fatalf("nil error mapped to %+v", got)
	}
	if got := ObjectStoreCRDMissing(apierrors.NewNotFound(schema.GroupResource{Resource: "objectstores"}, "s3")); got != nil {
		t.Fatalf("NotFound mapped to %+v", got)
	}
	noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: barmancloudv1.GroupVersion.Group, Kind: "ObjectStore"}}
	got := ObjectStoreCRDMissing(noMatch)
	if got == nil || got.Reason != ReasonBarmanPluginMissing {
		t.Fatalf("NoMatch = %+v, want Reason %q", got, ReasonBarmanPluginMissing)
	}
}

// noMatchObjectStoreClient returns NoMatch for ObjectStore Gets to simulate a
// cluster without the Barman Cloud Plugin CRDs.
type noMatchObjectStoreClient struct {
	client.Client
}

func (c noMatchObjectStoreClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*barmancloudv1.ObjectStore); ok {
		return &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: barmancloudv1.GroupVersion.Group, Kind: "ObjectStore"},
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestSyncBackupInfrastructureMissingCRD(t *testing.T) {
	instance := backupInstance("pg", "uid-1", "s3")
	_, base := storageTestContext(t, instance, testBackupStorage("s3", minioParameters, nil))
	ctx := controller.NewContext(context.Background(), noMatchObjectStoreClient{Client: base}, instance, "provider-cloudnative-pg")

	_, err := SyncBackupInfrastructure(ctx)
	bce := controller.AsBackupConfigError(err)
	if bce == nil || bce.Reason != ReasonBarmanPluginMissing {
		t.Fatalf("error = %v, want BackupConfigError reason %q", err, ReasonBarmanPluginMissing)
	}
}

package provider

import (
	"testing"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/openeverest/provider-cloudnative-pg/definition/backupclasses"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg/barman"
)

func TestShippedBackupClasses(t *testing.T) {
	p := New()
	classes := p.ShippedBackupClasses()
	if len(classes) != 1 {
		t.Fatalf("shipped %d BackupClasses, want 1", len(classes))
	}
	class := classes[0]
	if class.Name != barman.BackupClassName || class.Spec.ExecutorProvider() != p.Name() {
		t.Fatalf("class %q is executed by %q, want %q by %q",
			class.Name, class.Spec.ExecutorProvider(), barman.BackupClassName, p.Name())
	}

	entry := class.Spec.FindBackupStorageClass(s3class.ClassName)
	if entry == nil || entry.ParametersSchema == nil {
		t.Fatalf("class does not declare the s3 BackupStorageClass with a schema: %+v", entry)
	}
	enum := entry.ParametersSchema.OpenAPIV3Schema.Properties["implementation"].Enum
	has := func(value string) bool {
		for _, v := range enum {
			if string(v.Raw) == `"`+value+`"` {
				return true
			}
		}
		return false
	}
	if has("gcs") || !has("minio") {
		t.Errorf("implementation enum = %s, want minio and no gcs (barman cannot use GCS through S3)", enumString(enum))
	}
	wantTLS := &backupv1alpha1.BackupStorageTLSSupport{
		CustomCA:           backupv1alpha1.TLSOptionSupported,
		InsecureSkipVerify: backupv1alpha1.TLSOptionNone,
	}
	if *entry.TLS != *wantTLS {
		t.Errorf("tls = %+v, want %+v", entry.TLS, wantTLS)
	}
	if got := backupclasses.TLSSupport(barman.BackupClassName, s3class.ClassName); got != (controller.TLSSupport{CustomCA: true}) {
		t.Errorf("TLSSupport = %+v, want custom CA only", got)
	}
}

func enumString(enum []apiextensionsv1.JSON) string {
	out := ""
	for _, v := range enum {
		out += string(v.Raw) + " "
	}
	return out
}

package cnpg

import (
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

func TestValidateStorageChanges(t *testing.T) {
	engine := func(size, class string) corev1alpha1.ComponentSpec {
		s := &corev1alpha1.Storage{Size: resource.MustParse(size)}
		if class != "" {
			s.StorageClass = &class
		}
		return corev1alpha1.ComponentSpec{Storage: s}
	}
	wal := func(size, class string) *components.CNPGCustomSpec {
		w := &cnpgv1.StorageConfiguration{Size: size}
		if class != "" {
			w.StorageClass = &class
		}
		return &components.CNPGCustomSpec{WALStorage: w}
	}
	none := &components.CNPGCustomSpec{}

	for _, tt := range []struct {
		name              string
		old, updated      corev1alpha1.ComponentSpec
		oldCustom, custom *components.CNPGCustomSpec
		want              []string
		destructive       []bool
	}{
		{name: "unchanged", old: engine("10Gi", "fast"), updated: engine("10Gi", "fast"), oldCustom: wal("2Gi", ""), custom: wal("2Gi", "")},
		{name: "grown", old: engine("10Gi", ""), updated: engine("20Gi", ""), oldCustom: wal("2Gi", ""), custom: wal("4Gi", "")},
		{name: "same size, other notation", old: engine("1Gi", ""), updated: engine("1024Mi", ""), oldCustom: none, custom: none},
		{name: "WAL storage added", old: engine("10Gi", ""), updated: engine("10Gi", ""), oldCustom: none, custom: wal("2Gi", "wal")},
		{
			name: "data shrunk", old: engine("10Gi", ""), updated: engine("5Gi", ""), oldCustom: none, custom: none,
			want: []string{"e.storage.size"}, destructive: []bool{true},
		},
		{
			name: "data class changed", old: engine("10Gi", ""), updated: engine("10Gi", "fast"), oldCustom: none, custom: none,
			want: []string{"e.storage.storageClass"}, destructive: []bool{false},
		},
		{
			name: "WAL storage removed", old: engine("10Gi", ""), updated: engine("10Gi", ""), oldCustom: wal("2Gi", ""), custom: none,
			want: []string{"p.walStorage"}, destructive: []bool{true},
		},
		{
			name: "WAL shrunk and class changed", old: engine("10Gi", ""), updated: engine("10Gi", ""), oldCustom: wal("2Gi", "a"), custom: wal("1Gi", "b"),
			want: []string{"p.walStorage.size", "p.walStorage.storageClass"}, destructive: []bool{true, false},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			var destructive []bool
			for _, err := range ValidateStorageChanges(field.NewPath("e"), field.NewPath("p"), tt.old, tt.updated, tt.oldCustom, tt.custom) {
				if err.Type != field.ErrorTypeForbidden {
					t.Errorf("%s: type = %s, want Forbidden", err.Field, err.Type)
				}
				got = append(got, err.Field)
				destructive = append(destructive, strings.HasPrefix(err.Detail, Destructive))
			}
			if !slices.Equal(got, tt.want) || !slices.Equal(destructive, tt.destructive) {
				t.Fatalf("fields = %v destructive = %v, want %v %v", got, destructive, tt.want, tt.destructive)
			}
		})
	}
}

func TestValidateMajorVersionChange(t *testing.T) {
	for _, tt := range []struct {
		old, updated string
		wantErr      bool
		wantWarning  bool
	}{
		{old: "18.6", updated: "18.7"},
		{old: "18.7", updated: "18.6"},
		{old: "", updated: "18.7"},
		{old: "17.10", updated: "18.7", wantWarning: true},
		{old: "18.7", updated: "17.10", wantErr: true},
		{old: "14.23", updated: "15.18", wantErr: true},
	} {
		t.Run(tt.old+"->"+tt.updated, func(t *testing.T) {
			errs, warnings := ValidateMajorVersionChange(field.NewPath("spec", "version"), tt.old, tt.updated)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("errors = %v, wantErr %v", errs, tt.wantErr)
			}
			if (len(warnings) > 0) != tt.wantWarning {
				t.Errorf("warnings = %v, wantWarning %v", warnings, tt.wantWarning)
			}
			for _, w := range warnings {
				if !strings.HasPrefix(w, "spec.version: "+Destructive) {
					t.Errorf("warning %q does not start with the path and the destructive marker", w)
				}
			}
		})
	}
}

func TestBackupChangeWarnings(t *testing.T) {
	on := &corev1alpha1.InstanceBackupSpec{Enabled: true}
	off := &corev1alpha1.InstanceBackupSpec{}
	for _, tt := range []struct {
		name         string
		old, updated *corev1alpha1.InstanceBackupSpec
		want         bool
	}{
		{name: "kept on", old: on, updated: on},
		{name: "turned on", old: off, updated: on},
		{name: "never on", old: nil, updated: nil},
		{name: "turned off", old: on, updated: off, want: true},
		{name: "removed", old: on, updated: nil, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := BackupChangeWarnings(tt.old, tt.updated)
			if (len(got) > 0) != tt.want {
				t.Fatalf("warnings = %v, want %v", got, tt.want)
			}
			if tt.want && !strings.HasPrefix(got[0], "spec.backup.enabled: "+Destructive) {
				t.Fatalf("warning %q does not start with the path and the destructive marker", got[0])
			}
		})
	}
}

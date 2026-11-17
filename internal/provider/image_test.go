package provider

import (
	"testing"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

func TestResolveEngineImage(t *testing.T) {
	const (
		defaultImage = "ghcr.io/cloudnative-pg/postgresql:18.7-standard-trixie"
		v17Image     = "ghcr.io/cloudnative-pg/postgresql:17.10-standard-trixie"
		override     = "example.com/custom-pg:1"
	)
	spec := &corev1alpha1.ProviderSpec{
		Components: map[string]corev1alpha1.Component{
			common.ComponentEngine: {Type: common.ComponentTypePostgresql},
		},
		ComponentTypes: map[string]corev1alpha1.ComponentType{
			common.ComponentTypePostgresql: {
				DefaultVersion: "18.7",
				Versions: []corev1alpha1.ComponentVersion{
					{Version: "17.10", Image: v17Image},
					{Version: "18.7", Image: defaultImage},
				},
			},
		},
	}

	tests := []struct {
		name   string
		engine corev1alpha1.ComponentSpec
		want   string
	}{
		{
			name:   "image override wins over version and default",
			engine: corev1alpha1.ComponentSpec{Image: override, Version: "17.10"},
			want:   override,
		},
		{
			name:   "engine.version selects catalog image",
			engine: corev1alpha1.ComponentSpec{Version: "17.10"},
			want:   v17Image,
		},
		{
			name:   "unknown version falls back to default",
			engine: corev1alpha1.ComponentSpec{Version: "9.6"},
			want:   defaultImage,
		},
		{
			name:   "empty version uses component default",
			engine: corev1alpha1.ComponentSpec{},
			want:   defaultImage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEngineImage(tt.engine, spec)
			if got != tt.want {
				t.Fatalf("resolveEngineImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

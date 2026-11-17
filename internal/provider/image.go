package provider

import (
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// resolveEngineImage picks the Cluster image: an explicit engine.Image override,
// else the catalog image for engine.Version, else the component default.
func resolveEngineImage(engine corev1alpha1.ComponentSpec, spec *corev1alpha1.ProviderSpec) string {
	if engine.Image != "" {
		return engine.Image
	}
	if engine.Version != "" {
		if image := controller.GetImageForVersion(spec, common.ComponentEngine, engine.Version); image != "" {
			return image
		}
	}
	return controller.GetDefaultImageForComponent(spec, common.ComponentEngine)
}

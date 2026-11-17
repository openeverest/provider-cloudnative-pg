package provider

import (
	corev1 "k8s.io/api/core/v1"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

func buildClusterSpec(engine corev1alpha1.ComponentSpec, custom components.CNPGCustomSpec) cnpgv1.ClusterSpec {
	storage := cnpgv1.StorageConfiguration{
		Size: engine.Storage.Size.String(),
	}

	if engine.Storage.StorageClass != nil && *engine.Storage.StorageClass != "" {
		storage.StorageClass = engine.Storage.StorageClass
	}

	if custom.PersistentVolumeClaimTemplate != nil {
		storage.PersistentVolumeClaimTemplate = custom.PersistentVolumeClaimTemplate
	}

	if custom.ResizeInUseVolumes != nil {
		storage.ResizeInUseVolumes = custom.ResizeInUseVolumes
	}

	return cnpgv1.ClusterSpec{
		Instances:            int(*engine.Replicas),
		PriorityClassName:    custom.PriorityClassName,
		StorageConfiguration: storage,
		WalStorage:           custom.WALStorage,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *engine.Resources.Requests.Cpu(),
				corev1.ResourceMemory: *engine.Resources.Requests.Memory(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    *engine.Resources.Limits.Cpu(),
				corev1.ResourceMemory: *engine.Resources.Limits.Memory(),
			},
		},
	}
}

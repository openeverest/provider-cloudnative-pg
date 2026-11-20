package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	barmancommand "github.com/cloudnative-pg/barman-cloud/pkg/command"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg/barman"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const backupCAPath = "/etc/barman-cloud/backup-ca.crt"

const (
	backupDeletionLabel       = "backup.openeverest.io/data-deletion"
	backupDeletionScheduleKey = "backup.openeverest.io/schedule-name"
)

func (p *Provider) deleteBackupData(
	c *controller.Context,
	backup *backupv1alpha1.Backup,
	cnpgBackup *cnpgv1.Backup,
) (bool, error) {
	switch cnpgBackup.Status.Phase {
	case cnpgv1.BackupPhasePending, cnpgv1.BackupPhaseStarted,
		cnpgv1.BackupPhaseRunning, cnpgv1.BackupPhaseFinalizing:
		return false, nil
	}
	// Not started, or failed before Barman assigned an ID: nothing is addressable.
	if cnpgBackup.Status.BackupID == "" {
		log.FromContext(c.Context()).Info("CloudNativePG backup has no Barman backup ID; skipping data deletion",
			"name", cnpgBackup.Name, "phase", cnpgBackup.Status.Phase)
		return true, nil
	}

	jobName := backupDeletionJobName(backup.Name)
	job := &batchv1.Job{}
	err := c.Get(job, jobName)
	if apierrors.IsNotFound(err) {
		config, region, err := deletionStorage(c, backup, cnpgBackup)
		if err != nil {
			return false, err
		}
		job, err = buildBackupDeletionJob(backup, cnpgBackup, config, region)
		if err != nil {
			return false, err
		}
		if err := controllerutil.SetControllerReference(backup, job, c.Client().Scheme()); err != nil {
			return false, fmt.Errorf("set owner for backup data deletion Job: %w", err)
		}
		if err := c.Client().Create(c.Context(), job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create backup data deletion Job %q: %w", jobName, err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get backup data deletion Job %q: %w", jobName, err)
	}
	if !metav1.IsControlledBy(job, backup) {
		return false, fmt.Errorf("backup data deletion Job %q is not owned by Backup %q", jobName, backup.Name)
	}
	return deletionJobDone(job)
}

// deletionStorage renders the connection to the storage holding cnpgBackup's
// data, and its region.
func deletionStorage(
	c *controller.Context,
	backup *backupv1alpha1.Backup,
	cnpgBackup *cnpgv1.Backup,
) (barmanapi.BarmanObjectStoreConfiguration, string, error) {
	backupStorage, err := c.BackupStorage(backup.Spec.StorageRef.Name)
	if err != nil {
		return barmanapi.BarmanObjectStoreConfiguration{}, "", fmt.Errorf("get BackupStorage %q for data deletion: %w", backup.Spec.StorageRef.Name, err)
	}
	destination := cnpgBackup.Status.DestinationPath
	if destination == "" {
		destination = barman.DestinationPath(backupStorage, c.BackupLocation(backup, backupStorage))
	}
	config, err := barman.ObjectStoreConfiguration(c, backupStorage, destination)
	if err != nil {
		return barmanapi.BarmanObjectStoreConfiguration{}, "", fmt.Errorf("render BackupStorage %q for data deletion: %w", backupStorage.Name, err)
	}
	s3, _, err := c.BackupStorageS3(backupStorage)
	if err != nil {
		return barmanapi.BarmanObjectStoreConfiguration{}, "", fmt.Errorf("decode BackupStorage %q for data deletion: %w", backupStorage.Name, err)
	}
	return config, s3.Region, nil
}

func deletionJobDone(job *batchv1.Job) (bool, error) {
	if jobFailed(job) {
		return false, fmt.Errorf("backup data deletion Job %q failed; inspect its logs, then delete the Job to retry", job.Name)
	}
	return jobComplete(job), nil
}

// buildBackupDeletionJob builds the Job deleting cnpgBackup's data from the
// storage config connects to.
func buildBackupDeletionJob(
	backup *backupv1alpha1.Backup,
	cnpgBackup *cnpgv1.Backup,
	config barmanapi.BarmanObjectStoreConfiguration,
	region string,
) (*batchv1.Job, error) {
	aws := config.AWS
	if aws == nil || aws.AccessKeyIDReference == nil || aws.SecretAccessKeyReference == nil {
		return nil, fmt.Errorf("BackupStorage %q has incomplete S3 credentials; cannot delete Barman backup data", backup.Spec.StorageRef.Name)
	}
	if cnpgBackup.Status.BackupID == "" {
		return nil, fmt.Errorf("CloudNativePG backup %q has no Barman backup ID", cnpgBackup.Name)
	}

	destination := cnpgBackup.Status.DestinationPath
	if destination == "" {
		destination = config.DestinationPath
	}
	serverName := cnpgBackup.Status.ServerName
	if serverName == "" {
		serverName = cnpgBackup.Spec.Cluster.Name
	}
	if destination == "" || serverName == "" {
		return nil, fmt.Errorf("CloudNativePG backup %q is missing its Barman destination or server name", cnpgBackup.Name)
	}
	endpointURL := cnpgBackup.Status.EndpointURL
	if endpointURL == "" {
		endpointURL = config.EndpointURL
	}

	args := make([]string, 0, 8)
	if endpointURL != "" {
		args = append(args, "--endpoint-url", endpointURL)
	}
	args, err := barmancommand.AppendCloudProviderOptionsFromConfiguration(
		context.Background(),
		args,
		&config,
	)
	if err != nil {
		return nil, fmt.Errorf("build Barman delete options for BackupStorage %q: %w", backup.Spec.StorageRef.Name, err)
	}
	args = append(args, "--backup-id", cnpgBackup.Status.BackupID, destination, serverName)

	env := []corev1.EnvVar{
		secretEnv("AWS_ACCESS_KEY_ID", aws.AccessKeyIDReference),
		secretEnv("AWS_SECRET_ACCESS_KEY", aws.SecretAccessKeyReference),
	}
	if aws.RegionReference != nil {
		env = append(env, secretEnv("AWS_REGION", aws.RegionReference))
		env = append(env, secretEnv("AWS_DEFAULT_REGION", aws.RegionReference))
	} else if region != "" {
		env = append(env,
			corev1.EnvVar{Name: "AWS_REGION", Value: region},
			corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: region},
		)
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if config.EndpointCA != nil {
		env = append(env, corev1.EnvVar{Name: "AWS_CA_BUNDLE", Value: backupCAPath})
		volumes = append(volumes, corev1.Volume{
			Name: "endpoint-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: config.EndpointCA.Name,
					Items: []corev1.KeyToPath{{
						Key:  config.EndpointCA.Key,
						Path: "backup-ca.crt",
					}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "endpoint-ca",
			MountPath: backupCAPath,
			SubPath:   "backup-ca.crt",
			ReadOnly:  true,
		})
	}

	backoffLimit := int32(0)
	labels := map[string]string{backupDeletionLabel: "true"}
	if backup.Spec.ScheduleName != "" {
		labels[backupDeletionScheduleKey] = backup.Spec.ScheduleName
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupDeletionJobName(backup.Name),
			Namespace: backup.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					// Compliant with the "restricted" Pod Security Standard; the image runs as UID 26.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:         "delete-backup-data",
						Image:        barman.BackupDeletionImage,
						Command:      []string{"barman-cloud-backup-delete"},
						Args:         args,
						Env:          env,
						VolumeMounts: mounts,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}, nil
}

func secretEnv(name string, ref *machineryapi.SecretKeySelector) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
				Key:                  ref.Key,
			},
		},
	}
}

func backupDeletionJobName(backupName string) string {
	const prefix = "delete-backup-"
	const hashLength = 8
	if len(prefix+backupName) <= 63 {
		return prefix + backupName
	}
	sum := sha256.Sum256([]byte(backupName))
	hash := hex.EncodeToString(sum[:])[:hashLength]
	maxPrefix := 63 - hashLength - 1
	name := prefix + backupName
	return strings.TrimRight(name[:maxPrefix], "-.") + "-" + hash
}

func boolPtr(value bool) *bool { return &value }

func jobComplete(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

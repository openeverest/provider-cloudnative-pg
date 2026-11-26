package provider

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// Reasons of the problems the provider reports. A problem's reason becomes
// the status word users read, so each names one cause.
const (
	// reasonHibernatedOutsideOpenEverest: the cnpg.io/hibernation annotation
	// is set on the Cluster object. An Instance cannot ask for that, so it
	// was set there by someone else.
	reasonHibernatedOutsideOpenEverest = "HibernatedOutsideOpenEverest"
	// reasonUnrecoverable: CloudNativePG gave up on the cluster.
	reasonUnrecoverable = "Unrecoverable"
	// reasonConfigurationRejected: CloudNativePG refuses the Cluster's
	// definition.
	reasonConfigurationRejected = "ConfigurationRejected"
	// reasonImageCatalogInvalid: CloudNativePG cannot resolve an image from
	// an image catalog.
	reasonImageCatalogInvalid = "ImageCatalogInvalid"
	// reasonPluginFailed: a CloudNativePG plugin, such as the Barman Cloud
	// Plugin, is missing or fails.
	reasonPluginFailed = "PluginFailed"
	// reasonClusterObjectsNotCreated: CloudNativePG cannot create the objects
	// the cluster needs.
	reasonClusterObjectsNotCreated = "ClusterObjectsNotCreated"
	// reasonManagedRoleFailed: CloudNativePG cannot reconcile a managed role.
	reasonManagedRoleFailed = "ManagedRoleFailed"
	// reasonPoolerFailed: CloudNativePG reports a Pooler as failed.
	reasonPoolerFailed = "PoolerFailed"
)

// Reasons explaining why clients cannot connect, or why that is unknown.
const (
	reasonHibernated           = "Hibernated"
	reasonNoPrimary            = "NoPrimary"
	reasonSwitchingPrimary     = "SwitchingPrimary"
	reasonPrimaryNotReady      = "PrimaryNotReady"
	reasonInstancesNotReported = "InstancesNotReported"
	reasonNoPgBouncerReady     = "NoPgBouncerReady"
)

// Reasons of the operations the provider reports.
const (
	reasonRollingOutImage       = "RollingOutImage"
	reasonUpgradingMajorVersion = "UpgradingMajorVersion"
	reasonResizingVolumes       = "ResizingVolumes"
)

// rolloutImageFragment is how CloudNativePG words, in status.phaseReason, the
// restart of an instance that runs another image than the cluster's.
const rolloutImageFragment = "is using a different image"

// clusterFailure names a CloudNativePG phase that stops reconciliation.
type clusterFailure struct {
	reason string
	// terminal is set when nothing but a person can resolve the failure.
	terminal bool
}

// clusterFailures are the CloudNativePG phases that stop reconciliation until
// something changes. Phases of work in progress (switchover, failover,
// upgrade, replica creation) are not failures.
//
// Only an unrecoverable cluster is terminal. CloudNativePG does not say which
// generation of the Cluster its status is about, so right after a fix is
// applied the failure still showing may be the one being fixed, and the
// Instance must not read as stalled then.
var clusterFailures = map[string]clusterFailure{
	cnpgv1.PhaseUnrecoverable:              {reason: reasonUnrecoverable, terminal: true},
	cnpgv1.PhaseDefinitionInvalid:          {reason: reasonConfigurationRejected},
	cnpgv1.PhaseImageCatalogError:          {reason: reasonImageCatalogInvalid},
	cnpgv1.PhaseFailurePlugin:              {reason: reasonPluginFailed},
	cnpgv1.PhaseUnknownPlugin:              {reason: reasonPluginFailed},
	cnpgv1.PhaseCannotCreateClusterObjects: {reason: reasonClusterObjectsNotCreated},
	cnpgv1.PhaseArchitectureBinaryMissing:  {reason: corev1alpha1.ReasonEngineError},
}

// Status reports what the provider observes about the cluster: whether it
// runs what the Instance asks for, whether clients can connect, what is wrong
// and which long operation is running, and how many pods each component will
// have. The runtime derives the Instance's phase and conditions from these
// facts.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	pg, err := uncachedCluster(c)
	if apierrors.IsNotFound(err) {
		return withDesiredReplicas(c, controller.Progressing("Waiting for the CloudNativePG Cluster object to be created")), nil
	}
	if err != nil {
		return controller.Status{}, err
	}

	status := observeCluster(pg, seedingMessage(c))

	failure, waiting := cnpg.ManagedRoles(pg)
	if failure != "" {
		status = status.WithProblem(reasonManagedRoleFailed, failure)
	}
	status.Message = cmp.Or(status.Message, failure, waiting)

	// The extensions are created in a running cluster, so they are looked at
	// only once nothing else is waited for.
	if status.Message == "" {
		engine := c.Instance().Spec.Components[common.ComponentEngine]
		var custom components.CNPGCustomSpec
		if c.TryDecodeComponentParameters(engine, &custom) {
			if err := c.DecodeComponentParameters(engine, &custom); err != nil {
				return controller.Status{}, fmt.Errorf("failed to decode component custom spec: %w", err)
			}
		}
		// Validate reports an invalid configuration.
		_ = cnpg.ApplyConfiguration(&custom)
		status.Message = cnpg.ExtensionsWait(c, &custom)
	}

	poolers, err := observePoolers(c)
	if err != nil {
		return controller.Status{}, err
	}
	if poolers != nil {
		if poolers.failure != "" {
			status = status.WithProblem(reasonPoolerFailed, poolers.failure)
		}
		status.Message = cmp.Or(status.Message, poolers.failure, poolers.wait)
		// Clients connect through PgBouncer when the Instance has a proxy.
		if status.Available == controller.Available && !poolers.serving {
			status = status.WithUnavailable(reasonNoPgBouncerReady, "No PgBouncer pod is ready")
		}
	}

	// The connection details are reported as soon as clients can connect,
	// not only once the cluster has converged: whoever is told the database
	// is available must find where to connect, and the details must follow a
	// change that is still rolling out.
	if status.Message == "" || status.Available == controller.Available {
		if details, ok := connectionDetails(c, pg); ok {
			if poolers != nil {
				poolers.route(c, &details)
			}
			status.ConnectionDetails = details
		} else {
			status.Message = cmp.Or(status.Message, "Waiting for the CloudNativePG credentials Secret")
		}
	}

	status.Converged = status.Message == ""
	return withDesiredReplicas(c, status), nil
}

// uncachedCluster reads the Cluster object from the API server instead of the
// cache. Sync has just applied it, and the cache may still hold the object
// from before that: its status would then be compared with a spec that is no
// longer the latest, and the Instance would read as ready for a change
// CloudNativePG has not seen. The client reads unstructured objects without
// the cache.
func uncachedCluster(c *controller.Context) (*cnpgv1.Cluster, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cnpgv1.SchemeGroupVersion.WithKind(cnpgv1.ClusterKind))
	if err := c.Get(obj, c.Name()); err != nil {
		return nil, fmt.Errorf("get CloudNativePG Cluster %q: %w", c.Name(), err)
	}
	pg := &cnpgv1.Cluster{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, pg); err != nil {
		return nil, fmt.Errorf("decode CloudNativePG Cluster %q: %w", c.Name(), err)
	}
	return pg, nil
}

// seedingMessage says what a new Instance that is still being seeded from its
// data source waits for, or "" when it is not. The runtime reports the
// seeding itself, as a restore during which clients cannot connect.
func seedingMessage(c *controller.Context) string {
	ds := c.Instance().Spec.DataSource
	if ds == nil {
		return ""
	}
	if s := c.GetDataSourceStatus(); s != nil && s.Done {
		return ""
	}
	switch ds.Type {
	case backupv1alpha1.DataSourceTypePointInTime:
		return "Recovering the data source to a point in time"
	case backupv1alpha1.DataSourceTypeBackup:
		return "Restoring the backup of the data source"
	default:
		return "Restoring the data source"
	}
}

// observeCluster turns the Cluster object into the facts the provider reports
// about it. seeding says what a new Instance still being seeded waits for.
// The caller adds what the roles, extensions and poolers report, and decides
// whether the Instance has converged.
func observeCluster(pg *cnpgv1.Cluster, seeding string) controller.Status {
	var status controller.Status
	status.Available, status.AvailableReason, status.AvailableMessage = availability(pg)

	if hibernated(pg) {
		// Not terminal: tools hibernate a cluster for a short while, for
		// example to swap its volumes, and wake it up themselves.
		status = status.WithProblem(reasonHibernatedOutsideOpenEverest,
			"The cluster is hibernated: the "+utils.HibernationAnnotationName+
				" annotation is set on its CloudNativePG Cluster object, outside OpenEverest. Remove it to start the database again.")
	}

	if failure, failing := clusterFailures[pg.Status.Phase]; failing {
		if failure.terminal {
			status = status.WithTerminalProblem(failure.reason, phaseMessage(pg))
		} else {
			status = status.WithProblem(failure.reason, phaseMessage(pg))
		}
	}

	status.Message = clusterWait(pg, seeding)
	if status.Message == "" {
		return status
	}

	if reason, message, upgrading := upgrade(pg); upgrading {
		status = status.WithOperation(corev1alpha1.ConditionUpgrading, reason, message)
	}
	if volumes := pg.Status.ResizingPVC; len(volumes) > 0 {
		status = status.WithOperation(corev1alpha1.ConditionStorageResizing, reasonResizingVolumes,
			"Resizing volumes "+strings.Join(volumes, ", "))
	}
	return status
}

// hibernated reports whether the Cluster is asked to hibernate. The provider
// never sets the annotation and does not own it, so its Sync leaves it alone.
func hibernated(pg *cnpgv1.Cluster) bool {
	return pg.Annotations[utils.HibernationAnnotationName] == string(utils.HibernationAnnotationValueOn)
}

// phaseMessage is the phase CloudNativePG reports, with its reason.
func phaseMessage(pg *cnpgv1.Cluster) string {
	if pg.Status.PhaseReason == "" {
		return pg.Status.Phase
	}
	return pg.Status.Phase + ": " + pg.Status.PhaseReason
}

// clusterWait says what keeps the cluster from running what its Cluster
// object asks for, in CloudNativePG's own words where it has any. It returns
// "" when nothing does.
//
// CloudNativePG does not record which generation of the Cluster its status
// describes. What the status can be checked against is: the image and the
// number of instances. A change to anything else reads as carried out until
// CloudNativePG reacts to it.
func clusterWait(pg *cnpgv1.Cluster, seeding string) string {
	st := pg.Status
	ready := meta.FindStatusCondition(st.Conditions, string(cnpgv1.ConditionClusterReady))
	_, failing := clusterFailures[st.Phase]
	switch {
	case hibernated(pg):
		return "The cluster is hibernated on its CloudNativePG Cluster object"
	case seeding != "":
		return seeding
	case failing:
		return "CloudNativePG reports: " + phaseMessage(pg)
	case !pg.IsInitialized():
		return initializingMessage(pg)
	case pg.Spec.ImageName != "" && st.Image != pg.Spec.ImageName:
		return "Waiting for CloudNativePG to roll out image " + pg.Spec.ImageName
	case st.Instances != pg.Spec.Instances:
		return fmt.Sprintf("Waiting for CloudNativePG to scale to %d instances (it has %d)", pg.Spec.Instances, st.Instances)
	case st.ReadyInstances < st.Instances:
		return fmt.Sprintf("Waiting for the instances to become ready (%d of %d ready)", st.ReadyInstances, st.Instances)
	case st.Phase != "" && st.Phase != cnpgv1.PhaseHealthy:
		return "Waiting for CloudNativePG: " + phaseMessage(pg)
	case ready == nil || ready.Status != metav1.ConditionTrue:
		return "Waiting for CloudNativePG to report the cluster ready"
	case len(st.ResizingPVC) > 0:
		return "Waiting for volumes to be resized: " + strings.Join(st.ResizingPVC, ", ")
	}
	return ""
}

func initializingMessage(pg *cnpgv1.Cluster) string {
	if c := meta.FindStatusCondition(pg.Status.Conditions, string(cnpgv1.ConditionInitialized)); c != nil {
		if c.Message != "" {
			return c.Message
		}
		if c.Reason != "" {
			return c.Reason
		}
	}
	return "Waiting for CloudNativePG to initialize the cluster"
}

// upgrade says whether a new engine image is rolling out, or about to.
//
// A major version upgrade shows in the status: the data directory is still on
// the previous image. A minor one is inferred, since CloudNativePG records the
// image as the cluster's before it restarts the first instance: from an image
// it has not picked up yet, and from the reason it gives for a restart.
func upgrade(pg *cnpgv1.Cluster) (reason, message string, upgrading bool) {
	st := pg.Status
	switch {
	case st.Phase == cnpgv1.PhaseMajorUpgrade,
		st.PGDataImageInfo != nil && st.Image != "" && st.PGDataImageInfo.Image != st.Image:
		return reasonUpgradingMajorVersion, "Upgrading the PostgreSQL major version to image " + st.Image, true
	case pg.Spec.ImageName != "" && st.Image != "" && st.Image != pg.Spec.ImageName:
		return reasonRollingOutImage, fmt.Sprintf("Rolling out image %s (the cluster was running %s)", pg.Spec.ImageName, st.Image), true
	case strings.Contains(st.PhaseReason, rolloutImageFragment):
		return reasonRollingOutImage, "Rolling out image " + st.Image, true
	}
	return "", "", false
}

// availability says whether clients can connect: the read-write Service
// routes to the primary, so there must be one, it must not be changing, and
// it must be ready.
//
// It reads what CloudNativePG recorded on its last pass over the cluster,
// which makes the answer as old as that pass. A cluster whose instances
// CloudNativePG has not reported on is unknown, never guessed from counts:
// ready pods do not make a primary.
func availability(pg *cnpgv1.Cluster) (controller.Availability, string, string) {
	primary, target := pg.Status.CurrentPrimary, pg.Status.TargetPrimary
	switch {
	case hibernated(pg):
		return controller.Unavailable, reasonHibernated, "The cluster is hibernated"
	case primary == "":
		return controller.Unavailable, reasonNoPrimary, "The cluster has no primary yet"
	case target == cnpgv1.PendingFailoverMarker:
		return controller.Unavailable, reasonNoPrimary,
			fmt.Sprintf("CloudNativePG is failing over from %s and has not elected a new primary yet", primary)
	case target != "" && target != primary:
		return controller.Unavailable, reasonSwitchingPrimary,
			fmt.Sprintf("The primary is changing from %s to %s", primary, target)
	case len(pg.Status.InstancesStatus) == 0:
		return controller.AvailabilityUnknown, reasonInstancesNotReported,
			"CloudNativePG has not reported the state of the instances"
	case !slices.Contains(pg.Status.InstancesStatus[cnpgv1.PodHealthy], primary):
		return controller.Unavailable, reasonPrimaryNotReady, fmt.Sprintf("The primary %s is not ready", primary)
	}
	return controller.Available, "", ""
}

// withDesiredReplicas reports how many pods each component will have once the
// cluster runs what the Instance asks for. The counts come from the Instance,
// with the defaults Sync applies, not from the CloudNativePG objects, which
// may still describe an earlier spec. A hibernated cluster keeps them: the
// Instance does not ask for the hibernation.
func withDesiredReplicas(c *controller.Context, status controller.Status) controller.Status {
	specs := c.Instance().Spec.Components
	if engine, ok := specs[common.ComponentEngine]; ok && engine.Replicas != nil {
		status = status.WithDesiredReplicas(common.ComponentEngine, *engine.Replicas)
	}
	if proxy, ok := specs[common.ComponentProxy]; ok {
		status = status.WithDesiredReplicas(common.ComponentProxy, proxyPods(proxy))
	}
	return status
}

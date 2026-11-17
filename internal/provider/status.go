package provider

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

// Status computes the current status of the database instance.
//
// Query the operator's resource(s) and translate their status
// into the provider-runtime's Status type.
func (p *Provider) Status(c *controller.Context) (controller.Status, error) {
	l := log.FromContext(c.Context())
	l.Info("Computing status", "name", c.Name())

	if ds := c.Instance().Spec.DataSource; ds != nil {
		if s := c.GetDataSourceStatus(); s == nil || !s.Done {
			switch ds.Type {
			case backupv1alpha1.DataSourceTypePointInTime:
				return controller.Restoring("point-in-time recovery in progress"), nil
			case backupv1alpha1.DataSourceTypeBackup:
				return controller.Restoring("restoring from backup"), nil
			default:
				return controller.Restoring("restore in progress"), nil
			}
		}
	}

	pg := &cnpgv1.Cluster{}
	if err := c.Get(pg, c.Name()); err != nil {
		return controller.Pending("Waiting to get CloudNativePG cluster resource"), nil
	}

	// CNPG 1.30 encodes reconcile-time admission/validation failures as
	// PhaseDefinitionInvalid (via SetAdmissionError) with the detail in
	// PhaseReason. Other terminal phases are mapped the same way.
	if status, failed := failedClusterStatus(pg); failed {
		return status, nil
	}

	readyCondition := meta.FindStatusCondition(pg.Status.Conditions, string(cnpgv1.ConditionClusterReady))
	if readyCondition != nil && readyCondition.Status == metav1.ConditionTrue && pg.Status.Instances > 0 && pg.Status.ReadyInstances == pg.Status.Instances {
		if roleStatus, blocked := cnpg.ManagedRolesStatus(pg); blocked {
			return roleStatus, nil
		}

		engine := c.Instance().Spec.Components[common.ComponentEngine]
		var custom components.CNPGCustomSpec
		if c.TryDecodeComponentParameters(engine, &custom) {
			if err := c.DecodeComponentParameters(engine, &custom); err != nil {
				return controller.Provisioning(fmt.Sprintf("failed to decode component custom spec: %v", err)), nil
			}
		}
		if extStatus, blocked := cnpg.ExtensionsStatus(c, &custom); blocked {
			return extStatus, nil
		}

		details, ok := connectionDetails(c, pg)
		if !ok {
			return controller.Provisioning("waiting to get CloudNativePG credentials secret"), nil
		}
		status, blocked, err := poolerBlocksReady(c, &details)
		if err != nil {
			return controller.Status{}, err
		}
		if blocked {
			return status, nil
		}
		return controller.ReadyWithConnectionDetails(details), nil
	}

	// Prefer CNPG's Initialized condition (with LatestGeneratedNode fallback for
	// clusters upgraded from pre-1.30) over an Instances==0 heuristic.
	if !pg.IsInitialized() {
		return controller.Initializing(initializingMessage(pg)), nil
	}

	if pg.Status.Instances > 0 {
		return controller.Provisioning(fmt.Sprintf(
			"waiting for CloudNativePG cluster to be ready (%d/%d instances ready)",
			pg.Status.ReadyInstances,
			pg.Status.Instances,
		)), nil
	}

	return controller.Provisioning("waiting for CloudNativePG cluster to be ready"), nil
}

// failedClusterStatus maps CNPG phases that stop reconciliation until someone
// intervenes. In-progress phases (switchover, failover, upgrade, replica
// creation, supervised switchover) are left to the caller.
//
// PhaseDefinitionInvalid is how CNPG 1.30 surfaces reconcile-time validation
// failures on the Cluster status.
func failedClusterStatus(pg *cnpgv1.Cluster) (controller.Status, bool) {
	switch pg.Status.Phase {
	case cnpgv1.PhaseUnrecoverable,
		cnpgv1.PhaseFailurePlugin,
		cnpgv1.PhaseUnknownPlugin,
		cnpgv1.PhaseCannotCreateClusterObjects,
		cnpgv1.PhaseImageCatalogError,
		cnpgv1.PhaseArchitectureBinaryMissing,
		cnpgv1.PhaseDefinitionInvalid:
		message := pg.Status.Phase
		if pg.Status.PhaseReason != "" {
			message += ": " + pg.Status.PhaseReason
		}
		return controller.Failed(message), true
	default:
		return controller.Status{}, false
	}
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
	return "waiting for CloudNativePG cluster to initialize"
}

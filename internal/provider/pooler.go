package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	pgbouncerconfig "github.com/cloudnative-pg/cloudnative-pg/pkg/management/pgbouncer/config"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

const (
	// poolerNameSuffix keeps the Pooler name different from its Cluster.
	// The operator skips reconciliation when a Cluster of that name exists, and
	// its webhook rejects a Pooler named after the Cluster it references.
	// The Pooler's Service uses this same name. The operator's auth Secret is
	// also "<cluster>-pooler", which is a different kind and does not clash.
	poolerNameSuffix = "-pooler"

	// poolerRONameSuffix names the optional read-only Pooler. It must stay
	// distinct from the rw Pooler and from the Cluster.
	poolerRONameSuffix = "-pooler-ro"

	// pgbouncerContainerName is the container the operator's deployment builder
	// looks up when it fills in image, command, and ports. Resources set on it
	// are kept.
	pgbouncerContainerName = "pgbouncer"

	// pgbouncerClientTLSSSLMode is the PgBouncer setting for client-side TLS.
	// The operator defaults it to prefer, which accepts plaintext clients.
	pgbouncerClientTLSSSLMode = "client_tls_sslmode"
)

func poolerName(instance string) string {
	return instance + poolerNameSuffix
}

func poolerROName(instance string) string {
	return instance + poolerRONameSuffix
}

func proxyParameters(proxy corev1alpha1.ComponentSpec) (components.PgBouncerParameters, error) {
	var params components.PgBouncerParameters
	if proxy.Parameters == nil || len(proxy.Parameters.Raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(proxy.Parameters.Raw, &params); err != nil {
		return params, fmt.Errorf("failed to decode proxy parameters: %w", err)
	}
	return params, nil
}

func validateProxy(instance *corev1alpha1.Instance) error {
	proxy, ok := instance.Spec.Components[common.ComponentProxy]
	if !ok {
		return nil
	}
	if proxy.Type != "" && proxy.Type != common.ComponentTypePgbouncer {
		return fmt.Errorf("proxy type %q is not supported: use %q", proxy.Type, common.ComponentTypePgbouncer)
	}
	if errs := validation.IsDNS1035Label(poolerName(instance.Name)); len(errs) > 0 {
		return fmt.Errorf("proxy name %q is not a valid Service name: %s", poolerName(instance.Name), errs[0])
	}
	if proxy.Replicas != nil && *proxy.Replicas < 1 {
		return fmt.Errorf("proxy replicas must be at least 1")
	}
	params, err := proxyParameters(proxy)
	if err != nil {
		return err
	}
	switch cnpgv1.PgBouncerPoolMode(params.PoolMode) {
	case "", cnpgv1.PgBouncerPoolModeSession, cnpgv1.PgBouncerPoolModeTransaction:
	default:
		return fmt.Errorf("proxy poolMode %q is invalid: CloudNativePG accepts %q or %q",
			params.PoolMode, cnpgv1.PgBouncerPoolModeSession, cnpgv1.PgBouncerPoolModeTransaction)
	}
	if params.ReadOnly {
		if errs := validation.IsDNS1035Label(poolerROName(instance.Name)); len(errs) > 0 {
			return fmt.Errorf("read-only proxy name %q is not a valid Service name: %s",
				poolerROName(instance.Name), errs[0])
		}
		engine := instance.Spec.Components[common.ComponentEngine]
		if engine.Replicas == nil || *engine.Replicas < 2 {
			return fmt.Errorf("proxy readOnly requires at least 2 engine replicas")
		}
	}
	// Resources are optional for API clients (CNPG defaults apply), but when set
	// they must pass the same checks as the engine.
	if proxy.Resources != nil {
		if err := cnpg.ValidateResources(proxy.Resources); err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
	}
	return nil
}

// syncPooler creates the rw Pooler for the proxy component, and optionally a
// ro Pooler when parameters.readOnly is set. Removing the component deletes
// both; turning readOnly off deletes only the ro Pooler. The operator does not
// delete a Pooler with its Cluster. With requireTLS each pooler refuses
// plaintext clients too, since clients reach PostgreSQL through them.
func syncPooler(c *controller.Context, requireTLS bool) error {
	proxy, ok := c.Instance().Spec.Components[common.ComponentProxy]
	if !ok {
		if err := deletePooler(c, poolerName(c.Name())); err != nil {
			return err
		}
		return deletePooler(c, poolerROName(c.Name()))
	}
	params, err := proxyParameters(proxy)
	if err != nil {
		return err
	}
	if err := c.Apply(buildPooler(c, proxy, params, requireTLS, poolerName(c.Name()), cnpgv1.PoolerTypeRW)); err != nil {
		return err
	}
	if !params.ReadOnly {
		return deletePooler(c, poolerROName(c.Name()))
	}
	return c.Apply(buildPooler(c, proxy, params, requireTLS, poolerROName(c.Name()), cnpgv1.PoolerTypeRO))
}

func deletePooler(c *controller.Context, name string) error {
	exists, err := c.Exists(&cnpgv1.Pooler{}, name)
	if err != nil || !exists {
		return err
	}
	return c.Delete(&cnpgv1.Pooler{ObjectMeta: c.ObjectMeta(name)})
}

func buildPooler(
	c *controller.Context,
	proxy corev1alpha1.ComponentSpec,
	params components.PgBouncerParameters,
	requireTLS bool,
	name string,
	poolerType cnpgv1.PoolerType,
) *cnpgv1.Pooler {
	instances := int32(1)
	if proxy.Replicas != nil {
		instances = *proxy.Replicas
	}
	pooler := &cnpgv1.Pooler{
		ObjectMeta: c.ObjectMeta(name),
		Spec: cnpgv1.PoolerSpec{
			Cluster:   cnpgv1.LocalObjectReference{Name: c.Name()},
			Type:      poolerType,
			Instances: &instances,
			// Leave authQuery unset so the operator keeps its automated integration
			// and creates the cnpg_pooler_pgbouncer role itself.
			PgBouncer: &cnpgv1.PgBouncerSpec{PoolMode: cnpgv1.PgBouncerPoolMode(params.PoolMode)},
			Template: &cnpgv1.PodTemplateSpec{
				ObjectMeta: cnpgv1.Metadata{Labels: c.PodLabels(common.ComponentProxy)},
			},
		},
	}
	if requireTLS {
		pooler.Spec.PgBouncer.Parameters = map[string]string{pgbouncerClientTLSSSLMode: "require"}
	}
	if proxy.Resources != nil {
		// The operator requires containers when template.spec is set, and keeps
		// resources placed on the container it names pgbouncer.
		pooler.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:      pgbouncerContainerName,
			Resources: *proxy.Resources,
		}}
	}
	return pooler
}

// poolerBlocksReady keeps the Instance from becoming Ready until the Pooler(s)
// are serving, points the primary connection at the rw pooler's Service, and
// when readOnly is set points host-ro at the ro pooler's Service.
//
// The operator sets phase Active once the Pooler's prerequisites exist, and its
// status.instances counts pods created, not pods ready. Readiness therefore
// comes from the Deployment the operator names after the Pooler: its Service
// only routes to ready pods. Without readOnly, host-r and host-ro stay on the
// PostgreSQL Services.
func poolerBlocksReady(c *controller.Context, details *controller.ConnectionDetails) (controller.Status, bool, error) {
	proxy, ok := c.Instance().Spec.Components[common.ComponentProxy]
	if !ok {
		return controller.Status{}, false, nil
	}

	status, blocked, err := waitPoolerReady(c, poolerName(c.Name()))
	if err != nil || blocked {
		return status, blocked, err
	}

	// listen_port is forced to PgBouncerPort. The credential Secret's port is
	// the PostgreSQL port, which is not where this Service listens.
	host := serviceHost(poolerName(c.Name()), c.Namespace())
	port := strconv.Itoa(pgbouncerconfig.PgBouncerPort)
	details.Host = host
	details.Port = port
	details.URI = postgresURI(details.Username, details.Password, host, port,
		details.AdditionalProperties["database"], details.AdditionalProperties["sslmode"])

	params, err := proxyParameters(proxy)
	if err != nil {
		return controller.Status{}, true, err
	}
	if !params.ReadOnly {
		return controller.Status{}, false, nil
	}

	status, blocked, err = waitPoolerReady(c, poolerROName(c.Name()))
	if err != nil || blocked {
		return status, blocked, err
	}
	if details.AdditionalProperties == nil {
		details.AdditionalProperties = map[string]string{}
	}
	details.AdditionalProperties[hostROProperty] = serviceHost(poolerROName(c.Name()), c.Namespace())
	return controller.Status{}, false, nil
}

func waitPoolerReady(c *controller.Context, name string) (controller.Status, bool, error) {
	pooler := &cnpgv1.Pooler{}
	if err := c.Get(pooler, name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.Provisioning("waiting for the CloudNativePG pooler"), true, nil
		}
		return controller.Status{}, true, fmt.Errorf("failed to get the CloudNativePG pooler: %w", err)
	}
	if pooler.Status.Phase == cnpgv1.PoolerPhaseFailed {
		message := string(pooler.Status.Phase)
		if pooler.Status.PhaseReason != "" {
			message += ": " + pooler.Status.PhaseReason
		}
		return controller.Failed(message), true, nil
	}
	if pooler.Status.Phase != cnpgv1.PoolerPhaseActive {
		message := "waiting for the CloudNativePG pooler"
		if pooler.Status.Phase != "" {
			message = fmt.Sprintf("%s (%s)", message, pooler.Status.Phase)
		}
		if pooler.Status.PhaseReason != "" {
			message += ": " + pooler.Status.PhaseReason
		}
		return controller.Provisioning(message), true, nil
	}

	want := int32(1)
	if pooler.Spec.Instances != nil {
		want = *pooler.Spec.Instances
	}
	deployment := &appsv1.Deployment{}
	if err := c.Get(deployment, name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.Provisioning("waiting for the PgBouncer deployment"), true, nil
		}
		return controller.Status{}, true, fmt.Errorf("failed to get the PgBouncer deployment: %w", err)
	}
	if deployment.Status.ReadyReplicas < want {
		return controller.Provisioning(fmt.Sprintf("waiting for PgBouncer pods (%d/%d ready)",
			deployment.Status.ReadyReplicas, want)), true, nil
	}
	return controller.Status{}, false, nil
}

// instanceOfPoolerDeployment maps the PgBouncer Deployment the operator creates
// for a Pooler to its Instance, so a change in ready pods re-evaluates Status.
// The Deployment is owned by the Pooler, not the Instance, and its status can
// trail the pod events the runtime already watches.
func instanceOfPoolerDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[utils.PodRoleLabelName] != string(utils.PodRolePooler) {
		return nil
	}
	cluster := labels[utils.ClusterLabelName]
	if cluster == "" {
		return nil
	}
	name := obj.GetName()
	if name != poolerName(cluster) && name != poolerROName(cluster) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: cluster}}}
}

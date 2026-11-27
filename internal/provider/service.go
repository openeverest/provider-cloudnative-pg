package provider

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	cnpgpostgres "github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

var supportedServiceTypes = []corev1.ServiceType{
	corev1.ServiceTypeClusterIP,
	corev1.ServiceTypeNodePort,
	corev1.ServiceTypeLoadBalancer,
}

func exposedServiceName(clusterName string) string {
	return clusterName + "-rw-lb"
}

func serviceExposes(svc *corev1alpha1.Service) bool {
	if svc == nil {
		return false
	}
	switch svc.ServiceType {
	case corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort:
		return true
	default:
		return false
	}
}

// applyService maps engine.service onto CloudNativePG managed.services.additional
// with selectorType rw. ClusterIP (the default) needs no additional Service.
func applyService(pg *cnpgv1.Cluster, svc *corev1alpha1.Service) {
	if !serviceExposes(svc) {
		return
	}

	template := cnpgv1.ServiceTemplateSpec{
		ObjectMeta: cnpgv1.Metadata{
			Name:        exposedServiceName(pg.Name),
			Annotations: svc.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type: svc.ServiceType,
		},
	}
	if svc.ServiceType == corev1.ServiceTypeLoadBalancer && svc.LoadBalancerService != nil {
		if ranges := svc.LoadBalancerService.SourceRanges.NormalizedSourceRanges(); len(ranges) > 0 {
			template.Spec.LoadBalancerSourceRanges = ranges
		}
	}

	if pg.Spec.Managed == nil {
		pg.Spec.Managed = &cnpgv1.ManagedConfiguration{}
	}
	if pg.Spec.Managed.Services == nil {
		pg.Spec.Managed.Services = &cnpgv1.ManagedServices{}
	}
	pg.Spec.Managed.Services.Additional = []cnpgv1.ManagedService{{
		SelectorType:   cnpgv1.ServiceSelectorTypeRW,
		UpdateStrategy: cnpgv1.ServiceUpdateStrategyPatch,
		ServiceTemplate: template,
	}}
}

func validateService(instanceName string, svc *corev1alpha1.Service, custom *components.CNPGCustomSpec, hasProxy bool) (field.ErrorList, []string) {
	path := enginePath.Child("service")
	var errs field.ErrorList
	var warnings []string

	if svc != nil && svc.ServiceType != "" && !slices.Contains(supportedServiceTypes, svc.ServiceType) {
		supported := make([]string, 0, len(supportedServiceTypes))
		for _, t := range supportedServiceTypes {
			supported = append(supported, string(t))
		}
		errs = append(errs, field.NotSupported(path.Child("serviceType"), svc.ServiceType, supported))
	}

	if svc != nil && svc.LoadBalancerService != nil && len(svc.LoadBalancerService.SourceRanges) > 0 &&
		svc.ServiceType != corev1.ServiceTypeLoadBalancer {
		errs = append(errs, field.Forbidden(path.Child("loadBalancerService"),
			"loadBalancerService is only valid when serviceType is LoadBalancer"))
	}

	if svc != nil && svc.ServiceType == corev1.ServiceTypeLoadBalancer && svc.LoadBalancerService != nil {
		rangesPath := path.Child("loadBalancerService", "sourceRanges")
		for i, r := range svc.LoadBalancerService.SourceRanges {
			if _, _, err := net.ParseCIDR(r); err == nil {
				continue
			}
			if net.ParseIP(r) != nil {
				continue
			}
			errs = append(errs, field.Invalid(rangesPath.Index(i), r, "must be a CIDR or IP address"))
		}
	}

	if serviceExposes(svc) {
		name := exposedServiceName(instanceName)
		if dnsErrs := validation.IsDNS1035Label(name); len(dnsErrs) > 0 {
			errs = append(errs, field.Invalid(path, name,
				fmt.Sprintf("managed Service name is not a valid DNS label: %s", strings.Join(dnsErrs, "; "))))
		}
	}

	if serviceExposes(svc) && custom != nil && custom.Managed != nil && custom.Managed.Services != nil &&
		len(custom.Managed.Services.Additional) > 0 {
		errs = append(errs, field.Forbidden(engineParams.Child("managed", "services", "additional"),
			"cannot be combined with engine.service LoadBalancer or NodePort; use one or the other"))
	}

	// CNPG managed.services selectorType rw targets the primary, not the
	// Pooler (Pooler uses its own serviceTemplate). Reject so clients cannot
	// bypass PgBouncer; pooled external access belongs on proxy.service later.
	if serviceExposes(svc) && hasProxy {
		errs = append(errs, field.Forbidden(path,
			"cannot be combined with a proxy; engine.service exposes the PostgreSQL primary and bypasses PgBouncer"))
	}

	if svc != nil && svc.ServiceType == corev1.ServiceTypeLoadBalancer {
		hasRanges := svc.LoadBalancerService != nil && len(svc.LoadBalancerService.SourceRanges) > 0
		if !hasRanges {
			warnings = append(warnings, path.Child("loadBalancerService", "sourceRanges").String()+
				": LoadBalancer with no sourceRanges admits traffic from anywhere (0.0.0.0/0); "+
				"set CIDRs or IPs to restrict clients")
		}
	}

	return errs, warnings
}

func applyServiceExposure(c *controller.Context, svc *corev1alpha1.Service, details *controller.ConnectionDetails) (pendingMsg string, err error) {
	if svc == nil {
		return "", nil
	}
	switch svc.ServiceType {
	case corev1.ServiceTypeLoadBalancer:
		host, pending, err := loadBalancerHost(c)
		if err != nil || pending {
			if pending {
				return "waiting for the load balancer address", err
			}
			return "", err
		}
		details.Host = host
		details.URI = postgresURI(details.Username, details.Password, host, details.Port,
			details.AdditionalProperties["database"], details.AdditionalProperties["sslmode"])
		if details.AdditionalProperties == nil {
			details.AdditionalProperties = map[string]string{}
		}
		details.AdditionalProperties[hostLBProperty] = serviceHost(exposedServiceName(c.Name()), c.Namespace())
		return "", nil
	case corev1.ServiceTypeNodePort:
		nodePort, pending, err := nodePortAllocation(c)
		if err != nil || pending {
			if pending {
				return "waiting for the node port", err
			}
			return "", err
		}
		if details.AdditionalProperties == nil {
			details.AdditionalProperties = map[string]string{}
		}
		details.AdditionalProperties[hostLBProperty] = serviceHost(exposedServiceName(c.Name()), c.Namespace())
		details.AdditionalProperties[nodePortProperty] = strconv.Itoa(int(nodePort))
		return "", nil
	default:
		return "", nil
	}
}

func loadBalancerHost(c *controller.Context) (host string, pending bool, err error) {
	exposed, err := getExposedService(c)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", true, nil
		}
		return "", false, err
	}
	for _, ingress := range exposed.Status.LoadBalancer.Ingress {
		if ingress.Hostname != "" {
			return ingress.Hostname, false, nil
		}
		if ingress.IP != "" {
			return ingress.IP, false, nil
		}
	}
	return "", true, nil
}

func nodePortAllocation(c *controller.Context) (nodePort int32, pending bool, err error) {
	exposed, err := getExposedService(c)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return 0, true, nil
		}
		return 0, false, err
	}
	if port := postgresNodePort(exposed); port != 0 {
		return port, false, nil
	}
	return 0, true, nil
}

func getExposedService(c *controller.Context) (*corev1.Service, error) {
	exposed := &corev1.Service{}
	if err := c.Get(exposed, exposedServiceName(c.Name())); err != nil {
		return nil, fmt.Errorf("get exposed service: %w", err)
	}
	return exposed, nil
}

// postgresNodePort returns the NodePort for the PostgreSQL service port, or the
// first allocated NodePort if the postgres port is not listed yet.
func postgresNodePort(svc *corev1.Service) int32 {
	var fallback int32
	for _, p := range svc.Spec.Ports {
		if p.NodePort == 0 {
			continue
		}
		if p.Port == cnpgpostgres.ServerPort {
			return p.NodePort
		}
		if fallback == 0 {
			fallback = p.NodePort
		}
	}
	return fallback
}

// instanceOfManagedService maps a CloudNativePG managed Service to its Instance
// so Status re-runs when the load balancer address is assigned.
func instanceOfManagedService(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[utils.IsManagedLabelName] != "true" {
		return nil
	}
	cluster := labels[utils.ClusterLabelName]
	if cluster == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: cluster, Namespace: obj.GetNamespace()},
	}}
}

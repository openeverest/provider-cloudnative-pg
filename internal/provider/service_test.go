package provider

import (
	"context"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

func TestApplyService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		svc  *corev1alpha1.Service
		want *cnpgv1.ManagedService
	}{
		{name: "nil service"},
		{name: "empty service", svc: &corev1alpha1.Service{}},
		{name: "ClusterIP is a no-op", svc: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP}},
		{
			name: "LoadBalancer with annotations and source ranges",
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				Annotations: map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": "nlb"},
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8", "192.168.1.1"},
				},
			},
			want: &cnpgv1.ManagedService{
				SelectorType:   cnpgv1.ServiceSelectorTypeRW,
				UpdateStrategy: cnpgv1.ServiceUpdateStrategyPatch,
				ServiceTemplate: cnpgv1.ServiceTemplateSpec{
					ObjectMeta: cnpgv1.Metadata{
						Name:        "pg-rw-lb",
						Annotations: map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": "nlb"},
					},
					Spec: corev1.ServiceSpec{
						Type:                     corev1.ServiceTypeLoadBalancer,
						LoadBalancerSourceRanges: []string{"10.0.0.0/8", "192.168.1.1/32"},
					},
				},
			},
		},
		{
			name: "NodePort",
			svc:  &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			want: &cnpgv1.ManagedService{
				SelectorType:   cnpgv1.ServiceSelectorTypeRW,
				UpdateStrategy: cnpgv1.ServiceUpdateStrategyPatch,
				ServiceTemplate: cnpgv1.ServiceTemplateSpec{
					ObjectMeta: cnpgv1.Metadata{Name: "pg-rw-lb"},
					Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pg := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg"}}
			applyService(pg, tt.svc)
			if tt.want == nil {
				assert.Nil(t, pg.Spec.Managed)
				return
			}
			require.NotNil(t, pg.Spec.Managed)
			require.NotNil(t, pg.Spec.Managed.Services)
			require.Len(t, pg.Spec.Managed.Services.Additional, 1)
			assert.Equal(t, *tt.want, pg.Spec.Managed.Services.Additional[0])
		})
	}
}

func TestApplyServicePreservesDisabledDefaultServices(t *testing.T) {
	t.Parallel()
	pg := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pg"},
		Spec: cnpgv1.ClusterSpec{
			Managed: &cnpgv1.ManagedConfiguration{
				Services: &cnpgv1.ManagedServices{
					DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeRO},
				},
			},
		},
	}
	applyService(pg, &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer})
	require.NotNil(t, pg.Spec.Managed.Services)
	assert.Equal(t, []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeRO},
		pg.Spec.Managed.Services.DisabledDefaultServices)
	require.Len(t, pg.Spec.Managed.Services.Additional, 1)
	assert.Equal(t, "pg-rw-lb", pg.Spec.Managed.Services.Additional[0].ServiceTemplate.ObjectMeta.Name)
}

func TestValidateService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		instance    string
		svc         *corev1alpha1.Service
		custom      *components.CNPGCustomSpec
		hasProxy    bool
		wantErr     string
		wantWarning string
	}{
		{name: "nil", instance: "pg"},
		{name: "ClusterIP", instance: "pg", svc: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP}},
		{
			name:        "LoadBalancer without sourceRanges warns",
			instance:    "pg",
			svc:         &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			wantWarning: "0.0.0.0/0",
		},
		{
			name:     "ExternalName unsupported",
			instance: "pg",
			svc:      &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName},
			wantErr:  "serviceType",
		},
		{
			name:     "source ranges without LoadBalancer",
			instance: "pg",
			svc: &corev1alpha1.Service{
				ServiceType:         corev1.ServiceTypeNodePort,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"}},
			},
			wantErr: "loadBalancerService",
		},
		{
			name:     "invalid source range",
			instance: "pg",
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"not-a-cidr"},
				},
			},
			wantErr: "sourceRanges",
		},
		{
			name:     "bare IP source range is fine",
			instance: "pg",
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"192.168.1.1"},
				},
			},
		},
		{
			name:     "engine.service with managed.services.additional",
			instance: "pg",
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
				},
			},
			custom: &components.CNPGCustomSpec{
				Managed: &cnpgv1.ManagedConfiguration{
					Services: &cnpgv1.ManagedServices{
						Additional: []cnpgv1.ManagedService{{SelectorType: cnpgv1.ServiceSelectorTypeRW}},
					},
				},
			},
			wantErr: "managed.services.additional",
		},
		{
			name: "disabledDefaultServices alone is fine",
			instance: "pg",
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
				},
			},
			custom: &components.CNPGCustomSpec{
				Managed: &cnpgv1.ManagedConfiguration{
					Services: &cnpgv1.ManagedServices{
						DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeR},
					},
				},
			},
		},
		{
			name:     "instance name too long for managed Service",
			instance: strings.Repeat("a", 58),
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
				},
			},
			wantErr: "DNS label",
		},
		{
			name:     "proxy with LoadBalancer is forbidden",
			instance: "pg",
			svc: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
				},
			},
			hasProxy: true,
			wantErr:  "proxy",
		},
		{
			name:     "proxy with NodePort is forbidden",
			instance: "pg",
			svc:      &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			hasProxy: true,
			wantErr:  "proxy",
		},
		{
			name:     "proxy with ClusterIP is fine",
			instance: "pg",
			svc:      &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP},
			hasProxy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errs, warnings := validateService(tt.instance, tt.svc, tt.custom, tt.hasProxy)
			if tt.wantErr == "" {
				assert.Empty(t, errs)
			} else {
				require.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), tt.wantErr)
			}
			if tt.wantWarning == "" {
				assert.Empty(t, warnings)
				return
			}
			require.NotEmpty(t, warnings)
			assert.Contains(t, warnings[0], tt.wantWarning)
		})
	}
}

func TestApplyServiceExposure(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: "ns"},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.10"}},
			},
		},
	}
	hostname := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: "ns"},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{{Hostname: "db.example.com"}},
			},
		},
	}
	pendingLB := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: "ns"},
	}
	nodePortReady := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Port:     5432,
				NodePort: 30432,
			}},
		},
	}
	nodePortPending := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort},
	}

	tests := []struct {
		name         string
		svc          *corev1alpha1.Service
		objects      []client.Object
		wantHost     string
		wantPending  string
		wantHostLB   string
		wantNodePort string
	}{
		{name: "nil service", wantHost: "pg-rw.ns.svc"},
		{name: "ClusterIP", svc: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeClusterIP}, wantHost: "pg-rw.ns.svc"},
		{
			name:        "LoadBalancer missing service",
			svc:         &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			wantHost:    "pg-rw.ns.svc",
			wantPending: "waiting for the load balancer address",
		},
		{
			name:        "LoadBalancer pending",
			svc:         &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			objects:     []client.Object{pendingLB},
			wantHost:    "pg-rw.ns.svc",
			wantPending: "waiting for the load balancer address",
		},
		{
			name:       "LoadBalancer IP",
			svc:        &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			objects:    []client.Object{lb},
			wantHost:   "203.0.113.10",
			wantHostLB: "pg-rw-lb.ns.svc",
		},
		{
			name:       "LoadBalancer hostname",
			svc:        &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			objects:    []client.Object{hostname},
			wantHost:   "db.example.com",
			wantHostLB: "pg-rw-lb.ns.svc",
		},
		{
			name:        "NodePort missing service",
			svc:         &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			wantHost:    "pg-rw.ns.svc",
			wantPending: "waiting for the node port",
		},
		{
			name:        "NodePort pending allocation",
			svc:         &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			objects:     []client.Object{nodePortPending},
			wantHost:    "pg-rw.ns.svc",
			wantPending: "waiting for the node port",
		},
		{
			name:         "NodePort publishes host-lb and nodePort",
			svc:          &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			objects:      []client.Object{nodePortReady},
			wantHost:     "pg-rw.ns.svc",
			wantHostLB:   "pg-rw-lb.ns.svc",
			wantNodePort: "30432",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
			instance := &corev1alpha1.Instance{
				ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "ns"},
				Spec: corev1alpha1.InstanceSpec{
					Components: map[string]corev1alpha1.ComponentSpec{
						common.ComponentEngine: {Type: "postgresql"},
					},
				},
			}
			c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)
			details := controller.ConnectionDetails{
				Host:     "pg-rw.ns.svc",
				Port:     "5432",
				Username: "app",
				Password: "secret",
				AdditionalProperties: map[string]string{
					"database": "app",
					"sslmode":  sslModeRequire,
				},
			}
			pending, err := applyServiceExposure(c, tt.svc, &details)
			require.NoError(t, err)
			assert.Equal(t, tt.wantPending, pending)
			assert.Equal(t, tt.wantHost, details.Host)
			assert.Equal(t, tt.wantHostLB, details.AdditionalProperties[hostLBProperty])
			assert.Equal(t, tt.wantNodePort, details.AdditionalProperties[nodePortProperty])
		})
	}
}

func TestInstanceOfManagedService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{name: "unrelated service", obj: &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"}}},
		{
			name: "managed service of a cluster",
			obj: &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name:      "pg-rw-lb",
				Namespace: "ns",
				Labels: map[string]string{
					utils.IsManagedLabelName: "true",
					utils.ClusterLabelName:   "pg",
				},
			}},
			want: []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "pg", Namespace: "ns"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, instanceOfManagedService(context.Background(), tt.obj))
		})
	}
}

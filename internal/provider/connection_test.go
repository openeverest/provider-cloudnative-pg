package provider

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	cnpgpostgres "github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

func TestConnectionDetails(t *testing.T) {
	appDatabase := &cnpgv1.BootstrapConfiguration{
		InitDB: &cnpgv1.BootstrapInitDB{
			Database: "app",
			Owner:    "app",
			Secret:   &cnpgv1.LocalObjectReference{Name: "custom-app"},
		},
	}

	tests := []struct {
		name          string
		cluster       *cnpgv1.Cluster
		objects       []client.Object
		parameters    string
		service       *corev1alpha1.Service
		wantConverged bool
		wantMsg       string
		want          controller.ConnectionDetails
	}{
		{
			name: "application user",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "p@ss word", "5432"),
				credentialSecret("pg-app", "other", "nope", "5432"),
				credentialSecret("pg-superuser", "postgres", "admin", "5432"),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", "5432", "app", "p@ss word", "app", "custom-app", sslModeRequire, true, true,
				"postgresql://app:p%40ss%20word@pg-rw.ns.svc:5432/app?sslmode=require",
			),
		},
		{
			name: "requireTLS keeps sslmode require",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
			},
			parameters:    `{"requireTLS":true}`,
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", "5432", "app", "secret", "app", "custom-app", sslModeRequire, true, true,
				"postgresql://app:secret@pg-rw.ns.svc:5432/app?sslmode=require",
			),
		},
		{
			name: "port comes from the credential secret",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "15432"),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", "15432", "app", "secret", "app", "custom-app", sslModeRequire, true, true,
				"postgresql://app:secret@pg-rw.ns.svc:15432/app?sslmode=require",
			),
		},
		{
			name: "port falls back to the operator server port",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", ""),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", strconv.Itoa(cnpgpostgres.ServerPort), "app", "secret", "app", "custom-app", sslModeRequire, true, true,
				"postgresql://app:secret@pg-rw.ns.svc:"+strconv.Itoa(cnpgpostgres.ServerPort)+"/app?sslmode=require",
			),
		},
		{
			name: "superuser when there is no application database",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					EnableSuperuserAccess: ptr.To(true),
					Bootstrap: &cnpgv1.BootstrapConfiguration{
						InitDB: &cnpgv1.BootstrapInitDB{},
					},
					SuperuserSecret: &cnpgv1.LocalObjectReference{Name: "custom-superuser"},
				},
			}),
			objects: []client.Object{
				credentialSecret("custom-superuser", "postgres", "admin", ""),
				credentialSecret("pg-app", "app", "nope", "5432"),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", strconv.Itoa(cnpgpostgres.ServerPort), "postgres", "admin", "postgres", "custom-superuser", sslModeRequire, true, true,
				"postgresql://postgres:admin@pg-rw.ns.svc:"+strconv.Itoa(cnpgpostgres.ServerPort)+"/postgres?sslmode=require",
			),
		},
		{
			name: "application database does not fall back to the superuser",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					EnableSuperuserAccess: ptr.To(true),
					Bootstrap:             appDatabase,
				},
			}),
			objects: []client.Object{
				credentialSecret("pg-superuser", "postgres", "admin", "5432"),
			},
		},
		{
			name: "read-only service disabled",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Bootstrap: appDatabase,
					Managed: &cnpgv1.ManagedConfiguration{
						Services: &cnpgv1.ManagedServices{
							DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeRO},
						},
					},
				},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", "5432", "app", "secret", "app", "custom-app", sslModeRequire, true, false,
				"postgresql://app:secret@pg-rw.ns.svc:5432/app?sslmode=require",
			),
		},
		{
			name: "read service disabled",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Bootstrap: appDatabase,
					Managed: &cnpgv1.ManagedConfiguration{
						Services: &cnpgv1.ManagedServices{
							DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeR},
						},
					},
				},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", "5432", "app", "secret", "app", "custom-app", sslModeRequire, false, true,
				"postgresql://app:secret@pg-rw.ns.svc:5432/app?sslmode=require",
			),
		},
		{
			name: "LoadBalancer waits for an address",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: testNamespace}},
			},
			service: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			wantMsg: "waiting for the load balancer address",
		},
		{
			name: "LoadBalancer publishes the external address",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: testNamespace},
					Status: corev1.ServiceStatus{
						LoadBalancer: corev1.LoadBalancerStatus{
							Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.10"}},
						},
					},
				},
			},
			service:       &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			wantConverged: true,
			want: connectionWantExtra(
				"203.0.113.10", "5432", "app", "secret", "app", "custom-app", sslModeRequire, true, true,
				"postgresql://app:secret@203.0.113.10:5432/app?sslmode=require",
				map[string]string{hostLBProperty: "pg-rw-lb.ns.svc"},
			),
		},
		{
			name: "NodePort waits for allocation",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: testNamespace}},
			},
			service: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			wantMsg: "waiting for the node port",
		},
		{
			name: "NodePort publishes host-lb and nodePort",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Bootstrap: appDatabase},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: testNamespace},
					Spec: corev1.ServiceSpec{
						Type: corev1.ServiceTypeNodePort,
						Ports: []corev1.ServicePort{{
							Port:     5432,
							NodePort: 30432,
						}},
					},
				},
			},
			service:       &corev1alpha1.Service{ServiceType: corev1.ServiceTypeNodePort},
			wantConverged: true,
			want: connectionWantExtra(
				"pg-rw.ns.svc", "5432", "app", "secret", "app", "custom-app", sslModeRequire, true, true,
				"postgresql://app:secret@pg-rw.ns.svc:5432/app?sslmode=require",
				map[string]string{hostLBProperty: "pg-rw-lb.ns.svc", nodePortProperty: "30432"},
			),
		},
		{
			name: "single instance omits the read-only service",
			cluster: readyCluster(&cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Instances: 1,
					Bootstrap: appDatabase,
				},
			}),
			objects: []client.Object{
				credentialSecret("custom-app", "app", "secret", "5432"),
			},
			wantConverged: true,
			want: connectionWant(
				"pg-rw.ns.svc", "5432", "app", "secret", "app", "custom-app", sslModeRequire, true, false,
				"postgresql://app:secret@pg-rw.ns.svc:5432/app?sslmode=require",
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statusWithObjects(t, tt.cluster, tt.parameters, tt.service, tt.objects...)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got.Converged != tt.wantConverged {
				t.Fatalf("converged = %t, want %t (message: %s)", got.Converged, tt.wantConverged, got.Message)
			}
			if !tt.wantConverged {
				wantMsg := tt.wantMsg
				if wantMsg == "" {
					wantMsg = "Waiting for the CloudNativePG credentials Secret"
				}
				if got.Message != wantMsg {
					t.Fatalf("message = %q, want %q", got.Message, wantMsg)
				}
				return
			}
			assertConnectionDetails(t, got.ConnectionDetails, tt.want)
		})
	}
}

func readyCluster(pg *cnpgv1.Cluster) *cnpgv1.Cluster {
	pg.Name = "pg"
	pg.Namespace = testNamespace
	if pg.Spec.Instances == 0 {
		pg.Spec.Instances = 2
	}
	instances := make([]string, 0, pg.Spec.Instances)
	for i := range pg.Spec.Instances {
		instances = append(instances, fmt.Sprintf("pg-%d", i+1))
	}
	pg.Status = cnpgv1.ClusterStatus{
		Phase:           cnpgv1.PhaseHealthy,
		Instances:       pg.Spec.Instances,
		ReadyInstances:  pg.Spec.Instances,
		CurrentPrimary:  instances[0],
		TargetPrimary:   instances[0],
		InstancesStatus: map[cnpgv1.PodStatus][]string{cnpgv1.PodHealthy: instances},
		Conditions: []metav1.Condition{
			{Type: string(cnpgv1.ConditionClusterReady), Status: metav1.ConditionTrue},
			{Type: string(cnpgv1.ConditionInitialized), Status: metav1.ConditionTrue},
		},
	}
	return pg
}

func credentialSecret(name, username, password, port string) *corev1.Secret {
	data := map[string][]byte{
		"username": []byte(username),
		"password": []byte(password),
	}
	if port != "" {
		data["port"] = []byte(port)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       data,
	}
}

func connectionWant(host, port, username, password, database, secretName, sslmode string, read, readOnly bool, uri string) controller.ConnectionDetails {
	return connectionWantExtra(host, port, username, password, database, secretName, sslmode, read, readOnly, uri, nil)
}

func connectionWantExtra(host, port, username, password, database, secretName, sslmode string, read, readOnly bool, uri string, extraProps map[string]string) controller.ConnectionDetails {
	extra := map[string]string{
		"database":   database,
		"secretName": secretName,
		"sslmode":    sslmode,
	}
	if read {
		extra[hostRProperty] = "pg-r.ns.svc"
	}
	if readOnly {
		extra[hostROProperty] = "pg-ro.ns.svc"
	}
	for k, v := range extraProps {
		extra[k] = v
	}
	return controller.ConnectionDetails{
		Type:                 "postgresql",
		Provider:             common.ProviderName,
		Host:                 host,
		Port:                 port,
		Username:             username,
		Password:             password,
		URI:                  uri,
		AdditionalProperties: extra,
	}
}

func assertConnectionDetails(t *testing.T, got, want controller.ConnectionDetails) {
	t.Helper()
	if got.Type != want.Type || got.Provider != want.Provider || got.Host != want.Host ||
		got.Port != want.Port || got.Username != want.Username || got.Password != want.Password ||
		got.URI != want.URI {
		t.Fatalf("connection details = %+v, want %+v", got, want)
	}
	if len(got.AdditionalProperties) != len(want.AdditionalProperties) {
		t.Fatalf("additional = %#v, want %#v", got.AdditionalProperties, want.AdditionalProperties)
	}
	for key, value := range want.AdditionalProperties {
		if got.AdditionalProperties[key] != value {
			t.Fatalf("additional[%q] = %q, want %q (all: %#v)", key, got.AdditionalProperties[key], value, got.AdditionalProperties)
		}
	}
}

func statusWithObjects(t *testing.T, cluster *cnpgv1.Cluster, parameters string, service *corev1alpha1.Service, objects ...client.Object) (controller.Status, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{cnpgv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	engine := corev1alpha1.ComponentSpec{Service: service}
	if parameters != "" {
		engine.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: engine,
			},
		},
	}
	objects = append(objects, cluster)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	c := controller.NewContext(context.Background(), cl, instance, "provider-cloudnative-pg")
	return (&Provider{}).Status(c)
}

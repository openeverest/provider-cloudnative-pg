package provider

import (
	"context"
	"strconv"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	pgbouncerconfig "github.com/cloudnative-pg/cloudnative-pg/pkg/management/pgbouncer/config"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

func TestBuildPooler(t *testing.T) {
	c := proxyContext(t, proxyComponent(`{"poolMode":"transaction"}`, ptr.To(int32(2)), proxyResources()))

	pooler := buildPooler(c, c.Instance().Spec.Components[common.ComponentProxy], mustProxyParams(t, c), false, poolerName(c.Name()), cnpgv1.PoolerTypeRW)

	if pooler.Name != "pg-pooler" {
		t.Fatalf("name = %q", pooler.Name)
	}
	if pooler.Spec.Cluster.Name != "pg" {
		t.Fatalf("cluster = %q", pooler.Spec.Cluster.Name)
	}
	if pooler.Spec.Type != cnpgv1.PoolerTypeRW {
		t.Fatalf("type = %q", pooler.Spec.Type)
	}
	if pooler.Spec.PgBouncer == nil || pooler.Spec.PgBouncer.PoolMode != cnpgv1.PgBouncerPoolModeTransaction {
		t.Fatalf("pool mode = %+v", pooler.Spec.PgBouncer)
	}
	if pooler.Spec.Instances == nil || *pooler.Spec.Instances != 2 {
		t.Fatalf("instances = %v", pooler.Spec.Instances)
	}
	if pooler.Spec.Template == nil || pooler.Spec.Template.ObjectMeta.Labels[controller.ComponentLabel] != common.ComponentProxy {
		t.Fatalf("pod labels = %+v", pooler.Spec.Template)
	}
	containers := pooler.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Name != pgbouncerContainerName {
		t.Fatalf("containers = %+v", containers)
	}
	if containers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("100m")) != 0 {
		t.Fatalf("cpu request = %s", containers[0].Resources.Requests.Cpu())
	}
	if containers[0].Image != "" {
		t.Fatalf("image = %q, the operator selects the PgBouncer image", containers[0].Image)
	}
}

func TestBuildPoolerReadOnly(t *testing.T) {
	c := proxyContext(t, proxyComponent(`{"poolMode":"session","readOnly":true}`, ptr.To(int32(1)), nil))

	pooler := buildPooler(c, c.Instance().Spec.Components[common.ComponentProxy], mustProxyParams(t, c), false, poolerROName(c.Name()), cnpgv1.PoolerTypeRO)

	if pooler.Name != "pg-pooler-ro" {
		t.Fatalf("name = %q", pooler.Name)
	}
	if pooler.Spec.Type != cnpgv1.PoolerTypeRO {
		t.Fatalf("type = %q", pooler.Spec.Type)
	}
	if pooler.Spec.Cluster.Name != "pg" {
		t.Fatalf("cluster = %q", pooler.Spec.Cluster.Name)
	}
	if pooler.Spec.PgBouncer.PoolMode != cnpgv1.PgBouncerPoolModeSession {
		t.Fatalf("pool mode = %q", pooler.Spec.PgBouncer.PoolMode)
	}
}

func TestBuildPoolerDefaults(t *testing.T) {
	c := proxyContext(t, corev1alpha1.ComponentSpec{})

	pooler := buildPooler(c, c.Instance().Spec.Components[common.ComponentProxy], mustProxyParams(t, c), false, poolerName(c.Name()), cnpgv1.PoolerTypeRW)

	if pooler.Spec.Instances == nil || *pooler.Spec.Instances != 1 {
		t.Fatalf("instances = %v, want the operator default of 1", pooler.Spec.Instances)
	}
	if pooler.Spec.PgBouncer.PoolMode != "" {
		t.Fatalf("pool mode = %q, want empty so the operator default (session) applies", pooler.Spec.PgBouncer.PoolMode)
	}
	if len(pooler.Spec.Template.Spec.Containers) != 0 {
		t.Fatalf("containers = %+v, want none without resources", pooler.Spec.Template.Spec.Containers)
	}
	if len(pooler.Spec.PgBouncer.Parameters) != 0 {
		t.Fatalf("parameters = %v, want none without requireTLS", pooler.Spec.PgBouncer.Parameters)
	}
}

func TestBuildPoolerRequireTLS(t *testing.T) {
	c := proxyContext(t, corev1alpha1.ComponentSpec{})

	pooler := buildPooler(c, c.Instance().Spec.Components[common.ComponentProxy], mustProxyParams(t, c), true, poolerName(c.Name()), cnpgv1.PoolerTypeRW)

	if got := pooler.Spec.PgBouncer.Parameters[pgbouncerClientTLSSSLMode]; got != "require" {
		t.Fatalf("%s = %q, want require", pgbouncerClientTLSSSLMode, got)
	}
}

func TestValidateProxy(t *testing.T) {
	tests := []struct {
		name    string
		engine  corev1alpha1.ComponentSpec
		proxy   corev1alpha1.ComponentSpec
		wantErr string
	}{
		{name: "transaction", proxy: proxyComponent(`{"poolMode":"transaction"}`, ptr.To(int32(1)), nil)},
		{name: "session", proxy: proxyComponent(`{"poolMode":"session"}`, nil, nil)},
		{name: "omitted mode", proxy: corev1alpha1.ComponentSpec{}},
		{name: "statement is not a CloudNativePG mode", proxy: proxyComponent(`{"poolMode":"statement"}`, nil, nil), wantErr: "statement"},
		{name: "zero replicas", proxy: proxyComponent("", ptr.To(int32(0)), nil), wantErr: "at least 1"},
		{name: "pgbouncer type", proxy: corev1alpha1.ComponentSpec{Type: common.ComponentTypePgbouncer}},
		{name: "unknown type", proxy: corev1alpha1.ComponentSpec{Type: "pgpool"}, wantErr: "pgpool"},
		{
			name:    "readOnly with two engine replicas",
			engine:  corev1alpha1.ComponentSpec{Replicas: ptr.To(int32(2))},
			proxy:   proxyComponent(`{"readOnly":true}`, ptr.To(int32(1)), nil),
		},
		{
			name:    "readOnly with one engine replica",
			engine:  corev1alpha1.ComponentSpec{Replicas: ptr.To(int32(1))},
			proxy:   proxyComponent(`{"readOnly":true}`, ptr.To(int32(1)), nil),
			wantErr: "at least 2 engine replicas",
		},
		{
			name:    "readOnly without engine replicas",
			proxy:   proxyComponent(`{"readOnly":true}`, nil, nil),
			wantErr: "at least 2 engine replicas",
		},
		{
			name: "cpu request greater than limit",
			proxy: proxyComponent(`{"poolMode":"session"}`, ptr.To(int32(1)), &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}),
			wantErr: "greater than limit",
		},
		{
			name: "valid resources",
			proxy: proxyComponent(`{"poolMode":"session"}`, ptr.To(int32(1)), &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := proxyContext(t, tt.proxy)
			if tt.engine.Replicas != nil || tt.engine.Type != "" {
				c.Instance().Spec.Components[common.ComponentEngine] = tt.engine
			}
			err := validateProxy(c.Instance())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateProxyRejectsANameTheServiceCannotUse(t *testing.T) {
	c := proxyContext(t, corev1alpha1.ComponentSpec{})
	c.Instance().Name = strings.Repeat("a", 60)
	if err := validateProxy(c.Instance()); err == nil {
		t.Fatal("expected the pooler Service name to be rejected")
	}
}

func TestValidateProxyRejectsAReadOnlyNameTheServiceCannotUse(t *testing.T) {
	c := proxyContext(t, proxyComponent(`{"readOnly":true}`, ptr.To(int32(1)), nil))
	c.Instance().Spec.Components[common.ComponentEngine] = corev1alpha1.ComponentSpec{Replicas: ptr.To(int32(2))}
	// "-pooler" fits in 63 chars at length 56; "-pooler-ro" does not.
	c.Instance().Name = strings.Repeat("a", 56)
	if err := validateProxy(c.Instance()); err == nil {
		t.Fatal("expected the read-only pooler Service name to be rejected")
	}
}

func TestSyncPooler(t *testing.T) {
	t.Run("creates the pooler", func(t *testing.T) {
		c, cl := proxyContextWithClient(t, proxyComponent(`{"poolMode":"session"}`, ptr.To(int32(1)), nil))
		if err := syncPooler(c, false); err != nil {
			t.Fatal(err)
		}
		got := &cnpgv1.Pooler{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-pooler"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Type != cnpgv1.PoolerTypeRW || got.Spec.PgBouncer.PoolMode != cnpgv1.PgBouncerPoolModeSession {
			t.Fatalf("spec = %+v", got.Spec)
		}
		err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-pooler-ro"}, &cnpgv1.Pooler{})
		if client.IgnoreNotFound(err) != nil || err == nil {
			t.Fatalf("ro pooler get err = %v, want not found", err)
		}
	})

	t.Run("creates the read-only pooler", func(t *testing.T) {
		c, cl := proxyContextWithClient(t, proxyComponent(`{"poolMode":"transaction","readOnly":true}`, ptr.To(int32(2)), nil))
		if err := syncPooler(c, true); err != nil {
			t.Fatal(err)
		}
		rw := &cnpgv1.Pooler{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-pooler"}, rw); err != nil {
			t.Fatal(err)
		}
		if rw.Spec.Type != cnpgv1.PoolerTypeRW {
			t.Fatalf("rw type = %q", rw.Spec.Type)
		}
		ro := &cnpgv1.Pooler{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-pooler-ro"}, ro); err != nil {
			t.Fatal(err)
		}
		if ro.Spec.Type != cnpgv1.PoolerTypeRO || ro.Spec.PgBouncer.PoolMode != cnpgv1.PgBouncerPoolModeTransaction {
			t.Fatalf("ro spec = %+v", ro.Spec)
		}
		if ro.Spec.Instances == nil || *ro.Spec.Instances != 2 {
			t.Fatalf("ro instances = %v", ro.Spec.Instances)
		}
		if got := ro.Spec.PgBouncer.Parameters[pgbouncerClientTLSSSLMode]; got != "require" {
			t.Fatalf("ro %s = %q", pgbouncerClientTLSSSLMode, got)
		}
	})

	t.Run("deletes the read-only pooler when readOnly is turned off", func(t *testing.T) {
		c, cl := proxyContextWithClient(t, proxyComponent(`{"poolMode":"session"}`, ptr.To(int32(1)), nil))
		if err := cl.Create(context.Background(), &cnpgv1.Pooler{ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler-ro", Namespace: testNamespace}}); err != nil {
			t.Fatal(err)
		}
		if err := syncPooler(c, false); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-pooler"}, &cnpgv1.Pooler{}); err != nil {
			t.Fatal(err)
		}
		err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "pg-pooler-ro"}, &cnpgv1.Pooler{})
		if client.IgnoreNotFound(err) != nil || err == nil {
			t.Fatalf("ro pooler get err = %v, want not found", err)
		}
	})

	t.Run("deletes the poolers when the component is removed", func(t *testing.T) {
		c, cl := proxyContextWithClient(t)
		for _, name := range []string{"pg-pooler", "pg-pooler-ro"} {
			if err := cl.Create(context.Background(), &cnpgv1.Pooler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := syncPooler(c, false); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"pg-pooler", "pg-pooler-ro"} {
			err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &cnpgv1.Pooler{})
			if client.IgnoreNotFound(err) != nil || err == nil {
				t.Fatalf("%s get err = %v, want not found", name, err)
			}
		}
	})

	t.Run("does nothing without the component or a pooler", func(t *testing.T) {
		c, _ := proxyContextWithClient(t)
		if err := syncPooler(c, false); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPoolerConnectionDetails(t *testing.T) {
	cluster := readyCluster(&cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{
		Instances: 2,
		Bootstrap: &cnpgv1.BootstrapConfiguration{
			InitDB: &cnpgv1.BootstrapInitDB{Database: "app", Owner: "app", Secret: &cnpgv1.LocalObjectReference{Name: "custom-app"}},
		},
	}})
	secret := credentialSecret("custom-app", "app", "secret", "5432")
	port := strconv.Itoa(pgbouncerconfig.PgBouncerPort)

	t.Run("points at the pooler once it is active", func(t *testing.T) {
		pooler := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Spec:       cnpgv1.PoolerSpec{Instances: ptr.To(int32(1))},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive, Instances: 1},
		}
		got := proxyStatus(t, cluster, secret, nil, pooler, poolerDeployment("pg-pooler", 1))
		if got.Phase != corev1alpha1.InstancePhaseReady {
			t.Fatalf("phase = %q (%s)", got.Phase, got.Message)
		}
		if got.ConnectionDetails.Host != "pg-pooler.ns.svc" || got.ConnectionDetails.Port != port {
			t.Fatalf("endpoint = %s:%s", got.ConnectionDetails.Host, got.ConnectionDetails.Port)
		}
		if got.ConnectionDetails.URI != "postgresql://app:secret@pg-pooler.ns.svc:"+port+"/app?sslmode=require" {
			t.Fatalf("uri = %s", got.ConnectionDetails.URI)
		}
		if got.ConnectionDetails.AdditionalProperties[hostRProperty] != "pg-r.ns.svc" ||
			got.ConnectionDetails.AdditionalProperties[hostROProperty] != "pg-ro.ns.svc" {
			t.Fatalf("read endpoints = %#v", got.ConnectionDetails.AdditionalProperties)
		}
	})

	t.Run("points host-ro at the read-only pooler", func(t *testing.T) {
		rw := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Spec:       cnpgv1.PoolerSpec{Instances: ptr.To(int32(1))},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive, Instances: 1},
		}
		ro := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler-ro", Namespace: testNamespace},
			Spec:       cnpgv1.PoolerSpec{Type: cnpgv1.PoolerTypeRO, Instances: ptr.To(int32(1))},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive, Instances: 1},
		}
		got := proxyStatus(t, cluster, secret, &runtime.RawExtension{Raw: []byte(`{"readOnly":true}`)},
			rw, ro, poolerDeployment("pg-pooler", 1), poolerDeployment("pg-pooler-ro", 1))
		if got.Phase != corev1alpha1.InstancePhaseReady {
			t.Fatalf("phase = %q (%s)", got.Phase, got.Message)
		}
		if got.ConnectionDetails.Host != "pg-pooler.ns.svc" {
			t.Fatalf("host = %s", got.ConnectionDetails.Host)
		}
		if got.ConnectionDetails.AdditionalProperties[hostROProperty] != "pg-pooler-ro.ns.svc" {
			t.Fatalf("host-ro = %s", got.ConnectionDetails.AdditionalProperties[hostROProperty])
		}
	})

	t.Run("waits for the read-only pooler", func(t *testing.T) {
		rw := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Spec:       cnpgv1.PoolerSpec{Instances: ptr.To(int32(1))},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive, Instances: 1},
		}
		got := proxyStatus(t, cluster, secret, &runtime.RawExtension{Raw: []byte(`{"readOnly":true}`)},
			rw, poolerDeployment("pg-pooler", 1))
		if got.Phase != corev1alpha1.InstancePhaseProvisioning || got.Message != "waiting for the CloudNativePG pooler" {
			t.Fatalf("status = %s %q", got.Phase, got.Message)
		}
	})

	t.Run("waits while fewer PgBouncer pods are ready than requested", func(t *testing.T) {
		pooler := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Spec:       cnpgv1.PoolerSpec{Instances: ptr.To(int32(2))},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive, Instances: 2},
		}
		got := proxyStatus(t, cluster, secret, nil, pooler, poolerDeployment("pg-pooler", 1))
		if got.Phase != corev1alpha1.InstancePhaseProvisioning || got.Message != "waiting for PgBouncer pods (1/2 ready)" {
			t.Fatalf("status = %s %q", got.Phase, got.Message)
		}
	})

	t.Run("waits for the PgBouncer deployment", func(t *testing.T) {
		pooler := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive},
		}
		got := proxyStatus(t, cluster, secret, nil, pooler)
		if got.Phase != corev1alpha1.InstancePhaseProvisioning {
			t.Fatalf("phase = %q", got.Phase)
		}
	})

	t.Run("waits while the pooler is inactive", func(t *testing.T) {
		pooler := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseInactive, PhaseReason: "cluster not found"},
		}
		got := proxyStatus(t, cluster, secret, nil, pooler, poolerDeployment("pg-pooler", 1))
		if got.Phase != corev1alpha1.InstancePhaseProvisioning || got.Message != "waiting for the CloudNativePG pooler (inactive): cluster not found" {
			t.Fatalf("status = %s %q", got.Phase, got.Message)
		}
	})

	t.Run("reports a failed pooler", func(t *testing.T) {
		pooler := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseFailed, PhaseReason: "image catalog missing"},
		}
		got := proxyStatus(t, cluster, secret, nil, pooler)
		if got.Phase != corev1alpha1.InstancePhaseFailed || got.Message != "failed: image catalog missing" {
			t.Fatalf("status = %s %q", got.Phase, got.Message)
		}
	})

	t.Run("LoadBalancer does not override pooler host or gate Ready", func(t *testing.T) {
		pooler := &cnpgv1.Pooler{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace},
			Spec:       cnpgv1.PoolerSpec{Instances: ptr.To(int32(1))},
			Status:     cnpgv1.PoolerStatus{Phase: cnpgv1.PoolerPhaseActive, Instances: 1},
		}
		// Pending LB address must not keep the Instance from Ready when a proxy is set.
		lb := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "pg-rw-lb", Namespace: testNamespace}}
		got := proxyStatusWithService(t, cluster, secret, nil,
			&corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			pooler, poolerDeployment("pg-pooler", 1), lb)
		if got.Phase != corev1alpha1.InstancePhaseReady {
			t.Fatalf("phase = %q (%s)", got.Phase, got.Message)
		}
		if got.ConnectionDetails.Host != "pg-pooler.ns.svc" || got.ConnectionDetails.Port != port {
			t.Fatalf("endpoint = %s:%s", got.ConnectionDetails.Host, got.ConnectionDetails.Port)
		}
	})
}

func proxyStatus(t *testing.T, cluster *cnpgv1.Cluster, secret *corev1.Secret, proxyParams *runtime.RawExtension, objs ...client.Object) controller.Status {
	t.Helper()
	return proxyStatusWithService(t, cluster, secret, proxyParams, nil, objs...)
}

func proxyStatusWithService(t *testing.T, cluster *cnpgv1.Cluster, secret *corev1.Secret, proxyParams *runtime.RawExtension, service *corev1alpha1.Service, objs ...client.Object) controller.Status {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{cnpgv1.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	proxy := corev1alpha1.ComponentSpec{Parameters: proxyParams}
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace},
		Spec: corev1alpha1.InstanceSpec{Components: map[string]corev1alpha1.ComponentSpec{
			common.ComponentEngine: {Service: service},
			common.ComponentProxy:  proxy,
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{cluster, secret}, objs...)...).Build()
	c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)
	got, err := (&Provider{}).Status(c)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func poolerDeployment(name string, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Status:     appsv1.DeploymentStatus{Replicas: ready, ReadyReplicas: ready},
	}
}

func TestInstanceOfPoolerDeployment(t *testing.T) {
	poolerLabels := map[string]string{
		utils.ClusterLabelName: "pg",
		utils.PodRoleLabelName: string(utils.PodRolePooler),
	}
	tests := []struct {
		name   string
		object metav1.ObjectMeta
		want   bool
	}{
		{name: "rw pooler deployment", object: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace, Labels: poolerLabels}, want: true},
		{name: "ro pooler deployment", object: metav1.ObjectMeta{Name: "pg-pooler-ro", Namespace: testNamespace, Labels: poolerLabels}, want: true},
		{name: "pooler not managed by the provider", object: metav1.ObjectMeta{Name: "pg-custom-pooler", Namespace: testNamespace, Labels: poolerLabels}},
		{name: "unrelated deployment", object: metav1.ObjectMeta{Name: "pg-pooler", Namespace: testNamespace}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := instanceOfPoolerDeployment(context.Background(), &appsv1.Deployment{ObjectMeta: tt.object})
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("requests = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Name != "pg" || got[0].Namespace != testNamespace {
				t.Fatalf("requests = %v, want ns/pg", got)
			}
		})
	}
}

func proxyComponent(parameters string, replicas *int32, resources *corev1.ResourceRequirements) corev1alpha1.ComponentSpec {
	spec := corev1alpha1.ComponentSpec{Replicas: replicas, Resources: resources}
	if parameters != "" {
		spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	return spec
}

func proxyResources() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

func proxyContext(t *testing.T, proxy ...corev1alpha1.ComponentSpec) *controller.Context {
	t.Helper()
	c, _ := proxyContextWithClient(t, proxy...)
	return c
}

func proxyContextWithClient(t *testing.T, proxy ...corev1alpha1.ComponentSpec) (*controller.Context, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{cnpgv1.AddToScheme, corev1.AddToScheme, corev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace, UID: "uid"}}
	if len(proxy) > 0 {
		instance.Spec.Components = map[string]corev1alpha1.ComponentSpec{common.ComponentProxy: proxy[0]}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	return controller.NewContext(context.Background(), cl, instance, common.ProviderName), cl
}

func mustProxyParams(t *testing.T, c *controller.Context) components.PgBouncerParameters {
	t.Helper()
	params, err := proxyParameters(c.Instance().Spec.Components[common.ComponentProxy])
	if err != nil {
		t.Fatal(err)
	}
	return params
}

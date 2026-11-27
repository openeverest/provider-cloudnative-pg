package provider

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

const (
	providerSpecPath = "../../charts/provider-cloudnative-pg/generated/provider-spec.yaml"
	instanceFixture  = "testdata/validate/instance.yaml"
)

// engineParameters sets the engine component's parameters.
func engineParameters(raw string) func(*corev1alpha1.Instance) {
	return func(in *corev1alpha1.Instance) {
		c := in.Spec.Components[common.ComponentEngine]
		c.Parameters = &runtime.RawExtension{Raw: []byte(raw)}
		in.Spec.Components[common.ComponentEngine] = c
	}
}

// TestValidate runs the checks the runtime runs at admission and before every
// Sync: the schemas, supportedFields and catalog the provider publishes, then
// Validate.
func TestValidate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)

	tests := []struct {
		name         string
		mutate       func(*corev1alpha1.Instance)
		want         []string
		wantWarnings []string
	}{
		{name: "valid instance"},
		{
			name:   "missing engine",
			mutate: func(in *corev1alpha1.Instance) { delete(in.Spec.Components, common.ComponentEngine) },
			want:   []string{"spec.components[engine]"},
		},
		{
			name: "cpu request greater than limit",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Components[common.ComponentEngine].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
			},
			want: []string{"spec.components[engine].resources.requests[cpu]"},
		},
		{
			name:   "invalid priority class name",
			mutate: engineParameters(`{"priorityClassName":"Not_Valid"}`),
			want:   []string{"spec.components[engine].parameters.priorityClassName"},
		},
		{
			name:   "name too long",
			mutate: func(in *corev1alpha1.Instance) { in.Name = strings.Repeat("a", 51) },
			want:   []string{"metadata.name"},
		},
		{
			name: "every problem at once",
			mutate: func(in *corev1alpha1.Instance) {
				in.Name = strings.Repeat("a", 51)
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Storage.Size = resource.MustParse("512Mi")
				engine.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
				engine.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
				in.Spec.Components[common.ComponentEngine] = engine
				engineParameters(`{"priorityClassName":"Not_Valid","walStorage":{"size":"512Mi"}}`)(in)
			},
			want: []string{
				"metadata.name",
				"spec.components[engine].parameters.priorityClassName",
				"spec.components[engine].storage.size",
				"spec.components[engine].resources.requests[memory]",
				"spec.components[engine].resources.limits[memory]",
				"spec.components[engine].parameters.walStorage.size",
			},
		},
		{
			name:   "logical decoding slots synchronized without HA slots",
			mutate: engineParameters(`{"replicationSlots":{"highAvailability":{"enabled":false,"synchronizeLogicalDecoding":true}}}`),
			want:   []string{"spec.components[engine].parameters.replicationSlots"},
		},
		{
			name:   "pg_stat_statements parameters with the extension off",
			mutate: engineParameters(`{"extensions":{"pgStatStatements":{"enabled":false}},"postgresql":{"parameters":{"pg_stat_statements.max":"1000"}}}`),
			want:   []string{"spec.components[engine].parameters.extensions.pgStatStatements"},
		},
		{
			name:   "synchronous standbys not fewer than replicas",
			mutate: engineParameters(`{"postgresql":{"synchronous":{"method":"any","number":3}}}`),
			want:   []string{"spec.components[engine].parameters.postgresql.synchronous"},
		},
		{
			name: "image-volume extension on PostgreSQL 17",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Version = "17.10"
				engineParameters(`{"extensions":{"postgis":{"enabled":true}}}`)(in)
			},
			want: []string{"spec.components[engine].parameters.extensions"},
		},
		{
			name: "bootstrap volumeSnapshot with a dataSource",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.DataSource = &backupv1alpha1.DataSource{
					Type:   backupv1alpha1.DataSourceTypeBackup,
					Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "b"}},
				}
				engineParameters(`{"bootstrap":{"volumeSnapshot":{"name":"snap-1"}}}`)(in)
			},
			want: []string{"spec.components[engine].parameters.bootstrap.volumeSnapshot"},
		},
		{
			name: "proxy with an unsupported pool mode",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{
					Replicas:   new(int32(1)),
					Parameters: &runtime.RawExtension{Raw: []byte(`{"poolMode":"statement"}`)},
				}
			},
			want: []string{"spec.components[proxy]"},
		},
		{
			name: "read-only proxy with one engine replica",
			mutate: func(in *corev1alpha1.Instance) {
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Replicas = new(int32(1))
				in.Spec.Components[common.ComponentEngine] = engine
				in.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{
					Replicas:   new(int32(1)),
					Parameters: &runtime.RawExtension{Raw: []byte(`{"readOnly":true}`)},
				}
			},
			want: []string{"spec.components[proxy]"},
		},
		{
			name: "unsupported engine.service type",
			mutate: func(in *corev1alpha1.Instance) {
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName}
				in.Spec.Components[common.ComponentEngine] = engine
			},
			want: []string{"spec.components[engine].service.serviceType"},
		},
		{
			name: "engine.service with managed.services.additional",
			mutate: func(in *corev1alpha1.Instance) {
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Service = &corev1alpha1.Service{
					ServiceType: corev1.ServiceTypeLoadBalancer,
					LoadBalancerService: &corev1alpha1.LoadBalancerService{
						SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
					},
				}
				engine.Parameters = &runtime.RawExtension{Raw: []byte(`{"managed":{"services":{"additional":[{"selectorType":"rw","serviceTemplate":{"metadata":{"name":"custom"}}}]}}}`)}
				in.Spec.Components[common.ComponentEngine] = engine
			},
			want: []string{"spec.components[engine].parameters.managed.services.additional"},
		},
		{
			name: "invalid loadBalancer source range",
			mutate: func(in *corev1alpha1.Instance) {
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Service = &corev1alpha1.Service{
					ServiceType: corev1.ServiceTypeLoadBalancer,
					LoadBalancerService: &corev1alpha1.LoadBalancerService{
						SourceRanges: corev1alpha1.SourceRanges{"not-a-cidr"},
					},
				}
				in.Spec.Components[common.ComponentEngine] = engine
			},
			want: []string{"spec.components[engine].service.loadBalancerService.sourceRanges[0]"},
		},
		{
			name: "engine.service name too long",
			mutate: func(in *corev1alpha1.Instance) {
				in.Name = strings.Repeat("a", 58)
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Service = &corev1alpha1.Service{
					ServiceType: corev1.ServiceTypeLoadBalancer,
					LoadBalancerService: &corev1alpha1.LoadBalancerService{
						SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
					},
				}
				in.Spec.Components[common.ComponentEngine] = engine
			},
			want: []string{"spec.components[engine].service"},
		},
		{
			name: "proxy with engine.service LoadBalancer is forbidden",
			mutate: func(in *corev1alpha1.Instance) {
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Service = &corev1alpha1.Service{
					ServiceType: corev1.ServiceTypeLoadBalancer,
					LoadBalancerService: &corev1alpha1.LoadBalancerService{
						SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
					},
				}
				in.Spec.Components[common.ComponentEngine] = engine
				in.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{
					Replicas: new(int32(1)),
				}
			},
			want: []string{"spec.components[engine].service"},
		},
		{
			name: "LoadBalancer without sourceRanges warns",
			mutate: func(in *corev1alpha1.Instance) {
				engine := in.Spec.Components[common.ComponentEngine]
				engine.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer}
				in.Spec.Components[common.ComponentEngine] = engine
			},
			wantWarnings: []string{"spec.components[engine].service.loadBalancerService.sourceRanges"},
		},
		{
			name: "replication certificate with a proxy",
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Parameters = &runtime.RawExtension{Raw: []byte(`{"tls":{"internalCertificateSecretName":"repl"}}`)}
				in.Spec.Components[common.ComponentProxy] = corev1alpha1.ComponentSpec{Replicas: new(int32(1))}
			},
			want: []string{"spec.parameters.tls.internalCertificateSecretName"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := validationtest.Load(t, instanceFixture)
			if tt.mutate != nil {
				tt.mutate(in)
			}
			got := v.Run(in, nil)
			var fields []string
			for _, err := range got.Errors {
				fields = append(fields, err.Field)
			}
			assert.Equal(t, tt.want, fields, "errors: %v", got.Errors)

			var warned []string
			for _, w := range got.Warnings {
				warned = append(warned, strings.SplitN(w, ":", 2)[0])
			}
			assert.Equal(t, tt.wantWarnings, warned, "warnings: %v", got.Warnings)
		})
	}
}

// A reconciled Instance below the minimums, written before they existed,
// keeps syncing with a warning; a new one is rejected.
func TestValidateMinimumsOfReconciledInstance(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	small := func() *corev1alpha1.Instance {
		in := validationtest.Load(t, instanceFixture)
		in.Spec.Components[common.ComponentEngine].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
		in.Spec.Components[common.ComponentEngine].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
		return in
	}
	memory := []string{"spec.components[engine].resources.requests[memory]", "spec.components[engine].resources.limits[memory]"}

	created := v.Run(small(), nil)
	assert.Equal(t, memory, errorFields(created), "errors: %v", created.Errors)

	reconciled := small()
	reconciled.Status.ObservedGeneration = 1
	got := v.Run(reconciled, nil)
	assert.Empty(t, got.Errors)
	assert.Equal(t, memory, warningFields(got.Warnings), "warnings: %v", got.Warnings)

	edited := small()
	edited.Spec.Components[common.ComponentEngine].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
	got = v.Run(edited, reconciled)
	assert.Empty(t, got.Errors, "an update keeping the stored Instance's minimums")
	assert.Equal(t, memory, warningFields(got.Warnings), "warnings: %v", got.Warnings)

	edited.Spec.Components[common.ComponentEngine].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
	got = v.Run(edited, reconciled)
	assert.Equal(t, []string{"spec.components[engine].resources.requests[cpu]"}, errorFields(got), "a new minimum missed: %v", got.Errors)
}

// TestValidateDestructiveUpdate checks the changes that lose data or cannot
// be undone: rejected ones are Forbidden errors, admitted ones warnings, and
// both start with the destructive marker.
func TestValidateDestructiveUpdate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	engine := func(mutate func(*corev1alpha1.ComponentSpec)) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) {
			c := in.Spec.Components[common.ComponentEngine]
			mutate(&c)
			in.Spec.Components[common.ComponentEngine] = c
		}
	}
	backups := func(enabled bool) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) { in.Spec.Backup = &corev1alpha1.InstanceBackupSpec{Enabled: enabled} }
	}

	tests := []struct {
		name         string
		old, updated func(*corev1alpha1.Instance)
		want         []string
		wantWarnings []string
	}{
		{
			name:    "storage grown",
			updated: engine(func(c *corev1alpha1.ComponentSpec) { c.Storage.Size = resource.MustParse("20Gi") }),
		},
		{
			name:    "storage shrunk",
			updated: engine(func(c *corev1alpha1.ComponentSpec) { c.Storage.Size = resource.MustParse("5Gi") }),
			want:    []string{"spec.components[engine].storage.size"},
		},
		{
			name:    "storage class changed",
			updated: engine(func(c *corev1alpha1.ComponentSpec) { c.Storage.StorageClass = new("fast") }),
			want:    []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:    "WAL storage removed",
			old:     engineParameters(`{"walStorage":{"size":"2Gi"}}`),
			updated: engineParameters(`{}`),
			want:    []string{"spec.components[engine].parameters.walStorage"},
		},
		{
			name:         "major upgrade",
			old:          func(in *corev1alpha1.Instance) { in.Spec.Version = "17.10" },
			updated:      func(in *corev1alpha1.Instance) { in.Spec.Version = "18.7" },
			wantWarnings: []string{"spec.version"},
		},
		{
			name:    "major downgrade",
			old:     func(in *corev1alpha1.Instance) { in.Spec.Version = "18.7" },
			updated: func(in *corev1alpha1.Instance) { in.Spec.Version = "17.10" },
			want:    []string{"spec.version"},
		},
		{
			name:    "version left to the applied bundle",
			old:     func(in *corev1alpha1.Instance) { in.Spec.Version = "17.10"; in.Status.Version = "17.10" },
			updated: func(in *corev1alpha1.Instance) { in.Spec.Version = "" },
		},
		{
			name:         "backups turned off",
			old:          backups(true),
			updated:      backups(false),
			wantWarnings: []string{"spec.backup.enabled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			old := validationtest.Load(t, instanceFixture)
			old.Spec.Version = "18.7"
			if tt.old != nil {
				tt.old(old)
			}
			updated := old.DeepCopy()
			tt.updated(updated)

			got := v.Run(updated, old)
			assert.Equal(t, tt.want, errorFields(got), "errors: %v", got.Errors)
			assert.Equal(t, tt.wantWarnings, warningFields(got.Warnings), "warnings: %v", got.Warnings)
			for _, w := range got.Warnings {
				assert.Contains(t, w, cnpg.Destructive)
			}
		})
	}
}

func errorFields(res controller.ValidationResult) []string {
	var fields []string
	for _, err := range res.Errors {
		fields = append(fields, err.Field)
	}
	return fields
}

func warningFields(warnings []string) []string {
	var fields []string
	for _, w := range warnings {
		fields = append(fields, strings.SplitN(w, ":", 2)[0])
	}
	return fields
}

// TestValidateUpdate checks the rules about changes, which need the stored
// Instance and so only run at admission.
func TestValidateUpdate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)

	tests := []struct {
		name         string
		old, updated string
		want         []string
		wantWarnings []string
	}{
		{
			name:    "pgvector turned off",
			old:     `{"extensions":{"pgvector":{"enabled":true}}}`,
			updated: `{"extensions":{"pgvector":{"enabled":false}}}`,
			want:    []string{"spec.components[engine].parameters.extensions.pgvector.enabled"},
		},
		{
			name:    "pgvector turned on, no restart",
			old:     `{}`,
			updated: `{"extensions":{"pgvector":{"enabled":true}}}`,
		},
		{
			name:         "pgaudit turned off restarts",
			old:          `{"extensions":{"pgaudit":{"enabled":true}}}`,
			updated:      `{}`,
			wantWarnings: []string{"spec.components[engine].parameters.extensions.pgaudit.enabled"},
		},
		{
			name:         "postgis turned on restarts",
			old:          `{}`,
			updated:      `{"extensions":{"postgis":{"enabled":true}}}`,
			wantWarnings: []string{"spec.components[engine].parameters.extensions.postgis.enabled"},
		},
		{
			name:    "AGE turned on after creation",
			old:     `{}`,
			updated: `{"extensions":{"age":{"enabled":true}}}`,
			want:    []string{"spec.components[engine].parameters.extensions.age.enabled"},
		},
		{
			name:         "TimescaleDB edition switched",
			old:          `{"extensions":{"timescaledb":{"enabled":true}}}`,
			updated:      `{"extensions":{"timescaledbCommunity":{"enabled":true}}}`,
			want:         []string{"spec.components[engine].parameters.extensions.timescaledbCommunity.enabled"},
			wantWarnings: []string{"spec.components[engine].parameters.extensions.timescaledb.enabled"},
		},
		{
			name:         "priority class changed",
			old:          `{}`,
			updated:      `{"priorityClassName":"high"}`,
			wantWarnings: []string{"spec.components[engine].parameters.priorityClassName"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			old := validationtest.Load(t, instanceFixture)
			old.Spec.Version = "18.6"
			engineParameters(tt.old)(old)
			updated := old.DeepCopy()
			engineParameters(tt.updated)(updated)

			got := v.Run(updated, old)
			var fields []string
			for _, err := range got.Errors {
				fields = append(fields, err.Field)
			}
			assert.Equal(t, tt.want, fields, "errors: %v", got.Errors)

			var warned []string
			for _, w := range got.Warnings {
				warned = append(warned, strings.SplitN(w, ":", 2)[0])
			}
			assert.Equal(t, tt.wantWarnings, warned, "warnings: %v", got.Warnings)
		})
	}
}

// TestValidateTLSUpdate checks that the certificate source of a reconciled
// Instance can't change, while its mode can.
func TestValidateTLSUpdate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)

	for _, tt := range []struct {
		name, old, updated string
		want               []string
	}{
		{name: "mode", old: `{"tls":{"issuer":{"name":"acme"}}}`, updated: `{"tls":{"mode":"requireTLS","issuer":{"name":"acme"}}}`},
		{name: "issuer", old: `{"tls":{"issuer":{"name":"acme"}}}`, updated: `{"tls":{"issuer":{"name":"other"}}}`, want: []string{"spec.parameters.tls"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			old := validationtest.Load(t, instanceFixture)
			old.Spec.Parameters = &runtime.RawExtension{Raw: []byte(tt.old)}
			old.Status.Version = "18.6"
			updated := old.DeepCopy()
			updated.Spec.Parameters = &runtime.RawExtension{Raw: []byte(tt.updated)}

			got := v.Run(updated, old)
			var fields []string
			for _, err := range got.Errors {
				fields = append(fields, err.Field)
			}
			assert.Equal(t, tt.want, fields, "errors: %v", got.Errors)
		})
	}
}

// The monitoring parameters of earlier releases are accepted, so Instances
// that carry them keep working, and ignored with a warning.
func TestValidateDeprecatedMonitoring(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	topology := func(raw string) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) {
			in.Spec.Topology = &corev1alpha1.TopologySpec{Type: "replicaSet", Parameters: &runtime.RawExtension{Raw: []byte(raw)}}
		}
	}
	prometheus := "spec.topology.parameters.monitoring.prometheus"
	enablePodMonitor := "spec.components[engine].parameters.monitoring.enablePodMonitor"

	tests := []struct {
		name         string
		mutate       []func(*corev1alpha1.Instance)
		wantWarnings []string
	}{
		{
			name:   "prometheus off, as the UI wrote it",
			mutate: []func(*corev1alpha1.Instance){topology(`{"monitoring":{"prometheus":{"enabled":false}}}`)},
		},
		{
			name: "prometheus on with labels",
			mutate: []func(*corev1alpha1.Instance){
				topology(`{"monitoring":{"prometheus":{"enabled":true,"podMonitorLabels":{"release":"kube-prometheus-stack"}}}}`),
			},
			wantWarnings: []string{prometheus},
		},
		{
			name:         "CloudNativePG PodMonitor",
			mutate:       []func(*corev1alpha1.Instance){engineParameters(`{"monitoring":{"enablePodMonitor":true}}`)},
			wantWarnings: []string{enablePodMonitor},
		},
		{
			name: "both, which earlier releases rejected",
			mutate: []func(*corev1alpha1.Instance){
				topology(`{"monitoring":{"prometheus":{"enabled":true}}}`),
				engineParameters(`{"monitoring":{"enablePodMonitor":true}}`),
			},
			wantWarnings: []string{prometheus, enablePodMonitor},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := validationtest.Load(t, instanceFixture)
			for _, mutate := range tt.mutate {
				mutate(in)
			}
			got := v.Run(in, nil)
			assert.Empty(t, got.Errors)

			var warned []string
			for _, w := range got.Warnings {
				warned = append(warned, strings.SplitN(w, ":", 2)[0])
			}
			assert.Equal(t, tt.wantWarnings, warned, "warnings: %v", got.Warnings)
		})
	}
}

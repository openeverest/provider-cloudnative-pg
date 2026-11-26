package provider

import (
	"context"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

func userSecretContext(t *testing.T, ref string, objects ...client.Object) *controller.Context {
	t.Helper()
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: testNamespace}}
	if ref != "" {
		instance.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: ref}
	}
	cl := fake.NewClientBuilder().WithScheme(tlsScheme(t)).WithObjects(append([]client.Object{instance}, objects...)...).Build()
	return controller.NewContext(context.Background(), cl, instance, "provider-cloudnative-pg")
}

func clusterWithInitDB(initDB *cnpgv1.BootstrapInitDB) *cnpgv1.Cluster {
	pg := testCluster()
	pg.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{InitDB: initDB}
	return pg
}

func initDBSecret(name string) *cnpgv1.BootstrapInitDB {
	return &cnpgv1.BootstrapInitDB{Database: "app", Owner: "app", Secret: &cnpgv1.LocalObjectReference{Name: name}}
}

func TestApplyUserSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     string
		custom  components.CNPGCustomSpec
		objects []client.Object
		// want is the application Secret of a Cluster bootstrapped from custom.
		want string
	}{
		{name: "no userSecretRef", want: "pg-app"},
		{name: "new cluster", ref: "creds", want: "creds"},
		{
			name: "new cluster keeps the initdb database and owner",
			ref:  "creds",
			custom: components.CNPGCustomSpec{Bootstrap: &components.BootstrapConfiguration{
				InitDB: &cnpgv1.BootstrapInitDB{Database: "shop", Owner: "shop"},
			}},
			want: "creds",
		},
		{
			name: "initdb secret in the parameters wins",
			ref:  "creds",
			custom: components.CNPGCustomSpec{Bootstrap: &components.BootstrapConfiguration{
				InitDB: initDBSecret("param-creds"),
			}},
			want: "param-creds",
		},
		{
			name:    "existing cluster keeps its generated credentials",
			ref:     "creds",
			objects: []client.Object{clusterWithInitDB(&cnpgv1.BootstrapInitDB{Database: "app", Owner: "app"})},
			want:    "pg-app",
		},
		{
			name:    "existing cluster created with the secret",
			ref:     "creds",
			objects: []client.Object{clusterWithInitDB(initDBSecret("creds"))},
			want:    "creds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := userSecretContext(t, tt.ref, tt.objects...)
			custom := tt.custom
			require.NoError(t, applyUserSecret(c, &custom))

			pg := testCluster()
			if custom.Bootstrap != nil {
				pg.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{InitDB: custom.Bootstrap.InitDB}
			}
			assert.Equal(t, tt.want, pg.GetApplicationSecretName())
			if tt.custom.Bootstrap != nil && tt.custom.Bootstrap.InitDB.Database != "" {
				assert.Equal(t, tt.custom.Bootstrap.InitDB.Database, custom.Bootstrap.InitDB.Database)
			}
		})
	}
}

func TestValidateUserSecret(t *testing.T) {
	t.Parallel()

	shop := components.CNPGCustomSpec{Bootstrap: &components.BootstrapConfiguration{
		InitDB: &cnpgv1.BootstrapInitDB{Database: "shop", Owner: "shop"},
	}}
	tests := []struct {
		name    string
		custom  components.CNPGCustomSpec
		objects []client.Object
		want    string
	}{
		{name: "default owner", objects: []client.Object{credentialSecret("creds", "app", "pw", "")}},
		{name: "custom owner", custom: shop, objects: []client.Object{credentialSecret("creds", "shop", "pw", "")}},
		{
			name: "stringData",
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: testNamespace},
				StringData: map[string]string{"username": "app", "password": "pw"},
			}},
		},
		{name: "missing secret is left to the runtime"},
		{
			name:    "missing password",
			objects: []client.Object{credentialSecret("creds", "app", "", "")},
			want:    `missing "password"`,
		},
		{
			name: "missing both keys",
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: testNamespace},
				Data:       map[string][]byte{"user": []byte("app")},
			}},
			want: `missing "username", "password"`,
		},
		{
			name:    "username isn't the default owner",
			objects: []client.Object{credentialSecret("creds", "alice", "pw", "")},
			want:    `username "alice" must be the application database owner "app"`,
		},
		{
			name:    "username isn't the initdb owner",
			custom:  shop,
			objects: []client.Object{credentialSecret("creds", "app", "pw", "")},
			want:    `owner "shop"`,
		},
		{
			name:    "not checked once the cluster exists",
			objects: []client.Object{credentialSecret("creds", "alice", "pw", ""), clusterWithInitDB(initDBSecret("creds"))},
		},
		{
			name: "not checked when the parameters name the secret",
			custom: components.CNPGCustomSpec{Bootstrap: &components.BootstrapConfiguration{
				InitDB: initDBSecret("param-creds"),
			}},
			objects: []client.Object{credentialSecret("creds", "alice", "pw", "")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := userSecretContext(t, "creds", tt.objects...)
			errs, err := validateUserSecret(c, &tt.custom)
			require.NoError(t, err)
			if tt.want == "" {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			assert.Equal(t, "spec.userSecretRef.name", errs[0].Field)
			assert.Contains(t, errs[0].Detail, tt.want)
		})
	}
}

func TestValidateUserSecretWarnings(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	withRef := func(in *corev1alpha1.Instance) {
		in.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: "creds"}
	}

	tests := []struct {
		name         string
		old          func(*corev1alpha1.Instance)
		mutate       []func(*corev1alpha1.Instance)
		wantWarnings []string
	}{
		{name: "create", mutate: []func(*corev1alpha1.Instance){withRef}},
		{
			name:         "initdb secret in the parameters",
			mutate:       []func(*corev1alpha1.Instance){withRef, engineParameters(`{"bootstrap":{"initdb":{"secret":{"name":"param-creds"}}}}`)},
			wantWarnings: []string{"spec.userSecretRef"},
		},
		{
			name:         "added to an existing instance",
			old:          func(*corev1alpha1.Instance) {},
			mutate:       []func(*corev1alpha1.Instance){withRef},
			wantWarnings: []string{"spec.userSecretRef"},
		},
		{name: "kept on update", old: withRef, mutate: []func(*corev1alpha1.Instance){withRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var old *corev1alpha1.Instance
			if tt.old != nil {
				old = validationtest.Load(t, instanceFixture)
				tt.old(old)
			}
			in := validationtest.Load(t, instanceFixture)
			for _, mutate := range tt.mutate {
				mutate(in)
			}
			got := v.Run(in, old)
			assert.Empty(t, got.Errors)

			var warned []string
			for _, w := range got.Warnings {
				warned = append(warned, strings.SplitN(w, ":", 2)[0])
			}
			assert.Equal(t, tt.wantWarnings, warned, "warnings: %v", got.Warnings)
		})
	}
}

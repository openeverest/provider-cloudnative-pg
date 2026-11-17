package cnpg

import (
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

func TestManagedRolesStatus(t *testing.T) {
	role := func(name string) cnpgv1.RoleConfiguration {
		return cnpgv1.RoleConfiguration{Name: name}
	}
	tests := []struct {
		name        string
		pg          *cnpgv1.Cluster
		wantBlocked bool
		wantPhase   corev1alpha1.InstancePhase
		wantMsg     string
	}{
		{
			name: "no managed roles",
			pg:   &cnpgv1.Cluster{},
		},
		{
			name: "empty managed roles",
			pg: &cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{Managed: &cnpgv1.ManagedConfiguration{}},
			},
		},
		{
			name: "roles reconciled",
			pg: &cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{role("app"), role("ro")}},
				},
				Status: cnpgv1.ClusterStatus{
					ManagedRolesStatus: cnpgv1.ManagedRoles{
						ByStatus: map[cnpgv1.RoleStatus][]string{
							cnpgv1.RoleStatusReconciled: {"app", "ro"},
						},
					},
				},
			},
		},
		{
			name: "roles awaiting reconciliation",
			pg: &cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{role("app"), role("ro")}},
				},
				Status: cnpgv1.ClusterStatus{
					ManagedRolesStatus: cnpgv1.ManagedRoles{
						ByStatus: map[cnpgv1.RoleStatus][]string{
							cnpgv1.RoleStatusReconciled: {"app"},
						},
					},
				},
			},
			wantBlocked: true,
			wantPhase:   corev1alpha1.InstancePhaseProvisioning,
			wantMsg:     "waiting for managed PostgreSQL roles to be reconciled: ro",
		},
		{
			name: "cannot reconcile is Failed",
			pg: &cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{role("app")}},
				},
				Status: cnpgv1.ClusterStatus{
					ManagedRolesStatus: cnpgv1.ManagedRoles{
						CannotReconcile: map[string][]string{
							"app": {"role owns objects"},
						},
					},
				},
			},
			wantBlocked: true,
			wantPhase:   corev1alpha1.InstancePhaseFailed,
			wantMsg:     `managed PostgreSQL role "app" cannot be reconciled: role owns objects`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, blocked := ManagedRolesStatus(tt.pg)
			if blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v (status: %+v)", blocked, tt.wantBlocked, got)
			}
			if !tt.wantBlocked {
				return
			}
			if got.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", got.Phase, tt.wantPhase)
			}
			if !strings.Contains(got.Message, tt.wantMsg) {
				t.Fatalf("message = %q, want substring %q", got.Message, tt.wantMsg)
			}
		})
	}
}

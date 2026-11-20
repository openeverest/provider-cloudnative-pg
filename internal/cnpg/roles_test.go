package cnpg

import (
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

func TestManagedRoles(t *testing.T) {
	role := func(name string) cnpgv1.RoleConfiguration {
		return cnpgv1.RoleConfiguration{Name: name}
	}
	tests := []struct {
		name        string
		pg          *cnpgv1.Cluster
		wantFailure string
		wantWaiting string
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
			wantWaiting: "Waiting for managed PostgreSQL roles to be reconciled: ro",
		},
		{
			name: "cannot reconcile is a failure, sorted by role",
			pg: &cnpgv1.Cluster{
				Spec: cnpgv1.ClusterSpec{
					Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{role("app")}},
				},
				Status: cnpgv1.ClusterStatus{
					ManagedRolesStatus: cnpgv1.ManagedRoles{
						CannotReconcile: map[string][]string{
							"ro":  {"permission denied"},
							"app": {"role owns objects"},
						},
					},
				},
			},
			wantFailure: `managed PostgreSQL role "app" cannot be reconciled: role owns objects` + "\n" +
				`managed PostgreSQL role "ro" cannot be reconciled: permission denied`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure, waiting := ManagedRoles(tt.pg)
			if failure != tt.wantFailure {
				t.Fatalf("failure = %q, want %q", failure, tt.wantFailure)
			}
			if waiting != tt.wantWaiting {
				t.Fatalf("waiting = %q, want %q", waiting, tt.wantWaiting)
			}
		})
	}
}

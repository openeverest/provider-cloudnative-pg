package cnpg

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

// ManagedRoles reports on the roles the Cluster manages: failure says which
// CloudNativePG cannot reconcile and why, waiting which it has not reconciled
// yet. Both are "" when every role is reconciled.
func ManagedRoles(pg *cnpgv1.Cluster) (failure, waiting string) {
	if pg.Spec.Managed == nil || len(pg.Spec.Managed.Roles) == 0 {
		return "", ""
	}

	roleStatus := pg.Status.ManagedRolesStatus

	// Sorted, so the message is the same on every pass.
	failed := slices.Sorted(maps.Keys(roleStatus.CannotReconcile))
	failures := make([]string, 0, len(failed))
	for _, role := range failed {
		failures = append(failures, fmt.Sprintf(
			"managed PostgreSQL role %q cannot be reconciled: %s",
			role,
			strings.Join(roleStatus.CannotReconcile[role], "; "),
		))
	}
	if len(failures) > 0 {
		return strings.Join(failures, "\n"), ""
	}

	reconciledRoles := roleStatus.ByStatus[cnpgv1.RoleStatusReconciled]

	var rolesAwaitingReconciliation []string
	for _, roleConfig := range pg.Spec.Managed.Roles {
		if slices.Contains(reconciledRoles, roleConfig.Name) {
			continue
		}
		rolesAwaitingReconciliation = append(rolesAwaitingReconciliation, roleConfig.Name)
	}

	if len(rolesAwaitingReconciliation) == 0 {
		return "", ""
	}

	return "", fmt.Sprintf(
		"Waiting for managed PostgreSQL roles to be reconciled: %s",
		strings.Join(rolesAwaitingReconciliation, ", "),
	)
}

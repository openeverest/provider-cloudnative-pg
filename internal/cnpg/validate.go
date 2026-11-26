package cnpg

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

// Matches the topology.yaml bootstrap database/owner regex.
var postgresIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

var dns1035Label = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// MaxNameLength is the longest Instance name CloudNativePG accepts for a
// Cluster.
const MaxNameLength = 50

// The smallest engine resources PostgreSQL runs with. Storage applies to the
// data and the WAL volume: CloudNativePG's default max_wal_size is 1GB.
var (
	MinStorage = resource.MustParse("1Gi")
	MinCPU     = resource.MustParse("100m")
	MinMemory  = resource.MustParse("256Mi")
)

var minStorageDetail = "must be at least " + MinStorage.String() + ": CloudNativePG lets WAL grow to max_wal_size, 1GB by default"

// ValidateCustomSpec checks the engine parameters, reporting every problem at
// its field under path, the engine's parameters.
func ValidateCustomSpec(path *field.Path, custom *components.CNPGCustomSpec) field.ErrorList {
	var errs field.ErrorList
	if custom.PriorityClassName != "" {
		if msgs := validation.IsDNS1123Subdomain(custom.PriorityClassName); len(msgs) > 0 {
			errs = append(errs, field.Invalid(path.Child("priorityClassName"), custom.PriorityClassName, strings.Join(msgs, "; ")))
		}
	}
	if custom.WALStorage != nil {
		walPath := path.Child("walStorage")
		switch size := custom.WALStorage.GetSizeOrNil(); {
		case size == nil || size.Sign() <= 0:
			errs = append(errs, field.Required(walPath.Child("size"), "walStorage requires a valid positive size or pvcTemplate storage request"))
		case size.Cmp(MinStorage) < 0:
			errs = append(errs, field.Invalid(walPath.Child("size"), size.String(), minStorageDetail))
		}
	}
	if custom.Bootstrap != nil {
		if err := ValidateBootstrap(custom.Bootstrap); err != nil {
			errs = append(errs, field.Invalid(path.Child("bootstrap", "initdb"), field.OmitValueType{}, err.Error()))
		}
	}
	if strings.TrimSpace(custom.PgHBA) != "" {
		if custom.PostgresConfiguration != nil && len(custom.PostgresConfiguration.PgHBA) > 0 {
			errs = append(errs, field.Invalid(path.Child("pgHBA"), field.OmitValueType{}, "pgHBA cannot be combined with postgresql.pg_hba; move the rules to pgHBA"))
		} else if _, err := ParsePgHBA(custom.PgHBA); err != nil {
			errs = append(errs, field.Invalid(path.Child("pgHBA"), field.OmitValueType{}, err.Error()))
		}
	}
	return errs
}

func ValidateBootstrap(bootstrap *components.BootstrapConfiguration) error {
	if bootstrap.InitDB == nil {
		return nil
	}

	initDB := bootstrap.InitDB
	if (initDB.Database != "" && initDB.Owner == "") || (initDB.Database == "" && initDB.Owner != "") {
		return fmt.Errorf("bootstrap initdb database and owner must both be specified or both left empty")
	}
	if initDB.Database != "" && !postgresIdentifier.MatchString(initDB.Database) {
		return fmt.Errorf("bootstrap initdb database %q must be a valid PostgreSQL identifier (letters, numbers, underscores; cannot start with a number)", initDB.Database)
	}
	if initDB.Owner != "" && !postgresIdentifier.MatchString(initDB.Owner) {
		return fmt.Errorf("bootstrap initdb owner %q must be a valid PostgreSQL identifier (letters, numbers, underscores; cannot start with a number)", initDB.Owner)
	}
	if initDB.Secret != nil && initDB.Secret.Name == "" {
		return fmt.Errorf("bootstrap initdb secret name cannot be empty")
	}

	return nil
}

// ValidateName checks the Instance name, which names the CloudNativePG Cluster
// and everything derived from it: the Services <name>-rw, -ro and -r, the
// Secrets <name>-superuser and <name>-app, the pods <name>-<n>, the Poolers
// <name>-pooler and -pooler-ro, and the operator's Jobs.
func ValidateName(name string) field.ErrorList {
	path := field.NewPath("metadata", "name")
	var errs field.ErrorList
	if len(name) > MaxNameLength {
		errs = append(errs, &field.Error{
			Type: field.ErrorTypeTooLong, Field: path.String(), BadValue: name,
			Detail: fmt.Sprintf("must be at most %d characters: CloudNativePG derives Service, Secret, pod and Job names from it (<name>-rw, <name>-superuser, <name>-1, ...)",
				MaxNameLength),
		})
	}
	if !dns1035Label.MatchString(name) {
		errs = append(errs, field.Invalid(path, name,
			"must be a DNS-1035 label (lowercase letters, digits and '-', starting with a letter and ending with a letter or digit), as CloudNativePG Service names are"))
	}
	return errs
}

// ValidateEngine checks the engine component's replicas, storage and
// resources, reporting every problem at its field under path, the engine.
func ValidateEngine(path *field.Path, engine corev1alpha1.ComponentSpec) field.ErrorList {
	var errs field.ErrorList
	switch {
	case engine.Replicas == nil:
		errs = append(errs, field.Required(path.Child("replicas"), ""))
	case *engine.Replicas < 1:
		errs = append(errs, field.Invalid(path.Child("replicas"), *engine.Replicas, "must be at least 1"))
	}
	if engine.Storage == nil || engine.Storage.Size.Sign() <= 0 {
		errs = append(errs, field.Required(path.Child("storage", "size"), "storage size is required"))
	}
	if engine.Resources == nil {
		return append(errs, field.Required(path.Child("resources"), "CPU and memory requests and limits are required"))
	}
	return append(errs, resourceErrors(path.Child("resources"), engine.Resources)...)
}

// ValidateEngineMinimums reports the engine's storage size, CPU and memory
// set below what PostgreSQL needs to run, at their fields under path, the
// engine. ValidateEngine reports missing ones.
func ValidateEngineMinimums(path *field.Path, engine corev1alpha1.ComponentSpec) field.ErrorList {
	var errs field.ErrorList
	if engine.Storage != nil && engine.Storage.Size.Sign() > 0 && engine.Storage.Size.Cmp(MinStorage) < 0 {
		errs = append(errs, field.Invalid(path.Child("storage", "size"), engine.Storage.Size.String(), minStorageDetail))
	}
	if engine.Resources == nil {
		return errs
	}
	for _, list := range []struct {
		name      string
		resources corev1.ResourceList
	}{{"requests", engine.Resources.Requests}, {"limits", engine.Resources.Limits}} {
		for _, m := range []struct {
			name   corev1.ResourceName
			min    resource.Quantity
			detail string
		}{
			{corev1.ResourceCPU, MinCPU, "must be at least " + MinCPU.String()},
			{corev1.ResourceMemory, MinMemory, "must be at least " + MinMemory.String() +
				": PostgreSQL's default shared_buffers (128MB) and the CloudNativePG instance manager must fit"},
		} {
			value, ok := list.resources[m.name]
			if ok && value.Sign() > 0 && value.Cmp(m.min) < 0 {
				errs = append(errs, field.Invalid(path.Child("resources", list.name).Key(string(m.name)), value.String(), m.detail))
			}
		}
	}
	return errs
}

// ValidateResources checks required CPU and memory requests/limits, CPU
// notation (positive, millicore precision), and request ≤ limit.
func ValidateResources(resources *corev1.ResourceRequirements) error {
	if resources == nil {
		return fmt.Errorf("resources are required")
	}
	return resourceErrors(field.NewPath("resources"), resources).ToAggregate()
}

// resourceErrors reports every problem ValidateResources checks for, at its
// field under path, the resources.
func resourceErrors(path *field.Path, resources *corev1.ResourceRequirements) field.ErrorList {
	var errs field.ErrorList
	for _, list := range []struct {
		name      string
		resources corev1.ResourceList
	}{{"requests", resources.Requests}, {"limits", resources.Limits}} {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			value, ok := list.resources[name]
			p := path.Child(list.name).Key(string(name))
			switch {
			case !ok || value.IsZero():
				errs = append(errs, field.Required(p, "both CPU and memory "+list.name+" are required"))
			case name == corev1.ResourceCPU:
				if detail := cpuProblem(value); detail != "" {
					errs = append(errs, field.Invalid(p, value.String(), detail))
				}
			}
		}
	}
	if errs != nil {
		return errs
	}
	if resources.Requests.Cpu().Cmp(*resources.Limits.Cpu()) > 0 {
		errs = append(errs, field.Invalid(path.Child("requests").Key(string(corev1.ResourceCPU)), resources.Requests.Cpu().String(),
			fmt.Sprintf("CPU request %s is greater than limit %s", resources.Requests.Cpu().String(), resources.Limits.Cpu().String())))
	}
	if resources.Requests.Memory().Cmp(*resources.Limits.Memory()) > 0 {
		errs = append(errs, field.Invalid(path.Child("requests").Key(string(corev1.ResourceMemory)), resources.Requests.Memory().String(),
			fmt.Sprintf("memory request %s is greater than limit %s", resources.Requests.Memory().String(), resources.Limits.Memory().String())))
	}
	return errs
}

// cpuProblem applies the UI's CPU rule (positive, at most millicore precision)
// to the parsed quantity. The API server stores quantities canonically (0.5 as
// 500m, 1000 as 1k), so the UI regex cannot be matched against the string here.
func cpuProblem(cpu resource.Quantity) string {
	if cpu.Sign() <= 0 {
		return fmt.Sprintf("CPU %s must be greater than zero", cpu.String())
	}
	if resource.NewMilliQuantity(cpu.MilliValue(), resource.DecimalSI).Cmp(cpu) != 0 {
		return fmt.Sprintf("CPU %s is finer than a millicore; use cores (e.g. 0.5, 2) or millicores (e.g. 500m)", cpu.String())
	}
	return ""
}

// ValidateScheduling rejects placing pods through both parameters.affinity and
// schedulingPolicy, as both write the Cluster's spec.affinity.
func ValidateScheduling(policy *commonv1alpha1.SchedulingPolicy, custom *components.CNPGCustomSpec) error {
	if policy == nil || custom.Affinity == nil {
		return nil
	}
	if policy.Affinity != nil || len(policy.NodeSelector) > 0 || len(policy.Tolerations) > 0 {
		return fmt.Errorf("parameters.affinity cannot be combined with schedulingPolicy affinity, nodeSelector or tolerations")
	}
	return nil
}

// ValidateSynchronous mirrors the CloudNativePG webhook so an invalid
// synchronous replication setting is reported before the Cluster is applied.
func ValidateSynchronous(engine corev1alpha1.ComponentSpec, custom *components.CNPGCustomSpec) error {
	if custom.PostgresConfiguration == nil || custom.PostgresConfiguration.Synchronous == nil || engine.Replicas == nil {
		return nil
	}
	sync := custom.PostgresConfiguration.Synchronous
	if sync.Number < 1 {
		return fmt.Errorf("postgresql.synchronous.number must be at least 1")
	}
	candidates := int(*engine.Replicas) + len(sync.StandbyNamesPre) + len(sync.StandbyNamesPost)
	if sync.Number >= candidates {
		return fmt.Errorf("postgresql.synchronous.number (%d) must be lower than the number of replicas (%d) plus listed standby names",
			sync.Number, *engine.Replicas)
	}
	return nil
}

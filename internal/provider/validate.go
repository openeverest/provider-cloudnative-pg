package provider

import (
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition"
	"github.com/openeverest/provider-cloudnative-pg/definition/components"
	"github.com/openeverest/provider-cloudnative-pg/internal/cnpg"
	barman "github.com/openeverest/provider-cloudnative-pg/internal/cnpg/barman"
	"github.com/openeverest/provider-cloudnative-pg/internal/common"
)

var _ controller.ReferenceValidator = (*Provider)(nil)

var (
	componentsPath = field.NewPath("spec", "components")
	enginePath     = componentsPath.Key(common.ComponentEngine)
	engineParams   = enginePath.Child("parameters")
)

// Validate checks the rules about the Instance alone. The runtime runs it at
// admission and before every Sync, once the Instance matches the schemas, the
// supported fields and the catalog the Provider declares. On an update at
// admission, in.Old is the stored Instance, so changes it forbids are denied
// when written.
func (p *Provider) Validate(in controller.ValidationInput) controller.ValidationResult {
	instance := in.Instance
	engine, ok := instance.Spec.Components[common.ComponentEngine]
	if !ok {
		return controller.ValidationResult{Errors: field.ErrorList{field.Required(enginePath, "")}}
	}

	errs := cnpg.ValidateName(instance.Name)
	errs = append(errs, cnpg.ValidateEngine(enginePath, engine)...)
	errs = append(errs, invalid(field.NewPath("spec", "backup", "storages"), barman.ValidateSchedules(instance.Spec.Backup))...)
	errs = append(errs, invalid(field.NewPath("spec", "dataSource"), barman.ValidateDataSource(instance.Spec.DataSource))...)
	errs = append(errs, invalid(componentsPath.Key(common.ComponentProxy), validateProxy(instance))...)
	_, proxy := instance.Spec.Components[common.ComponentProxy]
	errs = append(errs, cnpg.ValidateTLS(instance, proxy)...)
	errs = append(errs, cnpg.ValidateTLSTransition(instance, in.Old)...)

	custom, err := engineCustomSpec(engine)
	if err != nil {
		return controller.ValidationResult{Errors: append(errs, field.Invalid(engineParams, field.OmitValueType{}, err.Error()))}
	}
	errs = append(errs, cnpg.ValidateCustomSpec(engineParams, &custom)...)
	errs = append(errs, invalid(engineParams.Child("postgresql", "synchronous"), cnpg.ValidateSynchronous(engine, &custom))...)
	errs = append(errs, invalid(engineParams.Child("replicationSlots"), cnpg.ValidateReplicationSlots(&custom, engine.Version, instance.Spec.Version))...)
	errs = append(errs, invalid(engineParams.Child("bootstrap", "volumeSnapshot"), cnpg.ValidateVolumeSnapshotBootstrap(&custom, instance.Spec.DataSource != nil))...)
	errs = append(errs, invalid(enginePath.Child("schedulingPolicy"), cnpg.ValidateScheduling(engine.SchedulingPolicy, &custom))...)
	_, hasProxy := instance.Spec.Components[common.ComponentProxy]
	serviceErrs, serviceWarnings := validateService(instance.Name, engine.Service, &custom, hasProxy)
	errs = append(errs, serviceErrs...)
	errs = append(errs, invalid(engineParams.Child("extensions"), cnpg.ValidateExtensions(&custom, engine.Version, instance.Spec.Version))...)
	errs = append(errs, invalid(engineParams.Child("extensions", "pgStatStatements"), cnpg.ValidatePgStatStatements(&custom))...)
	errs = append(errs, invalid(engineParams.Child("managed", "roles"), validateReservedRoles(&custom))...)

	warnings := append(monitoringWarnings(instance, &custom), serviceWarnings...)
	minimums := append(cnpg.ValidateEngineMinimums(enginePath, engine), cnpg.ValidateCustomSpecMinimums(engineParams, &custom)...)
	switch {
	case in.Old == nil && instance.Status.ObservedGeneration > 0:
		// A reconciled Instance written before the minimums keeps syncing.
		for _, err := range minimums {
			warnings = append(warnings, err.Error())
		}
	case in.Old != nil:
		// A minimum the stored Instance already misses doesn't block an update.
		kept, carried := carryOver(minimums, oldMinimums(in.Old))
		errs = append(errs, kept...)
		warnings = append(warnings, carried...)
	default:
		errs = append(errs, minimums...)
	}

	if in.Old != nil {
		oldEngine := in.Old.Spec.Components[common.ComponentEngine]
		versionErrs, versionWarnings := cnpg.ValidateMajorVersionChange(versionPath(instance), postgresVersion(in.Old), postgresVersion(instance))
		errs = append(errs, versionErrs...)
		warnings = append(warnings, versionWarnings...)
		warnings = append(warnings, cnpg.BackupChangeWarnings(in.Old.Spec.Backup, instance.Spec.Backup)...)
		old, err := engineCustomSpec(oldEngine)
		if err == nil {
			errs = append(errs, cnpg.ValidateStorageChanges(enginePath, engineParams, oldEngine, engine, &old, &custom)...)
			errs = append(errs, cnpg.ValidateExtensionChanges(&old, &custom, engineParams.Child("extensions"))...)
			warnings = append(warnings, cnpg.ExtensionRestartWarnings(&old, &custom, engineParams.Child("extensions"))...)
			if old.PriorityClassName != custom.PriorityClassName {
				warnings = append(warnings, engineParams.Child("priorityClassName").String()+": changing it restarts the instances one at a time")
			}
		}
	}

	return controller.ValidationResult{Errors: errs, Warnings: warnings}
}

// ValidateReferences checks the rules that read the cluster: the extension
// images in the ClusterImageCatalog, and the live Cluster and Database for
// writes admission didn't see, including a change of the HA replication slot
// prefix CloudNativePG would refuse, and the bootstrap VolumeSnapshot: authorized
// for this Instance, and unchanged once the Cluster exists, the certificate
// Secrets CloudNativePG would otherwise get stuck on, and that the
// certificates the Cluster uses don't change. At admission its errors are
// warnings; before Sync they block it.
func (p *Provider) ValidateReferences(c *controller.Context) (field.ErrorList, error) {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	custom, err := engineCustomSpec(engine)
	if err != nil {
		// Validate reports it.
		return nil, nil
	}
	extensionsPath := engineParams.Child("extensions")
	slotPrefixPath := engineParams.Child("replicationSlots", "highAvailability", "slotPrefix")
	volumeSnapshotPath := engineParams.Child("bootstrap", "volumeSnapshot")

	var errs field.ErrorList
	for _, check := range []struct {
		path *field.Path
		run  func() error
	}{
		{extensionsPath, func() error {
			_, err := cnpg.ResolveExtensionImages(c, p.extensionCatalog, &custom, engine.Version, c.Instance().Spec.Version)
			return err
		}},
		{extensionsPath, func() error { return cnpg.ValidateExtensionsNotDisabled(c, &custom) }},
		{extensionsPath, func() error { return cnpg.ValidateExtensionsAtCreation(c, &custom) }},
		{slotPrefixPath, func() error { return cnpg.ValidateReplicationSlotsChange(c, &custom) }},
		{volumeSnapshotPath, func() error { return cnpg.ValidateVolumeSnapshot(c, &custom) }},
	} {
		err := check.run()
		if err == nil {
			continue
		}
		// A read that failed for another reason than a missing object is retried.
		if status := apierrors.APIStatus(nil); errors.As(err, &status) && !apierrors.IsNotFound(err) {
			return nil, err
		}
		errs = append(errs, field.Invalid(check.path, field.OmitValueType{}, err.Error()))
	}

	tls, err := cnpg.TLS(c.Instance())
	if err != nil {
		// Validate reports it.
		return errs, nil
	}
	for _, check := range []func(*controller.Context, definition.TLSParameters) (field.ErrorList, error){
		validateCertificateSecrets,
		validateTLSChange,
	} {
		tlsErrs, err := check(c, tls)
		if err != nil {
			return nil, err
		}
		errs = append(errs, tlsErrs...)
	}
	return errs, nil
}

// engineCustomSpec decodes the engine component's parameters.
func engineCustomSpec(engine corev1alpha1.ComponentSpec) (components.CNPGCustomSpec, error) {
	var custom components.CNPGCustomSpec
	if engine.Parameters == nil || len(engine.Parameters.Raw) == 0 {
		return custom, nil
	}
	if err := json.Unmarshal(engine.Parameters.Raw, &custom); err != nil {
		return custom, fmt.Errorf("decode the engine parameters: %w", err)
	}
	return custom, nil
}

// postgresVersion is the PostgreSQL release the Instance asks for: the engine
// version, else the version bundle, else the bundle the runtime applied.
func postgresVersion(in *corev1alpha1.Instance) string {
	for _, v := range []string{in.Spec.Components[common.ComponentEngine].Version, in.Spec.Version, in.Status.Version} {
		if v != "" {
			return v
		}
	}
	return ""
}

// versionPath is the field that sets the PostgreSQL release.
func versionPath(in *corev1alpha1.Instance) *field.Path {
	if in.Spec.Components[common.ComponentEngine].Version != "" {
		return enginePath.Child("version")
	}
	return field.NewPath("spec", "version")
}

// invalid reports err, if any, as an invalid value of the field at path.
func invalid(path *field.Path, err error) field.ErrorList {
	if err == nil {
		return nil
	}
	return field.ErrorList{field.Invalid(path, field.OmitValueType{}, err.Error())}
}

// oldMinimums reports the minimums the stored Instance misses.
func oldMinimums(old *corev1alpha1.Instance) field.ErrorList {
	engine := old.Spec.Components[common.ComponentEngine]
	errs := cnpg.ValidateEngineMinimums(enginePath, engine)
	if custom, err := engineCustomSpec(engine); err == nil {
		errs = append(errs, cnpg.ValidateCustomSpecMinimums(engineParams, &custom)...)
	}
	return errs
}

// carryOver splits errs into those previous lacks, kept, and those previous
// already had at the same type and field, returned as warnings. The runtime
// ratchets admission the same way, but against Validate on the stored
// Instance, which reports its minimums as warnings once it was reconciled.
func carryOver(errs, previous field.ErrorList) (field.ErrorList, []string) {
	type key struct {
		typ   field.ErrorType
		field string
	}
	had := make(map[key]bool, len(previous))
	for _, err := range previous {
		had[key{err.Type, err.Field}] = true
	}
	var kept field.ErrorList
	var carried []string
	for _, err := range errs {
		if had[key{err.Type, err.Field}] {
			carried = append(carried, err.Error())
		} else {
			kept = append(kept, err)
		}
	}
	return kept, carried
}

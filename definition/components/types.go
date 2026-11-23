// Package components contains custom spec types for provider component types.
//
// Each struct here corresponds to a component type defined in versions.yaml
// and is converted to an OpenAPI schema during generation.
// Add fields when a component type needs custom configuration beyond
// what the base Instance spec provides.
//
// +k8s:openapi-gen=true
package components

import (
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// CNPGCustomSpec defines custom configuration for postgresql components.
// Add fields here when the postgresql component type needs custom configuration
// beyond what the base Instance spec provides.
type CNPGCustomSpec struct {
	// PriorityClassName sets the Kubernetes PriorityClass of the instance pods.
	// The PriorityClass must exist; changing it rolls the instance pods.
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// RequireTLS rejects client connections that do not use TLS by placing a
	// "hostnossl all all all reject" rule before the user's pg_hba rules.
	// Unset or false keeps CNPG's default, which also accepts unencrypted
	// connections. Applied by CNPG with a configuration reload, no restart.
	RequireTLS *bool `json:"requireTLS,omitempty"`

	// PgHBA declares which clients may connect to which databases as which
	// users: pg_hba.conf records, one per line, such as
	//
	//   hostssl orders orders_app 10.0.0.0/8 scram-sha-256
	//   host    orders all        all        reject
	//
	// Blank lines and # comments are ignored. Records are checked in order,
	// after the operator's own rules (replication, pooler, metrics) and before
	// the default rule that lets every user in with a password, so end with a
	// reject record to allow only what is listed. Records may be host, hostssl
	// or hostnossl with the methods scram-sha-256, md5, cert, ldap, radius or
	// reject; trust is not allowed. Cannot be combined with postgresql.pg_hba.
	// Applied by CNPG with a configuration reload, no restart.
	PgHBA string `json:"pgHBA,omitempty"`

	// WALStorage provisions a separate WAL PVC per PostgreSQL instance using
	// CNPG's native storage configuration. Once enabled it cannot be removed.
	WALStorage *cnpgv1.StorageConfiguration `json:"walStorage,omitempty"`

	// ResizeInUseVolumes lets CNPG grow the data volumes of running instances
	// when the engine's storage size increases; the StorageClass must allow
	// volume expansion. Unset keeps CNPG's default, true.
	ResizeInUseVolumes *bool `json:"resizeInUseVolumes,omitempty"`

	// PVCTemplate is the template of the data volumes' PersistentVolumeClaims
	// (CNPG storage.pvcTemplate), for settings the Instance doesn't have, such
	// as accessModes or volumeAttributesClassName. The engine's storage size
	// and storageClass take precedence over the template's.
	PersistentVolumeClaimTemplate *corev1.PersistentVolumeClaimSpec `json:"pvcTemplate,omitempty"`

	// Affinity is CNPG's scheduling configuration of the instance pods:
	// anti-affinity between instances, node selector, tolerations and
	// additional affinity rules. Cannot be combined with the engine's
	// schedulingPolicy affinity, nodeSelector or tolerations, which also set it.
	Affinity *cnpgv1.AffinityConfiguration `json:"affinity,omitempty"`

	// PostgreSQL is CNPG's PostgreSQL configuration: parameters
	// (postgresql.conf), pg_hba and pg_ident records, synchronous
	// replication, shared_preload_libraries, LDAP and more. CNPG manages a
	// fixed set of parameters itself and rejects them here. The provider adds
	// the records of requireTLS and pgHBA, the parameters and libraries of
	// enabled extensions and replication slot settings. pg_hba cannot be
	// combined with pgHBA. CNPG reloads PostgreSQL, or restarts the instances
	// for parameters that need it.
	PostgresConfiguration *cnpgv1.PostgresConfiguration `json:"postgresql,omitempty"`

	// Managed is CNPG's declarative management of PostgreSQL roles
	// (managed.roles) and of the instance's Services (managed.services). The
	// Instance is Provisioning until CNPG has reconciled every role, and
	// Failed while one cannot be.
	Managed *cnpgv1.ManagedConfiguration `json:"managed,omitempty"`

	// Bootstrap sets how a new instance is initialized. Only read at
	// creation, and ignored when the Instance restores a backup
	// (spec.dataSource).
	Bootstrap *BootstrapConfiguration `json:"bootstrap,omitempty"`

	// Certificates are the TLS certificates of the instance (CNPG
	// certificates). Without them CNPG issues and rotates its own.
	// serverTLSSecret needs serverCASecret and cannot be combined with
	// serverAltDNSNames; replicationTLSSecret needs clientCASecret.
	Certificates *cnpgv1.CertificatesConfiguration `json:"certificates,omitempty"`

	// Monitoring configures CNPG's metrics exporter on port 9187 of every
	// instance pod: custom queries, the default queries, TLS and relabeling.
	// The provider adds the queries of pg_stat_statements when that extension
	// is enabled. To have the Prometheus Operator scrape the instance, use
	// spec.topology.parameters.monitoring.prometheus instead of the deprecated
	// enablePodMonitor, which cannot be combined with it.
	Monitoring *cnpgv1.MonitoringConfiguration `json:"monitoring,omitempty"`

	// ReplicationSlots configures CNPG replication slots: the physical slots of
	// the HA standbys, synchronization of user-defined physical slots to the
	// standbys, and failover of logical decoding slots
	// (highAvailability.synchronizeLogicalDecoding). With synchronizeLogicalDecoding
	// on PostgreSQL 17+, the provider turns on hot_standby_feedback and
	// sync_replication_slots unless they are set explicitly.
	// highAvailability.slotPrefix cannot change while HA slots are enabled.
	ReplicationSlots *cnpgv1.ReplicationSlotsConfiguration `json:"replicationSlots,omitempty"`

	// Extensions configures first-class PostgreSQL extensions managed by this provider.
	Extensions *ExtensionsSpec `json:"extensions,omitempty"`
}

// ExtensionsSpec lists optional extensions the provider can install and enable.
// Image-volume extensions are mounted from the images in the provider's
// extensions ClusterImageCatalog (Helm values extensionCatalog) and need
// PostgreSQL 18 plus Kubernetes ImageVolume support (1.35+, or 1.33/1.34 with
// the ImageVolume feature gate); adding one rolls the instance pods.
type ExtensionsSpec struct {
	// TimescaleDB (image volume, Apache-2.0 edition) runs CREATE EXTENSION
	// timescaledb in the application database. Cannot be turned off once enabled.
	TimescaleDB *ExtensionToggle `json:"timescaledb,omitempty"`

	// TimescaleDBCommunity (image volume, Community Edition) is TimescaleDB with
	// the Timescale License (TSL) components such as compression and continuous
	// aggregates. The TSL is not open source: it allows internal use but
	// prohibits offering TimescaleDB to third parties as a database service.
	// Requires PostgreSQL 18.6 or later (18.6 and 18.7 each have their own build,
	// matched to the database version); cannot be combined with timescaledb. Can
	// only be enabled at creation; cannot be turned off.
	TimescaleDBCommunity *ExtensionToggle `json:"timescaledbCommunity,omitempty"`

	// PgVector runs CREATE EXTENSION vector in the application database. It ships
	// in the CloudNativePG standard images (PostgreSQL 14+), so no image change or
	// restart is needed. Cannot be turned off once enabled.
	PgVector *ExtensionToggle `json:"pgvector,omitempty"`

	// PgAudit enables audit logging. It ships in the standard images; the operator
	// preloads and creates it in every database while a pgaudit.* parameter is
	// set, so the provider defaults pgaudit.log to "ddl, role". Can be turned off.
	PgAudit *ExtensionToggle `json:"pgaudit,omitempty"`

	// PgStatStatements tracks planning and execution statistics of all SQL
	// statements (pg_stat_statements) and exports the top statements as
	// metrics. It ships in the standard images; the operator preloads and
	// creates it in every database while a pg_stat_statements.* parameter is
	// set, so the provider defaults pg_stat_statements.track to "top". Enabled
	// by default on new instances; instances created without it keep it off
	// until it is enabled. Turning it on or off restarts the instances.
	PgStatStatements *ExtensionToggle `json:"pgStatStatements,omitempty"`

	// PostGIS (image volume) runs CREATE EXTENSION postgis in the application
	// database. Cannot be turned off once enabled.
	PostGIS *ExtensionToggle `json:"postgis,omitempty"`

	// PgRouting (image volume) runs CREATE EXTENSION pgrouting in the application
	// database. Requires postgis. Cannot be turned off once enabled.
	PgRouting *ExtensionToggle `json:"pgrouting,omitempty"`

	// PgIVM (image volume) preloads pg_ivm and runs CREATE EXTENSION pg_ivm in the
	// application database. Cannot be turned off once enabled.
	PgIVM *ExtensionToggle `json:"pgivm,omitempty"`

	// Wal2JSON (image volume) makes the wal2json logical decoding output plugin
	// available to replication slots. There is no CREATE EXTENSION.
	Wal2JSON *ExtensionToggle `json:"wal2json,omitempty"`

	// AGE (image volume, Apache AGE graph database) preloads age, runs CREATE
	// EXTENSION age in the application database and grants its owner USAGE on
	// ag_catalog. Can only be enabled at creation; cannot be turned off.
	AGE *ExtensionToggle `json:"age,omitempty"`

	// PgCron (image volume) preloads pg_cron, sets cron.database_name to the
	// application database and cron.use_background_workers to on, runs CREATE
	// EXTENSION pg_cron there and grants its owner USAGE on the cron schema, so
	// the owner can schedule jobs. Jobs run on the primary only. Can only be
	// enabled at creation; cannot be turned off.
	PgCron *ExtensionToggle `json:"pgcron,omitempty"`

	// PgRepack (image volume) runs CREATE EXTENSION pg_repack in the
	// application database. The pg_repack client is in the instance pods at
	// /extensions/pg-repack/bin/pg_repack and runs as the superuser. Cannot be
	// turned off once enabled.
	PgRepack *ExtensionToggle `json:"pgrepack,omitempty"`

	// PgPartman (image volume) preloads the pg_partman background worker, runs
	// CREATE EXTENSION pg_partman in the partman schema of the application
	// database and grants its owner the privileges to manage partitions. The
	// worker runs maintenance for that database as the owner
	// (pg_partman_bgw.dbname and pg_partman_bgw.role), on the primary only.
	// Requires PostgreSQL 18.7+. Can only be enabled at creation; cannot be
	// turned off.
	PgPartman *ExtensionToggle `json:"pgpartman,omitempty"`
}

// ExtensionToggle switches a first-class extension on.
type ExtensionToggle struct {
	// Enabled turns the extension on. Whether it can be turned off again,
	// or only enabled at creation, depends on the extension.
	Enabled bool `json:"enabled"`
}

// BootstrapConfiguration sets how a new instance is initialized: an empty
// database (initdb) or a VolumeSnapshot of another instance.
type BootstrapConfiguration struct {
	// InitDB is CNPG's initdb bootstrap: the application database and its
	// owner (both CNPG's default, app, when unset; set both or neither), the
	// Secret with the owner's password, locale and encoding, and SQL to run
	// after initialization.
	InitDB *cnpgv1.BootstrapInitDB `json:"initdb,omitempty"`
	// VolumeSnapshot starts the instance from a VolumeSnapshot of another
	// instance's data volume, in the same namespace, instead of an empty
	// database. The snapshot must hold PGDATA and its WAL (no walStorage) and
	// can be crash-consistent: PostgreSQL recovers it from its own pg_wal, as
	// after a crash. On storage whose snapshots are copy-on-write clones, such
	// as ZFS, the instance starts in seconds whatever the size. Only read at
	// creation; initdb's database and owner name the existing application
	// database and owner.
	VolumeSnapshot *VolumeSnapshotSource `json:"volumeSnapshot,omitempty"`
}

// VolumeSnapshotSource names a VolumeSnapshot in the Instance's namespace.
type VolumeSnapshotSource struct {
	// Name of the VolumeSnapshot.
	Name string `json:"name"`
}

// PgBouncerParameters is the proxy component's configuration.
// The provider always creates a Pooler of type rw. With ReadOnly it also
// creates one of type ro. Instance count and pod resources come from the
// component's replicas and resources and apply to each Pooler.
type PgBouncerParameters struct {
	// PoolMode is the PgBouncer pool mode. CloudNativePG accepts session and
	// transaction. Empty selects the operator default, session.
	PoolMode string `json:"poolMode,omitempty"`

	// ReadOnly also creates a CloudNativePG Pooler of type ro that forwards to
	// the replicas. host-ro in the connection Secret then points at that
	// Pooler's Service. Requires at least two engine replicas, because the
	// cluster's -ro Service has no endpoints with a single instance.
	ReadOnly bool `json:"readOnly,omitempty"`
}
